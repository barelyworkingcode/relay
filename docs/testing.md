# Testing

Relay has no unit tests. `go test ./...` reports `no test files` for every
package and exits 0. CI rejects any Go test file outside `e2e/` and `fakerelay/`; relay's tests live
in the e2e module, and fakerelay's proof tests in its own module.

## What proves relay

Two things prove relay: the e2e feature tests and the devbox journeys.

- **e2e feature tests** (`e2e/`, its own Go module `relaye2e`) start real relay
  instances and drive them through the CLI and the HTTP API, the way a user or
  a client does. They run in CI on every PR and on `main`. Every feature-map
  row in [`FEATURES.md`](FEATURES.md) names the test that proves it.
- **Devbox journeys** (`cmd/devboxverify`) drive the running tray, the
  Settings UI and the services it supervises on the test machine. See
  [`cmd/devboxverify/README.md`](../cmd/devboxverify/README.md).

A bug's failing repro is an e2e feature test where one can reach it, a devbox
journey otherwise. A behaviour neither reaches is guarded by nothing; the
reviewer reads the diff against [`THREAT-MODEL.md`](THREAT-MODEL.md) and the
security invariants in `CLAUDE.md`.

## The e2e module

`e2e/` imports nothing of relay's: it knows relay only from its documents
(`docs/`) and its binaries. Layout:

| Path | Holds |
|---|---|
| `e2e/harness` | starts isolated instances and builds the bundle (`go doc relaye2e/harness`) |
| `e2e/fakes` | the fake MCP server, model host and agent CLI the instances talk to |
| `e2e/coverage` | the coverage check, `pending.txt` and its own self-tests |
| `e2e/features` | the feature tests, and `TestMain` |

**Run it.**

```bash
cd e2e && go test -race -parallel 32 ./features/     # locally: 32 instances at once
cd e2e && go test -race -run TestFakeMCPStdioListsTools ./features/   # one test
```

`TestMain` builds one `-tags relaytest` bundle (the `relay-e2e` binary,
`relay-sessions` and the fakes) into a run root under `/tmp`, then runs the
tests. A passing run removes the root. A failing run keeps it, so the instance
directories can be read, and the next run reaps it.

**The devbox 32 by 3 procedure.** CI runs at the runner's parallelism. On the
devbox, run the suite three times with 32 instances live at once:

```bash
cd e2e && go test -race -count=1 -parallel 32 ./features/   # run it three times
```

`TestInstancesIsolated` boots 32 instances and, when the run allows 32 parallel
tests, holds them at a barrier so all 32 are live together. It logs
`barrier: 32 instances live` on success; without that line the run did not
reach 32 at once.

**Remote in a test.** The harness's default `settings.json` has no `remote`
block, so an instance opens no remote or enrolment listener. A test that
enables `remote` sets both listens to `127.0.0.1:0` itself.

**The fakes.** Each is a small program the harness builds into the bundle and
starts per instance; none touches the network beyond loopback.

- `fakemcp` is an MCP server. Flags: `--catalogue <file>` (tool catalogue JSON,
  required), `--call-log <file>` (required), `--transport stdio|http`
  (default `stdio`), `--listen <addr>` (http, default `127.0.0.1:0`), `--oauth`
  (require OAuth on `/mcp`, http only) and `--token-ttl <d>` (access token
  lifetime, default 1h).
- `fakemodelhost` is a registered model host. Flags: `--models <file>` (model
  catalogue JSON, required) and `--call-log <file>` (required).
- `fakeagent` is the claude, pi and codex CLI. It picks its persona from the
  name it is run as and answers `--version`; any other name exits 2.

The catalogue is the harness spec's `Catalogue` field, written to
`<id>.catalogue.json` in the instance's fake directory. Every request a fake
serves is appended to its call log before the reply, as one JSON line written
with a single `write(2)` (`e2e/fakes/calllog`), so a test that has seen the
reply reads the line with no wait.

**Harness rules.** The coverage check enforces the first two.

- `t.Parallel()` is the first statement of every test.
- No `time.Sleep`. A test waits on a signal: a command exiting, a ready file,
  or an event from `relay logs --follow --event <key> --timeout <d>`.
- Every instance has its own config dir, HOME, TMPDIR, PATH and ports. Nothing
  touches the real config dir or the running tray.
- A failed test dumps the instance's evidence into the test log: the serve exit
  code, the stored logs, the audit file, serve's stderr and each fake's call
  log, each cut to its last lines. A test run with `-race` also fails on a
  race report in any relay process's stderr.
- A test imports the harness and the fakes, never relay's packages.

**The coverage check.** `TestFeatureMapCoverage` starts an instance, reads
`relay doors --json`, and checks that the door catalogue, `FEATURES.md`, the
reference documents and the tests describe the same product. A row with no
test, a door in no row, a missing or stale reference heading and a harness
rule break each fail it, with the rule and the place. The rules:

