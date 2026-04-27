# Relay (Go)

macOS MCP orchestrator and project manager. Tray app with project-scoped auth, Unix socket bridge, external MCP proxy (stdio and HTTP transports), and background service management.

## Modes

- `relay` -- tray app (default). Hosts bridge socket, manages services and projects, shows settings UI.
- `relay mcp --token TOKEN` -- stdio MCP server. Connects to bridge socket, proxies tool calls. Token determines visible MCPs/tools.
- `relay mcp register|unregister|list` -- CLI for external MCP server management.
- `relay service register|unregister|list` -- CLI for service self-registration.

## Architecture

Relay owns projects, MCPs, and services. relayLLM is a pure LLM execution engine — it receives directory + model + token from callers and has no project awareness. Eve fetches projects from relay, resolves templates, and passes explicit params to relayLLM.

Request flow: Browser → Eve (WS) → relayLLM → LLM API. When the LLM calls a tool: relayLLM's MCP manager → `relay mcp` subprocess (project token) → bridge socket → router (auth, filter, _meta inject) → actual MCP server (fsMCP, macMCP, etc.) → result back up the chain.

### Key files

```
trayapp.go          App lifecycle, menu, settings IPC, ToolRouter
settings.go         Config, project CRUD, permission derivation, SyncProjectToken
project.go          CreateProjectWithToken, token generation
tokens.go           hashToken, auth sentinel errors
types.go            StoredToken, Project, ChatTemplate, ExternalMcp, ServiceConfig
router.go           Auth (service → project tokens), tool filtering, _meta injection
external_mcp.go     mcpConnection interface + stdio/HTTP MCP clients, runtime schema storage
settings_html.go    Settings WKWebView HTML/JS
mcp_cmd.go          CLI: relay mcp register/unregister/list
service_cmd.go      CLI: relay service register/unregister/list
service_registry.go Background process management, ephemeral service tokens
bridge/             Unix socket IPC. Newline-delimited JSON.
mcp/                MCP types + stdio server (proxies to bridge)
```

## Projects

Projects are the primary infrastructure boundary in `settings.json`. Each binds: path, allowed MCPs, allowed models, chat templates, scoped token, disabled tools, and context.

- `allowed_mcp_ids: ["*"]` = all registered MCPs. Explicit IDs to restrict.
- `allowed_models: ["*"]` = all models. Explicit IDs to restrict.
- Permissions derived at auth time from `allowed_mcp_ids` — not stored separately.
- `fs_bash` auto-disabled for filesystem MCPs. `allowed_dirs` auto-set to project path.

Auth flow: `AuthenticateProject(plaintext)` → find project by token hash → derive permissions from `allowed_mcp_ids` + registered MCPs → return `StoredToken` view with permissions + disabled_tools + context.

## Security

- **Project tokens** -- inline in project (plaintext + SHA-256 hash). The token IS the security boundary.
- **Service tokens** -- ephemeral, in-memory. Full bridge access. For administrative operations.
- **Eve ↔ relayLLM** -- separate trust boundary (`RELAY_LLM_TOKEN` + Unix socket). See `relay_llm_channel.go`.
- **OAuth 2.1** -- HTTP MCPs use PKCE, dynamic registration, auto-refresh. See `oauth.go`.

## MCP Runtime Data

`discovered_tools` and `context_schema` are `json:"-"` — runtime-only, held in `ExternalMcpManager`, never persisted. Settings UI queries the live manager.

## Settings UI

IPC: `ipc(json)` → `window.webkit.messageHandlers.ipc.postMessage` (macOS).
Tabs: Services, MCP Servers, Projects.

## Ecosystem

Services (managed via `relay service register`):

- `../relayLLM/` -- LLM execution engine. Receives directory + model + token. No project awareness.
- `../eve/` -- Browser-based frontend. Fetches projects from relay, resolves templates.
- `../relayScheduler/` -- Task scheduler. Runs LLM prompts on schedule.
- `../relayTelegram/` -- Telegram bot bridge.

MCP Servers (managed via `relay mcp register`):

- `../macMCP/` -- Swift, 41 macOS-native tools.
- `../fsMCP/` -- TypeScript, 6 file system tools. Uses `_meta.allowed_dirs` for directory scoping.

## Build

```bash
./build.sh   # builds relay -> /Applications/Relay.app
```

Requires: Go 1.22+, macOS.
