# ADR-004: Native Project Management Lives in Relay

**Status:** Accepted
**Date:** 2026-05-24

## Context

Projects are relay's primary security boundary — each binds a directory, an
allowed-MCP list, optional per-tool disable list, allowed-models list, chat
templates, and a scoped bearer token. Until now, the only way to create or
modify a project was through Eve's `project-dialog.js`, which speaks to
relay's HTTP API at `/api/projects/*`.

That worked, but it had three problems:

1. **Eve is not always available.** Relay can run with no frontend at all
   (e.g. when driving headless via the CLI or via the scheduler). Users had
   to spin up Eve just to add or revoke a project.
2. **Security trimming was not first-class in the UI.** Eve's dialog
   exposed MCP-level allow/deny only; the per-tool `DisabledTools` field
   that the data model has supported since day one was invisible. Same
   story for the bearer token (returned by relay, never displayed) and the
   `GenerateSkill` flag.
3. **Skill generation already lives in relay** (`skills.go` —
   `EmitSkill`, `RemoveSkill`, `reconcileProjectSkill`). RelayLLM only
   consumes the generated `SKILL.md`. The "auto-create skills" feature
   the user asked about did not need to migrate; it needed to be made
   visible and triggerable from relay's own UI.

## Decision

Add a native Projects tab to relay's tray Settings WebView. Coexist with
Eve's existing dialog — do not replace it. Both UIs go through the same
`Settings.*Project*` mutators, so a project created in either place is
indistinguishable in the data store.

### Shared mutator layer

Every project mutation, regardless of entry point, lands on a Settings
method (`CreateProjectWithToken`, `UpdateProjectMcps`, `UpdateProjectName`,
`UpdateProjectDisabledTools`, `RotateProjectToken`, …). New methods added
in this work follow the same shape: take a project ID, mutate the
in-memory struct, do **not** save. Persistence is the caller's job
(`store.With`) — the same convention every other Settings mutator uses.

Adding new methods (not new structs) keeps the data model unchanged. Any
existing `settings.json` continues to load and round-trip without
migration. `DisabledTools` was already part of the `Project` struct — it
just wasn't editable from the UI.

### Two entry points, one boundary

```
            ┌─────────────────────┐
   Eve  ──► │  HTTP                │
            │  /api/projects/*     │ ──┐
            └─────────────────────┘   │
                                      ├──► Settings.*Project* mutators ──► settings.json
            ┌─────────────────────┐   │
 Tray UI ─► │  IPC                 │ ──┘
            │  ipc_projects.go     │
            └─────────────────────┘
```

The two paths are otherwise interchangeable. The HTTP path emits an
`onProjectsChanged` callback after every successful mutation; the tray
wires that to `pushFullProjects()` so an open Settings window stays in
sync with Eve's edits. Similarly, IPC handlers emit per-event messages
(`onProjectAdded`, `onProjectUpdated`, etc.) so the tray's own UI updates
instantly.

### Per-tool security trimming, surfaced

The data model has always supported per-tool denial via
`Project.DisabledTools[mcpID] []string`, and `router.go:checkToolAccess`
has always enforced it. The new tab exposes this as a tri-state per MCP:

- **All tools** — MCP in `AllowedMcpIDs`, `DisabledTools[id]` empty
- **Selected tools** — MCP in `AllowedMcpIDs`, `DisabledTools[id]` =
  `liveTools − checked` (computed by the UI from the picker)
- **No tools** — MCP not in `AllowedMcpIDs`

`UpdateProjectDisabledTools` refuses to set tools for an MCP not in
`AllowedMcpIDs`, even when called directly — defense against UI bugs that
might try to scope tools for an unallowed MCP and leave a stale list
ready to grant unintended access if the MCP is later added back. Covered
by `TestUpdateProjectDisabledTools_RefusesNotInAllowedMcps`.

### Token rotation

`RotateProjectToken(id)` replaces both `Token` (plaintext) and `TokenHash`
inline. The next `AuthenticateProject` call hashes the presented
plaintext and looks it up by hash — the old plaintext no longer
authenticates. Active Eve/relayLLM/CLI sessions holding the old plaintext
receive 401 on their next request and must re-auth.

The tray UI surfaces this with a confirmation modal (so it's not a
one-click footgun) and a "copy now, will not be shown again" banner.
RelayLLM does not need any changes: it already holds a token per session
and only re-resolves on PTY spawn via the bridge.

Security regression test: `TestSec_RotateProjectToken_OldTokenRejectedOnNextAuth`
(covered in `settings_projects_test.go` and `ipc_projects_test.go`).

### Skills auto-generation

`GenerateSkill` (the `Project` flag), `reconcileProjectSkill` (the
declarative "if on, write; if off, leave alone" helper), and `EmitSkill`
(the imperative "regen now" function) all pre-date this work. What's new:

- A toggle and "Regenerate Now" button in the Project edit form.
- An IPC handler (`ipcRegenProjectSkill`) and HTTP route
  (`POST /api/projects/{id}/regen_skill`) that call `EmitSkill` for one
  project regardless of `GenerateSkill`.
- Delete-side coverage: `ipcRemoveProject` calls `RemoveSkill` on the
  outgoing project's skill directory, mirroring the existing
  `project_routes.go DELETE` behavior.

End-to-end lifecycle covered by
`TestProjectLifecycle_CreateWithSkill_Delete_CleansUpSkillFile`.

### What does not change

- The `Project` struct. No schema migration.
- Eve's `project-dialog.js`. Continues to work against the unchanged
  HTTP contract.
- RelayLLM. Zero references to projects; consumes only `RELAY_TOKEN` env
  var and the bridge's `ResolvePtyEnv` response. No code changes, no test
  changes.
- The route auth flow. `AuthenticateProject` and `AuthenticateProjectByHash`
  are untouched. `checkToolAccess` is untouched.

## Consequences

**Good:**

- Relay's UI no longer depends on Eve being present to manage projects.
- Per-tool selection is finally usable from a UI, not just from
  hand-editing `settings.json`.
- Token rotation has a discoverable affordance and an audit trail in the
  fresh-token banner.
- Eve's HTTP contract is preserved — no consumer churn.

**Tradeoffs:**

- Two project-management UIs to keep in feature parity. Mitigated by both
  hitting the same mutators and by the new `onProjectsChanged` fan-out
  keeping them in sync at the data layer.
- The tray HTML grew by ~500 lines of JS. Acceptable for now; if the file
  becomes unwieldy, splitting into per-tab embedded assets is a separate
  refactor (and orthogonal to this work).

**Deferred:**

- Eve dialog deprecation. Optional follow-up: replace Eve's editor with
  a "Manage Projects in Relay" deep link once the tray UI proves itself.
- Project-path validation (exists, is directory, is absolute). Currently
  only rejects empty strings.
- `MCPToolsChanged` event so the tool cache refreshes mid-edit when an
  HTTP MCP completes OAuth. Today the form picks up changes on next form
  open.

## References

- Plan: `plans/i-made-myself-a-delegated-map.md`
- New code: `ipc_projects.go`, `settings.go` (3 methods),
  `project_routes.go` (3 routes), `settings.html` (Projects tab)
- Tests: `ipc_projects_test.go`, `settings_projects_test.go`,
  `project_routes_test.go` (extended)
- Related: ADR-001 (testing strategy), ADR-002 (test seams)
