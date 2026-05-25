# Make "Save" close the project edit form (parity with other Settings flows)

## Context

In the Settings UI's **Projects** tab, clicking **Save** on an existing project's edit form sends the update to the backend and the project list refreshes — but the edit form **stays open**. The user has to click **Cancel** to get back to the list. The expected behavior is: Save closes the add/edit sub-flow and returns to the project list view, while leaving the Settings window itself open.

All three Save buttons in Settings (`settings.html:732`, `:1321`, `:1743`) should behave the same way — close their sub-flow on success, keep the parent window open. After audit, **only one** of them is wrong; the other two already do the right thing, so this is a one-spot fix plus a verification pass.

## Audit of all Save flows

Single file: `/Users/jonathan/source/barelyworkingcode/relay/settings.html`. All three Save buttons inspected:

| Tab | Button | Handler | Closes sub-flow? | Mechanism |
|---|---|---|---|---|
| Services | `Save` (`:732`) | `saveServiceEdit()` (`:801–829`) | ✓ yes | Optimistic: sets `state.editingServiceId = null` immediately at `:827` after sending IPC |
| Services | New service (Add) | n/a — `addService()` flow → `onServiceAdded()` (`:841–846`) | ✓ yes | Callback sets `editingServiceId = null` on success |
| MCP Servers | Add (no edit form exists; see `:268`) | `addExternalMcp()` → `onExternalMcpAdded()` (`:621–627`) | ✓ yes | Callback sets `editingMcpId = null` at `:625` |
| Projects | `Create` (new) (`:1321`) | `saveProjectForm()` → `onProjectAdded()` (`:1455–1463`) | ✓ yes | Callback sets `editingProjectId = null` + `projectForm = null` at `:1459–1460` |
| **Projects** | **`Save` (edit) (`:1321`)** | `saveProjectForm()` → `onProjectUpdated()` (`:1465–1484`) | **✗ no** | Callback refreshes form state (token, disabled_tools) but **never closes the form** |
| Service Inspector | `Create`/`Save` resource (`:1743`) | `saveResourceForm()` → `onServiceResourceResult()` (`:1916–1943`) | ✓ yes | Callback clears `serviceResourceEditing[key]` + `serviceResourceForm[key]` at `:1934–1935` |

**Result:** The only broken flow is the existing-project edit Save in the Projects tab.

## Why `onProjectUpdated` currently leaves the form open

`onProjectUpdated` (`settings.html:1465–1484`) was written to handle two scenarios with the same code path:
1. The user clicked Save on this form.
2. (Theoretical) a server-side mutation happened to the same project (e.g. `SyncProjectToken` auto-disabling `fs_bash`) while the form was open.

For case (2) the code does a surgical merge of authoritative `disabled_tools` and `token` into the open form (`:1471–1476`), then refreshes. To keep that merge safe, the original author chose not to close the form.

But verifying against the actual IPC topology:
- `onProjectUpdated` is only emitted from `ipc_projects.go:202` (`ipcUpdateProject`) and `:330` (`ipcUpdateProjectDisabledTools`). Both are IPC handlers — neither is reachable from a cross-process source.
- HTTP/Eve mutations notify the tray via `pushFullProjects()` → `onProjectsReloaded` (`ipc_handlers.go:101–107`, `settings.html:897`), **not** `onProjectUpdated`.
- `update_project_disabled_tools` IPC is never sent from `settings.html` (only `update_project` is, in `saveProjectForm`).

So in practice `onProjectUpdated` only fires as a direct response to the user's own Save click on this very form. Closing the form there is safe and matches `onProjectAdded`'s shape.

## The fix

**File:** `/Users/jonathan/source/barelyworkingcode/relay/settings.html`
**Function:** `onProjectUpdated` (`:1465–1484`)

In the existing `if (state.editingProjectId === p.id && state.projectForm)` branch (`:1471–1476`), after the surgical state merge, close the form instead of just refreshing it. Concretely: replace the surgical-merge body with a clean close (`state.editingProjectId = null; state.projectForm = null;`). The surgical merge becomes dead code because the form is going away — the persisted project from `state.projects` (just updated at `:1467`) is the new source of truth, and the next time the user opens the form, `editProject()` (`settings.html:982-998`) will deep-copy the fresh row.

Result: Save on an existing project now ends in the list view, just like Create does (`onProjectAdded` at `:1459–1460`), and just like Services Save (`saveServiceEdit` at `:827`), and just like resource Save (`onServiceResourceResult` at `:1934–1935`).

### Error path

`onProjectError` (`:1524–1528`) already sets both `projectError` (rendered in the list view at `:932–933`) and `projectFormError` (rendered in the form at `:1161–1162`). Since the form is closed only on the success callback, validation failures from `saveProjectForm` (`:1434–1442`) and server-side errors still display in the open form. No change needed.

## Critical files

- `/Users/jonathan/source/barelyworkingcode/relay/settings.html` — only file modified. Single edit inside `onProjectUpdated` at `:1465–1484`.

## Reused existing utilities

- The `editingProjectId = null; projectForm = null;` close pattern is already established at `:1459–1460` (`onProjectAdded`), `:1492–1493` (`onProjectRemoved`), and `:903–904` (`onProjectsReloaded`).
- The trailing `if (state.page === 'projects') render('push');` (already at `:1483`) handles the repaint.

## Verification

1. Build and launch: `./build.sh` (default target installs to `/Applications/Relay.app` and launches).
2. Open **Relay → Settings → Projects**.
3. **Edit existing project flow:**
   - Click an existing project to edit it.
   - Change any field (e.g., name, system prompt on a chat template, toggle a tool checkbox).
   - Click **Save**.
   - **Expected:** the form closes, the projects list view is shown, the edited project reflects the new values. Settings window stays open.
4. **New project flow (regression check):**
   - Click **+ New Project**, fill in name + path, click **Create**.
   - **Expected:** form closes, new row appears in the list (unchanged behavior).
5. **Validation error path:**
   - Edit a project, blank the name, click **Save**.
   - **Expected:** form stays open, red error banner appears at `:1161` ("Project name is required").
6. **Other tabs — regression checks:**
   - Services tab: edit a service, click Save → form closes, list shown.
   - Service Inspector → Resources: create or edit a resource, click Create/Save → resource form closes, list shown.
   - MCP Servers tab: add a new MCP, click the add button → add form closes on success.
7. **Hermetic tests:** `go test ./...` — `ipc_projects_test.go` already covers the `onProjectUpdated` event emission; ensure nothing breaks. No new test needed (UI-only change in `settings.html`).
8. **Manual screencast (optional but recommended per project convention — see `feedback_test_with_chrome.md`):** if reachable via the WebView in dev mode, drive the click flow with the browser MCP tools and capture a short GIF showing Save → list view transition for both add and edit paths.
