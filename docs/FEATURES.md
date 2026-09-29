# Relay feature map

What a person uses relay for, the features that serve each goal, and the
devbox journey that proves it works today. Journeys follow goals end to end.
A PR that changes a mapped feature updates this file.

Columns:

- **Door**: how a journey can drive it without the screen. `HTTP` is a route
  on the frontend socket (credential class in brackets), `CLI` a `relay` verb,
  `bridge` the bridge socket, `screen` an IPC-only Settings or tray action
  with no other door.
- **Gate**: `owner gate` marks a step that needs the owner's credential or
  presence: every op in `presence.GatedOps`, and the passkey ceremony. An
  agent inside the product must never complete one. The devbox harness may,
  with the operator's test credentials. See [Owner gates](#owner-gates).
- **Journey**: an existing `devboxverify` journey, `preflight` (the read-only
  `relay grant --json` check the world runs before any journey), or `none`.
  A journey marked NOTRUN always reads NOTRUN and covers nothing today.

Priority is per goal: **must-have** means used daily and a silent break
strands the user; **should** means weekly or a break is loud; **later** means
rare or already guarded by unit tests alone.

## Goals

### G1 · Run an agent session in a project — must-have
Intent: start a chat, Claude, pi or terminal session in a project and work in it.
It worked: the session starts in the project folder, answers, and stops when told; it cannot reach another project.
Why must-have: eve's everyday path; every chat goes through it.
Areas: sessions, sandbox, templates, audit.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Launch a session | API (eve) | eve starts a session → `POST /api/sessions` | HTTP [execute] | — | session-chat-lifecycle, model-list-and-completion |
| Blank-model launch refused | API | `POST /api/sessions` with no model | HTTP [execute] | — | blank-model-refused |
| System-model chat launch refused | API (eve) | `POST /api/sessions` for a chat session on a model relayLLM marks `system` → 403 | HTTP [execute] | — | none |
| Message and stop a session | API (eve) | eve chat → `POST /api/sessions/{id}/message`, `DELETE /api/sessions/{id}`; `GET /api/sessions` | HTTP [proxy] | — | session-chat-lifecycle |
| Resume a session | API (eve) | `POST /api/sessions/{id}/resume` | HTTP [execute] | — | session-chat-resume |
| Terminals and their log | API (eve) | eve terminal → `POST /api/terminals` [execute]; `GET /api/terminals`, `GET /api/terminals/{id}/log`, `DELETE` [proxy] | HTTP | — | terminal-lifecycle, verify-fixtures-removed |
| Persistent sessions | API | `GET`/`DELETE /api/projects/{id}/persistent-sessions` | HTTP | — | slow-route-keepalive (`GET`, unreachable host) |
| Proxied calls after a slow relay route | API (eve) | eve's keep-alive socket: a relay route slower than 10 s, then `GET /api/models` | HTTP [read, proxy] | — | slow-route-keepalive |
| `relay sandbox <template>` | CLI | from a project folder, `relay sandbox world-probe` | CLI / bridge | — | acme-sandbox-reach |
| Sandbox containment (own project only) | sandbox | any sandboxed session | bridge | — | acme-sandbox-reach |
| Launch audit row, size-capped | audit | any refused launch | bridge | — | oversized-launch-audit-capped |
| Permission-mode restart (SSH hosts) | API | change permission mode on an SSH-host session | HTTP | — | permission-mode-restart (always NOTRUN: world has no hosts) |
| Terminal templates: list, add, edit, remove | Settings > Templates | Settings > Templates > Add | HTTP `/api/terminal/templates` | — | none |

### G2 · Give an agent access to one project and nothing else — must-have
Intent: grant a project its folder, mail account and chosen tools, and nothing wider.
It worked: `relay grant` shows exactly what was granted, and a session in it is refused everything else.
Why must-have: the product's security promise; a silent widening is the worst failure relay has.
Areas: projects, grants, sandbox.