| Rule | Meaning |
|---|---|
| R1 | Every catalogue door is named by a row. |
| R2 | Every door a row names is in the catalogue. |
| R3 | Every row has a test the check accepts (`e2e:`, a live `pending:#N`, or what its kind allows); `journey:` alone proves only an `exception:` row. |
| R4 | `e2e:` items and test functions agree: every item names a test, names are unique, and no test is unnamed by a row. |
| R5 | A gated row has refusal proof, and each owner-gated http or cli door has a deny test item. |
| R6 | Every `event:` key is in `docs/events.md` section 7. |
| R7 | Row IDs are well formed. |
| R8 | Every promise maps to rows with a refusal proof. |
| R9 | A row not proven by CI has an `event:`, `audit:` or `out:` proof item. |
| Q6 | Every http door has a heading in `docs/routes.md` and every cli verb one in `docs/cli.md`. |
| H1 to H3 | `t.Parallel()` first; no `time.Sleep`; the e2e module imports nothing of relay's. |

`TestFeatureMapCoverage` is the one test exempt from R4: it checks the map and
is named by no row. `e2e/coverage/pending.txt`
lists the issue numbers whose `pending:#N` rows still count as tested. It only
shrinks: the child that writes a row's tests removes its number.

**The e2e-test-writer agent** (`.claude/agents/e2e-test-writer.md`) writes
feature tests from the documents and the harness's Go doc alone. A
`PreToolUse` hook (`.claude/hooks/e2e-read-guard.sh`) refuses file tools under
`cmd/` and `internal/` and Bash commands that name them. It stops accidental
reads. It is not a security boundary: a shell command can always find another
way to read a file. When a document is too thin to write a test from, the
agent reports a doc gap and the document is fixed.

`scripts/demo.sh` starts the app against `test/fixtures/relay-home` and
`test/fixtures/scenarios` for hand-run visual checks, and `cmd/devui` is a
hand-run tool for Settings UI work.

## What gates what

| Stage | Runs | Where |
|---|---|---|
| commit | `gofmt -l`, `go build ./...`, `go vet ./...` | `.githooks/pre-commit` |
| push | `go build ./...` and `go vet ./...`, skipped when the pushed commits touch no Go sources or web assets | `.githooks/pre-push` |
| PR, and every push to `main` | `gofmt`, `go build ./...`, `go vet ./...`, `go vet -tags relaytest ./...`, `scripts/check-test-build.sh` (`absent` on an untagged build, `present` on a `relaytest` build), and a step that fails on any `_test.go` outside `e2e/` and `fakerelay/` | `.github/workflows/ci.yml` (`build` job) |
| PR, and every push to `main` | the `fakerelay` job on Linux: `gofmt -l`, `go vet ./...`, a `CGO_ENABLED=0` build for Linux and macOS, then `go test -race -count=1 -parallel 24 ./...` in `fakerelay/` | `.github/workflows/ci.yml` (`fakerelay` job) |
| PR, and every push to `main` | the `e2e` job: `gofmt -l` and `go vet ./...` in `e2e/`, then `go test -race -count=1 ./...` at the runner's default parallelism | `.github/workflows/ci.yml` (`e2e` job) |
| `./build.sh --test` | `go vet ./...` before install | `build.sh` |

Do not set `core.hooksPath` in this repo. A global hooks dispatcher runs the
files in `.githooks/`, and a local value skips its push checks.

`golangci-lint` (`.golangci.yml`) is not wired into any gate. Run it by hand
with `golangci-lint run ./...`.

## The test build

A build with `-tags relaytest` takes the outside world away from the test: no
person at the presence prompt, no login keychain, and a clock the test moves.
It is for a harness that runs a relay on its own config dir. A release build
has none of it; `scripts/check-test-build.sh absent` proves that in
`build.sh` and in CI.

```bash
./build.sh --test-build        # builds with -tags relaytest
go build -race -tags relaytest -o "$R/relay" ./cmd/relay   # a throwaway build
```

`--test-build` installs to `~/Applications/RelayTest.app`. The real tray in
`/Applications/Relay.app` is not touched, stopped or reopened. It refuses to
run while a process runs from that bundle, and it rejects `--release`.
`--test` is allowed.

