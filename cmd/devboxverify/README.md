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
(default `~/.config/relay-verify/credential`),
`RELAY_VERIFY_CONFIGURE_CREDENTIAL_FILE` (default
`~/.config/relay-verify/configure-credential`). The tool never builds or
mints. It changes settings only on its own fixture records (Verify Stale,
Verify Numbers), through relay's project route. Stdout is tab-separated `PREFLIGHT`, `WORLD`, `RESET`,
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

**stale-derived-access-edit.** An Access-only edit on a remote record that
holds a stale derived field (`file_dirs`, which the MCP now derives from the
project path) is accepted. The stale field is dropped; `mail_accounts` stays.
- Lives in: `internal/project/apply.go` (`ApplyUpdate`),
  `internal/project/grant_widening.go` (`jsonValueEqual`),
  `cmd/relay/project_ops.go` (`ProjectOps.Update`).
- Reached by: `PUT /api/projects/{id}` on the frontend socket with the
  configure credential (P7); read back through `relay grant --json`.
- Traps: fixture P5 is one-shot. A pass drops the field, and later runs read
  NOTRUN until P5 is re-armed. A build without the fix answers 400 naming
  `is derived by relay from the project's path`.

**context-number-resave.** Saving a context value that is numerically
unchanged (`1.0` sent as `1`) needs no presence and writes no
`config_change`. The journey restores `1.0` after.
- Lives in: `internal/project/apply.go` (`ApplyUpdate`),
  `internal/project/grant_widening.go` (`jsonValueEqual`),
  `cmd/relay/project_ops.go` (`ProjectOps.Update`).
- Reached by: `PUT /api/projects/{id}` on the frontend socket with the
  configure credential (P7); read back through `relay grant --json` and
  `relay audit --event config_change`.
- Traps: a build without the fix prompts. The request is bounded at 10 s and
  reads FAIL, leaving the dialog open (see Traps). A stored `n` other than
  `1.0` reads BLOCKED; restore it per P6.

**v1-conversion-refusal.** Always NOTRUN. A local-to-remote conversion is a
kind change, which the presence gate prompts for before it validates, so the
v1 refusal is reachable only after a human approves. No v1 MCP is registered
here either. `TestApplyUpdate_ConvertingV1GrantToRemote` is the guard.

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
- **P5. Verify Stale.** Uses the P7 credential.
  1. From a relay checkout, build the test MCP outside any repo:

     ```bash
     go build -o ~/.local/bin/devboxverify-testmcp ./cmd/testmcp
     ```

  2. Create the access profile before the MCP is registered, so relay holds
     no schema for it. It prompts once:

     ```bash
     SOCK=$(ls ~/Library/Application\ Support/relay/relay-frontend-*.sock)
     curl -sS --unix-socket "$SOCK" -H "Authorization: Bearer $(cat ~/.config/relay-verify/configure-credential)" -H 'Content-Type: application/json' -X POST http://relay/api/projects -d '{"name":"Verify Stale","kind":"remote","allowed_mcp_ids":["devboxverify-scope"],"access":{"devboxverify-scope":"read"},"context":{"devboxverify-scope":{"file_dirs":["/nonexistent/devboxverify"],"mail_accounts":["Alice"]}}}'
     ```

  3. From a desktop Terminal (it prompts):

     ```bash
     relay mcp register --id devboxverify-scope --name "devboxverify scope probe" --command ~/.local/bin/devboxverify-testmcp --env RELAY_TESTMCP_CONTEXT=v2
     ```

  On current builds this stale state is reachable only by writing the field
  while the MCP is unknown, hence the order. It is one-shot: a pass drops the
  field, and later runs read NOTRUN. Re-arm (two prompts):
  `relay mcp unregister --id devboxverify-scope`; a
  `PUT /api/projects/<id>` with step 2's `context` (prompts); then step 3.
- **P6. Verify Numbers.** The P5 curl, with the body below. It prompts once.
  `devboxverify-numbers` is never registered. The Settings form can't create
  this record: its JSON round trip is the bug itself.

  ```json
  {"name":"Verify Numbers","kind":"remote","allowed_mcp_ids":["devboxverify-numbers"],"context":{"devboxverify-numbers":{"n":1.0}}}
  ```

  If a run leaves `n = 1`, restore it with a `PUT /api/projects/<id>` of
  `{"context":{"devboxverify-numbers":{"n":1.0}}}` (no prompt on a fixed
  build).
- **P7. Configure credential, only for evidence runs.** At the console:

  ```bash
  relay credential mint --name devbox-verify-configure --class configure --ttl 24h
  ```

  Save the token alone, mode 0600, in the configure credential file. P4
  stays execute-only: a configure token on disk can rename, narrow or delete
  projects without presence. Without P7 both fix journeys read NOTRUN, which
  is the nightly state.

## Verifying a PR

1. Check out the PR head in its own worktree and run `./build.sh` there. That
   installs and relaunches Relay.app built from the PR.
2. From a `main` checkout, run
   `go run ./cmd/devboxverify --checkout <PR worktree> --post <N>`. To run
   journeys the PR adds, run it from the PR worktree instead; `--checkout`
   may name any worktree.
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
- A build without the context-number fix leaves a Relay password dialog open
  after context-number-resave FAILs on its 10 s bound. Press Cancel. Never
  approve a Relay dialog during a run.
- Never edit the fixtures from Settings. Any edit with the MCP connected
  drops Verify Stale's `file_dirs`.
- When showing red then green, run the red build first. A fixed build spends
  Verify Stale.
- Never post raw audit rows. They carry the home path. The tool writes its own
  details and scrubs the home directory from what it posts.