Creating a project and widening a grant are `project.grant`, an owner gate. Journeys cover the gate itself (the harness creates Verify Grant as the owner; a session has no door to it), the effect of an approved grant (preflight, acme-sandbox-reach) and the ungated edits: narrowing that takes effect in a live session, and re-saves that change nothing.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Create a project | Settings > Projects | Settings > Projects > Add project | HTTP `POST /api/projects` [configure] | owner gate | gate-project-grant-pos, gate-project-grant-neg |
| Widen a grant (MCPs, tools, access, scope) | Settings > Projects | project > Edit > Save | HTTP `PUT /api/projects/{id}` [configure] | owner gate | gate-project-grant-neg |
| Narrow a grant or ungated edit | Settings > Projects | project > Edit > Save | HTTP `PUT /api/projects/{id}` [configure] | — | grant-narrowing-live, stale-derived-access-edit, context-number-resave |
| Local-to-remote conversion | Settings > Projects | project > Edit > kind | HTTP `PUT` | owner gate | v1-conversion-refusal (NOTRUN) |
| Remove a project | Settings > Projects | project > Remove | HTTP `DELETE /api/projects/{id}` | — | verify-fixtures-removed |
| Show a project's grant | CLI | `relay grant --json` | CLI | — | preflight |
| Scope values (`contextSchema`) | Settings > Projects | project form > scope field | HTTP `/api/mcps/{id}/scope_fields` | owner gate if widening | context-number-resave (partial) |
| Default project (home/work) | Settings > Projects | Default project picker | HTTP `PUT /api/default_project/{mode}` | — | none |
| Rotate a project token | Settings > Projects | project > Rotate token | HTTP `POST /api/projects/{id}/rotate_token` [grant] | owner gate | gate-project-rotate-token-pos, gate-project-rotate-token-neg |
| Reveal a project token | Settings > Projects | project > eye icon | screen | — | none |
| Regenerate SKILL.md | Settings > Projects | project > Regen Skill | HTTP `POST /api/projects/{id}/regen_skill` | — | none |

### G3 · Add a tool and let a project use it — must-have
Intent: register an MCP server and have a project's agents call its tools.
It worked: the tools appear in the project's session, a call returns, and a disabled tool is refused.
Why must-have: every mail, calendar and file action an agent takes goes through the bridge.
Areas: mcps, grants, audit.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Register an MCP (stdio or HTTP) | Settings > MCP Servers, CLI | `relay mcp register`, or Add | CLI, HTTP `POST /api/mcps` | owner gate | gate-mcp-register-pos, gate-mcp-register-neg |
| Authenticate an HTTP MCP (OAuth) | Settings > MCP Servers | row > Authenticate | screen | owner gate | gate-mcp-oauth-start-pos (NOTRUN), gate-mcp-oauth-start-neg |
| List MCPs and their tools | Settings, CLI | `relay mcp list`; `GET /api/mcps/{id}/tools` | CLI, HTTP | — | gate-mcp-register-pos |
| Tool listing and calls through the bridge | bridge | a session's `relay mcp --token`; `relay mcp call --token` | bridge, CLI | — | acme-tools-through-bridge |
| Disable tools per project | Settings > Projects | project form > tool picker | HTTP `PUT /api/projects/{id}` `disabled_tools` [configure] | — | disabled-tool-refused |
| Unregister an MCP | Settings, CLI | `relay mcp unregister`, or Remove | CLI, HTTP `DELETE /api/mcps/{id}` | — | verify-fixtures-removed |
| Reset MCP permissions (macOS TCC) | Settings > MCP Servers | row > Reset permissions | screen | — | none |

### G4 · See what an agent touched — must-have
Intent: after a session, look up which tools it called, on what, and what was refused.
It worked: every call and refusal is in the log with its project, tool and outcome, and nothing is missing.
Why must-have: the only after-the-fact check on an agent; a dropped row is invisible.
Areas: audit.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Tail and filter the log | CLI | `relay audit --event …` | CLI | — | context-number-resave (reads `config_change`) |
| Query and filter in Settings | Settings > Tool Calls | filter form | HTTP `GET /api/audit` [read] | — | none |
| Tool-call rows (one row per local call; intent then completion for a remote one) | background | any bridge call | bridge | — | tool-call-audited |
| Issuance and config-change rows carrying the presence id | background | any owner gate passed | CLI, HTTP | owner gate | gate-credential-mint-pos, gate-mcp-register-pos, gate-project-grant-pos, gate-service-register-pos, gate-project-rotate-token-pos, gate-eve-enrolment-open-pos, gate-credential-revoke-pos |
| Session launch rows | background | any launch | HTTP, bridge | — | session-chat-lifecycle, terminal-lifecycle |
| Refusal rows | background | any refused launch | HTTP, bridge | — | blank-model-refused, oversized-launch-audit-capped |
| Export the log | Settings > Tool Calls | Export | HTTP `POST /api/audit/export` [configure] | — | none |
| Reveal the log file | Settings > Tool Calls | Reveal log | screen | — | none |

