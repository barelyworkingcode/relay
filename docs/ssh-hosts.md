# SSH hosts — a project whose directory lives on another machine

Design and contract. Three programs implement it — relay, relayLLM, eve — and
this document is the one place the wire shapes and the reasoning live. Code
carries the present tense; the *why* is here.

## The idea

A developer who works over `ssh` all day should be able to point relay at a
directory on that machine and get everything a local project gets: chat
sessions where the agent edits and runs code *there*, a file tree of *that*
directory, terminals that open *there*, and scheduled tasks that run *there*
on a cron kept *here*.

The machine running relay is the **console**. A machine reached over SSH is a
**host**. A project either lives on the console (as today) or on one host.

This is the mirror image of relay's existing *remote client* model
(`kind: remote`, enrolments, relayFS). That model lets a semi-trusted agent
elsewhere reach *into* this Mac with a narrow grant. SSH hosts let this Mac
reach *out* to a machine the operator already fully trusts, with the
operator's own SSH identity. The two never meet: a host project is
`kind: local` in shape and carries a `host_id`; `kind: remote` stays exactly
what it is.

## Decisions

**1. OpenSSH, not an SSH library.** Every component execs `/usr/bin/ssh`. The
operator's `~/.ssh/config`, agent, keys, `ProxyJump`, known hosts and
`Include`s all apply unchanged, so "it works in my terminal" means "it works
in relay". A Go or Node SSH library would re-implement a fraction of that and
fail differently from the terminal. Nothing here ever handles a private key.

**2. Relay owns the invocation; consumers get an argv prefix.** Relay stores
the host record and is the only place that turns it into `ssh` arguments.
relayLLM and eve receive `ssh_argv` (a ready-to-exec prefix ending in the
destination) and append their own remote command. One translation, one set of
options, one bug surface. All three processes run as the same user on the
console, so the prefix is meaningful to each of them.

**3. Connection sharing through OpenSSH `ControlMaster`.** Every invocation
carries `-o ControlMaster=auto -o ControlPath=<dir>/%C -o ControlPersist=600`.
The first connection to a host authenticates; every later `ssh` from any of
the three processes rides the same TCP session for free, and a new exec
channel costs tens of milliseconds. Relay does not babysit a daemon: the
master is whichever `ssh` got there first, and it lingers ten minutes past
its last use. Relay reads liveness with `ssh -O check`.

**4. The agent runs on the host.** A chat session on a host project spawns
`claude` *there*, over `ssh -T`, with the same stream-json protocol relayLLM
already speaks, on the same stdin/stdout pipes. The agent's Bash, Read, Edit
and Write tools therefore act on the host's filesystem and processes, which
is the entire point. relayLLM's own event loop is unchanged; only the process
it is reading from is remote.

**5. Permissions ride the stream, not a hook.** relayLLM's local design
registers a PreToolUse hook binary that dials back to relayLLM's Unix socket.
Neither the binary nor the socket exists on a host. Claude Code's
`--permission-prompt-tool stdio` moves the same question onto the process's
own stdout as a `control_request` (`subtype: can_use_tool`) and takes the
answer on stdin as a `control_response`. That is the mechanism the Claude
Agent SDK uses, it was verified on this box (see *Verification*), and it
needs nothing on the host but `claude` itself. Host sessions use it; console
sessions keep the hook.

**6. No relay MCPs on a host in v1.** Relay-brokered tools (`relay mcp`, the
bridge socket, project tokens) live on the console. A host session gets
Claude Code's built-in tools only, which covers the coding workflow. A
reverse-tunnelled bridge is a later step, not a v1 blocker.

**7. Eve's file plane is a small Node agent on the host.** Claude Code
requires Node, so a host that can run the agent can run a Node script. Eve
ships `remote-fs-agent.js`, launches it once per host over `ssh -T`, and
speaks newline-delimited JSON to it: list, read, write, rename, move, delete,
mkdir, stream, search, watch. The agent applies the same two-stage
containment check as the local FileService (lexical, then realpath). Node's
recursive `fs.watch` on the host feeds the same debounced `file_changed` /
`dir_changed` frames the browser already understands.

**8. The remote command line is shell-agnostic by construction.** The string
after the destination is executed by the host user's *login shell*, which may
be sh, bash, zsh or fish, and their quoting rules differ. Every remote
command is therefore the fixed form

    sh -c 'eval "$(printf %s <BASE64> | base64 -d)"'

