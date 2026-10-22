# Session adoption

One session model for every agent relay hosts, and a way to take over an agent
relay did not start. This note is the design the session-adoption epic builds.
Status: proposed. Items marked **unverified** were settled from the vendors'
documentation, not by testing against live sessions. The answers, and the
questions the documentation leaves open, are in [Vendor answers](#vendor-answers).

## Why

Relay reaches an agent in three different ways today, and each way has its own
start path, its own limits and its own way of losing the agent.

| Transport | Used by | Survives a dropped link | Survives a relay restart |
|---|---|---|---|
| Stream-json under the runner, local | headless Claude and codex, local (run directory under the config dir) | n/a | yes |
| Stream-json under the runner, local, ended with relay-sessions | pi, local; its model key ends with the launch | n/a | no |
| Stream-json over `ssh -T` | headless Claude and codex on a host with `detach_sessions: false` or a Windows host | no | no |
| Stream-json under the runner on the host | headless Claude and codex on a Unix host (default; `detach_sessions: false` gives `ssh -T`); Claude's runner is the pane of a tmux session `relay-agent-<id>` when the host has tmux 3.0 or later, codex's never is | yes | yes |
| A pty through the shim | terminal sessions, local | n/a | no |
| A pty inside the host's tmux | a host terminal template with `persist` | yes | yes |

The Chief of Staff start has two paths. Headless launches and then sends the
prompt through the send door. Terminal launches with the prompt in argv. The
two paths have different refusals, different reach (a terminal cannot start on
a host, an entry is headless only) and different audit shapes. A session
started in terminal mode cannot be messaged by the Chief of Staff, and cannot
become headless.

Drop-in moves a headless Claude, Pi or Codex session into a terminal and hands
it back, but only for sessions relay started or attached. A Pi drop-in opens
the project's `pi` template on the same session file. A Codex session relay
started opens `codex resume` on the same thread without the shared daemon. An
attached thread opens `codex resume` through the daemon's control socket with
no setting override, and the daemon and its other clients keep running. The
hand back resumes the same file or thread headless, an attached thread through
the adopt checks and a new durable intent. Pi and Codex drop-in is for local
projects only. A Claude, codex or pi session the owner began
in an ordinary terminal is visible (outside sessions) and read-only.

## Principles

1. **A session is a conversation, not a process.** Its identity is the agent's
   conversation id and its transcript file. Every agent relay supports keeps a
   transcript on disk and can resume from it.
2. **The driver is replaceable state.** A driver is whatever currently talks to
   the agent: a stream-json runner or a terminal UI. Swapping the driver keeps
   the session, its name and its transcript.
3. **One writer at a time.** While a terminal holds a session, sends refuse with
   `dropped_in`. Nothing types into a terminal UI on a program's behalf.
4. **The file channel is the control channel.** Messages in and events out move
   through files, so a lost connection loses nothing and every message has a
   receipt.
5. **The transcript is the source of truth for catch-up.** The live event stream
   is for latency. After a swap or a reconnect, state comes from the transcript.
6. **Takeover applies relay's policy; attach does not.** A session relay takes over (Claude, pi) runs under its project's sandbox and permission policy from the resume on. An attached codex thread keeps the policy it already runs under; relay changes none of its settings and claims no sandbox over it.

## The runner and the file channel

A runner is a small program that owns an agent's stdin and stdout. It is the
same program for every agent. It knows no protocol.

```
<state dir>/<session>/<generation>/
  inbox/        one file per message to deliver, in name order
  sent/         a message moves here when it is written to the agent
  out.jsonl     every line the agent printed, appended unchanged
  events.jsonl  one line per state change
  runner.json   pid, start time, session, generation, last delivered id
```

A message is `<id>.json` (starts a turn) or `<id>.ctl` (starts none: a
`control_response`, a JSON-RPC call). A sender writes `inbox/.<id>.tmp` and
renames it into `inbox/`; names starting with `.` are ignored. `events.jsonl`
holds `turn_started`, `turn_ended`, `asking` and `exited`, each with the byte
offset in `out.jsonl` of the line that caused it, appended before that line.
The runner matches two fixed strings the provider passes at start and parses
nothing else.

**The `agent` tool.** The runner speaks the owner's `agent` tool's file
channel: the same `inbox/` rename, name order and move to `sent/`. Relay does
not run the Python tool, which ships in no bundle, speaks Claude only and needs
python3, which a stock Mac host lacks. Relay's runner replaces its own
`in.jsonl` runner, so two programs speak one protocol and there is no third. The
runner does not use the tool's `state.json`, `meta.json`, heartbeat, `hold` or
`stop` files: relay-sessions keeps session state and stops by process group.

- `send` writes a file into `inbox/`. The runner writes it to the agent's stdin
  and moves it to `sent/`. The move is the receipt, so a send is delivered
  exactly when it returns.
- Readers follow `out.jsonl` by byte offset. A cursor survives a reader
  restart, so lines arrive once and in order.
- The runner outlives its caller. It is detached on Unix and runs in its own
  process group.
- The protocol lives in the provider, as today. Claude speaks stream-json,
  codex speaks the app-server JSON-RPC, pi speaks its rpc mode. The runner only
  moves lines.

**What tmux does.** tmux does not move files. It hosts the pane, gives the
session a stable name, lets a person attach, and lets the pane's program change
without losing the name. The pane runs either the runner (headless) or the
agent's own UI (terminal). `respawn-pane` swaps one for the other.

On a host with tmux 3.0 or later, a detached Claude session's runner is the
pane of the tmux session `relay-agent-<relay session id>`. The pane command
execs the runner, so `runner.json`'s `pid` is `#{pane_pid}` and equals its
`pgid`, and the stop check holds unchanged. The session is made once, with
`remain-on-exit on`, so a swap never loses it. The name never parses as a
persist name, so the persistent-session list and kill never touch it. Where a
host has no tmux 3.0, a headless session still runs under the runner alone and
one stderr line names the tmux version; its drop-in opens a terminal as before.
A delete ends the tmux session. Local sessions do not run in a pane.

## Swapping the driver

Headless to terminal:

1. Wait for idle: the last turn has ended and no tool call is open.
2. Interrupt if the owner asked not to wait, using the ladder below.
3. Stop the runner and its agent.
4. Start the agent's UI in the same pane with the agent's resume flag and the
   same conversation id.
5. Mark the session held. Sends refuse with `dropped_in`.

Terminal to headless is the reverse. The UI exits, the hold clears, the runner
starts, and the agent resumes from the transcript. This is what hand-back does
today, except the resume happens at once and not at the next message.

On a host with tmux, the swap is a pane swap. The handoff stops the runner and
the pane is dead. The drop-in terminal's remote command respawns that pane with
Claude's UI (`claude --resume <conversation id>`, from the host's `claude-code`
template command and args, never its `persist` wrapper, with the template's env
passed by `-e`) and attaches to the session. When the UI exits, the pane
command detaches every client, the terminal ends and the hand back runs. The
hand-back resume starts a new generation through the pane start: if the pane's
program still runs (the tab closed while Claude ran), the start sends `TERM`
to its group, `KILL` after 3 s, waits until the pane is dead, and respawns it
with the runner; if the tmux session is gone it makes it again. The result is
one tmux session, one conversation and a new generation. A swap back ends
Claude's UI, even when someone still uses it through `tmux attach` on the
host: the drop-in terminal is the handle of the swap.

