# devboxverify

Layer 2 verification: drive the **running** Relay.app, loaded with the
devboxWorld test world, through relay's own surfaces, and read the outcome
back, audit log included. No mocks. It runs on the devbox, never in CI.

```bash
go run ./cmd/devboxverify [--checkout DIR] [--world DIR] [--post PR | --phase api|screen]
```

Environment:

| Variable | Default | Used for |
|---|---|---|
| `RELAY_BIN` | `/Applications/Relay.app/Contents/MacOS/relay` | the app under test |
| `DEVBOXWORLD_ROOT` | `~/World` | the test world's folders |
| `RELAY_VERIFY_CREDENTIAL_FILE` | `~/.config/relay-verify/credential` | P4, the `execute` credential |
| `RELAY_VERIFY_MODEL` | `Chat` | the model the chat journeys pick from `GET /api/models`: equal, or ending in `/Chat` |
| `DEVBOXPRESENCE_BIN` | `~/.local/share/devboxverify/bin/devboxpresence` | the presence helper; testmcp and testservice are built beside it |
| `DEVBOX_ADMIN_PASSWORD_FILE` | `~/.claude/devbox-admin-password` | read by the helper only, never by this tool |
| `EVE_BROWSER_LOCK` | `~/.cache/eve/browser-tests.lock` | the shared screen lock |
| `EVE_BROWSER_LOCK_TIMEOUT` | `1800` | seconds to wait for the lock |

Stdout is tab-separated `PREFLIGHT`, `WORLD`, `RESET`, `JOURNEY`, `SUMMARY`
and `POSTED` lines and nothing else; progress and the world scripts' own
output go to stderr. Exit 0 when every journey is PASS or NOTRUN, 1 on any
FAIL or BLOCKED, 2 on a usage, preflight, reset or post failure. A journey
the phase does not select prints nothing.

The tool changes settings only on its own fixtures: Verify Stale and Verify
Numbers (one-time setup below), and the records the screen phase creates
with the run nonce in their names. It mints and revokes its own run
credential, and renews P4 when it is due.

## Phases

- **api** (`--phase api`): the execute credential (P4) and the bridge. No
  dialog, no lock, no screen. It runs from an SSH shell.
- **screen** (`--phase screen`): the owner gates and everything that needs a
  control-plane class P4 lacks. It needs the console session, holds the
  shared browser-test lock for the whole phase, and lets the presence helper
  answer or cancel relay's prompts. In order: mint the run credential, the
  owner-gate negatives, the NOTRUN positives, the setup positives (probe MCP,
  Verify Grant project, crash service), the feature journeys, then token
  rotation, eve's enrolment window, fixture removal and the run credential's
  revocation.
- No `--phase` runs both in one process, api first. `--post` always runs
  both and refuses `--phase`.

The nightly runs relay `--phase api`, then eve's own run, then relay
`--phase screen`. Eve's `nightly.js` owns that order.

Preflight, in order: `session` (not inside a relay session), `head`, `build`
(the app was built from HEAD, clean tree), `app` (the one running), `helpers`,
then for the screen phase `lock`, `console`, `password` and `sweep`, then
`pr` (with `--post`), `bootstrap` and `world`. Both phases run bootstrap,
world and reset.

- `helpers` checks the installed presence helper is present, Developer ID
  signed and built from this checkout's `cmd/devboxpresence` rev, and for the
  screen phase that `devboxpresence check` passes and `./cmd/testmcp` and
  `./cmd/testservice` build.
- `console` asks the kernel whether this process's session has graphic
  access, the same question relay asks before it will prompt.
- `password` runs `devboxpresence check --password`.
- `sweep` cancels any LocalAuthentication dialog already open.

After every screen journey the tool sweeps again. A journey that left a
prompt open reads FAIL "left a presence prompt open", whatever it read
before. Every owner-gate negative runs every night.

### The run credential

`gate-credential-mint-pos` mints `devbox-verify-run-<nonce>` with classes
`read`, `configure`, `grant` and `proxy` and a 1 h TTL, and the helper
answers its prompt. The token lives in memory only: never on disk, never in
a detail. Launches (`POST /api/sessions`, `POST /api/terminals`) use P4,
since they are class `execute`; everything else in the screen phase uses the
run credential. `gate-credential-revoke-pos` revokes it last. A journey that
needs it after a failed mint reads BLOCKED "no run credential".