where `<BASE64>` is a POSIX-sh script built with single-quote escaping. The
only characters the login shell ever parses are `[A-Za-z0-9+/=]` inside a
single-quoted string, which every shell treats identically. eve's Node agent
launches the same way, as `node -e "eval(Buffer.from('<BASE64>','base64').toString())"`.

**9. The probe finds absolute tool paths once.** Non-interactive `ssh` often
has a poorer PATH than the user's terminal (nvm, Homebrew on Linux, `~/.local/bin`).
Adding a host runs a probe that asks the *interactive login shell* where
`node` and `claude` are (`"$SHELL" -lic 'command -v node; command -v claude'`,
with a plain-PATH fallback) and stores absolute paths on the host record.
Every later invocation execs those absolute paths and never depends on PATH.
A `$SHELL` ending in `.exe` is skipped: Windows OpenSSH sets it to `cmd.exe`
or `powershell.exe`, neither takes `-lic`, and cmd answers with its banner.

**10. Fail fast, never prompt.** Every invocation carries
`-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=3`.
An SSH that would ask for a password hangs a headless pipe forever; refusing
to prompt turns that into an immediate, reportable error whose remedy is
"set up a key", surfaced by the probe.

**11. Terminal persistence belongs on the host.** A host terminal is an
`ssh -tt` child of the console; when the link drops, the remote command gets
a hangup and dies. A reverse tunnel from the host back to relay was
considered and rejected: the remote end would still die with the link. A
session that must survive a drop runs under a multiplexer on the host
(tmux, psmux), launched from a host template (see *Terminals on a host*).

## The session host never sandboxes a host project's session