### G5 · Keep background services running, including scheduled work — must-have
Intent: run relayLLM, eve, the scheduler and other services under relay, started at login and restarted on a crash.
It worked: the services are up after login, a crashed one comes back, and the tray and `relay service list` show the true state.
Why must-have: eve, chat and scheduled runs all sit on this; a service that stays down after a crash is noticed late. Scheduling itself lives in relayScheduler; relay's part is keeping it running.
Areas: services, tray.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Register a service (command, capabilities) | Settings > Services, CLI | `relay service register`, or Add Service | CLI, HTTP `POST /api/services` | owner gate | gate-service-register-pos, gate-service-register-neg |
| Edit a service | Settings > Services | row > Edit | HTTP `PUT /api/services/{id}` | owner gate if fields change | none |
| Start, stop, restart | Settings, tray, CLI | tray service row; `relay service restart` | CLI, HTTP `POST /api/services/{id}/start`, `/stop` [configure] | — | service-start-stop, session-host-restart |
| Autostart at login | Settings > Services | row > autostart | HTTP `PUT /api/services/{id}/autostart` | — | none |
| Restart on crash, then `failed` after max attempts | background | a service exits unrequested | CLI `relay service list` (STATE) | — | service-restart-on-crash (the restart, not `failed`) |
| Unregister | Settings, CLI | `relay service unregister` | CLI, HTTP `DELETE` | — | verify-fixtures-removed |
| Menu visibility and order | Settings > Services | row > menu checkbox; drag | HTTP `PUT /api/services/{id}/menu`, `/position` | — | none |
| Service actions and config (manifest) | Settings > Service Inspector | service > action or Config | screen | — | none |
| Reveal a service's log | Settings > Service Inspector | Reveal log | screen | — | none |

### G6 · Let a session use a model — must-have
Intent: pick a model for a project or service and have its sessions reach it through relay.
It worked: the model list shows the configured models, and a chat gets an answer from the chosen one.
Why must-have: every chat turn crosses the model endpoint.
Areas: models, sessions, audit.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Model list | Settings > Projects, API | model picker; `GET /api/models` (system-only models omitted) | HTTP [proxy] | — | model-list-and-completion |
| Pick a model per project or service | Settings | project form > Model; service > allowed models | HTTP `PUT /api/projects/{id}` | — | none |
| Model endpoint (`/v1/chat/completions`, `/v1/models`, passthrough) | socket, optional TCP | a session or relayLLM calls `model.sock` | HTTP (model socket) | — | model-list-and-completion (one chat turn and its `model_call` row) |
| Model keys (`rmk_`) for sessions | background | minted at session launch | HTTP (model socket) | — | none |

### G7 · Sign in from a browser — should
Intent: reach relay's and eve's web pages from a browser with a passkey.
It worked: a registered passkey signs in; a revoked one no longer does.
Why should: used when away from the Mac; a break is loud, since the sign-in fails in front of the user.
Areas: login.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Mint a login code | tray, CLI | tray > Show Login Code…; `relay login enrol` | CLI | owner gate | gate-login-bootstrap-mint-pos (NOTRUN), gate-login-bootstrap-mint-neg |
| Register a passkey | login page | `/relay/login` > code > Register | browser | owner gate (ceremony) | none |
| Sign in with a passkey | login page | `/relay/login` > passkey | browser | owner gate (ceremony) | none |
| List passkeys, sign out a session | Settings > Passkeys, CLI | `relay login list` | CLI, screen | — | none |
| Revoke a passkey | Settings > Passkeys, CLI | `relay login revoke --id` | CLI | owner gate | gate-login-passkey-revoke-pos (NOTRUN), gate-login-passkey-revoke-neg |
| Open eve passkey enrolment | tray, CLI | tray > Allow Eve Passkey Enrolment…; `relay eve enrol` | CLI | owner gate | gate-eve-enrolment-open-pos, gate-eve-enrolment-open-neg |
| List and revoke eve passkeys | Settings > Passkeys, CLI | `relay eve list`, `relay eve revoke` | CLI | revoke is an owner gate | gate-eve-passkey-revoke-pos (NOTRUN), gate-eve-passkey-revoke-neg |

