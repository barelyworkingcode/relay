# devboxverify

Layer 2 verification: drive the **running** Relay.app, loaded with the
devboxWorld test world, through relay's own surfaces, and read the outcome
back, audit log included. No mocks. It runs on the devbox, never in CI.

```bash
go run ./cmd/devboxverify [--checkout DIR] [--post PR | --phase api|screen]
```

Environment:

| Variable | Default | Used for |
|---|---|---|
| `RELAY_BIN` | `/Applications/Relay.app/Contents/MacOS/relay` | the app under test |
| `DEVBOXWORLD_MARKER` | `~/.config/devboxWorld/machine.json` | devboxWorld's machine marker: the only source of the world checkout, the world root and the world version |
| `RELAY_VERIFY_CREDENTIAL_FILE` | `~/.config/relay-verify/credential` | P4, the `execute` credential |
| `RELAY_VERIFY_MODEL` | `Chat` | the model the chat journeys pick from `GET /api/models`: equal, or ending in `/Chat` |
| `DEVBOXPRESENCE_BIN` | `~/.local/share/devboxverify/bin/devboxpresence` | the presence helper; testmcp and testservice are built beside it |
| `DEVBOX_ADMIN_PASSWORD_FILE` | `~/.claude/devbox-admin-password` | read by the helper only, never by this tool |
| `EVE_BROWSER_LOCK` | `~/.cache/eve/browser-tests.lock` | the shared screen lock |
| `EVE_BROWSER_LOCK_TIMEOUT` | `1800` | seconds to wait for the lock |

Stdout is tab-separated `PREFLIGHT`, `REPAIRED`, `WORLD`, `RESET`, `JOURNEY`, `TIMING`,
`SUMMARY` and `POSTED` lines and nothing else; progress and the world scripts' own
output go to stderr. Exit 0 when every journey is PASS or NOTRUN, 1 on any
FAIL or BLOCKED, 2 on a usage, preflight, reset or post failure. A journey
the phase does not select prints nothing. `TIMING\tjourney\t<id>\t<ms>`
follows each `JOURNEY` line: whole milliseconds, truncated, from just before
the journey starts until its result. `TIMING\trun\t<ms>` comes immediately
before `SUMMARY`, and only when `SUMMARY` prints; it is measured from entry
to `run()`. The evidence comment carries the same measurement as a
`| Run time | <s> s |` row right after `Tool commit`, with
`<s> = (ms+500)/1000`. The comment's `| Repaired |` row sits right after
`World verify`: `none`, or `<what>: <detail>` per repair joined `; `, with
pipes escaped.

A red run names its cause. A preflight FAIL whose detail starts
`BLOCKED fixture: ` is the test data (the world's version or catalogue); one
starting `BLOCKED environment: ` is the machine's world state (bootstrap or
`repair.sh`); `not a test machine: …` means this is not a bootstrapped VM.
A journey FAIL after a green preflight is the product.

## The world

The world comes only from devboxWorld's machine marker, which bootstrap's
`machine` step writes on a VM. There is no flag or environment override for
the world's checkout or root. The marker is refused, with devboxWorld's own
reasons, unless `kern.hv_vmm_present` is 1, it is a regular file with no
group or other permission bits, and it holds `schema` 1, absolute `world_checkout` and
`world_root`, a positive `world_version` and `written_at`. The marker is
`lstat`ed first: only "no such file" means absent (`not a test machine: run
devboxWorld bootstrap on a VM`, with no VM check). Any other `lstat` error
is checked for a VM, then reported as `marker is not readable`; so is a
read error after a good `lstat`.

The harness never reads, sets or clears `DEVBOXWORLD_ROOT`; the world
scripts inherit its environment unchanged, `DEVBOXWORLD_MARKER` included. A
stray `DEVBOXWORLD_ROOT` makes `repair.sh` refuse, which reads
`PREFLIGHT bootstrap FAIL BLOCKED environment: bootstrap incomplete; repair.sh
exited 2 without a result; run bootstrap.sh`, with the reason on stderr.

### The repair call

`bootstrap` and `world` share one call to devboxWorld's `repair.sh`, run
once from `world_checkout` in `bootstrap`'s place and killed at 900 s. Its
stdout is parsed and copied to stderr. `repair.sh` checks bootstrap, then
verifies the world; when `verify.sh` is red it resets once and verifies
again. It writes every detail; the harness only adds the
`BLOCKED environment: ` prefix to a FAIL. devboxWorld's `docs/WORLD.md`
holds the wire shape.

- `CHECK bootstrap OK|FAIL <d>` becomes `PREFLIGHT bootstrap OK <d>` or
  `FAIL BLOCKED environment: <d>`. With no bootstrap line it reads
  `bootstrap incomplete; repair.sh <how> without a result; run bootstrap.sh`.
- Each `REPAIRED\t<what>\t<d>` prints unchanged before the world line.
- `world` is OK, with `<d>` `green` or `green after repair`, only when
  `repair.sh` printed `CHECK world OK <d>`, exited 0 in time and printed a
  `SUMMARY` with `fail=0`. A `CHECK world FAIL <d>` line reads
  `BLOCKED environment: <d>` whatever the exit code; anything else reads
  `repair.sh <how> without a result`.
- `<how>` is `exited <n>` or `timed out after 900s`.
- `WORLD` and the `World verify` row take their counts from the last `SUMMARY`.