`internal/sessions` (the `relay-sessions` binary described in
[`docs/session-host.md`](session-host.md)) is the newer launch path for
`pty`/`claude`/`pi`/`chat` sessions and applies its own confinement (C7
SBPL sandbox profiles) to a **console** project's session. Today that
confinement is real for a `pty` launch only. Not for `claude`/`pi`, which
run with no sandbox and no launch identity despite relay believing
otherwise, nor for a `chat` session's own provider process or its optional
relay-MCP tool child — the shim-based path for that tool child exists in
code, but `RelayMCPCommand` is never set in production, only in tests, so
it never actually spawns through it. All of this is an existing,
documented gap, not something this document's own claim below is exempt
from: see [`docs/session-host.md`](session-host.md#what-is-not-built-yet)
(gap 1) and `internal/sessions/hostapi/types.go`'s own package doc. A host
project's session is refused that confinement outright, not merely skipped
by omission: `cmd/relay/session_launch.go`'s `AuthorizeLaunch` computes
`sandbox := wantsSandbox(req.Kind, tmpl) && !(proj != nil &&
proj.IsHosted())` — the whole point of the sandbox is confining a *local*
process, and a host project's actual target runs on the far end of `ssh`,
where a profile written on this disk confines nothing. Wrapping the local
`ssh` client itself in a sandbox profile would only break the one process
that has to reach the network, the operator's own key material, and
whatever `~/.ssh/config` names — it would not confine the session at all.
For the same reason a host template cannot ask for confinement:
`config.ValidateHostTemplate` refuses `sandbox`, `read` and `read_write` on
one (see *Terminals on a host*).
This is the session-host system's own version of what this document's
decision 6 says about relay-brokered tools: a host session gets none of
relay's local confinement machinery, on the same reasoning, because none of
it describes anything running on this machine.

## Data model (relay)

`settings.json` gains a top-level `hosts` array. A project gains `host_id`.

```jsonc
{
  "hosts": [
    {
      "id": "h_8f1c…",             // relay-assigned, stable
      "name": "devbox",             // display name; unique, non-empty
      "target": "admin@devbox.local", // user@host, host, or an ssh_config alias
      "port": 0,                    // 0 = ssh default / ssh_config
      "identity_file": "",          // optional; passed as -i
      "created_at": "2026-09-05T06:00:00Z",
      "probe": {                    // last probe result; absent until first run
        "at": "2026-09-05T06:00:04Z",
        "ok": true,
        "os": "Darwin", "arch": "arm64",
        "home": "/Users/admin",
        "shell": "/bin/zsh",
        "node_path": "/opt/homebrew/bin/node",  "node_version": "v24.7.0",
        "claude_path": "/opt/homebrew/bin/claude", "claude_version": "2.1.258",
        "error": ""                 // non-empty iff ok == false
      },
      "terminal_templates": [       // host-scoped; see Terminals on a host
        { "id": "shell", "name": "Shell" },
        { "id": "claude-code", "name": "Claude Code", "command": "/opt/homebrew/bin/claude" }
      ]
    }
  ],
  "projects": [
    { "id": "…", "name": "relayfs", "path": "/home/admin/src/relayfs", "host_id": "h_8f1c…", … }
  ]
}
```

Rules, enforced in `internal/project.ValidateShape` and a new
`internal/config` host validator:

- `host_id` must name an existing host. Deleting a host that any project
  references is refused with the list of project names.
- A host project's `path` must be absolute (lexically: begins with `/`). It is
  **never** stat'd, realpath'd or created on the console.
- A host project may not carry `allowed_mcp_ids`, `mounts`,
  or `generate_skill` (decision 6; and the skill generator writes into the
  project directory, which is not here). `chat_templates`,
  `allowed_models`, `permission_policy` and `session_folders` are allowed
  and mean what they mean locally. `allowed_templates` is allowed but gates
  console templates only, which a host project is never offered; its
  terminals come from the host's `terminal_templates`.
- `kind: remote` and `host_id` are mutually exclusive.
- `syncProjectToken` skips path injection for a host project: a host path must
  never become a console fsMCP `allowed_dirs` value.
- `host.name` is unique case-insensitively; `target` is non-empty and contains
  no whitespace or shell metacharacters (`[A-Za-z0-9._@:-]+` after an optional
  `user@`), and neither its host part nor its user part may begin with `-` —
  `SSHArgv` places the target positionally with no `--`, so a leading `-`
  (e.g. `-oProxyCommand=...`) would be read as an ssh option instead of a
  destination; `port` is 0 or 1–65535; `identity_file` is absolute or empty.

## `ssh_argv` — the one derivation

`config.Host.SSHArgv(controlDir string) []string` returns, in order:

```
ssh
-o BatchMode=yes
-o ConnectTimeout=10
-o ServerAliveInterval=15
-o ServerAliveCountMax=3
-o ControlMaster=auto
-o ControlPath=<controlDir>/%C
-o ControlPersist=600
[-p <port>]              if port != 0
[-i <identity_file>]     if set
<target>
```

Callers append `-T` or `-tt` and then `--` and the remote command. `%C` is
OpenSSH's hash of the connection tuple, so the path stays short and unique.
`controlDir` is `<relay data dir>/run/ssh` if that path is under 90 bytes
and contains no whitespace or quotes, else `/tmp/relay-ssh-<uid>`; either is
created `0700`. The 90-byte rule exists because `sun_path` is 104 bytes on
macOS and the hash adds 40. The whitespace rule exists because ssh's `-o`
parser splits the value at a space and refuses the option, and the macOS
data dir sits under `Application Support`, so on a Mac the `/tmp` form is
the one actually in use.

`RemoteCommand(cwd string, argv []string, env map[string]string) string`
builds decision 8's launcher. The decoded script is

```sh
cd '<cwd>' && exec env K='v' … '<argv0>' '<arg1>' …
```

with every value single-quoted (`'` → `'\''`). Without `cwd` the `cd` is
omitted. This function exists once in Go (`internal/sshhost`, used by relay
and vendored verbatim into relayLLM as `sshhost.go`, with a test that pins
identical output for identical input) and once in Node (eve
`ssh-command.js`). The three test suites share fixture strings in this
document's *Fixtures* section so they cannot drift.

## HTTP (relay frontend server, consumed by eve)

| Route | Class | Body / result |
|---|---|---|
| `GET /api/hosts` | Read | `[hostView]` |
| `GET /api/hosts/{id}` | Read | `hostView` |
| `POST /api/hosts` | Configure | `{name, target, port?, identity_file?}` → `hostView` (201); runs a probe synchronously, result included |
| `PUT /api/hosts/{id}` | Configure | same fields, all optional → `hostView`; re-probes if `target`, `port` or `identity_file` changed |
| `DELETE /api/hosts/{id}` | Configure | 204; 409 `{error, projects:[names]}` if referenced |
| `POST /api/hosts/{id}/probe` | Configure | → `hostView` with fresh `probe`; 30 s cap |
| `POST /api/hosts/{id}/disconnect` | Configure | `ssh -O exit`; → `hostView` |
| `GET /api/hosts/{id}/templates` | Read | the host's `terminal_templates` |
| `POST /api/hosts/{id}/templates` | Configure | `TerminalTemplate` → the template (201); 409 if the id exists |
| `PUT /api/hosts/{id}/templates/{tid}` | Configure | `TerminalTemplate` → the template; 404 if absent |
| `DELETE /api/hosts/{id}/templates/{tid}` | Configure | 204; 404 if absent |

The template routes go through `HostTemplateOps` (`List`, `Create`, `Update`,
`Remove`), which validates with `config.ValidateHostTemplate` and reports the
same not-found / exists / invalid errors as the console `TemplateOps`, plus
host-not-found. Like the console template routes they are `configure` class
and not presence-gated.

Probe and disconnect are Configure, not Execute: they only rewrite the host
record's own `probe` field and the local control socket. This choice was
originally also justified by eve's frontend credential never holding
`execute` — that premise changed under plan-broker-and-sessions.md's F1
decision (eve's frontend launch identity now holds `execute`, for the
session-host launch routes), so an Execute class would no longer make the
dialog's *Test connection* button dead from the browser the way it once
would have. The classification itself is unchanged here and needs a fresh
look against the current premise, not assumed still correct because it once
was — see STATUS-relay-security.md.

`hostView` is the record above (including `terminal_templates`) plus two
derived, read-only fields:

```jsonc
{ …host…, "status": "connected" | "idle" | "unreachable" | "unknown",
  "ssh_argv": ["ssh", "-o", "BatchMode=yes", …, "admin@devbox.local"] }
```

`status`: `connected` when `ssh -O check` succeeds (a master is live); `idle`
when the last probe was ok but no master is live; `unreachable` when the last
probe failed; `unknown` when never probed. `ssh_argv` is decision 2's prefix.

`projectView` gains `host_id` (omitempty). `POST /api/projects` and
`PUT /api/projects/{id}` accept `host_id` (`""` moves a project back to the
console; the validator rules above apply).

The tray's IPC (`create_project`, `update_project`) accepts the same field,
and gains `list_hosts`, `create_host`, `update_host`, `remove_host`,
`probe_host`, `disconnect_host` with the same shapes, and
`list_host_templates {host_id}`, `create_host_template {host_id, template}`,
`update_host_template {host_id, template}` and
`remove_host_template {host_id, id}`, whose results are emitted like the
console template IPC's. The Settings window's Hosts tab edits a host's
templates through them.

The probe emits an audit event per run (`host.probe`, outcome ok/failed,
target and the discovered paths) so `relay audit` shows what relay reached.

## Bridge (relay ↔ relayLLM)

`PtyEnvResponse` gains `Host *HostSpec` (omitempty). When the project has a
`host_id`:

```jsonc
{ "relay_token": "",              // no project token for a host session (decision 6)
  "working_dir": "/home/admin/src/relayfs",
  "host": { "id": "h_8f1c…", "name": "devbox",
            "ssh_argv": ["ssh", …, "admin@devbox.local"],
            "node_path": "/opt/homebrew/bin/node",
            "claude_path": "/opt/homebrew/bin/claude",
            "shell": "/bin/zsh", "os": "Darwin" } }
```

Directory containment for a host project is lexical: `path.Clean(dir)` must
equal the project path or start with it plus `/`. `ResolveProjectTemplate` is
unchanged; relayLLM wraps the template for the host.

A host whose probe never succeeded (`claude_path` empty) makes
`ResolvePtyEnv` fail with `host "devbox" has no claude: run a probe` so the
session refuses to start with a message that names the fix.

## Terminals on a host

A host carries its own templates in `hosts[].terminal_templates`, the same
`TerminalTemplate` shape as the console's `terminal_templates`. Their
`command` and `args` are interpreted **on the host**: nothing is resolved
against the console's PATH, `$SHELL` or filesystem.

**Seeding.** A successful probe of a host with no templates seeds
`config.DefaultHostTemplates(probe)` in the same settings write:

- `shell` / Shell, `command` empty — the host's login shell;
- `claude-code` / Claude Code, `command` = the probed `claude_path`, only
  when the probe found one.

Existing templates are never overwritten: a re-probe leaves edits alone, and
only a host with an empty list is seeded. Deleting every template on a host
therefore means the next successful probe re-seeds Shell and Claude Code;
keep at least one template to stop that.

**Launch.** relay-sessions execs `ssh_argv + ["-tt", "--", <remote>]` under
the local pty; `pty.Setsize` propagates as SIGWINCH through ssh. `<remote>`
is:

- `command` empty: `sshhost.LoginShellCommandForOS(os, dir, env)`, whose
  decoded script is `cd '<dir>' && exec env 'K'='v' … "$SHELL" -l`.
  `"$SHELL"` is emitted in double quotes, not single, so the host expands
  it: zsh on a Mac, cmd.exe under Windows OpenSSH.
- otherwise: `sshhost.RemoteCommandForOS(os, dir, [command, args…], env)`,
  the argv verbatim after `${PROJECT_PATH}` / `${PROJECT_ID}` expansion. No
  login shell resolves it, so `command` is best an absolute path.

Both use the Windows launcher when the probed OS is `MINGW*`/`MSYS*`/
`CYGWIN*` and decision 8's launcher otherwise. `TERM=xterm-256color` and
`COLORTERM=truecolor` are added to `env` unless the template sets them.
`Directory` defaults to the host's `home` when the caller sends none.

**Catalog and gating.** For a host project,
`GET /api/terminal/templates?project=<id>` returns the host's templates
(`config.TemplatesForProject`: sorted by id, invalid ones dropped and
logged). Console templates are never offered on a host project. A host
project may launch every template of its host; `allowed_templates` gates
console templates only. A claude (chat) session on a host project passes
the kind gate iff its host has a `claude-code` template, and still execs the
probed `claude_path` (see *relayLLM*). A pi session on a host project is
refused at launch (`provider_not_available_on_host`): pi's overlay writes
into the project directory and symlinks into the console's home (see *What
is deliberately absent*).

