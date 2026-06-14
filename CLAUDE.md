# Relay (Go)

macOS MCP orchestrator and project manager. Tray app with project-scoped auth, a
Unix-socket bridge, an external-MCP proxy (stdio + HTTP/OAuth), and background
service management.

## Modes

- `relay` — tray app (default). Hosts the bridge socket, manages services and projects, shows the settings UI.
- `relay mcp --token TOKEN` — stdio MCP server. Connects to the bridge; the token determines visible MCPs/tools.
- `relay mcp call --token TOKEN --list | --tool NAME [--args '<json>']` — one-shot list/invoke over the bridge (also spelled `relay mcpExec`). No long-lived session; handy for agents.
- `relay mcp register|unregister|list` — external MCP management.
- `relay service register|unregister|restart|list` — service self-registration. `restart` sends `ReloadService`; the tray does Stop → Start in place.

## Architecture

Relay is the container: it owns projects, MCPs, services, and the user-facing
front door. Service-specific knowledge lives in services, not in relay. Each
enhanced service (relayLLM, relayScheduler, …) declares a manifest describing the
routes it serves; relay's front-door dispatcher routes inbound traffic
accordingly. Protocol: [`docs/service-manifest.md`](docs/service-manifest.md).

Request flow: Browser → Eve → relay frontend socket → dispatcher → matching
enhanced service. When an LLM in that service calls a tool: service's MCP client
→ `relay mcp` subprocess (project token) → bridge socket → router (auth, filter,
`_meta` inject) → actual MCP server (fsMCP, macMCP, …) → result back up the chain.

### Key files

```
main.go                  Entry + command dispatch (relay / mcp / mcpExec / service)
trayapp.go               App lifecycle, menu, settings IPC, ToolRouter wiring
settings.go              Config, project CRUD, permission derivation
settings_store.go        Atomic settings.json read/write
types.go                 Project, StoredToken, ExternalMcp, ServiceConfig (Settings lives in settings.go)
tokens.go                hashToken, auth sentinel errors
project.go               Project + token creation
project_routes.go        HTTP project routes; shares Settings mutators with ipc_projects.go
project_dto.go           projectView DTO — strips the token from every response except rotate
router.go                Bridge auth (service vs project tokens), tool filtering, _meta injection
external_mcp.go          stdio/HTTP MCP clients + runtime schema storage (McpConnection iface)
http_mcp.go, oauth.go    HTTP transport + OAuth 2.1 (PKCE, dynamic registration, refresh)
mcp_cmd.go, exec_cmd.go, service_cmd.go   CLI subcommands
frontend_server.go       Front-door HTTP server; project routes local, rest falls through
frontend_dispatcher.go   Manifest-driven HTTP + WS dispatcher (longest-prefix match)
frontend_model_guard.go  Enforces a project's allowed_models before relayLLM sees the request
relay_llm_channel.go     Provisions the frontend socket + bearer token (filename legacy; contents are the generic FrontendChannel)
enhanced_services.go     In-memory registry of enhanced services; per-service reverse proxy
service_registry.go      Background process management + ephemeral service tokens
service_pidfile.go       Pidfiles under run/; enables orphan reclaim after a force-quit
service_status_client.go, service_status_poller.go   Generic per-service status polling + action dispatch
ipc_*.go                 Settings-UI IPC handlers (projects, services, mcps, service action/config)
service_config_file.go   resolveConfigPath security gate for the manifest config editor
settings_html.go         Settings WKWebView HTML/JS
bridge/                  Unix-socket IPC (newline-delimited JSON); manifest.go holds Manifest/FieldDecl
mcp/                     MCP types + stdio server (proxies to the bridge)
```

## Projects

Projects are the primary infrastructure boundary in `settings.json`. Each binds:
path, allowed MCPs, allowed models, chat templates, scoped token, disabled tools,
and context.

- `allowed_mcp_ids: ["*"]` = all registered MCPs; explicit IDs to restrict.
- `allowed_models: ["*"]` = all models; explicit IDs to restrict.
- Permissions are derived at auth time from `allowed_mcp_ids` — not stored separately.
- `fs_bash` auto-disabled for filesystem MCPs; `allowed_dirs` auto-set to the project path.

Auth flow: `AuthenticateProject(plaintext)` → find project by token hash → derive
permissions from `allowed_mcp_ids` + registered MCPs → return a `StoredToken`
view with permissions + disabled_tools + context.

## Service manifest (enhanced services)

Every spawned service gets `RELAY_BRIDGE_SOCKET` + `RELAY_SERVICE_ID`. Services
that implement the protocol dial the bridge with a `RegisterManifest` payload
declaring (a) the routes they serve, (b) their internal Unix socket + bearer
token, and (c) optional status endpoint, actions, and config editor. Generic
services ignore the env vars; relay never dispatches to them.

The dispatcher does longest-prefix match on registered routes and proxies to the
service's internal socket using its declared token; WS upgrades share the same
handler. The protocol is intentionally minimal — no version, no capability
declarations, no service-ID hardcoding anywhere in relay. Full spec:
[`docs/service-manifest.md`](docs/service-manifest.md).

## Security

The four-credential model (full inventory: [`docs/tokens.md`](docs/tokens.md);
brokering rationale: ADR-007):

