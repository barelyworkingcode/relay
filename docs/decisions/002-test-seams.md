# ADR-002: Production Test Seams

**Status:** Accepted
**Date:** 2026-05-24

## Context

The hermetic test tier (ADR-001) needs to substitute the config-directory
location, the bridge socket path, and the upstream service registry — without
a dependency-injection framework and without exposing test machinery in the
public API.

`../relayLLM`'s ADR-003 settled on small `Set*` setters with "test-only"
comments. We adopt the same pattern.

## Decision

Three narrow seams, all minimal:

1. **`bridge.SetConfigDirForTest(dir)` / `bridge.SetConfigDir(dir)`** —
   redirect `bridge.ConfigDir()` (and therefore `SocketPath()`, pidfile, log,
   and settings.json locations) to `dir`. Both names set the same package
   variable: the test variant is paired with a production variant driven by
   the `--config-dir` CLI flag, so the override stays invisible at production
   callsites that never set it.

2. **`SettingsStore` interface + `NewSettingsStoreAt(dir)`** — predates this
   overhaul. Tests that assert on persisted state pass a tempdir directly
   instead of relying on the global ConfigDir.

3. **`enhancedServiceRegistry` interface (planned, on-demand)** — the
   front-door dispatcher uses `*EnhancedServiceRegistry` concretely. If a
   dispatcher test grows to need a stub registry, we introduce an interface at
   the dispatcher boundary, not earlier. In the default tier, `NewFakeService`
   supplies a fake service's `ServiceID`/`Socket`/`Token`/`Manifest` and tests
   register it through the real `EnhancedServiceRegistry`
   (`registry.RegisterManifest(...)`), which suffices for almost every test.

## What we deliberately did NOT do

- **No `exec.Command` factory** for `service_registry.go`. Spawn logic (env
  injection, pidfile, log file, reaper, token cleanup) is too security-
  sensitive to fake — a fake would re-implement all of it and drift. Instead
  we build a real in-tree binary (`cmd/testservice/main.go`) that exercises
  the actual spawn path. Mirrors relayLLM ADR-003.

- **No clock injection.** The status-poll cadence is a fixed const
  (`StatusPollInterval`, `timeouts.go`) consumed only by the tray ticker.
  Poller tests skip the ticker entirely and call the `pollServiceStatuses`
  fan-out directly with a real registry, so no clock or interval seam is
  needed. The 60s permission-timeout clock seam from relayLLM doesn't apply
  here — relay owns no user-interactive timeouts.

- **No mock for `bridge.NewBridgeServer` or `bridge.NewClient`.** A real
  server + real client over a `t.TempDir()` socket is fast enough and more
  honest.

## Criteria for adding a new seam

All three must hold:

1. A planned default-tier test needs to control this piece.
2. The seam is a small interface or setter, not a DI rewrite.
3. The production code shape is no worse — ideally better — than without it.
   (`--config-dir` qualifies: it unlocks tests and is a useful production CLI
   flag.)

If you can't meet all three, the test belongs in the `-tags=live` tier.

## Consequences

- **Good:** Production code stays close to its non-test shape, and the seams
  are easy to spot (`Set*ForTest`).
- **Good:** `--config-dir` is a real CLI feature, not test scaffolding.
- **Trade-off:** A misused `SetConfigDirForTest` (set but not cleaned up)
  bleeds into other tests in the same package. `mkSandboxRelayHome(t)`
  registers `t.Cleanup` automatically — always go through the helper.

## See also

- ADR-001 — three-tier testing strategy
- `bridge/types.go` — `SetConfigDir`, `SetConfigDirForTest`, `ConfigDir`
- `settings_store.go` — `NewSettingsStoreAt`
- `support_test.go` — `mkSandboxRelayHome` (the canonical entrypoint)
- `cmd/testservice/main.go` — in-tree real binary for spawn tests
