# Native Project Management in Relay

## Context

Today, projects are managed through Eve's `project-dialog.js`, which calls relay's HTTP API at `/api/projects/*`. Relay's tray Settings UI has Services / MCP Servers / Service Inspector tabs, but **no Projects tab**. That makes the project — the primary security boundary in relay — the only first-class object you can't manage from relay's own UI.

The goal is to add a native Projects tab to relay's tray Settings, coexisting with Eve's existing dialog (both hit the same backend). Make security trimming first-class in the UI: per-MCP and per-tool, with token display and rotation.

What the exploration confirmed:

- **Project CRUD is already complete** in `settings.go` (CreateProjectWithToken, UpdateProject*, RemoveProject) and exposed via `project_routes.go`.
- **Tool-level security trimming is already enforced** in `router.go:45-57` (`checkToolAccess` checks `Permissions` map AND `DisabledTools`). The data model supports per-tool — only the UI doesn't expose it.
- **Skills generation already lives in relay** (`skills.go` — `EmitSkill`, `RemoveSkill`, `reconcileProjectSkill`). RelayLLM is purely a consumer (passes `RELAY_TOKEN` env var, reads `SKILL.md` from disk). **No relayLLM code changes required.**
- **What's missing**: a Projects tab in `settings.html`, IPC handlers (`ipc_projects.go`), a token rotation method, a per-tool tri-state picker, and a "regen skill now" trigger.

Branch name across all three repos: **`feature/native-project-mgmt`**.

## Approach

### A. Backend (relay)

#### New Settings methods — `settings.go`

```go
// RotateProjectToken generates fresh credentials, replaces Token + TokenHash,
// returns the new plaintext. Does not save; use within store.With.
// SECURITY NOTE: rotation invalidates the previous token at next AuthenticateProject
// call — active Eve/relayLLM sessions get 401 on their next request and must re-auth.
func (s *Settings) RotateProjectToken(id string) (newPlaintext string, ok bool)

// UpdateProjectDisabledTools replaces the per-MCP disabled-tools slice.
// Empty slice deletes the map key. Refuses MCPs not in AllowedMcpIDs (defense
// against UI bugs that try to scope tools for unallowed MCPs).
func (s *Settings) UpdateProjectDisabledTools(id, mcpID string, disabled []string)

// SetProjectGenerateSkill flips the GenerateSkill flag for reuse from IPC + HTTP.
func (s *Settings) SetProjectGenerateSkill(id string, gen bool)
```

#### New routes — `project_routes.go`

`RegisterProjectRoutes` signature grows by one argument: `tools MCPToolsProvider`.

```
POST /api/projects/{id}/rotate_token     → {token: "..."}        (200 / 404)
POST /api/projects/{id}/regen_skill      → {path: "..."}         (200 / 404 / 503 if no lister)
GET  /api/mcps/{id}/tools                → [{name, description}] (200 / 404)
```

`MCPToolsProvider` is a new one-method interface (`ToolInfos(id) []ToolInfo`) co-located with `ContextSchemasProvider` in `project_routes.go`. Already implemented by `*ExternalMcpManager`.

#### Project change notification

Currently `/api/projects/*` mutations don't fan out to the tray UI. Add an `onChange func()` callback to `RegisterProjectRoutes`; trayapp wires it to `pushFullProjects()`. Eve's mutations now propagate to the in-relay Projects tab live.

### B. IPC layer (new file)

#### `ipc_projects.go`

Mirrors `ipc_mcps.go` structurally. Handlers:

| Handler                          | Message constant                  | Emits                       |
|----------------------------------|-----------------------------------|-----------------------------|
| `ipcCreateProject`               | `create_project`                  | `onProjectAdded`            |
| `ipcUpdateProject`               | `update_project`                  | `onProjectUpdated`          |
| `ipcRemoveProject`               | `remove_project`                  | `onProjectRemoved`          |
| `ipcRotateProjectToken`          | `rotate_project_token`            | `onProjectTokenRotated`     |
| `ipcRegenProjectSkill`           | `regen_project_skill`             | `onProjectSkillRegen`       |
| `ipcUpdateProjectDisabledTools`  | `update_project_disabled_tools`   | `onProjectUpdated`          |
| `ipcListMcpTools`                | `list_mcp_tools`                  | `onMcpToolsListed`          |

`IPCContext` (in `ipc_handlers.go`) gains two fields: `Tools MCPToolsProvider` and `SkillLister SkillLister`. Wired in `trayapp.go`.

Update `ipc_handlers.go`:
- Add the 7 message-type constants.
- Register handlers in the `ipcHandlers` map.
- Extend `pushFullSettings` to include projects.
- Add `pushFullProjects()` (called from `onChange` wired into HTTP project routes).

