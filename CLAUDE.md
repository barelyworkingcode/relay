# Relay (Go)

macOS MCP orchestrator and project manager. Tray app with project-scoped auth, Unix socket bridge, external MCP proxy (stdio and HTTP transports), and background service management.

## Modes

- `relay` -- tray app (default). Hosts bridge socket, manages services and projects, shows settings UI.
- `relay mcp --token TOKEN` -- stdio MCP server. Connects to bridge socket, proxies tool calls. Token determines visible MCPs/tools.
- `relay mcp register|unregister|list` -- CLI for external MCP server management.
- `relay service register|unregister|restart|list` -- CLI for service self-registration. `restart` sends a `ReloadService` bridge message; the tray does Stop → Start in place.

## Architecture

Relay is the container: it owns projects, MCPs, services, and the user-facing front door. Specific service knowledge lives in services, not in relay. Each enhanced service (relayLLM, relayScheduler, etc.) declares a manifest describing the routes it serves; relay's front-door dispatcher routes inbound traffic accordingly. See `plans/service-manifest-spec.md` for the protocol.

Request flow: Browser → Eve → relay frontend socket → dispatcher → matching enhanced service. When an LLM in that service calls a tool: service's MCP client → `relay mcp` subprocess (project token) → bridge socket → router (auth, filter, _meta inject) → actual MCP server (fsMCP, macMCP, etc.) → result back up the chain.

### Key files

```
trayapp.go             App lifecycle, menu, settings IPC, ToolRouter wiring
settings.go            Config, project CRUD, permission derivation, SyncProjectToken
project.go             CreateProjectWithToken, token generation
tokens.go              hashToken, auth sentinel errors
types.go               StoredToken, Project, ChatTemplate, ExternalMcp, ServiceConfig
router.go              Auth (service → project tokens), tool filtering, _meta injection,
                       appRouter.RegisterManifest bridges to the enhanced-services registry
external_mcp.go        mcpConnection interface + stdio/HTTP MCP clients, runtime schema storage
settings_html.go       Settings WKWebView HTML/JS
mcp_cmd.go             CLI: relay mcp register/unregister/list
service_cmd.go         CLI: relay service register/unregister/restart/list
service_registry.go    Background process management, ephemeral service tokens.
                       Injects RELAY_BRIDGE_SOCKET + RELAY_SERVICE_ID into every spawn.
service_pidfile.go     Pidfile read/write under run/. Enables orphan reclaim on
                       next launch when the tray is SIGKILLed.
relay_llm_channel.go   Frontend channel: provisions the front-door Unix socket + bearer
                       token that Eve and other frontend consumers dial. (Name retained
                       for branch hygiene; rename to frontend_channel.go pending.)
enhanced_services.go   In-memory registry of relay-enhanced services. Bridge handler
                       writes on RegisterManifest; service_registry.Forget on exit;
                       front-door dispatcher reads via LookupByPath. Caches a per-
                       service *httputil.ReverseProxy with a pooling Transport.
service_status_client.go   Generic per-service HTTP-over-Unix-socket client. Used by
                           the Service Inspector for status polling + action dispatch.
                           Holds zero service-specific knowledge.
service_status_poller.go   Per-tick fan-out: polls every registered service's manifest-
                           declared status endpoint, emits the batch (one snapshot per
                           service) to the settings WebView via onServiceStatusBatch.
ipc_service_action.go      Generic action dispatcher. UI sends {serviceId, actionId,
                           row}; relay looks the action up in that service's manifest
                           (whitelist), substitutes row keys into pathTemplate with
                           URL-escaping, and dispatches via the status client. Refuses
                           anything not declared in the manifest.
frontend_server.go     Frontend HTTP server on the Unix socket. Project routes are
                       wired locally; everything else falls through to the dispatcher.
frontend_dispatcher.go Manifest-driven HTTP + WS dispatcher (one handler for both).
bridge/                Unix socket IPC. Newline-delimited JSON. Includes the
                       RegisterManifest request type and Manifest/Status/Action types.
mcp/                   MCP types + stdio server (proxies to bridge)
plans/service-manifest-spec.md  Service manifest protocol design.
```

