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
| PR, and every push to `main` | `gofmt`, `go build ./...`, `go vet ./...`, `go vet -tags testapprover ./cmd/relay`, and a step that fails on any `_test.go` outside `e2e/` | `.github/workflows/ci.yml` |
| `./build.sh --test` | `go vet ./...` before install | `build.sh` |

Do not set `core.hooksPath` in this repo. A global hooks dispatcher runs the
files in `.githooks/`, and a local value skips its push checks.

`golangci-lint` (`.golangci.yml`) is not wired into any gate. Run it by hand
with `golangci-lint run ./...`.

## The test-approver build

The `testapprover` build answers the presence prompt for `project.grant` and
refuses every other gated op, so a verify journey can create a project with
no one at the screen. Why, and why it cannot reach a release:
[`docs/presence-gate.md`](presence-gate.md#the-test-approver-build).

```bash
./build.sh --test-approver     # builds with -tags testapprover
```

It installs to `~/Applications/RelayTestApprover.app`. The real tray in
`/Applications/Relay.app` is not touched, stopped or reopened. It refuses to
run while a process runs from that bundle, and it rejects `--release`.
`--test` is allowed.

Both trays use the same ports and config dir, so only one runs. Swap in place.
`$CFG` is relay's config dir.

1. Unlock the keychain that holds the Developer ID signing identity, then
   run `./build.sh --test-approver`.
2. Find the release tray's pid: it is in the name of the socket
   `relay-frontend-<pid>.sock` in `$CFG`. Stop it and wait for it to exit:

   ```bash
   PID=<pid>
   kill -TERM $PID && caffeinate -w $PID
   ```

3. Start the test tray: `open ~/Applications/RelayTestApprover.app`.
4. Wait until `relay service list` shows `relaysessions running`, then
   `relay service restart --id eve-verify`. Before that wait the restart does
   nothing.
5. Check the signal below, then run the journey.

**The signal.** The test tray's binary contains the bytes `-tags=testapprover`
(its embedded build info). A release binary does not. Find the running
executable from the pid, then count the bytes:

```bash
EXE=$(ps -p <pid> -o comm=)
LC_ALL=C /usr/bin/grep -c -a -e '-tags=testapprover' "$EXE"   # >=1 test build, 0 release
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
LC_ALL=C /usr/bin/grep -c -a -e '-tags=testapprover' "$(ps -p <new pid> -o comm=)"   # prints 0
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
