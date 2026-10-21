# Outside sessions

Relay observes Claude, console Pi and codex daemon sessions started outside Relay on the console and in
registered SSH host project folders. These are display data, separate from
managed sessions. An `outside:` identity never grants input, resize, kill,
start, permission-answer or configuration authority.

## Read interface

The existing frontend socket exposes three Chief of Staff scoped doors:

- `GET /api/chief-of-staff/outside-sessions`
- `GET /api/chief-of-staff/outside-sessions/{id}`
- `GET /ws/outside-sessions`

Each requires the existing `X-Relay-Scope: chief-of-staff` request scope and
its normal caller authorization. No credential or listener is added. The
WebSocket sends replacement snapshots and closes with 1008 on a client data
frame. The wire snapshot is `{type:"outside_sessions",revision,
probeIntervalMs:15000,sessions,machines}`. `sessions` lists the live rows
sorted by `id`, then the ended rows, newest `endedAt` first, at most 50.

Rows contain `id`, `name`, nullable `projectId` and `hostId`, `machine`,
`directory`, `branch`, `state`, `since`, `updatedAt`, `source`, `outside:true`,
`readOnly:true`, nullable `tmuxSession`, `viewable` and `viewReason`.
States are `running`, `asking`, `idle` and `ended`; sources are `probe` and `hook`.
Every row also carries `claudeSessionId` (the registry `sessionId` of a live
row, or the transcript's id when the process has no registry entry; an ended
row's transcript stem), `adoptable` and, when not adoptable, `adoptReason` (see
[Adopting a session](#adopting-a-session)). An `ended` row also carries
`endedAt` (see [Ended sessions](#ended-sessions)).
`viewable` is true when a tmux pane's process ancestry ties the Claude process
to a tmux session and the machine is the console or a host with
`show_all_tmux_sessions` on. Otherwise `viewReason` is `not_in_tmux` (no pane
ties it) or `outside_view_disabled` (the host has not opted in). A row on an
unreachable machine reads `idle` and keeps its last probe's `viewable` until
it expires; the viewer route (`POST /api/tmux/views`) repeats every check, so a stale
`viewable` opens nothing.
Machine entries contain nullable `hostId`, `machine`, `status` (`reachable`,
`unreachable` or `unsupported`), `lastProbeAt` and a safe `error` string. Raw PIDs, process
arguments and environments never appear in responses; only the execute-class transcript route returns transcript pages.

Detail returns `{session,lastTurn:{text,at,complete},lastExcerpt}`. It checks
the live identity again and reads the server-derived transcript at request
time. Text joins visible assistant blocks after the latest genuine user
prompt, excluding tool-result boundaries, and keeps the last 8192 characters.
The excerpt is the last 500 characters of that same text. Partial output is
available; missing latest text is empty, never a cached previous answer.
An ended id answers the same shape from the 1 MiB transcript tail, with no new
probe. Unknown identities answer 404 `outside_session_not_found`; failed live
observation or transcript reads answer 502 `outside_status_unavailable`.

### Transcript page

`GET /api/outside-sessions/{id}/transcript?before=<cursor>&limit=<n>` is class
`execute` (socket only, held by the signed-in frontend) and is not reachable
from the Chief of Staff scope, which reads only the latest turn. It takes no
body and writes nothing. `{id}` is looked up in the live inventory map; the
file is the path the last probe derived, so no request field names a path. An
ended session leaves the map at the next probe (15 seconds) and answers 404; an
ended row's id answers 404 too, because the route reads live entries only.

The window is bytes `[start, end)` of the file: `end` is `before`, or the
size; `start` is `max(0, end - 262144)`. A first partial line is dropped when
`start` is above zero. A page holds the newest `limit` records (default 50, at
most 200) that yield entries; a record is never split. The same script runs on
the console (`sh -c`) and on a host (the probe's ssh shell read). It refuses a
symlink on the file or its three parent folders, prints `0 0` for a missing
file, exits 4 when `end` is past the size (400 `invalid_cursor`), and prints
`<size> <start>` then the window. Go refuses a longer body (502).

One JSONL record maps to entries by block, `id` being
`<record byte offset>.<block index>`:

| Record | Entry |
|---|---|
| `user`, string content or a `text` block | `user` |
| `user`, a `tool_result` block | `tool`, `kind:"result"`: string content, or its `text` blocks joined by a newline, other blocks as `[type]`; `is_error` prefixes `Error: ` |
| `assistant`, a `text` block | `assistant` |
| `assistant`, a `tool_use` block | `tool`, `kind:"call"`, `name` (the block's name, capped like text, `""` if absent): `<name> <compact input JSON>` |
| thinking blocks, `isSidechain`, `isMeta`, other record types, invalid JSON, blank text | skipped |

User and assistant entries carry neither `kind` nor `name`, and `text` keeps its
form, so older clients read the same bytes. A result carries no `name`: its block
holds only the call's id, and the call may sit on an older page.

Text over 8192 characters keeps its first 8191 and ends with `…`, so an entry is never longer than 8192. A window
without a complete record answers `entries: []` with `cursor` at its start, so
a caller steps past it. A missing file answers an empty page with a null cursor.

## Discovery and lifetime

The baseline interval is 15 seconds. Machines run independently. Native Unix
process identity/cwd discovery uses ps plus procfs or lsof and needs no
host-side installation or Claude hooks. A host whose launcher is Windows
reports status `unsupported` and no rows. Reads are bounded: at most 32 live
Claude processes per machine, and for each one only its own transcript (the
registry `sessionId` for that pid, else the newest transcript in its project
folder), as a 16 KiB tail plus the first line when no registry entry exists;
512 registry records of at most 64 KiB; and a 1 MiB transcript tail for detail.
Remote scripts use the existing SSH path.

A registry must correlate its PID, creation lifetime, session identity and cwd
with a live Claude process. Older clients require an unambiguous live cwd and
matching transcript metadata from that lifetime. Ambiguous evidence is omitted.
Hosted rows must lie in a registered project subtree. A project path is
matched both as stored and as resolved on its own machine, because the probe
reports a process's resolved cwd. Managed sessions are
excluded using the selected `RELAY_SESSION_ID`, process ancestry and the Claude
identities session-host returns from its internal `GET /managed-identities`
(`{claude_session_ids:[string]}`, beside `/launch`, off the manifest); Relay IDs are never treated as Claude UUIDs.

A successful probe replaces membership. A confirmed exit leaves the live rows on
the next baseline and moves to ended when its transcript file is inside the
window. Failed probes mark the machine unreachable, clear active state
and viewing, and evict rows by one minute since their last live observation.
Reports and detail reads cannot extend an exited identity's lifetime.

Probe state is an estimate from registry status and bounded transcript tails.
A paused tool request estimates asking; hooks identify permission prompts exactly.
Transcript and report text is untrusted display data and cannot confer action
or read authority. Transcript paths are derived from home/cwd/session identity;
a reported `transcript_path` never chooses a file to read.

## Console Pi rows

Pi rows add `agent:"pi"` and `conversationId` (the header UUID); they omit
`claudeSessionId`. Their private identity hashes machine, `pi`, conversation
UUID, PID and process lifetime. Claude identities remain unchanged. They use
the same list, detail and normalized transcript-page shapes. Pi `message`
records map user and assistant text, `toolCall` arguments and `toolResult`
content to the existing entries. Latest-turn reads reset at user messages.
No ended Pi scan is performed.

Discovery correlates the default Pi transcript's encoded cwd and timestamp
with a verified Pi package bin, actual process cwd and kernel start. A session
without its first message is absent. Managed Pi UUIDs come from the private
`GET /managed-identities` `pi_session_ids`, beside `claude_session_ids`;
ancestry independently excludes the session host's children. No raw process
arguments or paths supplied by a caller choose a transcript. See
[Pi adoption](session-adoption.md#pi-adoption) for validation and read bounds.

Only an idle Pi row offers Adopt. Busy rows have `adoptReason:"busy"` and
cannot be adopted even with `interrupt:true`. An idle adoption uses only
`outsideId`; it retains the exact transcript under the existing project Pi
grants. Missing grants answer `409 transcript_not_granted`. Success returns
`{sessionId,agent:"pi",conversationId,interrupted:false,recoverySent:false}`.
The same conversation continues headless, with no automatic model turn.

## Codex daemon rows

A codex thread that the per-user codex daemon has loaded is a console row with
`agent:"codex"` and `conversationId` (the thread UUID); it omits
`claudeSessionId`. The name is "Codex — <folder>", `since` is the thread's
creation time and `state` is `idle` or `running`. Its private identity hashes
machine, `codex`, the thread id and the daemon's lifetime (the control socket's
physical path, device, inode and modification time), so a restarted daemon
makes new ids. The daemon holds the threads of the codex desktop app and of a terminal whose
codex matches the daemon's version; a terminal started with codex 0.162.1 runs
an in-process server and shows no row. The folder is `environments[0].cwd`,
else `cwd`, and the accepted sources are `cli` and `vscode`; a desktop-app
thread in a registered project shows Adopt, as a terminal thread does. A thread
with no turn is absent. Relay reads no codex file: the
detail's `lastTurn` is empty and the transcript page is empty, as for a missing
file. Row text never carries thread text, only folder, state and times.

One connection per probe lists the threads. Only the default control socket
under the home folder is read, and only when this user alone owns it and no
other account can replace it. An absent daemon, an unsafe socket or any daemon
error gives no codex rows, one Warn, and leaves the Claude and Pi rows intact.
Managed codex thread ids come from the private `GET /managed-identities`
`codex_thread_ids`; when they cannot be read, no codex rows show. Only an idle
row offers Adopt: a busy thread is `adoptable:false` with `adoptReason:"busy"`.
See [Codex attach](session-adoption.md#codex-attach).

## Ended sessions

An outside session that ended inside the window stays listed as a row with
`state:"ended"`, read from its transcript file. No process evidence exists for
it, so the row has `source:"probe"`, `outside:true`, `readOnly:true`,
`tmuxSession:null`, `viewable:false` and `viewReason:"not_in_tmux"`, plus
`endedAt`: the file's modification time, RFC 3339 UTC. `since` is the earliest
timestamp in the 16 KiB tail, an estimate, else `endedAt`. `updatedAt` is the
scan time. `name` is `Claude — <folder name>`.

`claudeSessionId` is the transcript file's stem, the Claude conversation id. It
matches `^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`, so it never starts with `-`; relay
does not require a UUID. List, detail and the WebSocket snapshot all carry it,
on live rows too. It opens no route in the scope: the scope has no terminal
start and no resume.

- **Window.** 3 days. `settings.json` `outside_sessions.ended_window_days`
  sets it: 0 to 30 whole days, and 0 turns ended rows off. Any other number
  lists none (fail closed) and logs a warning when the value changes. A hand edit
  applies through the settings-file watch. There is no CLI verb or screen.
  Rows older than the window drop every two seconds, by the clock.
- **Order and cap.** At most 50 ended rows across all machines, newest first,
  ties by `id`. One row per machine and Claude session id; the newest file wins.
- **Identity.** `outside:` plus the first 16 bytes, hex, of
  sha256(machine key, NUL, Claude session id, NUL, `ended`). It is stable across
  probes and never equals the session's live identity. It grants nothing.
- **Scan.** Each baseline runs one scan per machine after a successful probe, in
  the same shell path as the probe, and publishes once with the probe. A
  drain-triggered probe runs no scan. A failed scan keeps the machine's earlier
  ended rows and logs `outside ended scan failed` on change; an unsupported or
  removed machine loses its ended rows. The scan lists regular files
  `~/.claude/projects/<folder>/<id>.jsonl` modified inside the window, newest
  first, at most 50, and reads a 16 KiB tail of each. It refuses a symlink on
  the file, its folder, `~/.claude/projects` and `~/.claude`, and writes nothing.
  A file counts toward the 50 only when its tail holds a `"cwd"` key. When a
  record's `cwd` is a subfolder of the launch folder, the row's directory is the
  nearest parent whose folder name is the transcript's own; none matches, no row.
  An unreadable or missing file drops its row, never the list.
- **Scope.** The console scans every folder under its own
  `~/.claude/projects/`. A host scans only folders whose name starts with the
  mangled stored or resolved path of a project on that host, and keeps a row only
  when its directory lies inside such a project, as live rows do.
- **Directory.** The `cwd` of the newest tail record whose `sessionId` is the
  file's name and whose `cwd` maps back to this very file under the machine's
  home. No such record: no row. A forged `cwd` cannot point at another file.
- **Never both.** A file is not listed when its session id is held: seen on a
  live process (before project filtering), in the managed list, or in a registry
  entry whose pid is a live process. The snapshot checks again, so a resumed
  session shows once, as live. A deleted managed session no longer on the
  managed list can show as ended; the row is display data only.
- **Last turn.** The detail of an ended id reads the 1 MiB tail and keeps the
  last 8192 characters, as for a live row. A read failure answers 502.

Transcript text stays untrusted display data. The transcript page route does not
serve ended ids.

## Adopting a session

`POST /api/sessions/adopt` (class `execute`, socket only, absent from the
`chief-of-staff` scope) is the one door that acts on an outside session. It
ends the process of an interactive Claude the owner started in a terminal on
this Mac and resumes the same conversation as an ordinary headless relay
session, in the same folder, in the registered local project that holds it.
Sessions on SSH hosts are not adopted.

- **Eligibility** is static and shown on the row: `adoptable`, else
  `adoptReason`, the first that applies of `ended`, `host_unsupported`,
  `no_project`, `not_registered` (no registry entry), `invalid_id` (the
  `sessionId` is not a UUID), `start_mismatch`, `not_interactive`,
  `not_command_line`, `ambiguous` (two live entries hold one `sessionId`).
  Relay reads `~/.claude/sessions/<pid>.json` in Go: at most 512 regular files
  of at most 64 KiB, no symlink on the file or its two parent folders. A
  candidate needs a live pid whose kernel start time, to the second, equals the
  entry's `procStart` (UTC), `kind` `interactive` and `entrypoint` `cli`. The
  registry never names a path; the transcript path is derived. Raw pids, start
  times, `kind` and `entrypoint` never leave relay.
- **Idle check**, at request time: the entry's `status` is `idle`; its
  `statusUpdatedAt` is at most 2 s older than the newest transcript record;
  the 256 KiB transcript tail holds no `tool_use` without a `tool_result`; and
  the Claude process has no child that started inside the last turn's tool
  window: at or after the first `assistant` record that follows the last prompt
  of this run, and at or before the newest transcript record (a child a tool
  starts follows the assistant record that holds its tool call; an MCP server
  that spawns late, after a fast reply, is born after the newest record and
  never counts). A child that started in an earlier turn does not count, so a
  background shell left from an earlier turn does not stop an adopt; it cannot
  be told from an MCP server by start time or path, and the person who clicks
  Adopt decides. A resume does not restore background shells. One false veto
  remains: an MCP server that spawns after the first assistant record while
  that first turn still runs; adopt with `interrupt` gets through. A child
  skipped for starting after the newest record makes relay read the tail again;
  a difference or a failed read counts as a child. A child whose executable is exactly `/usr/bin/caffeinate` (the path the kernel ran, read from `kern.procargs2`, not `ps` comm; an unreadable path counts as a child), which
  Claude starts at a turn and keeps for minutes, never counts. When the 256 KiB
  tail holds no prompt and does not reach the file's start, relay finds the
  boundary by scanning the file forward; a failed transcript or process-table read counts as a
  child. A busy session is refused with `busy` unless the body sets `interrupt`.
- **The ladder.** Interrupt sends SIGINT and waits for exit, at most 5 s.
  Takeover then sends SIGTERM if the process lives, waits 5 s, sends SIGKILL,
  waits 5 s, else answers `stop_failed`. Each wait runs on the injected clock
  and is armed before its signal. Before each signal relay re-reads the pid's
  start time; a mismatch sends nothing and answers `changed`. SIGINT on an
  interactive Claude is undocumented, so the ladder does not depend on it.
- **Order.** Audit ready, row, fresh registry match, project, managed list,
  idle check, model, `AuthorizeLaunch`, the `session_adopt` intent row (and the
  `session_interrupt` intent for a busy session), the signals, the launch, the
  recovery message, the completion row. Every refusal up to the intent leaves
  the process untouched; with auditing off nothing is signalled.
- **The resumed session** runs `settings {"headless":true,"agent":true}` under
  the project's sandbox, allowed models and permission policy, with
  `--resume <claudeSessionId>`. The model is the body's `model`, else the
  family alias (`haiku`, `sonnet`, `opus`) of the newest assistant record, else
  the alias for a broker-served model. The newest assistant record's model id
  stands for its modelMap key (the id itself when it is a key, else every key
  whose target it is), read from a fresh catalog through the model endpoint's
  cache. A key maps to an alias when relay's environment sets
  `ANTHROPIC_DEFAULT_<ALIAS>_MODEL` to it. Exactly one distinct alias is the
  model, and the session starts with the alias, never the key or the target.
  Otherwise, or with an unreadable catalog, the door answers `model_unknown`
  before anything is signalled.
- **Recovery message.** After a takeover that sent SIGINT, relay sends one
  message with origin `relay-recovery` (no event line of its own; the
  `session.adopt` event covers it), quoting at most 300 characters of the
  last prompt and 500 of the open tool call's input, and asks the agent to
  check whether the call took effect.
- **Selectors.** `outsideId` is the row's id. `claudeSessionId` is the power
  path; with no live console row for it relay runs one console probe first.
  A codex thread is selected by `outsideId` only: `claudeSessionId` naming a
  codex thread and any `model` for a codex row answer `400 invalid_body`. A
  codex adopt attaches to the daemon, ends no process and answers
  `{sessionId,agent:"codex",conversationId,interrupted:false,recoverySent:false}`.

## Optional hooks

On each originating machine, configure Claude Code command hooks explicitly.
Relay never edits existing hook settings. Run `scripts/outside-session-report.sh`
with native Claude hook JSON on stdin. Install the helper at an
operator-chosen path and use that path in each hook command. It writes at most
64 KiB atomically into the owner-only spool, drops larger events, keeps at
most 256 reports there, and calls no Relay action API.

### Which spool

Each relay instance drains one spool on the console, so two instances on one
machine never take each other's reports:

- A relay started with `RELAY_OUTSIDE_SPOOL` set drains that directory. The
  value must be an absolute, clean path; any other value means the console
  spool is never drained, rather than a guessed one.
- A relay started without it, the installed tray included, drains
  `~/.claude/relay-outside-reports/`, as it always has.
- The helper writes to `RELAY_OUTSIDE_SPOOL` when its environment has it and
  to `~/.claude/relay-outside-reports/` otherwise; it refuses a relative or unclean path
  (trailing slash, `//`, `.` or `..` elements; exit 1 with one line on stderr),
  and a symlink on the spool or its parent. So whoever starts an outside
  Claude session meant for an instance with its own spool puts that
  instance's `RELAY_OUTSIDE_SPOOL` in the session's environment, and the
  session's hooks inherit it.
- SSH hosts always use `~/.claude/relay-outside-reports/` on the host.

Draining applies the same checks to either spool: no symlink on it or its
parent, owned by the user, mode 0700.

| Claude event | State |
|---|---|
| `SessionStart` | idle |
| `UserPromptSubmit` | running |
| `PreToolUse`, `PostToolUse` | running |
| `PermissionRequest` | asking |
| `Notification` with `notification_type:permission_prompt` | asking |
| `Stop` | idle |
| `SessionEnd` | refresh liveness and remove when exited |

Native stdin includes `hook_event_name`, `session_id`, `cwd`,
`transcript_path`, and optionally `notification_type`. Reports are drained
every two seconds, independently of baseline probes, but only for a machine
whose last good probe saw the spool and which has a live outside row; an
unreachable machine is never drained. A report applies only to a live row with
the same session id and cwd whose process still has the pid and start time the
probe observed; a gone or restarted pid removes the row. The console checks
those start times directly, without a full probe. A report for no live row, or
for a row just removed, runs one full probe first, so a session resumed in the
same folder still shows its first report; so does any report on an SSH host or
after a failed start-time read. The full probe rebuilds rows from pid and start
time before any report applies. The drain has already taken its reports from
the spool, so when that probe fails they wait, at most a minute, for the next
probe that finds the machine reachable. A report writes only state, source and
update time, never adoptability or identity. Forged, stale or mismatched
reports do not seed identities. Healthy report completion to snapshot delivery
must fit five seconds. Hookless changes use the next completed 15-second probe.

## Viewing

A terminal-owned session cannot be attached. For tmux, only actual default
inventory correlated with live pane/process ancestry can offer a target.
Registry or hook socket metadata cannot authorize custom sockets. Row viewing
is advisory; the read-only viewer door independently repeats exact inventory,
capability and host opt-in checks. Host discovery remains visible with
`show_all_tmux_sessions` disabled. See [SSH hosts](ssh-hosts.md).
