# ADR-002: Production Test Seams

**Status:** Accepted
**Date:** 2026-05-24

## Context

The hermetic test tier (ADR-001) needs to substitute the config directory
location, the bridge socket path, and the upstream service registry —
without bringing in a dependency-injection framework and without
exposing test machinery in the public API.

`../relayLLM`'s ADR-003 settled on small `Set*` setters with "test-only"
comments. We adopt the same pattern.

## Decision

Three narrow seams, all minimal:

1. **`bridge.SetConfigDirForTest(dir)` / `bridge.SetConfigDir(dir)`** —
   redirects `bridge.ConfigDir()` (and therefore `bridge.SocketPath()`,
   pidfile paths, log file paths, settings.json location) to `dir`. The
   test variant is paired with a production variant of the same name
   driven by the `--config-dir` CLI flag. Setting both names through one
   variable keeps the override invisible at production callsites that
   never set it.

2. **`SettingsStore` interface + `NewSettingsStoreAt(dir)`** — already
   existed before this overhaul (`settings_store.go:47`). Tests that need
   to assert on persisted state pass a tempdir directly instead of
   relying on the global ConfigDir.

3. **`enhancedServiceRegistry` interface (planned, on-demand)** — the
   front-door dispatcher currently uses `*EnhancedServiceRegistry`
   concretely. If a dispatcher test grows to need a stub registry, we
   introduce an interface at the dispatcher boundary, not earlier. The
   default tier's `FakeService` helper registers manifests through the
   real registry, which is fine for almost every test.

## What we deliberately did NOT do

- **No `exec.Command` factory** for `service_registry.go`. Spawn logic
  (env injection, pidfile, log file, reaper, token cleanup) is too
  security-sensitive to test through a fake — the fake would have to
  re-implement all of it and would drift. Instead we build a real
  in-tree binary (`cmd/testservice/main.go`) that exercises the actual
  spawn path. Mirrors relayLLM ADR-003 exactly.

- **No clock injection.** Status poller intervals are tunable via
  existing exported fields; tests that need determinism use small
  intervals + polling, not a fake clock. The 60s permission timeout
  pattern from relayLLM doesn't apply here (relay doesn't own user-
  interactive timeouts).

- **No mock for `bridge.NewBridgeServer` or `NewBridgeClient`.** Real
  server + real client over a `t.TempDir()` socket is fast enough and
  more honest.

## Criteria for adding a new seam

All three must hold:

1. A planned test in the default tier needs to control this piece.
2. The seam is a small interface or setter, not a DI rewrite.
3. The production code shape is no worse — and ideally better — than
   without it. (`--config-dir` qualifies: it both unlocks tests and is a
   useful production CLI flag.)

If you can't meet all three, the test belongs in the `-tags=live` tier.

## Consequences

- **Good:** Production code stays close to its non-test shape. The seams
  are easy to spot (`Set*ForTest`).
- **Good:** `--config-dir` is a real CLI feature, not test scaffolding.
- **Trade-off:** A misused `SetConfigDirForTest` (set but not cleaned up)
  bleeds into other tests in the same package. `mkSandboxRelayHome(t)`
  registers `t.Cleanup` automatically — always call it via the helper,
  never directly.

## See also

- ADR-001 — three-tier testing strategy
- `bridge/types.go` — `SetConfigDir`, `SetConfigDirForTest`, `ConfigDir`
- `settings_store.go` — `NewSettingsStoreAt`
- `support_test.go` — `mkSandboxRelayHome` (the canonical entrypoint)
- `cmd/testservice/main.go` — in-tree real binary for spawn tests