### The presence helper

`cmd/devboxpresence` answers or cancels relay's LocalAuthentication prompt the
way a person would, through Accessibility and key events, and only a prompt
that appeared after its own trigger and whose text carries that request's
subject: the run nonce, or the id it acts on. Two prompts carry no nonce.
`execute-credential-renewal` mints the fixed name `devbox-verify`, so it
expects relay's whole reason for that mint, closing period included.
`gate-eve-enrolment-open-pos` expects relay's fixed reason, which names no
subject. Relay has no API, flag or environment variable that skips the gate. Its CLI,
exit codes and safety checks are in [its README](../devboxpresence/README.md).

The helper that runs is the installed one, never a fresh build: macOS ties
its Accessibility grant to the signed binary. devboxWorld's bootstrap builds
it with its source rev, signs it with the Developer ID identity, installs it
at `~/.local/share/devboxverify/bin/devboxpresence` and grants it
Accessibility once. Until the bootstrap does that, do it by hand from a relay
checkout:

```bash
rev=$(git log -1 --format=%H -- cmd/devboxpresence)
go build -ldflags "-X main.sourceRev=$rev" -o ~/.local/share/devboxverify/bin/devboxpresence ./cmd/devboxpresence
codesign --force --sign "Developer ID Application" ~/.local/share/devboxverify/bin/devboxpresence
```

Then add that binary under System Settings > Privacy & Security >
Accessibility. Rebuild and re-sign after any change to `cmd/devboxpresence`;
`helpers` refuses a stale one.

### The lock

The screen phase takes eve's browser-test lock: the same file and protocol
as eve's `scripts/browser-lock.js`, an exclusive `flock` retried every
second up to `EVE_BROWSER_LOCK_TIMEOUT`. The holder writes
`{"pid":N,"command":"…","since":"…"}` into the file and truncates it on
release. The kernel drops the lock when its holder dies. The api phase never
takes it.

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
holds a stale derived field (`file_dirs`, which relay derives from the
project path) is accepted. The stale field is dropped; `mail_accounts` stays.
- Lives in: `internal/project/apply.go` (`ApplyUpdate`),
  `cmd/relay/project_ops.go` (`ProjectOps.Update`).
- Reached by: `PUT /api/projects/{id}` on the frontend socket with the run
  credential; read back through `relay grant --json`.
- Traps: fixture P5 is one-shot. A pass drops the field, and later runs read
  NOTRUN until P5 is re-armed. A build without the fix answers 400 naming
  `is derived by relay from the project's path`.

**context-number-resave.** Saving a context value that is numerically
unchanged (`1.0` sent as `1`) needs no presence and writes no
`config_change`. The journey restores `1.0` after.
- Lives in: `internal/project/apply.go` (`ApplyUpdate`),
  `internal/project/grant_widening.go` (`jsonValueEqual`),
  `cmd/relay/project_ops.go` (`ProjectOps.Update`).
- Reached by: `PUT /api/projects/{id}` on the frontend socket with the run
  credential; read back through `relay grant --json` and
  `relay audit --event config_change`.
- Traps: a build without the fix prompts. The request is bounded at 10 s and
  reads FAIL; the sweep after it cancels the prompt. A stored `n` other than
  `1.0` reads BLOCKED; restore it per P6.

**v1-conversion-refusal.** Always NOTRUN. A local-to-remote conversion is a
kind change, which the presence gate prompts for before it validates, so the
v1 refusal is reachable only after a human approves. No v1 MCP is registered
here either. `TestApplyUpdate_ConvertingV1GrantToRemote` is the guard.

**acme-tools-through-bridge** (api). A session in Acme Corp lists only its
granted `mail_*` tools, calls one, and is denied one outside the grant.
- Lives in: `cmd/relay/router.go` (`ListTools`, `checkToolAccess`),
  `cmd/relay/exec_cmd.go` (`relay mcp call`).
- Reached by: a tokenless `relay mcp call --list` and `--tool` inside a
  world-probe session attached over the bridge.
- Traps: the denial must say `access denied`; any other failure is FAIL.

**tool-call-audited** (api). Each of those two calls writes exactly one
`call_tool` row, with no `phase`, actor `project_session`, the session's id,
mcp `macmcp` and outcome `ok` or `denied`.
- Lives in: `cmd/relay/audit_call.go`.
- Reached by: the same session calls, then `relay audit --event call_tool
  --kind project_session --json`.
