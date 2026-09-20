# Service Manifest protocol

Canonical contract for how an enhanced service integrates with relay's front
door and settings UI. Authoritative type definitions live in
`bridge/manifest.go` (the doc-commented Go structs); this file is the prose
spec and rationale.

## Why this exists

Relay is a container/orchestrator + project-administration/security boundary —
a *registry*. It must hold no hardcoded knowledge of any specific service.
Before this protocol, relay reverse-proxied the front door to one service
(relayLLM) by name and injected its creds by matching literal slugs, and
relayLLM in turn carried a hardcoded proxy so it could reach relayScheduler on
relay's behalf. One service holding hardcoded knowledge of another.

The manifest protocol removes all of that. Each service declares its routes,
status surface, user actions, and editable config; relay wires everything
generically. Adding a first- or third-party service is a matter of implementing
the protocol, not editing relay. The protocol is small enough to implement in
an afternoon — that is the success criterion.

## Three-tier service model

Every entry in `Settings.Services` is one of:

1. **Generic.** Relay spawns it, captures logs to
   `~/Library/Application Support/relay/logs/<id>.log`, lets the user
   start/stop it. Opaque otherwise. No new requirements.
2. **Relay-enhanced.** Implements the manifest protocol. Standalone it reads
   config from disk and serves directly; under relay it lights up: front-door
   routing, shared MCP, project-scoped sessions, status UI.
3. **First-party.** relayLLM, relayScheduler, Eve, relayComfy — reference
   implementations of tier 2. No privileged path in relay.

## Detection sentinel — automatic, no flag

Every spawned service receives:

```
RELAY_BRIDGE_SOCKET=/path/to/relay.sock
RELAY_SERVICE_ID=relayllm
RELAY_LAUNCH_FD=3
```

Present → enhanced mode (read the launch secret and say `Hello`, bind a
listener, register a manifest). Absent → standalone mode (read config from
disk, serve directly). None of these is a credential; how a launched process
proves who it is — the launch fd, `Hello`, and authentication by peer audit
token — is [`docs/launch-identity.md`](launch-identity.md).

There is no `enhanced: true` setting and no slug list in relay. A service that
doesn't implement the protocol simply ignores the env vars; no manifest
registers and relay never dispatches to it. The source of config is a
deployment fact, not a code fork — the service has one config loader.

## Manifest

```jsonc
{
  "routes": ["/api/sessions/", "/ws"],
  "status": { "path": "/api/status" },
  "actions": [
    { "id": "stop_llama", "label": "Stop", "method": "DELETE",
      "pathTemplate": "/api/llama/instances/{alias}", "forEach": "instances" }
  ],
  "config": {
    "path": "/Users/me/.relayllm/config.json",
    "label": "config.json",
    "schema": [ { "id": "apiKey", "label": "API key", "type": "secret" } ]
  }
}
```

- **`routes`**: path prefixes (ending `/`) and exact paths the service serves.
  Drives the front-door dispatcher's longest-prefix match. WebSocket paths
  (e.g. `/ws`) are valid entries. **A route may not claim a path relay itself
  serves** — see the conflict rules below.
- **`status`** (optional): a single GET endpoint relay polls (every
  `StatusPollInterval`, 2s) to render in the settings UI. Free-form JSON,
  rendered generically.
- **`actions`** (optional): user-triggerable RPCs surfaced as buttons.
  A flat action (empty `forEach`) is one global button. A `forEach` action
  names a top-level array key in the status response; the UI renders one button
  per row and substitutes the row's keys into `{placeholders}` in
  `pathTemplate`. The manifest *is* the action whitelist — relay refuses any
  action not declared, paths come only from the manifest, and row *values* are
  URL-escaped before substitution (`ipc_service_action.go`).
- **`config`** (optional): one editable config file plus the schema relay
  renders a nested form from. Relay reads and writes the file *directly from the
  tray process* (the service hosts no endpoint; bytes are opaque text on the
  wire), validates it parses, and restarts the service to apply unless
  `applyMode: "live"`. A declared path is never trusted blindly: `..` segments
  are rejected at registration time (`ConfigDecl.validate` in
  `bridge/manifest.go`), and at use time `resolveConfigPath`
  (`service_config_file.go`) re-enforces absolute path, allowed-root containment
  (via `EvalSymlinks`), regular-file, and a size cap.

