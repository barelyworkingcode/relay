# Testing Roadmap

Cross-repo status of bringing each sibling repo up to the bar in
ADR-001 (the three-tier testing strategy). relay is the reference impl.

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
     field in the MCP schema — see `internal/project.V1AllowedDirsField`).
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

### The router and the registry must share one launch table
The registry begins launches in `service.Launches` and the router binds and
looks them up there. Wiring a test that gives each its own
`service.NewLaunches()` silently breaks identity auth — every `Hello` is
refused because the router's table never saw the launch. Mirror production:
`reg.Launches = router.launches`. See
`service_registry_test.go:startSandboxBridge`.

### A sandboxed `HOME` breaks a spawned browser, silently
`mkSandboxRelayHome`/`mkEmptySandboxRelayHome` point `HOME` at a temp dir for
the whole test process, and a Chrome spawned afterwards inherits it. Such a
Chrome starts, attaches to the DevTools Protocol and answers every command,
but **every navigation hangs before it commits** — which reads as the server
under test not answering, and is not. `webauthn_browser_live_test.go`'s
`chromeEnv` restores the account's real `HOME` for the browser only; Chrome's
own state stays in `--user-data-dir`, and relay's config dir is still
sandboxed, which is what the headline rule is about.

### No `exec.Command` factory for spawn tests
Don't mock subprocess spawn — see ADR-002
(test seams: use the real thing over a fake in-process double). Use the real
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

## Named gaps in the passkey login evidence

ADR-016 decision 8 states these rather than leaving them to be assumed away.
`webauthn_browser_live_test.go` runs one real ceremony in headless Chrome
against the real `/relay/login` document; here is what that does **not** buy.

### No real hardware authenticator
The live tier drives Chrome's virtual CTAP2 authenticator. This host is an
Apple VM with no Touch ID, no Secure Enclave and no USB passthrough, so no
genuine authenticator — platform or roaming — has ever produced an assertion
relay has seen. A virtual authenticator configured with
`isUserVerified: true` always sets the UV bit, so the suite proves relay
*checks* that bit and never that real hardware would have set it. Closing this
needs a machine with a real authenticator; no amount of software raises it.

### No Safari coverage
Safari is the only other browser on this host and the one the owner would
actually use. Its consent UI, its passkey storage in iCloud Keychain and its
own view of `localhost` as a secure context are exercised by nothing. A
Chrome-green suite is evidence about WebAuthn, not about Safari. Safari has no
DevTools-Protocol equivalent for injecting a virtual authenticator, so this
gap cannot be closed by the same mechanism and would need a driven real
authenticator — which is the first gap again.

## Not yet adopted

Tracked but unbuilt: a weekly `go test -race ./...` cron, a goroutine-leak
check in `TestMain` (diff `runtime.NumGoroutine()` before/after each test),
and byte-equality goldens for the bridge wire format. None exist today; add
if a drift or leak incident warrants it.
