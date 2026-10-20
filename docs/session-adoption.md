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
| Stream-json over a child's pipes | headless Claude, local | n/a | no |
| Stream-json over `ssh -T` | headless Claude on a host | no | no |
| Stream-json under a detached runner on the host | headless Claude on a Unix host with `detach_sessions` | yes | yes |
| A pty through the shim | terminal sessions, local | n/a | no |
| A pty inside the host's tmux | a host terminal template with `persist` | yes | yes |

The Chief of Staff start has two paths. Headless launches and then sends the
prompt through the send door. Terminal launches with the prompt in argv. The
two paths have different refusals, different reach (a terminal cannot start on
a host, an entry is headless only) and different audit shapes. A session
started in terminal mode cannot be messaged by the Chief of Staff, and cannot
become headless.

Drop-in moves a headless Claude session into a terminal and hands it back, but
only for sessions relay started. A Claude, codex or pi session the owner began
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
6. **Adoption applies relay's policy.** An adopted session runs under its
   project's sandbox and permission policy. That is the purpose, not a side
   effect.

## The runner and the file channel

A runner is a small program that owns an agent's stdin and stdout. It is the
same program for every agent. It knows no protocol.

```
<state dir>/<session>/
  inbox/        one file per message to deliver, in order
  sent/         a message moves here when it is written to the agent
  out.jsonl     every line the agent printed, appended
  runner.json   pid, start time, generation, cursor
```

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
agent's own UI (terminal). `tmux respawn-pane -k` swaps one for the other.
Where a host has no tmux, a headless session still runs under the runner alone.

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

A session starts headless with a known conversation id. The Chief of Staff has
one start path. "Start in a terminal" becomes "start, then swap".

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
| Idle | registry `status` is idle and `statusUpdatedAt` is fresh; no unmatched `tool_use`; no child process | last turn record is `task_complete` | last message is an assistant message with `stopReason: stop` |
| Interrupt | SIGINT, then SIGTERM, then SIGKILL | `turn/interrupt` | rpc `abort`; SIGINT is undocumented (item 4) |
| Take over | stop the process; resume under the runner with `--resume <id>` | connect to the daemon's socket and drive the thread; stop nothing (open: a second client on a thread another client has open, item 5) | stop the process; resume under the runner with `--session <id>` and `--session-dir` |
| Not adopted | `kind` other than interactive, `entrypoint` other than the command line | a thread with a running turn and no interrupt consent | an ambiguous match |

Observed versions: Claude Code 2.1.304, codex 0.162.1, pi 1.0.2. Pi and codex
create their transcript at the first turn, so a session with no turn has
nothing to adopt and nothing to lose.

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
- Only sessions in a registered project are adopted, and a project's policy
  applies from the resume on.
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
   rpc `abort`, and a pi under the runner runs in rpc mode. Whether `--session`
   appends to the existing file is open: `--session` opens by path, id or
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
   README describes the terminal UI as running the server in-process. The
   codex adapter must not assume either until codex documents them. Sources:
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
