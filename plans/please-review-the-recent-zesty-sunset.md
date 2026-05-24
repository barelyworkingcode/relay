# Relay Test Suite Overhaul — Plan

## Context

Relay's test suite has solid pockets (auth, settings, projects, external MCP, OAuth — 17 files / ~4.4k LOC / ~40% coverage) but no gating mechanism, no integration harness, and large untested surfaces around the bridge, frontend dispatcher, service lifecycle, and enhanced-services registry. The recent overhaul in `../relayLLM` produced a reliable three-tier model (default hermetic / `-tags=live` / `-tags=llm`) gated by a pre-commit hook. That model is the target for relay.

Goal: bring relay to the same quality bar so the host application is regression-safe as we hand it off, then mirror the pattern out to eve / fsMCP / macMCP in follow-on work.

**Hard constraint (user-stated):** No test may read or mutate the real user config directory (`~/Library/Application Support/relay/`). Every test path must be redirected to a `t.TempDir()` sandbox; we will add an explicit guard and the constraint will be the first sentence of the testing standards doc.

## Goals

1. Default `go test ./...` is hermetic, fast (target <3s), and runs on every commit via `.githooks/pre-commit`.
2. Coverage gaps closed in priority order: security/auth, bridge IPC, frontend dispatch, service lifecycle, IPC handlers.
3. Relay↔relayLLM integration validated at two tiers: a hermetic `FakeRelayLLMService` in the default suite, and `-tags=live` tests that spawn the real `../relayLLM` binary.
4. Committed fixture set under `test/fixtures/relay-home/` provides realistic multi-project sandboxes (skills, CLAUDE.md, sample prompts, sample tool-call JSON) so tests look like real installs without touching the user's.
5. **Same fixture set doubles as a demo/screenshot harness** — a `./scripts/demo.sh` launches relay pointed at the fixture set (read-write copy in `/tmp`) so we can produce reproducible screenshots and screencasts of a populated, working install without exposing real data.
6. ADR-style decision records under `docs/decisions/` so the next agent inherits the rationale, not just the code.

## Non-goals