Without a pane (a local session, or a host without tmux 3.0), the swap uses the
drop-in terminal, a pty that runs `claude --resume <conversation id>`. Closing
it hands the session back and relay resumes it at once, on the drop-in
caller's behalf, through the ordinary resume. Each direction writes a
`session_swap` intent row before it acts; with auditing off the swap is
refused and nothing resumes. Relay never swaps on its own after a start: the
owner's Drop in, or Take over after this turn, is the act. A relay restart
while a terminal holds a session drops the link, and the session needs a
Resume as before.

A session starts headless with a known conversation id. The Chief of Staff has
one start path. "Start in a terminal" becomes "start, then swap": a start with
`mode: terminal` runs the headless start and the answer says `headless`.

## Reading a session

- Live: follow `out.jsonl`.
- Catch-up and terminal mode: read the transcript. Deduplicate by record id
  when switching between the two.
- A pending prompt in terminal mode is visible only on screen. The pane capture
  is the fallback, and it is a hint, not a protocol.

## Adopting an agent relay did not start

Adopt takes an idle agent that someone started in an ordinary terminal and puts
it under a runner or its daemon, on the same conversation.

Flow: discover, check idle, interrupt if needed, take over, tell the agent what
happened.

### Per-agent adapters

| | Claude Code | Codex | Pi |
|---|---|---|---|
| Who owns the session | the process | a shared per-user daemon; the terminal UI is a client | the process |
| Find live sessions | `~/.claude/sessions/<pid>.json`: `sessionId`, `cwd`, `startedAt`, `procStart`, `status`, `kind`, `entrypoint` | the thread id in `~/.codex/thread-writer-locks/<id>.lock`, held open by the daemon; `threads` table in `state_5.sqlite` | none. Match the process start time and cwd to the transcript file name |
| Transcript | `~/.claude/projects/<encoded cwd>/<id>.jsonl` | `~/.codex/sessions/YYYY/MM/DD/rollout-<created>-<id>.jsonl`, created at the first turn | `<sessions dir>/<encoded cwd>/<start UTC>_<id>.jsonl`, created at the first turn |
| Idle | registry `status` is idle and `statusUpdatedAt` is fresh; no unmatched `tool_use`; no child started in the last turn's tool window | last turn record is `task_complete` | last message is an assistant message with `stopReason: stop` |
| Interrupt | SIGINT, then SIGTERM, then SIGKILL | `turn/interrupt` | rpc `abort`; SIGINT is undocumented (item 4) |
| Take over | stop the process; resume under the runner with `--resume <id>` | attach to the daemon's control socket and drive the thread; stop nothing, change no setting | stop an idle process; resume under the runner with `--session <exact file>` and `--session-dir <parent>` |
| Not adopted | `kind` other than interactive, `entrypoint` other than the command line | a busy thread, a subagent or ephemeral thread, one not from the command line, a changed or ambiguous match | a busy session, ambiguous match, custom root or missing transcript grant |

