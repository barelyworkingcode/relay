# Richer Project System — Architecture & Implementation Status

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

### Pending: Phase 1 completion (relay)

- [ ] Bridge API: `ListProjects`, `GetProject`, `ListMcps` request types
- [ ] IPC handlers for project CRUD (`ipc_projects.go`)
- [ ] Settings UI: Projects tab (replace Security tab)
- [ ] Wire project CRUD into settings HTML/JS

### Pending: Phase 2 (relayLLM)

- [ ] Remove `project.go` (project store) and `/api/projects` endpoints
- [ ] Accept `mcpToken` in session creation
- [ ] Use project token in `provider_chat_base.go` instead of `RELAY_MCP_TOKEN`
- [ ] Add `/api/relay/projects` proxy endpoint for Eve

### Pending: Phase 3 (Eve)

- [ ] Fetch projects from relay (via relayLLM proxy)
- [ ] Resolve templates, pass explicit params to relayLLM
- [ ] Update project dialog

### Pending: Phase 4 (Scheduler)

- [ ] Fetch project info from relay
- [ ] Resolve templates before session creation

## Future Work

- **Eve project management UI** — full CRUD within Eve (not just relay settings)
- **OS-level `fs_bash` sandboxing** — macOS `sandbox-exec` to restrict to `allowed_dirs`
- **Per-template MCP subsets** — "read-only" template disables write tools
- **Project audit trail** — log tool calls per project/session
- **Direct bridge invocation** — relayLLM calls bridge directly instead of per-session `relay mcp` processes
- **relayLLM validates token→path** — call relay to verify directory matches token's project