**Validation.** `config.ValidateHostTemplate` is `ValidateTerminalTemplate`
plus refusals of `sandbox`, `read` and `read_write` (see *The session host
never sandboxes*), `env_passthrough` (it would copy the console's
environment to another machine), and `model_key` or `${MODEL_KEY}` (the
model endpoint listens on the console; the key has no business leaving it).

**Example: a shell that survives an ssh drop on a Windows host.** The remote
script runs under Git for Windows' `sh` with a non-interactive PATH, so the
executable is named by its full path:

```json
{ "id": "psmux", "name": "psmux",
  "command": "C:/tools/psmux/psmux.exe",
  "args": ["new-session", "-A", "-s", "relay-${PROJECT_ID}"] }
```

`-A` attaches when the named session exists, so reopening the terminal after
a drop resumes it (decision 11). `tmux new-session -A -s relay-${PROJECT_ID}`
does the same on a POSIX host.

## relayLLM

**Session and terminal records** gain `Host *HostSpec` (json `host`,
omitempty), filled from `ResolvePtyEnv` at create time and refreshed at each
spawn; a stored value is the fallback when the bridge is unavailable, so a
persisted host session resumes after a restart.

**Claude provider on a host.** `buildClaudeArgs` is unchanged except:
`--permission-prompt-tool stdio` is added and `--mcp-config` is omitted.
`Start()` execs `ssh_argv + ["-T", "--", RemoteCommand(dir, [claude_path, args…], env)]`
with `env` = `{RELAY_LLM_SESSION_ID}` only: no hook socket, no hook token, no
project token. `ensureHookConfig` is skipped for a host session. `cmd.Dir`
stays the console's cwd (irrelevant). `resolveClaudePath` is not consulted.

