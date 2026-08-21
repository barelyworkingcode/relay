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
- `relay audit [--tail N] [--project ID] [--outcome denied] [--grep TEXT] [--json]` — tail the tool-call audit log. Reads the file directly, so it works with the tray stopped.
- `relay enrol create --client-id ID --grant PROJECT_ID [--grant ...] | list | revoke --client-id ID` — remote-client enrolment. Signs a client certificate off relay's own CA and emits a bundle to copy to the client machine. Host-side operator act only: no self-service enrolment, no bootstrap token.

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
audit.go                 Tool-call audit log: event model, async writer, ring, redaction, query
audit_call.go            Nil-safe per-call event builder used by the router instrumentation
audit_cmd.go             `relay audit` CLI
enrolment.go             Enrolment CRUD, grant validation, revocation + its live-connection hook
enrolment_ca.go          Relay's self-signed CA: lazy generation, client/server cert issuance, fingerprints
enrol_cmd.go             `relay enrol` CLI
remote_server.go         Remote mTLS listener: two-entry dispatch table, cert→enrolment→grant, revocation hook
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
ipc_*.go                 Settings-UI IPC handlers (projects, services, mcps, service action/config, audit)
service_config_file.go   resolveConfigPath security gate for the manifest config editor
settings_html.go         Settings WKWebView HTML/JS
bridge/                  Unix-socket IPC (newline-delimited JSON); manifest.go holds Manifest/FieldDecl.
                         frameconn.go is the framing/scanner/deadline plumbing BOTH listeners share;
                         remote_request.go + remote_caller.go are the remote wire type and attested identity
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

