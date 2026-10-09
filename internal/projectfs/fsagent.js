'use strict';

// Relay's file-plane agent for an SSH host. Relay launches it on the host as
// `node -e "eval(gunzip(...))"`, so it runs as a source string, not a module:
// one self-contained file, Node core only, Node 18+. It speaks
// newline-delimited JSON on stdin/stdout (protocol version 2); stderr is not
// part of the protocol.
//
// Containment: every path is root-relative. A ".." segment is TRAVERSAL. Each
// component under the root is checked with lstat and a symbolic link is
// SYMLINK; the final component of a read or write is also opened with
// O_NOFOLLOW. The lstat walk and the open are separate calls, so a link
// planted between them is not caught: host sessions run unconfined, and this
// is not a promise against them.

const fs = require('fs');
const fsp = fs.promises;
const path = require('path');
const os = require('os');
const readline = require('readline');
const { execFile } = require('child_process');

const PROTOCOL_VERSION = 2;
const MAX_READ_BYTES = 10 * 1024 * 1024;
const MAX_WRITE_BYTES = 10 * 1024 * 1024;
const MAX_PASTE_BYTES = 10 * 1024 * 1024;
const STREAM_CHUNK_BYTES = 512 * 1024;
const GIT_TIMEOUT_MS = 10000;
const GIT_DEFAULT_MAX_BYTES = 8 * 1024 * 1024;
const GIT_MAX_BYTES = 32 * 1024 * 1024;
const SEARCH_MAX_MATCHES = 500;
const SEARCH_MATCHES_PER_FILE = 50;
const SEARCH_MAX_FILE_BYTES = 5 * 1024 * 1024;
const SEARCH_MAX_SCANNED_BYTES = 10 * 1024 * 1024;
const SEARCH_TIME_LIMIT_MS = 5000;
const SEARCH_MAX_GLOBS = 5;
const SEARCH_MAX_GLOB_LEN = 200;
const SEARCH_MAX_QUERY_LEN = 1000;
const BINARY_SNIFF_BYTES = 8000;
const PASTE_DIR = '/tmp';
const PASTE_NAME_RE = /^eve-paste-[0-9]+-[0-9a-f]+\.(png|jpg|gif|webp)$/;
const NOFOLLOW = fs.constants.O_NOFOLLOW || 0;

const MESSAGES = {
  ENOENT: 'No such file or directory',
  EACCES: 'Permission denied',
  EISDIR: 'Path is a directory',
  ENOTDIR: 'Not a directory',
  EEXIST: 'Already exists',
  SYMLINK: 'Symbolic links are not opened',
  TRAVERSAL: 'Path traversal not allowed',
};

class AgentError extends Error {
  constructor(message, code, size) {
    super(message);
    this.code = code;
    this.size = size;
  }
}

function fail(code, message, size) {
  return new AgentError(message || MESSAGES[code] || code, code, size);
}

// Anything that is not already an AgentError collapses to a code in the wire
// contract, never a raw errno the client does not know.
function toWireError(err) {
  if (err instanceof AgentError) {
    const out = { error: err.message, code: err.code };
    if (err.size !== undefined) out.size = err.size;
    return out;
  }
  let code = 'ERROR';
  switch (err && err.code) {
    case 'ENOENT': code = 'ENOENT'; break;
    case 'EACCES': case 'EPERM': code = 'EACCES'; break;
    case 'EISDIR': code = 'EISDIR'; break;
    case 'ENOTDIR': code = 'ENOTDIR'; break;
    case 'EEXIST': case 'ENOTEMPTY': code = 'EEXIST'; break;
    case 'ELOOP': case 'EMLINK': code = 'SYMLINK'; break;
    default: break;
  }
  const message = code === 'ERROR' ? ((err && err.message) || String(err)) : MESSAGES[code];
  return { error: message, code };
}

function send(obj) {
  process.stdout.write(JSON.stringify(obj) + '\n');
}

function rootOf(msg) {
  const r = msg.root;
  if (typeof r !== 'string' || !path.isAbsolute(r)) throw fail('INVALID', 'root must be an absolute path');
  return path.resolve(r);
}

