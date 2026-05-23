# Service Manifest — design spec

## Context

Relay's job is "container/orchestrator for services" + "project administration + security." It is a *registry*. Today it has accidental hardcoded knowledge of one specific service (relayLLM) in three places:

1. `frontend_proxy.go` / `frontend_server.go` reverse-proxies every `/api/*` and `/ws` request to relayLLM specifically.
2. `relay_llm_channel.go:EnvFor` switches on the literal slugs `"relayllm"` / `"relay-llm"` to inject internal-socket creds.
3. `LLMChannel` itself is built for one service — there is no path for relayScheduler or any future service to plug in symmetrically.

A second symptom of the same problem lives in relayLLM: it carries `scheduler_proxy.go` + `scheduler_ws.go` so that Eve, which connects "only to relayLLM," can reach relayScheduler. One service holding hardcoded knowledge of another.

This spec replaces those hardcoded bindings with a **manifest protocol**: each service that wants to integrate with relay declares its routes, status surface, and user actions; relay reads the manifest and wires everything generically. Adding a new first- or third-party service becomes a matter of implementing the protocol, not editing relay.

## Three-tier service model

Every entry in `Settings.Services` is one of:

1. **Generic.** Relay spawns it, captures logs to `~/.relay/logs/{id}.log`, lets the user start/stop it. Opaque otherwise. No new requirements.
2. **Relay-enhanced.** Service implements the manifest protocol (below). Standalone, it reads config from disk and serves directly. Under relay, it lights up: front-door routing, shared MCP, project-scoped sessions, status UI, etc.
3. **First-party.** relayLLM, relayScheduler, Eve, relayComfy. Same protocol as #2 — they are reference implementations. No privileged path.

The protocol must be small enough that a third party can implement it in an afternoon. That's the success criterion.

## Protocol

### 1. Detection sentinel — automatic, no flag

Every service relay spawns receives a `RELAY_BRIDGE_SOCKET` env var. The service itself decides what to do with it:

```
RELAY_BRIDGE_SOCKET=/path/to/relay.sock
RELAY_SERVICE_ID=relayllm
```

Present → enhanced mode (bind a listener, dial bridge, register manifest).
Absent  → standalone mode (read config from disk, serve directly).

No `enhanced: true` setting in relay. No hardcoded slug list. A service that doesn't implement the protocol just ignores the env vars; no manifest registers; relay never dispatches to it. A service that does implement it lights up automatically.

The detection is a deployment fact, not a code fork. The service has one config loader; only the *source* of config differs.

### 2. Manifest

```jsonc
{
  "routes":  ["/api/sessions/", "/ws"],
  "status":  { "path": "/api/status" },
  "actions": [
    {
      "id":           "stop_llama",
      "label":        "Stop",
      "method":       "DELETE",
      "pathTemplate": "/api/llama/instances/{alias}"
    }
  ]
}
```

That's the entire schema for V1.

- **`routes`**: path prefixes (and exact paths) the service serves. Drives relay's front-door dispatcher. WebSocket paths are valid entries.
- **`status`**: a single GET endpoint relay polls (~2s) to render in the settings UI. Free-form JSON; the UI renders it generically.
- **`actions`**: user-triggerable RPCs that show up as buttons in the settings UI. V1: flat — one declaration = one global button. `pathTemplate` is a literal HTTP path (no substitution).

No `version`, `consumes`, `provides`, `logs`, `schema`, or auth declarations. When a real need shows up, add the field; defer until then.

**Per-row actions are a deliberate V1 omission.** When we want a "Stop this llama instance" button per row in a status table, we add a single optional field — e.g. `forEach: "instances"` — that tells the UI to render one button per entry in the named status array, with `{key}` placeholders substituted from the row. Additive, non-breaking, deferred until needed.

### 3. Bridge handshake

The existing `relay.sock` (per `bridge/types.go`) is already the relay↔service control plane (MCP tool calls, reconcile notifications). Manifest registration becomes one new request on the same socket. **The service picks its own listener address + bearer token** and tells relay both, so relay never has to dictate or guess where the service listens:

```
service starts
service detects RELAY_BRIDGE_SOCKET, picks its own internal socket path + bearer token
service binds the listener (0600 perms)
service dials relay.sock, sends RegisterManifest{ServiceID, Manifest, InternalSocket, InternalToken}
relay authenticates the bridge call with the service's MCP token (already issued at spawn)
relay validates the manifest, checks route conflicts, stores everything
relay updates its dispatch table; front-door requests start flowing
```

Lifecycle:

- **Re-register** is allowed (idempotent for the same payload; on change, dispatch table is rebuilt).
- **Bridge disconnect** = service is going away. Relay removes routes from the dispatch table. Subsequent requests for those routes get 503 until re-registration.
- **Conflicts** (two services declare the same route prefix) fail the second `RegisterManifest` with an error; the service can log and exit, or back off.
- **Socket cleanup** is the service's responsibility: remove any stale socket at the chosen path on startup, `os.Remove` on shutdown. Relay never sees the file.

### 4. Front-door dispatch

Relay's `frontend_server.go` is rebuilt as a **per-service reverse proxy with a path-prefix dispatch table**. Pseudocode:

