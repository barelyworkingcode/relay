# Events

An event is one log line, written at the end of one operation, that says what
ran, how it ended and under which trace. A test or an operator reads events
with `relay logs` to see what relay did, without scraping prose.

## 1. What an event is

- **One line per operation.** An operation is a mutation, an action or a read
  that a door starts: a CLI verb, an HTTP route, a bridge request, a Settings
  action. Its core writes the line when the operation ends, whether it
  succeeded, failed or was refused.
- **It is not the audit log.** The audit log (`docs/audit-log.md`) stays the
  security record: it is durable, it names the actor and authority, and a
  failed write refuses issuance. An event is a diagnostic line in
  `logs/relay.log`. It is bounded by log rotation and carries no authority, no
  arguments and no content. Nothing may rely on an event for a security
  decision.
- **Same file, same shape.** Event lines are ordinary lines of the logging
  standard (`docs/logging-standard.md`) with two added keys. Sibling services
  that follow the standard read them without change.

## 2. The line shape

An event line keeps the nine keys of the standard and adds `event` and
`reason`. Nothing is renamed: the "time, trace, outcome" of an operation are
the existing `ts`, `trace_id` and `status`.

| Key | On an event line |
|---|---|
| `ts`, `level`, `service`, `duration_ms`, `trace_id` | As in the standard. `duration_ms` runs from `BeginEvent` to `End`. |
| `msg` | The event key. |
| `op` | The event key. |
| `status` | The outcome: `ok`, `error` or `denied`. |
| `error` | The error's own text. `""` when ok. Free text and not stable: never assert on it. |
| `event` | The event key. Only an event line has a top-level `event`. |
| `reason` | A stable snake_case code. Present when `status` is not `ok`; absent when it is. |
| per-event fields | Ids, names, counts and booleans only. Never a path, argument, body, prompt, token or display name. Strings are cut to 500 runes. |

```json
{"ts":"2026-10-09T12:00:00.123Z","level":"warn","msg":"service.register","service":"relay","op":"service.register","status":"denied","duration_ms":3,"error":"no session can display a presence prompt","trace_id":"rlcheck-deny-01","event":"service.register","reason":"presence_no_session","service_id":"acme","action":"create"}
```

Two invariants hold for every event:

- **Exactly one line per operation.** Each operation a door starts writes
  exactly one event line, however many doors reach the same core.
- **Written before the answer.** An event from the relay process is written
  synchronously, before the caller gets its result: before the CLI exits,
  before the HTTP response and before the bridge reply. A test that has the
  answer can read the line. Events from relay-sessions are the exception; see
  section 8.

The trace comes from the caller. A CLI call names it with the global
`--trace ID` (`docs/cli.md`). An HTTP call names it with the `X-Trace-Id`
header. An ID outside `[A-Za-z0-9_-]{8,64}` is replaced by a fresh one. Each
Settings action gets its own trace, which a caller cannot choose. The remote
listener mints its own.

## 3. The naming rule

- **Pattern.** `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`: `<domain>.<action>` or
  `<domain>.<object>.<action>`.
- **Domain.** The core's noun: `server`, `project`, `grant`, `chief_of_staff`,
  `mcp`, `tool`, `bridge`, `credential`, `doors`, `enrolment`, `remote`, `login`, `eve`,
  `sealed`, `service`, `host`, `host_template`, `template`, `file`, `audit`,
  `session`, `terminal`, `sandbox`, `model`, `chat`, `status`, `files`.
- **Reused names.** Where a name in `presence.GatedOps`, an `adminOps` entry or
  an existing boundary `op` already names the operation, the key is that exact
  string (`credential.mint`, `project.rotate_token`, `remote.configure`,
  `session.drop_in`, `model.request`).
- **Standard actions.** Reads use `.list` and `.get`. Background state changes
  use `.state`.
- **Literal keys.** Each key is a string literal at its `BeginEvent` call,
  never computed, so a search for the key finds the code.
- **Stability.** A key never changes meaning.

## 4. Outcomes and reason codes

