# PTY scheduled tasks with replayable output in Eve

## Context

Today `relayScheduler` can only fire **headless chat tasks**: it POSTs `/api/sessions` to relayLLM with `settings:{headless:true}`, sends a prompt, and logs the response. Eve "view last run" clicks join the saved `lastSessionId` and renders the transcript.

We want to schedule **PTY/terminal tasks** (e.g. `npm run test`, `yarn deploy`, ad-hoc shell pipelines) and let the user click a completed task in Eve to view what happened — colors, cursor moves, the lot.

relayLLM already has full PTY infrastructure (`POST /api/terminals`, `terminal_session.go`, WebSocket `join_terminal`, template substitution), but:

1. Scrollback is in-memory only (100KB ring, `terminal_session.go:19`) — nothing on disk.
2. Scrollback is only retrievable via WebSocket attach; there is no HTTP fetch endpoint.
3. Sessions vanish on relayLLM restart or after idle timeout.
4. The scheduler has no concept of PTY at all — `Task` and `executeTask` are hard-wired to headless chat.

**Intended outcome:** scheduled PTY tasks fire on time, their output is persisted to disk in relayLLM and replayed in Eve via xterm.js when the user clicks "view last run". Existing headless tasks are unaffected.

---

## Design decisions (confirmed with user)

| Decision | Choice |
|---|---|
| Done detection | Scheduler opens a WebSocket to relayLLM, attaches via `join_terminal`, waits for `terminal_exit`, captures exit code. Exit 0 = success, nonzero = error. |
| Eve render | Read-only xterm.js replay of raw bytes (preserves ANSI/colors). |
| Command source | Template + per-task `extraArgs` override (appended to template `Args` after substitution). |
| Log cap | 1 MB per session on disk, **head/tail layout** (not naive drop-oldest). |
| Per-task timeout | Mandatory wall-clock cap (`MaxDurationSeconds`, default 30 min). |

### Why head/tail instead of "drop oldest bytes"
ANSI streams establish state at the start (cursor home, screen clear, SGR resets). Lopping bytes off the front of an ANSI stream produces garbled colors and a misplaced cursor on replay. Instead:

- Capture the first ≤64KB into `{id}.head.log` once at session start, then close.
- Stream everything else into `{id}.tail.log`, rotating when it exceeds (1MB - 64KB).
- Replay = `head` + a `[…log truncated…]` marker + `tail`.

Head preserves the program's initial mode-setting; tail preserves recent output.

### Exit code conventions
- `0..255` — real process exit codes.
- `-1` — session lost (relayLLM restart / WS dropped, terminal not found on rejoin).
- `-2` — scheduler-side timeout (`MaxDurationSeconds` elapsed).
- `-3` — terminal create failed.

---

## File-level changes

### relayLLM (/Users/jonathan/source/barelyworkingcode/relayLLM)

- **`terminal_session.go`** — add `extraArgs []string`, `logHeadPath`, `logTailPath`, `logHead *os.File`, `logTail *os.File`, `headBytes int`, `tailBytes int` to `TerminalSession`. In `Start` (line 54), append `s.extraArgs` to resolved args (after `subs.expand` so substitution applies to overrides too) and open both log files (`0600`). In `readLoop` (line 122), tee each chunk to scrollback **and** to the log: fill head first (up to 64KB, then close head file), then append to tail; when tail crosses (1MB - headBytes), rename `tail.log` → `tail.log.old`, reopen fresh `tail.log`. On replay we read `head + marker + tail.log.old? + tail.log`. `Close` flushes and closes both files but leaves them on disk.

- **`terminal_manager.go`** — `NewTerminalManager` takes a new `dataDir` param. `Create` (line 39) takes `extraArgs []string` and sets `session.logHeadPath`/`logTailPath` under `{dataDir}/terminal_logs/`. Add `SweepOldLogs(maxAge, maxTotalBytes)` GC — delete files older than 30 days; if total still over 500 MB, delete oldest until under cap. Called from a daily goroutine.

- **`api.go`** — `POST /api/terminals` body adds `ExtraArgs []string`; passed through to `Create`. New handler `GET /api/terminals/{id}/log` (around line 286) returns the stitched head+tail bytes as `application/octet-stream`. Works whether the session is live, exited-but-resident, or gone. 404 only if no log file exists. Reuses existing bearer middleware.

