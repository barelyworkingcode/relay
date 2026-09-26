# devboxverify

Layer 2 verification: drive the **running** Relay.app, loaded with the
devboxWorld test world, through relay's own surfaces, and read the outcome
back, audit log included. No screen, no mocks. It runs on the devbox, never in
CI.

```bash
go run ./cmd/devboxverify [--checkout DIR] [--world DIR] [--post PR]
```

Environment: `RELAY_BIN` (default `/Applications/Relay.app/Contents/MacOS/relay`),
`DEVBOXWORLD_ROOT` (default `~/World`), `RELAY_VERIFY_CREDENTIAL_FILE`
(default `~/.config/relay-verify/credential`). The tool never builds, mints
or edits settings. Stdout is tab-separated `PREFLIGHT`, `WORLD`, `RESET`,
`JOURNEY`, `SUMMARY` and `POSTED` lines and nothing else; progress and the
world scripts' own output go to stderr. Exit 0 when
every journey is PASS or NOTRUN, 1 on any FAIL or BLOCKED, 2 on a usage,
preflight, reset or post failure.

## Feature map

**blank-model-refused.** A chat session launched with no model is refused
before anything starts, and the refusal is audited.
- Lives in: `cmd/relay/session_launch.go` (`AuthorizeLaunch`, the
  `model_required` refusal), `cmd/relay/session_routes.go` (`POST /api/sessions`).
- Reached by: HTTP over `<configdir>/relay-frontend-<pid>.sock`, Host `relay`,
  bearer credential of class `execute`.
- Traps: this guards the relay-side refusal only. The refusal inside
  relay-sessions is unreachable here, because relay never sends it a
  blank-model launch. A 403 naming template `chat` means Acme Corp does not
  allow the `chat` template (setup P3).

**permission-mode-restart.** Always NOTRUN. The Kill-then-Start restart runs
only for SSH-host projects; a local session answers `resume_required`
(`internal/sessions/provider/claude.go`, `SetPermissionMode`). The world has
no hosts, so the unit test is the only guard.

**oversized-launch-audit-capped.** A refused sandbox launch with a huge
template name writes a size-capped audit row.
- Lives in: `cmd/relay/sandbox_attach.go` (`SandboxAttach`),
  `cmd/relay/session_launch.go` (`newSessionLaunchAuditEvent`), `internal/audit`.
- Reached by: `SandboxAttach` on the bridge socket `<configdir>/relay.sock`.
- Traps: passes only on a build that carries the error cap. On a build
  without it the detail names the uncapped row size.

**acme-sandbox-reach.** A sandboxed shell in Acme Corp reads Acme's
`PROJECT.md` and is refused Globex's.
- Lives in: `cmd/relay/sandbox_attach.go`, `cmd/relay/session_launch.go`
  (`wantsSandbox`), `internal/bridge/sandbox.go` (the wire).
- Reached by: `SandboxAttach` with template `world-probe`, then `input`
  frames; the transcript comes back as `output` frames until `exit`.
- Traps: the markers are built by `printf` at run time, so the terminal's echo
  of the input never matches them. `inside_session` or `peer_confined` means
  the tool ran inside a relay session.

## One-time setup

None of this drifts `verify.sh`.

- **P1.** A sandboxed terminal template `world-probe` that runs `/bin/sh`,
  with no folders.
- **P2.** A template with id `chat`, sandboxed, no folders. Settings offers
  only templates that exist.
- **P3.** In Settings, Acme Corp's allowed templates are `chat` and
  `world-probe`.
- **P4.** An `execute`-class credential, minted at the console (it raises a
  presence prompt):

  ```bash
  relay credential mint --name devbox-verify --class execute --ttl 168h
  ```

  Save the token alone, mode 0600, in the credential file, outside any repo.
  Re-mint when it expires; an expired one reads as BLOCKED.

## Verifying a PR

1. Check out the PR head in its own worktree and run `./build.sh` there. That
   installs and relaunches Relay.app built from the PR.
2. From a `main` checkout, run
   `go run ./cmd/devboxverify --checkout <PR worktree> --post <N>`.
3. Preflight refuses unless the app was built from the PR head with a clean
   tree, is the one running, and started after it was installed; then
   `bootstrap.sh --check` must pass and `verify.sh` must be green. Only then
   does `reset.sh` run.
4. Rebuild from `main` when done, so the app is left on `main`.

## Traps

- Run from an operator shell, never inside a relay session. Relay refuses a
  sandbox attach from inside one.
- `build.sh` signs and relaunches the app. Unlock the signing keychain first,
  or the build fails at `codesign`.
- A stale or incomplete bootstrap (a helper build out of date, say) fails
  preflight as `bootstrap incomplete; run bootstrap.sh`. It is checked before
  `reset.sh` because reset takes the world down before it checks bootstrap,
  and would leave it down.
- Audit rows are recorded asynchronously and read back for up to 5 s. A
  dropped row reads as FAIL, not as a pass.
- A regression that accepts the blank-model launch leaves a chat session
  running in Acme Corp. The detail names it; stop it by hand.
- Never post raw audit rows. They carry the home path. The tool writes its own
  details and scrubs the home directory from what it posts.