`status` is the outcome. `reason` says why when it is not `ok`.

| Level | When |
|---|---|
| `info` | `ok`. |
| `warn` | `denied`; and `error` with reason `invalid`, `not_found`, `conflict` or `cancelled`. |
| `error` | Every other `error`. |

**Denied reasons** (the operation was refused by a decision):

| Reason | Meaning |
|---|---|
| `presence_refused` | The owner refused the presence prompt. |
| `presence_no_session` | No session can show a presence prompt. |
| `presence_unavailable` | Presence checking is unavailable or not wired for the operation. |
| `presence_invalid` | The presence grant is invalid. |
| `presence_timeout` | The presence prompt went unanswered until the requester left. |
| `unauthorized` | No or wrong credential. |
| `not_granted` | The credential's class or scope does not allow it. |
| `read_only` | The target is read-only. |
| `symlink` | A path crossed a symlink the containment rule refuses. |
| `throttled` | A rate limit applied. |
| `audit_unavailable` | The audit log could not record the operation, so it was refused. |

**Error reasons** (the operation failed): `invalid`, `not_found`, `conflict`,
`unavailable`, `upstream`, `timeout`, `cancelled`, `internal`.

**Event-specific codes.** `session.launch` and `session.drop_in` use the
refusal code of the launch or drop-in as `reason`: `denied` below status 500,
`error` at or above.

## 5. What has no event

- A refusal that happens at a door before any core runs: a missing credential,
  a class check, a body that does not decode, an unknown route. The
  `frontend.request` and `bridge.request` lines and the audit log cover them.
- `relay audit`, `relay enrol ca-fingerprint` and `relay logs`. They read local
  files, and a CLI process's own log lines also go to the user's terminal.
- Settings-only actions with no core (`reveal_*`, service inspector actions)
  and screen-only reads.
- Routes forwarded to services outside relay.
- A poll that succeeds. A poll read writes a line only when it fails.

## 6. Reading events with `relay logs`

```
relay [--config-dir DIR] [--trace ID] logs [--json] [--event KEY] [--since TIME] [--follow [--timeout DUR]]
```

It reads, under the config dir, `logs/relay.log.1`, `logs/relay.log`,
`logs/relaysessions.log.1` and `logs/relaysessions.log`. Missing files are
skipped. Lines are merged and sorted by `ts`. It needs no running server and
creates nothing. `--trace`, `--event` and `--since` combine with AND.
`--json` prints each matching line exactly as stored. Exit codes: `0` a line
was printed (with `--follow --event`, the first match), `1` nothing matched or
the timeout or a signal came first, `2` usage error or no relay log. The full
reference is in `docs/cli.md`.

```
relay --config-dir "$X" logs --event service.restart --trace rlcheck-ok-0001 --json
```

## 7. The catalogue

The emitter is the function that calls `BeginEvent`. A read writes its event in
the door's handler, before the response, because the accessors behind it also
run inside other operations and would write twice. A mutation or action writes
it in the core method every door calls, so a door never writes one. The Fields
column lists the keys an `ok` line carries; on a failed line they are present
when known. `docs/logging-schema.json` enforces them.

Five events replace a line that exists today and keep every key and value of
it; only `msg` changes, to the event key: `session.drop_in`,
`chief_of_staff.send`, `model.request`, `chat.turn` and `session.state`. Their
keys are listed in the rows below.

### Server

| Event | When written | Doors | Fields |
|---|---|---|---|
| `server.ready` | Once, after every listener is bound | `relay serve`, tray start (background, trace `""`) | `config_dir`, `ready_file`, `pid` |

### Projects

