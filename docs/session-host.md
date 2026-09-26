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
| `hook` | Claude Code, as its own configured `PreToolUse` hook, once per tool call | Dials the host's hook socket and asks `/permission` whether the call may proceed; denies the call itself, rather than staying silent, whenever it can't get a clear answer — see [Tool permissions](#tool-permissions-post-permission). |

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

`Args` carries `-relay-mcp-command <relayBin>` alongside the socket flags,
the relay binary path relay derives `Command` from: relay-sessions
learns the one binary that serves `relay mcp` this way, with no env fallback
and no path derivation of its own, and hands it on to Claude's `--mcp-config`
and a chat session's tool child as `RelayMCPCommand` (see [The
shim](#the-shim-relay-sessions-exec)).

Its capabilities are `manifest` and `sessions` — the second is refused on
every other service record by `internal/config/models.go`'s
`validateCapabilities`, so no other service, first-party or user-registered,
can ever be handed `SessionExited` or the unfiltered model list that
capability grants (see [`docs/launch-identity.md`](launch-identity.md#identity-kinds-and-capabilities)).

A build carrying an embedded helper cdhash (`build.sh`'s Helpers signing
step) additionally pins the process that binds this launch's Hello to the
signed relay-sessions binary specifically, not merely to whatever process
guessed the launch secret: `(*service.Launches).SetHelperVerifier` installs a
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
  carrying `POST /launch`, `POST /terminate`, and (mounted alongside them,
  on the same mux — see [What is not built yet](#what-is-not-built-yet) gap
  2) the eve-facing session/terminal/model HTTP+WS surface relay's
  front-door dispatcher reaches by forwarding on eve's behalf;
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

That is the "host side" — relay-sessions verifying the caller dialing *it*.
The other direction, relay verifying the process it dials really is
relay-sessions, is `dialVerifiedUnix` — defined once in
`cmd/relay/model_endpoint.go` (the same helper `upstreamTransport` uses to
verify relayLLM's router socket) and called directly, not reimplemented,
from `cmd/relay/sessionhost_client.go`: on every `/launch`
and `/terminate` call, `sessionHostClient.resolve` re-reads relay-sessions'
currently bound `service`-kind launch identity fresh, never cached, and
`dialVerifiedUnix` reads the *live* peer's own kernel-attested `(pid,
pidversion)` off the freshly dialed connection and refuses unless it
matches that identity's process exactly — so a same-uid process that merely
learned the internal socket path, or a relay-sessions that died and was
replaced between calls, cannot answer for `/launch` or `/terminate` either.
Both halves are mutual: relay-sessions checks the pid making the request,
and relay checks the pid answering it, on every call.

### relay's own reads: `sessionHostClient`

Besides `/launch` and `/terminate`, relay reads three routes of the eve-facing
surface itself, not on eve's behalf. Each goes through
`cmd/relay/sessionhost_client.go`:

| Method | Route | Used by |
|---|---|---|
| `LiveTerminalNames` | `GET /api/terminals` | `PersistentSessionOps`, to tell a running persist terminal from a dead one |
| `ListModels` | `GET /api/models` | `ModelCatalogOps`, the Settings model picker ([model-endpoint.md](model-endpoint.md#choosing-a-projects-models-in-settings)) |
| `DialWS` | `/ws` | `relay sandbox`, to attach to the session it launched ([sandbox-command.md](sandbox-command.md)) |

Every one of them takes the path `/launch` takes. `resolve` re-reads the bound
launch identity fresh, `dialVerifiedUnix` checks the answering peer, and the
request carries the bearer the host registered. A direct read is no weaker
than a forwarded one, and the host's `guarded` check sees no difference
between them. Non-200 answers and transport failures both collapse to
`errSessionHostUnavailable`. Response bodies are read through
`maxSessionHostResponseBytes`.

### `POST /launch`

Body is `hostapi.LaunchRequest` (v1). `handleLaunch` is a thin dispatcher —
`kind: "pty"` routes to `internal/sessions/terminal.Manager`, `kind:
"claude"|"pi"|"chat"` routes to `internal/sessions/session.Manager` — each of
which owns its own shim-spawn (or direct-spawn) mechanics end to end,
including the identity Hello wait. On success: `201` with
`{session_id, root_pid, body}`. `root_pid` is the shim's pid for a `pty`
launch and `0` for a provider-hosted (claude/pi/chat) launch. `ClaudeProvider`
reports a process root, but only to `/permission`'s ancestry walk; `/launch`
does not report it (see [Known gaps](#what-is-not-built-yet)).

Error codes C5 names explicitly, each mapped from a manager error:

| HTTP | code | meaning |
|---|---|---|
| 400 | `invalid_spec` | malformed or self-contradictory request, including a new `chat` launch with a blank model (the default for an unnamed error on a `pty` launch; a `claude`/`pi`/`chat` launch instead defaults an unnamed error to `500`/`spawn_failed` — `terminalLaunchStatus` and `sessionLaunchStatus`, `internal/sessions/hostapi/dispatch.go`, disagree on this) |
| 409 | `session_exists` | this session id is already live |
| 502 | `identity_refused` | the shim's Hello did not bind |
| 500 | `spawn_failed` | the target process could not be started |

### A chat session names its model

A new `chat` launch must name a model. Blank means empty after trimming
whitespace, and a missing field is blank. Both sides refuse it:

- relay: `AuthorizeLaunch` (`cmd/relay/session_launch.go`) refuses it with
  code `model_required`. The HTTP answer is `400` with body
  `{"error": "<message>"}`; the message names the session when the request
  gave one, trimmed and capped at 64 runes with `…` appended when cut. The
  check runs after the caller, project, directory and template checks and
  before the `allowed_models` check. Nothing is minted: no model key, sandbox
  profile, launch identity, host call or ledger record. One `session_launch`
  audit record with outcome `error` is written, carrying the same message.
- relay-sessions: `buildSessionSpec`
  (`internal/sessions/hostapi/dispatch.go`) refuses it, and `/launch` answers
  `400` `invalid_spec`. No provider starts and no session is created.

Neither side picks a default model for a chat session. A default would run a
model the user never chose, and it would skip the project's `allowed_models`
check, which only tests a named model.

Resume is exempt on both sides. It sends the session's stored model back
through the same path, and a stored model may be blank. `pty`, `claude` and
`pi` launches are unaffected.

### `POST /terminate`

Body is `{session_id, reason}`. Resolves the id against `terminal.Manager`
then `session.Manager` (whichever owns it), marks it terminating (so its own
exit report reads `reason: "closed"` rather than `"exit"`), and closes it —
for a pty session, SIGTERM then SIGKILL sent to the shim's own pid and,
separately, to `-targetPID` (the target's whole process group, not the
shim's) — the provider's own `Kill()` for a provider-hosted one. Always
`204`, including for an id neither manager recognizes: an unrecognized id
is the same no-op `/terminate` always was.

Relay is also a viewer of the `/ws` on this socket, for `relay sandbox`: it
dials it with the same peer verification and bearer as `/launch`, joins the
terminal it just launched, and relays its bytes to the CLI
([`docs/sandbox-command.md`](sandbox-command.md)). Relay ends such a session
itself with `/terminate` when the CLI disconnects, because a terminal that lost
its last viewer would otherwise idle until the template's timeout.

## Tool permissions (`POST /permission`)

`POST /permission` arrives on the hook socket, dialed by a `relay-sessions
hook` process running as Claude Code's own `PreToolUse` hook (see [The binary
and its three modes](#the-binary-and-its-three-modes)). `handlePermission`
(`internal/sessions/hostapi/server.go`) never trusts the request body for who
is calling: it resolves the connecting pid to a session id first, the same
way `checkInternalPeer` resolves the internal socket's callers, and only then
reads the body.

### Identity: whose call is this

`internal/membership.Resolve` walks the requester's own process ancestry up
to a known root. A pty session's root has always been the shim's own pid,
recorded as `root_pid` at `/launch` time (see [The
shim](#the-shim-relay-sessions-exec)). A provider-hosted session has no
`/launch`-time entry at all, so the walk's set of roots is extended with
**live provider roots, read from `session.Manager` on every `/permission`
request**. Only claude sessions have one; pi and chat sessions report no
root and never run the hook:

- A local claude session's root is the pid the provider actually spawned:
  `relay-sessions exec`'s own pid when the session runs shimmed (every
  production local launch that carries a sandbox profile or a launch
  identity), or `claude` itself when the launch has neither.
- The root's start time is read immediately after `cmd.Start()` returns, and
  the walk requires an exact match on pid *and* start time — a bare pid
  match would also match whatever unrelated process the kernel later
  recycles that pid to.
- An SSH session reports no root; `handleControlRequest`'s own path does not
  go through this walk at all.
- A provider that has already exited reports no root.

Every walk failure — no root, a start-time mismatch, a pid that isn't a
descendant — is a `403`, before the request body is even decoded. This is
deliberate: a `403` becomes a hook `deny` (see "The hook fails closed"
below), so a caller the walk can't place is refused the tool call, not
granted it. Treating an unresolvable session as unrestricted, or defaulting
to allow when the walk errors, would let any process that merely knows the
hook socket's path answer for a session it isn't.

Roots are read live rather than written once at `/launch` because a launch-time
entry goes stale the moment a provider restarts a process outside `/launch` —
`SetPermissionMode`'s Kill-then-Start cycle chief among them (see [gap
1](#what-is-not-built-yet)) — leaving a stale entry pointing at a pid the
kernel may since have reused. Reading `session.Manager` fresh on each request
means a restarted provider's new pid is what the walk actually sees, and a
torn-down session reports no root instead of a dangling one.

### Decision order

Once the ancestry walk resolves a session id, and the decoded body's own
`sessionId` matches it (a mismatch is also a `403`), `handlePermission` runs:

1. A malformed body or an empty `sessionId`: `400`.
2. `cfg.Permissions == nil`: `200` deny, `session host: no permission manager`.
3. No live session for the id (`sessions.LiveSession`): `200` deny, `session
   has no tool-permission flow`.
4. A snapshot of the session's `PermissionMode`, `Headless` (bypassPermissions),
   `Policy` and `Directory`, taken under the session lock, goes through
   `permission.Preflight`. First match wins:

   | # | Condition | Decision |
   |---|---|---|
   | 1 | `Policy.DeniedTools` matches | deny — denied by project policy |
   | 2 | mode `bypassPermissions` | allow — bypassPermissions mode |
   | 3 | `Policy.AllowedTools` lists the tool by bare name | allow — allowed by project policy |
   | 4 | mode `acceptEdits`; tool is `Edit`, `MultiEdit`, `Write` or `NotebookEdit`; its `file_path`/`notebook_path` is an absolute path lexically under the cleaned session `Directory` | allow — acceptEdits mode: edit inside the session directory |
   | 5 | anything else (`default`, `plan`, an unknown mode) | ask a person |

   Rule 3 auto-approves only bare tool names in `allowed_tools`. A
   `Tool:arg` allow rule still prompts: the argument is matched by substring
   on the serialized input, which can't bound a chained command
   (`Bash:"command":"git` would also match `git status; curl … | sh`). Deny
   rules (rule 1) do match arguments, where broader matching is the safe
   direction.

   Rule 4's check is lexical: an in-tree symlink pointing outside the
   directory is auto-approved, and the sandbox is the file boundary.

5. `Preflight` deciding without asking ends the request there: no
   `permission_request` frame, no wait.
6. `Preflight` says ask, but no viewer has joined the session (`HasViewers`):
   `200` deny, `no client is viewing this session to approve the tool call`.
   No request is created — nothing is queued to replay if a viewer joins
   later.
7. A viewer is present: `CreateRequest` mints a permission id and
   `NotifySession` sends every viewer of the session a `permission_request`
   frame (`sessionId`, `permissionId`, `toolName`, `toolInput`, `toolUseId`).
8. `WaitForDecisionContext` returns the person's decision as given (an
   empty-reason deny becomes `Denied by user`), denies with `No response`
   after 60 seconds with no decision, and — on a hook-side disconnect —
   cleans up the pending request and writes nothing back.

Every path from step 2 on logs `slog.Info("permission decided", …)` with the
session, tool and decision — never the tool's input.

### Timeout ordering

Three timeouts nest, each looser than the one it wraps: the host's 60-second
wait for a person to answer, inside the hook client's 90-second HTTP timeout
for the whole round trip, inside Claude Code's own 120-second timeout on the
hook process. The ordering exists so a slow decision degrades in the host's
favor before it ever reaches Claude Code's timeout — a Claude Code hook
timeout counts as "no decision" exactly like exit 0 with no output (see the
outcome table below), which falls through to Claude Code's own, weaker rules.
60 < 90 < 120 means the host always answers before its own client gives up,
and the client always answers before Claude Code would, so a Claude Code hook
timeout is a configuration bug, never an expected outcome.

### The hook fails closed (`hook.Run`, `internal/sessions/hook/`)

In the ordinary case the hook writes exactly one line to stdout and exits 0:
a `hookSpecificOutput` naming `permissionDecision: "allow"` or `"deny"`. Two
cases skip the round trip entirely — exit 0, no output, and no `/permission`
call — because they aren't tool-call decisions at all:

- `RELAY_SESSIONS_HOOK_SOCKET` is unset: an operator's own `claude` running
  in a project directory relay didn't launch.
- The tool is `ExitPlanMode`, `AskUserQuestion` or `ToolSearch`: Claude
  Code's own built-ins, never routed through a relay decision.

Every other failure denies, with a reason prefixed `relay-sessions hook:`
naming what failed, rather than staying silent: an empty
`RELAY_SESSION_ID`, unreadable stdin, a dial failure, a transport error, the
90-second client timeout firing, a non-`200` response from the host, an
undecodable body, or a decision value that's neither `allow` nor `deny`.
This is deliberate: exit 0 with no output is reserved for the two skip cases
above. A hook that can't reach the host, or gets an answer it doesn't
understand, denies explicitly, so a host outage refuses tool calls instead of
quietly deferring to Claude Code's own rules.

pty terminals, SSH sessions, pi sessions and chat sessions never run this
hook; only a local Claude Code session configures it.

### Claude Code's own hook-outcome table

Measured on Claude Code 2.1.281 with relay's flags (`--print`, stream-json,
`--mcp-config`):

| Hook result | Claude Code |
|---|---|
| `permissionDecision: "allow"` | runs the tool |
| `"deny"` | refuses; Claude sees the reason |
| `"ask"` | refuses, under `--print` |
| exit 2 | refuses; stderr is the reason |
| exit 0 with no output, exit 1, or a hook timeout | no decision — Claude Code's own permission check runs. Under `--print` with no matching allow rule, that check refuses MCP tools, `Bash` and edits, and allows only read-only built-ins |
| `"deny"` under `bypassPermissions` mode | refuses — a hook deny beats bypass, so the host itself answers `allow` explicitly whenever the session's mode is `bypassPermissions` (Preflight rule 2), rather than relying on Claude Code to apply bypass on the hook's behalf |

### Why a hook, not `--permission-prompt-tool stdio`

`stdio` hands the decision to Claude Code's own permission engine: its
`allowed_tools`/`disallowedTools` rules would approve a call before relay's
host process ever sees it. A `PreToolUse` hook means every tool call — MCP or
built-in — reaches `/permission`, and the session's own policy and viewers,
first. Claude Code's own rules run only as the fallback for the two
deliberate skips above; a hook failure denies rather than falling through. A
session that wants Claude Code's own allow rules to also apply sets them via
`allowed_tools` in its policy by bare tool name, or eve's "Allow all" — read
by `Preflight` rule 3 — rather than widening what the hook itself lets
through.

The SSH control path (`handleControlRequest`) is unrelated to any of this and
unchanged.

## The shim: `relay-sessions exec`

Every pty session, and every `claude`/`pi` session whose launch carries a
sandbox profile or a launch identity, runs under a small process,
`relay-sessions exec`, whose whole job is C6. A pty session always runs
`--pty`; a `claude`/`pi` session never does — pipe mode (direct fd
passthrough, no pty, no copier goroutine) is the shape
`internal/sessions/provider/shimspawn.go` builds, and it needs no
`--pty` for the shim's identity, sandboxing, or signal-forwarding behavior
to apply. A `claude`/`pi` launch with neither a sandbox profile nor a launch
identity to present skips the shim entirely and spawns directly, same as
before (see [Known gaps](#what-is-not-built-yet), gap 1, for what this
still doesn't cover: `root_pid`, and `SetPermissionMode`'s new
resume-required behavior on a shim-wrapped claude session). A `chat`-kind
session's own provider process doesn't go through the shim either:
`ChatProvider` is an in-process HTTP client talking
to relay's model broker, with no external CLI child at all
(`internal/sessions/provider/chat_base.go`'s `ChatConfig` doc comment states
this plainly). `buildChatMCPManager` (`chat_base.go`) spawns a chat
session's relay-MCP tool child through the shim when the session opts into
`useRelayTools`:
`cmd/relaysessions/main.go` resolves `RelayMCPCommand` once at start from the
`-relay-mcp-command` flag the built-in service record carries (see [The
built-in service record](#the-built-in-service-record)) and sets it on both
`ChatConfig` and `ClaudeConfig`. A host session still gets no tool child:
relay mints no sandbox profile or launch identity for a host project, so a
local tool child would run unconfined on this Mac, acting on this machine's
resources instead of the host's. A session that asked for
`useRelayTools` and got no tool server logs `relay tools requested but
unavailable` at Warn, with a `reason` of `relay_mcp_command_unset`,
`host_session`, `mcp_start_failed` or `relay_server_failed`.

The tool child, chat or Claude alike, reaches the relay that launched this
host through `RELAY_BRIDGE_SOCKET`, which it inherits from relay-sessions'
environment. relay-sessions passes no `--config-dir`: it has no config dir,
only the socket path. `relay mcp` honours the variable and dials that socket
once before serving, so a relay under a non-default config dir gets its own
sessions' tool calls, and an unreachable socket ends the child at startup,
which surfaces as `relay_server_failed` or `mcp_start_failed`. The precedence
and the error lines are in [cli.md](cli.md#which-socket-the-relay-mcp-stdio-server-dials).

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

## Provider stderr

`logProviderStderr` (`internal/sessions/provider/stderr.go`) is the one
function both claude and pi read their child's stderr through — a shared
function because the redaction rule below must not drift between two copies.
Host (SSH) claude sessions get the same treatment.

A spawn's stderr lines log at `Warn` until the first non-empty line of stdout,
so a launch failure names itself instead of leaving only an exit code behind;
every line after that first stdout line logs at `Debug`. Warn logging is
capped at 20 lines per spawn, and hitting the cap logs one further line
naming the limit — everything past it still runs, at `Debug`. An empty line
is skipped, and a trailing `\r` is dropped.

Each line is redacted before it is logged, then truncated to 1024 bytes on a
rune boundary; reading continues past an overlong line rather than stopping
there, since a `bufio.Scanner`'s default 64 KiB limit would otherwise block
the child. Redaction runs, in order: the exact secrets the caller passed in
(the identity secret for claude; the model key and the identity secret for
pi), then `rmk_[0-9a-f]{64}`, then `sk-[A-Za-z0-9_-]{20,}`, then
`(?i)(bearer\s+)<token>`.

The child's stderr is wired through a plain `os.Pipe()`, not
`cmd.StderrPipe()`: `Wait` closes a `StderrPipe`'s read end itself, racing
whatever is still reading from it, and that race is how the one line that
would have named a launch failure gets lost. The write end becomes
`cmd.Stderr`, and the parent closes it once `Start` returns. Stdout is wired
the same way (`newStdoutPipe`) for the same reason: the lines lost to that
race would be the child's last events, such as a final `result`.

When the child exits, `waitForExit` drains before it reports: `Wait` returns,
both read ends get a read deadline of `providerDrainTimeout` (2s, read once at
spawn), and it waits for the stdout and stderr readers to finish. Only then is
`process_exited` emitted, so every line the child wrote is handled first. The
deadline exists because a grandchild that inherited a write end can hold it
open indefinitely; past the deadline, whatever it writes is dropped. The
deadline bounds reads, not handler time: a handler slower than the deadline
loses whatever backlog is still unread when it passes.

Only the provider's current spawn emits `process_exited`. After `Kill()` then
`Start()` on the same provider, the old spawn's drain can finish once the new
spawn is live; that exit is logged at `Debug` (`provider exit from a
superseded spawn dropped`) and dropped, with no crash `Warn`.

`logProviderStderr` also keeps the last 10 lines it logged, redacted and
truncated as above, and hands them back when it finishes. On a non-zero exit,
`waitForExit` logs one `Warn`, `provider exited with error`, with `session`,
`kind`, `exitCode` and `stderr_tail` (oldest first), before `process_exited`.
This is what names a mid-session crash, whose stderr lines otherwise log only
at `Debug`. A zero exit adds nothing, and neither does a stop relay itself
initiates: `Kill()` marks the spawn killed before it signals, and that exit
logs no crash `Warn` whatever its code, though `process_exited` still follows.
The early-failure paths (identity refused, spawn failed) drain stderr
themselves and close stdout unread.

## What a child inherits

A terminal, a claude or pi session and an MCP server child each start from
`relay-sessions`' own environment, minus two sets:

- **Relay's credentials**: `RELAY_SERVICE_TOKEN`, `RELAY_PROJECT_TOKEN` and the
  rest of `relaySecretEnvKeys`.
- **The variables of any Claude Code session that launched relay**:
  `CLAUDECODE`, `CLAUDE_PID`, `CLAUDE_EFFORT` and everything prefixed
  `CLAUDE_CODE_` (`childenv.IsParentClaudeSession`). Relay is often started from
  a terminal, and a build script run inside a Claude Code session hands it that
  session's environment: its id, its bridge and messaging token, and the
  `CLAUDE_CODE_CHILD_SESSION` marker. Without the scrub, a `claude` in a terminal
  relay starts believes it is a child of that session (it turns transcript
  saving off) and every terminal can read the parent's messaging token.

`CLAUDE_CONFIG_DIR` and the `ANTHROPIC_*` names are not dropped: they configure a
Claude Code the operator means to run. A template's `env` and `env_passthrough`
are applied after this base, so a template that wants one of the dropped names,
`CLAUDE_CODE_OAUTH_TOKEN` for example, names it. Services relay starts (relayLLM,
eve) still inherit relay's environment apart from relay's own tokens; that is not
covered here.

## What a sandboxed session can reach

File access is denied by default, in both directions. `sandboxSpecForLaunch`
(`cmd/relay/session_sandbox.go`) names what a session is granted and
`internal/sessions/sandbox` renders it as one Seatbelt profile: a bare
`(deny file-read* file-write*)`, then the system baseline reads, then the
baseline carve-out (`/usr/local/etc` and `/usr/local/var`, read and write),
then the three Homebrew files the baseline reopens read-only, then the
session's grants, then read-only `stat` on the parents of each grant
so a process can reach it, then the template's `deny` list (below), then an
unlink-and-clone deny on every ancestor of a denied path and on every
unix-socket deny dir and its ancestors, then a deny on unlinking or renaming a
socket inside a socket-deny dir. Apart from that fixed carve-out, nothing is
denied by name unless a template says so.
Relay's own data directory, another project, eve's data and `~/.ssh` are
unreachable unless a template grants them, so a directory nobody thought to
protect is protected anyway. Everything that is not a file (network, process,
mach) stays `(allow default)`; the unix-socket, loopback and setuid rules are
separate and unchanged. `relay mcp`, spawned as a chat session's tool child or
as Claude's own `--mcp-config` child, needs no entry in `read` for the relay
binary itself: `(deny file-read*)` blocks opening a file's contents, not
executing a binary already named by absolute path, and `relay mcp` reaches
relay only by dialing the bridge socket, already allowed by the unix-socket
rule.

The unix-socket rule denies connecting to any socket beneath relay's config
dir, relayLLM's data dir and eve's data dir (when set), then reopens only
relay's own `relay.sock`, `model.sock` and `relaysessions-hook.sock`. Under
`relay --config-dir`, the default config dir
(`~/Library/Application Support/relay`) is denied too, because another relay
instance keeps its sockets there.

A session's folders come from four places, and only the third is configured:

| Grant | Paths | Where it lives |
|---|---|---|
| **System baseline** (read-only, every sandboxed session) | `/usr`, `/System/Library`, `/private/etc`, `/private/var/db/timezone`, `/private/var/select`, and the root directory and the `/var`, `/etc`, `/tmp` links themselves; `/usr/local/etc` and `/usr/local/var` are denied (read and write), except the files `/usr/local/etc/openssl@3/cert.pem`, `/usr/local/etc/ca-certificates/cert.pem` and `/usr/local/etc/gitconfig`, which are read-only | `sandbox.baselineReadDirs`. The smallest set a shell, `git`, `curl`, `ssh`, `python`, `go` and `node` needed, measured under a deny-all profile on macOS 26. It holds no user data. `sandbox.baselineDenyDirs` carves out Intel Homebrew's service config and data, which can hold credentials. `sandbox.baselineReopenedFiles` reads back its CA bundle (the `openssl@3` link and the `ca-certificates` file it points at) and system gitconfig, without which Homebrew curl, python and git lose TLS verification and their git config. They are fixed literals, never resolved on the host. Nothing else beneath the carve-out is reopened. The deny renders before the session's own grants, so any later grant that covers part of either subtree reopens that part: a grant beneath it (`/usr/local/etc/example.conf`, say) and an ancestor grant such as `/usr/local` alike. `/usr` and `/usr/local`, the carve-out's ancestors, cannot be renamed, removed or cloned (the ancestor rule below), so no session can clone `/usr` or `/usr/local` to read `etc` under a new name; a clone needs only read on its source. |
| **Every session** | the project directory (read-write), `os.TempDir()`, `DARWIN_USER_TEMP_DIR` and `/dev` (read-write), the developer tools (read-only: `<Xcode>.app/Contents`, or `/Library/Developer/CommandLineTools`, resolved from `/var/select/developer_dir`) | `sandboxSpecForLaunch`. A pi session also gets `<config dir>/sessions/pi-sessions` for its transcript, since that path moves with `relay --config-dir` and no template can name it. |
| **The template** | its `read` and `read_write` lists | the template's entry in `settings.json` |

A fourth list, `deny`, is not a grant. It carves a path out of a grant that
covers it: `{"read_write": ["~"], "deny": ["~/.ssh"]}` gives the session its
home directory except `~/.ssh`. A denied path is rendered last, after every
grant and after the ancestor `stat` rules, so nothing reopens it: not the
template's grants, not the project directory, not the always-granted
temp/`/dev`/developer-tools paths. It blocks read, write and `stat` alike.
Denying a path that a session needs to run (the project directory, say)
locks the session out of it; relay does not second-guess that. `deny` takes
the same entry shape as `read` and `read_write`, and is ignored only for a
terminal launch of a template that says `"sandbox": false`. A claude, pi or
chat session on a console project always applies its kind template's folders,
`deny` included.

**Every ancestor of a denied path is pinned in place.** A Seatbelt path rule
governs the path, not the inode behind it: a session holding read-write on
`~` that renames `~/.config` to `~/c` reaches `~/c/gh`, which no rule names.
And `file-clone` is not part of `file-write*`, so a directory can be cloned
even where writing into it is denied. So `Render` adds one
`(deny file-write-unlink file-clone …)` block naming, as literals, every
directory from each denied path's parent up to but not including `/`, for the
template's `deny` entries and the baseline carve-out alike. It renders after
every grant, so no grant reopens it, and before the unix-socket rules. The
spellings come from the deny's own walk (below), so an ancestor is named as the
kernel resolves it; one that cannot be spelled refuses the render rather than
being left out. The cost: a session cannot rename, remove, swap or clone any
ancestor of a denied path, its own grant root included. With `"read_write":
["~"]` and `"deny": ["~/.config/gh"]`, `~/.config` and `~` itself are fixed in
place; everything inside them still works (creating, deleting and renaming
other entries, `chmod`, `utimes`, listing, cloning a non-ancestor).

The same block names every unix-socket deny dir (relay's config dir, the
default relay dir under `--config-dir`, relayLLM's data dir, eve's data dir)
**itself** as well as every ancestor of it. The connect deny is a path regex,
so it has the same gap: a session that renames the dir, or any directory above
it, connects to a socket under the new name. The dir itself is named because,
unlike a `deny` entry, no file deny covers it, and a shell template's
read-write `~` would otherwise let it be renamed or removed. The literals are
deduplicated with the deny ancestors and sorted by path. Each socket-deny dir
is walked once, and that walk supplies its connect regex, its literal and, when
the entry's last component is a link, the link's own path as a second literal
with its ancestors. A socket-deny dir reached through a link is not refused,
unlike a `deny` entry: the connect rule matches the resolved path whatever link
led there. One that cannot be spelled refuses the render with
`unix_connect_deny: …`. The cost falls on any session whose grant covers a
socket-deny dir or a directory above it, even with no `deny` entry: it cannot
rename, remove, swap or clone that dir or any directory above it. For a shell
session's read-write `~` that is `~` itself, `~/Library`,
`~/Library/Application Support` and the socket-deny dirs. Eve's default data
dir is `<WorkingDir>/data`, so a project rooted at eve's checkout holds it
inside its read-write grant: every session kind there cannot rename or remove
`data`, the project root or anything above it, and `rm -rf data` or
`git clean -fdx` leaves the directory behind. Writing, renaming and removing
files inside these dirs, `chmod`, `utimes` and listing still work.

**A socket cannot leave its deny dir either.** Renaming a socket file out of a
socket-deny dir, or unlinking it, would leave it connectable at a path the
connect deny does not match. So each socket-deny dir `D` also renders

```
(deny file-write-unlink
  (require-all (subpath "<D>") (vnode-type SOCKET)))
```

in its own block after the unlink-and-clone block and before the unix-socket
connect rules, one term per dir, spelled from the same walk. Regular files
in `D` are untouched. Its limit is a socket in a subdirectory of `D`: the
subdirectory is not pinned, so renaming it carries the socket out. The layouts
relay denies are flat. Measured on a devbox, relay's dir holds five sockets
and relayLLM's two, all directly inside the dir; eve's holds none. So no
subdirectory needs a deny-dir entry of its own. A service that moves its
sockets into a subdirectory must add that subdirectory to the deny dirs.

What the ancestor rule does not cover:

- A hard link or clone of a denied file made before the deny was added, or
  outside the session's reach, is a separate name and is reachable wherever a
  grant covers it.
- An ancestor missing at launch. The deny and its ancestors are named as
  written from the first missing component on, and a session that can write
  the parent may create that component as a symlink; the kernel then resolves
  the denied name to the link's target, which the deny does not cover. Deny
  paths that exist at launch.
- A socket-deny dir missing at launch, the same way. Its missing components
  are named as written, and a session that can write the parent may create
  the dir as a symlink before the service does; the service's sockets then
  land at the link's target, which the connect deny does not match. The
  default relay dir under `--config-dir`, relayLLM's dir and eve's dir can be
  absent when a session launches.
- A socket in a subdirectory of a socket-deny dir (above): the subdirectory
  can be renamed, taking the socket with it.
- A single-file deny covers that file only, not a writer's temp sibling. If a
  process outside the session saves the file atomically (`.env.tmp`, then a
  rename) while the session runs, the session can read or hard-link the temp
  file before the rename. Deny the directory when its contents are secret.

**Templates live only in `settings.json`.** Nothing is computed in code, so
every template, including the ones relay seeds, can be edited or removed. The
Settings window's Templates tab edits them, and `POST /api/terminal/templates`, `PUT` and `DELETE
/api/terminal/templates/{id}` do the same over HTTP, both through
`TemplateOps`. The routes are `configure` class and **deliberately not
presence-gated**, unlike `ProjectOps` and `McpOps`: a caller holding a
`configure` credential (a frontend-capable service included) can widen a
template's folders or opt it into a model key with no prompt. That is the same
trust hosts and services already carry, chosen knowingly; gating template
saves means adding them to `presence.GatedOps`. Each template carries its own
folders:

```json
"terminal_templates": [
  {
    "id": "claude-code", "name": "Claude Code", "command": "claude", "sandbox": true,
    "read_write": ["~/.claude", "~/.claude.json", "~/.cache", "~/Library/Caches"],
    "read": ["~/Library/Keychains", "~/.local/bin", "~/.local/share/claude", "~/.zshrc"]
  },
  { "id": "shell", "name": "Shell", "sandbox": true, "read_write": ["~"] }
]
```

Each entry is an absolute path or starts with `~`, and never `/`. A directory
grants its subtree; an existing regular file grants that file, and a
read-write file also grants the atomic-write siblings a CLI leaves beside it
(`.lock`, `.tmp.*`, `.backup`; SP2 row 17). A read-write entry gets those
siblings only when its name looks like a file (its last name, with one leading
dot trimmed, contains a dot) **and** it is a regular file on disk; otherwise it
renders as `(subpath …)`, which on a regular file grants that file alone. The
name decides because a session can swap a read-write directory for a file
before the next launch, and the disk alone would then hand it the siblings.
Two residuals follow. A read-write directory whose own name contains a dot
(`~/foo.d`) and is swapped for a regular file still gains its siblings. And a
read-write file whose name has no dot after its first character
(`~/.gitconfig`) gets no siblings, so an atomic writer's `.lock` beside it is
refused: git writing `~/.gitconfig` through `~/.gitconfig.lock` fails under a
read-write grant of `~/.gitconfig`. Keep such a file in a directory of its
own and grant that directory instead: git's `~/.config/git/config` with
`~/.config/git` read-write. A read-write directory that does
not exist is created at launch, because a `(subpath)` rule cannot create its
own ancestors (`go build` with no `~/go` needs `~/go/pkg` to exist). An entry
that cannot be placed refuses the template when settings are read, and the
launch if it slips through, rather than being dropped: a dropped entry would
leave a tool silently unreachable. A template is sandboxed unless it says
otherwise: `sandbox` absent means sandboxed, and only an explicit
`"sandbox": false` opts out. `read`, `read_write` and `deny` are ignored only
for a terminal launch of a template that says `"sandbox": false`; a claude, pi
or chat session on a console project always sandboxes and always applies its
kind template's folders, whatever that template's `sandbox` says. A stored template without
the field is sandboxed from the upgrade that introduced this rule on, with the
folders it already lists; nothing rewrites it to `false`, so an operator who
wants it unconfined says so.

A **claude, pi or chat session** is not launched from a template, but it reads
its folders from the template named for its kind: `claude-code`, `pi` and
`chat` (`kindTemplateIDs`). A missing template is not a refusal; the session
gets only what every session gets, and relay logs which template to add.

When `terminal_templates` is empty, relay writes one default at start: the
shell, sandboxed, with `~` read-write. That grant includes `~/.ssh` and every
other credential directory under the home directory; narrow it by editing the
template. To keep the shell out of the credential directories, add
`"deny": ["~/.ssh"]`, or the relay settings directory, to the template.

**A project opts in to templates.** `allowed_templates` on the project record
is an array: empty is none, a lone `"*"` is every template, otherwise the ids
listed (`"*"` beside other entries is refused). It gates every launch: a
terminal template, and the `claude-code`, `pi` or `chat` template a claude, pi
or chat session reads, so a project needs `claude-code` listed to run Claude
sessions. An unlisted template is refused `template_not_allowed`; a launch
naming no project is refused `project_required` for every kind, so there are
no ad-hoc terminals. A project created without the field holds none. Projects
that predate it (settings version 1) were migrated once to `["*"]`, and their
old per-project `shell_templates` were dropped. A remote project must keep the
list empty. `GET /api/terminal/templates?project=<id>` returns that project's
permitted templates, and `[]` with no project; the Settings window lists all
of them over IPC. The list rides on the project save, so it is gated as any
project edit already is.

**A host project uses its host's templates instead.** Both the catalog route
and the launch path pick templates with `config.TemplatesForProject`: for a
project with a `host_id` that is the host's `terminal_templates`, and console
templates are never offered. A host project may launch every template of its
host; `allowed_templates` gates console templates only. A claude session on a
host project passes the kind gate iff the host has a `claude-code` template;
a chat session keeps the console `allowed_templates` gate; pi is refused on a
host. A host template never sandboxes: it cannot set `"sandbox": true` or
carry `read` or `read_write`, and one that omits `sandbox` launches
unconfined. An empty `command` there runs the host's login shell rather
than relay's `$SHELL`. Shape, seeding and launch argv are in
[`docs/ssh-hosts.md`](ssh-hosts.md#terminals-on-a-host).

**A persistent host terminal is an ordinary host PTY.** A host template with
`persist` differs only in its remote command, which is
`<tmux> new-session -A -s <name> …`; relay builds that argv before launch.
relay-sessions knows nothing new: it holds the `ssh -tt` child in memory like
any other terminal, and closing, idling out or restarting ends that child,
which detaches tmux on the host rather than killing it. Naming, enumeration,
reattach and kill are in
[`docs/ssh-hosts.md`](ssh-hosts.md#persistent-terminals).

**A template can point a client at relay's model endpoint.** `model_key: true`
mints a per-session key, and `${MODEL_KEY}` in an `env` value delivers it.
`${MODEL_ENDPOINT_URL}` in an `env` value becomes `http://<model_endpoint.listen>`.
With the listener off the launch is refused (`model_endpoint_unavailable`)
instead of leaving the placeholder or dropping only the URL, because a
`${MODEL_KEY}` header with no relay URL would be sent to the client's real
provider. Relay never turns the listener on for you.

Three rules the measurement turned up. **Exec does not need a read grant on the
binary**, so a system binary runs without one; **a symlink does**: a tool that
lives behind a link (`~/.local/bin/claude`, `~/.bun/bin/pi`) needs the
directory holding the link and the directory holding its target. And the
kernel matches the path *it* resolved, in the volume's own letter case, so
`sandbox.resolve` asks the kernel for the on-disk spelling of every grant: a
project path stored as `/users/me/Proj` would otherwise match nothing and lock
the session out of its own directory. A grant whose final component is a
symlink also names the link itself.

**A read-write grant never follows a symlink, except the system ones.** A
session holding read-write on a directory can replace it, or any directory
under it, with a symlink: `(subpath D)` covers D itself. If the next launch
followed that link, the session would have chosen its own grant. And a
writable root is keyed where it is named (below), so a root reached through
any other link would leave where it resolves uncovered; a root that is one of
the `/tmp`, `/var` and `/etc` links is keyed both where it is named and where
it resolves. So `Render` walks every
`read_write` entry, directory or file, the project path included, from `/`
with `Lstat` on each component, splicing in each link's target (a relative
target resolves against the link's already resolved directory, at most 32
links; more fails the render). If the walk follows any link other than the
`/tmp`, `/var` and `/etc` links in `/`, the final component included and
whoever owns it, the launch is refused with `sandbox.LinkedGrantError`
(`grant "<entry>" follows symlink "<link>"; use the real path`): `400
sandbox_unavailable`, a `session_launch` error in the audit log, one refusal
Warn (`session sandbox: read-write grant refused`; see `ensureGrantDirs` below
for the one other line a refused launch can log), and no profile written. A
link counts whether or not its target exists. `t.TempDir()`,
`/tmp/claude-<uid>` and anything else reached only through `/tmp`, `/var` or
`/etc` keep working; a root-owned link such as `/var/select/sh`, or any link in
`/private/tmp`, is refused. The cost is deliberate: a dotfiles-managed
`~/.claude`, or a project reached through a link, is refused as a read-write
grant; the operator names the real path instead. Only the launch's own grants
refuse: another template's or project's symlinked root refuses at its own
launch, not at an unrelated one.

A **locked** directory, for the read and deny rules below, is one root owns
and the running user cannot write (`access(dir, W_OK)` fails). That covers the
`/tmp`, `/var` and `/etc` links in `/`, while a link directly in
`/private/tmp` (root's, but world-writable) is not in a locked directory.

**A read grant never follows a link a sandboxed session could have made.**
The strict rule above cannot apply to reads: Homebrew and installer link
chains run through directories the user can write, and `/Applications` is
admin-writable. So a read grant is refused only when the link could be a
session's. `Render` walks every `read` entry, file or directory, and the
system baseline dirs, the same way as a read-write grant, and one walk supplies
both the check and the rendered terms. For every link the walk follows whose
directory is not locked, it asks whether the **writable roots** (W) cover that
link. If they do, the launch is refused with `sandbox.LinkedReadError`
(`read grant "<entry>" follows symlink "<link>", which a sandboxed session
could have made`, prefixed `read: ` or `baseline: `): the same `400
sandbox_unavailable`, audited, with one `session sandbox: read grant refused`
Warn and no profile written. A dangling link is not followed, so it renders
naming only the link.

W is built per launch by `sandboxWritableRoots`
(`cmd/relay/session_sandbox_writable.go`) plus the launch's own grants:

- the home directory, always;
- `os.TempDir()`, `DARWIN_USER_TEMP_DIR`, `/dev` and
  `<config dir>/sessions/pi-sessions`, the read-write paths relay grants
  without a template;
- the `read_write` entries of every sandboxed console template, and of the
  `claude-code`, `pi` and `chat` templates whatever their `sandbox` flag, since
  a claude, pi or chat session always sandboxes;
- every local project path (not remote, not on a host);
- the launch's own `read_write` entries, which `Render` adds itself.

Coverage is decided by location, not by spelling and not by the inode found
at a root. Each root is keyed as the identity (device and inode) of its nearest
existing ancestor plus the names from there down, compared with Unicode case
folding and normalization. So letter case and firmlinks cannot hide a match,
nor can the `/tmp`, `/var` and `/etc` aliases: a root that is one of them is
also keyed where it resolves, and a grant is walked to its resolution. And a
session that removes and recreates its root, or swaps it for a link, between W
being built and a grant being walked is still caught: the ancestor is outside
its reach. The exception is an undotted read-write regular-file root: its
subpath grant lets a session replace it with a directory, and a file root does
not cover what lies beneath it, so a link planted there in that window is
missed. Folding can equate names the volume
keeps apart; that refuses more, never less. A directory root covers a link
anywhere beneath it. A regular-file root covers the entries directly in its
parent directory. A read-write file grant with a dotted name lets a session
create its atomic-write siblings beside it; an undotted one does not, and
covering its parent anyway over-covers deliberately, since refusing more is
the fail-closed direction. Every root also covers its own
entry, so a root replaced with a link is caught. A root that is missing is
keyed by its nearest existing ancestor, and a `..` in the part the walk could
not reach cancels the name before it or steps the ancestor up; a root whose
walk fails contributes nothing; a relative root fails the render. W takes
template entries from the same validated view a launch uses, so a template
dropped at resolution contributes nothing, and an entry no launch can grant
(`~/../..` resolving to `/`, say) is skipped with a `session sandbox: writable
root skipped` Warn naming the template and the entry. Unsandboxed templates and hosted or remote projects are
not in W.

The cost: because home is always in W, a read entry reached through a link
anywhere in the home directory is refused. A dotfiles-managed `~/.zshrc` in the
`claude-code` template's `read` list refuses every Claude launch until the
entry is removed or names the real file; the same holds for any template read
entry that is itself a link in home.

**The gap.** W is computed from the settings of this launch, not from every
setting that ever applied. A link planted, outside home, in a region current
settings no longer name is not caught: a template narrowed since, a project
deleted or moved, or a template that was sandboxed and now says
`"sandbox": false`. Links in home are caught, since home is always in W. What a session writes
that is not a link (rc files, scripts, provider binaries another session runs)
and unix-socket entries are outside this rule.

**A `deny` entry never follows a link the user could have made, either.** Each
`deny`, the baseline carve-outs included, is walked the same way, and that one
walk supplies both the deny's own terms and its ancestors. Following a link in
a directory that is not locked, intermediate, final or dangling, refuses the
launch with `sandbox.LinkedDenyError` (`deny "<entry>" follows symlink
"<link>", which a sandboxed session could have made`): the same `400
sandbox_unavailable`, audited, with one `session sandbox: deny refused` Warn
and no profile written. Otherwise the ancestors pinned would be the link's,
not the denied path's, and a session could have planted the link to pick
them. Links through `/tmp`, `/var` and `/etc` still work. The baseline case
fires only where the user can write `/usr/local` and a carve-out there is a
link.
A link whose target does not exist is not followed: the walk treats it as
the first missing component, so a read grant on it names only the link's own
path, never a target a session could later create. Any entry, read, deny,
read-write or unix-socket, that is relative, follows more than 32 links, or passes through a
link that cannot be read fails the render and refuses the launch.

Every term a read-write grant renders comes from that one walk: the
`subpath` or file regex, the link literal and the ancestor `stat` paths.
Resolving the path a second time would give a session a window to swap a link
in after the check. The case-correcting open (`onDiskPath`) uses
`O_NOFOLLOW_ANY`, so if a link appears between the walk and the open, the
open fails and the profile keeps the walk's own spelling.

Creating missing read-write directories (`ensureGrantDirs`) runs before
`Render`, and its `MkdirAll` follows links. Through a planted link it can
create an empty `0700` directory at the link's target before the render
refuses the launch. Nothing is granted on it; the directory is left as it is.
A read-write entry whose final component is a dangling link makes `MkdirAll`
fail instead, since the name exists but is not a directory, and it logs
`session sandbox: could not create a granted directory` before the refusal
Warn. So a refused launch can log two lines.

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
    pi-sessions/                  # pi's own JSONL transcripts (internal/sessions/provider/pi.go)
    profiles/<session id>.sb      # C7 SBPL sandbox profiles, one per sandboxed launch
    .migrated-from-relayllm       # migrate.Run's idempotency marker, written once
```

The `sessions/profiles/` directory is written by **relay itself**
(`cmd/relay/session_sandbox.go`'s `writeSessionSandboxProfile`, at
`sessionProfilesDir()` — the same `<config dir>/sessions/profiles` path,
computed independently from `bridge.ConfigDir()`), not by relay-sessions;
the shim reads it by the absolute path relay hands it in the `LaunchSpec`.
A profile is removed at session teardown (`sandbox.Remove`, called from
`sessionAccount.end`).

`sessions/pi-sessions/` sits inside relay's own config dir, which no sandbox
profile grants (see [What a sandboxed session can
reach](#what-a-sandboxed-session-can-reach)), so a sandboxed pi session would
be unable to write its own transcript on its first turn. `sandboxSpecForLaunch`
grants read-write on exactly this one leaf (`sessionPiSessionsDir()`) —
computed independently, the same way `sessionProfilesDir()` is, so it stays
correct under a `relay --config-dir` override without needing to import
`provider.PiConfig`. The tradeoff this accepts: a sandboxed pi session can
read (and write) another sandboxed pi session's own transcript, since the
grant names the whole `pi-sessions/` directory, not a single session's file
within it.

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
fresh, tokenless `bridge.Client` per call, authenticated by this process's
own bound `service`-kind launch identity rather than by C3 membership:
`SessionExited` is gated on the `sessions` capability that identity holds,
checked by `router_sessions.go`'s `SessionExited` handler via
`requireServiceIdentity` (`router.go`) — a path that deliberately never
consults `resolveAuth`'s C3-membership step at all, since every step there
resolves to a project and a service identity is not one
([`docs/tokens.md`](tokens.md#what-determines-a-callers-authority-now)).
This is the **real, current sender**: `runService` builds and wires it
directly, and also calls `RegisterManifest`
(`config.RelaySessionsManifestRoutes`, `["/api/terminals/", "/api/sessions/"]`)
once its own Hello confirms it was launched by relay, so relay's dispatch
table and the created-terminal/session route reservation both come up
correctly on every start rather than only after a manual re-register.

`reason` is `"exit"` for an ordinary exit, or `"closed"` when this host's own
`/terminate` marked the session terminating (`markTerminatingIfAlive`) before
signalling it — `onTerminalExit`/`onSessionExit` (`internal/sessions/hostapi/server.go`)
consume that flag once and report `"closed"` only if it was set, `"exit"`
otherwise. These are the **only** two reasons any code in this repo actually
produces. C5 also names `"idle"` and `"deleted"`, and relay's own consumer
(`cmd/relay/router_sessions.go`) is prepared to handle both, but neither is
reachable today: `session.Manager.DeleteSession` kills its target through
this same exit path without ever marking it terminating, so it too reports
`"exit"`, not `"deleted"` — and idle-close is covered by gap 4 below.
`internal/sessions/hostapi/server.go`'s own comment says this plainly.

On relay's side, `SessionExited` (`cmd/relay/router_sessions.go`) tears down
whatever relay itself minted for that session — the launch identity
(`sessionAccount.launch.End()`), any
model key (`ModelKeyTable.Revoke`), the sandbox profile file
(`sandbox.Remove`) — updates the session ledger (`StateDormant`, or removed
outright for `reason: "deleted"`), and writes a `session_end` audit event.
See [`docs/audit-log.md`](audit-log.md#session-host-events) for the exact
record shape.

## Resume (user action only) — SH-6

A project-bound provider session (claude/pi/chat) never respawns itself when its provider process dies.
`internal/sessions/session.Manager.SendMessage` returns `ErrResumeRequired`
instead of silently restarting anything, and the session sits `dormant` in
the ledger until a caller explicitly resumes it: `POST /launch` with
`resume: true` and the existing session id, which `AuthorizeLaunch`
(`cmd/relay/session_launch.go`) only accepts when the ledger record exists,
is `StateDormant`, and names the same project the resume request names — a
caller cannot squat a live session id or resume one project's session under
another project's name this way.

There is no project-less launch: `AuthorizeLaunch` refuses one for every kind
(`project_required`), so every session relay launches carries a project's
authority and none auto-respawns.

A project-less session can still reach the `Manager`: one created directly
through its API, or one lazy-loaded from disk (a migrated relayLLM session
file). `SendMessage` and `ClearSession` restart a project-less session only
when this process launched it, and only with the `CreateSpec` it was launched
with — identity, model key and sandbox profile included. If a launch for that
session is in progress, the restart waits for it to finish and then reads the
slot again. Anything else answers `resume_required`: a session this process
never launched, a launch that failed or was stopped, a slot removed or
replaced in the meantime. The slot is checked again once the new provider is
built; if the session was ended, stopped or taken by a new launch while it was
being built, that provider is killed and the restart is refused too. A spec
holding only the provider kind would start claude or pi with no sandbox, no
identity and no key, so there is no fallback to one. A migrated project-less session is therefore read-only history:
`clear_session` still clears it, but to continue the user starts a new session
in a project. The error text says so, and both doors pass it on beside the
code: the WS `resume_required` frame and the HTTP 409 body each carry it as
`message`. Clients branch on `code` or `error`, never on `message`.

A resumed launch runs the whole `AuthorizeLaunch` gauntlet again, including
re-merging the *current* project permission policy — a policy edited since
the original launch governs the resumed session, not whatever was merged in
originally — and mints a fresh launch identity secret and (if the kind wants
one) a fresh model key, exactly as a brand-new launch does. `internal/sessions/api/ws_session.go`'s
`sendResumeRequired` is the frame a live WS viewer sees when it asks to send
a message to a session that needs this before it can continue — reachable
now that `internal/sessions/api` is mounted (gap 2 below is fixed).

## What is not built yet

These are real, current gaps. Documenting them precisely — not smoothing
them into "future work" — is this document's job as much as describing
what works.

1. ~~Claude/pi launches run with no sandbox and no launch identity, despite
   relay believing otherwise.~~ **Fixed.** `provider.ClaudeConfig`/
   `provider.PiConfig` (`internal/sessions/provider`) now carry `ShimBinary`,
   `SandboxProfile` and `Identity` fields — the same shape `ChatConfig`
   already had — and `session.Manager.buildProvider` threads
   `spec.SandboxProfile`/`spec.Identity` into both, mirroring the `chat`
   branch it already did this for. Both providers' `Start` build a
   `relay-sessions exec` invocation (`internal/sessions/provider/
   shimspawn.go`'s `buildShimCmd`, mirroring `internal/sessions/mcp`'s
   `buildShimCommand` and `internal/sessions/terminal`'s own `buildShimCmd`)
   whenever either field is set, in pipe mode — never `--pty` — and refuse
   to spawn unconfined when a sandbox profile is requested but no shim
   binary is configured (`provider.ErrShimRequired`); a launch with neither
   set still spawns directly, unchanged. What is still open, on purpose:
   - `root_pid` stays `0` for every provider-hosted (claude/pi/chat) launch
     in `hostapi`'s `201` response. `ClaudeProvider` reports a process root
     (the shim's pid, or claude's own), but only `/permission`'s ancestry
     walk reads it; reporting it from `/launch` is a separate piece of work.
   - `ClaudeProvider.SetPermissionMode`'s existing Kill-then-Start restart
     is unsound once a launch's identity and sandbox profile are real: the
     identity secret is single-use, and `Kill` fires `process_exited`,
     which ends the launch identity and deletes the sandbox profile file
     relay wrote — so `Start` afterward would hand `--sandbox-profile` a
     path that no longer exists. A shim-wrapped claude session's
     `SetPermissionMode` now refuses with `ErrRestartNeedsResume` instead,
     surfaced over WS as the same `resume_required` frame
     `ErrResumeRequired` already produces (`internal/sessions/api/
     ws_session.go`) — the caller drives a real resume (`POST /launch
     resume:true`), which mints both fresh, rather than this package
     attempting to reuse either.
   - A sandboxed pi session can read (and, if it chose to, tamper with)
     another sandboxed pi session's own JSONL transcript: pi's transcripts
     live under relay's own directory, which no sandbox profile grants, so
     `sandboxSpecForLaunch` (`cmd/relay/session_sandbox.go`) grants
     read-write on exactly that one leaf (`<config dir>/sessions/pi-sessions`,
     `Spec.ReadWrite`, `internal/sessions/sandbox/sandbox.go`). A documented
     tradeoff, not fixed here.
   - The broker-vs-subprocess question this fix's own design explicitly
     carved out (whether relayLLM's model broker should sit behind the same
     boundary) is separate, later work.
   A chat-kind session's own provider process is unaffected by any of this
   in the same way it is exempt from the shim entirely (see [The
   shim](#the-shim-relay-sessions-exec)) — it has no external CLI child to
   sandbox or identify. `ChatConfig` carries the `Sandbox` and `Identity`
   fields its optional relay-MCP tool child needs to run through the shim
   like a terminal (`buildChatMCPManager`) — see [The
   shim](#the-shim-relay-sessions-exec).
2. ~~The eve-facing session HTTP/WS surface exists but is not reachable.~~
   **Fixed.** `internal/sessions/api` (`HandleListSessions`,
   `HandleDeleteSession`, `HandleSessionMessageSync`, `HandleListTerminals`,
   `HandleDeleteTerminal`, `HandleTerminalLog`, `HandleModels`, and the WS
   session/terminal handlers) is mounted on relay-sessions' own internal
   socket, alongside `/launch` and `/terminate`, by `hostapi.New`/
   `Server.ListenInternal` (`internal/sessions/hostapi/server.go`) — not by
   `cmd/relaysessions/main.go`, so every constructor of a `hostapi.Server`
   gets the mount, including `cmd/relay`'s own capstone integration test.
   `RelaySessionsManifestRoutes` (`internal/config/models.go`) now also
   names `/api/models` and `/ws`, so relay's front-door dispatcher resolves
   them through relay-sessions' manifest instead of 404ing before ever
   reaching the socket. Every mounted route is wrapped in the same
   `checkInternalPeer` mutual check `/launch`/`/terminate` already used
   (`Server.guarded`, `server.go`) — see the note on that wrapper's trust
   model just below. `terminal.Manager.SetOutputHandler` (new; mirrors the
   existing `SetExitHandler`) is what makes a joined WS viewer actually see
   live terminal output rather than a one-time scrollback dump — it did not
   exist before this fix, since nothing needed it while the surface it fed
   was unreachable. `terminal_templates` (the WS message eve's Shell
   Launcher used to send to populate its "New" tab) is retired the same way
   `terminal_create` already was: the template catalog lives in relay itself
   (`GET /api/terminal/templates`), so relay-sessions now answers an
   explicit refusal frame for it rather than dropping it silently.

   **The trust model this mount runs under, stated plainly** (the comment on
   `Server.guarded` in `internal/sessions/hostapi/server.go` is the
   authoritative copy; this is the same fact for a reader who does not start
   from the Go source): passing `checkInternalPeer` proves "this request
   came from relay" — either a direct `/launch`/`/terminate` call or one
   relay's front-door dispatcher forwarded on eve's behalf — never "this
   caller may see this specific session". Relay's frontend socket is a
   single trust domain: any frontend-capable caller (eve, or a control-plane
   credential holding `proxy`) reaches every session on the host through
   this mount, the same way it already reached every project's MCP tools
   through the manifest-proxy design generally. This is a pre-existing
   property of that whole design, now load-bearing for session content
   specifically, and is not fixed here — see the "not fixed here" framing
   this whole section already uses for gaps 1 and 5. The one piece of
   scoping this fix does add: `internal/sessions/api/ws_session.go`'s
   `handlePermissionResponse` refuses to resolve a pending Claude Code
   tool-approval prompt unless the resolving WS connection has itself
   `join_session`'d that prompt's session first — closing the sharpest edge
   in this trust model (an unscoped permission decision) without attempting
   the broader per-caller session-ownership model relay has no concept of
   anywhere today.
3. **`session_bound` is never emitted.** `internal/audit/audit.go` reserves
   the constant and the `AuditActorProjectSession`/`AuditAuthSession`
   vocabulary is real and wired for tool calls and model calls — but nothing
   in this repo constructs a `session_bound` *event* specifically (the event
   that would mark a project_session launch identity successfully binding at
   Hello, distinct from the launch identity itself binding, which is
   unaudited today). See [`docs/audit-log.md`](audit-log.md#session-host-events).
4. **Idle close would not report `reason: "idle"` even now that it is
   reachable.** `internal/sessions/api/ws_terminal.go`'s `join`/`leave`/
   `handleDisconnect` call `terminal.Manager.NotifyViewerChange` on every
   real join/leave/disconnect now that the WS surface is mounted (gap 2 is
   fixed), so `terminal.Manager`'s idle callback (`onIdle`,
   `internal/sessions/terminal/manager.go`) genuinely fires when a
   terminal's last viewer leaves. But `onIdle` still calls `m.Close(id)`
   unconditionally, never marking the session terminating first (the one
   thing that turns `"exit"` into `"closed"`), so an idle-close funnels
   through the same exit report a natural process death does and reports
   `reason: "exit"`, indistinguishable from any other exit. No code anywhere
   in this repo constructs the literal `"idle"`. This is not fixed here.
5. **`handleTerminate`'s existence-or-liveness probe gates the wrong thing.**
   `handleTerminate` (`internal/sessions/hostapi/server.go`) does call
   `Get`/check `Alive()`/`markTerminatingIfAlive` before signalling — but
   that check only decides whether the session's own exit report reads
   `reason: "closed"` instead of `"exit"`; it never decides whether a
   signal is sent. The real hazard is bookkeeping that outlives the process
   it describes: a terminal session stays in `terminal.Manager`'s table
   after a natural exit — only `Close` removes the table entry, never the
   exit itself (`internal/sessions/terminal/manager.go`'s own comment on
   `SetExitHandler`) — so a `/terminate` naming an id whose process already
   exited on its own can still reach `Session.Close`
   (`internal/sessions/terminal/session.go`), which unconditionally sends
   `SIGTERM` to `shimPID` and, separately, to `-targetPID` — against
   whatever process now holds that recycled pid. `Close`'s follow-up
   `SIGKILL` does **not** fire in this exact scenario: it is gated by
   `select { case <-s.waitDone: …; case <-clock.After(terminateGrace):
   SIGKILL }`, and `waitDone` is already closed for a process that already
   exited, so that branch wins immediately. The recycled-pid hazard here is
   a stray `SIGTERM`, not a `SIGKILL`. This is not fixed here.
6. ~~`/permission` is a hard-coded refusal, and a provider-hosted session
   has no membership entry to even reach it.~~ `handlePermission`
   (`internal/sessions/hostapi/server.go`) answers every admitted call
   `{"decision":"deny","reason":"session host: no policy engine wired
   yet"}` — a fixed placeholder, not a policy engine. Worse, `launchSession`
   (`internal/sessions/hostapi/server.go`) deliberately registers no
   membership table entry for a claude/pi/chat launch at all: a
   provider-hosted session gives `handlePermission`'s C3 ancestry walk
   (`internal/membership.Resolve`) no pid to ever resolve a root from, even
   once a real policy engine exists. A Claude Code hook call for one of
   these sessions is therefore refused with a `403` before it ever reaches
   the hard-coded deny. ~~The net effect: every `PreToolUse` hook call from a
   claude/pi/chat session today is silently allowed through, gated by
   nothing at all.~~ **Fixed.** See [Tool
   permissions](#tool-permissions-post-permission) for the real decision
   flow, including the live provider roots that let the ancestry walk
   resolve a claude session at all. One correction to the claim
   struck above: a hook call that got no decision was never "silently
   allowed" — the `403` sent it to Claude Code's own permission check
   instead, and under `--print` with no matching allow rule, that check
   refused MCP tools, `Bash` and edits, and allowed only read-only
   built-ins. The live bug was Claude Code refusing calls relay never got a
   chance to approve, not an open gate; `internal/sessions/hook.Run` denies
   on a non-`200` response now, rather than the exit-0, no-output "no
   decision" that produced that refusal.

## Code map

| concern | file |
|---|---|
| binary entry, `service` mode | `cmd/relaysessions/main.go` |
| shim (`exec` mode) | `internal/sessions/shim/shim.go` |
| hook client (`hook` mode) | `internal/sessions/hook/` |
| internal API server, `/launch`/`/terminate`/`/permission`, and the mounted eve-facing surface | `internal/sessions/hostapi/{server,dispatch,types}.go` |
| terminal (pty) sessions | `internal/sessions/terminal/` |
| provider-hosted (claude/pi/chat) sessions | `internal/sessions/session/`, `internal/sessions/provider/` |
| provider stderr logging (Warn until first stdout, redaction) | `internal/sessions/provider/stderr.go` |
| eve-facing HTTP/WS handlers (mounted by `hostapi.New`/`ListenInternal`) | `internal/sessions/api/` |
| C3 process-ancestry membership | `internal/membership/` |
| tool-permission decisions: `Preflight`, the wait/decide flow behind `/permission` | `internal/sessions/permission/` |
| C7 sandbox profile rendering | `internal/sessions/sandbox/`, `cmd/relay/session_sandbox.go` |
| writable roots a read grant's links are checked against | `cmd/relay/session_sandbox_writable.go` |
| relay-side launch authorization | `cmd/relay/session_launch.go` |
| relay-side HTTP routes, resume, accounting | `cmd/relay/session_routes.go` |
| built-in service record, helper path resolution | `internal/service/builtin_sessions.go` |
| cdhash pinning | `internal/service/codesign_darwin.go`, `internal/service/helper_verify.go` |
| the two manifest routes' reservation | `internal/bridge/manifest.go`, `internal/config/models.go` (`RelaySessionsManifestRoutes`) |
