# Settings Page — Deep Holistic Review & Fix

## Context

The settings page (`settings.html`, embedded into the tray via `settings_html.go`) is a ~1,675-line single-file SPA with a sidebar + content layout. Two classes of problems have accumulated:

1. **Functional regressions.** The user reports the sidebar "stops working" after moving around. Root cause confirmed: the render-suppression guards (`_svcFormRendered`, `_projFormRendered`) used to prevent IPC pushes from wiping in-flight form edits *also* swallow user-initiated tab-switch renders. Once an Edit form is open, switching away and back to that tab leaves the content area frozen on the previous tab. Compounding the issue: several IPC push handlers (`onServiceAdded`, `onServiceRemoved`, `onSettingsReloaded`) call `render()` unconditionally and can kick the user off whichever tab they're on. A document-level click listener at line 1595 has an unprotected `JSON.parse` that can throw on malformed snapshot data.

2. **UI inconsistency.** The Projects tab follows a "list view → dedicated form view" pattern (clean, focused). Services and MCPs cram an always-visible Add/Edit form below the list, doubling the page height and mixing read/write affordances. User wants Services and MCPs to adopt the Projects pattern, with the `+ Add` button moved to a top-right header position.

The outcome: tab switching that never silently fails, IPC pushes that never steal focus from the user's current tab, and a consistent Add affordance across all three tabs.

## Plan

### Phase 1 — Fix the tab-switch render-suppression bug (highest priority)

The current guard in `render()` (`settings.html:285-307`) conflates two concerns: (a) protecting in-flight form text from being wiped by an external data push, and (b) tab-switch rendering. The fix is to split those concerns.

**Approach: pass a `source` flag through `render()`**

- Add an optional `source` parameter to `render(source)`. Callers that originate from IPC pushes pass `'push'`; callers that originate from user navigation (`showPage`, `editService`, `cancelServiceEdit`, etc.) pass `'user'` or omit it.
- The `_svcFormRendered` / `_projFormRendered` guards only suppress re-render when `source === 'push'`. User-initiated renders always proceed.
- This preserves the existing "don't wipe my typing" behavior on status polls and `onProjectsReloaded`, but never blocks a tab switch or an explicit user action.

**Files:** `settings.html` — `render()` (lines 285-307), and every callsite (~30 of them; trivial mechanical pass).

### Phase 2 — Defensive IPC push handlers

Make every `window.on*` IPC push handler:

1. Mutate `state` unconditionally (data should always stay current).
2. Only call `render()` when `state.page` matches the affected tab. If the user is on a different tab, the next tab switch will pick up the fresh state.
3. Always pass `source: 'push'` to `render()`.

**Specific handlers to fix in `settings.html`:**

- `onServiceAdded` (line 742) and `onServiceRemoved` (line 747) — currently call `render()` unconditionally. Gate on `state.page === 'services'`.
- `onSettingsReloaded` (line 782) — currently a full unconditional re-render. Gate on current tab, and split: update state for all tabs, but only render the active one.
- `onProjectAdded`, `onProjectUpdated`, `onProjectRemoved`, `onMcpToolsListed`, `onExternalMcpAdded`, etc. — audit and apply the same pattern. Most already check `state.page === 'projects'` (good model to copy).
- `onServiceStatusBatch` (1651) and `onServiceActionResult` (1660) — already guarded correctly; leave alone.

### Phase 3 — Harden the document-level click listener

The delegated listener at `settings.html:1595-1600` does `JSON.parse(btn.dataset.row)` without a try/catch. A malformed `data-row` would throw out of the listener — not catastrophic (the browser catches it) but creates noisy console errors and aborts the dispatch silently.

Wrap the parse in try/catch, log via `console.warn` with the offending string, and bail out cleanly. While here, also guard `dispatchServiceAction` callers — a single try/catch around the whole listener body is the simplest answer.

### Phase 4 — Add button alignment (Services and MCPs)

Adopt the Projects "list view + dedicated form view" pattern for both Services and MCPs, with the Add button moved to a **top-right header position** (next to the `<h2>` title).

**State changes** (`settings.html` state object, lines 241-270):

