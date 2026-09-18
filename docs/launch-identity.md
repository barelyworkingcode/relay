# Launch identity

How a process relay launches proves who it is to relay without holding a relay
credential. This is relay's one protocol for every relay-launched process;
what differs between processes is only the identity record relay binds and the
capabilities that record carries.

## Why not a token in the environment

On macOS any process can read the startup environment and argv of every other
process running as the same user (`sysctl KERN_PROCARGS2`, which `ps -E`
uses), and Seatbelt cannot refuse that read without also breaking the Go
runtime. A bearer token in a service's environment is therefore a bearer token
every same-user process holds. No relay credential is placed in any
environment or argv.

## The protocol

### 1. The launch fd

For every launch, relay:

1. generates 32 random bytes and encodes them as **64 lowercase hex
   characters** — the launch secret, with no trailing newline;
2. creates a pipe, writes the secret into it, and **closes the write end**;
3. passes the read end as `exec.Cmd.ExtraFiles[0]`, which the child sees as
   **fd 3**, and sets `RELAY_LAUNCH_FD=3`;
4. closes its own copy of the read end once the child has started.

Relay keeps only the secret's SHA-256. The descriptor survives relay's
`$SHELL -l -c` launch and a wrapper script.

The service, **before it spawns anything**:

- reads fd 3 to EOF and closes it;
- refuses anything that is not exactly 64 lowercase hex characters;
- never logs the secret and never places it in a child's environment or argv.

### 2. Hello

On a fresh connection to `RELAY_BRIDGE_SOCKET`, one newline-terminated line:

```json
{"type":"Hello","name":"<RELAY_SERVICE_ID>","token":"<64 hex chars>"}
```

Relay compares the secret, in constant time, with the one it generated for the
live launch named `name`. On success it binds that launch to the **kernel audit
token of the connection's peer** (`getsockopt(SOL_LOCAL, LOCAL_PEERTOKEN)`:
pid at bytes 20–23, pidversion at 28–31, little-endian) and answers:

```json
{"type":"OK","data":{"kind":"service","service_id":"<name>","relay_pid":<relay's pid>}}
```

Hello binds **recognition**. The response never carries a credential; it may
carry non-secret configuration, never a secret. After Hello the process holds
no relay bearer at all.

Every refusal is the same frame:

```json
{"type":"Error","code":-32001,"message":"hello refused"}
```

The reason — no live launch by that name, a wrong or malformed secret, a launch
already bound, a process that already holds an identity, a peer whose audit
token cannot be read — goes to relay's log only. The message never echoes the
secret and never says which condition failed.

The secret is **spent** by a successful Hello: a second Hello for the same
launch is refused, even with the right secret. A wrong secret does not spend
it — a guess against 256 bits buys nothing, and spending on a miss would let
any same-user process that can name a service turn its start into a failure.

### 3. Authentication afterwards

A bridge request with no `token` — the key omitted or empty, which relay treats
identically — from a peer whose `(pid, pidversion)` equals a bound identity is
authenticated as that identity, on any connection the process opens. A request
that presents a token is judged as that token, never as the identity beside it.

The frontend socket reads the same peer audit token per connection and
resolves it per request: a request with **no `Authorization` header** from a
identity holding the `frontend` capability is that identity. A request with an
`Authorization` header is always judged as a bearer.

Only the exact process that said Hello is the identity. Its children are not,
nor is anything else running as the same user. With a wrapper script, the
process that presents the secret — the daemon, not the wrapper — becomes the
identity.

### 4. Lifetime

An identity lives from Hello until relay's service registry sees that launch
end: the launched process exits, or it is stopped. A restart is a new launch
with a new secret, a new Hello and a new identity, and beginning a launch ends
any earlier launch under the same name. Matching on pidversion as well as pid
means a recycled pid never inherits an identity. The table lives only in
relay's memory.

This holds identically whether the restart is an operator's (`relay service
restart`, Settings, tray relaunch) or relay's own, after a service exits on
its own and relay's supervision restarts it (`docs/service-manifest.md#restart-supervision`):
both call the same `Start`, both mint a fresh secret, and the exiting
process's own defers clear its identity before that call can begin. This is
what makes a service's own "restart myself" story safe to remove — a service
cannot hand its successor a still-valid secret to skip Hello, because there
is no such thing; the successor is a new launch like any other, and the
predecessor's identity is gone before it exists.