Field types: leaves `text`, `textarea`, `bool`, `number`, `select` (needs
`options`), `secret`, `string[]`, `stringMap`, `keyValue`, `json`; recursive
`object` (`fields`), `array`/`map` (`item`). See `FieldDecl` in
`bridge/manifest.go` for the full per-field semantics.

No `version`, `consumes`, `provides`, or auth declarations. Add a field when a
real need shows up; defer until then.

## Bridge handshake

The bridge socket (`RELAY_BRIDGE_SOCKET`) is the relay↔service control plane
(MCP tool calls, reconcile notifications). Manifest registration is one more
request on it. **The service picks its own internal listener address + bearer
token** and tells relay both, so relay never dictates or guesses where the
service listens:

```
service detects RELAY_LAUNCH_FD, reads the launch secret, sends Hello (docs/launch-identity.md)
service picks its own internal socket + bearer token
service binds the listener (0600 perms)
service dials the bridge, sends RegisterManifest{serviceId, manifest, internalSocket, internalToken} with no token
relay authenticates the call by the peer's launch identity, which must hold the manifest capability
relay refuses a serviceId other than the identity's own
relay validates the manifest, checks route conflicts, updates its dispatch table
front-door requests start flowing
```

Only a service whose record grants the `manifest` capability may register a
manifest (`relay service register --capability manifest`). The internal bearer lives only in the memory of relay and
the service.

Lifecycle:

- **Re-register** replaces the prior record (the service is the source of truth
  for its own routes, address, token); the dispatch table rebuilds.
- **Bridge disconnect / process exit** ⇒ relay `Forget`s the service and drops
  its routes; subsequent requests 404 until re-registration.
- **Route conflict** (two services declare the same exact route string) fails
  the second `RegisterManifest`; the service can log and exit or back off.
- **Relay-route conflict** (a declared route overlaps a path relay serves)
  fails `RegisterManifest` the same way. What an operator sees, on the
  service's side as the registration error and in relay's log:

  ```
  manifest registry: route "/api/projects" collides with "/api/projects", which relay serves
  ```

  The reserved set is **accumulated from relay's own registrations**
  (`RouteRegistrar`, ADR-015), never written down beside this check: a list
  maintained by hand drifts the first time someone adds a route, and a
  security check that has silently stopped covering half the surface is worse
  than none. Three properties follow from that:

  - **Overlap, not string equality.** A prefix route containing a relay path
    collides (`/api/` swallows `/api/projects`), and so does a path inside a
    relay prefix. Equality alone would let a manifest sit under relay and
    survive only because `http.ServeMux` prefers the more specific pattern —
    an ordering property, not a check.
  - **A wildcard reserves its subtree.** Relay serves `/api/projects/{id}`, so
    the whole of `/api/projects/` is relay's; a manifest may not place a route
    inside it.
  - **The `/` catch-all is not reserved.** It is the mount that reaches
    services, not a path relay serves. Reserving it would refuse every service
    there is.

  `/relay/` is refused separately and absolutely (ADR-016 decision 5), in its
  own words — it carries the unauthenticated login ceremony, which registers
  outside `RouteRegistrar` and so appears in no accumulated set.
- **Socket cleanup** is the service's job: remove a stale socket on startup,
  `os.Remove` on shutdown. Relay never touches the file.

### `/launch` and `/terminate`: reserved outside every manifest

