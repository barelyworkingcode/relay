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
```

Present → enhanced mode (bind a listener, dial the bridge, register a
manifest). Absent → standalone mode (read config from disk, serve directly).

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
  (e.g. `/ws`) are valid entries.
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
service detects RELAY_BRIDGE_SOCKET, picks its own internal socket + bearer token
service binds the listener (0600 perms)
service dials the bridge, sends RegisterManifest{serviceId, manifest, internalSocket, internalToken}
relay authenticates the call with the service's MCP token (issued at spawn)
relay validates the manifest, checks route conflicts, updates its dispatch table
front-door requests start flowing
```

Lifecycle:

- **Re-register** replaces the prior record (the service is the source of truth
  for its own routes, address, token); the dispatch table rebuilds.
- **Bridge disconnect / process exit** ⇒ relay `Forget`s the service and drops
  its routes; subsequent requests 404 until re-registration.
- **Route conflict** (two services declare the same exact route string) fails
  the second `RegisterManifest`; the service can log and exit or back off.
- **Socket cleanup** is the service's job: remove a stale socket on startup,
  `os.Remove` on shutdown. Relay never touches the file.

## Front-door dispatch

`frontend_server.go` wires relay-internal project routes first, then falls
through to `frontend_dispatcher.go`. The dispatcher does longest-prefix-match
against every registered manifest's routes, then reverse-proxies to the
matching service's internal Unix socket — one handler serves both HTTP and WS
(it detects upgrades). It strips inbound `Authorization` (the frontend token,
already validated) and injects the service-declared internal token. Two trust
boundaries stay distinct: frontend token authenticates Eve/Scheduler → relay;
internal token authenticates relay → service.

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