**Three seams, three files in the config dir `X`.** Each acts only on a dir
other than the default one ([below](#the-default-config-dir)).

| Seam | Control | Design |
|---|---|---|
| Presence | `X/test-presence.json` | [`presence-gate.md`](presence-gate.md#the-test-build-presence) |
| Keychain | `X/test-keychain.json` (the store), `X/test-keychain-fault.json` | [`sealed-config.md`](sealed-config.md#the-test-builds-keychain-provider) |
| Clock | `relay debug clock [set <RFC3339> \| advance <duration>]` | below |

All three files are private to the user: regular files, owned by the user,
mode 0600. A harness keeps `X` under `/tmp`, outside every grant, so a session
cannot write them.

A run starts with `relay serve --config-dir X` and waits for its first stdout
line (`X/ready.json`). Every verb takes the same `--config-dir X`.

**The clock.** `relay [--config-dir X] debug clock` prints the server's time
and its offset from wall time; `set <RFC3339>` and `advance <duration>` move
it. `--json` prints `{"now": ..., "offset_ms": ...}`. The clock lives in
memory, so a restart returns to wall time. It governs every decision that
grants, refuses, expires, retries or closes a window:

- control-plane credential expiry (mint, authentication, reaping), and the
  `credential list` expiry filter and `credential mint` expiry line;
- login: the bootstrap code lifetime, browser-session expiry and listing, the
  WebAuthn challenge lifetime and the ceremony limiter;
- the eve passkey enrolment window and the tray countdown;
- remote enrolment requests (lodge and collect lifetimes, limiter, the pending
  notifier) and the remote tool and mount budgets;
- launch identity lifetime and reaping;
- service restart backoff and stable window, and `service list`'s next attempt;
- external MCP restart backoff and stable window, and HTTP MCP OAuth token
  expiry and refresh window;
- the model catalogue lifetime.

Log and audit timestamps, durations, socket and TLS deadlines, X.509 validity
(the peer checks it on its own clock), diagnostic rate limits, the keychain
bound, the OAuth callback wait, the host status cache and the presence nonce
lifetime stay on wall time, so `relay logs --since` and `relay audit --since`
read real time. relay-sessions is not on the server clock.

Wait on a signal, not a duration. An answer to a presence prompt is the
`debug.presence.answer` event; a clock move is complete when the
`debug.clock` command exits, because a timer due at the new time has fired by
then; a decision that reacts in the background is the `service.state` or
`mcp.state` event. `relay logs --follow --event <key> --timeout <d>` waits on
one.

### The default config dir

The seams never act on the default config dir (`bridge.DefaultConfigDir()`),
compared by file identity, so a spelling that differs in case or through a
symlink is still the default. A config dir that cannot be examined gets no
seams. A default dir that cannot be examined, as on a CI runner where it does
not exist, makes every existing dir a non-default one, so no non-default dir
touches the login keychain. There
the test build approves `project.grant` alone and refuses every other gated
op, reads no test file, uses the login keychain, and `debug clock` refuses
with "the clock is fixed on the default config dir". That keeps the in-place
swap below working for a journey that runs against the real config dir.

Both trays use the same ports and config dir, so only one runs. Swap in place.
`$CFG` is relay's config dir.

1. Unlock the keychain that holds the Developer ID signing identity, then
   run `./build.sh --test-build`.
2. Find the release tray's pid: it is in the name of the socket
   `relay-frontend-<pid>.sock` in `$CFG`. Stop it and wait for it to exit:

   ```bash
   PID=<pid>
   kill -TERM $PID && caffeinate -w $PID
   ```

3. Start the test tray: `open ~/Applications/RelayTest.app`.
4. Wait until `relay service list` shows `relaysessions running`, then
   `relay service restart --id eve-verify`. Before that wait the restart does
   nothing.
5. Check the signal below, then run the journey.

**The signal.** The test tray's binary contains the bytes `-tags=relaytest`
(its embedded build info). A release binary does not. Find the running
executable from the pid, then count the bytes:

```bash
EXE=$(ps -p <pid> -o comm=)
LC_ALL=C /usr/bin/grep -c -a -e '-tags=relaytest' "$EXE"   # >=1 test build, 0 release
go version -m "$EXE" | /usr/bin/grep -e '-tags='            # the same, with the Go toolchain
```

**Swap back.** The journey is not finished until the release tray is back.

```bash
PID=<pid of the test tray, from relay-frontend-<pid>.sock>
kill -TERM $PID && caffeinate -w $PID
open /Applications/Relay.app
# wait until `relay service list` shows relaysessions running
relay service restart --id eve-verify
curl -s http://127.0.0.1:3100/api/auth/status     # no "trusted" field
```

Then the one-line release check: on the running release tray the count is 0.

```bash
LC_ALL=C /usr/bin/grep -c -a -e '-tags=relaytest' "$(ps -p <new pid> -o comm=)"   # prints 0
```

**Risks while swapped.**

- Every gated op but `project.grant` is refused, including mint, revoke,
  enrolment and service registration. Do other work after the swap back.
- Relay's own `devboxverify` preflight refuses a tray that is not running
  from `RELAY_BIN`; with `RELAY_BIN` pointed at the test bundle, its gate
  journeys fail on the refusals.
- Project `SKILL.md` files written while the test tray runs may name the test
  bundle path. They are rewritten when the release tray restarts.

## Not covered

- Anything no devbox journey reaches. No test guards it.
- Cocoa tray UI (menu, dock): exercise it via `scripts/demo.sh`.
- Real `launchd` integration beyond what the journeys drive.
- Live OAuth round-trips against a real provider.
- Notarization and code-signing: exercised by `./build.sh --release`.
- Real authenticator hardware and Safari for the passkey ceremony.

Architecture decisions are cited inline throughout as ADR-NNN. Cross-repo test
status: [`docs/testing-roadmap.md`](testing-roadmap.md).