Observed versions: Claude Code 2.1.304, codex 0.162.1, pi 1.0.2. Pi and codex
create their transcript at the first turn, so a session with no turn has
nothing to adopt and nothing to lose.

### Pi adoption

Pi adoption is console-only and idle-only. It recognizes a kernel-reported
executable or node script whose resolved package bin is Pi: current
`@earendil-works/pi-coding-agent` or historical
`@mariozechner/pi-coding-agent`. A process name or filename suffix alone is
insufficient. The installed package metadata selects the bin, including the
current `dist/bundle/cli.js` entry. `omp` is a separate, unsupported fork.
Installed Pi can erase its script arguments by setting `process.title`. This
second recognition path requires kernel argv[0] exactly `pi`, every remaining
argument empty, and the kernel executable to be the same regular executable
file as the privately resolved operator Node runtime. A basename or title
alone is insufficient. Unreadable arguments and a different runtime refuse.
The runtime pin comes from the ordinary operator-selected Pi launcher,
including its supported Homebrew wrapper, verified package bin and Node
shebang. An env-node shebang uses the operator PATH; an absolute shebang uses
that exact executable even when PATH selects a different Node. Reads are
bounded to 64 KiB and four wrapper/entry stages. Unknown or compound wrappers
disable this recognition path; no shell text is evaluated. An empty private
Node pin also disables title recognition.
The same transcript, cwd, lifetime, ambiguity and managed-session checks still
apply. These observations correlate a process; they do not attest vendor code.


Discovery reads only `~/.pi/agent/sessions/--<encoded cwd>--/`. The header's
UUID, cwd and timestamp must derive the exact regular filename and correlate
with the process's kernel start (one second before through less than thirty
seconds after). It refuses links from home through the file, duplicate
process/file matches and custom `--session-dir` arguments. No transcript or
first conversation message means no row. Managed UUIDs and process ancestry
exclude Relay sessions independently; failed managed-identity reads list no
Pi sessions. At most 512 candidate files are inspected; headers are 64 KiB,
and streaming readiness reads bound each complete JSONL line to 8 MiB.

The readiness parser follows the last persisted leaf's `id`/`parentId` chain.
Only a last conversation message from the assistant with `stopReason: stop`,
no unmatched tool call and no working child is idle. Malformed, incomplete or
unreadable evidence never proves idle. Busy adoption returns `409 busy` even
with `interrupt:true`; outside Pi's stdin belongs to the terminal, so Relay
sends neither RPC nor SIGINT to it.

Authorization and a durable `session_adopt` intent precede identity-checked
TERM, the exit notification, and only if needed the bounded KILL escalation.
The existing project Pi grants must allow the original transcript; a missing
grant or explicit denial returns `409 transcript_not_granted` before stopping
anything. No grant is added. Resume passes the exact file and its parent to
`--session` and `--session-dir`; it does not copy, fork or truncate the file.
The host revalidates its default-root path and header. Provider readiness
confirms the same UUID and filename through RPC `get_state`. Adoption starts
no turn and sends no recovery prompt. A failed launch leaves the transcript
intact for manual resume.

The private provider state retains both `piSessionId` and `piSessionFile` for
later authorized resumes. Delete removes only that validated file. Ordinary
Pi sessions retain their existing Relay transcript storage and state support.
After adoption, managed interrupt uses RPC `abort`. Before delivery the
manager asks the authenticated built-in session host's `SessionInterrupt`
bridge door to persist an intent. An audit failure or absent handler refuses
before any state, tool or input change; the WebSocket returns
`{type:"error",code:"audit_unavailable",sessionId,message}`. An acknowledgement
certifies the durable intent, not successful delivery; delivery failures use
the provider's existing error handling. Ordinary Pi and Claude interrupts
retain their existing behavior.

### Codex attach

The per-user codex app-server daemon holds the threads of the codex desktop
app and of a terminal started with `codex --remote unix://` (0.163.0
observed); each is one client of it. Relay attaches as one more client. It
starts no codex, ends no process and changes no setting of the thread. A
terminal started without `--remote` (every codex 0.162.1 terminal, and a plain
0.163.0 one) runs an in-process server: its thread never reaches the daemon and
shows no row. A desktop-app thread in a registered
project shows Adopt, as a terminal thread does.