- **Project token** (`RELAY_PROJECT_TOKEN`) — the security boundary, scoped to a project's allowed MCPs/tools. Plaintext + SHA-256 hash inline in the project. **Relay is the sole broker:** Eve references projects by id only (the DTO strips the token from every response except rotate); relayLLM resolves the token just-in-time from the bridge by `projectId`, injects it into spawned children, and never stores it or accepts it from Eve.
- **Service token** (`RELAY_SERVICE_TOKEN`) — ephemeral, in-memory, full bridge access; lets a service authenticate its own bridge calls. **Never injected into a spawned child** — if a project token can't be resolved, the child gets no token (fail closed).
- **Frontend token** (`RELAY_FRONTEND_TOKEN`) — frontend consumers dial `RELAY_FRONTEND_SOCKET` (0600), bearer-checked on every HTTP + WS before dispatch; an empty configured token fails closed. Injected only into frontend consumers (`service register --no-frontend-creds` keeps it out of backends).
- **Enhanced internal bearer** — each service picks its own internal socket + token and declares both via the manifest; relay strips inbound `Authorization` and injects the service-declared token when proxying.

**TCC permissions** — relay holds the personal-information entitlements
(`Relay.entitlements`) and fires the prompts from its own process; MCPs declare
what they need with `--tcc-services foo,bar` and inherit relay's grants via TCC's
responsible-parent attribution at runtime. Rationale + checklist for adding a TCC
service: ADR-005.

## Settings UI

IPC: `ipc(json)` → `window.webkit.messageHandlers.ipc.postMessage`. Tabs:
Services, MCP Servers, Projects, Service Inspector.

The Projects tab is native and co-equal with Eve's project dialog — both hit the
same `Settings.*Project*` mutators (relay via `ipc_projects.go`, Eve via
`project_routes.go`), so HTTP and IPC paths are interchangeable. Cross-process
changes propagate live: an HTTP project mutation fires `onProjectsChanged`, which
re-renders an open Settings window. See ADR-004.

## Ecosystem

First-party services are reference implementations of the manifest protocol — no
privileged path in relay.

- `../relayLLM/` — LLM execution engine. Its manifest (see relayLLM's `manifest.go`) covers sessions, terminals, models, permission, status, generated assets, local-model (llama/mlx) management, and `/ws`.
- `../eve/` — browser frontend; dials relay's frontend socket.
- `../relayScheduler/` — task scheduler; registers `/api/tasks/*`, dispatched directly.
- `../relayTelegram/` — Telegram bot bridge.
- `../macMCP/` — Swift, macOS-native tools.
- `../fsMCP/` — TypeScript file system tools; uses `_meta.allowed_dirs` for scoping.

## Build

```bash
./build.sh              # build + install /Applications/Relay.app and launch it
./build.sh --test       # run the hermetic suite first; abort install on failure
./build.sh --release    # sign + notarize + emit /tmp/Relay.dmg (Developer ID required)
```

`--test` and `--release` may be combined; `--release` implies `--test`. Requires
a recent Go toolchain (see `go.mod`) and macOS.

## Testing

**Headline rule:** no test may read or mutate the real user config directory
(`~/Library/Application Support/relay/`). Tests route through
`mkSandboxRelayHome(t)` (in `support_test.go`), which redirects
`bridge.ConfigDir()` to a per-test temp dir under `/tmp` (via `mkShortTempDir`,
which sidesteps the 104-char Unix-socket path limit) populated from
`test/fixtures/relay-home/`. The `support_safety_test.go` guard fails the suite
if anything in the real ConfigDir changes during a run.

### Three tiers

| Command | What runs | When |
|---|---|---|
| `go test ./...` | Hermetic suite — pure Go, no spawned binaries, no user files | Every commit (pre-commit hook) |
| `go test -tags=live ./...` | Spawns the real `../relayLLM` binary end-to-end | After relay↔relayLLM boundary changes |
| `go test -race ./...` | Hermetic suite + race detector | Pre-push hook; before merging concurrency changes |

Install the hooks once per clone: `git config core.hooksPath .githooks`.

### Adding a test

1. Pick the tier (ADR-001). ~95% belong in the default hermetic tier.
2. Reading/writing settings, pidfiles, logs, or the bridge socket → call `mkSandboxRelayHome(t)` first.
3. Need a working router → `newTestRouter(t, settings, mgr)`.
4. Exercising a manifest-registering service → `NewFakeService(t, FakeServiceOptions{...})`. The relayLLM contract is covered by `integration_fake_relayllm_test.go`.
5. Need a real spawned subprocess → the `cmd/testservice` / `cmd/testmcp` binaries, built on demand via `buildTestServiceBinary(t)` / `buildTestMcpBinary(t)`, never an `exec.Command` mock.
6. Live-tier tests carry `//go:build live` and `t.Skip` gracefully if `../relayLLM` isn't built.

### Not covered by the suite

- Cocoa tray UI (menu, dock) — exercise via `scripts/demo.sh`.
- Real `launchd` integration — `service_registry` is tested against `cmd/testservice`.
- Live OAuth round-trips — `oauth_test.go` covers PKCE/dynamic registration in isolation.
- Notarization / code-signing — exercised by `./build.sh --release`.

ADRs: see [`docs/decisions/`](docs/decisions/). Cross-repo test status:
[`docs/testing-roadmap.md`](docs/testing-roadmap.md).
