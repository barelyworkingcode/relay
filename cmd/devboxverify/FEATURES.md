# Relay feature map

What a person uses relay for, the features that serve each goal, and the
devbox journey that proves it works today. Journeys follow goals end to end.
A PR that changes a mapped feature updates this file.

Columns:

- **Door**: how a journey can drive it without the screen. `HTTP` is a route
  on the frontend socket (credential class in brackets), `CLI` a `relay` verb,
  `bridge` the bridge socket, `screen` an IPC-only Settings or tray action
  with no other door.
- **Gate**: `stays human` marks every op in `presence.GatedOps`, the passkey
  ceremony and grant approval. A journey may observe what a human approved;
  it never approves or bypasses the gate itself.
- **Journey**: an existing `devboxverify` journey, `preflight` (the read-only
  `relay grant --json` check the world runs before any journey), or `none`.

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
| Launch a session | API (eve) | eve starts a session → `POST /api/sessions` | HTTP [execute] | — | none (only the refusal below) |
| Blank-model launch refused | API | `POST /api/sessions` with no model | HTTP [execute] | — | blank-model-refused |
| Message, resume, stop a session | API (eve) | eve chat → `POST /api/sessions/{id}/message`, `/resume`, `DELETE /api/sessions/{id}` | HTTP [execute] | — | none |
| Terminals and their log | API (eve) | eve terminal → `POST /api/terminals`, `GET /api/terminals/{id}/log`, `DELETE` | HTTP [execute] | — | none |
| Persistent sessions | API | `GET`/`DELETE /api/projects/{id}/persistent-sessions` | HTTP | — | none |
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

Creating a project and widening a grant are `project.grant`, so they stay human. Journeys cover the effect of a grant a human approved (preflight, acme-sandbox-reach) and the ungated edits: narrowing, and re-saves that change nothing.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Create a project | Settings > Projects | Settings > Projects > Add project | HTTP `POST /api/projects` [configure] | stays human | none |
| Widen a grant (MCPs, tools, access, scope) | Settings > Projects | project > Edit > Save | HTTP `PUT /api/projects/{id}` [configure] | stays human | none |
| Narrow a grant or ungated edit | Settings > Projects | project > Edit > Save | HTTP `PUT /api/projects/{id}` [configure] | — | stale-derived-access-edit, context-number-resave |
| Local-to-remote conversion | Settings > Projects | project > Edit > kind | HTTP `PUT` | stays human | v1-conversion-refusal (always NOTRUN) |
| Remove a project | Settings > Projects | project > Remove | HTTP `DELETE /api/projects/{id}` | — | none |
| Show a project's grant | CLI | `relay grant --json` | CLI | — | preflight |
| Scope values (`contextSchema`) | Settings > Projects | project form > scope field | HTTP `/api/mcps/{id}/scope_fields` | stays human if widening | context-number-resave (partial) |
| Default project (home/work) | Settings > Projects | Default project picker | HTTP `PUT /api/default_project/{mode}` | — | none |
| Rotate a project token | Settings > Projects | project > Rotate token | HTTP `POST /api/projects/{id}/rotate_token` | stays human | none |
| Reveal a project token | Settings > Projects | project > eye icon | screen | — | none |
| Regenerate SKILL.md | Settings > Projects | project > Regen Skill | HTTP `POST /api/projects/{id}/regen_skill` | — | none |