function splitRel(rel) {
  const s = rel === undefined || rel === null ? '' : rel;
  if (typeof s !== 'string' || s.includes('\0')) throw fail('INVALID', 'invalid path');
  const parts = [];
  for (const seg of s.split('/')) {
    if (seg === '' || seg === '.') continue;
    if (seg === '..') throw fail('TRAVERSAL');
    if (path.sep === '\\' && seg.includes('\\')) throw fail('INVALID', 'invalid path');
    parts.push(seg);
  }
  return parts;
}

function checkName(name) {
  if (typeof name !== 'string' || name === '' || name === '.' || name === '..' || /[/\0]/.test(name) || (path.sep === '\\' && name.includes('\\'))) {
    throw fail('INVALID', 'invalid name');
  }
  return name;
}

// Walks root/rel one component at a time with lstat. A symbolic link at any
// component is SYMLINK; a missing final component is allowed only when
// missingOk, and then st is null. The root itself is stat'd, not lstat'd: the
// operator chose it.
async function walk(root, rel, missingOk) {
  const parts = splitRel(rel);
  let cur = root;
  if (parts.length === 0) {
    return { full: root, parts, st: await fsp.stat(root) };
  }
  let st = null;
  for (let i = 0; i < parts.length; i++) {
    cur = path.join(cur, parts[i]);
    const last = i === parts.length - 1;
    try {
      st = await fsp.lstat(cur);
    } catch (err) {
      if (err.code === 'ENOENT' && last && missingOk) return { full: cur, parts, st: null };
      throw err;
    }
    if (st.isSymbolicLink()) throw fail('SYMLINK');
    if (!last && !st.isDirectory()) throw fail('ENOTDIR');
  }
  return { full: cur, parts, st };
}

function typeOf(st) {
  return st.isSymbolicLink() ? 'symlink' : st.isDirectory() ? 'directory' : 'file';
}

function relJoin(parts) {
  return parts.join('/');
}

// Refuses an existing destination, as the console backend does. The one
// exception is a case-only rename of the same inode, which on a
// case-insensitive volume looks like a collision with itself.
//
// This is deliberate: Node has no no-replace rename, so the check and the
// rename are two calls. A destination created between them is replaced;
// only the host's own processes can win that race.
async function renameNoReplace(from, to) {
  let dst = null;
  try {
    dst = await fsp.lstat(to);
  } catch (err) {
    if (err.code !== 'ENOENT') throw err;
  }
  if (dst) {
    const src = await fsp.lstat(from);
    const caseOnly = path.dirname(from) === path.dirname(to) &&
      path.basename(from) !== path.basename(to) &&
      path.basename(from).toLowerCase() === path.basename(to).toLowerCase() &&
      dst.dev === src.dev && dst.ino === src.ino;
    if (!caseOnly) throw fail('EEXIST');
  }
  await fsp.rename(from, to);
}

async function openNoFollow(full, flags, mode) {
  return fsp.open(full, flags | NOFOLLOW, mode);
}

function decodeData(content, encoding) {
  const text = typeof content === 'string' ? content : '';
  return encoding === 'base64' ? Buffer.from(text, 'base64') : Buffer.from(text, 'utf8');
}

// Search ----------------------------------------------------------------