**Control requests.** `processLine` learns `type: "control_request"`. For
`subtype: "can_use_tool"`:

1. Evaluate `session.Policy` with `MatchToolRule` exactly as `/api/permission`
   does (deny, then allow).
2. Otherwise create a pending request in `PermissionManager` and push the
   existing `permission_request` event to the session's viewers, carrying
   `tool_name`, `input`, `description` and the claude `tool_use_id`, so eve's
   permission UI is unchanged.
3. On `permission_response`, write to stdin:
   - allow: `{"type":"control_response","response":{"request_id":R,"subtype":"success","response":{"behavior":"allow","updatedInput":<input>}}}`
   - deny: `{"type":"control_response","response":{"request_id":R,"subtype":"success","response":{"behavior":"deny","message":<reason or "Denied by user">}}}`
4. A request unanswered after 60 s is denied with `"No response"`, matching
   the hook's timeout; a stop/kill denies every pending request.

Any other `control_request` subtype is answered
`{"subtype":"error","error":"unsupported"}` so the CLI never blocks on us.

**History on join.** `readClaudeHistory` for a host session fetches
`~/.claude/projects/<encoded dir>/<sid>.jsonl` over
`ssh_argv + ["-T", "--", RemoteCommand("", ["cat", path])]` with a 10 s cap;
an error yields an empty history, never a failed join. Session delete removes
the same file with `rm -f` over ssh, best-effort. Sub-agent transcripts are
not fetched in v1.

