# Testing Roadmap

Cross-repo status of bringing each sibling repo up to the bar in
[ADR-001](decisions/001-testing-strategy.md). relay is the reference impl.

Headline rule everywhere: **no test may touch the user's real config
directory** (full rationale in ADR-001). Each repo enforces it differently;
record the approach here when you add coverage to a new one.

## Status by repo

| Repo | Default tier | Live tier | Pre-commit gate | Sandbox guard |
|---|---|---|---|---|
| **relay** | yes (~1.5s) | yes (`-tags=live`) | yes (`.githooks/pre-commit`) | yes (`support_safety_test.go`) |
| **relayLLM** | yes | yes (`-tags=live` + `-tags=llm`) | yes (`.githooks/pre-commit`) | partial |
| **eve** | yes (Jest) + e2e (Playwright) | no | no | no |
| **relayScheduler** | partial (`client_test.go`) | no | no | no |
| **macMCP** | none | no | no | no |
| **fsMCP** | none | no | no | no |
| **relayTelegram** | none | no | no | no |

relayLLM's tier model is documented in its
`docs/decisions/002-three-tier-testing.md`.

## Recommended next pickup order

1. **fsMCP** — no harness yet, smallest TypeScript surface, exercised on
   every relay tool call. Highest leverage per hour. Needs:
   - Vitest or Jest setup.
   - Sandbox helpers that isolate `_meta.allowed_dirs` from the host FS.
   - Cross-repo contract test asserting its tool schema matches relay's
     auto-disable-on-fs expectation (relay keys off the `allowed_dirs`
     field in the MCP schema — see `settings.go:398`).
2. **eve** — already has Jest unit + Playwright e2e; finish the standard:
   wire a pre-commit gate, add a sandbox guard, and reuse relay's
   `FakeRelayLLMService` over an injectable backend URL in the e2e tier.
3. **relayScheduler** — extend beyond `client_test.go` to a full tier +
   pre-commit gate.
4. **macMCP** — Swift, slowest to set up (XCTest + sandboxed FileManager).
   Defer until the others are done.

## Lessons learned (relay overhaul)

Pitfalls so the next repo's overhaul moves faster.

### macOS Unix-socket length cap (104 chars)
`t.TempDir()` paths on macOS look like
`/var/folders/k6/.../T/TestName1234567890/001/` — easily 80+ chars. Add a
socket name (`relay.sock`) and you blow past 104 with
`bind: invalid argument`. Allocate socket-holding dirs via
`os.MkdirTemp("/tmp", "...")`. See `support_test.go:mkShortTempDir`.

### Sandbox guard false positives from a live tray app
If the user's actual relay tray is running while tests execute, it
modifies its log files mid-run and the naive snapshot guard flags it as
contamination. Fix: ignore `logs/`, `run/`, and `*.sock` in the snapshot.
See `support_safety_test.go:shouldIgnoreForSafetySnapshot`.

### `sync.Mutex` in `serviceTokenStore` must not be copied
The router embeds `serviceTokenStore` by value (`router.go:71`); the
service registry takes a pointer to that same field. Wiring tests that
allocate their own `&serviceTokenStore{}` and pass it to the registry
while the router keeps a copy silently break token auth — registry writes
hashes into one map, router reads from another. Mirror production:
`reg.TokenStore = &router.serviceTokens`. See
`service_registry_test.go:startSandboxBridge`.

### No `exec.Command` factory for spawn tests
Don't mock subprocess spawn — see ADR-002
([002-test-seams.md](decisions/002-test-seams.md)). Use the real
`cmd/testservice/main.go` binary so tests exercise the production spawn
path (env injection, pidfile, log routing, reaper, token cleanup).

### Bridge-server tests don't need a hand-made socket path
`bridge.NewBridgeServer` derives its socket from `bridge.SocketPath()` →
`bridge.ConfigDir()`. If the test sets the ConfigDir override (via
`mkSandboxRelayHome` or `bridge.SetConfigDirForTest`), the bridge lands at
the right place automatically. Don't construct a separate `sockPath` — it
diverges from what the server binds. See
`mcp/server_test.go:startBridgeForMCP`.

### Cross-repo contract via committed JSON fixture
`test/fixtures/manifests/relayllm.json` is the source of truth for what
relayLLM registers. The hermetic `FakeRelayLLMService` loads it; the live
tier (`-tags=live`) asserts the real binary still matches. relayLLM should
add a test asserting its generated manifest equals this file, or drift can
creep in from the relayLLM side.

## Not yet adopted

Tracked but unbuilt: a weekly `go test -race ./...` cron, a goroutine-leak
check in `TestMain` (diff `runtime.NumGoroutine()` before/after each test),
and byte-equality goldens for the bridge wire format. None exist today; add
if a drift or leak incident warrants it.
