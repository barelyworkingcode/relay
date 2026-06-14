# ADR-001: Testing Strategy

**Status:** Accepted
**Date:** 2026-05-24

## Headline rule

**No test may read or mutate the real user config directory
(`~/Library/Application Support/relay/`).** Every test that touches
settings, pidfiles, logs, or the bridge socket must redirect
`bridge.ConfigDir()` to a sandbox by calling `mkSandboxRelayHome(t)`
before doing anything that writes. The `support_safety_test.go` guard
enforces this suite-wide: it fingerprints the real ConfigDir tree (entry
set + mtimes, ignoring `logs/`, `run/`, and sockets) before and after the
run and fails if anything changed.

If you find yourself reading `os.UserConfigDir()` in a test, or calling a
helper that does, stop — wire it through `mkSandboxRelayHome(t)` instead.

## Context

Relay is the host application: it owns projects, MCP registrations, OAuth
tokens, ephemeral service tokens, and the bridge socket. A test that
accidentally writes to the real config dir can corrupt a developer's
workspace, leak test tokens into production settings, or block the next
launch with a stale pidfile.

The test-suite overhaul in `../relayLLM` (its ADR-002, "Three-Tier
Testing") settled on a three-tier model gated by Go build tags. We adopt
the same pattern here for consistency: `go test ./...` is reliable, fast,
and safe by default.

## Decision

### Three tiers

| Command | What runs | Requirements | When |
|---|---|---|---|
| `go test ./...` | Hermetic suite | None — pure Go, no network, no spawned services, no user files | Every commit (`.githooks/pre-commit`) |
| `go test -tags=live ./...` | Integration with the real `../relayLLM` binary | `../relayLLM/relayLLM` built locally | Manually, before merging changes to the relay↔relayLLM boundary |
| `go test -race ./...` | Hermetic suite + race detector | None | Enforced by `.githooks/pre-push` |

Only the hermetic suite gates commits. `-race` runs at push time (slower,
kept off the commit path). `-tags=live` is fully manual — it depends on a
locally-built upstream binary.

### Sandbox seam

`mkSandboxRelayHome(t)` (in `support_test.go`) is the canonical entry
point: it seeds a fresh sandbox from `test/fixtures/relay-home/`, points
`bridge.ConfigDir()` at it via `bridge.SetConfigDirForTest`, sets
`HOME`/`XDG_CONFIG_HOME` as defense-in-depth, and restores the override on
cleanup. The sandbox lives under `/tmp`, not `t.TempDir()` — macOS caps
Unix-socket paths at 104 chars and the bridge socket lives inside
ConfigDir, so a typical `t.TempDir()` path already overflows.

The same override is exposed in production as `--config-dir <path>`. The
flag enables multi-instance use and powers `scripts/demo.sh`. ADR-002
explains why test-only seams that also improve production are doubly
welcome.

### Fixture-driven tests

Test code should not hand-construct realistic settings JSON. The sandbox
helper copies a fully-populated install from `test/fixtures/relay-home/`
and tests mutate the copy. The same fixture tree backs the demo/screenshot
harness — see ADR-003.

## Consequences

- **Good:** `go test ./...` runs hermetically with no external services,
  no compiled relayLLM, and an empty user ConfigDir.
- **Good:** Sandbox safety is enforced by code (`support_safety_test.go`),
  not convention.
- **Good:** The live tier preserves real-binary integration testing
  without bleeding into the default suite.
- **Bad:** Three tiers means more to remember. Mitigated by the "Adding a
  test" guide in `CLAUDE.md`.
- **Trade-off:** The hermetic `FakeRelayLLMService` loads a hand-maintained
  fixture (`test/fixtures/manifests/relayllm.json`) that can drift from
  real relayLLM behavior. There is **no** automated cross-repo check —
  relayLLM has its own golden route-table test
  (`../relayLLM/manifest_test.go`), but nothing asserts it against our
  fixture. Update the fixture by hand when relayLLM adds a route, and
  catch drift via the live tier. (The fixture is currently missing
  `/api/mlx/` — fix when next touched.)

## See also

- ADR-002 — production test seams (ConfigDir override, etc.)
- ADR-003 — fixture layout and content guidelines
- `.githooks/pre-commit`, `.githooks/pre-push` — enforcement
- `support_test.go`, `support_safety_test.go` — test infrastructure
- `CLAUDE.md` — "Testing" and "Adding a test" sections (tier selection,
  the ~95% default-tier rule, helper inventory)