### A model key is bound to its launch

A session's model key (`rmk_…`, [`docs/model-endpoint.md`](model-endpoint.md#model-keys))
is not a launch identity, but it lives and dies with one. After a
`project_session` launch says Hello, `launchOnHost` binds the key it minted for
that session to the launch handle (`ModelKeyTable.BindLaunch`), and every
`Lookup` re-derives whether the launch is still live (`Launch.Live`) — the same
shape `ModelHostRegistry.liveLocked` gives a registered host, and for the same
reason: relay-sessions reporting `SessionExited` is not the only way a launch
ends. The root-exit watcher (`membership.WatchExit`) ends the launch when its
root process does, a later `Begin` under the same name replaces it, and
`EndByParent` ends every session a relay-sessions launch owned; each kills the
key on its next use, with no report, no timer and no sweeper. `Launch.WasBound`
is how the binder tells a launch that says Hello from one that never will.

A launch that never says Hello is not bound, on purpose: a `chat` session's
provider runs in-process with no shim, so its launch expires unbound after
`ProjectSessionLaunchTTL` and a key tied to it would die seconds into the
session. That key, and any key for an ad-hoc or SSH-hosted session (no launch
at all), keep `SessionExited`-only revocation.

## Fail-closed rules

- `RELAY_LAUNCH_FD` set, and the read fails or yields anything but 64 hex
  characters: the service exits non-zero.
- Hello refused or unanswered: the service exits non-zero. It never degrades to
  a relay-less mode while relay believes it launched it.
- `RELAY_LAUNCH_FD` unset: the process was not launched by relay and runs its
  own standalone mode.
- A tokenless bridge request from a peer whose identity does not hold the
  operation's capability is not a service: it falls to membership auth
  (plan-broker-and-sessions.md §2 C3), which can only ever yield a project's
  scope via a live session's verified process ancestry, and never reaches a
  service operation.
- A peer whose audit token cannot be read matches no identity.

## Environment

Set by relay on a service launch. None of it is secret.

| var | meaning |
|---|---|
| `RELAY_BRIDGE_SOCKET` | bridge socket path |
| `RELAY_SERVICE_ID` | the launch name Hello presents |
| `RELAY_FRONTEND_SOCKET` | frontend socket path, set exactly when the service holds the `frontend` capability |
| `RELAY_MCP_COMMAND` | relay binary path |
| `RELAY_LAUNCH_FD` | `3` |

Relay removes `RELAY_SERVICE_TOKEN`, `RELAY_MCP_TOKEN` and
`RELAY_FRONTEND_TOKEN` from every environment it passes to a service, including
anything relay's own environment carries (`bridge.RemovedCredentialEnv`).
A service should also strip `RELAY_LAUNCH_FD` from the environment of anything
it spawns.

## Identity kinds and capabilities

An identity record carries a `kind`. The protocol above is identical for every
kind; the kind decides which capability table the record is read against.
There is one decision function, `service.Allowed(kind, capabilities,
operation)`, and both the bridge router and the frontend server ask it and
nothing else.

Today every identity is kind `service`, and its capabilities are the service
record's `capabilities` — a set, fixed when the launch begins:

| capability | operations it grants |
|---|---|
| `frontend` | The frontend socket, with no `Authorization` header, holding `read`, `configure`, `proxy` and `execute` (never `grant`). Attributed in `control_decision` records as `launch:service:<id>`. Relay sets `RELAY_FRONTEND_SOCKET` exactly when this capability is held. `execute` was added by the approved F1/SP8 decision (plan-broker-and-sessions.md), once every other `execute`-class route on this socket was presence-gated or scoped to a launch — it is what lets eve reach the session-host launch routes (`POST /api/terminals`, `POST /api/sessions`, `POST /api/sessions/{id}/resume`). |
| `manifest` | `RegisterManifest`, only for a `serviceId` equal to the launch name. |
| `models` | Model-endpoint calls on `model.sock` with no header, limited by the service record's own `allowed_models` (empty means none, `["*"]` means every model — the opposite of a project's own default), and `GET /v1/models`. See [`docs/model-endpoint.md`](model-endpoint.md). |
| `model_host` | `RegisterModelHost`: registering this service's router socket as the model endpoint's one upstream, under its own id only. See [`docs/model-endpoint.md`](model-endpoint.md). |
| `sessions` | `SessionExited`, and `GET /v1/models` unfiltered (never a model call). Only the built-in `relaysessions` service record may hold this — any other record naming it fails validation. |