- **`ws.go`** — `handleCreateTerminal` (line 444) accepts `ExtraArgs` for parity with HTTP.

- **`main.go`** — `os.MkdirAll({dataDir}/terminal_logs, 0700)` on startup; pass `dataDir` to `NewTerminalManager`; spawn daily log-sweeper goroutine.

- **`CLAUDE.md`** — document the new endpoint and on-disk log layout.

### relayScheduler (/Users/jonathan/source/barelyworkingcode/relayScheduler)

- **`task.go`** — `Task` struct (line 6) gains: `SessionType string` (`"headless"` default, `"pty"` for new), `TemplateID string`, `ExtraArgs []string`, `Directory string` (optional override), `MaxDurationSeconds int`, `LastTerminalID string`. `Execution` struct (line 60) gains: `TerminalID string`, `ExitCode *int` (pointer so 0 is distinguishable from absent), `ExitSignal string` (optional). All fields zero-valued for old tasks — no migration needed.

- **`client.go`** —
  - Add `CreateTerminal(project, templateID, name, extraArgs)` posting to `/api/terminals` with `{templateId, name, directory, cols:120, rows:30, extraArgs}`. Returns `{terminalId, state}`.
  - Add `AttachTerminalAndWait(terminalID, timeout) (exitCode int, err error)`: opens WebSocket to `${relayLLM}/ws`, sends `{"type":"join_terminal","terminalId":id}`, reads frames until `type:"terminal_exit"` (capture `exitCode`), `error`, or `timeout`. On WS error or "terminal not found": fall back to polling `GET /api/terminals/{id}` every 3s until state==`stopped` (got exit code) or 404 (return -1). `gorilla/websocket` is already in `go.mod` via `hub.go`.
  - Add `GetTerminalLog(id) ([]byte, error)` → `GET /api/terminals/{id}/log`.
  - Add `CloseTerminal(id)` → `DELETE /api/terminals/{id}`.

- **`scheduler.go`** —
  - Generalize the "end previous session" step (line 221): dispatch on `task.SessionType` → `EndSession(prevSessionID)` for chat, `CloseTerminal(prevTerminalID)` for PTY. Wait for ack (~2s budget) so back-to-back fires don't race.
  - `executeTask` (line 211) branches on `task.SessionType`:
    - `"headless"` (or empty): existing path unchanged.
    - `"pty"`: resolve project → `CreateTerminal` → persist `LastTerminalID` via new `store.SetLastTerminalID` → broadcast `task_started{terminalId}` → `AttachTerminalAndWait(MaxDurationSeconds or default 1800s)` → on exit, fetch last ~16KB of log for `Execution.Response` preview → log Execution with `TerminalID`, `ExitCode`, status (`success` if exit==0, `error` otherwise, `timeout` if -2) → broadcast `task_completed`.

- **`store.go`** — add `SetLastTerminalID(id, terminalID)` mirror of `SetLastSessionID`.

- **`api.go`** — task create/update validation branches on `SessionType`: chat requires `Prompt` + `Model`; PTY requires `TemplateID`. Both require `ProjectID`.

- **`hub.go` / `scheduler_ws.go`** — task lifecycle broadcasts include `terminalId` when present; existing `sessionId` field remains for chat tasks.

### eve (/Users/jonathan/source/barelyworkingcode/eve)

- **`public/dialogs/task-dialog.js`** —
  - `_renderNewForm` (around line 140): add a "Type" radio at the top (Chat / Terminal). When Terminal: hide Prompt + Model; show Template `<select>` (populated from `/api/terminal/templates`), `extraArgs` text input (split on whitespace into array), optional Directory override, Timeout (minutes) field.
  - Payload assembly (around line 341): include `sessionType`, `templateId`, `extraArgs`, `directory`, `maxDurationSeconds` when terminal.
  - "View Last Run" handler (around line 110): branch on `task.sessionType`. If `pty`: open the read-only PTY viewer with `task.lastTerminalId`. Else: existing `joinSession(lastSessionId)`.

- **`public/terminal-manager.js`** — add `viewReadOnly(terminalId)`: try `join_terminal` over the existing WS first; on `error: terminal not found` fall back to `GET /api/terminals/{id}/log` and feed bytes to xterm via `term.write` (chunked at 64KB).

- **`public/app.js`** — add `joinTerminal(terminalId, {readOnly:true})` companion to `joinSession`.