- Traps: a local call writes one row; only a remote call writes an intent
  and a completion. The baseline row must still be in the last 200.

**Owner-gate positives** (screen): `gate-credential-mint-pos`,
`execute-credential-renewal`, `gate-mcp-register-pos`,
`gate-project-grant-pos`, `gate-service-register-pos`,
`gate-project-rotate-token-pos`, `gate-eve-enrolment-open-pos`,
`gate-credential-revoke-pos`. The harness triggers the op as the owner, the
helper answers the prompt whose text carries the request's subject (the
nonce, except for the renewal and the eve enrolment above), and the journey
checks the effect and a new issuance or `config_change` row carrying
`presence_id`. The mint, MCP, project and service positives set up the run
credential, the probe MCP (testmcp), the Verify Grant project and the crash
service (testservice) that later journeys use; a journey whose fixture is
missing reads BLOCKED naming the journey that sets it.
- Lives in: `cmd/relay/presence_gate.go`, `internal/presence`, and each op's
  core (`credential_ops.go`, `mcp_ops.go`, `project_ops.go`,
  `service_ops.go`, `eve_enrolment_ops.go`).
- Reached by: the `relay` CLI (admin_op over the bridge) or the frontend
  socket with the run credential.
- Traps: helper exit 1, 3, 4 or 5 reads BLOCKED with its detail. A CLI that
  says it cannot show a prompt means the run is not at the console.
  `execute-credential-renewal` reads NOTRUN until P4 has 48 h left; then it
  mints a new 168 h P4 and rewrites the credential file at mode 0600.
  `gate-eve-enrolment-open-pos` consumes the window it opened, so a run
  never ends with it open.

**Owner-gate positives that never run.** `gate-enrolment-create-pos`,
`gate-enrolment-sign-pos`, `gate-enrolment-update-pos`,
`gate-enrolment-revoke-pos`, `gate-login-bootstrap-mint-pos`,
`gate-login-passkey-revoke-pos`, `gate-mcp-oauth-start-pos`,
`gate-remote-configure-pos`, `gate-sealed-reset-pos` and
`gate-eve-passkey-revoke-pos` always read NOTRUN; the detail and FEATURES.md
say why for each.

**Owner-gate negatives with a prompt** (screen): `gate-<op>-neg` for
`credential.mint`, `credential.revoke`, `mcp.register`, `service.register`,
`eve.enrolment.open`, `eve.passkey.revoke`, `enrolment.create`,
`enrolment.sign`, `enrolment.update`, `enrolment.revoke`,
`login.bootstrap.mint` and `login.passkey.revoke`. The harness plays the
agent: inside a world-probe session in Acme Corp it runs the op's `relay`
command while the helper cancels the prompt. PASS needs a prompt that
appeared and closed, a non-zero exit saying `presence was refused`, and no
effect (the name, id or window is absent or unchanged).
- Lives in: `internal/presence` (`Gate.Request`), `cmd/relay/admin_ops.go`,
  and each op's core.
- Reached by: `relay` inside the session, over the bridge's `admin_op`.
- Traps: helper exit 1 is FAIL, since the gate never asked. Relay records no
  row for a cancelled prompt, so the detail says the refusal is not audited.
  The enrolment update and revoke negatives name an id that does not exist:
  the gate runs before the lookup, and a "not found" reads FAIL.
  `gate-eve-passkey-revoke-neg` reads NOTRUN unless relay's eve passkey
  mirror holds a non-pending passkey that is not the last one.

**Owner-gate negatives with no door** (screen): `gate-project-grant-neg`,
`gate-project-rotate-token-neg`, `gate-remote-configure-neg`,
`gate-mcp-oauth-start-neg`, `gate-sealed-reset-neg`. From inside the
session, `curl --unix-socket` to the op's HTTP route cannot connect (exit 7),
and an `admin_op` for the op sent with `nc -U` to the bridge answers
`unknown admin operation`. No prompt appears.
- Lives in: `cmd/relay/session_sandbox.go` (what the sandbox re-permits),
  `cmd/relay/admin_ops.go` (`adminOps`).
- Traps: BLOCKED when the session has no `curl` or `nc`.

