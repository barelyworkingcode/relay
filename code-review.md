# Code Review Log

## Review 10 — 2026-03-26 (Opus 4.6)

**Scope**: Modified files only — `trayapp.go`, `bridge/server.go`, `service_registry.go`. Focused on shutdown reorder, signal handler fix, and event-driven OnProcessExit callback.

**Findings** (2 MEDIUM, fixed):

1. **Redundant `signal.Reset` after `signal.Stop`** (`trayapp.go`): `signal.Stop(sigCh)` already restores the OS default signal disposition when `sigCh` is the sole channel registered for those signals. The extra `signal.Reset` call was unnecessary. Removed.

2. **`OnProcessExit` dispatches UI work during shutdown** (`trayapp.go`): During `cleanup()`, `registry.StopAll()` kills processes; each reaper goroutine fires `OnProcessExit`, which dispatches `updateMenu()` to the main thread. When cleanup runs from SIGTERM (goroutine, not main thread), these dispatches execute on the main run loop and can touch partially torn-down state. Added `ctx.Err()` guard to suppress dispatches after shutdown begins.

**Verified**: Clean build, SIGTERM shutdown with no orphans, no flip-flop with Reviews 6–9.

---

## Review 9 — 2026-03-24 (Opus 4.6)

**Scope**: Full codebase review — every Go source file across main, bridge, mcp, jsonrpc packages (~35 files). Line-by-line analysis focused on HIGH priority: bugs, race conditions, resource leaks, security, enterprise patterns, DRY, separation of concerns, Go best practices, code reduction.

**Finding**: No HIGH priority issues. Consistent with Reviews 6–8 — not flip-flopping.

Verified: lock ordering (m.mu → toolsMu; tokenMu → mu) consistent; all goroutines have clean shutdown; deep-copy settings prevents shared-state mutation; TOCTOU avoided in Reconcile, resolveAndRemove, and token generation; OAuth PKCE S256 with proper token refresh serialization; bridge panic recovery and per-connection context cancellation; Unix socket 0600 permissions; no DRY violations or code reduction opportunities.

**No changes made.**

---

## Review 8 — 2026-03-24 (Opus 4.6)

**Scope**: Full codebase review — every Go source file (main, bridge, mcp, jsonrpc packages; ~30 files). Manual line-by-line analysis focused on HIGH priority: bugs, enterprise patterns, DRY, separation of concerns, Go best practices, code reduction.

**Finding**: No HIGH priority issues. Consistent with Reviews 6 and 7 — not flip-flopping.

Verified: lock ordering consistent (ExternalMcpManager.mu → toolsMu; tokenMu → mu), no deadlock risk. All goroutines have clean shutdown (readLoop drains pending, StopAll kills concurrently, goFunc tracked via WaitGroup). Settings deep-copy via JSON round-trip prevents shared-state mutation. Interface boundaries (McpConnection, SettingsStore, ToolProvider, ServiceManager) provide clean separation. DRY patterns already consolidated (generic unmarshalIPC/ensureSlice/ensureMap, shared CLI framework, transport-agnostic mcpHandshake).

No code reduction opportunities found — previous reviews already eliminated redundancy.

**No changes made.**

---

## Review 7 — 2026-03-24 (Opus 4.6)

**Scope**: Full codebase review — all Go source files across main, bridge, mcp, jsonrpc packages. Focused on HIGH priority: enterprise patterns, DRY, separation of concerns, Go best practices, bugs, code reduction. Automated analysis flagged ~15 candidates; each was manually verified against the source.

**Finding**: No HIGH priority issues found. All automated findings were false positives or low-severity:
- Alleged race in `Reconcile()` — both lists computed in single `RLock`; individual ops are idempotent and thread-safe.
- Alleged goroutine leak in `readLoop` — pending entry deleted on write failure; readLoop drains all pending on death.
- Alleged nil metadata panic in token refresh — `discoverOAuth()` always returns non-nil via fallback path.
- Alleged service stop deadlock — `proc.done` closed via `defer`, runs even on panic.
- Other DRY/error-handling findings were all LOW severity, not flip-flopping from Review 6.

**No changes made.**

---

## Review 6 — 2026-03-24 (Opus 4.6)

**Scope**: Full codebase review — all 40+ Go source files across main, bridge, mcp, jsonrpc packages. Focused on HIGH priority: enterprise patterns, DRY, separation of concerns, Go best practices, bugs, and code reduction.

**Finding**: No HIGH priority issues found. The codebase is clean and well-structured.

Previous reviews (commits `3baaf6f`–`12b2e5c`) already addressed race conditions, spec conformance, security hardening, DRY consolidation (generic helpers, shared CLI framework, centralized timeouts), and observability. The current state reflects those improvements — dependency injection via interfaces, compile-time assertions, proper mutex discipline with documented lock ordering, atomic file writes, and thorough test coverage.

**No changes made.**
