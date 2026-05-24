# Testing Roadmap

Living doc that captures what's tested where, what's coming next, and
lessons we've learned bringing each repo up to the bar described in
[ADR-001](decisions/001-testing-strategy.md).

The headline rule everywhere: **no test may touch the user's real config
directory.** Each repo enforces this in its own way; record the approach
in this doc when you add coverage to a new one.

## Status by repo

| Repo | Default tier | Live tier | Pre-commit gate | Sandbox guard | Notes |
|---|---|---|---|---|---|
| **relay** | ✅ (1.5s, ~50 files) | ✅ `-tags=live` | ✅ `.githooks/pre-commit` | ✅ `support_safety_test.go` | This document's reference impl |
| **relayLLM** | ✅ (166 tests, 1.5s) | ✅ `-tags=live` + `-tags=llm` | ✅ `.githooks/pre-commit` | partial | See its `docs/decisions/002-three-tier-testing.md` |
| **eve** | ❌ | ❌ | ❌ | ❌ | Next priority — browser frontend; needs Playwright or similar |
| **fsMCP** | partial | ❌ | ❌ | ❌ | Has some unit tests; missing sandbox + commit gate |
| **macMCP** | partial | ❌ | ❌ | ❌ | Swift suite; needs structure-level overhaul |
| **relayScheduler** | ❌ | ❌ | ❌ | ❌ |  |
| **relayTelegram** | ❌ | ❌ | ❌ | ❌ |  |

## Recommended next pickup order

1. **fsMCP** — small TypeScript surface, exercised on every relay tool
   call. Highest leverage per hour of work. Needs:
   - Vitest or Jest setup.
   - Sandbox helpers that isolate `_meta.allowed_dirs` from the host FS.
   - Cross-repo contract test that asserts its tool schema matches what
     relay's auto-disable-on-fs logic expects.
2. **eve** — Playwright tests against the frontend dispatcher. Should
   reuse relay's `FakeRelayLLMService` over an injectable backend URL.
3. **macMCP** — Swift, slowest to set up. XCTest + sandboxed FileManager
   helpers. Defer until eve is done.

## Lessons learned (relay overhaul)

Capture pitfalls here so the next repo's overhaul moves faster.

### macOS Unix-socket length cap (104 chars)
`t.TempDir()` paths on macOS look like
`/var/folders/k6/.../T/TestName1234567890/001/` — easily 80+ chars. Add a
relay-like socket name (`relay.sock`) and you blow past 104 with
`bind: invalid argument`. Always allocate socket-holding dirs via
`os.MkdirTemp("/tmp", "...")`. See `support_test.go:mkShortTempDir`.

### Sandbox guard false positives from a live tray app
If the user's actual relay tray is running while tests execute, it will
modify its log files mid-run. The naive snapshot guard flags this as
test contamination. Fix: ignore `logs/`, `run/`, and `*.sock` in the
snapshot. See `support_safety_test.go:shouldIgnoreForSafetySnapshot`.

### `sync.Mutex` in `serviceTokenStore` must not be copied
The router embeds `serviceTokenStore` by value. The service registry
takes a pointer to that same field. Wiring tests that allocate their
own `&serviceTokenStore{}` and pass it to the registry while the router
gets a copy will silently break token auth — registry writes hashes
into one map, router reads from a different one. Mirror the production
wiring (`reg.TokenStore = &router.serviceTokens`). Reference:
`service_registry_test.go:startSandboxBridge`.

### Don't add an exec.Command factory just for tests
The temptation to mock subprocess spawn so tests stay in-process is
strong, but per ADR-002 the fake has to re-implement env injection,
pidfile management, log routing, the reaper, and token cleanup — and
will drift. Instead, build a tiny real binary
(`cmd/testservice/main.go`) that exercises the production spawn path.
The integration tests then verify what actually happens, not what we
think happens.

### Bridge-server tests don't need a hand-made socket path
`bridge.NewBridgeServer` derives its socket from `bridge.SocketPath()`
which derives from `bridge.ConfigDir()`. If your test sets the
ConfigDir override (via `mkSandboxRelayHome` or
`bridge.SetConfigDirForTest`), the bridge automatically lands at the
right place. Don't construct an `sockPath` variable separately — it'll
just diverge from what the server actually binds. See
`mcp/server_test.go:startBridgeForMCP` for the right pattern.

### Cross-repo contract via committed JSON fixture
Relay's `test/fixtures/manifests/relayllm.json` is the source of truth
for what relayLLM is supposed to register. The hermetic
FakeRelayLLMService loads it; the live tier (`-tags=live`) asserts the
real binary still matches. relayLLM should add its own test that
asserts its actual generated manifest equals the same JSON file —
otherwise drift can creep in via the relayLLM side.

## Process improvements proposed (not yet adopted)

- **`make race` weekly cron** — `go test -race ./...` is too slow for
  pre-commit but cheap weekly. Worth wiring into a launchd plist or a
  GitHub Action when we add one.
- **Goroutine-leak check in `TestMain`** — diff `runtime.NumGoroutine()`
  before/after each test; fail on > 5 leaked. Caught at least one WS
  proxy leak during the dispatcher overhaul.
- **Coverage HTML on demand** — `make coverage` emits `coverage.html`.
  No threshold gate per ADR-001; gates incentivize tests-for-coverage.
- **Snapshot tests for the bridge wire format** — `bridge/testdata/*.json`
  goldens, asserted with byte-equality. Catches accidental
  field-renames before they cascade across services. Skipped from V1
  scope; revisit if we see a drift incident.