- Coverage threshold gates (relayLLM doesn't have them; gates lead to test-for-coverage).
- Testing Cocoa tray UI or `platform.Run()` — wire-up below tray is already headless-testable.
- Overhauling eve/fsMCP/macMCP test suites — captured in `docs/testing-roadmap.md` for later.

---

## Phase 1 — Foundation & sandbox safety

**1.1 Add the ConfigDir override seam** (load-bearing — without this every test risks the real `~/Library/Application Support/relay`)

Edit `bridge/types.go`: change `ConfigDir()` to honor an internal override.

```go
var configDirOverride string

// SetConfigDirForTest redirects ConfigDir() to dir. Test-only; call t.Cleanup
// with SetConfigDirForTest("") to restore.
func SetConfigDirForTest(dir string) { configDirOverride = dir }

func ConfigDir() string {
    if configDirOverride != "" { return configDirOverride }
    // ...existing body
}
```

Audit every callsite of `bridge.ConfigDir()` and `bridge.SocketPath()` — anything that writes (pidfiles in `service_pidfile.go`, log files in `service_registry.go`, settings in `settings_store.go`) must route through it.

**1.2 Pre-commit hook** — copy `../relayLLM/.githooks/pre-commit` to `relay/.githooks/pre-commit`. Same body: `go build ./... && go vet ./... && go test ./...`. Install instructions in CLAUDE.md.

**1.3 ADR docs** under `docs/decisions/`:
- `000-readme.md` — ADR index
- `001-testing-strategy.md` — three-tier model, hermetic-first, **"no test touches the real user ConfigDir"** as the headline rule
- `002-test-seams.md` — narrow seams added (ConfigDir override + any others). Mirrors relayLLM's ADR-003 criteria
- `003-fixture-layout.md` — what's under `test/fixtures/` and how to extend it

**1.4 CLAUDE.md testing section** — add "Testing" + "Adding a test" + "What's NOT tested" (Cocoa UI, real launchd, real OAuth callbacks).

**1.5 Sandbox-leak guard** — `support_safety_test.go` runs in `TestMain` and fails the suite if anything in the real `bridge.ConfigDir()` was modified during the run (record mtime of the dir before and after; allow not-exists).

---

## Phase 2 — Sandbox infrastructure

**2.1 Committed fixture tree** at `test/fixtures/relay-home/` — checked in, never mutated by tests (tests copy into tempdir). Same tree is the demo/screenshot harness, so projects are named and populated to look like a real install (no `proj-alpha` placeholders, no PII, no real tokens, no machine-specific paths).

```
test/fixtures/relay-home/
├── settings.json                       # 3 projects, 3 MCPs, 2 service configs — populated but generic
├── projects/
│   ├── acme-website/                   # web app — broad MCP access, full toolset
│   │   ├── CLAUDE.md                   # written like a real onboarding doc
│   │   ├── README.md
│   │   ├── skills/
│   │   │   ├── deploy/SKILL.md
│   │   │   └── triage-issue/SKILL.md
│   │   ├── src/{main.go,handlers.go}   # small but real-looking code for fs MCP path tests
│   │   └── docs/architecture.md
│   ├── field-notes/                    # knowledge/notes project — restricted allowed_mcp_ids
│   │   ├── CLAUDE.md
│   │   ├── 2026-Q1/{meeting-notes.md,research.md}
│   │   └── inbox/ideas.md
│   └── greenhouse-monitor/             # IoT / hardware — service-driven, narrow toolset
│       ├── CLAUDE.md
│       ├── firmware/main.c
│       └── schematics.md
└── samples/                            # canonical inputs for tests + demo replays
    ├── prompts/
    │   ├── simple.txt
    │   ├── tool-call-read-file.txt
    │   └── multi-step-deploy.txt       # used in screencasts
    ├── tool-calls/                     # canonical MCP JSON-RPC requests/responses
    │   ├── tools-list.json
    │   ├── read-file.{req,resp}.json
    │   └── search-files.{req,resp}.json
    └── screencast-scripts/             # ordered prompt sequences for reproducible demos
        ├── tour-the-tray.md
        └── permission-flow-demo.md
```

Content guidelines (enforced in `docs/decisions/003-fixture-layout.md`):
- No real names, emails, tokens, hostnames, or repo paths.
- Generic but recognizable domains (a website, a notes vault, an IoT project) so screenshots feel real, not placeholder-y.
- Skills and CLAUDE.md files written as if onboarding a new developer — readable on a screen capture, no lorem ipsum.
- Tokens in `settings.json` are obvious test sentinels (`test-token-acme-website-do-not-use`) so a screenshot can never accidentally leak a real one.

**2.1.b Demo / screenshot harness** — `scripts/demo.sh`:
- Copies `test/fixtures/relay-home/` to `/tmp/relay-demo-home/` (writable, isolated).
- Exports `RELAY_DEMO_HOME=/tmp/relay-demo-home` and launches relay with `bridge.SetConfigDirForTest`-equivalent CLI flag (`relay --config-dir`, new — see 2.1.c).
- `--reset` flag wipes and recopies for a clean state.
- `--scenario <name>` swaps in a scenario-specific overlay from `test/fixtures/scenarios/` (e.g., `services-running`, `permission-prompt-active`, `fresh-install`) so screencasts can capture specific states deterministically.
- Output README documents recording recipes (recommended Cleanshot/QuickTime presets, window sizes) so screenshots stay visually consistent across captures.

**2.1.c Production CLI flag** — `relay --config-dir <path>` (small addition to `main.go`): same code path as `SetConfigDirForTest` but user-facing. Useful for the demo harness, for power users running multiple relay instances, and as a graceful production surface for the test seam (mirrors relayLLM ADR-003's "seam should not make production worse" criterion — this one actually makes it better).

**2.2 Test support package** — new `support_test.go` (same-package, not `_test/` dir, matching relayLLM):
- `mkSandboxRelayHome(t)` — `t.TempDir()` + recursive copy of `test/fixtures/relay-home/` + `bridge.SetConfigDirForTest(dir)` + `t.Setenv("HOME", dir)` defense-in-depth + `t.Cleanup` restore.
- `newTestRouter(t)` — returns a fully wired `appRouter` against the sandbox (already partly exists; consolidate).
- `newTestBridge(t)` — in-process `bridge.NewBridgeServer` on a `t.TempDir()` socket; returns server + a `bridge.NewBridgeClient` connected to it.
- `FakeService(t, manifest)` — registers a manifest, serves a stub HTTP+WS handler on a tempdir Unix socket, records inbound requests for assertion.
- `FakeRelayLLMService(t)` — specialization of FakeService preloaded with relayLLM's known route set (loaded from a shared `test/fixtures/manifests/relayllm.json` that doubles as the cross-repo contract — relayLLM's own tests can assert their actual manifest matches).
- Assertion helpers: `assertManifestRoutes(t, ...)`, `assertBridgeRoundTrip(t, ...)`, `dialUnix(t, path)`.

**2.3 In-tree test service binary** at `cmd/testservice/main.go` — minimal Go program that reads `RELAY_BRIDGE_SOCKET` + `RELAY_SERVICE_ID`, registers a manifest, optionally serves a status endpoint, exits on SIGTERM. Built via `go build` in `TestMain` and used by `service_registry_test.go`. **Avoids adding an exec.Command factory seam** (per relayLLM ADR-003 — too much production complexity, would have to replicate env injection / pidfile / log routing logic in a fake).

---

## Phase 3 — Coverage gaps (security + IPC + lifecycle first)

**3.1 Bridge wire protocol** — new `bridge/contract_test.go`:
- Real `BridgeServer` ↔ real `BridgeClient` over `t.TempDir()` socket, exercising every request type in `bridge/types.go`.
- Golden files under `bridge/testdata/` — canonical JSON for each request/response shape. Catches accidental wire-format breaks that would silently desync external services.

**3.2 Frontend dispatcher** — new `frontend_dispatcher_test.go`:
- Longest-prefix-match correctness (incl. tie-breaking, trailing-slash, query-string).
- WS upgrade dispatched correctly — use `httptest.NewServer` inbound + real `net.Listen("unix")` upstream (the Hijack path requires real `http.ResponseWriter`, not in-process synthesis).
- Bearer token enforcement (missing / wrong / right).
- Inbound `Authorization` header stripped and service-declared token injected upstream.
- `forwardDispatchedWS` goroutine pair under `-race`; assert no goroutine leak (count diff before/after).

**3.3 Frontend server** — new `frontend_server_test.go`:
- Socket created with 0600 perms.
- Bearer rejection.
- Unknown route → 404.

**3.4 Enhanced services registry** — new `enhanced_services_test.go`:
- RegisterManifest CRUD; route-conflict detection on re-registration with overlapping prefixes.
- ReverseProxy cache lifecycle; eviction on `Forget`.
- `onChange` callback fires exactly once per logical event (regression-prone).

**3.5 Service lifecycle** — new `service_registry_test.go` (uses `cmd/testservice` binary):
- Spawn → child receives `RELAY_BRIDGE_SOCKET` + `RELAY_SERVICE_ID` env vars.
- Restart in place; pidfile updated.
- Orphan reclaim on fresh start (write a stale pidfile pointing to live `cmd/testservice`, verify reclaim).
- StopAll completes before test returns (helper must `<-proc.done` per process — avoids tempdir-cleanup race).

**3.6 MCP stdio server** — new `mcp/server_test.go`:
- Initialize / tools/list / tools/call round-trip against the in-process test bridge.

**3.7 IPC handlers** — new `ipc_handlers_test.go`, expand `ipc_mcps_test.go` / `ipc_services_test.go`:
- Auth checks for every IPC message type.
- Malformed payload rejection.

**3.8 Security regression suite** — new `security_regression_test.go` (single file, single audit surface):
- Service-token cannot be used as a project-token (and vice-versa).
- Manifest action whitelist bypass attempts (path traversal in `pathTemplate`, undeclared actions).
- Longest-prefix path-matching ambiguity (e.g., `/api/sessions` vs `/api/sessions-export`).
- Frontend bearer rejection on every path (HTTP + WS).
- Settings.json with no Version field (downgrade attack) — should reject or migrate, not silently accept.
- Inbound Authorization header never leaks to upstream service.
- Project token rotation invalidates prior token immediately.

Each test names the scenario like a CVE so future audits scan one file.

---

## Phase 4 — Relay ↔ relayLLM integration

**4.1 Hermetic (default tier)** — `integration_fake_relayllm_test.go`:
- Spin up `newTestBridge` + `FakeRelayLLMService` + `newTestRouter`.
- Assert: relay dispatches `/api/sessions/...`, `/api/models`, `/ws` to the fake's socket; injects declared bearer; preserves request body.
- Manifest contract: `FakeRelayLLMService` loads its routes from `test/fixtures/manifests/relayllm.json`. Drift mitigation: relayLLM repo gets a single test that asserts its actual generated manifest equals this file.

**4.2 Live tier** — `integration_live_relayllm_test.go` with `//go:build live`:
- Locate `../relayLLM/relayLLM` binary; `t.Skip` if not built.
- Start headless relay (router + bridge + frontend, no tray) in the test.
- Spawn relayLLM via `service_registry`; wait for manifest registration; query `/api/models` end-to-end; assert real response shape.
- Document in `docs/decisions/001-testing-strategy.md`: when to run (manually after relay or relayLLM changes that touch the boundary).

---

## Phase 5 — Wire-up

**5.1** `build.sh` — add `--test` flag that runs `go build ./... && go vet ./... && go test ./...` before installing. Failure aborts install.

**5.2** Install instructions in CLAUDE.md: `git config core.hooksPath .githooks` + `go test ./...` + `go test -tags=live ./...`.

**5.3** `docs/testing-roadmap.md` — captures lessons-learned, recommended order for next services (eve / fsMCP / macMCP), per-project notes section so each handoff records gotchas.

**5.4 Additional improvements worth adopting** (proposed):
- `make race` target (or `.githooks/pre-push`) running `go test -race ./...` weekly. Relay has more concurrent state than relayLLM (service reaper, status poller atomics, registry mutex), so race detection matters more here — but it's too slow for every commit.
- `make coverage` emits `coverage.html`; record current baseline number in CLAUDE.md (no gate).
- Goroutine-leak check in `TestMain` (count before / after) — catches reaper and WS-proxy leaks early.

---

## Critical files to modify or create

**New:**
- `.githooks/pre-commit`
- `docs/decisions/000-readme.md`, `001-testing-strategy.md`, `002-test-seams.md`, `003-fixture-layout.md`
- `docs/testing-roadmap.md`
- `test/fixtures/relay-home/...` (tree shown in 2.1)
- `test/fixtures/scenarios/...` (demo overlays)
- `test/fixtures/manifests/relayllm.json`
- `scripts/demo.sh` (+ recording-recipes README)
- `cmd/testservice/main.go`
- `support_test.go`, `support_safety_test.go`
- `bridge/contract_test.go`, `bridge/testdata/*.json`
- `frontend_dispatcher_test.go`, `frontend_server_test.go`
- `enhanced_services_test.go`, `service_registry_test.go`
- `mcp/server_test.go`
- `ipc_handlers_test.go`
- `security_regression_test.go`
- `integration_fake_relayllm_test.go`, `integration_live_relayllm_test.go`

**Modified:**
- `bridge/types.go` — add `SetConfigDirForTest` + override-aware `ConfigDir`
- `main.go` — `--config-dir` flag (Phase 2.1.c)
- `CLAUDE.md` — Testing + Adding a test + What's NOT tested + Demo harness pointer
- `build.sh` — `--test` flag
- (Possibly) `ipc_mcps.go` / `ipc_services.go` test files — expand existing

## Reused existing primitives

- `SettingsStore` interface + `NewSettingsStoreAt(dir)` (`settings_store.go:47`) — already a clean seam, just needs to be used everywhere
- `mockMcpConn` + helpers (`mock_mcp_test.go`) — keep as-is; complementary to new fakes
- `bridge.NewScanner`, `bridge.MaxMessageSize` — reused by contract tests
- Existing test files (router_test, settings_test, project_test, etc.) — left alone; new infra is additive

## Verification

End-to-end checks after implementation:

1. **Sandbox safety:** Run `go test ./...` with `~/Library/Application Support/relay/` snapshot (`stat -f%m` mtime before/after); assert unchanged. `support_safety_test.go` enforces this on every run.
2. **Hermetic default tier:** `go test ./...` passes in under 3s on a machine with no relayLLM / no services running, on a fresh checkout with no `~/Library/Application Support/relay/` directory.
3. **Pre-commit gate:** `git commit` on a touched Go file runs build / vet / test; failure aborts.
4. **Live tier:** with `../relayLLM` built, `go test -tags=live ./...` spawns relayLLM, completes manifest handshake, and a real `/api/models` round-trip succeeds.
5. **Race-clean:** `go test -race ./...` is clean.
6. **Security regression:** every test in `security_regression_test.go` passes and would fail if the relevant guard were removed (verify by temporarily breaking each guard).
7. **Coverage delta:** `go test -coverprofile=...` shows coverage rising on `bridge/`, `frontend_dispatcher.go`, `enhanced_services.go`, `service_registry.go`, `mcp/`, IPC handlers.
8. **Demo harness:** `./scripts/demo.sh --reset` launches relay against `/tmp/relay-demo-home/`; tray shows three populated projects; `--scenario services-running` captures a state with services live for screencast. Confirm the real `~/Library/Application Support/relay/` is untouched after running.