| Event | When written | Doors | Fields |
|---|---|---|---|
| `project.list` | Before the list is returned | `GET /api/projects` | `count` |
| `project.get` | Before the project is returned | `GET /api/projects/{id}` | `project_id` |
| `project.create` | At the end of `ProjectOps.Create` | `POST /api/projects`, `relay project create`, Settings | `project_id`, `kind` |
| `project.update` | At the end of `ProjectOps.Update` | `PUT /api/projects/{id}`, `relay project update`, `relay project edit`, Settings | `project_id`, `gated` |
| `project.remove` | At the end of `ProjectOps.Remove` | `DELETE /api/projects/{id}`, `relay project remove`, Settings | `project_id` |
| `project.default.set` | At the end of `SetDefaultProject` | `PUT /api/default_project/{mode}`, Settings | `mode`, `project_id` |
| `project.rotate_token` | At the end of `RotateToken` | `POST /api/projects/{id}/rotate_token`, `relay project rotate-token`, Settings | `project_id` |
| `project.reveal_token` | At the end of `RevealToken` | `relay project token` | `project_id` |
| `project.regen_skill` | At the end of `RegenSkill` | `POST /api/projects/{id}/regen_skill`, `relay project regen-skill`, Settings | `project_id` |
| `project.disabled_tools.set` | At the end of `SetDisabledTools` | Settings | `project_id`, `mcp_id`, `count` |
| `project.describe` | At the end of `appRouter.DescribeProject` | bridge `describe_project` | `project_id` |

### Chief of Staff

| Event | When written | Doors | Fields |
|---|---|---|---|
| `chief_of_staff.config.get` | Before the config is returned | `GET /api/chief-of-staff/config` | none |
| `chief_of_staff.config.set` | At the end of `SetChiefOfStaff` | `PUT /api/chief-of-staff/config`, Settings | `project_id` |
| `chief_of_staff.config.clear` | At the end of `ClearChiefOfStaff` | `DELETE /api/chief-of-staff/config`, Settings | none |
| `chief_of_staff.send` | When a message is sent to the Chief of Staff session | `POST /api/chief-of-staff/messages` | `session_id` (`origin` (existing)) |
| `chief_of_staff.start` | When a Chief of Staff session is started | `POST /api/chief-of-staff/sessions` | `session_id`, `project_id`, `kind` (the outcome follows the HTTP status: `403` is `denied`, `400` is `error` with reason `invalid`) |

### Grants

| Event | When written | Doors | Fields |
|---|---|---|---|
| `grant.view` | Before the grant view is printed | `relay grant` | `count` |
| `grant.describe` | At the end of `handleRemoteDescribeGrant` | remote `describe_grant` | `project_id`, `client_id` |
| `grant.narrow` | At the end of `ProjectOps.NarrowForEnrolment` | remote `narrow_grant` | `project_id`, `client_id`, `changed` |

### MCPs

| Event | When written | Doors | Fields |
|---|---|---|---|
| `mcp.list` | Before the list is returned | `GET /api/mcps`, `relay mcp list` | `count` |
| `mcp.tools.list` | Before the tool list is returned | `GET /api/mcps/{id}/tools` | `mcp_id`, `count` |
| `mcp.scope_fields.get` | Before the fields are returned | `GET /api/mcps/{id}/scope_fields`, `relay mcp scope-fields` | `mcp_id` |
| `mcp.scope_field.enumerate` | Before the values are returned | `POST /api/mcps/{id}/enumerate` | `mcp_id`, `field` |
| `mcp.register` | At the end of `McpOps.Add` | `POST /api/mcps`, `relay mcp register`, Settings | `mcp_id`, `transport` |
| `mcp.unregister` | At the end of `McpOps.Remove` | `DELETE /api/mcps/{id}`, `relay mcp unregister`, Settings | `mcp_id` |
| `mcp.oauth.start` | At the end of `McpOps.StartOAuth` | `relay mcp authenticate`, Settings | `mcp_id` |
| `mcp.permissions.reset` | At the end of `McpOps.ResetPermissions` | `relay mcp reset-permissions`, Settings | `mcp_id` |
| `mcp.reconcile` | When the external MCP set is reconciled | bridge `reconcile_external_mcps` | none |
| `mcp.reload` | When one external MCP is reloaded | bridge `reload_external_mcp` | `mcp_id` |
| `mcp.state` | When an MCP is published and on each health report (background) | none (background) | `mcp_id`, `state` (`state` is `up`, `down`, `restart_failed`, `restarted` or `abandoned`) |