### G3 · Add a tool and let a project use it — must-have
Intent: register an MCP server and have a project's agents call its tools.
It worked: the tools appear in the project's session, a call returns, and a disabled tool is refused.
Why must-have: every mail, calendar and file action an agent takes goes through the bridge.
Areas: mcps, grants, audit.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Register an MCP (stdio or HTTP) | Settings > MCP Servers, CLI | `relay mcp register`, or Add | CLI, HTTP `POST /api/mcps` | stays human | none |
| Authenticate an HTTP MCP (OAuth) | Settings > MCP Servers | row > Authenticate | screen | stays human | none |
| List MCPs and their tools | Settings, CLI | `relay mcp list`; `GET /api/mcps/{id}/tools` | CLI, HTTP | — | none |
| Tool listing and calls through the bridge | bridge | a session's `relay mcp --token`; `relay mcp call --token` | bridge, CLI | — | none |
| Disable tools per project | Settings > Projects | project form > tool picker | screen | — | none |
| Unregister an MCP | Settings, CLI | `relay mcp unregister`, or Remove | CLI, HTTP `DELETE /api/mcps/{id}` | — | none |
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
| Tool-call rows (intent then completion) | background | any bridge call | bridge | — | none |
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
| Register a service (command, capabilities) | Settings > Services, CLI | `relay service register`, or Add Service | CLI, HTTP `POST /api/services` | stays human | none |
| Edit a service | Settings > Services | row > Edit | HTTP `PUT /api/services/{id}` | stays human if fields change | none |
| Start, stop, restart | Settings, tray, CLI | tray service row; `relay service restart` | CLI, HTTP `POST /api/services/{id}/start`, `/stop` | — | none |
| Autostart at login | Settings > Services | row > autostart | HTTP `PUT /api/services/{id}/autostart` | — | none |
| Restart on crash, then `failed` after max attempts | background | a service exits unrequested | CLI `relay service list` (STATE) | — | none |
| Unregister | Settings, CLI | `relay service unregister` | CLI, HTTP `DELETE` | — | none |
| Menu visibility and order | Settings > Services | row > menu checkbox; drag | HTTP `PUT /api/services/{id}/menu`, `/position` | — | none |
| Service actions and config (manifest) | Settings > Service Inspector | service > action or Config | screen | — | none |
| Reveal a service's log | Settings > Service Inspector | Reveal log | screen | — | none |

### G6 · Let a session use a model — must-have
Intent: pick a model for a project or service and have its sessions reach it through relay.
It worked: the model list shows the configured models, and a chat gets an answer from the chosen one.
Why must-have: every chat turn crosses the model endpoint.
Areas: models, sessions.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Model list | Settings > Projects, API | model picker; `GET /api/models` | HTTP | — | none |
| Pick a model per project or service | Settings | project form > Model; service > allowed models | HTTP `PUT /api/projects/{id}` | — | none |
| Model endpoint (`/v1/chat/completions`, `/v1/models`, passthrough) | socket, optional TCP | a session or relayLLM calls `model.sock` | HTTP (model socket) | — | none |
| Model keys (`rmk_`) for sessions | background | minted at session launch | HTTP (model socket) | — | none |

### G7 · Sign in from a browser — should
Intent: reach relay's and eve's web pages from a browser with a passkey.
It worked: a registered passkey signs in; a revoked one no longer does.
Why should: used when away from the Mac; a break is loud, since the sign-in fails in front of the user.
Areas: login.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Mint a login code | tray, CLI | tray > Show Login Code…; `relay login enrol` | CLI | stays human | none |
| Register a passkey | login page | `/relay/login` > code > Register | browser | stays human (ceremony) | none |
| Sign in with a passkey | login page | `/relay/login` > passkey | browser | stays human (ceremony) | none |
| List passkeys, sign out a session | Settings > Passkeys, CLI | `relay login list` | CLI, screen | — | none |
| Revoke a passkey | Settings > Passkeys, CLI | `relay login revoke --id` | CLI | stays human | none |
| Open eve passkey enrolment | tray, CLI | tray > Allow Eve Passkey Enrolment…; `relay eve enrol` | CLI | stays human | none |
| List and revoke eve passkeys | Settings > Passkeys, CLI | `relay eve list`, `relay eve revoke` | CLI | revoke stays human | none |

### G8 · Operate relay from the tray and Settings — should
Intent: see at a glance that relay is healthy and get to what needs attention.
It worked: Settings opens, Overview's tiles and attention list match reality, and the tray shows each service's state.
Why should: used daily, but a break is visible at once.
Areas: tray, settings-ui.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Open Settings | tray | tray > Settings… (⌘,) | screen | — | none |
| Overview tiles and attention list | Settings > Overview | Overview | screen | — | none |
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
| Mint a credential | CLI | `relay credential mint --class …` | CLI | stays human | none |
| List credentials | CLI | `relay credential list` | CLI | — | none |
| Revoke a credential | CLI | `relay credential revoke --id` | CLI | stays human | none |
| Class enforcement on `/api/*` | API | any route with a bearer | HTTP | — | every journey (implicitly) |