Two routes are refused to **every** manifest, unconditionally, by
`Manifest.Validate` (`internal/bridge/manifest.go`) itself — not by the
route-conflict check above, and not overridable by any service, including
`relaysessions`: `/launch`, `/terminate`, and anything nested under either
(`/launch/…`, `/terminate/…`). These are `relay-sessions`' own internal
API (C5 §3.3): a second, peer-verified Unix socket relay dials directly,
mutually authenticated by the connecting pid matching relay's own pid (as
recorded at that host's Hello) and a bearer the host generated for itself
and told relay via `RegisterManifest` — never routed through the manifest
dispatcher's `/` catch-all, and never proxied to anyone. Refusing the two
path strings at the manifest-registration layer means relay-sessions cannot
accidentally (or any other service, maliciously) advertise either name in
its own *public* manifest and expose that peer-verification-free socket to
an ordinary frontend caller through the unverified reverse proxy. Full
design: [`docs/session-host.md`](session-host.md#the-internal-api-launch-and-terminate).

### The `sessions` capability and the shared-prefix exception

`relaysessions` — the built-in session-host service (`internal/service/builtin_sessions.go`,
synthesized fresh on every start, never a user-registered record) — is the
only service allowed to hold `config.ServiceCapabilitySessions` ("sessions"):
`internal/config/models.go`'s `validateCapabilities` refuses that capability
name on any other record's `capabilities` list outright, so nothing else can
ever be granted the `SessionExited` bridge op or the unfiltered model list
that capability also grants
([`docs/launch-identity.md`](launch-identity.md#identity-kinds-and-capabilities)).

Separately, `relaysessions`' manifest declares two path **prefixes**,
`/api/terminals/` and `/api/sessions/` (`config.RelaySessionsManifestRoutes`)
— and relay itself already registers several routes under those same two
paths directly, ahead of the manifest dispatcher: the bare creates
(`POST /api/terminals`, `POST /api/sessions`), the bare lists (`GET`), and
three of relay's own wildcard patterns, `POST /api/sessions/{$}`,
`POST /api/sessions/{id}/resume` and `POST /api/terminals/{$}`.
`relayRoutePath` (`cmd/relay/enhanced_services.go`) truncates each
registered pattern at its first `{`, so those wildcards reserve relay's own
subtree as `/api/sessions/` and `/api/terminals/` — the identical two
prefixes `relaysessions`' manifest declares. Ordinarily a manifest route
that overlaps a path relay itself serves is refused (the relay-route-conflict
rule above); `relaysessions` is the one, named exception
(`sessionHostSharedPrefixes` in `cmd/relay/enhanced_services.go`), keyed on
the *colliding relay route* being one of those two reserved prefixes — not
on the manifest route itself matching one of them exactly — so `relaysessions`
is exempt across that entire subtree and could legally declare something
like `/api/sessions/foo`, not only the two literal prefix strings.

The exemption largely holds in practice: for a (method, path) pair both
relay and `http.ServeMux` route to relay's own pattern, relay's more
specific registration wins over the manifest dispatcher's `/` catch-all, so
the create route and the manifest's identically-shaped route do not fight
over the same request. That precedence is narrower than "the two claims
cannot collide," though: it is scoped to the exact (method, path) pairs
relay itself registers — `GET /api/sessions/{id}` matches no relay pattern
at all and falls through to the manifest dispatcher genuinely, not just in
this check — and it holds only while relay's session routes are registered
in the first place. `RegisterSessionRoutes` runs only
`if deps.sessionHost.ready()` (`cmd/relay/frontend_server.go`); when that's
false, relay registers and reserves nothing under `/api/sessions/` or
`/api/terminals/`, so there is no "relay wins" precedence to fall back on
in that state — a request there reaches whatever the manifest dispatcher
resolves, unguarded by `AuthorizeLaunch`. This is exactly why the check
above is defense-in-depth rather than provably redundant. Concretely: `POST
/api/sessions` (create) is relay's; `GET /api/sessions/{id}` (once that
handler exists — see the gap below) is `relaysessions`' own manifest route.
This is the same split C5 describes as "relay's own routes (create, resume,
the bare list proxies) and relay-sessions' own manifest (every other
per-session operation underneath them)".

**The manifest also declares two bare paths relay never serves itself:**
`/api/models` and `/ws`. Unlike the two shared prefixes above, these need no
`sessionHostSharedPrefixes` exemption — relay registers nothing under either
path, so `collidingRelayRouteLocked` never finds anything to collide with.

