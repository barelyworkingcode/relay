# Testing

**Headline rule:** no test may read or mutate the real user config directory
(`~/Library/Application Support/relay/`). Tests route through
`mkSandboxRelayHome(t)` (in `support_test.go`), which redirects
`bridge.ConfigDir()` to a per-test temp dir under `/tmp` (via `mkShortTempDir`,
which sidesteps the 104-char Unix-socket path limit) populated from
`test/fixtures/relay-home/`. The `support_safety_test.go` guard fails the suite
if anything in the real ConfigDir changes during a run. The suite expects
relay stopped: a running instance legitimately rewrites `settings.json` there
on its own schedule and will trip this guard for a reason that has nothing to
do with the code under test.

## Config dir isolation

Every package, not just `cmd/relay`, keeps its tests off the real config dir.
Outside `cmd/relay`, a package does it once in `TestMain`: point `HOME` (and
`XDG_CONFIG_HOME`) at a temp dir with `os.Setenv`, as
`internal/service/main_test.go` does. A package can also call
`bridge.SetConfigDirForTest` instead, as `internal/sshhost` does. `cmd/relay`
isolates per test with `mkSandboxRelayHome` and relies on its end-of-run
guard. The path getters (`bridge.ConfigDir`, `SocketPath`, `ModelSocketPath`)
only compute paths. The code that binds or writes creates the directory.

`internal/bridge/home_isolation_gate_test.go` enforces this. It finds every
package with tests whose source mentions `bridge.ConfigDir(`,
`bridge.SocketPath(`, `bridge.ModelSocketPath(` or `os.UserConfigDir(`, and
requires a `TestMain` that calls `os.Setenv("HOME"`. The exemptions
(`cmd/relay`, `internal/sshhost`) are a named list, each with its reason. The
gate also fails if it matches no packages, so a broken scan can't pass. Its
blind spot: it reads source text, so it can't see a package that reaches these
helpers only through another package. Such a package still needs its own
`TestMain` isolation. Nothing enforces that.

## Three tiers

| Command | What runs | When |
|---|---|---|
| `go test ./...` | Hermetic suite — pure Go, no spawned binaries, no user files | Every push (pre-push hook); every PR and `main` (CI) |
| `go test -tags=live ./...` | Spawns real binaries end-to-end: the `../relayLLM` binary, and a headless Google Chrome that runs the passkey login ceremony against the real `/relay/login` document (`webauthn_browser_live_test.go`) | By hand: after relay↔relayLLM boundary changes; after any change to the WebAuthn verifier, the login routes or the login page |
| `go test -race ./...` | Hermetic suite + race detector | Every PR and `main` (CI); locally on the package you touched when changing anything concurrent |

## What gates what

| Stage | Runs | Where |
|---|---|---|
| commit | `gofmt -l`, `go build ./...`, `go vet ./...` | `.githooks/pre-commit` |
| push | `go test ./...`, skipped when the pushed commits touch no Go sources, fixtures or web assets | `.githooks/pre-push` |
| PR, and every push to `main` | `gofmt`, `go vet`, `go test ./...` and `go test -race ./...` as two parallel jobs | `.github/workflows/ci.yml` |

Install the hooks once per clone: `git config core.hooksPath .githooks`.

The commit hook holds only what costs seconds, because a commit is the unit of
work in progress and a gate that takes minutes gets bypassed or batched around.
The suite runs where a change becomes someone else's problem: the push, and
then the PR. The race detector runs only in CI: a race build shares no test
cache with a plain one, so locally it is always a second full run of the suite
on top of the first; it finds the same races on a runner as on a laptop; and a
PR is the last point at which finding one is still cheap.

The live tier is not in CI: it needs a built `../relayLLM` beside the checkout.

Nearly all of the suite's wall time is `cmd/relay`, which runs serially:
`mkSandboxRelayHome` redirects the config directory with `t.Setenv`, and Go
refuses `t.Parallel()` in a test that has set an environment variable. Packages
run in parallel with each other, so code that moves into `internal/`
([`package-layout.md`](package-layout.md)) takes its tests' time off the
critical path.

`golangci-lint` (`.golangci.yml`) is not wired into any gate — run it by hand
with `golangci-lint run ./...`.

## Adding a test

1. Pick the tier (ADR-001). ~95% belong in the default hermetic tier.
2. Reading/writing settings, pidfiles, logs, or the bridge socket → call `mkSandboxRelayHome(t)` first.
3. Need a working router → `newTestRouter(t, settings, mgr)`.
4. Exercising a manifest-registering service → `NewFakeService(t, FakeServiceOptions{...})`. The relayLLM contract is covered by `integration_fake_relayllm_test.go`.
5. Need a real spawned subprocess → the `cmd/testservice` / `cmd/testmcp` binaries, built on demand via `buildTestServiceBinary(t)` / `buildTestMcpBinary(t)`, never an `exec.Command` mock.
6. Live-tier tests carry `//go:build live` and `t.Skip` gracefully when the
   real binary they need is absent — `../relayLLM` unbuilt, or Google Chrome
   not installed. A developer without one must see a skip, never a failure.
7. The WebAuthn verifier is covered twice on purpose (ADR-016 decision 8):
   `internal/login`'s `webauthn_test.go` software client owns every negative
   case in the hermetic tier, and cmd/relay's `webauthn_browser_live_test.go`
   (driving `internal/login/loginfake`'s software authenticator over real
   HTTP) runs exactly one ceremony in a real Chrome — the only evidence that
   relay agrees with a user agent it did not also write. Neither covers real authenticator
   hardware or Safari; both gaps are named in `docs/testing-roadmap.md`.

## Not covered by the suite

- Cocoa tray UI (menu, dock) — exercise via `scripts/demo.sh`.
- Real `launchd` integration — `service_registry` is tested against `cmd/testservice`.
- Live OAuth round-trips — `internal/mcpbroker/oauth_test.go` covers PKCE/dynamic registration in isolation.
- Notarization / code-signing — exercised by `./build.sh --release`.

Architecture decisions are cited inline throughout as ADR-NNN. Cross-repo test status:
[`docs/testing-roadmap.md`](testing-roadmap.md).
