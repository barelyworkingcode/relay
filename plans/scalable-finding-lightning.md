# Front-Door Migration: Relay as the HTTP API Gateway

## Context

The project-scoping work (below) made relay the owner of projects, MCPs, and security. But Eve still talks to relayLLM as its only backend, with relayLLM proxying project queries back to relay's bridge. That's an awkward two-hop flow that puts project knowledge into relayLLM, which we explicitly designed to be a pure execution engine. relayScheduler also calls relayLLM directly.

This refactor flips the topology: **relay becomes the front-door HTTP API server.** Eve and relayScheduler talk only to relay. relay handles project endpoints directly and reverse-proxies session/terminal/permission/model traffic to relayLLM, which becomes reachable only through relay over an internal Unix socket.

After this change:
- Eve has one backend (relay) instead of two.
- relayLLM has zero project awareness.
- The architecture matches the mental model: relay owns projects/MCPs/security; relayLLM is a pure agentic engine.

The agentic engine itself (session manager, providers, MCP integration, permission management, WS hub) **stays in relayLLM** for this phase. Moving it to relay is a separate, deliberate refactor.

## Architecture After Migration

```
Browser ─WebAuthn→ Eve ─HTTPS/Unix→ relay (HTTP/WS) ─Unix→ relayLLM (internal)
                                       │
                                       ├─direct: /api/projects, /api/projects/{id}
                                       ├─proxy:  /api/sessions/*, /api/terminal*, /api/models, /api/tasks/*, /api/permission, /api/generated/*
                                       └─proxy:  /ws (WebSocket termination + bidi forwarding)

relayScheduler ─Unix→ relay ─Unix→ relayLLM (same path as Eve)
relayTelegram ─HTTPS→ Eve (no change; Eve already routes through relay)
relayLLM ─Unix→ bridge socket (relay.sock) for service-token operations and `relay mcp` subprocesses
```

## Sockets and Tokens

Three Unix sockets (mode 0600), one bridge:

| Socket | Bound by | Used by | Auth |
|--------|----------|---------|------|
| `relay.sock` | relay (bridge) | `relay mcp` subprocesses, relay's own clients | service tokens / project tokens (existing) |
| `relay-frontend-{pid}.sock` (new) | relay | Eve, relayScheduler | `RELAY_FRONTEND_TOKEN` (renamed from `RELAY_LLM_TOKEN`) |
| `relay-llm-{pid}.sock` | relayLLM | relay only | filesystem permissions; optional `RELAY_LLM_INTERNAL_TOKEN` for defense in depth |

Token model:
- **`RELAY_FRONTEND_TOKEN`** — bearer token for Eve/scheduler → relay. relay generates and injects at spawn. Same lifecycle as the current `RELAY_LLM_TOKEN`. Rename for clarity.
- **`RELAY_LLM_INTERNAL_TOKEN`** — bearer token for relay → relayLLM internal calls. Defense in depth on top of socket permissions.
- **`RELAY_MCP_TOKEN`** — service token for the bridge (existing, unchanged). relayLLM still uses this to spawn `relay mcp` subprocesses.

## What Each Repo Does

### relay (Go)

**New files:**
- `frontend_server.go` — HTTP server on the frontend socket; mux registration; auth middleware.
- `frontend_proxy.go` — `httputil.ReverseProxy` for HTTP, manual gorilla/websocket bidi forwarding for `/ws`.
- `project_routes.go` — Direct handlers for `GET /api/projects` and `GET /api/projects/{id}`. Reads from settings; converts internal snake_case to the camelCase shape Eve already expects (so Eve's normalizer can simplify or stay).

**Modified:**
- `relay_llm_channel.go` — Provisions both sockets: frontend (relay binds) and internal (relayLLM binds). Renames env vars: `RELAY_LLM_SOCKET` → `RELAY_FRONTEND_SOCKET` (Eve-facing) plus a new `RELAY_LLM_INTERNAL_SOCKET` (relay→relayLLM). Same for the token rename.
- `service_registry.go` — Inject the new env vars when spawning Eve, scheduler, and relayLLM. Service-ID matcher in `participatesInLLMChannel()` still applies.
- `trayapp.go` — Construct the frontend server, wire its lifecycle to app shutdown. Pass the internal socket path to relayLLM via env.

**Dependencies:** Add `github.com/gorilla/websocket` to relay's `go.mod` for the WS proxy.

### relayLLM (Go)

**Removed:**
- `bridge_client.go` — no longer needs to talk to the bridge for projects.
- `RegisterProjectRoutes` block in `api.go` — `/api/projects` endpoints removed.
- TCP listening — relayLLM's `-port` flag and TCP server come out. Internal Unix socket only.

**Modified:**
- `main.go` — Bind the internal socket from `RELAY_LLM_INTERNAL_SOCKET`. Drop the public TCP listener and the `RELAY_LLM_PORT` flow. Drop bridge client construction. Optional: validate `RELAY_LLM_INTERNAL_TOKEN` on every request.
- `llama_proxy_server.go` (if present) — May still need a TCP listener for the OpenAI-compatible llama-server proxy, since that's a different surface (called by relayLLM's own provider code, not by external clients). Keep that as-is unless the user wants to consolidate.