- **`public/message-dispatcher.js`** — handle `lastTerminalId` updates on `task_started` / `task_completed`.

- **`public/core/state-store.js`** — track `taskTerminalIds` alongside `taskSessionIds` so terminal IDs are tagged as task-owned in the sidebar.

- **`public/sidebar/project-tree-item.js`** — branch on `task.sessionType` when rendering "view last run".

### relay (/Users/jonathan/source/barelyworkingcode/relay)

No route changes required. The existing `/api/*` and `/ws` proxy already covers `/api/terminals/{id}/log`. Confirm no path-specific middleware strips the bearer token (`frontend_proxy.go:57`).

---

## Reused existing functions / utilities

- `pty.StartWithSize` and the read loop (`terminal_session.go:100`) — unchanged; we just add a tee.
- `subs.expand` (template substitution at `terminal_session.go:67`) — re-used for `extraArgs`.
- `scrollBuffer` (terminal_session.go:298) — unchanged; in-memory ring stays for fast live-attach.
- WebSocket `join_terminal` / `terminal_exit` / `terminal_output` event flow (`ws.go:479`, `ws.go:45`) — scheduler becomes another consumer alongside Eve.
- `gorilla/websocket` — already a dependency via `relayScheduler/hub.go`.
- `Task` / `Execution` / `logStore` (`task.go`, `logstore.go`) — additive fields only, no migration.
- `bearerAuth` middleware (`auth.go:23`) — wraps the new `/log` endpoint with no extra work.
- `EndSession`/`CloseTerminal` cleanup pattern — extend the existing "end previous session" branch.

---

## Implementation order

1. **relayLLM disk persistence + `/api/terminals/{id}/log`** (additive; no callers depend on it yet). Existing interactive terminals start logging — no visible behavior change.
2. **relayLLM `extraArgs` plumbing** through `POST /api/terminals` and `ws.go:handleCreateTerminal`.
3. **relayScheduler client additions** (`CreateTerminal`, `AttachTerminalAndWait`, `GetTerminalLog`, `CloseTerminal`). Unit-tested against a stub HTTP+WS server.
4. **relayScheduler `Task`/`Execution` schema bump** (additive, no migration).
5. **relayScheduler `executeTask` branch + previous-session cleanup generalization** + task-create validation by `SessionType`.
6. **Eve task dialog + read-only PTY viewer + view-last-run branching.**
7. **Polish**: log sweeper, signal capture, sidebar branch on `sessionType`.

A short-lived env-var flag (`RELAY_PTY_TASKS_ENABLED`) gates the new POST shape on the scheduler for the first release, removed after a week of stability.

---

## Verification

**Regression (must stay green)**
- Existing chat task: create, run, "View Last Run" still joins the chat session and renders the transcript.
- Existing interactive terminals (claude-code, shell): still attach over WS and render scrollback.

**relayLLM**
- Smoketest: `POST /api/terminals templateId=shell extraArgs=["-c","echo hi; sleep 1; exit 3"]` → WS `join_terminal` → assert `terminal_exit{exitCode:3}` → `GET /api/terminals/{id}/log` returns bytes containing `hi`.
- 5 MB output test (`yes | head -c 5M`): final log files total ≤ 1 MB; replay decodes without error; head preserves the SGR prologue from the start.
- Log sweeper: pre-create a 31-day-old file; assert it's deleted on the next sweep.

**relayScheduler**
- Create PTY task `sessionType=pty, templateId=shell, extraArgs=["-c","exit 0"]` with on-demand schedule → `POST /api/tasks/{id}/run` → poll history → assert `ExitCode==0`, `TerminalID` non-empty, `Status==success`.
- Create PTY task with `MaxDurationSeconds=2` running `sleep 60` → assert `Status==timeout`, `ExitCode==-2`, terminal cleaned up server-side.
- Crash-recovery: kill relayLLM mid-run → scheduler marks Execution `error`, `Response` populated from disk log on next relayLLM start, not hung.
- Concurrency: schedule two PTY tasks at the same minute, same template → both produce independent log files and exit codes.

**Eve**
- Create PTY task via dialog → run → click "View Last Run" → xterm renders raw output with colors intact (verify with `ls --color` template).
- Live-attach: while PTY is still running, "View Last Run" opens read-only viewer streaming live output via `join_terminal`.
- Empty output (`true`): viewer shows blank terminal, no JS errors.
