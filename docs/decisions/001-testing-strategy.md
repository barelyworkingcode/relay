# ADR-001: Testing Strategy

**Status:** Accepted
**Date:** 2026-05-24

## Headline rule

**No test may read or mutate the real user config directory
(`~/Library/Application Support/relay/`).** Every test must redirect
`bridge.ConfigDir()` to a `t.TempDir()` sandbox before touching anything
that writes. The `support_safety_test.go` guard enforces this on every
test run by snapshotting the real ConfigDir's mtime before and after the
suite.

If you find yourself reading `os.UserConfigDir()` in a test, or calling a
helper that does, stop — wire it through `mkSandboxRelayHome(t)` instead.

## Context

Relay is the host application: it owns projects, MCP registrations, OAuth
tokens, ephemeral service tokens, and the bridge socket. A test that
accidentally writes to the real config dir can corrupt a developer's
workspace, leak test tokens into production settings, or block the next
launch with a stale pidfile. The cost of one such bug is hours of
debugging plus a lost workspace.

The recently-completed test-suite overhaul in `../relayLLM` (see its
ADR-002) settled on a three-tier model gated by Go build tags. We adopt
the same pattern here for consistency and because it earned its keep:
`go test ./...` is reliable, fast, and safe by default.

## Decision

### Three tiers

| Command | What runs | Requirements | When |
|---|---|---|---|
| `go test ./...` | Hermetic suite | None — pure Go, no network, no spawned services, no user files | Every commit (pre-commit hook) |
| `go test -tags=live ./...` | Integration with real `../relayLLM` binary | `../relayLLM/relayLLM` built locally | Manually before merging changes that touch the relay↔relayLLM boundary |
| `go test -race ./...` | Hermetic suite + race detector | None | Weekly, or before merging concurrency-touching changes |

The hermetic suite is the only one gated by `.githooks/pre-commit`. The
`-tags=live` and `-race` runs are opt-in because they're slower and/or
environment-dependent.

### Sandbox seam

`bridge.SetConfigDirForTest(dir)` (in `bridge/types.go`) overrides
`ConfigDir()` for the lifetime of a test. Every test that touches
settings, pidfiles, service logs, or the bridge socket must call
`mkSandboxRelayHome(t)` (in `support_test.go`) which:

1. Allocates `t.TempDir()`.
2. Recursively copies `test/fixtures/relay-home/` into it (read-write).
3. Calls `bridge.SetConfigDirForTest(dir)`.
4. Sets `HOME` and `XDG_CONFIG_HOME` to the tempdir as defense-in-depth.
5. Registers a `t.Cleanup` that restores the override to "".

Same seam is exposed in production as `--config-dir <path>`. The CLI flag
enables multi-instance use and powers `scripts/demo.sh`. ADR-002 explains
why test-only seams that *also* improve production are doubly welcome.

### Fixture-driven tests

Test code should not hand-construct realistic settings JSON. Instead, the
sandbox helper copies a fully-populated install from
`test/fixtures/relay-home/` and tests mutate the copy. The same fixture
tree is the demo/screenshot harness — see ADR-003.

## Consequences

- **Good:** `go test ./...` always runs in <3s on a fresh checkout with no
  external services, no compiled relayLLM, and an empty user ConfigDir.
- **Good:** Sandbox safety is enforced by code (`support_safety_test.go`),
  not just by convention.
- **Good:** Live tier preserves the value of real-binary integration
  testing without bleeding into the default suite.
- **Bad:** Two tiers means two things to remember. Mitigated by the
  "Adding a test" guide in `CLAUDE.md`.
- **Trade-off:** The hermetic FakeRelayLLMService can drift from real
  relayLLM behavior. Drift is caught by the live tier (run manually) and
  by a cross-repo contract test in `../relayLLM` that asserts its actual
  manifest equals the JSON loaded by our fake.

## When to add a test in which tier

- **Default tier:** anything that can be exercised in-process with the
  TestServer / FakeService / FakeBridge helpers. Aim for ~95% of tests.
- **`-tags=live` tier:** anything that needs the real `../relayLLM`
  binary, real subprocess lifecycle observation, or real WS upgrades
  against the real upstream protocol. Document why the default tier
  isn't sufficient in the test file's header comment.

## See also

- ADR-002 — production test seams (ConfigDir override, etc.)
- ADR-003 — fixture layout and content guidelines
- `.githooks/pre-commit` — enforcement
- `support_test.go`, `support_safety_test.go` — test infrastructure
- `CLAUDE.md` — "Testing" and "Adding a test" sections