**pi provider on a host** is refused at create time with
`provider "pi" is not available on a host project` — pi's overlay writes
files into the project directory and symlinks into the console's home.
Ollama / OpenAI / llama / mlx providers are console HTTP providers with no
process on the host; they are allowed (their built-in tools run locally, which
is the same trade the console makes) but their `appendClaudeMd` read of
`<dir>/CLAUDE.md` is skipped for a host project.

**Terminals on a host** are launched by relay-sessions, not relayLLM; see
*Terminals on a host* above.

**Idle, stop, kill** are unchanged: they act on the local `ssh` process, and
`ssh` propagates SIGINT/SIGTERM/hangup to the remote command.

## eve

**Project record** (`project-normalize.js`): gains `hostId` (string, `''` for
console) and, resolved server-side from the hosts cache, `host: {id, name,
status}`; the browser never sees `ssh_argv`. A `hosts` cache beside
`projectCache` is refreshed from `GET /api/hosts` whenever projects are.

**`GET /api/hosts` and the mutations** are proxied through eve's routes like
projects (`routes/index.js`), refreshing both caches on write.

**FileService factory.** `FileHandlers` takes `fileServiceFor(project)`:
`LocalFileService` (today's class, renamed in name only) for console
projects, `RemoteFileService` for host projects. Both implement the same
method set; `validatePath` on the remote side is a lexical pre-check, the
host agent does the realpath half. `routes/index.js`'s `/api/files/:projectId/*`
streams a host file through the agent's `stream` op instead of `res.sendFile`.
`search_project` on a host project runs the agent's `search` op.
`module-service.js` returns no modules for a host project (modules are
served from the console's disk; v2).

**Host agent pool** (`ssh-host-pool.js`): one `HostAgent` per host id,
spawned as `ssh_argv + ["-T", "--", <node launcher>]`, JSON lines both ways,
30 s per-request timeout, exponential reconnect 1 s → 30 s, in-flight
requests rejected on exit with `host "devbox" unreachable`. State changes
emit `host_status` to every browser connection:
`{type:'host_status', hostId, name, status:'connecting'|'connected'|'unreachable', error?}`.

**Agent protocol** (`remote-fs-agent.js`, runs on the host):

```
→ {id, op:"hello", version:1}                 ← {id, ok, home, os, node}
→ {id, op:"list",   root, path, showHidden}   ← {id, ok, entries:[{name,type:"file"|"directory"|"symlink",size,mtime}]}
→ {id, op:"read",   root, path, maxBytes}     ← {id, ok, content, size}      (utf8; >maxBytes → error "too large")
→ {id, op:"write",  root, path, content}      ← {id, ok}
→ {id, op:"writeb64", root, path, data}       ← {id, ok}                     (upload)
→ {id, op:"rename", root, path, newName}      ← {id, ok, path}
→ {id, op:"move",   root, path, destDir}      ← {id, ok, path}
→ {id, op:"delete", root, path}               ← {id, ok}                     (fs.rm recursive; no trash on a host)
→ {id, op:"mkdir",  root, parent, name}       ← {id, ok, path}
→ {id, op:"stat",   root, path}               ← {id, ok, type, size, mtime}
→ {id, op:"stream", root, path}               ← {id, chunk:<b64>}* then {id, ok, size}
→ {id, op:"search", root, query, {regex,caseSensitive,globs,maxMatches}} ← {id, ok, matches:[{path,line,col,text}], truncated}
→ {id, op:"watch",  root}                     ← {id, ok}   then events {event:"change", root, path}
→ {id, op:"unwatch", root}                    ← {id, ok}
   errors: {id, ok:false, error, code}   codes: ENOENT, EACCES, EISDIR, TOO_LARGE, TRAVERSAL, UNSUPPORTED
```

Every path is relative to `root`; the agent refuses anything that resolves
outside `root` (code `TRAVERSAL`) using the same two-stage rule as the local
FileService. The search walks with `fs.opendir`, skips `.git` and
`node_modules`, and caps at 500 matches / 10 MB scanned / 5 s. The watcher
is one recursive `fs.watch` per root, ref-counted per browser connection on
eve's side, debounced on eve's side by the existing FileWatcher.

**UI.** The browser learns a project is on a host from `project.host`. Where
it shows:

- A **host chip** on the project's sidebar header, Home card and ⌘K row:
  a small monospace-flavoured tag with the host name and a status dot
  (connecting → pulsing amber, connected → green, unreachable → red). Console
  projects have no chip; the absence is the design.
- The **Files tab** while the host is connecting shows a skeleton and
  "Connecting to devbox…"; on failure, an inline notice "devbox is
  unreachable" with a Retry that re-requests the listing.
- **Terminal tabs** on a host are titled `devbox · zsh`.
- The **project dialog** gets a "Where" segmented control: `This Mac` |
  `<each host>` | `+ Host…`. Choosing a host relabels the path field
  ("Path on devbox") and hides the MCP picker (decision 6) with a one-line
  reason. `+ Host…` opens an inline form (Name, SSH target, Port, Identity
  file) with **Test connection**, which calls the probe and shows the
  result card (OS, node, claude — each found or missing, with the missing
  one named as the thing to fix).
- **Delete** on a host file says "Delete permanently" (no Trash).

Terminal-to-project association (`getTerminalsForPath`) keys on
`(hostId, directory)` instead of the directory alone.

## Scheduled tasks

relayScheduler creates sessions through relayLLM by `projectId` and
`directory`; relayLLM resolves the host through `ResolvePtyEnv` exactly as
for an interactive session. A task on a host project therefore runs on the
host with no scheduler change. The cron stays on the console, which is what
"cron running local, remote execution" asks for.

## What is deliberately absent (v1)

- Relay MCPs / project tokens on a host (decision 6).
- Password or interactive authentication (decision 10).
- Modules on a host project.
- pi on a host project.
- Sub-agent transcripts on join.
- Per-host per-project overrides of `node_path` / `claude_path`: re-probe
  instead.
- Windows hosts as such. One works when Git for Windows' `bin` directory is
  on the user PATH, so the launcher's `sh` and `base64` resolve from cmd.exe;
  use `C:/…` project directories so both Git's sh and node read them.
  A host whose probed OS is `MINGW*`/`MSYS*`/`CYGWIN*` gets
  `sh -c "set -f; IFS=; eval $(printf %s <b64> | base64 -d)"` instead of
  decision 8's launcher (`sshhost.RemoteCommandForOS`): under `-tt` Windows
  OpenSSH re-quotes the line through conhost, and the nested quotes of the
  POSIX form do not survive.
  The seeded `shell` template execs the host's `$SHELL` (cmd.exe under
  Windows OpenSSH); a psmux template gives a shell that outlives the link.

## Fixtures

Shared by the Go and Node `RemoteCommand` tests. Inputs → decoded script.

```
cwd="/home/a b"  argv=["/usr/bin/claude","--print","it's"]  env={}
  → cd '/home/a b' && exec env '/usr/bin/claude' '--print' 'it'\''s'

cwd=""  argv=["cat","/x/y.jsonl"]  env={"TERM":"xterm-256color"}
  → exec env 'TERM'='xterm-256color' 'cat' '/x/y.jsonl'
```

The launcher wrapping the script is always exactly
`sh -c 'eval "$(printf %s <BASE64> | base64 -d)"'` with standard (padded)
base64 and no line breaks.

## Verification

On the devbox (macOS 26, OpenSSH, Claude Code 2.1.258), 2026-09-05:

- `ssh localhost "sh -c 'eval \"\$(printf %s <b64> | base64 -d)\"'"` executed
  the decoded script through zsh as the login shell.
- `claude -p --output-format stream-json --input-format stream-json
  --permission-prompt-tool stdio --permission-mode default` emitted
  `{"type":"control_request","request":{"subtype":"can_use_tool","tool_name":"Write",…}}`
  for a Write, and a `control_response` with `behavior: "allow"` written to
  stdin let the tool run; the stream then carried the tool result and a
  `result` event as usual.

The end-to-end acceptance for this feature is the devbox registered as a
host *of itself* (`admin@localhost`): a host project on it must show its
files, open a terminal, run a chat session that edits a file, answer a
permission prompt, and survive a relayLLM restart with its history intact.