**Rows.** The thread folder is `environments[0].cwd`; when `environments` is
absent, null or empty, it is the top-level `cwd`. A thread with no absolute
folder shows no row. The accepted sources are `cli` and `vscode` (the daemon
reports `vscode` for a terminal and for the desktop app). A thread with a
`parentThreadId` is `not_interactive`; an ephemeral thread shows no row (the daemon
refuses its turn list); any other source
(`exec`, `mcp`, a subagent object) is `not_command_line`. The `thread/resume`
reply is checked against the session folder by the same rule.

**Socket.** Relay reads only `<home>/.codex/app-server-control/app-server-control.sock`.
No option, thread id, home or socket path reaches the door. An absent socket is
no daemon. The link and its folder must be owned by the effective uid. The
target must be a socket owned by the effective uid with no group or other
permission bits, in a folder owned by that uid or root that is neither group-
nor world-writable (unless sticky). Anything else is an unsafe socket and
gives no codex rows. Relay dials the resolved physical path, upgrades on path
`/` only (never `/daemon/shutdown`) and reads at most 16 MiB per message.
The daemon's identity (physical path, device, inode, modification time) is the
daemon's lifetime evidence: a restarted daemon is a different identity.

**Handshake and allowlist.** `initialize` carries exactly
`{"clientInfo":{"name":"relay","version":"1"}}`: no capabilities, because a
capability can change the daemon for later connections. Relay sends only
`initialize`, `initialized`, `thread/loaded/list`, `thread/read`,
`thread/turns/list`, `thread/resume`, `turn/start`, `turn/interrupt` and
`thread/unsubscribe`. Replies to the daemon's requests pass. No other method
leaves the process.

**Discovery.** One connection lists the loaded threads (at most 256; more is
an error) and reads each thread's metadata and its newest turn. A thread with
no turn shows no row. A row carries the folder, state (`idle` or `running`)
and the start time, never the thread's text. A thread is not adoptable when its
id is not a UUID (`invalid_id`), it has a parent thread
(`not_interactive`; an ephemeral thread shows no row), its source is not the command line (`not_command_line`),
or the daemon lists its id more than once (`ambiguous`). Idle needs thread
status `idle` and a newest turn that is not in progress; a busy thread is
`adoptable:false` with `adoptReason:"busy"`. A daemon error, an absent daemon
or an unsafe socket gives no codex rows, one Warn, and leaves the Claude and Pi
rows as they are. Managed codex thread ids are excluded; a failed read of them
gives no codex rows. A codex row has no transcript: its latest turn and its
transcript page are empty.

**Adopt.** The row is selected by `outsideId` only; a `model` is refused
(`400 invalid_body`) because attach sends none. In order: audit ready, host
ready, the row, a fresh discovery with exactly one candidate of the same id,
daemon and folder, a local registered project, the managed codex ids and a
fresh idle check. A busy thread is `409 busy`, also with `interrupt:true`, and
relay sends the daemon nothing that changes it. Launch authorization is the
same decision as a codex launch in the project, with the thread's model. Then
the durable `session_adopt` intent row (`method:"attach"`,
`relay_sandbox:false`), a last check, the host launch and the completion row.
The launch carries the thread id privately to the session host, which accepts
it only for a local, headless codex launch with a UUID, fresh or the resume
described below. Each audit row of an attach, the launch row included, says
`sandbox:false`. An attach that
fails after the intent answers `502 launch_failed`: "relay could not attach to
the codex thread; nothing in codex changed".

**Provider.** The session host's codex provider, seeded with the thread id and
`daemonAttach`, never starts a program and never uses the runner, launcher,
sandbox profile or launch identity. It dials the daemon, requires the thread in
`thread/loaded/list`, then sends `thread/resume` with the thread id alone. The
reply must name the thread and the session's folder. The state follows
`thread/status/changed` for its thread (`active`, `idle`, `systemError`);
other threads' notifications are ignored. `thread/closed` or a closed socket
ends the provider, and the session reads dormant; there is no reconnect loop.
Relay answers a server request only when it names the session's thread and the
turn that relay's own `turn/start` opened, while that turn is open; a request
that names no turn counts only while relay's turn is the open one. The answer is
a decline and none is approved. Any other request, a terminal's turn included,
gets no answer from relay and one Info line with its method and turn id, never
its text, so codex's own prompt stays with the terminal user.

**Interrupt, delete, resume.** Stop sends `turn/interrupt` only after the
manager's `SessionInterrupt` gate persisted a `session_interrupt` intent row
(`method:"turn/interrupt"`); an audit failure leaves the turn running and the
WebSocket answers `audit_unavailable`. Kill, release and delete send a
best-effort `thread/unsubscribe` and close the connection. They send no
`turn/interrupt`, delete no thread, archive nothing and remove no folder.

