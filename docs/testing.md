# Testing

**Headline rule:** no test may read or mutate the real user config directory
(`~/Library/Application Support/relay/`). Tests route through
`mkSandboxRelayHome(t)` (in `support_test.go`), which redirects
`bridge.ConfigDir()` to a per-test temp dir under `/tmp` (via `mkShortTempDir`,
which sidesteps the 104-char Unix-socket path limit) populated from
`test/fixtures/relay-home/`.

The `support_safety_test.go` guard enforces it, and the suite runs with relay
running or stopped. `TestMain` points `HOME` and `XDG_CONFIG_HOME` at a fresh
root under `/tmp` (`/tmp/relay-suite-home-*`) before any test runs, so nothing
in the process reaches the real config dir through `HOME`. A test that forgets
`mkSandboxRelayHome` resolves the config dir to
`<root>/Library/Application Support/relay`, the tripwire. The suite fails if
the tripwire exists after the run (no ignore list: `logs/`, `run/` and sockets
count too), or if the config dir no longer resolves to it (a test left `HOME`,
`XDG_CONFIG_HOME` or the override changed). The isolated root is a tripwire,
not a place tests may use. Helper children that re-exec the test binary
inherit the root through `RELAY_TEST_SUITE_HOME` and reuse it rather than
making their own, so a child's leak lands on the tripwire the parent checks,
and only the parent removes the root.

The guard also compares the real config dir before and after the run, which
catches a route that ignores `HOME`. That comparison is skipped when a relay
answers on the real `relay.sock` at the start of the run: the live app
rewrites `settings.json` on its own schedule. The guard then prints one
`sandbox guard: ... real config dir not compared this run` line. CI never has
a relay running, so CI always runs the comparison.

Prove the guard still bites with the leak probe, which writes the tripwire's
`settings.json` and must turn the package red with `SANDBOX VIOLATION`:

```bash
go test -count=1 -tags leakprobe -run '^TestLeakProbe_' ./cmd/relay/
```

## Config dir isolation

Every package, not just `cmd/relay`, keeps its tests off the real config dir.
A package does it once in `TestMain`: point `HOME` (and `XDG_CONFIG_HOME`) at
a temp dir with `os.Setenv`, as `internal/service/main_test.go` does. A
package can also call `bridge.SetConfigDirForTest` instead, as
`internal/sshhost` does. `cmd/relay` isolates `HOME` in its `TestMain` and
still sandboxes each test with `mkSandboxRelayHome`. The path getters
(`bridge.ConfigDir`, `SocketPath`, `ModelSocketPath`) only compute paths. The
code that binds or writes creates the directory.

`internal/bridge/home_isolation_gate_test.go` enforces this. It finds every
package with tests whose source mentions `bridge.ConfigDir(`,
`bridge.SocketPath(`, `bridge.ModelSocketPath(` or `os.UserConfigDir(`, and
requires a `TestMain` that calls `os.Setenv("HOME"`. The exemption list
(`internal/sshhost`) names each package with its reason. The
gate also fails if it matches no packages, so a broken scan can't pass. Its
blind spot: it reads source text, so it can't see a package that reaches these
helpers only through another package. Such a package still needs its own
`TestMain` isolation. Nothing enforces that.

## Three tiers

| Command | What runs | When |
|---|---|---|
| `go test ./...` | Hermetic suite — pure Go, no spawned binaries, no user files | Every push (pre-push hook); every PR and `main` (CI) |
| `go test -tags=live ./...` | Spawns real binaries end-to-end: the `../relayLLM` binary, and a headless Google Chrome that runs the passkey login ceremony against the real `/relay/login` document (`webauthn_browser_live_test.go`) and drives the Settings model picker against the real Settings bundle with a mock `list_models` bridge (`settings_model_picker_browser_live_test.go`), and drives the Projects tab's Mode buttons, Default projects panel and the Overview's default-project Needs-attention row (`settings_project_mode_browser_live_test.go`) | By hand: after relay↔relayLLM boundary changes; after any change to the WebAuthn verifier, the login routes or the login page; after any change to the model picker or `list_models`; after any change to project modes or default projects in the Settings UI |
| `go test -race ./...` | Hermetic suite + race detector | Every PR and `main` (CI); locally on the package you touched when changing anything concurrent |

## What gates what

| Stage | Runs | Where |
|---|---|---|
| commit | `gofmt -l`, `go build ./...`, `go vet ./...` | `.githooks/pre-commit` |
| push | `go test ./...`, skipped when the pushed commits touch no Go sources, fixtures or web assets | `.githooks/pre-push` |
| PR, and every push to `main` | `gofmt`, `go vet`, `go test ./...` and `go test -race ./...` as two parallel jobs | `.github/workflows/ci.yml` |
| PR | `burn-in`: each added or changed `TestXxx` function, `go test -race -count=10` | `.github/workflows/burn-in.yml`, `cmd/burnin` |

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

## Burn-in

A new test that fails one run in ten passes its own PR and fails someone else's
later. The `burn-in` job repeats the test functions a PR adds or changes, ten
times each under the race detector, so the flake fails on the PR that wrote it.
It adds no CPU load of its own: no `-p`, `-cpu` or `-shuffle`, one package at a
time.

- **Changed** means the declaration text differs between the merge-base of
  the PR's base and its head:
  the `func` line through the closing brace, comments and whitespace inside
  included, the doc comment above excluded. A function the base does not have
  is new, so a rename runs the new name and a move to another package runs
  there. A function moved to another file of the same package with identical
  text does not run. A deleted test does not run.
- **Default build only.** A file counts only when `go list` puts it in its
  package's test files on the runner (macOS). Files behind the `live` or
  `leakprobe` tags, under `testdata/`, or for another OS are skipped, and
  `-list` says how many.
- **Never run:** `Example`, `Benchmark` and `Fuzz` functions, and `TestMain`.
  Only top-level `func TestXxx(t *testing.T)` functions are picked. Subtests of
  a picked function run with it.
- **Helper-only gap.** A PR that changes only a helper in a `_test.go` file, or
  `TestMain`, selects nothing and the job passes. The selection does not follow
  call graphs. Nor does a test that enters the default build unchanged (a
  `live` tag removed, or a move out of a tagged file).
- **A PR that changes no test** passes with "nothing to run".
- **Failure** prints `::error::burn-in: <package> failed under go test -race
  -count=10 -run <regex>`, naming the package and its tests. Every package runs
  even after one fails.

Preview the selection before pushing, and reproduce a failure with the same
command without `-list`:

```bash
go run ./cmd/burnin -list origin/main HEAD
go run ./cmd/burnin origin/main HEAD
```

The program is stdlib-only (`go/parser` selects, `git show` at the merge-base
supplies the base copy, so a branch behind `main` never picks up tests `main`
changed) and has no configuration: ten runs is the count.

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