- `editingServiceId: null` already exists — extend its semantics: `null` = list view, `'new'` = create form view, `'<uuid>'` = edit form view (mirrors how `editingProjectId` works).
- Add `serviceForm: null` to hold the in-flight form values (mirrors `projectForm`). On Save/Cancel, clear it and set `editingServiceId = null`. This removes the need to read values from the DOM via `svcFormValues()` since we'll persist them in state as the user types — but for the first pass, keep `svcFormValues()` and just read on submit (minimum-change refactor).
- Similarly add `editingMcpId: null` / `mcpForm: null` (MCPs currently have no edit support, only add — we're not adding edit; just gating create form behind a state flag).

**Render changes:**

- `renderServices()` (lines 571-651):
  - If `editingServiceId` is set → return `renderServiceForm()` (new function, extracted from lines 610-649).
  - Otherwise: render the `<h2>Services</h2>` row with `+ New Service` button on the right. Then render the list. No bottom form.
- `renderMcpServers()` (lines 309-410): same split. Extract the add form (lines 354-408) into `renderMcpForm()`. Header row gets `+ New MCP Server` button on the right.
- `renderProjects()` (line 815): leave as-is for now (button stays at bottom). User explicitly scoped this change to Services/MCPs.

**Header layout pattern** (reusable across the three tabs — write once, apply twice for this PR):

```html
<div class="page-header">
  <h2>Services</h2>
  <button class="btn" onclick="newService()">+ New Service</button>
</div>
```

Add a `.page-header` CSS rule next to the existing `.sidebar` / `.content` rules near the top of `settings.html`: `display:flex; justify-content:space-between; align-items:center; margin-bottom:16px`. The existing `<h2>` selector already has a margin; collapse it inside `.page-header h2 { margin: 0 }`.

**New functions to add (mirror the Projects pattern):**

- `newService()` — set `editingServiceId = 'new'`, `serviceForm = blankServiceForm()`, `_svcFormRendered = false`, `render()`.
- `cancelServiceEdit()` — already exists at line 697; extend to clear `serviceForm` and reset flag.
- `newMcp()` / `cancelMcpEdit()` — same pattern for MCPs.
- `blankServiceForm()` and `blankMcpForm()` — return the empty form-state objects.

**IPC contracts:** Unchanged. `add_service`, `update_service`, `add_external_mcp` etc. continue to work exactly as today. We are only restructuring the JS render layer.

### Phase 5 — Consistency cleanup

Small wins worth bundling into the same PR since we're already in the file:

- Remove dead `_svcFormRendered` / `_projFormRendered` *if* Phase 1 makes them redundant (it should — the new `source === 'push'` check replaces them). Single guard concept, not two flags.
- The `onServiceStatus` handler at line 758 does surgical DOM updates for the running toggle (good). Confirm that pattern still works after Phase 4 splits the form into its own render function — the `[data-svc-running="..."]` selector will still match because those toggles live in the list, not the form.
- Verify `onSettingsError` banner (line 773) survives across tab switches — it appends to `document.body`, so it should. Leave alone.

## Critical files

- `settings.html` — the entire change surface area. ~150 lines of JS edits + small CSS additions.
- `settings_html.go` — no changes expected. The embedded HTML is rebuilt on each launch; no IPC contract changes.
- `ipc_services.go`, `ipc_mcps.go`, `ipc_projects.go` — read-only reference. No backend changes.

## Verification

End-to-end manual verification with `./build.sh` then exercising the tray app's Settings window:

1. **Sidebar regression repro & fix.**
   - Open Settings → Services tab → click Edit on any service → switch to Projects tab → switch back to Services. Today: content stays on Projects. After fix: Services edit form re-appears.
   - Repeat with Projects edit form and switching to Services and back.

2. **IPC push doesn't steal tabs.**
   - Open Settings on Inspector tab. From a separate terminal, run `relay service register <new-service>`. The Inspector tab should NOT flip to Services. Inspector should keep showing live status data.
   - On Projects tab, externally mutate a project via Eve (or directly edit `settings.json` and bounce the tray). Projects tab updates; if user was on a different tab, that tab is not disturbed.

3. **Add button consistency.**
   - Services tab: top-right `+ New Service` button. Click → form view replaces list. Cancel → returns to list. Save → returns to list with new service visible.
   - MCPs tab: same flow with `+ New MCP Server`.
   - Projects tab: unchanged (`+ New Project` still at bottom of list).

4. **Document click listener hardening.**
   - Manually inject a malformed `data-row` via the DevTools console (`document.querySelector('.svc-action-btn').dataset.row = 'not-json'` then click). Expect a single `console.warn`, no uncaught exception, no broken subsequent clicks.

5. **Test suite.**
   - `go test ./...` (hermetic tier) — no Go behavior changed, but make sure nothing regresses. `settings_projects_test.go` and `settings_test.go` should both still pass.
   - No new Go tests required; this PR is JS-only inside the embedded HTML. (If we ever decide to test the WebView JS, it'd need a Playwright/Cypress harness — out of scope.)

6. **Chrome verification (per project convention).**
   - The settings UI is a WKWebView, not a Chrome tab, so Chrome MCP tooling doesn't apply directly. Visual verification is via the tray app on a built binary.