## Projects

Projects are the primary infrastructure boundary in `settings.json`. Each binds: path, allowed MCPs, allowed models, chat templates, scoped token, disabled tools, and context.

- `allowed_mcp_ids: ["*"]` = all registered MCPs. Explicit IDs to restrict.
- `allowed_models: ["*"]` = all models. Explicit IDs to restrict.
- Permissions derived at auth time from `allowed_mcp_ids` — not stored separately.
- `fs_bash` auto-disabled for filesystem MCPs. `allowed_dirs` auto-set to project path.

Auth flow: `AuthenticateProject(plaintext)` → find project by token hash → derive permissions from `allowed_mcp_ids` + registered MCPs → return `StoredToken` view with permissions + disabled_tools + context.

## Service Manifest (Enhanced Services)

Every spawned service receives `RELAY_BRIDGE_SOCKET` + `RELAY_SERVICE_ID` env vars. Services that implement the manifest protocol dial the bridge with a `RegisterManifest` payload declaring (a) the routes they serve, (b) their internal Unix socket + bearer token, and (c) optional status endpoint / actions. Generic services ignore the env vars; relay never dispatches to them.

The dispatcher (`frontend_dispatcher.go`) does longest-prefix-match on registered routes; matches forward to the service's internal socket using its declared bearer token. WS upgrades are handled by the same dispatcher.

The protocol is intentionally minimal — no `version`, no capability declarations, no service-ID hardcoding anywhere in relay. The full design and migration plan live in `plans/service-manifest-spec.md`.

## Security

- **Project tokens** -- inline in project (plaintext + SHA-256 hash). The token IS the security boundary.
- **Service tokens** -- ephemeral, in-memory. Full bridge access. For administrative operations.
- **Frontend channel** -- Eve and other frontend consumers dial `RELAY_FRONTEND_SOCKET` with `RELAY_FRONTEND_TOKEN` (Unix socket, 0600 perms). Bearer-checked on every HTTP + WS request before dispatch.
- **Enhanced internal sockets** -- each enhanced service picks its own internal socket + token and tells relay both via `RegisterManifest`. Relay strips inbound Authorization and injects the service-declared token when proxying. Distinct from frontend creds.
- **OAuth 2.1** -- HTTP MCPs use PKCE, dynamic registration, auto-refresh. See `oauth.go`.

## MCP Runtime Data

`discovered_tools` and `context_schema` are `json:"-"` — runtime-only, held in `ExternalMcpManager`, never persisted. Settings UI queries the live manager.

## Settings UI

IPC: `ipc(json)` → `window.webkit.messageHandlers.ipc.postMessage` (macOS).
Tabs: Services, MCP Servers, Projects.

## Ecosystem

Services (managed via `relay service register`). First-party services are reference implementations of the manifest protocol — no privileged path in relay:

- `../relayLLM/` -- LLM execution engine. Registers manifest covering `/api/sessions/*`, `/api/terminals/*`, `/api/models`, `/api/permission`, `/api/llama/*`, `/api/status`, `/api/generated/*`, `/ws`.
- `../eve/` -- Browser-based frontend. Dials relay's frontend socket; relay dispatches to whatever backend serves each path.
- `../relayScheduler/` -- Task scheduler. Registers its own manifest covering `/api/tasks/*`; dispatched directly (no longer proxied through relayLLM).
- `../relayTelegram/` -- Telegram bot bridge.

MCP Servers (managed via `relay mcp register`):

- `../macMCP/` -- Swift, 41 macOS-native tools.
- `../fsMCP/` -- TypeScript, 6 file system tools. Uses `_meta.allowed_dirs` for directory scoping.

## Build

```bash
./build.sh   # builds relay -> /Applications/Relay.app
```

Requires: Go 1.22+, macOS.