`allow_cwd_auth` (default false, per project) opts into a token-less fallback:
a caller with no token whose working directory is inside the project path
authenticates as that project via `AuthenticateProjectByPath`, with identical
scope. A present-but-invalid token never falls back. See
[`docs/tokens.md`](docs/tokens.md#directory-auth-allow_cwd_auth).

### Project kind: local vs. remote

`Project.Kind` (`ProjectKindLocal` / `ProjectKindRemote`) distinguishes a
host-directory project from a pure capability grant to a client on another
machine — e.g. an LLM agent running on a separate VM that reaches relay for
tool access instead of running on the host. Always test `proj.IsRemote()`,
never compare `Kind` against `ProjectKindLocal`: the zero value is local, so
every project written before this field existed round-trips unaffected, and
an equality check invites a future bug where an unset field reads as remote.

A remote project has no `Path` and cannot have anything that presumes a host
directory — `AllowCwdAuth`, `GenerateSkill`, `ShellTemplates`, and the
`allowed_mcp_ids: ["*"]` wildcard are all refused by `validateProjectShape`
(`project.go`), as is a non-empty `allowed_models` (an empty allowlist is the
only value `modelAllowedForProject` won't misread as "unrestricted"). A
filesystem-scoped MCP (schema declares `allowed_dirs`) can't be granted to a
remote project either (`ValidateProjectGrants`) — and `SyncProjectToken`
independently refuses to derive `allowed_dirs` for one regardless, since an
MCP's schema is discovered at runtime and could gain that field after a
grant was already validated. Sessions (`refuseRemoteSession`) and PTY
launches (`refuseRemotePty`) are refused at the point of use too, not just
at validation — see ADR-009 for why each of these is defended twice rather
than once.

See [ADR-009](docs/decisions/009-remote-projects.md) for the full reasoning.

### Remote client enrolment

An **enrolment** (`enrolment.go`, `settings.json` → `enrolments`) binds one
client certificate to the remote projects it may use. It is keyed by
*certificate, not by machine* — several agents on one VM each hold their own
enrolment, granted, audited, and revoked independently, and nothing may assume
one per machine. There is **no bearer token anywhere on this path**: a stolen
`settings.json` grants no remote access at all.

Relay is its own CA (`enrolment_ca.go`), generated lazily on first use and
persisted as `ca.key` / `ca.crt` (0600) in the config dir — not in
`settings.json`, which is rewritten in full on every mutation. Client certs are
long-lived because *revocation, not expiry, is the control*; revoking deletes
the record and fires `SetEnrolmentRevocationHook` so the listener can close
live connections.

Grants are validated at enrolment (`ValidateEnrolmentGrants` — every grant must
name a project with `IsRemote()` true) and at conversion
(`ValidateProjectEnrolments` — remote→local is refused while any enrolment
grants the project, naming the offenders), and a third time at call time by the
listener (`RemoteServer.resolveGrant` re-checks `IsRemote()` immediately before
dispatch, so a grant that went stale by any route relay did not anticipate
fails closed).

### The remote listener

`RemoteServer` (`remote_server.go`) is a **second listener beside**
`BridgeServer` — never a mode of it. Its dispatch table (`remoteHandlers`) has
exactly two entries, `ListTools` and `CallTool`: the other eight bridge request
types have no code path from a remote connection at all, so a new admin op is
unreachable from a VM until someone deliberately adds it to a list that is
visibly a security boundary.

Mutual TLS against relay's own CA (`tls.RequireAndVerifyClientCert`). The peer
certificate is fingerprinted and resolved to an enrolment **before any request
is read** — an unenrolled certificate is closed without processing, so it
cannot probe. The resolved identity goes into the context via
`bridge.WithRemoteCaller`, which is what puts every call on the fail-closed
audit path. The wire type (`bridge.RemoteRequest`) carries only
`type` / `name` / `arguments` / `project_id`; there is no token and no cwd, and
decoding is strict (`DisallowUnknownFields`) so a client sending `cwd` gets a
loud error rather than silent divergence. The project token is resolved
host-side from the granted project and never appears on the wire.

Every remote call is budgeted (`enrolment_budget.go`). Each enrolment carries a
rolling-window call-rate and result-volume cap, enforced in `appRouter.CallTool`
and refused with the `throttled` outcome — distinct from `denied` (a tool the
grant never included) and `tool_error` (a boundary inside the MCP) because it is
the only one of the three that says the grant was legitimate and the *pattern of
use* was not. Budgets live on the **enrolment, not the project**: the enrolment
is the unit of compromise, so it is the unit that bounds one. Rate is checked
before the MCP runs; volume is necessarily charged after a call returns, so the
guarantee is "at most one call's worth over the cap", not a hard ceiling. Local
callers are not budgeted at all — one context lookup and nothing else.

Auditing is a hard dependency of remote access: with `audit.enabled: false` the
listener refuses to start rather than serving unrecorded calls. The case for
letting a VM reach host mail rests on detection, so there is deliberately no
window in which a remote call runs without a record.

Config — absent block means **no listener at all**, and the default binds
loopback so misconfiguration cannot expose the control plane to a LAN:

```json
"remote": { "enabled": true, "listen": "127.0.0.1:9910" }
```

The listener **refuses to start when auditing is disabled**: a remote grant is
justified by the calls it records, so serving remote traffic unrecorded is not
a degraded mode. Local tooling is unaffected. It also sets read+write deadlines
(inactivity, not a cap on work) and keeps a connection table keyed by
fingerprint so `SetEnrolmentRevocationHook` closes a revoked client's *live*
connections.

See [ADR-010](docs/decisions/010-remote-client-transport-and-identity.md).

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

**Tool-call audit log** — every call, denial, and auth failure is recorded at
`appRouter.CallTool`, the single chokepoint every transport funnels through.
Attribution comes from relay's own auth resolution (project id) and the kernel
(peer pid off the bridge socket), never from the caller. Arguments are redacted
and capped; results are metadata-only unless explicitly opted in. For a local
caller the sink fails open and shows its drop count rather than stalling a tool
call; for a remote one it is fail-closed — an `intent` record is written and
flushed before the MCP runs, a `completion` record with the same `id` follows,
and a call whose intent cannot be recorded is refused (ADR-010 decision 5).
Viewer: Settings → Tool Calls, or `relay audit` (`--kind remote` for anything a
VM did). Full reference: [`docs/audit-log.md`](docs/audit-log.md); rationale:
ADR-008, narrowed for remote callers by ADR-010.

**TCC permissions** — relay holds the personal-information entitlements
(`Relay.entitlements`) and fires the prompts from its own process; MCPs declare
what they need with `--tcc-services foo,bar` and inherit relay's grants via TCC's
responsible-parent attribution at runtime. Rationale + checklist for adding a TCC
service: ADR-005.

## Settings UI

IPC: `ipc(json)` → `window.webkit.messageHandlers.ipc.postMessage`. Tabs:
Services, MCP Servers, Projects, Service Inspector, Tool Calls.

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
