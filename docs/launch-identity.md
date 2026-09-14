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

## Fail-closed rules

- `RELAY_LAUNCH_FD` set, and the read fails or yields anything but 64 hex
  characters: the service exits non-zero.
- Hello refused or unanswered: the service exits non-zero. It never degrades to
  a relay-less mode while relay believes it launched it.
- `RELAY_LAUNCH_FD` unset: the process was not launched by relay and runs its
  own standalone mode.
- A tokenless bridge request from a peer whose identity does not hold the
  operation's capability is not a service: it falls to directory auth (`allow_cwd_auth`), which can only ever
  yield a project, and never reaches a service operation.
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
| `frontend` | The frontend socket, with no `Authorization` header, holding exactly `read`, `configure` and `proxy` (never `grant` or `execute`). Attributed in `control_decision` records as `launch:service:<id>`. Relay sets `RELAY_FRONTEND_SOCKET` exactly when this capability is held. |
| `manifest` | `RegisterManifest`, only for a `serviceId` equal to the launch name. |
| `projects` | `ResolvePtyEnv`, `ResolveProjectTemplate`, `ListProjects`, `GetProject`, and tokenless `ListTools`/`CallTool` across every MCP. |

`Hello` needs no capability: every launched service may say it. A service
with an empty set can start and say `Hello` and can do nothing else through
relay. The set grants the union of its capabilities' operations and nothing
else, and a capability name relay does not know grants nothing.

| service | capabilities |
|---|---|
| eve, relaySTT | `frontend` |
| relayLLM | `manifest`, `projects` |
| relayTTS | `manifest` (a migrated record starts with `manifest`, `projects`; narrow it with `relay service register --capability manifest`) |
| relayScheduler | `frontend`, `manifest` |

### The service record

```json
{"id": "relayllm", "command": "…", "capabilities": ["manifest", "projects"]}
```

Every record relay writes carries `capabilities`, an empty set as `[]`. A
record whose `capabilities` names anything other than `frontend`, `manifest`
or `projects` fails validation: relay logs it on load, keeps it in
`settings.json` untouched, and refuses to start it.

A record written before capabilities existed has no `capabilities` key
(`null` reads the same) and may carry `frontend_consumer`. It is migrated once,
in memory, on load — `frontend_consumer` unset or `true` becomes
`["frontend"]`, `false` becomes `["manifest", "projects"]` — and the next write
persists `capabilities` and drops `frontend_consumer`. A record that already
has `capabilities` keeps them and its `frontend_consumer` is discarded.

`relay service register --capability NAME` (repeatable) sets the set; a
register with no `--capability` sets the empty set and says so. An HTTP or
Settings-window update that omits `capabilities` keeps the stored set.

### Later kinds

A project session, whose capability is one project's grant, is a new `kind`
value and a new table in `service.Allowed`, bound by this same launch fd,
Hello and audit-token check. `RELAY_PROJECT_TOKEN` is still injected into
project shells today; it is not governed by this document yet.

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