```
on HTTP request:
  service := dispatchTable.LongestPrefixMatch(req.URL.Path)
  if service == nil: 404
  reverseProxy(service.InternalSocket, service.Token, req)

on WS upgrade:
  service := dispatchTable.LongestPrefixMatch(req.URL.Path)
  if service == nil: 404
  wsProxy(service.InternalSocket, service.Token, req)
```

Per-service internal sockets + tokens are provisioned by relay at manifest-registration time (generalizing today's `LLMChannel.Internal`). The bearer-token injection / inbound-auth-strip pattern from `newRelayLLMProxy` is preserved — just per-service instead of hardcoded.

Project routes (`project_routes.go`) and any other relay-owned middleware run *before* dispatch, exactly as today.

## Standalone vs enhanced

| Aspect | Standalone | Enhanced (RELAY_BRIDGE_SOCKET set) |
|---|---|---|
| Listener address | own TCP/Unix socket from config | relay-issued internal Unix socket |
| Auth | self-managed token or open loopback | relay-issued ephemeral bearer |
| MCP | none, or local config (V1: none) | available via injected env (`RELAY_MCP_TOKEN` + `RELAY_MCP_COMMAND`) |
| Scheduler integration | n/a | requests flow through relay's dispatcher to relayScheduler |
| Project scope | flat session namespace | opaque mcpToken in session payloads (see below) |
| Status / actions UI | none | rendered from manifest in relay's settings window |
| Manifest registration | skipped | sent on bridge connect |

The wire language never changes — only the source of configuration.

## Project scope: no new plumbing

Projects are entirely a relay concern. Relay knows the user, the project, and issues a project-scoped opaque `mcpToken` when relaying session-creation requests. Services treat the token as opaque bytes:

- They forward it to spawned CLIs (Claude, pi) so the spawned `relay-mcp` subprocesses authenticate scoped to that project.
- They use it themselves for MCP calls from their tool loops.
- They never decode it, never branch on it, never log "project" as a concept.

This is already how today's session API works (`McpToken` field on session creation). The spec just locks it in: **no `X-Relay-Project` header, no path prefix, no project-aware dispatch.** Project scope is end-to-end opaque from the service's perspective.

## Migration plan

Sequenced, each step reversible/buildable:

### relay (`service-manifest` branch)

1. Bridge: add `RegisterManifest` request type, schema validation, dispatch-table store.
2. Per-service internal-socket+token provisioning (generalize `LLMChannel.Internal`).
3. New `frontend_dispatcher.go` with prefix-match routing; deprecate `newRelayLLMProxy` (becomes one entry in the table at runtime).
4. Settings window: generic "Service" tab driven by each registered manifest's status/actions (replaces the hardcoded approach that was shelved).
5. Delete `LLMChannel.EnvFor`'s hardcoded slug switch — env vars come from the per-service provisioning step.

### relayLLM (`service-manifest` branch — created after spec review)

1. Detect `RELAY_BRIDGE_SOCKET`; if set, connect to bridge and send manifest after listener is ready.
2. Listener address comes from `RELAY_INTERNAL_SOCKET` (enhanced) or config file (standalone).
3. **Delete `scheduler_proxy.go` and `scheduler_ws.go`.** Eve will reach relayScheduler through relay's front door directly.
4. Re-land the shelved API additions (`llama_manager.ListInstances/StopInstance`, `api_status.go`) — they are exactly what the manifest's `status` and `actions` declarations point at.

### relayScheduler (`service-manifest` branch)

1. Same detection sentinel + manifest registration.
2. Declares `/api/tasks/`, `/ws/tasks` (or similar) so relay can dispatch directly.

### Eve

1. No code change to URL/auth (Eve already talks to relay's frontend socket).
2. Verify task and session traffic flows correctly through the dispatched front door instead of through relayLLM's deleted scheduler proxy.

## Verification

End-to-end check the new pattern works:

1. **Standalone relayLLM**: `RELAY_BRIDGE_SOCKET` unset → relayLLM binds its own listener per `config.json`, serves `/api/sessions`, no MCP, no scheduler. `curl` it directly. Works.
2. **Enhanced relayLLM under relay**: relay spawns it with bridge socket env. relayLLM registers manifest. `curl` to relay's frontend hits `/api/sessions` → dispatched → relayLLM responds. WS upgrade works.
3. **Scheduler routes through relay**: with relayLLM-scheduler-proxy deleted, Eve creates a scheduled task. Request hits relay frontend → dispatched to relayScheduler. Task fires, calls back into relayLLM via relay's frontend (no service-to-service hardcoded URL).
4. **Status UI renders generically**: relayLLM and relayScheduler both register `status` endpoints with different shapes. Both render in the settings panel without relay containing any code specific to either service's response shape.
5. **Stop a llama instance**: action button in relayLLM's panel dispatches `DELETE /api/llama/instances/qwen3-8b` through relay's front door. Server-side stop executes. Status poll re-renders the table without the row.

## Out of scope (deferred — add when needed)

- Manifest versioning
- Capability advertisements (`provides` / `consumes`)
- WS event-stream declarations (today every service serves `/ws`; defer until two services need different WS paths)
- Cross-service event subscription (today no service subscribes to another's events)
- Action confirmation prompts in the UI (defer until an action is destructive enough to need one)
- Per-action authorization scopes (current model: any action declared in manifest is callable by relay; relay's own auth gates who can talk to relay's frontend)