### Tools and bridge

| Event | When written | Doors | Fields |
|---|---|---|---|
| `tool.list` | At the end of `appRouter.ListTools` | bridge `list_tools` (`relay mcp`, `relay mcpExec --list`, sessions), remote | `project_id`, `transport`, `count` |
| `tool.call` | At the end of `appRouter.CallTool` | bridge `call_tool` (`relay mcp call`, `relay mcpExec`, sessions), remote | `project_id`, `mcp_id`, `tool`, `transport`, `tool_error` |
| `bridge.hello` | At the end of `appRouter.Hello` | bridge `hello` | `service_id`, `kind` |
| `service.manifest.register` | At the end of `appRouter.RegisterManifest` | bridge `register_manifest` | `service_id` |

### Credentials

| Event | When written | Doors | Fields |
|---|---|---|---|
| `credential.list` | Before the list is printed | `relay credential list` | `count` |
| `credential.mint` | At the end of `CredentialOps.Mint` | `relay credential mint` | `credential_id`, `classes` |
| `credential.revoke` | At the end of `CredentialOps.Revoke` | `relay credential revoke` | `credential_id` |

### Status

| Event | When written | Doors | Fields |
|---|---|---|---|
| `status.view` | Before the status document is returned | `relay status` | none |

### Doors

| Event | When written | Doors | Fields |
|---|---|---|---|
| `doors.list` | Before the doors document is returned | `relay doors` | `count` |

### Enrolment

| Event | When written | Doors | Fields |
|---|---|---|---|
| `enrolment.list` | Before the list is returned | `GET /api/enrolments`, `relay enrol list` | `count` |
| `enrolment.get` | Before the enrolment is returned | `GET /api/enrolments/{id}` | `client_id` |
| `enrolment.create` | At the end of the matching `EnrolmentOps` method | `POST` and `DELETE /api/enrolments`, `relay enrol create/sign/update/revoke`, Settings | `client_id` |
| `enrolment.sign` | At the end of the matching `EnrolmentOps` method | `POST` and `DELETE /api/enrolments`, `relay enrol create/sign/update/revoke`, Settings | `client_id` |
| `enrolment.update` | At the end of the matching `EnrolmentOps` method | `POST` and `DELETE /api/enrolments`, `relay enrol create/sign/update/revoke`, Settings | `client_id` |
| `enrolment.revoke` | At the end of the matching `EnrolmentOps` method | `POST` and `DELETE /api/enrolments`, `relay enrol create/sign/update/revoke`, Settings | `client_id` |
| `enrolment.request.list` | Before the list is printed | `relay enrol requests` | `count` |
| `enrolment.request.approve` | At the end of `EnrolmentOps.Approve` | `relay enrol approve`, Settings | `request_id`, `client_id` |
| `enrolment.request.refuse` | At the end of `EnrolmentOps.Refuse` | `relay enrol refuse`, Settings | `request_id` |
| `enrolment.request.lodge` | At the end of `handleEnrolmentLodge` | enrolment listener | `request_id` |
| `enrolment.request.poll` | On failure only, in `handleEnrolmentPoll` (quiet) | enrolment listener | `request_id` |

### Remote

| Event | When written | Doors | Fields |
|---|---|---|---|
| `remote.config.get` | Before the config is returned | `GET /api/remote`, `relay remote show` | none |
| `remote.configure` | At the end of `SetRemoteConfig` | `PUT /api/remote`, `relay remote set`, Settings | `enabled` |

### Login

