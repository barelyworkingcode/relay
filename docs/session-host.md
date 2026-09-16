# Session host (`relay-sessions`)

The canonical reference for contracts C5 and C6: the binary that hosts every
terminal, Claude Code, pi and chat session relay launches, the internal API
relay drives it through, and the shim every session actually runs under.
Code carries the present tense; the *why* is here.

This document describes what is real in this tree today, including what is
built but not yet wired together. Where a piece is missing, it says so
plainly rather than describing the design as though it were the current
behavior — see [What is not built yet](#what-is-not-built-yet).

## The binary and its three modes

`cmd/relaysessions` builds one binary, `relay-sessions`, with three modes
selected by `os.Args[1]`:

| mode | who runs it | what it does |
|---|---|---|
| `service` | relay, once, as a launched service (`Command: RelaySessionsHelperPath(relayBin)`, `internal/service/builtin_sessions.go`) | Says a plain `service`-kind Hello (unmodified `bridge.SendHello`), runs the one-time relayLLM data migration, builds a real `terminal.Manager`/`session.Manager`, serves C5's internal API and C6's hook socket, and registers its manifest. |
| `exec` | `relay-sessions service`, once per terminal or provider-hosted session it spawns | The shim (C6): becomes the session's root process, optionally proves a launch identity, execs the real target, forwards signals, and reports status. See [The shim](#the-shim-relay-sessions-exec). |
| `hook` | Claude Code, as its own configured `PreToolUse` hook, once per tool call | Dials the host's hook socket and asks `/permission` whether the call may proceed. |

Only `service` mode is a long-lived process; `exec` and `hook` both exit when
their one job is done.

## The built-in service record

`relay-sessions` is not a service an operator registers. `service.BuiltinRelaySessionsService` (`internal/service/builtin_sessions.go`)
synthesizes its record fresh on every start, from relay's own bundle path and
config dir — `Command`, `Args`, `DisplayName` and `Capabilities` are never
read from `settings.json`, even if a stored record exists (`sanitizeIfBuiltin`
strips a stored `Command`/`Args` on load as defense in depth). `Autostart` is
the one field a stored record contributes, and a fresh install defaults it to
`true`.

Its capabilities are `manifest` and `sessions` — the second is refused on
every other service record by `internal/config/models.go`'s
`validateCapabilities`, so no other service, first-party or user-registered,
can ever be handed `SessionExited` or the unfiltered model list that
capability grants (see [`docs/launch-identity.md`](launch-identity.md#identity-kinds-and-capabilities)).

A build carrying an embedded helper cdhash (`build.sh`'s Helpers signing
step) additionally pins the process that binds this launch's Hello to the
signed relay-sessions binary specifically, not merely to whatever process
guessed the launch secret: `service.SetHelperVerifier` installs a
`HelperVerifier` that `Launches.BindKind` calls, for a `service`-kind launch
named `relaysessions` only, immediately before the secret is accepted as
spent (`internal/service/launch_identity.go`, `internal/service/codesign_darwin.go`).
The check reads the *live* peer's own code signature via its kernel-attested
audit token (`checkGuest`) and compares its cdhash against the one `build.sh`
embedded at build time — proving code identity, not merely secret
possession. A build with no embedded cdhash (an unsigned dev build) skips the
check entirely rather than refusing every launch.

## The internal API: `/launch` and `/terminate`

`internal/sessions/hostapi.Server` serves two Unix sockets:

- **the internal socket** (`RelaySessionsInternalSocketPath(configDir)`,
  `<configDir>/relaysessions-internal.sock`) — dialed only by relay itself,
  carrying `POST /launch` and `POST /terminate`;
- **the hook socket** (`RelaySessionsHookSocketPath(configDir)`,
  `<configDir>/relaysessions-hook.sock`) — dialed by `relay-sessions hook`
  processes, carrying `POST /permission`.

Both routes are reserved at the protocol level, not just by convention:
`internal/bridge/manifest.go`'s `Manifest.Validate` refuses any manifest —
including relay-sessions' own — that declares a route equal to or nested
under `/launch` or `/terminate`. The risk this closes is relay-sessions
advertising either path in its *public* manifest, which would expose its
peer-verification-free internal API to any ordinary frontend caller through
the manifest dispatcher's unverified reverse proxy. A different service
declaring the identical route string only ever reaches its own, unrelated
socket, since dispatch is by manifest owner, not by path text alone.

### Mutual peer verification (C5 §3.3)

`checkInternalPeer` (`internal/sessions/hostapi/server.go`) is the "Host
side" half of C5's mutual check, and it requires **both** of the following —
neither alone is sufficient:

1. the accepted connection's peer pid (read from the kernel via
   `peertoken.FromConn`, never asserted) equals `relay_pid` from this host's
   own Hello `OK` response, captured once at startup;
2. the request's `Authorization` header matches, in constant time, the
   bearer this host generated for itself and handed to relay in its
   `RegisterManifest` call.

The bearer is generated in memory (`generateBearer`, 32 random bytes as 64
lowercase hex) and never touches a flag, an environment variable, or a log
line — any same-uid process, including a sandboxed session target, can read
another process's argv via `ps(1)`, so the bearer is handed to `hostapi.Config`
and to the `RegisterManifest` call directly, in memory, and nowhere else.

The relay side of the same check — `router.go`'s `RegisterManifest` handler —
requires the caller's launch identity to hold the `manifest` capability and
refuses a `serviceId` other than the identity's own name, so only the one
process that said Hello as `relaysessions` can ever register this host's
socket and bearer with relay in the first place.

### `POST /launch`

Body is `hostapi.LaunchRequest` (v1). `handleLaunch` is a thin dispatcher —
`kind: "pty"` routes to `internal/sessions/terminal.Manager`, `kind:
"claude"|"pi"|"chat"` routes to `internal/sessions/session.Manager` — each of
which owns its own shim-spawn (or direct-spawn) mechanics end to end,
including the identity Hello wait. On success: `201` with
`{session_id, root_pid, body}`. `root_pid` is the shim's pid for a `pty`
launch and `0` for a provider-hosted (claude/pi/chat) launch — no
`provider.Provider` implementation exposes a pid this handler could report
(see [Known gaps](#what-is-not-built-yet)).

Error codes C5 names explicitly, each mapped from a manager error:

| HTTP | code | meaning |
|---|---|---|
| 400 | `invalid_spec` | malformed or self-contradictory request (the default for an unnamed error too) |
| 409 | `session_exists` | this session id is already live |
| 502 | `identity_refused` | the shim's Hello did not bind |
| 500 | `spawn_failed` | the target process could not be started |

### `POST /terminate`

Body is `{session_id, reason}`. Resolves the id against `terminal.Manager`
then `session.Manager` (whichever owns it), marks it terminating (so its own
exit report reads `reason: "closed"` rather than `"exit"`), and closes it —
SIGTERM then SIGKILL on the shim's process group for a pty session, the
provider's own `Kill()` for a provider-hosted one. Always `204`, including
for an id neither manager recognizes: an unrecognized id is the same
no-op `/terminate` always was.

## The shim: `relay-sessions exec`

Every pty session, and every `chat`-kind provider session, runs under a
small process, `relay-sessions exec`, whose whole job is C6. (A `claude`- or
`pi`-kind session does not go through the shim at all today — see
[Known gaps](#what-is-not-built-yet), gap 1.)

```
relay-sessions exec --session-id <id> [--identity] [--pty] \
  [--sandbox-profile <abs path>] --status-fd 4 -- <target argv…>
```

Steps run in this exact order, and both the implementation and its own tests
depend on the order, not just the end result:

1. **Read the identity secret (`--identity` only).** Fd 3 is read to EOF, at
   most 65 bytes so an over-long pipe is detected rather than truncated, then
   **closed unconditionally** — the target must never inherit an open fd 3,
   whether or not the secret was valid. Only exactly 64 lowercase hex
   characters is accepted.
2. **Become a PTY session leader (`--pty` only).** `setsid` then
   `TIOCSCTTY` on fd 0.
3. **Hello (`--identity` only).** Dials `RELAY_BRIDGE_SOCKET`, sends a
   `project_session`-kind Hello naming the session id and the secret just
   read. A refusal — bad secret, launch already bound, the process's own
   ancestry watch registration failing — exits `78` (`ExitIdentityFailure`)
   without ever spawning the target.
4. **Report status.** `hello_ok` (with this process's own pid) or
   `no_identity` on fd 4, newline-delimited JSON, one line per step from here
   on — so the host never has to guess what happened from the exit code
   alone.
5. **Wrap in `sandbox-exec` (`--sandbox-profile` only).** The target argv
   becomes `/usr/bin/sandbox-exec -f <profile> -- <target argv…>`.
6. **Spawn the target.** `Setpgid: true` always; `Foreground: true` and
   `Ctty: 0` (fd 0, the PTY slave already ctty'd in step 2) when `--pty`. A
   spawn failure reports `spawn_failed` with the errno and exits `127`
   (`ENOENT`) or `126` (any other cause).
7. **Forward signals.** `SIGHUP`/`SIGINT`/`SIGTERM`/`SIGQUIT` received by the
   shim are sent to `-targetPID` — the whole process group Setpgid placed the
   target in, not just the target itself, so a shell's own children see the
   same signal a real terminal would deliver to its foreground group.
8. **Wait and report exit.** The target's own exit code on a normal exit, or
   `128 + signal` on a signalled one (matching a shell's `$?`). A final
   `exit` status event carries the raw wait status and signal.

**The shim's own pid — never the target's — is the session's root** (SH
§4.2). It is what relay's launch identity binds to at Hello, so
`internal/sessions/hostapi` records the shim's pid as `root_pid`, and C3's
ancestry walk for that session starts looking for members at the target (the
shim's child) and up from there. The shim never `exec`s into the target
specifically so its own pid survives unchanged across the target's entire
lifetime.

Identity fd 3 and status fd 4 are never in `ExtraFiles` for the target: fd 3
is fully closed by the time the target spawns (step 1), and fd 4 is
`CLOSE_ON_EXEC` (set at open), so `exec(2)` closes it in the child
automatically.

## Host data directory layout

Everything relay-sessions owns lives under one directory, resolved from the
already-resolved `RELAY_BRIDGE_SOCKET` path rather than re-derived from
`os.UserConfigDir()` — the latter would silently drop a `relay --config-dir`
override this process never otherwise sees, since it doesn't inherit relay's
in-memory override across the exec boundary.

```
<config dir>/
  relaysessions-internal.sock     # C5 internal API: /launch, /terminate
  relaysessions-hook.sock         # C6 hook socket: /permission
  sessions/
    terminal_logs/                # pty session logs
    sessions/                     # session.Store's persisted claude/pi/chat records
    profiles/<session id>.sb      # C7 SBPL sandbox profiles, one per sandboxed launch
```

The `sessions/profiles/` directory is written by **relay itself**
(`cmd/relay/session_sandbox.go`'s `writeSessionSandboxProfile`, at
`sessionProfilesDir()` — the same `<config dir>/sessions/profiles` path,
computed independently from `bridge.ConfigDir()`), not by relay-sessions;
the shim reads it by the absolute path relay hands it in the `LaunchSpec`.
A profile is removed at session teardown (`sandbox.Remove`, called from
`sessionAccount.end`).

`~/Library/Application Support/relayLLM` (relayLLM's own, separate data
directory) is a one-time, best-effort migration *source*: `runService` calls
`internal/sessions/migrate.Run` at every startup, copying relayLLM's old
session data into the layout above without ever deleting the source. A
migration failure is logged and does not stop the host from starting.

## The `SessionExited` bridge notification

When `hostapi.Server` learns a session it dispatched to has exited — from
`terminal.Manager`'s or `session.Manager`'s own exit hook — it calls the
callback `runService` installed:

```go
srv.SetExitHandler(func(id string, rootPID, exitCode int, reason string) {
    reportSessionExited(bridgeSock, id, rootPID, exitCode, reason)
})
```

`reportSessionExited` sends relay's `SessionExited` bridge request — a
fresh, tokenless `bridge.Client` per call, relying entirely on C3 membership
over the connection's own peer credentials (this process is itself a `service`-kind
identity, and `SessionExited` is gated on the `sessions` capability that
identity holds; `router.go`'s `requireServiceIdentity` is the check). This is
the **real, current sender**: `runService` builds and wires it directly, and
also — the fix landed this session — actually calls `RegisterManifest`
(`config.RelaySessionsManifestRoutes`, `["/api/terminals/", "/api/sessions/"]`)
once its own Hello confirms it was launched by relay, so relay's dispatch
table and the created-terminal/session route reservation both come up
correctly on every start rather than only after a manual re-register.

`reason` is `"exit"` for an ordinary exit, `"closed"` when this host's own
`/terminate` caused it, `"deleted"` for `session.Manager.DeleteSession`
(never reached by `/terminate`, which only ever `EndSession`s — a
graceful stop, not a data wipe). `"idle"` is a fourth reason C5 names but
nothing in this repo can reach yet — see [Known gaps](#what-is-not-built-yet).

On relay's side, `SessionExited` (`cmd/relay/audit_call.go`,
`cmd/relay/router_sessions.go`) tears down whatever relay itself minted for
that session — the launch identity (`sessionAccount.launch.End()`), any
model key (`ModelKeyTable.Revoke`), the sandbox profile file
(`sandbox.Remove`) — updates the session ledger (`StateDormant`, or removed
outright for `reason: "deleted"`), and writes a `session_end` audit event.
See [`docs/audit-log.md`](audit-log.md#session-host-events) for the exact
record shape.

## Resume (user action only) — SH-6

A project-bound provider session (claude/pi/chat; **not** an ad-hoc,
project-less one) never respawns itself when its provider process dies.
`internal/sessions/session.Manager.SendMessage` returns `ErrResumeRequired`
instead of silently restarting anything, and the session sits `dormant` in
the ledger until a caller explicitly resumes it: `POST /launch` with
`resume: true` and the existing session id, which `AuthorizeLaunch`
(`cmd/relay/session_launch.go`) only accepts when the ledger record exists,
is `StateDormant`, and names the same project the resume request names — a
caller cannot squat a live session id or resume one project's session under
another project's name this way.

Only an **ad-hoc** session (`ProjectID == ""`, pty-only per SH §3.1's own
rule) still auto-respawns; SH-6's restriction is specific to a project-bound
session, which carries real authority (a model key, a permission policy) an
automatic respawn should not be trusted to re-establish silently.

A resumed launch runs the whole `AuthorizeLaunch` gauntlet again, including
re-merging the *current* project permission policy — a policy edited since
the original launch governs the resumed session, not whatever was merged in
originally — and mints a fresh launch identity secret and (if the kind wants
one) a fresh model key, exactly as a brand-new launch does. `internal/sessions/api/ws_session.go`'s
`sendResumeRequired` is the frame a live WS viewer sees when it asks to send
a message to a session that needs this before it can continue.

## What is not built yet

These are real, current gaps this session's own reviews found and did not
close. Documenting them precisely — not smoothing them into "future work" —
is this document's job as much as describing what works.

1. **Claude/pi launches run with no sandbox and no launch identity, despite
   relay believing otherwise.** `provider.ClaudeConfig`/`provider.PiConfig`
   (`internal/sessions/provider`) carry no `Sandbox` or `Identity` fields at
   all — unlike `provider.ChatConfig`, which does — and both providers spawn
   via a bare `exec.Command`: no shim, no `sandbox-exec` wrapping, no
   identity presented anywhere. Meanwhile relay's own `AuthorizeLaunch`
   (`cmd/relay/session_launch.go`) writes a real SBPL profile file to disk
   and mints a real launch secret for **every** claude/pi launch (`wantsSandbox`
   returns `true` unconditionally for `claude`/`pi`/`chat`), and
   `internal/sessions/hostapi`'s `launchSession` answers `201` as if both
   were applied. `internal/sessions/hostapi/types.go`'s own package doc
   states this plainly in code; this is the same fact surfaced here for a
   reader who does not start from the Go source. A chat-kind session is
   unaffected — `ChatConfig` already has the fields and is wired through
   the shim like a terminal.
2. **The eve-facing session HTTP/WS surface exists but is not reachable.**
   `internal/sessions/api` (`HandleListSessions`, `HandleDeleteSession`, the
   WS session/terminal handlers, `/api/permission`, `/api/generated/`,
   `/api/models`) is real, tested code — but nothing in `cmd/relaysessions`
   mounts it onto a real HTTP server, and it is never declared in the
   manifest `RegisterManifest` sends (only `["/api/terminals/",
   "/api/sessions/"]`, the two prefixes relay itself reserves for this
   service). A request to any of these paths today either 404s inside
   relay-sessions' own internal mux (which only serves `/launch` and
   `/terminate`) or, for the two reserved prefixes, is handled by relay's
   own minimal per-session HTTP routes (`cmd/relay/session_routes.go`),
   not by this package. Wiring this surface up is a genuinely separate,
   unbuilt unit.
3. **`session_bound` is never emitted.** `internal/audit/audit.go` reserves
   the constant and the `AuditActorProjectSession`/`AuditAuthSession`
   vocabulary is real and wired for tool calls and model calls — but nothing
   in this repo constructs a `session_bound` *event* specifically (the event
   that would mark a project_session launch identity successfully binding at
   Hello, distinct from the launch identity itself binding, which is
   unaudited today). See [`docs/audit-log.md`](audit-log.md#session-host-events).
4. **`NotifyViewerChange`-driven idle close is unreachable.** `reason: "idle"`
   on `SessionExited` depends on `terminal.Manager.NotifyViewerChange`, which
   only the eve-facing WS handlers in gap 2 ever call. Until that surface is
   mounted, a session never closes itself for being unwatched.
5. **`handleTerminate` has no existence-or-liveness probe before signalling.**
   A `/terminate` naming a long-dead session id (past the point its own exit
   was already reported and consumed) can SIGTERM/SIGKILL whatever process
   or process group now holds that recycled pid, if it names an id whose
   bookkeeping was already cleared incorrectly. This was flagged by this
   session's own review of `internal/sessions/hostapi` and is not fixed
   here — flagged, not silently patched, per this unit's own instructions.

## Code map

| concern | file |
|---|---|
| binary entry, `service` mode | `cmd/relaysessions/main.go` |
| shim (`exec` mode) | `internal/sessions/shim/shim.go` |
| hook client (`hook` mode) | `internal/sessions/hook/` |
| internal API server, `/launch`/`/terminate`/`/permission` | `internal/sessions/hostapi/{server,dispatch,types}.go` |
| terminal (pty) sessions | `internal/sessions/terminal/` |
| provider-hosted (claude/pi/chat) sessions | `internal/sessions/session/`, `internal/sessions/provider/` |
| eve-facing HTTP/WS handlers (not yet mounted) | `internal/sessions/api/` |
| C3 process-ancestry membership | `internal/membership/` |
| C7 sandbox profile rendering | `internal/sessions/sandbox/`, `cmd/relay/session_sandbox.go` |
| relay-side launch authorization | `cmd/relay/session_launch.go` |
| relay-side HTTP routes, resume, accounting | `cmd/relay/session_routes.go` |
| built-in service record, helper path resolution | `internal/service/builtin_sessions.go` |
| cdhash pinning | `internal/service/codesign_darwin.go`, `internal/service/helper_verify.go` |
| the two manifest routes' reservation | `internal/bridge/manifest.go`, `internal/config/models.go` (`RelaySessionsManifestRoutes`) |