function escapeRegExp(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

function globToRegExp(glob) {
  const escaped = glob.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.');
  return new RegExp('^' + escaped + '$');
}

function compileGlobs(globs) {
  if (globs === undefined || globs === null) return { include: [], exclude: [] };
  if (!Array.isArray(globs) || globs.length > SEARCH_MAX_GLOBS) throw fail('INVALID', 'too many globs');
  const include = [];
  const exclude = [];
  for (const raw of globs) {
    if (typeof raw !== 'string' || raw.length > SEARCH_MAX_GLOB_LEN + 1) throw fail('INVALID', 'invalid glob');
    const negated = raw.startsWith('!');
    const g = negated ? raw.slice(1) : raw;
    if (g === '' || g.length > SEARCH_MAX_GLOB_LEN || g.startsWith('/') || g.split('/').includes('..')) {
      throw fail('INVALID', 'invalid glob');
    }
    (negated ? exclude : include).push(globToRegExp(g));
  }
  return { include, exclude };
}

function buildMatcher(msg) {
  const query = msg.query;
  if (typeof query !== 'string' || query.length === 0) throw fail('INVALID', 'Search query is empty');
  if (query.length > SEARCH_MAX_QUERY_LEN) throw fail('INVALID', 'Search query is too long');
  // Smart case: an unset case_sensitive means case-sensitive only when the
  // query itself contains an upper-case letter.
  const caseSensitive = typeof msg.case_sensitive === 'boolean' ? msg.case_sensitive : query !== query.toLowerCase();
  let source = msg.regex ? query : escapeRegExp(query);
  if (msg.word) source = '\\b(?:' + source + ')\\b';
  let re;
  try {
    re = new RegExp(source, caseSensitive ? '' : 'i');
  } catch (err) {
    throw fail('INVALID', 'Invalid regex: ' + err.message);
  }
  return re;
}

function runGitRaw(cwd, args, maxBytes, timeoutMs) {
  return new Promise((resolve, reject) => {
    execFile('git', args, {
      cwd,
      encoding: 'buffer',
      timeout: timeoutMs,
      maxBuffer: maxBytes,
      env: gitEnv(),
    }, (err, stdout, stderr) => {
      const stderrText = Buffer.isBuffer(stderr) ? stderr.toString('utf8') : String(stderr || '');
      if (err) {
        if (err.code === 'ENOENT') return reject(fail('GIT_MISSING', 'git is not installed on the host'));
        if (err.code === 'ERR_CHILD_PROCESS_STDIO_MAXBUFFER') return reject(fail('TOO_LARGE', 'git output too large'));
        if (err.killed) return reject(fail('TIMEOUT', 'git timed out'));
        if (typeof err.code !== 'number') return reject(fail('ERROR', err.message || 'git failed'));
      }
      resolve({ code: err ? err.code : 0, stdout, stderr: stderrText });
    });
  });
}

// Inherited GIT_* (GIT_DIR, GIT_WORK_TREE, ...) would redirect git away from
// the requested directory.
function gitEnv() {
  const env = {};
  for (const [k, v] of Object.entries(process.env)) {
    if (!k.startsWith('GIT_')) env[k] = v;
  }
  env.GIT_OPTIONAL_LOCKS = '0';
  env.GIT_TERMINAL_PROMPT = '0';
  env.LC_ALL = 'C';
  return env;
}

// The git-managed file set under root, or null when root is not in a work
// tree (or git is missing or refuses): the caller then walks the tree.
async function gitFileSet(root) {
  try {
    const { code, stdout } = await runGitRaw(root, [
      '-c', 'core.fsmonitor=false', '-c', 'core.hooksPath=/dev/null',
      'ls-files', '-co', '--exclude-standard', '-z',
    ], 256 * 1024 * 1024, SEARCH_TIME_LIMIT_MS);
    if (code !== 0) return null;
    const out = [];
    const seen = new Set();
    for (const p of stdout.toString('utf8').split('\0')) {
      if (p === '' || seen.has(p)) continue;
      seen.add(p);
      out.push(p);
    }
    return out;
  } catch {
    return null;
  }
}

async function* walkFiles(root, rel, state) {
  if (state.stop()) return;
  let entries;
  try {
    entries = await fsp.readdir(path.join(root, rel), { withFileTypes: true });
  } catch {
    return;
  }
  entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  for (const e of entries) {
    if (state.stop()) return;
    if (e.name.startsWith('.') || e.name === 'node_modules') continue;
    const childRel = rel === '' ? e.name : rel + '/' + e.name;
    if (e.isDirectory()) yield* walkFiles(root, childRel, state);
    else if (e.isFile()) yield childRel;
  }
}

const searches = new Map(); // request id -> { cancelled }

async function opSearch(msg) {
  const root = rootOf(msg);
  const re = buildMatcher(msg);
  const globs = compileGlobs(msg.globs);
  const maxMatches = Math.min(Number(msg.max_matches) > 0 ? Number(msg.max_matches) : SEARCH_MAX_MATCHES, SEARCH_MAX_MATCHES);
  const start = Date.now();
  const flag = { cancelled: false };
  searches.set(msg.id, flag);

  const matches = [];
  let truncated = false;
  let scanned = 0;
  const state = {
    stop() {
      if (flag.cancelled) return true;
      if (Date.now() - start > SEARCH_TIME_LIMIT_MS || scanned > SEARCH_MAX_SCANNED_BYTES) {
        truncated = true;
        return true;
      }
      return false;
    },
  };

  try {
    await walk(root, '', false);
    const listed = await gitFileSet(root);
    const files = listed === null ? walkFiles(root, '', state) : listed;
    for await (const rel of files) {
      if (state.stop()) break;
      if (globs.include.length && !globs.include.some((g) => g.test(rel))) continue;
      if (globs.exclude.some((g) => g.test(rel))) continue;
      const found = await searchFile(root, rel, re, SEARCH_MATCHES_PER_FILE);
      scanned += found.scanned;
      for (const m of found.matches) {
        matches.push(m);
        if (matches.length >= maxMatches) { truncated = true; break; }
      }
      if (matches.length >= maxMatches) break;
    }
  } finally {
    searches.delete(msg.id);
  }
  return { matches, truncated };
}

// Opens rel with O_NOFOLLOW and skips anything that is not a plain text
// file: symlinks, specials, oversize and binary (a NUL in the first 8000
// bytes). O_NOFOLLOW checks only the last component, so the lstat walk of the
// parent directories is what rejects a path through a symlinked directory.
async function searchFile(root, rel, re, perFile) {
  const none = { matches: [], scanned: 0 };
  let handle;
  try {
    const slash = rel.lastIndexOf('/');
    if (slash > 0) await walk(root, rel.slice(0, slash), false);
    handle = await openNoFollow(path.join(root, ...rel.split('/')), fs.constants.O_RDONLY);
  } catch {
    return none;
  }
  try {
    const st = await handle.stat();
    if (!st.isFile() || st.size > SEARCH_MAX_FILE_BYTES) return none;
    const buf = await handle.readFile();
    if (buf.subarray(0, BINARY_SNIFF_BYTES).includes(0)) return { matches: [], scanned: st.size };
    const lines = buf.toString('utf8').split(/\r?\n/);
    const matches = [];
    // Every non-empty match on a line counts, as in the console backend.
    const every = new RegExp(re.source, re.flags.includes('g') ? re.flags : re.flags + 'g');
    for (let i = 0; i < lines.length && matches.length < perFile; i++) {
      every.lastIndex = 0;
      let m;
      while (matches.length < perFile && (m = every.exec(lines[i])) !== null) {
        if (m[0].length === 0) { every.lastIndex++; continue; }
        matches.push({ path: rel, line: i + 1, col: m.index + 1, len: m[0].length, text: lines[i] });
      }
    }
    return { matches, scanned: st.size };
  } catch {
    return none;
  } finally {
    await handle.close().catch(() => {});
  }
}

// Ops -------------------------------------------------------------------

const watchers = new Map(); // root string -> fs.FSWatcher

const ops = {
  async hello(msg) {
    return { version: PROTOCOL_VERSION, home: os.homedir(), os: process.platform, node: process.version };
  },

  async list(msg) {
    const root = rootOf(msg);
    const w = await walk(root, msg.path, false);
    if (!w.st.isDirectory()) throw fail('ENOTDIR');
    const dirents = await fsp.readdir(w.full, { withFileTypes: true });
    const showHidden = !!msg.show_hidden;
    const wanted = dirents.filter((e) => showHidden || !e.name.startsWith('.'));
    const entries = [];
    for (let i = 0; i < wanted.length; i += 64) {
      const batch = await Promise.all(wanted.slice(i, i + 64).map(async (e) => {
        let st;
        try {
          st = await fsp.lstat(path.join(w.full, e.name));
        } catch {
          return null; // vanished mid-listing
        }
        return { name: e.name, type: typeOf(st), size: st.size, mtime_ms: Math.floor(st.mtimeMs) };
      }));
      for (const b of batch) if (b) entries.push(b);
    }
    return { entries };
  },

  async stat(msg) {
    const root = rootOf(msg);
    const w = await walk(root, msg.path, false);
    return { type: typeOf(w.st), size: w.st.size, mtime_ms: Math.floor(w.st.mtimeMs) };
  },

  async read(msg) {
    const root = rootOf(msg);
    const w = await walk(root, msg.path, false);
    const limit = Math.min(Number(msg.max_bytes) > 0 ? Number(msg.max_bytes) : MAX_READ_BYTES, MAX_READ_BYTES);
    const handle = await openNoFollow(w.full, fs.constants.O_RDONLY);
    try {
      const st = await handle.stat();
      if (st.isDirectory()) throw fail('EISDIR');
      if (st.size > limit) throw fail('TOO_LARGE', 'File too large', st.size);
      const content = await handle.readFile({ encoding: 'utf8' });
      return { content, size: st.size };
    } finally {
      await handle.close().catch(() => {});
    }
  },

  // Pull-based: each call returns one chunk, so a slow consumer never
  // backs up the single stdout pipe that every other request shares.
  async stream(msg) {
    const root = rootOf(msg);
    const w = await walk(root, msg.path, false);
    const offset = Math.max(0, Number(msg.offset) || 0);
    const length = Math.min(Number(msg.length) > 0 ? Number(msg.length) : STREAM_CHUNK_BYTES, STREAM_CHUNK_BYTES);
    const handle = await openNoFollow(w.full, fs.constants.O_RDONLY);
    try {
      const st = await handle.stat();
      if (st.isDirectory()) throw fail('EISDIR');
      const buf = Buffer.alloc(length);
      const { bytesRead } = await handle.read(buf, 0, length, offset);
      return { data: buf.subarray(0, bytesRead).toString('base64'), eof: bytesRead === 0 || offset + bytesRead >= st.size, size: st.size };
    } finally {
      await handle.close().catch(() => {});
    }
  },

  async write(msg) {
    const root = rootOf(msg);
    const data = decodeData(msg.content, msg.encoding);
    if (data.length > MAX_WRITE_BYTES) throw fail('TOO_LARGE', 'File too large', data.length);
    const w = await walk(root, msg.path, true);
    if (w.parts.length === 0 || (w.st && w.st.isDirectory())) throw fail('EISDIR');
    const flags = fs.constants.O_WRONLY | fs.constants.O_CREAT | (msg.create_only ? fs.constants.O_EXCL : fs.constants.O_TRUNC);
    const handle = await openNoFollow(w.full, flags, 0o666);
    try {
      await handle.writeFile(data);
    } finally {
      await handle.close().catch(() => {});
    }
    return {};
  },

  async mkdir(msg) {
    const root = rootOf(msg);
    const name = checkName(msg.name);
    const w = await walk(root, msg.parent, false);
    if (!w.st.isDirectory()) throw fail('ENOTDIR');
    await fsp.mkdir(path.join(w.full, name));
    return { path: relJoin([...w.parts, name]) };
  },

  async rename(msg) {
    const root = rootOf(msg);
    const newName = checkName(msg.new_name);
    const w = await walk(root, msg.path, false);
    if (w.parts.length === 0) throw fail('INVALID', 'Cannot rename project root');
    await renameNoReplace(w.full, path.join(path.dirname(w.full), newName));
    return { path: relJoin([...w.parts.slice(0, -1), newName]) };
  },

  async move(msg) {
    const root = rootOf(msg);
    const src = await walk(root, msg.path, false);
    if (src.parts.length === 0) throw fail('INVALID', 'Cannot move project root');
    const dest = await walk(root, msg.dest_dir, false);
    if (!dest.st.isDirectory()) throw fail('ENOTDIR');
    const base = src.parts[src.parts.length - 1];
    await renameNoReplace(src.full, path.join(dest.full, base));
    return { path: relJoin([...dest.parts, base]) };
  },

  async delete(msg) {
    const root = rootOf(msg);
    const w = await walk(root, msg.path, false);
    if (w.parts.length === 0) throw fail('INVALID', 'Cannot delete project root');
    // No trash on a host: the delete is permanent.
    await fsp.rm(w.full, { recursive: true });
    return {};
  },

  search: opSearch,

  async git(msg) {
    const root = rootOf(msg);
    const args = msg.args;
    if (!Array.isArray(args) || !args.every((a) => typeof a === 'string')) throw fail('INVALID', 'git args must be an array of strings');
    const w = await walk(root, msg.cwd, false);
    if (!w.st.isDirectory()) throw fail('ENOTDIR');
    const limit = Math.min(Number(msg.max_bytes) > 0 ? Number(msg.max_bytes) : GIT_DEFAULT_MAX_BYTES, GIT_MAX_BYTES);
    const { code, stdout, stderr } = await runGitRaw(w.full, args, limit, GIT_TIMEOUT_MS);
    return { exit_code: code, stdout: stdout.toString('base64'), stderr };
  },

  // Outside any project root by design: a single relay-generated segment,
  // and O_EXCL refuses an existing file or symlink.
  async pastetmp(msg) {
    const name = String(msg.name || '');
    if (!PASTE_NAME_RE.test(name)) throw fail('INVALID', 'Invalid paste file name');
    const data = Buffer.from(typeof msg.data === 'string' ? msg.data : '', 'base64');
    if (data.length > MAX_PASTE_BYTES) throw fail('TOO_LARGE', 'File too large', data.length);
    const full = path.join(PASTE_DIR, name);
    await fsp.writeFile(full, data, { flag: 'wx', mode: 0o600 });
    return { path: full };
  },

  async watch(msg) {
    const root = rootOf(msg);
    const st = await fsp.stat(root);
    if (!st.isDirectory()) throw fail('ENOTDIR');
    if (watchers.has(msg.root)) return {};
    let watcher;
    try {
      watcher = fs.watch(root, { recursive: true }, (eventType, filename) => {
        if (!filename) return;
        send({
          event: 'fs',
          root: msg.root,
          path: String(filename).split(path.sep).join('/'),
          kind: eventType === 'rename' ? 'rename' : 'change',
        });
      });
    } catch (err) {
      throw fail('UNSUPPORTED', err.message);
    }
    watcher.on('error', () => {
      try { watcher.close(); } catch { /* already closed */ }
      watchers.delete(msg.root);
      send({ event: 'watch_error', root: msg.root });
    });
    watchers.set(msg.root, watcher);
    return {};
  },

  async unwatch(msg) {
    const watcher = watchers.get(msg.root);
    if (watcher) {
      try { watcher.close(); } catch { /* already closed */ }
      watchers.delete(msg.root);
    }
    return {};
  },

  async cancel(msg) {
    const flag = searches.get(msg.target);
    if (flag) flag.cancelled = true;
    return {};
  },
};

async function handle(msg) {
  const fn = Object.prototype.hasOwnProperty.call(ops, msg.op) ? ops[msg.op] : null;
  if (!fn) {
    send({ id: msg.id, ok: false, error: 'Unknown op: ' + msg.op, code: 'ERROR' });
    return;
  }
  try {
    const result = await fn(msg);
    send({ id: msg.id, ok: true, ...result });
  } catch (err) {
    send({ id: msg.id, ok: false, ...toWireError(err) });
  }
}

process.stdout.on('error', () => process.exit(0));

const rl = readline.createInterface({ input: process.stdin, terminal: false });
rl.on('line', (line) => {
  if (!line.trim()) return;
  let msg;
  try {
    msg = JSON.parse(line);
  } catch {
    return; // no id to reply to
  }
  if (msg === null || typeof msg !== 'object') return;
  handle(msg);
});

// stdin closing (ssh exiting) ends the agent.
rl.on('close', () => process.exit(0));