**session-chat-lifecycle** (screen). A chat session in Acme Corp launches,
answers one message, is deleted and leaves the list, and its launch is
audited.
- Lives in: `cmd/relay/session_routes.go` (create), `cmd/relay/session_launch.go`
  (`AuthorizeLaunch`), `internal/sessions/api/http_session.go` (message,
  delete, list), behind relay's proxy.
- Reached by: `POST /api/sessions` with P4; `POST /api/sessions/{id}/message`,
  `DELETE /api/sessions/{id}` and `GET /api/sessions` with the run
  credential (class `proxy`); `relay audit --event session_launch`.
- Traps: the model is `RELAY_VERIFY_MODEL` as listed by `GET /api/models`;
  a model that is not listed, a 403 at launch (Acme does not allow `chat` or
  that model) and a model host answering 503 read BLOCKED. `DELETE` answers
  204.

**terminal-lifecycle** (screen). A world-probe terminal in Acme Corp
launches, is listed, has a non-empty log within 5 s, is deleted and leaves
the list, and its launch is audited.
- Lives in: `cmd/relay/session_routes.go`, `internal/sessions/api/http_terminal.go`
  (`HandleTerminalLog`), `internal/sessions/terminal`.
- Reached by: `POST /api/terminals` with P4; `GET /api/terminals`,
  `GET /api/terminals/{id}/log` and `DELETE /api/terminals/{id}` with the
  run credential.
- Traps: the log route answers 404 until the log exists, so it is polled.

**model-list-and-completion** (screen). `GET /api/models` lists the chosen
model, one chat turn in Acme Corp gets an answer, and a new `model_call` row
for Acme records that model with outcome `ok`. The session is deleted after.
- Lives in: `internal/sessions/api/models.go`, `cmd/relay/model_endpoint.go`,
  `cmd/relay/audit_model.go`.
- Reached by: the session-chat-lifecycle calls, then `relay audit --event
  model_call --json`, filtered to rows since the journey began.
- Traps: rows are matched by time, not a baseline id, because other
  services' model calls can push an id out of the window.

**disabled-tool-refused** (screen). In a live session in Verify Grant,
`testmcp_ping` answers; a `PUT /api/projects/{id}` with `disabled_tools`
naming it returns 200 within 10 s without a prompt; the same session no
longer lists it and its call is refused `is disabled for this token`. The
journey then clears `disabled_tools` and checks the tool is listed again.
- Lives in: `cmd/relay/router.go` (`checkToolAccess`, `ListTools`),
  `internal/project/apply.go` (`ApplyUpdate`).
- Reached by: `relay mcp call` in a world-probe session attached in the
  Verify Grant folder; the PUT with the run credential.
- Traps: BLOCKED "fixture: testmcp_ping not listed" means the Verify Grant
  grant does not admit the probe tool at all, so there was nothing to take
  away. The restore runs even when the checks fail.

**grant-narrowing-live** (screen). A live session in Verify Grant lists
`testmcp_ping`; `PUT /api/projects/{id}` with `allowed_mcp_ids []` returns
200 within 10 s without a prompt; in the same session the list is empty and
the call is refused.
- Lives in: `internal/project/grant_widening.go` (narrowing is not a
  widening), `internal/project/apply.go`, `cmd/relay/router.go`.
- Reached by: as disabled-tool-refused, which must run first: this one
  leaves the project with no MCP until verify-fixtures-removed deletes it.

**service-start-stop** (screen). `POST /api/services/{id}/start` brings the
crash service to STATE `running` with its process up within 5 s; `/stop`
returns it to STATE `-` with no process within 5 s.
- Lives in: `cmd/relay/service_routes.go`, `cmd/relay/service_ops.go`,
  `internal/service` (the registry and supervision).