`Hello` needs no capability: every launched service may say it. A service
with an empty set can start and say `Hello` and can do nothing else through
relay. The set grants the union of its capabilities' operations and nothing
else, and a capability name relay does not know grants nothing.

The retired `projects` capability (`ResolvePtyEnv`, `ResolveProjectTemplate`,
`ListProjects`, `GetProject`, and tokenless `ListTools`/`CallTool` across
every MCP) no longer exists: those operations were deleted outright
(plan-broker-and-sessions.md §2 C1) in favor of the `project_session` kind
below. A stored record naming `projects` is not refused — the name is
silently dropped on load, logged at info, and persists without it on the
next write, so an existing install keeps starting across the upgrade.

| service | capabilities |
|---|---|
| eve, relaySTT | `frontend` |
| relayLLM | `manifest`, `model_host` |
| relayTTS | `manifest`, `models` |
| relayScheduler | `frontend`, `manifest` |

### The service record

```json
{"id": "relayllm", "command": "…", "capabilities": ["manifest", "model_host"]}
```

Every record relay writes carries `capabilities`, an empty set as `[]`. A
record whose `capabilities` names anything other than `frontend`, `manifest`,
`models`, `model_host` or `sessions` fails validation: relay logs it on load,
keeps it in `settings.json` untouched, and refuses to start it. `sessions` is
additionally refused on any record but the built-in `relaysessions` one.

A record written before capabilities existed has no `capabilities` key
(`null` reads the same) and may carry `frontend_consumer`. It is migrated once,
in memory, on load — `frontend_consumer` unset or `true` becomes
`["frontend"]`, `false` becomes `["manifest"]` — and the next write
persists `capabilities` and drops `frontend_consumer`. A record that already
has `capabilities` keeps them (minus a retired `projects` entry, dropped the
same way) and its `frontend_consumer` is discarded.

`relay service register --capability NAME` (repeatable) sets the set; a
register with no `--capability` sets the empty set and says so. An HTTP or
Settings-window update that omits `capabilities` keeps the stored set.

### Editing capabilities from the Settings window

The service create/edit dialog carries a checkbox per known capability a
user may register for their own service (`frontend`, `manifest`, `models`,
`model_host`; `sessions` is deliberately not offered here, since only the
built-in `relaysessions` record may ever hold it); an unknown name is
refused server-side before anything is written
(`config.ServiceConfig.validateCapabilities`), the same check a CLI
`--capability` typo hits.