| Event | When written | Doors | Fields |
|---|---|---|---|
| `login.list` | Before the list is printed | `relay login list` | `count` |
| `login.bootstrap.mint` | At the end of `LoginOps.MintBootstrap` | `relay login enrol`, tray | none |
| `login.passkey.revoke` | At the end of `RevokePasskey` | `relay login revoke`, Settings | `passkey_id` |
| `login.session.list` | Before the list is printed | `relay login sessions` | `count` |
| `login.session.sign_out` | At the end of `SignOut` | `relay login sign-out`, Settings | `credential_id` |
| `login.page` | In `serveDocument` | `GET /relay/login` | none |
| `login.challenge` | In `serveChallenge` | `POST /relay/login/challenge` | none |
| `login.passkey.register` | In `serveVerify`, when the body registers a passkey | `POST /relay/login/verify` | `passkey_id` (exactly one of this and `login.sign_in` per request) |
| `login.sign_in` | In `serveVerify`, when the body signs in | `POST /relay/login/verify` | `passkey_id`, `credential_id` (a body that is neither writes this key with reason `invalid`) |

### Eve

| Event | When written | Doors | Fields |
|---|---|---|---|
| `eve.list` | Before the list is printed | `relay eve list` | `count` |
| `eve.enrolment.open` | At the end of `EveEnrolmentOps.Open` | `relay eve enrol`, tray | none |
| `eve.enrolment.status` | On failure only (quiet) | `GET /api/eve/passkey-enrolment` | none |
| `eve.enrolment.consume` | At the end of `Consume` | `POST /api/eve/passkey-enrolment/consume` | none |
| `eve.passkey.report` | At the end of `EvePasskeyOps.Report` | `PUT /api/eve/passkeys` | `count` |
| `eve.passkey.revocations` | On failure only (quiet) | `GET /api/eve/passkeys/revocations` | `count` |
| `eve.passkey.revoke` | At the end of `Revoke` | `relay eve revoke`, Settings | `passkey_id` |

### Sealed store

| Event | When written | Doors | Fields |
|---|---|---|---|
| `sealed.reset` | At the end of `resetSealedStore` | `relay sealed reset`, tray | none |

### Services

| Event | When written | Doors | Fields |
|---|---|---|---|
| `service.list` | Before the list is returned | `GET /api/services`, `relay service list` | `count` |
| `service.get` | Before the service is returned | `GET /api/services/{id}` | `service_id` |
| `service.register` | At the end of `ServiceOps.Register`; `relay service register` writes only this event | `relay service register` | `service_id`, `action` (`action` is `create` or `update`) |
| `service.create` | At the end of `ServiceOps.Create` | `POST /api/services`, Settings add | `service_id` |
| `service.update` | At the end of `ServiceOps.Update` | `PUT /api/services/{id}` | `service_id` |
| `service.unregister` | At the end of `ServiceOps.Remove` | `DELETE /api/services/{id}`, `relay service unregister`, Settings | `service_id` |
| `service.start` | At the end of `ServiceOps.Start` / `Stop` | `POST /api/services/{id}/start` and `/stop`, `relay service start` and `stop`, Settings, tray menu | `service_id` |
| `service.stop` | At the end of `ServiceOps.Start` / `Stop` | `POST /api/services/{id}/start` and `/stop`, `relay service start` and `stop`, Settings, tray menu | `service_id` |
| `service.restart` | At the end of `ServiceOps.Restart` | `relay service restart`, bridge `reload_service` | `service_id` |
| `service.autostart.set` | At the end of `SetAutostart` | `PUT /api/services/{id}/autostart`, Settings | `service_id`, `autostart` |
| `service.move` | At the end of `ServiceOps.Move` | `PUT /api/services/{id}/position`, Settings | `service_id`, `index` |
| `service.menu.set` | At the end of `SetMenuHidden` | `PUT /api/services/{id}/menu`, Settings | `service_id`, `hidden` |
| `service.config.save` | At the end of `SaveConfigFile` | `relay service config --set`, Settings | `service_id`, `restarted` |
| `service.config.get` | At the end of `readServiceConfig` | `relay service config`, Settings | `service_id` |
| `service.action` | At the end of `runServiceAction` | `relay service action`, Settings | `service_id`, `action_id` |
| `service.state` | On spawn and on every phase change (background) | none (background) | `service_id`, `phase`, `attempt` (`phase` is `running`, `restarting` or `failed`; `exit_code` is optional) |

### Hosts