`relaysessions`' internal mux serves the real per-session HTTP/WS surface
(`internal/sessions/api`) alongside `/launch` and `/terminate`, all guarded
by the same peer+bearer check (`hostapi.Server.guarded`) — a request that
reaches it via any of the four manifest routes gets a real handler, not a
placeholder 404. See
[`docs/session-host.md`](session-host.md#what-is-not-built-yet) gap 2 for
the mount itself and the single-trust-domain property it runs under.

## Restart supervision

Relay only ever started a service at tray launch (autostart) or on an
explicit `Start`/`Reload`, and never noticed one die on its own — a crashed
eve, relayLLM or relayTTS stayed down until the next tray restart. Launch
identity (`docs/launch-identity.md`) made this worse for a service trying to
restart itself: its launch secret is single-use and spent at Hello, so a
wrapper that respawned its own daemon produced a second process with no way
to get a secret of its own, and now exits `78` instead.

`internal/service.Registry` supervises every service it starts — autostart
or an explicit `Start` this session, never one the operator stopped
(`relay service` `stop`/`Stop`) — and restarts an unrequested exit through
the same `Start` path a fresh launch always uses (a brand new secret, a new
Hello; see `docs/launch-identity.md`'s Lifetime section for why the previous
identity is already gone by the time this runs). Numbers, all in
`internal/service/supervision.go`:

| constant | value | meaning |
|---|---|---|
| `ServiceRestartBaseDelay` | 1s | delay before the first restart attempt |
| `ServiceRestartMaxDelay` | 60s | backoff cap (doubles each attempt: 1s, 2s, 4s, 8s, 16s, ...) |
| `ServiceRestartMaxAttempts` | 5 | consecutive failures before relay gives up |
| `ServiceRestartStableWindow` | 60s | a run at least this long resets the attempt counter |

The counter bounds restart *intensity*, not lifetime attempts: a service that
crashes once a week is restarted forever, one that crashes on every launch is
marked **failed** (with its last exit code) after `ServiceRestartMaxAttempts`
and stays down until an explicit start or restart — `relay service restart`,
Settings, or a tray relaunch. Exit code 78 (`EX_CONFIG` — a service that
could not establish its launch identity) counts as a failure like any other
and is logged distinctly, since it usually means the service tried to
restart itself instead of exiting for relay to do it.

Every restart is one log line naming the service id, attempt number, exit
code and delay — never a secret, since none of those fields is one. State is
exposed read-only, never mutated by a reader: `relay service list`'s `STATE`
column and the Settings status poll both show `running` / `restarting
(attempt N, next in Xs)` / `failed (exit E)`, reading
`Registry.SupervisionStatuses()`, which exists only in the running tray's
memory (`relay service list` is itself answered by the tray, `service.list`,
which carries that state beside the records).

An operator `Stop` (directly, via `Remove`, or as `Reload`'s first half)
always retires the id's restart campaign before anything is killed, so a
service the operator stopped is never mistaken for a crash; `StopAll` (tray
shutdown) does the same for every id at once, cancelling any restart mid
backoff before a single process is torn down.

`frontend_server.go` wires relay-internal project routes first, then falls
through to `frontend_dispatcher.go`. The dispatcher does longest-prefix-match
against every registered manifest's routes, then reverse-proxies to the
matching service's internal Unix socket — one handler serves both HTTP and WS
(it detects upgrades). It strips inbound `Authorization` (a control-plane
credential, already validated, when one was sent) and injects the
service-declared internal token. Two trust boundaries stay distinct: a
launch identity holding `frontend` or a control-plane credential authenticates
the caller → relay; the internal token authenticates relay → service.

## Standalone vs enhanced

| Aspect | Standalone | Enhanced (`RELAY_BRIDGE_SOCKET` set) |
|---|---|---|
| Listener | own socket from config | service-picked internal Unix socket, declared via `RegisterManifest` |
| Auth | self-managed / open loopback | service-picked bearer, declared via `RegisterManifest` |
| MCP | none (V1) | `relay mcp` reachable via injected `RELAY_MCP_COMMAND`; tool calls are scoped by the project's opaque `mcpToken` (below), not a standing token |
| Project scope | flat session namespace | opaque `mcpToken` in session payloads (below) |
| Status / actions / config UI | none | rendered from manifest in settings |

The wire language never changes — only the source of configuration.

## Project scope: no new plumbing

Projects are entirely a relay concern. Relay issues a project-scoped opaque
`mcpToken` when relaying session-creation requests. Services treat it as opaque
bytes: forward it to spawned CLIs (so their `relay-mcp` subprocesses
authenticate scoped to the project) and use it for their own tool-loop MCP
calls. They never decode it, branch on it, or log "project" as a concept. No
`X-Relay-Project` header, no path prefix, no project-aware dispatch — project
scope is end-to-end opaque from the service's view. (See the token-brokering
model in `docs/decisions/007-project-token-brokering.md`.)

## Out of scope (deferred — add when needed)

- Manifest versioning.
- Capability advertisements (`provides` / `consumes`).
- Cross-service event subscription (no service subscribes to another's events).
- Action confirmation prompts (defer until an action is destructive enough).
- Per-action authorization scopes (current model: any action declared in a
  manifest is callable by relay; relay's own frontend auth gates who reaches
  relay).