A repaired run resets twice: once inside `repair.sh` and once after
preflight.

`worldVersion` in `world.go` is relay's pin; it must equal the marker's
`world_version`. The fixtures come from `<world_checkout>/data/world.json`:
its `projects`, its `fixtures` catalogue (`project:<key>`,
`file:<key>/<rel>`) and `relay_mcp`, the MCP id and tool pattern every
world project grants. `relay_mcp` is a published constant, not a catalogue
id, so a journey reads it without declaring it; the tool prefix is its
`tools` with one trailing `*` trimmed. macMCP's own tool names
(`mail_list_accounts`, `contacts_list`) stay in the journeys.

World data is checked in this order, and the first failure is the
`fixtures` check's `BLOCKED fixture: <reason>`: `world data is not
readable`; `world data is not valid JSON` (also when the top level is not
an object); `world_version is not a positive integer`; `fixtures is not a
list of strings`; `projects is not a list`; `project <i> is malformed`
(0-based); `relay_mcp is malformed`; `fixture <id> does not resolve` (an id
that is not `project:` or `file:`, names no project key, or whose rel is
empty, starts with `/` or contains `..`). The harness does not stat the
world's files. Every journey declares the fixtures it reads in
`Needs`, and at run time sees only those: a lookup of anything else reads
BLOCKED `undeclared fixture <id>`. Project names and folders are read from
the catalogue, never written into a journey.

The tool changes settings only on its own fixtures: Verify Stale and Verify
Numbers (one-time setup below), and the records the screen phase creates
with the run nonce in their names, plus one terminal template with the fixed
id `devboxverify-extra-args` (nonce in its name only). Verify Grant allows
that template. It mints and revokes its own run
credential, and renews P4 when it is due.

## Phases

- **api** (`--phase api`): the execute credential (P4) and the bridge. No
  dialog, no lock, no screen. It runs from an SSH shell.
- **screen** (`--phase screen`): the owner gates and everything that needs a
  control-plane class P4 lacks. It needs the console session, holds the
  shared browser-test lock for the whole phase, and lets the presence helper
  answer or cancel relay's prompts. In order: mint the run credential, the
  owner-gate negatives, the NOTRUN positives, the two `relay serve` instance
  journeys (OAuth and sealed reset), the setup positives (probe MCP,
  Verify Grant project, crash service), the feature journeys, then token
  rotation, eve's enrolment window, fixture removal and the run credential's
  revocation.
- No `--phase` runs both in one process, api first. `--post` always runs
  both and refuses `--phase`.

The nightly runs relay `--phase api`, then eve's own run, then relay
`--phase screen`. The nightly runner owns that order.

Preflight, in order: `machine` (the marker, read on a VM: `vm; world v<N>`),
`pin` (the marker's world version is relay's), `fixtures` (every fixture the
selected journeys declare is in the catalogue), then `session` (not inside a
relay session), `head`, `build` (the app was built from HEAD, clean tree),
`app` (the one running), `helpers`, then for the screen phase `lock`,
`console`, `password` and `sweep`, then `pr` (with `--post`), `bootstrap` and
`world`, both run from the marker's `world_checkout`. `pin` reads OK
`v<N>`; `fixtures` reads OK `<n> fixtures for <m> journeys` and FAIL
`BLOCKED fixture: <journey> needs <id>[, <id>…]`, joined `; `, listing only
ids missing from the catalogue, journeys in run order. The first three run
before any lock, script or network call, so a machine without a valid
marker gets `PREFLIGHT machine FAIL not a test machine: …` and exit 2 with
nothing touched. Both phases run bootstrap, world and reset.

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
no hosts, so nothing guards it until a feature test covers it.

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
here either. Nothing guards it until a feature test covers it.

**acme-tools-through-bridge** (api). A session in Acme Corp lists only its
granted tools (world.json's `relay_mcp.tools`), calls one, and is denied
one outside the grant.
- Lives in: `cmd/relay/router.go` (`ListTools`, `checkToolAccess`),
  `cmd/relay/exec_cmd.go` (`relay mcp call`).
- Reached by: a tokenless `relay mcp call --list` and `--tool` inside a
  world-probe session attached over the bridge.
- Traps: the denial must say `access denied`; any other failure is FAIL.

**tool-call-audited** (api). Each of those two calls writes exactly one
`call_tool` row, with no `phase`, actor `project_session`, the session's id,
mcp `relay_mcp.id` and outcome `ok` or `denied`.
- Lives in: `cmd/relay/audit_call.go`.
- Reached by: the same session calls, then `relay audit --event call_tool
  --kind project_session --json`.
- Traps: a local call writes one row; only a remote call writes an intent
  and a completion. The baseline row must still be in the last 200.

**file-plane-contained** (screen). Two legs, each through the file routes on
the frontend socket with the execute-class launch credential, and the journey
passes only if both pass. The console leg runs in Acme; the host leg runs in
a project on a loopback SSH host, set up and removed by the same code as
`session-drop-in-host` (P11). In each: a write answers 200 with an `intent`
row (read once, when the route answers) and a `completion` row `ok` with the
same id; a `..` path answers 403 `TRAVERSAL`, a write through a symlink
planted in the project folder 403 `SYMLINK`, and a write after
`relay project update --files-read-only=true` 403 `READ_ONLY`, each with one
`denied` row (no phase, error = code) and no file left behind.
- Lives in: `cmd/relay/file_ops.go`, `cmd/relay/file_routes.go`,
  `cmd/relay/audit_file.go`, `cmd/relay/project_cmd.go`, the host agent in
  `internal/projectfs`.
- Reached by: `POST /api/projects/{id}/files/write`, then `relay audit
  --event file_op --project <id> --json`.
- Traps: it is a screen journey because creating the host project raises a
  presence prompt. The completion and denied rows are written after the
  response with no signal outside relay, so the journey polls the audit log,
  bounded at 10 s, until all of them are there. The first host write waits
  for the embedded agent to start over ssh; the HTTP response is the signal,
  with a 35 s bound. The flag is always cleared at teardown.

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

**gate-mcp-oauth-start-pos** (screen). Runs the installed binary as `relay
serve` on `/tmp/dbv-oauth`, a dir the journey recreates, never the tray's
store. A loopback provider inside the harness serves discovery, dynamic
registration, the authorization code with PKCE and a one-tool MCP that
answers only the token it issued. The journey registers the MCP (prompt),
runs `mcp authenticate --json` (prompt `mcp.oauth.start`), reads the
authorization URL from the first stdout line and plays the browser: the
provider answers 302 and relay's callback answers 200. PASS when the
provider saw one registration, one authorization and one PKCE-verified code
exchange; `relay logs` has one `mcp.oauth.start` event with `status` ok; a
`config_change` row with `presence_id` names the MCP; `settings.json` holds
neither issued token in clear and the access token is an envelope under the
instance's `sealed_key_id`; and, after the instance restarts, `mcp.state`
`up` arrives for the MCP, the provider saw a request with the issued token
and saw no registration, authorization, exchange or refresh.
BLOCKED when `RELAY_BIN` is a test build (the login keychain is not in use)
or the provider cannot bind. The instance and the provider are stopped on
every path; a process left naming the dir is killed and reads FAIL.
- Waits: serve's one stdout line and its exit; the first stdout line of
  `mcp authenticate`; the callback's HTTP response; `mcp.state` through
  `relay logs --follow --since`.
- Lives in: `journey_mcp_oauth.go`, `oauth_fixture.go`, `serve_instance.go`.

**gate-sealed-reset-pos** (screen). Runs `relay serve` on `/tmp/dbv-sealed`
from the installed binary. Its sealing key has its own login-keychain item
(`docs/sealed-config.md`), so a reset there cannot touch the tray's. Before
anything is reset the journey reads the tray's `sealed_key_id` and FAILs,
resetting nothing, if the instance holds the same one. It then runs
`relay sealed reset` (prompt `sealed.reset`, answered only by relay's whole
zero-count reason, which the tray's store never produces). PASS when
`sealed.reset` has one `ok` event, the instance holds a new `sealed_key_id`
with `admin_secret` sealed under it, `relay status` reports no `seal_status`
now and after a restart, its keychain item is present, and the tray's key id
and `seal_status` are unchanged. A reset writes no audit row, so the detail
says not audited. BLOCKED when `RELAY_BIN` is a test build or the installed
store is already degraded.
- Waits: as above, plus the `sealed.reset` event, written before the CLI
  answers.
- Lives in: `journey_sealed_reset.go`, `serve_instance.go`.

**Owner-gate positives that never run.** `gate-enrolment-create-pos`,
`gate-enrolment-sign-pos`, `gate-enrolment-update-pos`,
`gate-enrolment-revoke-pos`, `gate-login-bootstrap-mint-pos`,
`gate-login-passkey-revoke-pos`, `gate-remote-configure-pos` and
`gate-eve-passkey-revoke-pos` always read NOTRUN; the detail and the feature
map ([`docs/FEATURES.md`](../../docs/FEATURES.md)) say why for each.

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

**terminal-extra-args** (screen). A terminal launched from a fixture template
in the Verify Grant project, with `extraArgs` of one marker, prints the marker
and exits 0. The template (fixed id `devboxverify-extra-args`) prints its
first argument; the marker is only in the launch body. The journey waits up
to 10 s for the row to read `stopped`, then up to 5 s for the log to hold
`extra-arg:devboxverify-extra-arg-marker`, and deletes terminal and template.
- Lives in: `cmd/relay/session_launch.go` (`AuthorizeLaunch`),
  `cmd/relay/session_routes.go` (`createTerminalWireBody`).
- Reached by: `DELETE` and `POST /api/terminal/templates` with the run
  credential; `POST /api/terminals` with P4; `GET /api/terminals` and
  `GET /api/terminals/{id}/log` with the run credential.
- Traps: the launch answers 201 even when `extraArgs` is dropped, so the
  marker check comes before the exit code. A run on main with exit 0 and no
  marker goes through the Proof rules; it may be lost pty output. Do not add
  a `sleep` to the script. The template is allowed in Verify Grant only, so
  the journey needs gate-project-grant-pos to have passed.

**model-list-and-completion** (screen). `GET /api/models` lists the chosen
model, one chat turn in Acme Corp gets an answer, and a new `model_call` row
for Acme records that model with outcome `ok`. The session is deleted after.
- Lives in: `internal/sessions/api/models.go`, `cmd/relay/model_endpoint.go`,
  `cmd/relay/audit_model.go`.
- Reached by: the session-chat-lifecycle calls, then `relay audit --event
  model_call --json`, filtered to rows since the journey began.
- Traps: rows are matched by time, not a baseline id, because other
  services' model calls can push an id out of the window.

**session-chat-resume** (screen). A chat session in Acme Corp answers, is
ended without being deleted, answers `resume_required`, resumes with
`resumed: true`, and answers again. The session is deleted after.
- Lives in: `cmd/relay/session_routes.go` (`handleResumeSession`),
  `cmd/relay/router_sessions.go` (`SessionExited` ages the ledger record),
  `internal/sessions/session` (`Create` with `Resume`).
- Reached by: the session-chat-lifecycle calls; the WS `end_session` frame on
  `/ws` with the run credential; `POST /api/sessions/{id}/resume` with P4.
- Traps: `end_session` has no reply, so the journey waits for the session's
  `session_end` audit row before it sends. It cannot drive a stale `live`
  record left by a Relay relaunch: restarting Relay mid-run stops eve-verify.

**chat-tool-search-tokens** (screen). A chat in Verify Skills sends "Reply with
the single word: ready" twice, each in a fresh session: once with
`sessions/chat.json` set to `{"toolSearch":{"mode":"off"}}`, once with
`{"toolSearch":{"mode":"on","pinned":["tides_lookup"]}}`. The first
`model_call` row of each gives `prompt_tokens` and `request_bytes`. PASS when
all four are above 0, both are lower with tool search on, and the
relay-sessions log has a `chat.tool_search` line for each session: `active=false
reason=off` for the first, `active=true reason=on skills>=40 pinned=1` for the
second. BLOCKED when Verify Skills or the model is missing. The journey backs
up `chat.json` first and puts it back (or removes it, if there was none)
whatever the outcome; a restore that fails is a FAIL.
- Lives in: `internal/sessions/toolsearch/`,
  `internal/sessions/provider/chat_toolsearch.go`, `cmd/relaysessions/main.go`
  (the `chat.json` path), `cmd/testmcp/wide.go`.
- Reached by: `POST /api/sessions` with P4, the message route with the run
  credential, `relay audit --event model_call --json`, and
  `<configdir>/logs/relaysessions.log`. It writes
  `<configdir>/sessions/chat.json` directly: that file is the power door.
- Traps: relay-sessions reads `chat.json` when a chat provider starts, so the
  file is written before each session, not during one. The skills are read
  from Verify Skills' folder at the same moment. If the model never answers
  with prompt tokens, the row shows 0 and the journey FAILs: the model host
  must report usage (setup P8 step 4).

**session-agent-state** (screen). A headless agent session in Acme Corp
(`POST /api/sessions` with model `haiku` and settings `{"headless":true,"agent":true}`)
runs one turn, "Reply with exactly: verify-<nonce>-done", and is then ended
over `/ws`. A connection dialled before the launch, with the run credential,
sees every `session_state` frame. PASS when the frames for the session include
running, idle and ended in that order; one `turn_done` arrives before the idle
frame and its excerpt contains the marker; the list row then has
`attention.state` `idle` with `since` equal to the idle frame's; after the end
the row has no `attention`; and `<configdir>/logs/relaysessions.log` has
exactly one `op=session.state` line per frame seen for the session, none of
them containing the marker. BLOCKED when the `system/init` model is not
`claude-haiku-5-5` (the detail names the id), or a launch is refused (setup P9). The
session is deleted whatever the outcome.
- Lives in: `internal/sessions/attention/`, `internal/sessions/session`
  (`SetAttentionSink`, `Summary.Attention`), `internal/sessions/api/ws_session.go`.
- Reached by: `POST /api/sessions` with P4, the message route and
  `GET /api/sessions` with the run credential, `/ws` on the frontend socket.
- Traps: the connection never joins before the launch, so the id is unknown
  until the 201; the state frames reach it anyway, and it joins afterwards for
  the `system/init` event. Claude's Stop kills the process, so `end_session`
  reads idle then ended, not ended alone. Run on Claude Haiku
  `claude-haiku-5-5`; any other model reads BLOCKED.

**session-codex** (screen). A Codex session in Acme Corp (`POST /api/sessions`
with eve's body: model `codex/gpt-6-luna`, settings `{"useRelayTools":true}`,
`appendClaudeMd` true) answers one message sent over `/ws`: `join_session`,
then `send_message` "Reply with exactly: verify-<nonce>-codex". A connection
dialled before the launch, with the run credential, sees every frame. PASS when
`GET /api/models` lists `codex/gpt-6-luna`; the launch answers 201;
`system/init` names model `gpt-6-luna`; assistant text deltas and one
`turn_done` whose excerpt contains the marker arrive before the first `idle`
frame that follows `running`; the list row has `attention.state` `idle` with
`since` equal to that frame's; `end_session` gives `ended` and the row then has
no `attention`; and `DELETE` answers 2xx. Every other outcome is FAIL with the
status and detail, including Codex missing, a 403 launch refusal (setup P10)
and a different model. The journey adds no NOTRUN or BLOCKED of its own; only
a missing run or execute credential reads as before. The session is deleted
whatever the outcome.
- Lives in: `internal/sessions/provider/codex.go`, `internal/sessions/session`
  (attention), `internal/sessions/api/ws_session.go`.
- Reached by: `GET /api/models`, `POST /api/sessions` with P4,
  `GET /api/sessions` with the run credential, `/ws` on the frontend socket.
- Traps: the turn waits up to 120 s for the idle frame, inside the 180 s
  budget. Codex signs in with its own login, so a missing `codex` binary or
  login shows as a launch failure or a missing model row.

**session-drop-in** (screen). A headless agent session in Acme Corp (model
`haiku`, settings `{"headless":true,"agent":true}`) runs one turn, "Reply with
exactly: verify-<nonce>-done", and is taken over through the CLI's door:
`DropInAttach` on `relay.sock`. PASS when a `running` frame and the
agent's `process_exited` frame follow the answer, the list row has `live` false,
the marker appears in the resumed terminal's output (a folder-trust prompt is
answered with Enter), `/exit` ends the terminal (no exit frame in 15 s: the
connection is closed instead, and the detail says so), an `idle` frame follows,
`<configdir>/logs/relay.log` has exactly one `op=session.drop_in` line with
`status` ok and `host` console, the audit has a `session_end` row for the agent
with reason `closed` and a `session_launch` row for the terminal. The session
and terminal are deleted whatever the outcome. BLOCKED when `system/init` does
not report `claude-haiku-5-5`. The HTTP door on a host is
session-drop-in-host.
- Lives in: `cmd/relay/session_dropin.go`, `internal/bridge/dropin.go`,
  `internal/sessions/session/dropin.go`, `internal/sessions/hostapi`.
- Reached by: `POST /api/sessions` with P4, the message route with the run
  credential, `/ws`, `relay.sock`.

**session-drop-in-host** (screen). Eve's door on an SSH host: a host
`loopback-<nonce>` targeting `localhost` and a project `Drop-in Host <nonce>` on
a fresh folder; a headless agent session there runs the same turn, which must
end `idle`. `POST /api/sessions/{id}/drop-in` answers 201 with a
`claudeSessionId` that is a UUID, and a terminal id; the host's `~/.claude/projects/*/<claudeSessionId>.jsonl` holding the first turn's prompt, read over ssh; the agent's `process_exited`
frame follows and its list row has `live` false; within 20 s the host's process
table (`ssh -o BatchMode=yes localhost ps`) holds a `claude` command with
`--resume <that uuid>`, and the first turn's marker shows in the terminal
within 60 s, a folder-trust prompt answered Yes. `/exit` is sent to the terminal; with no `terminal_exit`
in 15 s the terminal is deleted instead. An `idle` frame follows, and the log
has exactly one `op=session.drop_in` line with `status` ok, `host`
`loopback-<nonce>` and the terminal's id. The session, terminal, project and
host are deleted whatever the outcome. BLOCKED when the loopback host or its
project cannot be created, the host launch is refused (setup P11), or
`system/init` does not report `claude-haiku-5-5`.
- Proves the handoff, the `--resume <uuid>` launch on the host and that the
  conversation resumed there. A host `claude` that cannot sign in over SSH
  (setup P11) reads FAIL: the first turn errors.
- Lives in: as session-drop-in.
- Reached by: as session-drop-in, with the terminal over `/ws`.
- Traps: the project folder is `grant-dropin-<nonce>` under the state folder so
  verify-fixtures-removed finds it. A killed run leaves the host, project and
  terminal until that journey runs.

**session-drop-in-tool-refused** (screen). A headless agent session in Acme
Corp (same model and settings) is asked to run `sleep 20` with Bash. When the
`tool_use` `content_block_stop` frame arrives, `POST /api/sessions/{id}/drop-in`
with P4 must answer 409 `tool_running` with Bash named in its message, in under
5 s. PASS also needs the agent still `live` in the list, no terminal started by
the call, and exactly one `op=session.drop_in` line for the session with
`status` denied and `error` tool_running. BLOCKED when no tool call arrives
within 60 s, or `system/init` is not `claude-haiku-5-5`. The session is
deleted whatever the outcome.
- Lives in: `internal/sessions/session/dropin.go` (`Handoff`), `cmd/relay/session_dropin.go`.
- Reached by: as session-drop-in.

**chief-of-staff-send** (screen). A headless agent session in Acme Corp
(model `haiku`, settings `{"headless":true,"agent":true}`) takes two turns.
Two connections are dialled on `/ws` before the launch with the run
credential: an observer, and one with the header `X-Relay-Scope:
chief-of-staff`. The person's turn is `POST /api/sessions/{id}/message` with
"Reply with exactly: verify-<nonce>-person" and a forged `"origin":
"chief-of-staff"` in the body. The Chief of Staff's turn is `POST
/api/chief-of-staff/messages` with the scope header, "Reply with exactly:
verify-<nonce>-cos". PASS when the send answers 202 with origin
`chief-of-staff`; the scoped connection sees running, a `turn_done` whose
excerpt holds the cos marker, then idle, and the scoped `GET /api/sessions`
row is idle; scoped `GET /api/projects` and scoped `POST
/api/sessions/{id}/message` answer 403, and unscoped `POST
/api/chief-of-staff/messages` answers 403; a `join_session` frame on the scoped
connection closes it with 1008 and no `session_joined` arrives; the observer's
live `user_message` frames and its re-joined history carry no origin on the
person's text and `chief-of-staff` on the cos text; `relay audit --event
session_message` has, for the session, exactly one intent (outcome `pending`)
and one completion (`ok`) sharing an id, with `args.origin` `chief-of-staff`,
`args.session_id` the session and a `ts`; the intent's `text_bytes` equals the byte length of the sent text, and its `text`, when logged, contains `-cos`; no
`session_message` row contains the person marker; and `relay audit --event
control_decision` has denied rows with `outside chief-of-staff scope` for the
two scoped requests and `class not granted` for the unscoped one. BLOCKED when
the `system/init` model is not `claude-haiku-5-5`, a launch is
refused, the run credential is missing, or the audit log is unreadable. The session
is ended and deleted whatever the outcome.
- Lives in: `cmd/relay/session_chief_of_staff.go`, `cmd/relay/api_credential.go`
  (the scope), `cmd/relay/frontend_dispatcher.go` (read-only `/ws`),
  `internal/sessions/hostapi` (`POST /send`), `internal/sessions/session`
  (`SendMessageAs`, `MarkOrigins`), `internal/audit`.
- Reached by: `POST /api/sessions` with P4; the run credential for the message
  route, `/ws` and the delete; `relay audit --event session_message --json` and
  `--event control_decision --json`.
- Traps: the person's turn is awaited to idle before the send, or the send
  answers 409 `already_processing`. The control_decision rows are matched by
  method, path and time, so a scoped `GET /api/projects` from another caller in
  the same second could satisfy the check. The session list is read with the
  scope header, which narrows the run credential for that request only. Run on
  Claude Haiku `claude-haiku-5-5`; any other model reads BLOCKED.

**cos-start-host** (screen). The Chief of Staff starts an agent in a project on
an SSH host. A host `loopback-<nonce>-cos` targeting `localhost` and a project
`Drop-in Host cos <nonce>` on a fresh folder are created (the project create
raises a presence prompt, answered by the helper). A caller minted with classes
proxy and execute sends `POST /api/chief-of-staff/sessions` with the scope
header, `haiku` and "Reply with exactly: verify-<nonce>-host". PASS when the
start answers 201 with origin `chief-of-staff` and mode `headless`; a
`turn_done` frame holds the marker, then idle; the scoped `GET /api/sessions`
row has origin `chief-of-staff`; the local process list holds an `ssh ... -T --`
command whose base64 script, decoded, sets `RELAY_SESSION_ID` to the session
id; a scoped `POST /api/chief-of-staff/messages` answers 202 and a second
`turn_done` holds "verify-<nonce>-host2"; the audit has an ok `session_launch`
row with origin `chief-of-staff` and `host_id` the host, and two intent and two
completion `session_message` rows (the start prompt and the send), all with that
origin. The session, project and host are deleted whatever the outcome. BLOCKED
when the host or project cannot be created (setup P11), the audit is
unreadable or a credential is refused; FAIL when the host's `claude` never
answers.
- Lives in: `cmd/relay/session_chief_of_staff_start.go`, `cmd/relay/session_chief_of_staff.go`, `cmd/relay/session_launch.go`,
  `internal/sshhost`.
- Reached by: the minted credential for the start, send and list; the run
  credential for host, project and delete; `/ws`; `relay audit --json`.
- Traps: needs setup P11. Every wait is a response, the presence dialog result or
  a `turn_done` frame; the process list is read once, after the first turn.

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

**settings-window-services** (screen). The harness reads Accessibility
itself, through cgo, so it needs no other driver. It starts the crash service
through the API, opens Settings from the tray (the status item with help
"Relay", then "Settings..."), presses the Services tab and finds the crash
service's card by its display name. Stop is pressed on that card; the window
must read `stopped` with a Start button, and `relay service list` must show
the service stopped within 5 s. Start is pressed; the window must show a new
pid within 10 s and `service.list` must agree. The crash service is then
stopped through the API, and the window closed if the journey opened it.
- Lives in: `cmd/relay/trayapp.go` (the Settings... item),
  `cmd/relay/ipc_services.go` (`ipcStartService`, `ipcStopService`),
  `cmd/relay/ipc_handlers.go` (dispatch, `serviceStatusEventPayload`),
  `web/src/app.js` (`renderServices`, `toggleServiceRunning`,
  `onServiceStatus`).
- Reached by: Accessibility from the harness; the API for the fixture's start
  and restore; `service.list` plus `pgrep` for state.
- Traps: BLOCKED under the LaunchAgent recipe, where the process leading the
  job holds no Accessibility grant. A Services Edit form left open hides the
  cards.

**cos-settings** (screen). The Chief of Staff panel in Settings > Projects
saves what the person picks. The journey reads `GET /api/chief-of-staff/config`
with the run credential, opens Settings from the tray (as
settings-window-services), presses Projects and chooses Haiku in the model
pop-up. The page must show the static text `Verify Grant <nonce>: It doesn't
allow the claude-code template.` The journey types that project's full name
into the project pop-up; the pop-up must never show it within 5 s. It then
types Acme's name, and the pop-up must show it. GET must show
`configured:true`, Acme's id and model `haiku` within 5 s (polled every
200 ms; the WebView raises no event a harness can read). The earlier setting
is then put back (`PUT` of the earlier block, or `DELETE` when it was unset),
read back, and the window closed if the journey opened it. The restore runs on
every path past the first read; a restore that does not read back is FAIL.
- Lives in: `web/src/app.js` and `web/src/lib/chief_of_staff.js` (the panel),
  `cmd/relay/ipc_projects.go` (`set_chief_of_staff`),
  `cmd/relay/project_routes.go` (the three routes), `internal/config/chief_of_staff.go`
  (the suitability table).
- Reached by: Accessibility from the harness; the run credential for the
  routes (read and configure classes, no presence prompt). A pop-up is the
  `AXPopUpButton` whose AXTitle is its `aria-label`; AXValue is the selected
  option. The harness never opens a pop-up: an open native menu makes every
  read of the page fail. It sets `AXFocused` on the pop-up and posts the
  option's text to relay as key events (`CGEventPostToPid`, no Return), and
  WebKit's type-ahead selects the option and fires change.
- Traps: Verify Grant's absence from the pop-up is shown by bounded
  observation, because an absent option raises nothing. BLOCKED when Acme
  shows a reason line (fixture), when gate-project-grant-pos left no Verify
  Grant project, with no run credential, under the LaunchAgent recipe, and
  without the Accessibility grant (P7). Posting keys needs the same grant.

**slow-route-keepalive** (screen). A relay route that runs past the 10 s
read deadline must not poison the connection it arrived on. The journey adds
host `blackhole-<nonce>` (target `192.0.2.1`, which drops packets) and
project `Unreachable Host <nonce>` on it, then on one keep-alive connection
sends `GET /api/models`, `GET /api/projects/{id}/persistent-sessions` and
`GET /api/models` again. PASS when the slow list answers 502 after 10 s on
the reused connection and the second models call answers 200 on it too. The
project and host are deleted after.
- Lives in: `cmd/relay/frontend_server.go` (`setFrontendRouteReadDeadline`),
  `cmd/relay/persistent_session_routes.go`, `cmd/relay/host_routes.go`.
- Reached by: `POST /api/hosts` (class `configure`, no prompt, bounded at
  40 s for the ssh probe), `POST /api/projects` through the helper (the
  `project.grant` prompt), then the three GETs with the run credential over
  one client that holds a single connection.
- Traps: a build without the fix reads FAIL, the second models call a 502
  `context canceled`. BLOCKED when the slow list answers anything but 502,
  answers in under 10 s or on a new connection: then nothing was proven.
  `tmux_path` is set, or the list answers 409 without running ssh.

**session-host-restart** (screen). The admin op `service.restart
{"id":"relaysessions"}` answers without error, and within 15 s
`relaysessions` is STATE `running` with a `relay-sessions` pid it did not
have before. Runs last among the feature journeys: it restarts the session
host under every live session.
- Lives in: `cmd/relay/service_ops.go` (`Restart` resynthesizes the built-in
  record), `internal/service` (the registry).
- Reached by: the bridge admin op, ungated, as `relay service restart --id
  relaysessions` sends it; STATE from `service.list`, the process from
  `pgrep -f` on `relay-sessions service`, polled every 200 ms.
- Traps: BLOCKED when the host is not running with a process beforehand. A
  build that stops the host and then fails validation reads FAIL with the
  op's error; the host stays down until Relay.app is relaunched.

**verify-fixtures-removed** (screen). The `devboxverify-extra-args` terminal
template is deleted if present and checked gone. Every `devboxverify-probe-*` MCP and
`devboxverify-crash-*` service is unregistered (both ungated) and every
`Verify Grant *` and `Unreachable Host *` project deleted, from this run or a
crashed one, with their state folders. Every `Drop-in Host *` project is deleted
too. Then every host named `blackhole-*` with target `192.0.2.1`, and every host
named `loopback-*` with target `localhost`, is deleted. Every terminal whose directory is under a
`grant-*` state folder or the World root is deleted, whatever its state. PASS
when none is left and no prompt appeared.
- Lives in: `cmd/relay/mcp_ops.go`, `cmd/relay/service_ops.go`,
  `cmd/relay/project_ops.go`, `cmd/relay/host_routes.go`,
  `internal/sessions/api/http_terminal.go`.
- Traps: without the run credential the projects, hosts and terminals stay,
  the rest is still removed, and the journey reads BLOCKED. A terminal left
  behind makes eve's next page load open on it instead of Home.

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
- **P7.** The app that runs devboxverify holds Accessibility: add the desktop
  Terminal under System Settings > Privacy & Security > Accessibility.
  Without it settings-window-services reads BLOCKED.
- **P8. Verify Skills.** A project whose chat sees 48 relay tools and 48
  skills that cover them. Needs the `configure` credential of P5 for the setup
  alone.
  1. Build the test MCP as in P5 step 1. It serves the 48-tool catalogue when
     `RELAY_TESTMCP_CATALOG=wide`.
  2. Make the project folder and write its skills (one per domain, each
     listing its one tool):

     ```bash
     mkdir -p ~/verify-skills
     ~/.local/bin/devboxverify-testmcp --write-skills ~/verify-skills
     ```

  3. Create the project with the P5 curl, body below, then set its folder to
     `~/verify-skills` in Settings. It prompts once. Add `chat` to its allowed
     templates and, if the model list is restricted, the `RELAY_VERIFY_MODEL`
     model.

     ```json
     {"name":"Verify Skills","kind":"local","allowed_mcp_ids":["devboxverify-wide"]}
     ```

  4. From a desktop Terminal (it prompts), register the MCP under its own id:

     ```bash
     relay mcp register --id devboxverify-wide --name "devboxverify wide catalogue" --command ~/.local/bin/devboxverify-testmcp --env RELAY_TESTMCP_CATALOG=wide
     ```

  The model host must return token usage for chat completions, or the journey
  reads `prompt_tokens` 0 and FAILs. The Child B eve journey uses the same
  project: it asks for the tide code of a port and `tides_lookup` answers
  `TIDE-` plus the first 8 hex digits of the SHA-256 of the lower-cased,
  trimmed port name.
- **P9. Agent state.** The `session-agent-state` journey launches a `claude-code`
  session on `haiku`. In Settings, Acme Corp's allowed templates include
  `claude-code`; the `claude-code` template exists; and, if Acme restricts
  models, Acme allows `haiku`. Without it the journey reads BLOCKED on the
  launch refusal.
- **P10. Codex.** The `session-codex` journey launches `codex/gpt-6-luna`. Done
  once on the devbox console, like P9: add a `codex` console template with
  read `/opt/homebrew`, `~/.gitconfig`, `~/.zshenv`, `~/.zprofile`, `~/.zshrc`
  and read_write `~/.codex`, `~/.cache`, `~/Library/Caches`; add `codex` to
  Acme Corp's allowed templates (this widens a grant, so it raises a presence
  prompt); and, if Acme restricts models, allow `codex/gpt-6-luna`. Codex must
  be installed and signed in. Without it the journey FAILs on the launch
  refusal or the missing model row.
- **P11. Loopback SSH.** The `session-drop-in-host` and `cos-start-host` journeys add a host
  that targets `localhost`, so relay runs `claude` over SSH to the box itself.
  The box's own public key is in the login user's `authorized_keys`, the SSH
  client trusts the box's host key without a prompt, and `claude` is on the
  login shell's PATH for a non-interactive SSH command (check with
  `ssh localhost 'command -v claude'`). Without the key or the host-key trust
  the journey reads BLOCKED; with no `claude` on that PATH it reads FAIL ("no
  system/init"), because the first turn never starts. That `claude` must also
  sign in for SSH logins. An SSH session starts with the login keychain
  locked, and an unlock in one SSH session does not carry to the next, so the
  login user's `~/.zshenv` unlocks it when `SSH_CONNECTION` is set (check with
  `ssh localhost 'claude -p "say ok" </dev/null'`). Without it the first turn
  errors and the journey reads FAIL.

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
   prompts and the installed helper trusted for Accessibility. Under that
   recipe settings-window-services reads BLOCKED, because the process leading
   the job holds no Accessibility grant of its own, and the status reads
   `error`; run from a desktop Terminal to get a verdict.
3. Preflight refuses unless the app was built from the PR head with a clean
   tree, is the one running, and started after it was installed; then
   `repair.sh` must report bootstrap complete and the world green, repaired
   at most once. Only then does `reset.sh` run.
4. Rebuild from `main` when done, so the app is left on `main`.

## Traps

- Run from an operator shell, never inside a relay session. Relay refuses a
  sandbox attach from inside one.
- `build.sh` signs and relaunches the app. Unlock the signing keychain first,
  or the build fails at `codesign`.
- A stale or incomplete bootstrap (a helper build out of date, say) fails
  preflight as `BLOCKED environment: bootstrap incomplete; needs a person: …;
  run bootstrap.sh`. `repair.sh` checks it before any reset because reset
  takes the world down before it checks bootstrap, and would leave it down.
- Audit rows are recorded asynchronously and read back for up to 5 s. A
  dropped row reads as FAIL, not as a pass.
- A regression that accepts the blank-model launch, or one whose `DELETE`
  fails in session-chat-lifecycle or terminal-lifecycle, leaves a session
  running in Acme Corp. The detail names it; stop it by hand.
- A screen run that dies mid-way leaves its fixtures; the next run's
  verify-fixtures-removed removes them. A credential it minted expires within
  the hour.
- A crashed slow-route-keepalive leaves its Unreachable Host project. On a
  build without the read-deadline fix, eve lists that project's persistent
  sessions on every page load, and each list poisons eve's connection: every
  chat, shell and model call answers 502 until verify-fixtures-removed runs
  or relay restarts.
- Never touch the Settings window or the tray menu during a run. The
  journey drives them, and a click lands on whatever is in front.
- Never touch a Relay dialog during a run. The helper answers or cancels the
  harness's own prompts, and the sweep after each screen journey cancels any
  left open.
- A stale-derived-access-edit FAIL "stale file_dirs was kept" can also mean
  `devboxverify-scope` is registered but not connected, so relay holds no
  schema for it. Check it in Settings before reading the FAIL as a
  regression. This can never produce a false PASS.
- The two instance journeys leave `/tmp/dbv-oauth` and `/tmp/dbv-sealed` and
  one login-keychain item each (`config-seal-key.<hash>`). The next run
  recreates the dirs and reuses the items; the harness never deletes a
  keychain item, since `security delete-generic-password` can raise a dialog.
  A run that crashed leaves a `relay serve` there; the next run stops it
  first.
- Never edit the fixtures from Settings. Any edit with the MCP connected
  drops Verify Stale's `file_dirs`.
- When showing red then green, run the red build first. A fixed build spends
  Verify Stale.
- Never post raw audit rows. They carry the home path. The tool writes its own
  details and scrubs the home directory from what it posts. Details never
  carry a token; anything shaped like one is redacted.