| Event | When written | Doors | Fields |
|---|---|---|---|
| `host.list` | Before the list is returned | `GET /api/hosts` | `count` |
| `host.get` | Before the host is returned | `GET /api/hosts/{id}` | `host_id` |
| `host.create` | At the end of the matching `HostOps` method | `/api/hosts` routes, Settings | `host_id` |
| `host.update` | At the end of the matching `HostOps` method | `/api/hosts` routes, Settings | `host_id` |
| `host.remove` | At the end of the matching `HostOps` method | `/api/hosts` routes, Settings | `host_id` |
| `host.probe` | At the end of the matching `HostOps` method | `/api/hosts` routes, `relay host probe`, Settings | `host_id` |
| `host.disconnect` | At the end of the matching `HostOps` method | `/api/hosts` routes, `relay host disconnect`, Settings | `host_id` |
| `host.pastetmp` | At the end of `FileOps.PasteTmp` | `POST /api/hosts/{id}/pastetmp` | `host_id` |

### Host templates

| Event | When written | Doors | Fields |
|---|---|---|---|
| `host_template.list` | Before the list is returned | `GET /api/hosts/{id}/templates` | `host_id`, `count` |
| `host_template.create` | At the end of the matching `HostTemplateOps` method | host template routes, Settings | `host_id`, `template_id` |
| `host_template.update` | At the end of the matching `HostTemplateOps` method | host template routes, Settings | `host_id`, `template_id` |
| `host_template.remove` | At the end of the matching `HostTemplateOps` method | host template routes, Settings | `host_id`, `template_id` |

### Templates

| Event | When written | Doors | Fields |
|---|---|---|---|
| `template.list` | Before the list is returned | `GET /api/terminal/templates` | `count` |
| `template.get` | Before the template is returned | `GET /api/terminal/templates/{id}` | `template_id` |
| `template.create` | At the end of the matching `TemplateOps` method | template routes, Settings | `template_id` |
| `template.update` | At the end of the matching `TemplateOps` method | template routes, Settings | `template_id` |
| `template.remove` | At the end of the matching `TemplateOps` method | template routes, Settings | `template_id` |

### Files

| Event | When written | Doors | Fields |
|---|---|---|---|
| `file.list` | At the end of the matching `fileSession` method; when `FileOps.open` refuses, the route's `session` helper writes the same key | `POST /api/projects/{id}/files/*` | `project_id` |
| `file.stat` | At the end of the matching `fileSession` method; when `FileOps.open` refuses, the route's `session` helper writes the same key | `POST /api/projects/{id}/files/*` | `project_id` |
| `file.read` | At the end of the matching `fileSession` method; when `FileOps.open` refuses, the route's `session` helper writes the same key | `POST /api/projects/{id}/files/*` | `project_id` |
| `file.write` | At the end of the matching `fileSession` method; when `FileOps.open` refuses, the route's `session` helper writes the same key | `POST /api/projects/{id}/files/*` | `project_id` |
| `file.mkdir` | At the end of the matching `fileSession` method; when `FileOps.open` refuses, the route's `session` helper writes the same key | `POST /api/projects/{id}/files/*` | `project_id` |
| `file.rename` | At the end of the matching `fileSession` method; when `FileOps.open` refuses, the route's `session` helper writes the same key | `POST /api/projects/{id}/files/*` | `project_id` |
| `file.move` | At the end of the matching `fileSession` method; when `FileOps.open` refuses, the route's `session` helper writes the same key | `POST /api/projects/{id}/files/*` | `project_id` |
| `file.delete` | At the end of the matching `fileSession` method; when `FileOps.open` refuses, the route's `session` helper writes the same key | `POST /api/projects/{id}/files/*` | `project_id` |
| `file.search` | At the end of the matching `fileSession` method; when `FileOps.open` refuses, the route's `session` helper writes the same key | `POST /api/projects/{id}/files/*` | `project_id` |
| `file.git` | At the end of the matching `fileSession` method; when `FileOps.open` refuses, the route's `session` helper writes the same key | `POST /api/projects/{id}/files/*` | `project_id` |
| `file.stream` | Once the body is fully written or fails | `GET /api/projects/{id}/files/stream` | `project_id` |
| `file.ws.close` | At connection end in `serveFilesWS` | `GET /ws/files` | none |
| `files.watch` | At the end of `adminFilesWatch`, when the peer leaves | `relay files watch` | `project_id` |