### Eve (Node.js)

**Modified:**
- `relay-transport.js` — Read `RELAY_FRONTEND_SOCKET` instead of `RELAY_LLM_SOCKET`; same connection model. Token env var rename: `RELAY_FRONTEND_TOKEN`.
- `server.js` — `normalizeProject()` can stay; relay returns snake_case from its `settings.json` so the normalizer continues to do useful work. (Alternative: relay returns camelCase and we delete the normalizer. Recommend keeping snake_case in relay's HTTP API to match its on-disk format.)
- `assertStartupConfig()` — Updated env var names in the failure messages.
- Project session-creation flow already passes `mcpToken` and `directory` from the cached project — no further change needed.

### relayScheduler (Go)

**Modified:**
- `client.go` — Replace `RELAY_LLM_URL` with `RELAY_FRONTEND_SOCKET` (Unix socket dial via `http.Transport.DialContext`). Replace `RELAY_LLM_TOKEN` with `RELAY_FRONTEND_TOKEN`. Same HTTP paths (`POST /api/sessions`, `POST /api/sessions/{id}/message`, `POST /api/sessions/{id}/stop`) — those proxy through relay to relayLLM transparently.
- `main.go` — Update flag/env wiring.

### relayTelegram

No changes. It calls Eve, which already routes through the new architecture once Eve is updated.

### settings.json

No structural change. The orchestrator (relay) provisions sockets at runtime; nothing on disk needs to move.

## Reverse Proxy Implementation Notes

**HTTP proxy (relay → relayLLM):**

```go
proxy := &httputil.ReverseProxy{
    Director: func(r *http.Request) {
        r.URL.Scheme = "http"
        r.URL.Host  = "relayllm" // dummy; Transport overrides
        // Authorization header for internal token (if used) injected here
    },
    Transport: &http.Transport{
        DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
            return (&net.Dialer{}).DialContext(ctx, "unix", internalSock)
        },
    },
}
```

This handles streaming responses correctly out of the box (no buffering for chunked/SSE bodies).

**WebSocket proxy (relay → relayLLM):**

`httputil.ReverseProxy` does not handle WS. Use `gorilla/websocket`:

```go
upgrader := websocket.Upgrader{ /* CheckOrigin etc. */ }
clientConn, err := upgrader.Upgrade(w, r, nil)

upstreamHeader := http.Header{} // copy Authorization, etc.
upstreamConn, _, err := websocket.DefaultDialer.Dial("ws+unix://"+internalSock+"/ws", upstreamHeader)

// Bidi forwarding goroutines, close both on either side closing.
```

For Unix-socket WS dialing, set `websocket.Dialer.NetDial` to `net.Dial("unix", internalSock)`.

**Auth at relay's edge:**

Frontend token validated once on every HTTP/WS upgrade. After validation, relay forwards the request without revalidating downstream (relayLLM trusts requests from its internal socket).

## Critical Files to Modify

| File | Action |
|------|--------|
| `relay/frontend_server.go` | New |
| `relay/frontend_proxy.go` | New |
| `relay/project_routes.go` | New (HTTP wrappers around existing `findProjectByID` / settings reads) |
| `relay/relay_llm_channel.go` | Provisions two sockets and two env-var sets |
| `relay/service_registry.go` | Inject new env vars |
| `relay/trayapp.go` | Wire frontend server lifecycle |
| `relay/go.mod` | Add `github.com/gorilla/websocket` |
| `relayLLM/main.go` | Internal socket only; drop TCP and bridge client |
| `relayLLM/api.go` | Remove `RegisterProjectRoutes` |
| `relayLLM/bridge_client.go` | Delete |
| `eve/relay-transport.js` | Rename env vars; otherwise unchanged |
| `eve/server.js` | Update env var names; `normalizeProject` stays |
| `eve/auth.js` (and others using `RELAY_LLM_*`) | Rename references |
| `relayScheduler/client.go` | Switch to relay frontend socket + token |
| `relayScheduler/main.go` | Update flags/env |

## Existing Code to Reuse

- `Settings.Projects` and `findProjectByID` (relay/`settings.go`) — relay's project route handlers read these directly. No new data model.
- `LLMChannel` provisioning logic in `relay_llm_channel.go` — extends naturally to two sockets.
- `participatesInLLMChannel()` in `relay_llm_channel.go` — same matcher governs which services get the new env vars.
- Eve's `normalizeProject()` in `server.js` — keeps working unchanged.
- relay's existing bridge auth (`resolveAuth`) for the existing bridge socket — unchanged.

## Verification

1. **Build:** All four repos build. `go test ./...` passes in relay and relayLLM. Eve and scheduler load.
2. **Restart all services** (relay relaunches the others as managed services). Confirm relayLLM starts on the internal socket and is not reachable from any TCP port.
3. **Eve loads projects:** `curl --unix-socket $RELAY_FRONTEND_SOCKET -H "Authorization: Bearer $RELAY_FRONTEND_TOKEN" http://x/api/projects` returns the project list.
4. **Session create + message:** In Eve, open a project, start a session, send "hello", confirm streaming response.
5. **Tool call (project-scoped):** Ask the LLM to read a file in the project path → fsMCP call succeeds. Ask it to read `/etc/passwd` → blocked by `allowed_dirs`.
6. **Permission flow:** Trigger a permission-required tool → modal appears → approve → tool runs.
7. **Terminal:** Open a terminal in the project, run `pwd`, confirm output streams back.
8. **Scheduler:** Trigger a scheduled task → relay logs the inbound `POST /api/sessions` from scheduler → session runs → response received.
9. **Browser dev tools:** Confirm Eve only opens connections to its own server (no direct relayLLM URLs in flight).
10. **Negative test:** With relay running but relayLLM stopped, `POST /api/sessions` returns 502 from relay (proxy error) — confirms relayLLM is no longer directly accessible and proxy is the only path.

## Risks / Open Questions

- **WebSocket connection lifecycle:** Long-lived sessions; ensure relay doesn't time out the proxied WS. Set generous read/write deadlines.
- **Streaming correctness:** Reverse proxy must not buffer SSE-style or chunked LLM responses. Standard `httputil.ReverseProxy` handles this; verify with a long-streaming response.
- **Header propagation:** `Authorization`, `X-Forwarded-For` (if needed), `User-Agent` — copied through. Other Eve-specific headers reviewed.
- **Hooks:** relayLLM writes `.claude/settings.local.json` with `hookURL` pointing to itself for permission callbacks. Now that relayLLM is on an internal socket, the hook URL must be the relay frontend socket (so the Claude CLI subprocess, which runs as the user, can reach back). Confirm hookURL plumbing.
- **`assertStartupConfig` in Eve:** Fail-closed checks for `RELAY_LLM_*` env vars — update to the new names; otherwise Eve will refuse to start.

## Out of Scope

- Moving the agentic engine (session manager, providers, MCP integration, permission management, WS hub) into relay.
- Replacing relay mcp subprocesses with direct bridge calls from relayLLM.
- Eve project CRUD UI (still managed via relay's settings UI).
- TLS for the frontend socket (Unix socket permissions are sufficient for local-host).

---

# Project System — Implementation Status (Earlier Phase)

## Context

Projects were a lightweight concept in relayLLM — a name, path, and unused `AllowedTools` field. Every LLM session got a god-mode service token with access to all MCPs and all tools. This refactor makes projects a first-class infrastructure boundary in relay with scoped auth, while simplifying relayLLM to a pure execution engine.

## Architecture Decisions

1. **Relay owns projects** — name, path, MCPs, models, templates, token. Stored in `settings.json`.
2. **relayLLM is a pure execution engine** — receives directory + model + token from callers. No project awareness.
3. **Allowlist-only with wildcard** — `["*"]` allows all, explicit IDs restrict.
4. **Project tokens are inline** — permissions derived at auth time from `allowed_mcp_ids`, no separate token storage.
5. **No `tokens[]` array** — removed entirely. Auth is: service tokens (in-memory) → project tokens (inline in `projects[]`).
6. **MCP tool/schema data is runtime-only** — `discovered_tools` and `context_schema` are `json:"-"`, never persisted.
7. **`fs_bash` disabled by default** per-project via `disabled_tools`. OS-level sandboxing deferred.
8. **Claude CLI trusted** within its working directory.
9. **Server-side template resolution** — relay resolves templates; consumers pass `projectId + templateId`.

## Data Model

```
settings.json
├── external_mcps[]     — MCP server config (command, args, transport, oauth)
├── services[]          — background services (relayLLM, eve, scheduler, ...)
├── projects[]          — infrastructure boundaries
│   ├── id, name, path, created_at
│   ├── allowed_mcp_ids  — ["*"] or ["fsmcp", "macmcp"]
│   ├── allowed_models   — ["*"] or ["claude-opus", "gemma4:latest"]
│   ├── chat_templates[] — {id, name, model, mode, voice, system_prompt}
│   ├── token, token_hash — scoped credential (plaintext + SHA-256)
│   ├── disabled_tools   — {mcpId: ["tool_name"]}
│   └── context          — {mcpId: {allowed_dirs: [path]}}
└── admin_secret        — bridge admin auth
```

## Auth Flow

```
resolveAuth(token):
  1. Check in-memory service tokens (full access, bypass all)
  2. Check project tokens:
     - Hash plaintext, find project by token_hash
     - Derive permissions from allowed_mcp_ids (+ external_mcps list)
     - ["*"] → all MCPs PermOn
     - Return StoredToken view with permissions + disabled_tools + context
  3. No match → reject
```

## Implementation Status

### Done (relay repo, `project-scoping` branch)

- [x] `types.go` — Project, ChatTemplate structs
- [x] `project.go` — CreateProjectWithToken, generateProjectToken
- [x] `settings.go` — Project CRUD, SyncProjectToken, AuthenticateProject, isWildcard, schemaHasField
- [x] `tokens.go` — Slimmed to hashToken + sentinel errors
- [x] `router.go` — checkToolAccess operates on StoredToken directly; resolveAuth checks service → project tokens
- [x] `external_mcp.go` — Runtime-only schemas; GetContextSchema, ToolInfos, IsConnected methods
- [x] `settings_store.go` — normalize/defaults for Projects
- [x] Removed: `ipc_tokens.go`, `migrate_projects.go`, Tokens[] field, discovered_tools/context_schema persistence
- [x] `settings.json` — migrated 3 projects with tokens, cleaned stale data
- [x] Tests — 5 project tests + updated router/settings tests, all passing

### Done: Bridge API + relayLLM proxy

- [x] Bridge API: `ListProjects`, `GetProject` request types
- [x] relayLLM removed local project store, proxies via bridge
- [x] Sessions accept `mcpToken`; provider uses it instead of `RELAY_MCP_TOKEN`
- [x] Eve fetches projects from relayLLM (which proxies to bridge), normalizes to camelCase, passes `mcpToken` in session create

### Pending: Settings UI

- [ ] IPC handlers for project CRUD (`ipc_projects.go`)
- [ ] Settings UI: Projects tab (replace Security tab)
- [ ] Wire project CRUD into settings HTML/JS

### Superseded: this proxy chain is being replaced by the front-door migration above. After that migration, project endpoints live directly on relay's HTTP API, not on relayLLM.

## Future Work

- **Eve project management UI** — full CRUD within Eve (not just relay settings)
- **OS-level `fs_bash` sandboxing** — macOS `sandbox-exec` to restrict to `allowed_dirs`
- **Per-template MCP subsets** — "read-only" template disables write tools
- **Project audit trail** — log tool calls per project/session
- **Direct bridge invocation** — relayLLM calls bridge directly instead of per-session `relay mcp` processes
- **relayLLM validates token→path** — call relay to verify directory matches token's project