### G8 · Operate relay from the tray and Settings — should
Intent: see at a glance that relay is healthy and get to what needs attention.
It worked: Settings opens, Overview's tiles and attention list match reality, and the tray shows each service's state.
Why should: used daily, but a break is visible at once.
Areas: tray, settings-ui.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Open Settings | tray | tray > Settings… (⌘,) | screen | — | none |
| Overview tiles and attention list | Settings > Overview | Overview | screen | — | none |
| Recent tool calls (call_tool rows only) | Settings > Overview | Overview > Recent tool calls | screen | — | none |
| Reveal config and logs folders | Settings > Overview | Reveal config / Reveal logs | screen | — | none |
| Service rows with state | tray | menu bar icon | screen | — | none |
| Pending enrolment line and notification | tray | tray line or banner → Remote Clients | screen | — | none |
| Sealed-store warning | tray | menu bar icon | screen | — | none |
| Quit Relay | tray | tray > Quit Relay | screen | — | none |

### G9 · Let a script or another tool drive relay — should
Intent: give a script a scoped credential for relay's control plane.
It worked: the credential's class allows what it should and nothing more; a revoked one gets 401.
Why should: every journey and several clients depend on it, but a break is loud.
Areas: credentials.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Mint a credential | CLI | `relay credential mint --class …` | CLI | owner gate | gate-credential-mint-pos, execute-credential-renewal, gate-credential-mint-neg |
| List credentials | CLI | `relay credential list` | CLI | — | gate-credential-mint-pos |
| Revoke a credential | CLI | `relay credential revoke --id` | CLI | owner gate | gate-credential-revoke-pos, gate-credential-revoke-neg |
| Class enforcement on `/api/*` | API | any route with a bearer | HTTP | — | every journey (implicitly) |

### G10 · Give a remote machine access — later
Intent: let another machine reach chosen projects over mTLS.
It worked: an approved client reaches its projects and only those; a revoked one is refused.
Why later: set up rarely; almost every step is an owner gate.
Areas: remote.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Configure the remote listener | Settings > Remote Clients | listener form | screen | owner gate | gate-remote-configure-pos (NOTRUN), gate-remote-configure-neg |
| Lodge, list, refuse an enrolment request | CLI, Settings | `relay enrol requests`, `relay enrol refuse` | CLI, HTTP `/api/enrolments` | — | none |
| Approve, sign, create, update, revoke | CLI, Settings | `relay enrol approve …` | CLI | owner gate | gate-enrolment-create-pos, gate-enrolment-sign-pos, gate-enrolment-update-pos, gate-enrolment-revoke-pos (all NOTRUN); gate-enrolment-create-neg, gate-enrolment-sign-neg, gate-enrolment-update-neg, gate-enrolment-revoke-neg |
| CA fingerprint | CLI | `relay enrol ca-fingerprint` | CLI | — | none |
| Remote calls (fail-closed, audited) | mTLS listener | an enrolled client calls a tool | mTLS | — | none |

### G11 · Work on a remote directory over SSH — later
Intent: treat a folder on another machine as a project.
It worked: sessions and tools run on the host through one SSH connection.
Why later: the devbox world has no hosts; only slow-route-keepalive adds one,
an unreachable host it removes again.
Areas: hosts.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Add, edit, remove a host | Settings > Hosts | Add host | HTTP `/api/hosts` [configure] | — | slow-route-keepalive (add, remove), verify-fixtures-removed (remove) |
| Probe, disconnect | Settings > Hosts | row > Probe / Disconnect | HTTP `/api/hosts/{id}/probe`, `/disconnect` | — | none |
| Host templates | Settings > Hosts | row > Templates | HTTP `/api/hosts/{id}/templates` | — | none |