### Audit

| Event | When written | Doors | Fields |
|---|---|---|---|
| `audit.query` | Before the records are returned | `GET /api/audit` | `count` |
| `audit.path.get` | Before the path is returned | `GET /api/audit/log` | none |
| `audit.export` | At the end of `audit.AuditOps.Export` | `POST /api/audit/export`, Settings | `count` |

### Sessions and terminals

| Event | When written | Doors | Fields |
|---|---|---|---|
| `session.list` | In `handleProxyList`, `listViaHost` | `GET /api/sessions`, `relay session list` | `count` |
| `terminal.list` | In `handleProxyList`, `listViaHost` | `GET /api/terminals`, `relay terminal list` | `count` |
| `session.launch` | In `launchWithEvent` | `POST /api/sessions`, `POST /api/terminals`, `relay session start`, `relay terminal start` | `session_id`, `project_id`, `kind` (on a refusal `reason` is the launch refusal code) |
| `session.resume` | In `resumeSession` | `POST /api/sessions/{id}/resume`, `relay session resume` | `session_id`, `project_id` |
| `session.drop_in` | In `dropIn` | `POST /api/sessions/{id}/drop-in`, bridge `drop_in_attach` (`relay drop-in`) | `session_id`, `host`, `terminal_id` |
| `session.mode` | At the end of `adminSessionMode` | `relay session mode` | `session_id` (a `resume_required` answer from a local claude session ends the event `error` with reason `internal`) |
| `session.persistent.list` | Before the list is returned | `GET /api/projects/{id}/persistent-sessions`, `relay terminal persistent-list` | `project_id` |
| `session.persistent.kill` | At the end of `PersistentSessionOps.Kill` | `DELETE /api/projects/{id}/persistent-sessions/{name}`, `relay terminal persistent-kill` | `project_id` |
| `session.exited` | At the end of `appRouter.SessionExited` | bridge `session_exited` | `session_id` |
| `session.delete` | In relay-sessions `HandleDeleteSession` / `HandleSessionMessageSync` | forwarded `DELETE /api/sessions/{id}`, `POST /api/sessions/{id}/message`, `relay session stop`, `relay session message` | `session_id` |
| `session.message` | In relay-sessions `HandleDeleteSession` / `HandleSessionMessageSync` | forwarded `DELETE /api/sessions/{id}`, `POST /api/sessions/{id}/message`, `relay session stop`, `relay session message` | `session_id` |
| `terminal.delete` | In relay-sessions `HandleDeleteTerminal` / `HandleTerminalLog` | forwarded terminal routes, `relay terminal stop`, `relay terminal log` | `terminal_id` |
| `terminal.log` | In relay-sessions `HandleDeleteTerminal` / `HandleTerminalLog` | forwarded terminal routes, `relay terminal stop`, `relay terminal log` | `terminal_id` |
| `session.ws.close` | At connection end in the relay-sessions hub | `/ws` | none |
| `session.state` | On an agent state change (background, relay-sessions) | none (background) | `session_id`, `from`, `to` (the agent states before and after) |

### Sandbox

| Event | When written | Doors | Fields |
|---|---|---|---|
| `sandbox.attach` | At the end of `appRouter.SandboxAttach` | bridge `sandbox_attach` (`relay sandbox`) | `project_id`, `template`, `session_id` (a folder in no project ends `error` with reason `not_found`; a template outside `allowed_templates` ends `denied`) |

### Models