Update `ipc_handlers_test.go`:
- Append the 7 constants to the `required` slice in `TestIPCDispatch_AllDeclaredMessageTypesHaveHandlers`.

### C. UI — `settings.html`

Add a "Projects" sidebar item (3rd, between MCP Servers and Service Inspector). The CLAUDE.md project doc already says this tab should exist — aligning code with docs.

State additions:
```js
state.projects             // [Project, ...]
state.mcpToolCache         // { mcpId: [{name, description}, ...] }   (preseeded via __MCP_TOOL_CACHE_JSON__)
state.editingProjectId     // null = list, "new" = create form, "<id>" = edit
state.projectTokenVisible  // { id: bool }   (eye-toggle)
state.projectFreshToken    // { id: plaintext }   (transient banner after rotate)
state.projectFormError
```

#### List view
Card per project: name, truncated path, `N MCPs • M models • policy:{mode} • skill:{on/off}`, buttons: Edit / Regen Skill / Delete. Bottom: **+ New Project**.

#### Edit form (sections)

1. **Identity** — name, path (helper: "absolute; subs gain auto allowed_dirs scope")
2. **Allowed MCPs** — wildcard toggle; when off, per-MCP rows with tri-state buttons:
   - **All tools** (default; `DisabledTools[id]` empty)
   - **Selected tools** (expands to per-tool checkboxes; `DisabledTools[id] = liveTools − checked`)
   - **No tools** (MCP removed from `AllowedMcpIDs`)
3. **Allowed Models** — wildcard + comma-separated (matches eve UX)
4. **Chat Templates** — list, inline editor: name, model, mode, voice, system prompt, append_claude_md, use_relay_tools
5. **Permission Policy** — default_mode select; allowed_tools / denied_tools textareas
6. **Skill** — `GenerateSkill` toggle + **Regenerate Now** button
7. **Token** (edit only) — masked + eye toggle + copy + **Rotate Token** (with confirm modal that mentions active sessions invalidate)

Tool-checkbox changes update local form state only; persisted on **Save**, which fires a single `update_project` IPC with the full payload. Avoids spurious SKILL.md regens per click and matches existing Service form UX.

Tri-state derivation (load-bearing):
```
wildcard(allowed_mcp_ids):            All tools (no per-MCP knobs)
id not in allowed_mcp_ids:            No tools
id in allowed; disabled_tools empty:  All tools
id in allowed; disabled_tools set:    Selected tools (checked = liveTools − DisabledTools[id])
```

When `mcpToolCache[id]` is missing on first paint, dispatch `list_mcp_tools` and show "Loading…".

