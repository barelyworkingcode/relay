# Why the Settings UI Bugs Weren't Caught — Memo

> Decision: keep this as analysis only. No test work scoped from this. Accepting that `settings.html` regressions are caught manually (consistent with ADR-001).

## Context

Two bugs in `settings.html` slipped through the existing test suite and were only found by manual inspection in the running app:

1. **Tool count always zero.** The MCP Servers card read `mcp.discovered_tools`, but `ExternalMcp.DiscoveredTools` is `json:"-"` in `types.go:49`, so that field never reached the UI. The live data lives in `state.mcpToolCache`, populated by `buildToolCache()` in `ipc_handlers.go:83`.
2. **"Selected" tri-state button didn't stick.** `projMcpState()` in `settings.html:1058` returned `'all'` whenever `disabled_tools[mcpID].length === 0`. But `setProjMcpState('selected')` writes `[]` as a "selected mode active" sentinel — so the very next render flipped the button back to "All tools" and the tool picker never appeared.

Both bugs were live in shipped code and visible the moment the Settings window opened.

## Why the tests didn't catch them

### The repo has zero JavaScript test coverage

- No Jest, Vitest, Playwright, Puppeteer, or JSDOM harness.
- No `.test.js` / `.spec.js` files anywhere.
- `settings.html` is a single embedded blob (`//go:embed settings.html` in `settings_html.go:10`) and runs only inside the native WKWebView at runtime.

### The testing strategy explicitly punts on UI

`docs/decisions/001-testing-strategy.md` lists what is *not* tested:
> Cocoa tray UI (`platform_darwin.go`, menu rendering, dock interactions) — exercise via `scripts/demo.sh`.

`docs/testing-roadmap.md` flags the same gap for the eve frontend (`❌ Playwright tests against the frontend dispatcher. Highest leverage per hour.`). `settings.html` isn't even listed — it's implicitly part of the same untracked UI surface.

The hermetic Go suite passes because it tests the Go layer end-to-end, but stops at the IPC boundary. Both bugs lived past that boundary.

### Bug 1 was *almost* caught by a Go test, but the contract isn't asserted

- `ipcListMcpTools` is tested at `ipc_projects_test.go:329–363` — verifies the IPC handler returns tools.
- `buildToolCache` has no dedicated unit test; it's exercised only indirectly via `pushFullSettings`.
- **No test asserts the round-trip:** "if the UI relies on `mcp_tool_cache` to count tools, the payload `pushFullSettings` emits must contain it." That's a contract the UI silently depended on, and Go tests had no awareness of it.

Before today's fix, `pushFullSettings` didn't even include `mcp_tool_cache` — meaning *after* an MCP add/remove/auth, the count would stay stale until the window was reopened. A Go-level contract test would have caught that omission.

### Bug 2 was pure client-side logic

`projMcpState` / `setProjMcpState` are JavaScript-only state machines. There is no Go-side equivalent and no harness that runs them. The empty-array sentinel is a JS-only convention with no other code (Go or otherwise) to enforce it.

## Categorization

| Bug | Class | Where it lived | Why undetected |
|---|---|---|---|
| 1 | UI/Go payload contract drift | Go emits payload missing a key the UI silently depends on | No assertion that the JSON shape `pushFullSettings` emits matches what `settings.html` reads |
| 2 | Pure JS state-machine bug | A sentinel convention contradicted by its own consumer | No JS test runner; manual eyeball is the only enforcement |

## Implications going forward

- The two bug classes are different and would need different tooling to catch automatically.
- For now, both classes remain manually-detected — same posture as the eve frontend and the Cocoa tray. ADR-001 is consistent with this.
- If future settings UI work warrants it, the lightest paths would be (a) a Go-side contract test that asserts `pushFullSettings` includes every key the UI's render functions reference, and (b) extracting `settings.html`'s pure helpers into a file testable under `node --test` (built-in, no npm deps). Not scoped here.