**Resume of a dormant attached session.** The ledger record keeps the thread
(`codex_thread`). `POST /api/sessions/{id}/resume` and `relay session resume`
run the adopt checks again, in this order: audit ready (`503 audit_unavailable`),
host ready (`502 unavailable`), the record's project is a registered local
project that holds its folder (`409 no_project`), a fresh discovery of exactly
one candidate with the same thread id (none is `409 ended`, several `409
ambiguous`) in the same folder (`409 changed`) that is adoptable, no other live
session holding the thread (`409 managed`), an idle check on the same daemon
(`409 busy`, or `409 changed` for another daemon or a changed thread), the
thread's model (`409 model_unknown`) and launch authorization with `Resume:true`.
Then the durable `session_adopt` intent (`method:"reattach"`, `session_id`,
`relay_sandbox:false`), a last check, the host launch and the completion. A
daemon that cannot be read before the intent is `502 unavailable` and writes no
intent; a failure after it is `502 launch_failed`. A daemon restart does not
block the resume: discovery and the check both see the current daemon. The
session host refuses (`ErrAttachRequired`, `400 invalid_spec`) to resume a stored
attached codex session unless the request carries `attach_codex_thread` for the
same thread, so nothing resumes the thread around the intent. The row of a
dormant session is hidden from the outside list, so the resume is the one door
back; relay never starts its own codex for it.

**Not covered.** A custom `CODEX_HOME`, a terminal UI running its own embedded
server (every terminal started without `--remote unix://`), SSH hosts, `codex exec`,
MCP and subagent threads, ephemeral threads, and every thread source other than `cli` and `vscode`. A codex terminal
UI opened inside a relay terminal session that joins the daemon lists like any
daemon thread. After attach, Eve shows turns from the attach on.

### A Claude session in no project

A live console Claude row whose folder no registered local project holds is
adoptable. The adopt resumes the same conversation headless in that folder, and
the session runs **unsandboxed**. It is the owner's choice: the owner sees the
Adopt button and takes no extra confirmation. Pi and Codex rows in no project
stay `adoptable:false` with `adoptReason:"no_project"`: Pi needs a project for
its model key and its transcript grant, and a Codex attach needs one for launch
authorization on attach and reattach.

What an unsandboxed session has and lacks:

- No sandbox profile, no project permission policy, no allowed-models check, no
  template allow-list, no model key and no launch identity. It runs as the
  account that runs relay, with that account's reach.
- A tool call that needs approval still asks the owner through relay's
  permission hook. The hook finds the session by its runner's process tree, not
  by a launch identity.
- Relay tools are absent: the session has no launch identity and no
  `useRelayTools` setting, so a relay tool call fails closed.
- It gets the `claude_token_file` token, when set, on fd 5 through the shim
  (the shim runs with no profile), as a sandboxed Claude does. The process
  could read the file by path, so it gains no reach. `CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=5`
  names the descriptor; the token is never in the environment.
- Being headless sets no `bypassPermissions` on it. It runs the mode its
  settings name (`default` after an adopt), so a tool call that needs approval
  asks the owner. Only the owner's `relay session mode` sets bypass.
- `relay session mode` answers `resume_required` for it when `claude_token_file`
  is set, as for a sandboxed session: the token is per launch and cannot be
  reused by a restart in place. With no token set it restarts in place.

Adopt order for such a row: audit ready (`503 audit_unavailable`), host ready,
the row, a fresh candidate, then the folder is matched against the registered
local projects again (a match is `409 changed`: adopt again from the refreshed
row, sandboxed; relay never picks unsandboxed over a project), then the folder
must be an existing directory after symlinks resolve (`409 directory_unusable`,
no further trust rule, so the home folder or `/` is allowed), then the managed
list, the idle check, the model, launch authorization, the durable
`session_adopt` intent, the interrupt, the take over, the launch and the
recovery message. Nothing is signalled before the intent is durable. The `201`
body adds `"unsandboxed": true`.

The ledger record carries `unsandboxed: true` and an empty `project_id`, and the
session host keeps the mark on the session across a relay-sessions restart.
`GET /api/sessions` lists the session with `unsandboxed: true` and `projectId:""`,
and `GET /api/terminals` lists its drop-in terminal with `unsandboxed: true`; no
other session or terminal carries the field. A resume of the dormant session, a
drop-in and the hand-back resume run unsandboxed in the same folder, and each
refuses with `409 directory_unusable` when the folder is gone, before anything
stops. A project registered for the folder later does not change the record: it
stays unsandboxed and keeps its chip; delete the session and adopt again to
sandbox it. After a relay restart the rebind of such a session is refused with
the reason `unsandboxed` (it has no launch identity to bind).

Audit rows of the session record that no project policy or sandbox applied:
`session_adopt`, `session_interrupt`, `session_swap` and `session_message` carry
`no_project:true` and `relay_sandbox:false` with no `project_id`;
`session_launch` and `session_resume` carry `sandbox:false` and `no_project:true`.

### Sessions on SSH hosts

A live Claude row of a Unix SSH host is adoptable, in its host project. The
adopt ends the terminal Claude on the host and resumes the same conversation
headless there, in the row's folder, as `claude --resume <conversation id>`
under the host project's policy. A host session has no sandbox, no launch
identity and no `unsandboxed` field. On a host with tmux 3.0 or later the agent
runs in the tmux session `relay-agent-<session id>`; without it, under the
plain runner. A terminal Claude that ran in a tmux pane ends, and the adopt
starts a fresh `relay-agent` session: it does not reuse the pane.