### G12 · Recover from a broken sealed store — later
Intent: start over when the keychain key is lost.
It worked: relay names what it destroys, and starts clean.
Why later: break-glass only; it is an owner gate by design.
Areas: sealed.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Reset Sealed Store… | tray | tray > Reset Sealed Store… | screen | owner gate | gate-sealed-reset-pos (NOTRUN), gate-sealed-reset-neg |

## Owner gates

An owner gate is a step that needs the owner's credential or presence. An
agent inside the product (a relay session, or eve's chat agent) must never
complete one. The devbox harness may, with the operator's test credentials:
`devboxpresence` answers relay's presence prompt with the devbox admin
password. There is never an API or flag that skips a gate.

Each gate has two journeys. The **positive** passes the gate as the owner
would and checks the effect and its audit row with the presence id. The
**negative** plays an in-product agent: inside a world-probe session in
Acme Corp it attempts the step, the helper cancels the prompt, and the
journey checks that presence refused it and nothing changed. Relay records
no row for a cancelled prompt today, so no negative checks the audit. An op
with no door from a session reads PASS when the session cannot reach it at
all: its HTTP route is outside the sandbox and the bridge answers `unknown
admin operation`. No prompt appears and nothing is audited.

The passkey ceremony (register, sign in) is an owner gate with no journey
here.

| Op | Protects | Positive | Negative |
|---|---|---|---|
| `credential.mint` | issuing a control-plane credential | gate-credential-mint-pos (mints the run credential); execute-credential-renewal (renews P4 when due) | gate-credential-mint-neg |
| `credential.revoke` | revoking a credential | gate-credential-revoke-pos (revokes the run credential) | gate-credential-revoke-neg |
| `mcp.register` | a command relay will run as an MCP | gate-mcp-register-pos | gate-mcp-register-neg |
| `mcp.oauth.start` | authenticating an HTTP MCP | gate-mcp-oauth-start-pos: NOTRUN, the door is IPC-only and needs a real OAuth provider | gate-mcp-oauth-start-neg (no door from a session) |
| `service.register` | a command relay will run as a service | gate-service-register-pos | gate-service-register-neg |
| `project.grant` | creating a project or widening a grant | gate-project-grant-pos | gate-project-grant-neg (no door from a session) |
| `project.rotate_token` | a project's bearer token | gate-project-rotate-token-pos | gate-project-rotate-token-neg (no door from a session) |
| `remote.configure` | the mTLS listener | gate-remote-configure-pos: NOTRUN, it changes the live listener the VM stack uses | gate-remote-configure-neg (no door from a session) |
| `enrolment.create` | issuing a remote identity | gate-enrolment-create-pos: NOTRUN, remote identities are out of scope until G10 | gate-enrolment-create-neg |
| `enrolment.sign` | signing a remote client's certificate | gate-enrolment-sign-pos: NOTRUN, as above | gate-enrolment-sign-neg |
| `enrolment.update` | changing a remote client's grants | gate-enrolment-update-pos: NOTRUN, as above | gate-enrolment-update-neg |
| `enrolment.revoke` | revoking a remote client | gate-enrolment-revoke-pos: NOTRUN, as above | gate-enrolment-revoke-neg |
| `login.bootstrap.mint` | a code that registers a browser passkey | gate-login-bootstrap-mint-pos: NOTRUN, the code is redeemed only by the browser ceremony | gate-login-bootstrap-mint-neg |
| `login.passkey.revoke` | revoking a relay passkey | gate-login-passkey-revoke-pos: NOTRUN, there is no disposable relay passkey | gate-login-passkey-revoke-neg |
| `eve.enrolment.open` | eve's five-minute passkey enrolment window | gate-eve-enrolment-open-pos (closes the window after) | gate-eve-enrolment-open-neg |
| `eve.passkey.revoke` | revoking an eve passkey | gate-eve-passkey-revoke-pos: NOTRUN, relay keeps one eve passkey mirror that each eve's report replaces, so no verify passkey can be revoked through it | gate-eve-passkey-revoke-neg: NOTRUN while the mirror holds no revocable passkey, since relay refuses an unknown or last id before the gate |
| `sealed.reset` | the sealed store | gate-sealed-reset-pos: NOTRUN, break-glass destroys the store | gate-sealed-reset-neg (no door from a session) |

## Areas

Each area has a stable name, the code it owns, the tests that cover it and
the journeys that exercise it. Goals and features above use these names.
`cmd/relay` is one Go package, so its `tests` globs pick files for a reviewer
and for `-run` selection, not a separate `go test` target.

```yaml
areas:
  sessions:
    code: [cmd/relay/session_*.go, cmd/relay/router_sessions.go, cmd/relay/sessionhost_client.go, cmd/relay/persistent_session_*.go, cmd/relay/mount_session.go, cmd/relaysessions/**, internal/sessions/**]
    tests: [cmd/relay/session_*_test.go, cmd/relay/router_sessions_test.go, cmd/relay/mount_session_test.go, cmd/relaysessions/*_test.go, internal/sessions/**/*_test.go]
    journeys: [blank-model-refused, permission-mode-restart, oversized-launch-audit-capped, acme-sandbox-reach, session-chat-lifecycle, terminal-lifecycle, model-list-and-completion, session-chat-resume, slow-route-keepalive, session-host-restart, verify-fixtures-removed]
  sandbox:
    code: [cmd/relay/sandbox_*.go, cmd/relay/session_sandbox*.go, internal/bridge/sandbox*.go, internal/sessions/sandbox/**]
    tests: [cmd/relay/sandbox_*_test.go, cmd/relay/session_sandbox*_test.go, internal/sessions/sandbox/**/*_test.go]
    journeys: [oversized-launch-audit-capped, acme-sandbox-reach, acme-tools-through-bridge, gate-credential-mint-neg, gate-credential-revoke-neg, gate-mcp-register-neg, gate-service-register-neg, gate-eve-enrolment-open-neg, gate-eve-passkey-revoke-neg, gate-enrolment-create-neg, gate-enrolment-sign-neg, gate-enrolment-update-neg, gate-enrolment-revoke-neg, gate-login-bootstrap-mint-neg, gate-login-passkey-revoke-neg, gate-project-grant-neg, gate-project-rotate-token-neg, gate-remote-configure-neg, gate-mcp-oauth-start-neg, gate-sealed-reset-neg, grant-narrowing-live]
  templates:
    code: [cmd/relay/template_*.go, cmd/relay/ipc_templates.go]
    tests: [cmd/relay/template_*_test.go, cmd/relay/ipc_templates_test.go, cmd/relay/settings_templates_ui_test.go]
    journeys: [terminal-lifecycle]
  projects:
    code: [cmd/relay/project_*.go, cmd/relay/ipc_projects.go, cmd/relay/skills.go, internal/project/**]
    tests: [cmd/relay/project_*_test.go, cmd/relay/ipc_project*_test.go, cmd/relay/settings_project*_test.go, cmd/relay/skills_test.go, internal/project/*_test.go]
    journeys: [v1-conversion-refusal, gate-project-grant-neg, gate-project-rotate-token-neg, gate-project-grant-pos, stale-derived-access-edit, context-number-resave, grant-narrowing-live, gate-project-rotate-token-pos, verify-fixtures-removed]
  grants:
    code: [cmd/relay/grant_cmd.go, cmd/relay/router.go, internal/project/apply.go, internal/project/grant_widening.go, internal/membership/**]
    tests: [cmd/relay/grant_*_test.go, cmd/relay/router_*_test.go, cmd/relay/scope_*_test.go, cmd/relay/settings_scope*_test.go, internal/membership/*_test.go]
    journeys: [acme-sandbox-reach, acme-tools-through-bridge, gate-project-grant-neg, gate-project-grant-pos, stale-derived-access-edit, context-number-resave, disabled-tool-refused, grant-narrowing-live]
  mcps:
    code: [cmd/relay/mcp_*.go, cmd/relay/ipc_mcp*.go, cmd/relay/exec_cmd.go, internal/mcp/**, internal/mcpbroker/**, internal/bridge/**, internal/jsonrpc/**]
    tests: [cmd/relay/mcp_*_test.go, cmd/relay/exec_cmd_test.go, cmd/relay/arg*_test.go, cmd/relay/router_tool*_test.go, internal/mcp/*_test.go, internal/mcpbroker/*_test.go, internal/bridge/*_test.go]
    journeys: [acme-tools-through-bridge, tool-call-audited, gate-mcp-register-neg, gate-mcp-oauth-start-neg, gate-mcp-oauth-start-pos, gate-mcp-register-pos, disabled-tool-refused, verify-fixtures-removed]
  audit:
    code: [cmd/relay/audit_*.go, cmd/relay/ipc_audit.go, internal/audit/**]
    tests: [cmd/relay/audit_*_test.go, cmd/relay/settings_audit_ui_test.go, internal/audit/*_test.go]
    journeys: [blank-model-refused, oversized-launch-audit-capped, tool-call-audited, gate-credential-mint-pos, gate-mcp-register-pos, gate-project-grant-pos, gate-service-register-pos, context-number-resave, session-chat-lifecycle, terminal-lifecycle, model-list-and-completion, session-chat-resume, gate-project-rotate-token-pos, gate-eve-enrolment-open-pos, gate-credential-revoke-pos]
  services:
    code: [cmd/relay/service_*.go, cmd/relay/cli_service.go, cmd/relay/enhanced_services.go, cmd/relay/ipc_service*.go, internal/service/**]
    tests: [cmd/relay/service_*_test.go, cmd/relay/cli_service*_test.go, cmd/relay/enhanced_services*_test.go, cmd/relay/ipc_service*_test.go, cmd/relay/settings_service*_test.go, cmd/relay/launch_*_test.go, internal/service/*_test.go]
    journeys: [gate-service-register-neg, gate-service-register-pos, service-start-stop, service-restart-on-crash, session-host-restart, verify-fixtures-removed]
  models:
    code: [cmd/relay/model_*.go, cmd/relay/router_model_host.go, cmd/relay/frontend_model_guard.go, cmd/relay/relay_llm_channel.go, cmd/relay/ipc_models.go, internal/modelbroker/**]
    tests: [cmd/relay/model_*_test.go, cmd/relay/router_model_host_test.go, cmd/relay/frontend_model_guard_test.go, cmd/relay/relay_llm_channel_test.go, cmd/relay/ipc_models_test.go, cmd/relay/settings_model_picker*_test.go, cmd/relay/integration_*relayllm_test.go, internal/modelbroker/*_test.go]
    journeys: [model-list-and-completion]
  login:
    code: [cmd/relay/login_*.go, cmd/relay/ipc_login.go, cmd/relay/eve_*.go, internal/login/**, internal/ceremonylimit/**]
    tests: [cmd/relay/login_*_test.go, cmd/relay/ipc_login_test.go, cmd/relay/eve_*_test.go, cmd/relay/webauthn_browser_live_test.go, internal/login/*_test.go]
    journeys: [gate-eve-enrolment-open-neg, gate-eve-passkey-revoke-neg, gate-login-bootstrap-mint-neg, gate-login-passkey-revoke-neg, gate-login-bootstrap-mint-pos, gate-login-passkey-revoke-pos, gate-eve-enrolment-open-pos, gate-eve-passkey-revoke-pos]
  credentials:
    code: [cmd/relay/credential_*.go, cmd/relay/api_credential.go, cmd/relay/frontend_*.go, internal/control/**, internal/peertoken/**]
    tests: [cmd/relay/credential_*_test.go, cmd/relay/api_credential*_test.go, cmd/relay/frontend_*_test.go, cmd/relay/transport_enforcement_test.go]
    journeys: [gate-credential-mint-pos, execute-credential-renewal, gate-credential-mint-neg, gate-credential-revoke-neg, slow-route-keepalive, gate-credential-revoke-pos]
  remote:
    code: [cmd/relay/enrol*.go, cmd/relay/ipc_enrolments.go, cmd/relay/remote_*.go, internal/enrolment/**]
    tests: [cmd/relay/enrol*_test.go, cmd/relay/ipc_enrolment*_test.go, cmd/relay/remote_*_test.go, cmd/relay/audit_remote_test.go, cmd/relay/settings_enrolments*_test.go, internal/enrolment/*_test.go]
    journeys: [gate-enrolment-create-neg, gate-enrolment-sign-neg, gate-enrolment-update-neg, gate-enrolment-revoke-neg, gate-remote-configure-neg, gate-enrolment-create-pos, gate-enrolment-sign-pos, gate-enrolment-update-pos, gate-enrolment-revoke-pos, gate-remote-configure-pos]
  hosts:
    code: [cmd/relay/host_*.go, cmd/relay/ipc_host*.go, internal/sshhost/**]
    tests: [cmd/relay/host_*_test.go, cmd/relay/settings_hosts_ui_test.go, internal/sshhost/*_test.go]
    journeys: [permission-mode-restart, slow-route-keepalive, verify-fixtures-removed]
  presence:
    code: [cmd/relay/presence_gate.go, cmd/relay/admin_ops.go, cmd/relay/admin_read_ops.go, internal/presence/**]
    tests: [cmd/relay/presence_*_test.go, cmd/relay/gate_*_test.go, cmd/relay/config_queue_*_test.go, internal/presence/*_test.go]
    journeys: [gate-credential-mint-pos, execute-credential-renewal, gate-credential-mint-neg, gate-credential-revoke-neg, gate-mcp-register-neg, gate-service-register-neg, gate-eve-enrolment-open-neg, gate-eve-passkey-revoke-neg, gate-enrolment-create-neg, gate-enrolment-sign-neg, gate-enrolment-update-neg, gate-enrolment-revoke-neg, gate-login-bootstrap-mint-neg, gate-login-passkey-revoke-neg, gate-project-grant-neg, gate-project-rotate-token-neg, gate-remote-configure-neg, gate-mcp-oauth-start-neg, gate-sealed-reset-neg, gate-enrolment-create-pos, gate-enrolment-sign-pos, gate-enrolment-update-pos, gate-enrolment-revoke-pos, gate-login-bootstrap-mint-pos, gate-login-passkey-revoke-pos, gate-mcp-oauth-start-pos, gate-remote-configure-pos, gate-sealed-reset-pos, gate-mcp-register-pos, gate-project-grant-pos, gate-service-register-pos, gate-project-rotate-token-pos, gate-eve-enrolment-open-pos, gate-eve-passkey-revoke-pos, gate-credential-revoke-pos]
  sealed:
    code: [cmd/relay/sealed_reset.go, internal/sealed/**, internal/config/**]
    tests: [cmd/relay/sealed_*_test.go, cmd/relay/trayapp_sealed_test.go, cmd/relay/settings_store*_test.go, internal/sealed/*_test.go]
    journeys: [gate-sealed-reset-neg, gate-sealed-reset-pos]
  tray:
    code: [cmd/relay/trayapp.go, cmd/relay/tray_notify.go, cmd/relay/cocoa_darwin.go, cmd/relay/native_view.go, cmd/relay/icon.go, cmd/relay/platform.go]
    tests: [cmd/relay/trayapp_test.go, cmd/relay/tray_*_test.go, cmd/relay/service_menu_order_test.go]
    journeys: []
  settings-ui:
    code: [web/**, cmd/relay/settings_html.go, cmd/relay/ipc_handlers.go, cmd/relay/ipc_overview.go, cmd/relay/overview_seed.go, internal/webassets/**]
    tests: [cmd/relay/settings_*_test.go, cmd/relay/ipc_handlers_test.go, cmd/relay/ipc_contract_test.go]
    journeys: []
```

## Notes

- Every journey needs the running app and the devbox world. The api phase
  uses the `execute` credential (P4) and the bridge. The screen phase mints a
  run credential (`read`, `configure`, `grant`, `proxy`, 1 h) through the
  owner gate, holds it in memory only, uses it for everything but launches,
  and revokes it at the end.
- Screen journeys take the shared browser-test lock and need the console
  session.
- Fixtures the screen phase creates (the probe MCP, the crash service, the
  Verify Grant project, the Unreachable Host project and its `blackhole-`
  host) carry the run nonce and are removed by verify-fixtures-removed,
  including a crashed run's leftovers. So are the
  terminals any journey opened under a `grant-*` state folder or the World
  root, stopped or not.
- `screen` features have no door but the Settings window or tray. They are
  the last candidates for a journey; an API door is preferred when one exists.
- `permission-mode-restart` and `v1-conversion-refusal` are listed for
  completeness. They always read NOTRUN and cover nothing today.
