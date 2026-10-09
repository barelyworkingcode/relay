# Testing

Relay has no unit tests. `go test ./...` reports `no test files` for every
package and exits 0. CI rejects any Go test file outside `e2e/`.

## What proves relay

The devbox journeys in `cmd/devboxverify` drive the real app end to end: the
running tray, its sockets, the Settings UI and the services it supervises.
They build `cmd/testmcp` and `cmd/testservice`, two small probe binaries, and
`cmd/devboxpresence`. See [`cmd/devboxverify/README.md`](../cmd/devboxverify/README.md)
for the commands, the journeys and how a run is graded.

Until `e2e/` exists, a bug's failing repro is a devbox journey. A behaviour no
journey reaches is guarded by nothing; the reviewer reads the diff against
[`docs/THREAT-MODEL.md`](THREAT-MODEL.md) and the security invariants in
`CLAUDE.md`.

`scripts/demo.sh` starts the app against `test/fixtures/relay-home` and
`test/fixtures/scenarios` for hand-run visual checks, and `cmd/devui` is a
hand-run tool for Settings UI work.

## What gates what

| Stage | Runs | Where |
|---|---|---|
| commit | `gofmt -l`, `go build ./...`, `go vet ./...` | `.githooks/pre-commit` |
| push | `go build ./...` and `go vet ./...`, skipped when the pushed commits touch no Go sources or web assets | `.githooks/pre-push` |
| PR, and every push to `main` | `gofmt`, `go build ./...`, `go vet ./...`, `go vet -tags relaytest ./...`, `scripts/check-test-build.sh` (`absent` on an untagged build, `present` on a `relaytest` build), and a step that fails on any `_test.go` outside `e2e/` | `.github/workflows/ci.yml` |
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

The seams never act on the default config dir (`bridge.DefaultConfigDir()`,
symlinks resolved; a config dir that does not resolve, or a default dir that
fails to resolve for a reason other than not existing, counts as the default.
A default dir that does not exist, as on a CI runner, makes every existing dir
a non-default one, so no non-default dir touches the login keychain). There
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