**Eligibility** is the probe's: the registry tests of any row, a host with
`detach_sessions` absent or `true` (a host that ends its agent with the link
cannot hold a detached script), and a host project that allows `claude-code`.
The door tests it again, fresh, at request time.

**Order.** Audit ready, host ready, the in-flight lock, a fresh eligibility
test, the managed list, a read of the transcript tail over SSH (the same
256 KiB window a local adopt reads), the verdict, the model, launch
authorization, the `session_adopt` intent, the `session_interrupt` intent when
the adopt interrupts, the dormant ledger record, then one SSH command. Nothing
reaches the host before the intents and the record are durable. The verdict is
busy when the row's state is not `idle` or the tail holds an open tool call; a
busy session without `interrupt` answers `409 busy` and nothing is sent. The
model is the body's `model`, else the family alias of the newest assistant
record, else none: a host adopt never answers `model_unknown`.

**The script.** One SSH command writes `adopt.sh` beside the session's run
folders on the host (`~/.relay/agents/<session id>/adopt.sh`, with `adopt.state`
and `adopt.log`), starts it detached (`setsid`, else `set -m`, HUP ignored) and
prints each new `adopt.state` line behind `@@RELAY_ADOPT@@` until the terminal
one. Relay sends no signal over SSH itself. The script:

1. computes the pid's lifetime exactly as the outside probe does and compares
   it with the row's, byte for byte. A gone pid goes to the start. A different
   lifetime before the first signal answers `refused changed` and sends
   nothing;
2. reads `~/.claude/sessions/<pid>.json` (no symlink, at most 64 KiB) and
   requires its `sessionId` to be the conversation, else `refused changed`;
3. tests idleness unless the adopt interrupts: the registry status is not one
   the probe reads as running or asking, the transcript is the size the tail
   read saw, and the process has no child but the turn helper
   (`/usr/bin/caffeinate`) that started in the last turn's window. The door
   sends the window, or tests every child when the tail cannot give a turn
   boundary. Not idle answers `refused busy`;
4. sends SIGINT when the adopt interrupts, then SIGTERM and SIGKILL as needed,
   with the local ladder's 5 s waits, re-reading the lifetime before each
   signal. A mismatch or a gone pid after a signal ends the ladder and goes to
   the start. A process alive after the SIGKILL wait answers
   `failed stop_failed`;
5. runs the same start as a detached session (`failed launch_failed` when it
   fails) and writes `ok <generation>`.

`adopt.state` holds each `signal INT|TERM|KILL <unix ms>` line and exactly one
terminal line: `ok <generation>`, `refused busy`, `refused changed`,
`failed stop_failed` or `failed launch_failed`.

**Answers.** `ok` is `201`; the live record replaces the dormant one, and an
adopt that interrupted sends the recovery message. `refused busy|changed` and
`failed stop_failed` answer `409 busy`, `409 changed` and `502 stop_failed` and
remove the dormant record. `failed launch_failed` answers `502 launch_failed`
and keeps the record. When the SSH command ends with no terminal line, relay
answers `502 host_link_lost` and keeps the dormant record: the script may still
run, and it ends the outside process and starts the agent, or refuses, on its
own. The audit completion of a host adopt carries `interrupted` and no
`signals`; its rows carry `host_id`.

**The dropped link.** The record lists as dormant, with Resume. A resume of it
asks relay-sessions to attach a live `relay-agent-<id>` run first, as for any
detached session. With none it runs the script again with `no_signal`: a pid
alive with the pinned lifetime answers `409 changed`, starts nothing and removes
the record ("the terminal session still runs; adopt it again"), and anything
else starts `claude --resume` on the host. A resume never starts a Claude beside
the outside process. If the link returned and the script had already finished,
its agent is the run that Resume attaches. A relay-sessions restart does not
reattach an agent that was never saved, so Resume does it.

An adopt that takes longer than relay's 15 s wait for relay-sessions ends the
same way: `host_link_lost`, a dormant record, and Resume attaches what the
script started.

### The idle check

Several signals combine. A signal can veto idle. Only the agent's own state can
confirm it.

- Confirm: the agent's status field or turn-boundary record says idle.
- Veto: an open tool call in the transcript, a child process of the agent, or
  high CPU.
- Low CPU never confirms. An agent waiting on the model or a long tool uses
  almost none.

Input typed but not sent is invisible to every signal. A person is the owner of
that input. Adoption by a button is a decision by that person.

### The interrupt ladder

Waiting for idle is the default. When the owner asks not to wait, or a timeout
passes:

1. Interrupt as the agent's own stop (SIGINT, or `turn/interrupt`). The agent
   ends the turn and records the interruption.
2. Wait a bounded time for the agent to go idle.
3. SIGTERM, wait, then SIGKILL.