### G10 · Give a remote machine access — later
Intent: let another machine reach chosen projects over mTLS.
It worked: an approved client reaches its projects and only those; a revoked one is refused.
Why later: set up rarely; almost every step is a human gate.
Areas: remote.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Configure the remote listener | Settings > Remote Clients | listener form | screen | stays human | none |
| Lodge, list, refuse an enrolment request | CLI, Settings | `relay enrol requests`, `relay enrol refuse` | CLI, HTTP `/api/enrolments` | — | none |
| Approve, sign, create, update, revoke | CLI, Settings | `relay enrol approve …` | CLI | stays human | none |
| CA fingerprint | CLI | `relay enrol ca-fingerprint` | CLI | — | none |
| Remote calls (fail-closed, audited) | mTLS listener | an enrolled client calls a tool | mTLS | — | none |

### G11 · Work on a remote directory over SSH — later
Intent: treat a folder on another machine as a project.
It worked: sessions and tools run on the host through one SSH connection.
Why later: the devbox world has no hosts, so no journey can run.
Areas: hosts.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Add, edit, remove a host | Settings > Hosts | Add host | HTTP `/api/hosts` | — | none |
| Probe, disconnect | Settings > Hosts | row > Probe / Disconnect | HTTP `/api/hosts/{id}/probe`, `/disconnect` | — | none |
| Host templates | Settings > Hosts | row > Templates | HTTP `/api/hosts/{id}/templates` | — | none |

### G12 · Recover from a broken sealed store — later
Intent: start over when the keychain key is lost.
It worked: relay names what it destroys, and starts clean.
Why later: break-glass only; it stays human by design.
Areas: sealed.

