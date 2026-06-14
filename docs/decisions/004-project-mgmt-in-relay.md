# ADR-004: Native Project Management Lives in Relay

**Status:** Accepted
**Date:** 2026-05-24

## Context

Projects are relay's primary security boundary — each binds a directory, an
allowed-MCP list, an optional per-tool disable list, an allowed-models list,
chat templates, and a scoped bearer token. Originally the only way to create or
modify a project was Eve's `project-dialog.js`, speaking to relay's HTTP API at
`/api/projects/*`. Three problems:

1. **Eve is not always available.** Relay runs fine with no frontend (headless
   CLI, scheduler-driven). Managing projects should not require booting Eve.
2. **Security trimming was not first-class in the UI.** Eve's dialog exposed
   MCP-level allow/deny only. The per-tool `DisabledTools` field — supported by
   the data model since day one — was invisible, as were the bearer token and
   the `GenerateSkill` flag.
3. **Skill generation already lives in relay** (`skills.go`). RelayLLM only
   consumes the generated `SKILL.md`. The feature didn't need to migrate; it
   needed to be made visible and triggerable from relay's own UI.

## Decision

Add a native Projects tab to relay's tray Settings WebView. Coexist with Eve's
dialog — do not replace it. Both UIs go through the same `Settings.*Project*`
mutators, so a project created in either place is indistinguishable in the data
store.

### One mutator layer, two entry points

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

Every mutator follows the established convention: take a project ID, mutate the
in-memory struct, **do not save** — persistence is the caller's job via
`store.With`. Adding methods (not structs) keeps the data model unchanged: any
existing `settings.json` loads and round-trips without migration.

The paths stay in sync. HTTP mutations fire an `onProjectsChanged` callback,
wired in the tray to `pushFullProjects()`, so an open Settings window reflects
Eve's edits. IPC handlers emit per-event messages (`onProjectAdded`,
`onProjectUpdated`, etc.) for the tray's own UI.

### Per-tool security trimming, surfaced

The data model has always supported per-tool denial via
`Project.DisabledTools[mcpID]`, enforced by `checkToolAccess` (router.go). The
tab exposes it as a tri-state per MCP — all tools / selected tools / no tools —
computed by the picker against the MCP's live tool list.

`UpdateProjectDisabledTools` refuses to scope tools for an MCP that is not in
the project's `AllowedMcpIDs` (and not wildcard), even when called directly:
otherwise a later allow-MCP change would silently inherit a stale deny list.

### Token rotation

`RotateProjectToken(id)` replaces both the plaintext token and its hash inline.
The old plaintext stops authenticating on the next `AuthenticateProject` call;
active Eve/relayLLM/CLI sessions holding it get 401 and must re-auth. The tray
surfaces this behind a confirmation modal and a "copy now, will not be shown
again" banner. RelayLLM needs no changes — it re-resolves the project token
just-in-time from the bridge on PTY spawn (see ADR-007).

### Skills auto-generation

`GenerateSkill`, `reconcileProjectSkill` (the declarative "if on, regenerate"
helper), and `EmitSkills` (the imperative regen) all pre-date this work. New:

- A toggle and "Regenerate Now" button in the edit form.
- `ipcRegenProjectSkill` and `POST /api/projects/{id}/regen_skill`, which call
  `EmitSkills` for one project regardless of `GenerateSkill`.
- Delete-side cleanup: `ipcRemoveProject` calls `RemoveSkill`, mirroring the
  HTTP DELETE route.

### What does not change

- The `Project` struct. No schema migration.
- Eve's `project-dialog.js` and the HTTP contract it depends on.
- RelayLLM. It holds no project config or struct and never stores or receives
  the token from Eve; it references a project only by `projectId`, brokering the
  scoped token just-in-time via the bridge's `ResolvePtyEnv` call and injecting
  `RELAY_PROJECT_TOKEN` into spawned children.
- The route auth flow: `AuthenticateProject`, `AuthenticateProjectByHash`, and
  `checkToolAccess` are untouched.

## Consequences

**Good:**

- Project management no longer depends on Eve being present.
- Per-tool selection is usable from a UI, not just by hand-editing
  `settings.json`.
- Token rotation has a discoverable affordance and a fresh-token banner.
- Eve's HTTP contract is preserved — no consumer churn.

**Tradeoffs:**

- Two project-management UIs to keep at feature parity. Mitigated by sharing the
  mutators and the `onProjectsChanged` fan-out that keeps them in sync.

**Deferred:**

- Eve dialog deprecation — optional follow-up: replace Eve's editor with a
  "Manage Projects in Relay" deep link once the tray UI proves itself.
- Deeper project-path validation. `validateProjectPath` (project.go) already
  rejects empty, relative, and `..`-traversal paths; existence / is-directory
  checks are still open.
- An `MCPToolsChanged` event so the tool cache refreshes mid-edit when an HTTP
  MCP finishes OAuth. Today the form picks up changes on next open.

## References

- New surfaces: `ipc_projects.go`, project mutators in `settings.go` /
  `project.go`, project routes in `project_routes.go`, Projects tab in
  `web/src` (bundled to `web/dist/settings.html`).
- Tests: `settings_projects_test.go`, `ipc_projects_test.go`,
  `project_routes_test.go`.
- Related: ADR-001 (testing strategy), ADR-002 (test seams),
  ADR-007 (project-token brokering), `docs/tokens.md`.