An interrupt loses the turn in flight and any session-only state: background
shells, subagents, permission grants made for the session. It cannot undo side
effects that already happened: a half-run command, a half-written file.

### The first message after a takeover

After a takeover that interrupted a turn, relay sends the agent one message,
marked with its origin, built from the transcript tail: what it was doing, which
tool call was open, and an instruction to check the state of anything that call
touched before continuing. The same message follows a crash resume.

### Crash and adoption are one operation

An agent that exits unexpectedly (an API failure, a kill) is resumed from its
transcript by the same code path. The supervisor sees the exit, and resumes
with the recovery message.

## Authority and audit

- Adopt, interrupt and swap are owner actions in this design. The Chief of
  Staff gains no new authority. Whether it may adopt on its own is an open
  question in the epic.
- A Claude or Pi session is adopted in a registered project, a Claude session also in no project (see [A Claude session in no project](#a-claude-session-in-no-project)). A project's policy applies from the resume on to a session relay takes over, not to an attached codex thread.
- Every adopt, interrupt, swap and recovery message writes an audit row before
  it acts, as every other launch and send does.
- Transcript and registry text is untrusted display data and grants no
  authority. A registry file never chooses a path to read. The path is derived
  from the home folder, the working folder and a validated id.
- A process whose start time does not match its registry entry is not adopted.
- An ambiguous match is never adopted.

## Local and remote

The same adapters run on a host over ssh, using the same reads the outside
probe already uses. The runner and tmux are installed or probed per host, as
`node`, `claude` and `tmux` are today.

**Windows** has no tmux, no SIGINT for a non-console process and no detached
runner today. The runner is plain standard-library code and should work. The
terminal path and the interrupt need a decision (see open questions).

## Vendor answers

Each unverified item, answered from the vendor's documentation, changelog or
published source. Nothing here was tested against a live session. "Open" means
the documentation does not say, and the dependent code must not assume an
answer.

1. **Does `--resume` keep the same conversation id and transcript file?**
   Yes, by implication. `--fork-session` is documented as "when resuming,
   create a new session ID instead of reusing the original", so plain
   `--resume` and `--continue` reuse the id. The sessions page says two
   terminals that resume one session without forking interleave into one
   transcript, so the file is shared and appended to. `--session-id <uuid>`
   sets the id at start, which is how a session starts headless with a known
   id. No page states in so many words that a resume appends to `<id>.jsonl`.
   Sources: [CLI reference](https://code.claude.com/docs/en/cli-reference),
   [Sessions](https://code.claude.com/docs/en/sessions).

2. **How does Claude Code handle an interrupted turn on resume, and a tool call
   with no result?** Documented for a crash or kill. A tool that was still
   running when the previous process ended is not run again on resume. Claude
   sees the call marked as cut off before its result was recorded, and is told
   to check whether it took effect. `CLAUDE_CODE_RESUME_INTERRUPTED_TURN`
   changes this: the interrupted turn runs again after a restart. The
   changelog records fixes to this path in 2.1.296, 2.1.299 and 2.1.300
   (finished turns re-run, a resumed subagent told a tool call was rejected,
   the first entry after a resume lost when a crash cut the last line short).
   An interrupt by SIGINT, as distinct from a kill, is open. The recovery
   message in this design repeats Claude's own "check whether it took effect"
   instruction, so it does not depend on the answer. Sources:
   [Sessions](https://code.claude.com/docs/en/sessions),
   [CHANGELOG](https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md).

3. **What is the registry's `messagingSocketPath` for, and does it offer a
   stop?** The field and the `~/.claude/sessions/<pid>.json` file are not
   documented. The documented feature it most likely names is the per-session
   inbox socket for cross-session messaging: a Unix socket, shown in `/status`
   as `uds:<path>`, and given to hooks and Bash commands as
   `CLAUDE_CODE_MESSAGING_SOCKET` with a token in
   `CLAUDE_CODE_MESSAGING_TOKEN`. It carries plain text only. A message is read
   between tool calls, a running tool is never interrupted, and a message
   cannot run a command or answer a prompt. It offers no stop. `claude stop`
   and `claude respawn` apply to background sessions only. Adoption keeps the
   signal ladder for Claude. Sources:
   [Cross-session messaging](https://code.claude.com/docs/en/cross-session-messaging),
   [Environment variables](https://code.claude.com/docs/en/env-vars),
   [CLI reference](https://code.claude.com/docs/en/cli-reference).

4. **Does pi stop its turn cleanly on SIGINT, and does `--session` append to the
   same file?** SIGINT is open: the docs name no signal behaviour in either
   mode. Interactive pi stops a task with Escape. Rpc mode has a documented
   `{"type":"abort"}` command that aborts the current operation and answers
   once the session is idle, plus `abort_retry` and `abort_bash`. Closing the
   child's stdin asks for an orderly shutdown. Pi's interrupt is therefore the
   rpc `abort`, and a pi under the runner runs in rpc mode. `--session`
   appends to the existing file (confirmed by installed Pi 1.0.0
   `SessionManager.open`, which sets the existing file as flushed and appends).
   The CLI accepts a path, id or partial id: `--session` opens by path, id or
   partial id, `--session-dir` overrides storage and lookup, and only
   `--fork` is documented as making a new session. The layout is documented:
   `~/.pi/agent/sessions/--<path>--/<timestamp>_<id>.jsonl`, the path with its
   leading separator dropped and `/`, `\` and `:` replaced by `-`. Sources:
   [usage](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/usage.md),
   [rpc](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/rpc.md),
   [rpc commands](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/rpc-commands.md),
   [cli](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/cli.md),
   [session format](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/session-format.md).

5. **Can a second client drive a codex thread another client has open, and
   what is the daemon socket's handshake?** The handshake is documented. Each
   connection sends one `initialize` request with client metadata, then an
   `initialized` notification. A request before that gets `Not initialized`;
   a second `initialize` gets `Already initialized`. `--listen` takes
   `stdio://` (the default, newline-delimited JSON), `unix://` or
   `unix://<path>` (WebSocket over a Unix socket, the default control socket
   when no path is given), an experimental `ws://`, or `off`. `thread/resume`
   reopens a stored thread, `thread/start` subscribes the caller to the
   thread's events, `turn/interrupt {threadId, turnId}` ends a turn with
   status `interrupted`, and a loaded thread with no subscribers and no
   activity for 30 minutes closes. Subscriptions are per connection, which
   implies several clients, but a second client driving a thread another
   client has open is open. So is a shared per-user daemon: the docs name the
   default control socket and say nothing of its lifecycle, and the app-server
   README describes the terminal UI as running the server in-process. Codex
   0.162.1 does run a shared per-user daemon, and the version-pinned source
   answers the second-client question: the
   [daemon README, lines 22-24](https://github.com/openai/codex/blob/rust-v0.162.1/codex-rs/app-server-daemon/README.md#L22-L24)
   says shared clients use the daemon's inherited environment, with no
   per-client isolation, and the
   [running-thread resume path, lines 4348-4397](https://github.com/openai/codex/blob/rust-v0.162.1/codex-rs/app-server/src/request_processors/thread_processor.rs#L4348-L4397)
   retains a loaded thread's settings and ignores conflicting overrides while
   another subscriber remains. A second subscriber therefore resumes a loaded
   thread and keeps its settings (the daemon of codex 0.163.0 behaves the
   same, read-only probe, relay#575), so relay attaches, sends no setting and
   claims no project sandbox over the thread. Sources:
   [app-server](https://github.com/openai/codex/tree/main/codex-rs/app-server),
   [app-server docs](https://learn.chatgpt.com/docs/app-server.md).

6. **Does the permission hook's ancestry check hold when the agent runs under a
   tmux pane?** Yes, if the pane's own process is the root. Claude's hooks
   page says hooks inherit Claude Code's environment and run in their own
   session with no controlling terminal; the exec form spawns the command
   directly and the shell form goes through `sh -c`. So a hook is claude's
   descendant whatever claude's own parent is. The docs say nothing of tmux.
   Relay's walk ([session-host.md](session-host.md#identity-whose-call-is-this))
   goes from the hook up to a known root, matched on pid and start time. A
   tmux pane's process is a child of the tmux server, not of relay or of the
   attaching client, so the walk never reaches relay's launch. It holds when
   the root relay records is the process relay starts in the pane (the runner
   or the shim), with its pid and start time read when the pane starts. A
   root above the tmux server fails closed: the walk answers `403` and the
   hook denies. Sources: [Hooks](https://code.claude.com/docs/en/hooks).

7. **Does `omp` follow pi's layout?** In shape, not in detail. `omp` is
   oh-my-pi, a fork of pi (npm `@oh-my-pi/pi-coding-agent`). Its home is
   `~/.omp/agent/`, not `~/.pi`. Sessions are
   `~/.omp/agent/sessions/<encoded cwd>/<timestamp>_<id>.jsonl`, with a
   different cwd encoding: `-<relative>` under the home folder,
   `-tmp-<relative>` under the temp folder, and `--<encoded absolute>--`
   elsewhere. Ids are UUIDv7. `omp --mode rpc` speaks NDJSON with `abort`,
   `abort_and_restore_queue`, `abort_retry` and `abort_bash`, and adds
   `--mode rpc-ui`. `--session` matches an id or file-name prefix,
   case-insensitive. `--session-dir` appears only in the rpc `open_session`
   description. An omp adapter is a separate adapter with its own path
   encoding, not pi's with a different root. Sources:
   [oh-my-pi](https://github.com/can1357/oh-my-pi),
   [session](https://github.com/can1357/oh-my-pi/blob/main/docs/session.md),
   [rpc](https://github.com/can1357/oh-my-pi/blob/main/docs/rpc.md).

## Out of scope

- Agents relay does not support (a new provider is a separate story).
- Moving a conversation between machines.
- A shared view of several clients on one terminal, beyond what the tmux viewer
  already offers.