- Reached by: the routes with the run credential (class `configure`); the
  STATE comes from the bridge's `service.list`, the data `relay service
  list` renders; the process from `pgrep -f` on the unique `crash-<nonce>.env`
  argument.

**service-restart-on-crash** (screen). The crash service is started, its
process killed with SIGKILL, and within 15 s relay has it `running` again
with a new pid. The detail says whether `restarting (attempt 1` was seen
between the two. The service is stopped after.
- Lives in: `internal/service/supervision.go` (`scheduleRestart`).
- Reached by: as service-start-stop, polled every 200 ms.
- Traps: the first restart waits 1 s by design; a build that gives up at the
  first attempt reads FAIL with STATE `failed (exit …)`.

**verify-fixtures-removed** (screen). Every `devboxverify-probe-*` MCP and
`devboxverify-crash-*` service is unregistered (both ungated) and every
`Verify Grant *` project deleted, from this run or a crashed one, with their
state folders. PASS when none is left and no prompt appeared.
- Lives in: `cmd/relay/mcp_ops.go`, `cmd/relay/service_ops.go`,
  `cmd/relay/project_ops.go`.
- Traps: without the run credential the projects stay, the rest is still
  removed, and the journey reads BLOCKED.

## One-time setup

None of this drifts `verify.sh`.

- **P1.** A sandboxed terminal template `world-probe` that runs `/bin/sh`,
  with no folders.
- **P2.** A template with id `chat`, sandboxed, no folders. Settings offers
  only templates that exist.
- **P3.** In Settings, Acme Corp's allowed templates are `chat` and
  `world-probe`. If Acme restricts models, it allows the one
  `RELAY_VERIFY_MODEL` names.
- **P4.** An `execute`-class credential, minted at the console (it raises a
  presence prompt):

  ```bash
  relay credential mint --name devbox-verify --class execute --ttl 168h
  ```

  Save the token alone, mode 0600, in the credential file, outside any repo.
  The screen phase renews it once fewer than 48 h are left
  (execute-credential-renewal); an expired one reads as BLOCKED.
- **P5. Verify Stale.** Needs a `configure` credential for the setup alone.
  Mint one at the console
  (`relay credential mint --name devbox-verify-setup --class configure --ttl 1h`),
  save the token alone at mode 0600 in `~/.config/relay-verify/setup-credential`,
  and revoke it and delete the file when setup is done.
  1. From a relay checkout, build the test MCP outside any repo:

     ```bash
     go build -o ~/.local/bin/devboxverify-testmcp ./cmd/testmcp
     ```

  2. Create the access profile before the MCP is registered, so relay holds
     no schema for it. It prompts once:

     ```bash
     SOCK=$(ls ~/Library/Application\ Support/relay/relay-frontend-*.sock)
     curl -sS --unix-socket "$SOCK" -H "Authorization: Bearer $(cat ~/.config/relay-verify/setup-credential)" -H 'Content-Type: application/json' -X POST http://relay/api/projects -d '{"name":"Verify Stale","kind":"remote","allowed_mcp_ids":["devboxverify-scope"],"access":{"devboxverify-scope":"read"},"context":{"devboxverify-scope":{"file_dirs":["/nonexistent/devboxverify"],"mail_accounts":["Alice"]}}}'
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

## Verifying a PR

1. Check out the PR head in its own worktree and run `./build.sh` there. That
   installs and relaunches Relay.app built from the PR.
2. From a `main` checkout, in a desktop Terminal (the screen phase needs the
   console session), run
   `go run ./cmd/devboxverify --checkout <PR worktree> --post <N>`. To run
   journeys the PR adds, run it from the PR worktree instead; `--checkout`
   may name any worktree. `--post` runs both phases. From SSH, run it
   through the one-shot LaunchAgent recipe in the
   [presence helper's README](../devboxpresence/README.md). Measured: a full
   run that way completes both phases in about 25 s, with relay raising its
   prompts and the installed helper trusted for Accessibility.
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
- A regression that accepts the blank-model launch, or one whose `DELETE`
  fails in session-chat-lifecycle or terminal-lifecycle, leaves a session
  running in Acme Corp. The detail names it; stop it by hand.
- A screen run that dies mid-way leaves its fixtures; the next run's
  verify-fixtures-removed removes them. A credential it minted expires within
  the hour.
- Never touch a Relay dialog during a run. The helper answers or cancels the
  harness's own prompts, and the sweep after each screen journey cancels any
  left open.
- A stale-derived-access-edit FAIL "stale file_dirs was kept" can also mean
  `devboxverify-scope` is registered but not connected, so relay holds no
  schema for it. Check it in Settings before reading the FAIL as a
  regression. This can never produce a false PASS.
- Never edit the fixtures from Settings. Any edit with the MCP connected
  drops Verify Stale's `file_dirs`.
- When showing red then green, run the red build first. A fixed build spends
  Verify Stale.
- Never post raw audit rows. They carry the home path. The tool writes its own
  details and scrubs the home directory from what it posts. Details never
  carry a token; anything shaped like one is redacted.
