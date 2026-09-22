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
| 400 | `invalid_spec` | malformed or self-contradictory request (the default for an unnamed error on a `pty` launch; a `claude`/`pi`/`chat` launch instead defaults an unnamed error to `500`/`spawn_failed` — `terminalLaunchStatus` and `sessionLaunchStatus`, `internal/sessions/hostapi/dispatch.go`, disagree on this) |
| 409 | `session_exists` | this session id is already live |
| 502 | `identity_refused` | the shim's Hello did not bind |
| 500 | `spawn_failed` | the target process could not be started |

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
this plainly). `buildChatMCPManager` (`chat_base.go`) can in principle spawn
a chat session's relay-MCP tool child through the shim — the code path
exists, gated on the session opting into `useRelayTools` and `RelayMCPCommand`
being non-empty — but `cfg.RelayMCPCommand` is never set in production:
`cmd/relaysessions/main.go`'s own `ChatConfig` literal omits it entirely, and
only test code ever sets it. `buildChatMCPManager` therefore always returns
`nil` today, regardless of a session's own settings, so this tool child is
never actually spawned, shim or otherwise.

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
`(deny file-read* file-write*)`, then the grants, then read-only `stat` on the
parents of each grant so a process can reach it, then the template's `deny`
list (below). Nothing is denied by name unless a template says so.
Relay's own data directory, another project, eve's data and `~/.ssh` are
unreachable unless a template grants them, so a directory nobody thought to
protect is protected anyway. Everything that is not a file (network, process,
mach) stays `(allow default)`; the unix-socket, loopback and setuid rules are
separate and unchanged.

A session's folders come from four places, and only the third is configured:

| Grant | Paths | Where it lives |
|---|---|---|
| **System baseline** (read-only, every sandboxed session) | `/usr`, `/System/Library`, `/private/etc`, `/private/var/db/timezone`, `/private/var/select`, and the root directory and the `/var`, `/etc`, `/tmp` links themselves | `sandbox.baselineReadDirs`. The smallest set a shell, `git`, `curl`, `ssh`, `python`, `go` and `node` needed, measured under a deny-all profile on macOS 26. It holds no user data. |
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
the same entry shape as `read` and `read_write`, is ignored without
`"sandbox": true`.

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
(`.lock`, `.tmp.*`, `.backup`; SP2 row 17). A read-write directory that does
not exist is created at launch, because a `(subpath)` rule cannot create its
own ancestors (`go build` with no `~/go` needs `~/go/pkg` to exist). An entry
that cannot be placed refuses the template when settings are read, and the
launch if it slips through, rather than being dropped: a dropped entry would
leave a tool silently unreachable. `sandbox` absent means unsandboxed, so a
template that should be confined says `"sandbox": true`; `read` and
`read_write` are ignored without it.

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
(`project_required`), so every session that can exist carries a project's
authority and none auto-respawns.

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
     in `hostapi`'s `201` response — a real root (the shim's own pid) now
     exists for a shim-wrapped claude/pi launch, but no
     `provider.Provider` implementation exposes a pid this handler could
     report, and wiring that through is a separate piece of work.
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
   sandbox or identify. `ChatConfig` already carried the `Sandbox` and
   `Identity` fields its optional relay-MCP tool child would need to run
   through the shim like a terminal (`buildChatMCPManager`), but that child
   is never actually spawned in production today — see [The
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
6. **`/permission` is a hard-coded refusal, and a provider-hosted session
   has no membership entry to even reach it.** `handlePermission`
   (`internal/sessions/hostapi/server.go`) answers every admitted call
   `{"decision":"deny","reason":"session host: no policy engine wired
   yet"}` — a fixed placeholder, not a policy engine. Worse, `launchSession`
   (`internal/sessions/hostapi/server.go`) deliberately registers no
   membership table entry for a claude/pi/chat launch at all: a
   provider-hosted session gives `handlePermission`'s C3 ancestry walk
   (`internal/membership.Resolve`) no pid to ever resolve a root from, even
   once a real policy engine exists. A Claude Code hook call for one of
   these sessions is therefore refused with a `403` before it ever reaches
   the hard-coded deny, and `internal/sessions/hook.Run` fails open on that
   `403` — a non-`200` response returns exit code `0`, the same as an
   explicit allow. The net effect: every `PreToolUse` hook call from a
   claude/pi/chat session today is silently allowed through, gated by
   nothing at all.

## Code map

| concern | file |
|---|---|
| binary entry, `service` mode | `cmd/relaysessions/main.go` |
| shim (`exec` mode) | `internal/sessions/shim/shim.go` |
| hook client (`hook` mode) | `internal/sessions/hook/` |
| internal API server, `/launch`/`/terminate`/`/permission`, and the mounted eve-facing surface | `internal/sessions/hostapi/{server,dispatch,types}.go` |
| terminal (pty) sessions | `internal/sessions/terminal/` |
| provider-hosted (claude/pi/chat) sessions | `internal/sessions/session/`, `internal/sessions/provider/` |
| eve-facing HTTP/WS handlers (mounted by `hostapi.New`/`ListenInternal`) | `internal/sessions/api/` |
| C3 process-ancestry membership | `internal/membership/` |
| C7 sandbox profile rendering | `internal/sessions/sandbox/`, `cmd/relay/session_sandbox.go` |
| relay-side launch authorization | `cmd/relay/session_launch.go` |
| relay-side HTTP routes, resume, accounting | `cmd/relay/session_routes.go` |
| built-in service record, helper path resolution | `internal/service/builtin_sessions.go` |
| cdhash pinning | `internal/service/codesign_darwin.go`, `internal/service/helper_verify.go` |
| the two manifest routes' reservation | `internal/bridge/manifest.go`, `internal/config/models.go` (`RelaySessionsManifestRoutes`) |
