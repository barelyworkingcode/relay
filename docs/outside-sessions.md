# Outside Claude sessions

Relay observes Claude sessions started outside Relay on the console and in
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
probeIntervalMs:15000,sessions,machines}`.

Rows contain `id`, `name`, nullable `projectId` and `hostId`, `machine`,
`directory`, `branch`, `state`, `since`, `updatedAt`, `source`, `outside:true`,
`readOnly:true`, nullable `tmuxSession`, `viewable` and `viewReason`.
States are `running`, `asking` and `idle`; sources are `probe` and `hook`.
Machine entries contain nullable `hostId`, `machine`, `status` (`reachable`
or `unreachable`), `lastProbeAt` and a safe `error` string. Raw PIDs, process
arguments, environments and full transcripts never appear in responses.

Detail returns `{session,lastTurn:{text,at,complete},lastExcerpt}`. It checks
the live identity again and reads the server-derived transcript at request
time. Text joins visible assistant blocks after the latest genuine user
prompt, excluding tool-result boundaries, and keeps the last 8192 characters.
The excerpt is the last 500 characters of that same text. Partial output is
available; missing latest text is empty, never a cached previous answer.
Unknown or exited identities answer 404 `outside_session_not_found`; failed
live observation or transcript reads answer 502 `outside_status_unavailable`.

## Discovery and lifetime

The baseline interval is 15 seconds. Machines run independently. Native Unix
process identity/cwd discovery uses ps plus procfs or lsof; Windows uses
PowerShell and same-user read access to native process parameters. Neither
needs a host-side installation or Claude hooks. Reads are bounded: 512 registry
records of at most 64 KiB, 128 transcript prefixes and tails of 8 KiB each, and a
1 MiB transcript tail for detail. Remote scripts use the existing SSH path.

A registry must correlate its PID, creation lifetime, session identity and cwd
with a live Claude process. Older clients require an unambiguous live cwd and
matching transcript metadata from that lifetime. Ambiguous evidence is omitted.
Hosted rows must lie in a registered project subtree. Managed sessions are
excluded using the selected `RELAY_SESSION_ID`, process ancestry and known
provider Claude identities; Relay IDs are never treated as Claude UUIDs.

A successful probe replaces membership. A confirmed exit disappears on the
next baseline. Failed probes mark the machine unreachable, clear active state
and viewing, and evict rows by one minute since their last live observation.
Reports and detail reads cannot extend an exited identity's lifetime.

Probe state is an estimate from registry status and bounded transcript tails.
A paused tool request estimates asking; hooks identify permission prompts exactly.
Transcript and report text is untrusted display data and cannot confer action
or read authority. Transcript paths are derived from home/cwd/session identity;
a reported `transcript_path` never chooses a file to read.

## Optional hooks

On each originating machine, configure Claude Code command hooks explicitly.
Relay never edits existing hook settings. Run `scripts/outside-session-report.sh`
on Unix, or `powershell.exe -NoProfile -NonInteractive -File
scripts/outside-session-report.ps1` on Windows, with native Claude hook JSON on
stdin. Install the helper at an operator-chosen path and use that path in each
hook command. Both helpers write at most 64 KiB atomically into the owner-only
`~/.claude/relay-outside-reports/` spool and call no Relay action API.

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
at a two-second cadence independently of baseline probes. Each state update
requires a fresh live process/session/cwd match; forged, stale or mismatched
reports do not seed identities. Healthy report completion to snapshot delivery
must fit five seconds. Hookless changes use the next completed 15-second probe.

## Viewing

A terminal-owned session cannot be attached. For tmux, only actual default
inventory correlated with live pane/process ancestry can offer a target.
Registry or hook socket metadata cannot authorize custom sockets. Row viewing
is advisory; the read-only viewer door independently repeats exact inventory,
capability and host opt-in checks. Host discovery remains visible with
`show_all_tmux_sessions` disabled. See [SSH hosts](ssh-hosts.md).