Edge cases the UI handles:
- **Dangling MCP ID** in `AllowedMcpIDs` (MCP was deleted in MCP Servers tab) → strike-through with "MCP no longer registered" label.
- **Tool no longer present** in `ToolInfos` but still in `DisabledTools` → render as muted "(no longer available)" so the user can prune it.
- **Empty `ToolInfos`** (HTTP MCP not yet OAuth'd) → "Authenticate this MCP in MCP Servers first."

### D. trayapp.go wiring

- `openSettingsWindow`: build seed `mcpToolCache` from `extMgr.ToolInfos(id)` per registered MCP, pass into `renderSettingsHTML`.
- `runTrayApp` / `frontend_server.go`: pass `extMgr` (as `MCPToolsProvider`) and an `onChange` callback into `RegisterProjectRoutes`.
- `IPCContext` initialized with `Tools` and `SkillLister`.

### E. settings_html.go

Add placeholders: `__PROJECTS_JSON__`, `__MCP_TOOL_CACHE_JSON__`. Substitute in `renderSettingsHTML`.

## Critical files

**Modified:**
- `/Users/jonathan/source/barelyworkingcode/relay/settings.go` — three new methods (RotateProjectToken, UpdateProjectDisabledTools, SetProjectGenerateSkill)
- `/Users/jonathan/source/barelyworkingcode/relay/project_routes.go` — 3 new routes, new arg on `RegisterProjectRoutes`, `MCPToolsProvider` interface, `onChange` callback
- `/Users/jonathan/source/barelyworkingcode/relay/ipc_handlers.go` — 7 new message constants, register handlers, extend `pushFullSettings`, add `pushFullProjects`, extend `IPCContext`
- `/Users/jonathan/source/barelyworkingcode/relay/settings.html` — Projects tab UI (large)
- `/Users/jonathan/source/barelyworkingcode/relay/settings_html.go` — placeholders + substitutions
- `/Users/jonathan/source/barelyworkingcode/relay/trayapp.go` — wire seed cache + onChange + IPCContext fields
- `/Users/jonathan/source/barelyworkingcode/relay/frontend_server.go` — pass new args through
- `/Users/jonathan/source/barelyworkingcode/relay/CLAUDE.md` — Projects tab now exists; remove discrepancy

**New:**
- `/Users/jonathan/source/barelyworkingcode/relay/ipc_projects.go`
- `/Users/jonathan/source/barelyworkingcode/relay/ipc_projects_test.go`
- `/Users/jonathan/source/barelyworkingcode/relay/settings_projects_test.go`
- `/Users/jonathan/source/barelyworkingcode/relay/docs/decisions/004-project-mgmt-in-relay.md` (ADR)

**Reused (no changes):**
- `skills.go` — `EmitSkill`, `RemoveSkill`, `reconcileProjectSkill` cover all skill needs
- `project.go` — `generateProjectToken` reused by `RotateProjectToken`
- `external_mcp.go` — `ToolInfos(id)` already returns the picker payload

## Test plan

All tests must call `mkSandboxRelayHome(t)` (CLAUDE.md headline rule; enforced by `support_safety_test.go`).

**New files**
- `ipc_projects_test.go` — happy path + edge cases for each of 7 handlers (malformed payload coverage comes free via `TestIPCDispatch_HandlerSurvivesMalformedPayload`).
- `settings_projects_test.go` — direct unit tests for `RotateProjectToken`, `UpdateProjectDisabledTools`, `SetProjectGenerateSkill`, plus interaction with `SyncProjectToken` (wildcard ↔ explicit transitions preserve / clean `DisabledTools` correctly).

**Extended**
- `project_routes_test.go` — `TestProjectRoutes_RotateToken`, `TestProjectRoutes_RegenSkill_OK`, `TestProjectRoutes_RegenSkill_NoListerReturns503`, `TestProjectRoutes_ListMcpTools`.
- `ipc_handlers_test.go` — extend `required` slice with 7 new constants.
- `security_regression_test.go` — five additions:
  1. `TestSec_RotateProjectToken_OldTokenRejectedOnNextAuth`
  2. `TestSec_UpdateProjectDisabledTools_NotInAllowedMcp_NoOp`
  3. `TestSec_RotateProjectToken_PersistsAcrossReload`
  4. `TestSec_ListMcpTools_DoesNotLeakCredentials` (no command/env/oauth_state fields in response)
  5. `TestSec_RegenSkill_NoTokenInSkillMd`

**relayLLM** — no code changes; no test changes. Verified by grepping the tree: `auth.go`, `proxy_registry.go`, `permission.go`, `manifest.go` contain zero references to projects. relayLLM consumes `RELAY_TOKEN` env var only.

**eve** — no code changes for coexist mode; the existing `project-dialog.js` continues to work against the unchanged `POST/PUT/DELETE /api/projects/*` contract. (A follow-up PR could deprecate eve's dialog and link to relay's tray, but that's not in this work.)

## Verification

End-to-end manual + automated:

1. **Hermetic tests pass**: `go test ./...` (every test sandboxed; `support_safety_test.go` confirms real ConfigDir untouched).
2. **Pre-commit hook passes**: `go build ./... && go vet ./... && go test ./...`.
3. **Live tier sanity** (optional, post-merge): `go test -tags=live ./...` to confirm relayLLM still spawns sessions cleanly with no project-related regressions.
4. **Demo harness**: `scripts/demo.sh --reset` → open Settings → Projects tab → create a project → tri-state tool selection → save → confirm `SKILL.md` regenerated under `<path>/.claude/skills/relay/` → rotate token → confirm new plaintext in UI, old plaintext rejected by `relay mcp call`.
5. **Eve coexistence check**: open eve's project dialog after creating a project in relay's tray → confirm it shows up. Edit it in eve → confirm relay's tab updates (proves `onChange` fan-out works).
6. **Chrome browser test** for the WebView UI per `feedback_test_with_chrome.md` memory: open the tray Settings via demo harness, screenshot Projects tab list view + edit view + tri-state expanded, validate the tool picker shows real MCP tools from the fixture.

## Known follow-ups (not in this PR)

- **Project path validation** (exists, is directory, is absolute) — scope expansion; flag separately.
- **`MCPToolsChanged` event** to refresh `mcpToolCache` when a new MCP authenticates while the Settings window is open.
- **MCP-rename cleanup pass** to prune stale `DisabledTools` entries automatically at MCP reconcile time (pre-existing silent permissive drift; not introduced here).
- **Eve dialog deprecation** — when relay's UI feels solid, add a "Manage Projects in Relay" deep link to eve and rip out `project-dialog.js`.
