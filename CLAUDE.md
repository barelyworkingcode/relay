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
ipc_projects.go            Projects-tab IPC handlers: create/update/remove project,
                           rotate_project_token, regen_project_skill, update_project_
                           disabled_tools, list_mcp_tools. Shares all Settings mutators
                           with project_routes.go so HTTP (Eve) and IPC (tray UI) paths
                           are interchangeable.
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
Tabs: Services, MCP Servers, Projects, Service Inspector.

### Projects tab

Native, co-equal with Eve's project dialog — both hit the same `Settings.*Project*` mutators (relay via `ipc_projects.go`, Eve via `project_routes.go`). The tri-state per-MCP picker exposes `DisabledTools`; the token panel surfaces rotate-now (via `RotateProjectToken`); the Skill section surfaces `GenerateSkill` plus a manual `EmitSkill` trigger. Cross-process changes propagate live: HTTP project mutations fire the `onProjectsChanged` callback (wired through `NewFrontendServer`) which calls `pushFullProjects()` so an open Settings window re-renders.

See `docs/decisions/004-project-mgmt-in-relay.md`.

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
./build.sh              # default: builds + installs to /Applications/Relay.app and launches it
./build.sh --test       # runs the hermetic test suite first; aborts install if anything fails
./build.sh --release    # signs + notarizes + emits /tmp/Relay.dmg (Developer ID required)
```

`--test` and `--release` may be combined; `--release` implies `--test`.

Requires: Go 1.22+, macOS.

## Testing

**Headline rule:** No test may read or mutate the real user config directory (`~/Library/Application Support/relay/`). Tests must always route through `mkSandboxRelayHome(t)` (defined in `support_test.go`), which redirects `bridge.ConfigDir()` to a per-test `t.TempDir()` populated from `test/fixtures/relay-home/`. The `support_safety_test.go` guard fails the suite if anything in the real ConfigDir is modified during a test run.

### Three tiers

| Command | What runs | When |
|---|---|---|
| `go test ./...` | Hermetic suite — pure Go, no spawned binaries, no user files | Every commit (pre-commit hook) |
| `go test -tags=live ./...` | Spawns the real `../relayLLM` binary and exercises end-to-end | Manually after relay↔relayLLM boundary changes |
| `go test -race ./...` | Hermetic suite + race detector | Weekly, or before merging concurrency changes |

### Install the pre-commit hook (one-time per clone)

```bash
git config core.hooksPath .githooks
```

The hook runs `go build ./... && go vet ./... && go test ./...` on any commit that touches `*.go`, `go.mod`, or `go.sum`. Skip with `git commit --no-verify` in emergencies only.

### Adding a test

1. Decide the tier (see ADR-001 — `docs/decisions/001-testing-strategy.md`). 95% of tests belong in the default hermetic tier.
2. If your test reads or writes settings, pidfiles, logs, or the bridge socket: call `mkSandboxRelayHome(t)` first.
3. If your test needs a working router + bridge + frontend: call `newTestRouter(t)`.
4. If your test exercises a service that registers a manifest: use `FakeService(t, manifest)` (or `FakeRelayLLMService(t)` for the relayLLM contract).
5. If your test needs a real spawned subprocess: use the `cmd/testservice` binary (built automatically by `TestMain`), never an `exec.Command` mock.
6. Live-tier tests get `//go:build live` and `t.Skip` gracefully if `../relayLLM` isn't built.

### Demo & screenshot harness

`scripts/demo.sh` launches relay against a writable copy of `test/fixtures/relay-home/` in `/tmp`, so screenshots and screencasts are reproducible and never expose real data. See the script's `--help` for `--reset` and `--scenario <name>` options. The same fixture tree backs both the test suite and the demo harness — see ADR-003 for content rules (no PII, no real tokens, no machine paths).

### What's NOT tested

- Cocoa tray UI (`platform_darwin.go`, menu rendering, dock interactions) — exercise via `scripts/demo.sh`.
- Real `launchd` integration — services spawned by `service_registry` are tested with the in-tree `cmd/testservice` binary instead.
- OAuth 2.1 callbacks against real providers — the `oauth_test.go` suite covers PKCE/dynamic registration in isolation; live OAuth round-trips are manual.
- Notarization / code-signing — exercised by `./build.sh --release`, not in the test suite.

### Reference docs

- `docs/decisions/001-testing-strategy.md` — three-tier model, sandbox rule
- `docs/decisions/002-test-seams.md` — which production seams exist and why
- `docs/decisions/003-fixture-layout.md` — fixture tree, content rules
- `docs/testing-roadmap.md` — next services to bring up to this standard (eve, fsMCP, macMCP)