`command`, `args`, `working_dir`, `url` and `autostart` have no narrower
reading, so any actual change to one of those gates, and a resend that
changes nothing at all (the dialog always sends the whole record on every
save, so a byte-identical resend must not manufacture a prompt either)
needs no gate. `display_name`, `capabilities` and `allowed_models` gate on
ANY actual change, in either direction — an earlier revision of
`serviceUpdateNeedsGate` (`cmd/relay/service_ops.go`) read ADR-018 decision
1 (obtaining or widening a capability is privileged, using or narrowing one
is not) as meaning dropping a capability or model id needed no prompt, and
never inspected `display_name` at all; that was correct only while an
operator-minted credential was the sole way to reach this route. Since
eve's frontend launch identity was granted `execute`
(plan-broker-and-sessions.md's F1/SP8 decision), any frontend-capable
service can reach this route for ANY service's record, not just its own, so
a silent rename or a silent capability/model narrowing became a real
integrity/availability exposure and both now gate too
(`STATUS-relay-security.md`).

Capabilities is written by full replacement, the same as the CLI's own
register semantics: a save that omits the field from the request (there is
no such door today, the Settings window always sends it) keeps the stored
set, but a save that sends it, however it's spelled, names every capability
the service holds after that save — there is no partial "add one, leave the
rest" shape on the wire.

### `project_session`: the session-host identity kind

`project_session` is the second `kind` value, bound by this same launch fd,
Hello and audit-token check — a session-host root process (a
`relay-sessions exec` shim, per C2 and C6) rather than a registered service.
Unlike `service`, its authority is not a fixed capability set but the named
project's own live grant: `service.Allowed(IdentityKindProjectSession, nil,
op)` grants a fixed operation set — `Hello`, `ListTools`/`CallTool`
(`OpProjectTools`), `DescribeProject`, `ListSkillBuckets`, and model-endpoint
calls/listing (`OpModelCall`/`OpModelList`) — scoped at call time by the
project itself, never by a capability list on the identity. `BindKind`
additionally pins the root process's exact kernel start time
(`RootStartSec`/`RootStartUsec`) and registers an ancestry-exit watch
(`internal/membership.WatchExit`) so the identity ends the moment its root
process does, not only when the service registry notices a launch end.

**Root-vs-descendant membership is implemented and live**: a tokenless
caller reaching a `project_session`'s grant by being a real, kernel-verified
process-tree descendant of its root — never by presenting a secret of its
own — is `resolveAuth`'s C3 step (`cmd/relay/router.go`), consulted only
once a token is absent and the peer holds no bound launch identity of its
own. `internal/membership.Resolve` walks the caller's ancestry via
`proc_pidinfo`, matching a candidate root by pid **and** its exact process
start time (a pid number alone is reused too often to trust), up to a
`relay-sessions exec` shim or relay's own host pid, whichever it meets
first. A caller admitted this way authenticates as `AuditAuthSession`
(`internal/audit/audit.go`) with `AuditActorProjectSession`, carrying the
session id that vouched for it — this is what replaced the retired
`allow_cwd_auth` mechanism (see [`docs/tokens.md`](tokens.md#directory-auth-allow_cwd_auth-retired)).

The session's root process itself — the shim that said Hello — is
authenticated as the bound `project_session` identity directly, the same
peer-audit-token match every launch identity uses; C3's ancestry walk is
only consulted for its *descendants* (the target the shim spawned, and
anything that target spawns in turn).

`RELAY_PROJECT_TOKEN` is unaffected by this kind's existence: a project
shell or agent CLI spawned outside the session-host path still gets one
injected as before. A session-host-launched `pty` instead relies on its
bound `project_session` identity, or C3 membership if it is a descendant
rather than the root — no project token is injected into a session-host
child's environment at all.

**This does not hold for a `claude`/`pi` launch.** Neither
`provider.ClaudeConfig` nor `provider.PiConfig` (`internal/sessions/provider`)
carries an `Identity` field, so neither process — nor anything it spawns —
ever says Hello, and there is no bound `project_session` identity for it at
all. Nor does C3 membership cover it: `(*service.Launches).RootByPID` only
recognizes a pid as a membership root when it is bound in the launch table
under `IdentityKindProjectSession`, so an unbound `claude`/`pi` launch is
nobody's root — there is nothing for a descendant to authenticate against
either. A `claude`/`pi` launch authenticates by neither mechanism today; see
[`docs/session-host.md`](session-host.md#what-is-not-built-yet) gap 1 and
[`docs/ssh-hosts.md`](ssh-hosts.md#the-session-host-never-sandboxes-a-host-projects-session),
which already state this gap honestly.

Full design of the binary that launches these identities, the internal API
that authorizes a launch, and the shim that presents the secret:
[`docs/session-host.md`](session-host.md).

Code: `internal/service/launch_identity.go` (the table and `Identity`),
`internal/peertoken` (the audit token), `internal/bridge/launch.go` (the Go
service half: `ReadLaunchSecret`, `SendHello`), `cmd/relay/router.go`
(`Hello`, `resolveAuth`, `requireServiceIdentity`),
`cmd/relay/frontend_server.go` (`frontendCredentialAuth`).

## What this does not defend against

- **Code execution inside the identified process.** Anything running as that
  process — an injected library, a compromised dependency, a debugger attached
  to it — is that process, and holds its identity.
- **Debugging a non-hardened process.** A same-user process may be able to
  attach to a service built without the hardened runtime and act inside it;
  launch identity does not change what `task_for_pid` allows.
- **The launched process's own children, if the service leaks the secret.** A
  service that does not close fd 3 before spawning, or that logs or exports the
  secret, hands a child the chance to Hello first. The pipe is drained by the
  service's read, so a child that inherits fd 3 afterwards reads EOF.
- **Root or the machine's owner acting as root.** The audit token is the
  kernel's account of a process; a principal that controls the kernel's
  answer is outside this model.
- **A login shell profile.** `$SHELL -l -c` runs the user's profile before the
  service; a profile that reads fd 3 or exports a variable acts with the user's
  authority.