| Event | When written | Doors | Fields |
|---|---|---|---|
| `model.request` | At the end of a model request; poll paths stay silent on success | model endpoint (unix, TCP) | `method`, `path`, `http_status`, `transport`, `caller_kind`, `caller`, `session_id` (only for a session admitted by launch identity or as a member of one), `model` (absent when unknown) |
| `model.host.register` | At the end of `appRouter.RegisterModelHost` | bridge `register_model_host` | `service_id` |
| `model.list` | In relay-sessions `HandleModels` | forwarded `GET /api/models`, `relay model list` | `count` |

### Chat

| Event | When written | Doors | Fields |
|---|---|---|---|
| `chat.turn` | At the end of a chat turn (relay-sessions) | `/ws` `send_message` only; `POST /api/sessions/{id}/message` and `relay session message` write none | `session_id` (tool search is not a field here: it logs its own `chat.tool_search` line at session start) |

### Test build only

Written only by a `relaytest` build, on a config dir the test seams act on
([`docs/testing.md`](testing.md#the-test-build)). A release build writes none
of them. A wait for one uses `relay logs --follow --event <key>`.

| Event | When written | Doors | Fields |
|---|---|---|---|
| `debug.presence.answer` | In the test approver's `EvaluateOp`, before it returns or, for `timeout`, before it blocks; `error` / `invalid` for an invalid outcome file, only for a caller that has a console session (any other caller is refused first as `presence_no_session`, with no event) | any presence-gated operation (background, trace of the caller) | `gated_op`, `answer` (`approve`, `deny` or `timeout`), `source` (`file` or `default`) |
| `debug.keychain.fault` | In the file keyring, on each operation a fault changes, before the operation acts | any sealed-store operation (background, trace `""`) | `keychain_op` (`load`, `create` or `destroy`), `fault` |
| `debug.clock.get` | In the `debug.clock` admin op for `get` (quiet on success) | `relay debug clock`, and every CLI view that reads the clock | `now`, `offset_ms` |
| `debug.clock.set` | In the `debug.clock` admin op for `set`; `error` / `invalid` or `unavailable` | `relay debug clock set` | `now`, `offset_ms` |
| `debug.ssh.stub` | At `relay serve` start, when `X/test-ssh.json` names a valid stub; an invalid file stops serve instead | `relay serve` (background, trace `""`) | `command` |
| `debug.clock.advance` | In the `debug.clock` admin op for `advance`; `error` / `invalid` or `unavailable` | `relay debug clock advance` | `now`, `offset_ms` |

## 8. Background events and how to wait on them

`server.ready`, `service.state`, `mcp.state`, `session.state`, `chat.turn` and
`session.exited` are written when something happens, not when a caller asks.
`server.ready` and `session.exited` carry no caller's trace.

- **Events from the relay process** are written before the answer, so there is
  nothing to wait for after the CLI exits or the response arrives.
- **Events from relay-sessions** (`session.delete`, `session.message`,
  `terminal.delete`, `terminal.log`, `model.list`, `session.ws.close`,
  `chat.turn`, `session.state`) reach `relaysessions.log` through relay's
  capture of the helper's stderr, after the response. So do the `service.state`
  lines of a supervised service. Wait for them with a deadline, not a sleep:

```
relay --config-dir "$X" --trace "$T" logs --follow --event service.state --since 1m --timeout 30s --json
```

`--follow --event` prints the first match and exits `0`. A match already in the
files counts, so the wait cannot lose a race with the operation. Exit `1`
means the deadline passed. There is no field filter; a caller that needs one
service's `service.state` among several filters the `--json` stream itself.

## 9. Changing the catalogue

Adding, renaming or removing an event key changes four things in the same
change: the code that calls `BeginEvent`, this file, the `event` enum and
`allOf` entries in `docs/logging-schema.json`, and the feature map in
`docs/FEATURES.md`. The three sets must stay equal:

- the string literals passed to `BeginEvent`, found with
  `grep -rhoE 'BeginEvent\([^"]*"[a-z0-9_.]+"' cmd internal`;
- the first-column keys of the tables in section 7;
- `jq -r '.properties.event.enum[]' docs/logging-schema.json`.

No line other than an event line may carry a top-level `event` key; a log call
that needs that name uses another (`settings_event`).