| Feature | Surface | Reach | Door | Gate | Journey |
|---|---|---|---|---|---|
| Reset Sealed Store… | tray | tray > Reset Sealed Store… | screen | stays human | none |

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
    journeys: [blank-model-refused, oversized-launch-audit-capped, permission-mode-restart]
  sandbox:
    code: [cmd/relay/sandbox_*.go, cmd/relay/session_sandbox*.go, internal/bridge/sandbox*.go, internal/sessions/sandbox/**]
    tests: [cmd/relay/sandbox_*_test.go, cmd/relay/session_sandbox*_test.go, internal/sessions/sandbox/**/*_test.go]
    journeys: [acme-sandbox-reach]
  templates:
    code: [cmd/relay/template_*.go, cmd/relay/ipc_templates.go]
    tests: [cmd/relay/template_*_test.go, cmd/relay/ipc_templates_test.go, cmd/relay/settings_templates_ui_test.go]
    journeys: []
  projects:
    code: [cmd/relay/project_*.go, cmd/relay/ipc_projects.go, cmd/relay/skills.go, internal/project/**]
    tests: [cmd/relay/project_*_test.go, cmd/relay/ipc_project*_test.go, cmd/relay/settings_project*_test.go, cmd/relay/skills_test.go, internal/project/*_test.go]
    journeys: [stale-derived-access-edit, context-number-resave, v1-conversion-refusal]
  grants:
    code: [cmd/relay/grant_cmd.go, cmd/relay/router.go, internal/project/apply.go, internal/project/grant_widening.go, internal/membership/**]
    tests: [cmd/relay/grant_*_test.go, cmd/relay/router_*_test.go, cmd/relay/scope_*_test.go, cmd/relay/settings_scope*_test.go, internal/membership/*_test.go]
    journeys: [acme-sandbox-reach, stale-derived-access-edit, context-number-resave]
  mcps:
    code: [cmd/relay/mcp_*.go, cmd/relay/ipc_mcp*.go, cmd/relay/exec_cmd.go, internal/mcp/**, internal/mcpbroker/**, internal/bridge/**, internal/jsonrpc/**]
    tests: [cmd/relay/mcp_*_test.go, cmd/relay/exec_cmd_test.go, cmd/relay/arg*_test.go, cmd/relay/router_tool*_test.go, internal/mcp/*_test.go, internal/mcpbroker/*_test.go, internal/bridge/*_test.go]
    journeys: []
  audit:
    code: [cmd/relay/audit_*.go, cmd/relay/ipc_audit.go, internal/audit/**]
    tests: [cmd/relay/audit_*_test.go, cmd/relay/settings_audit_ui_test.go, internal/audit/*_test.go]
    journeys: [blank-model-refused, oversized-launch-audit-capped, context-number-resave]
  services:
    code: [cmd/relay/service_*.go, cmd/relay/cli_service.go, cmd/relay/enhanced_services.go, cmd/relay/ipc_service*.go, internal/service/**]
    tests: [cmd/relay/service_*_test.go, cmd/relay/cli_service*_test.go, cmd/relay/enhanced_services*_test.go, cmd/relay/ipc_service*_test.go, cmd/relay/settings_service*_test.go, cmd/relay/launch_*_test.go, internal/service/*_test.go]
    journeys: []
  models:
    code: [cmd/relay/model_*.go, cmd/relay/router_model_host.go, cmd/relay/frontend_model_guard.go, cmd/relay/relay_llm_channel.go, cmd/relay/ipc_models.go, internal/modelbroker/**]
    tests: [cmd/relay/model_*_test.go, cmd/relay/router_model_host_test.go, cmd/relay/frontend_model_guard_test.go, cmd/relay/relay_llm_channel_test.go, cmd/relay/ipc_models_test.go, cmd/relay/settings_model_picker*_test.go, cmd/relay/integration_*relayllm_test.go, internal/modelbroker/*_test.go]
    journeys: []
  login:
    code: [cmd/relay/login_*.go, cmd/relay/ipc_login.go, cmd/relay/eve_*.go, internal/login/**, internal/ceremonylimit/**]
    tests: [cmd/relay/login_*_test.go, cmd/relay/ipc_login_test.go, cmd/relay/eve_*_test.go, cmd/relay/webauthn_browser_live_test.go, internal/login/*_test.go]
    journeys: []
  credentials:
    code: [cmd/relay/credential_*.go, cmd/relay/api_credential.go, cmd/relay/frontend_*.go, internal/control/**, internal/peertoken/**]
    tests: [cmd/relay/credential_*_test.go, cmd/relay/api_credential*_test.go, cmd/relay/frontend_*_test.go, cmd/relay/transport_enforcement_test.go]
    journeys: []
  remote:
    code: [cmd/relay/enrol*.go, cmd/relay/ipc_enrolments.go, cmd/relay/remote_*.go, internal/enrolment/**]
    tests: [cmd/relay/enrol*_test.go, cmd/relay/ipc_enrolment*_test.go, cmd/relay/remote_*_test.go, cmd/relay/audit_remote_test.go, cmd/relay/settings_enrolments*_test.go, internal/enrolment/*_test.go]
    journeys: []
  hosts:
    code: [cmd/relay/host_*.go, cmd/relay/ipc_host*.go, internal/sshhost/**]
    tests: [cmd/relay/host_*_test.go, cmd/relay/settings_hosts_ui_test.go, internal/sshhost/*_test.go]
    journeys: [permission-mode-restart]
  presence:
    code: [cmd/relay/presence_gate.go, internal/presence/**]
    tests: [cmd/relay/presence_*_test.go, cmd/relay/gate_*_test.go, cmd/relay/config_queue_*_test.go, internal/presence/*_test.go]
    journeys: []
  sealed:
    code: [cmd/relay/sealed_reset.go, internal/sealed/**, internal/config/**]
    tests: [cmd/relay/sealed_*_test.go, cmd/relay/trayapp_sealed_test.go, cmd/relay/settings_store*_test.go, internal/sealed/*_test.go]
    journeys: []
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

- Every journey needs the running app and the devbox world; the `execute`
  credential (P4) covers G1, and the `configure` credential (P7) is needed
  for any G2 edit. Without P7 those journeys read NOTRUN.
- Browser-driven journeys (G7, any Settings screen journey) take the shared
  browser-test lock.
- `screen` features have no door but the Settings window or tray. They are
  the last candidates for a journey; an API door is preferred when one exists.
- `permission-mode-restart` and `v1-conversion-refusal` are listed for
  completeness. They always read NOTRUN and cover nothing today.
