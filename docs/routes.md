# Route reference

Every way into relay over HTTP, WebSocket or a network socket: its method and
path, who may call it, what it takes and what it answers. It is written for a
person who drives relay and does not read the code. The design and its
reasoning are in [`architecture.md`](architecture.md); this file is the
call-by-call contract.

Each `### <name>` heading is the route's door name, byte for byte as
`relay doors --json` prints it (for example `### POST /api/projects`).
Surfaces `relay doors` does not list carry a prefix: `proxy:`, `ws:`, `model:`,
`enrol:` and `remote:`. [`FEATURES.md`](FEATURES.md) names doors the same way,
and a feature row's Doors cell points at a heading here.

Shapes are JSON field tables. A field marked `required` must be present; every
other field may be omitted. Unknown fields are ignored unless the entry says
the body is strict.

## 1. How to read this

## Listeners

| Listener | What it is | Where its address is |
|---|---|---|
| `socket` | The frontend Unix socket. HTTP over a socket file, mode `0600`. | `sockets.frontend` in `ready.json` (see `docs/cli.md`, `relay serve`) |
| `tcp` | The loopback control-plane listener. Plain HTTP on `127.0.0.1`. It exists only when `api.listen` (or `RELAY_API_LISTEN`) is set. | `listeners.api` in `ready.json` |

Every route is on the socket. Only routes of class `read`, `configure` and
`grant` are also on TCP; `execute`, `proxy` and `chief_of_staff` routes are not
registered there. A request for one of those on TCP is answered by the Go HTTP
mux: `404` with the body `404 page not found`, or `405` with an `Allow` header
when the same path exists for another method. Nothing runs and no credential
is checked. The three login routes (section 5) exist only on TCP.

## Credential classes and the bearer header

A caller sends `Authorization: Bearer <token>`. A token is a control-plane
credential, minted once with `relay credential mint --name N --class read
--class configure ...` (the token is printed once and is not recoverable). A
credential holds a set of classes; an empty set holds nothing.

| Class | Meaning | Listeners |
|---|---|---|
| `read` | Discloses configuration or history | socket, tcp |
| `configure` | Changes relay's own state | socket, tcp |
| `grant` | Issues or revokes a credential another party holds | socket, tcp |
| `execute` | The caller supplies what runs or what is exposed | socket only |
| `proxy` | Reaches a surface relay does not classify (the `/` mount) | socket only |
| `chief_of_staff` | Held only inside the Chief of Staff scope; no credential holds it directly | socket only |
| `public` | No credential. The login routes only. | tcp only |

On the socket a request with no `Authorization` header at all is accepted when
the connecting process is a launched service holding the `frontend` capability
(`docs/launch-identity.md`). Such a caller holds `read`, `configure`, `proxy`
and `execute`, never `grant`. A test that drives relay with a minted credential
always sends the header.

The two refusals, in order:

| Status | Body (`text/plain`) | Cause |
|---|---|---|
| `401` | `unauthorized` | No header, a header that is not `Bearer <token>`, an unknown or expired token, or no credentials configured. The cases are not told apart. |
| `403` | `Forbidden` | The token is valid but its classes do not include the route's class. |

Both refusals come from the bearer middleware, before any handler runs, so
neither writes a route event. A `401` writes only the `frontend.request` line
and no `control_decision` row. A `403` also writes a `control_decision` audit
row with `outcome: denied` (`docs/events.md` section 5).

## `X-Trace-Id`

Send `X-Trace-Id: <id>` on any request. The ID must match
`[A-Za-z0-9_-]{8,64}`; anything else is replaced by a fresh one. The trace ID
is on every event line the request writes, so
`relay --config-dir DIR logs --trace ID --event KEY --json` reads them back.
The response does not echo the header.

## `X-Relay-Scope`

`X-Relay-Scope: chief-of-staff` narrows a request to the Chief of Staff scope.
It needs a credential holding `proxy`. Inside the scope only these doors
answer: `POST /api/chief-of-staff/messages`, `POST /api/chief-of-staff/sessions`,
`GET /api/sessions` and `GET /ws`. Any other door answers `403`. Any other
value of the header answers `403`. Without the header the two
`/api/chief-of-staff/messages|sessions` routes answer `403` for every
credential.

## Error bodies

| Source | Body |
|---|---|
| Door refusals (`401`, `403`) | `text/plain`, see above |
| Most handler errors | `{"error": "<text>"}`. The text is human wording; assert on the status and the event reason, not the text. |
| File routes | `{"error": "<text>", "code": "<CODE>"}`, plus `"size"` on `TOO_LARGE` |
| Chief of Staff routes | `{"error": "<code>", "message": "<text>"}` |
| Drop-in refusals | `{"error": "<code>", "message": "<text>"}` |
| Forwarded relay-sessions routes | empty body for most statuses |
| Model endpoint | the provider-shaped error JSON, see section 6 |

## Gated routes

A route marked with a Gate runs an owner presence prompt (`docs/presence-gate.md`)
before it changes anything. The gate is in the operation every door shares, not
in the route. A refused prompt, no session able to show one, or an unwritable
audit log refuses the call and changes nothing. The event line carries
`status: denied` and a `reason` of `presence_refused`, `presence_no_session`,
`presence_unavailable` or `audit_unavailable`, and a `control_decision` audit
row with `outcome: denied`, `method` set to the gate name (for example
`project.grant`) and `via: http` is written. The HTTP status of the refusal
depends on the route family:

| Family | Refusal status |
|---|---|
| Project routes (`project.grant`, `project.rotate_token`) | `403` |
| Service, MCP, enrolment and remote-config routes | `500` |

The `500` is what the code answers today; the event line still reads `denied`.
Assert on the event, not on the status, for the `500` family.

## Events and audit rows

Each entry names its event key. An event is one log line written before the
response; read it with `relay logs --trace ID --event KEY --json`
(`docs/events.md`). Keys of routes that live in relay-sessions
(`session.delete`, `session.message`, `terminal.delete`, `terminal.log`,
`model.list`, `session.ws.close`) reach the log after the response; wait for
them with `relay logs --follow --event KEY --trace ID --timeout 30s`.

The outcome of an event follows the response status unless the entry says
otherwise:

| Status | `status` | `reason` |
|---|---|---|
| 2xx | `ok` | absent |
| `401` | `denied` | `unauthorized` (on `/api/*` the bearer middleware answers a `401` before any handler, so no event is written) |
| `403` | `denied` | `not_granted` (or the presence reason above) |
| `404` | `error` | `not_found` |
| `409` | `error` | `conflict` |
| `429` | `denied` | `throttled` |
| `503` | `error` | `unavailable` |
| `504` | `error` | `timeout` |
| other 4xx | `error` | `invalid` |
| other 5xx | `error` | `internal` |

Every route also writes a `control_decision` audit row, except a `401` (`docs/audit-log.md`):
`method`, `path`, `class`, `transport` (`socket` or `tcp`), `actor.cred_id`
and `outcome` (`ok` or `denied`). Entries list only the rows a route adds
beyond it: `config_change`, `credential_issued`, `credential_revoked`,
`file_op`, `session_launch`, `session_resume`, `session_message`,
`host.probe`. Read them with `relay audit --event NAME --json` or
`GET /api/audit`.

## Writes need a working sealed store

Every mutation saves `settings.json` through the sealed store. When the store
cannot be opened (a locked or refused keychain) a mutation answers `500` with an
`error` text beginning `save settings` or `failed to save settings`. That is an
environment fault, not a contract of the route.

## 2. Frontend routes

The routes below are the `http` doors of the catalogue, grouped by the goal in
`FEATURES.md` each serves. Request bodies are JSON with `Content-Type:
application/json`; the entry gives the field table.

## Shared shapes

**Project object** (what every project route answers; never carries a token):

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Project ID |
| `name` | string | Display name |
| `path` | string | Absolute directory (a host path for a hosted project; empty for an access profile) |
| `kind` | string | `"local"` or `"remote"`; absent means local |
| `host_id` | string | SSH host the path lives on; absent for this Mac |
| `mode` | string | `"home"`, `"work"` or `"both"` |
| `default_for` | string[] | Modes this project is the default for |
| `allowed_mcp_ids` | string[] | MCP IDs the project may use; `["*"]` means all |
| `allowed_models` | string[] | Model IDs; empty or `["*"]` means all |
| `chat_templates` | object[] | Chat templates (`id`, `name`, `model`, `mode`, `voice`, `system_prompt`, `append_claude_md`, `use_relay_tools`) |
| `allowed_templates` | string[] | Terminal template IDs; `["*"]` means all |
| `created_at` | string | RFC 3339 |
| `disabled_tools` | object | MCP ID to tool names turned off |
| `context` | object | MCP ID to scope values |
| `allowed_tools` | object | MCP ID to allowed tool names (access profiles) |
| `access` | object | MCP ID to `"read"` or `"write"` |
| `allow_external` | object | MCP ID to boolean |
| `permission_policy` | object | `default_mode`, `allowed_tools`, `denied_tools` |
| `generate_skill` | boolean | Regenerate SKILL.md on change |
| `session_folders` | string[] | Folder labels offered for sessions |
| `mounts` | object[] | Mount grants: `id`, `path`, `access` (`"read"` or `"write"`) |
| `files_read_only` | boolean | File plane refuses writes |

**Service object:**

| Field | Type | Meaning |
|---|---|---|
| `id`, `display_name` | string | Identity |
| `command` | string | Program relay runs |
| `args` | string[] | Arguments |
| `env` | object | Environment (secret values are masked) |
| `working_dir` | string | Working directory |
| `autostart` | boolean | Start with relay |
| `hide_from_menu` | boolean | Hidden from the tray menu |
| `url` | string | Optional web address |
| `capabilities` | string[] | Launch-identity capabilities (`frontend`, `manifest`, `models`, `model_host`, ...) |
| `allowed_models` | string[] | Model grant for the `models` capability; empty means none |
| `running` | boolean | Process is up |
| `process_error` | string | Set when the record was saved but starting it failed |

**Host object:**

| Field | Type | Meaning |
|---|---|---|
| `id`, `name` | string | Identity |
| `target` | string | SSH destination |
| `port` | number | SSH port; absent means 22 |
| `identity_file`, `tmux_path` | string | Optional overrides |
| `created_at` | string | RFC 3339 |
| `probe` | object | Last probe: `at`, `ok`, `os`, `arch`, `home`, `shell`, `node_path`, `node_version`, `claude_path`, `claude_version`, `tmux_path`, `error` |
| `status` | string | `unknown`, `unreachable`, `idle` or `connected` |
| `ssh_argv` | string[] | The ssh command prefix relay uses |
| `terminal_templates` | object[] | Terminal template objects (below); always an array |

**Terminal template object:**

| Field | Type | Meaning |
|---|---|---|
| `id` | string | `[A-Za-z0-9._-]+`; set by the path on `PUT` |
| `name` | string | Display name (required) |
| `command`, `args` | string, string[] | What the terminal runs |
| `env` | object | Environment; `${MODEL_KEY}` allowed only with `model_key` |
| `description`, `icon` | string | Display |
| `idleTimeout` | number | Minutes; `0` means the default |
| `env_passthrough` | string[] | Host variables copied into the child |
| `sandbox` | boolean | Absent or `true` means sandboxed |
| `read`, `read_write`, `deny` | string[] | Sandbox folders |
| `model_key` | boolean | Mint a model key for the session |
| `persist` | boolean | Persistent tmux terminal (hosted projects) |

**Enrolment object:**

| Field | Type | Meaning |
|---|---|---|
| `client_id` | string | Client identity |
| `fingerprint` | string | Certificate SHA-256 |
| `project_ids` | string[] | Access profiles the client may use |
| `budget` | object | `window_seconds`, `max_calls`, `max_result_bytes`, `mount_max_ops`, `mount_max_read_bytes`, `mount_max_write_bytes` |
| `created_at` | string | RFC 3339 |
| `dir` | string | Bundle directory; only on create |
| `bundle_error` | string | Set when the record was saved but the bundle write failed |
| `cli_admin` | boolean | Client may use the remote configuration plane |

**MCP object:** `id`, `display_name`, `transport` (`"stdio"` or `"http"`),
`command`, `args`, `env` (secret values masked), `url`, `tcc_services`, and
`auth_required` (boolean; only on create, when the server answered `401`).

## 2.1 Sessions and terminals (G1)

Creating a session or terminal is relay's own door: relay authorizes the launch
against the project, then asks the session host to start it. Listing and
stopping are forwarded to the session host (section 3).

### POST /api/sessions

- **Listeners:** socket. **Credential:** `execute`. **Gate:** none.
- **Request:** JSON body.

  | Field | Type | Meaning |
  |---|---|---|
  | `projectId` | string | Project to launch in (required for a project session) |
  | `model` | string | `haiku`, `sonnet` or `opus` (Claude), `pi/<id>`, `codex/<id>`, or any other model ID (a chat session) |
  | `directory` | string | Working directory; must lie inside the project |
  | `name` | string | Session name |
  | `settings` | object | Client settings, merged under the project's `permission_policy`; `agent: true` makes a tracked headless agent |
  | `systemPrompt` | string | Chat and pi system prompt |
  | `appendClaudeMd` | boolean | Append the project's CLAUDE.md |

  The body is at most 1 MiB.
- **Response `201`:** the session object: `sessionId`, `projectId`, `name`,
  `directory`, `model`, `providerType`, `createdAt`, `messages`, `stats`, and
  `headless`, `agent`, `origin`, `host` when set.
- **Errors:**
  - `400` malformed JSON or an invalid field;
  - `403` the project is not available for a launch, the model or directory is
    not allowed, or the model is reserved for system use;
  - `413` body too large;
  - `502` the session host answered with a failure (`{"error":"launch failed"}`);
  - `503` the session ledger or the session host is unavailable.
- **Event:** `session.launch` (fields `session_id`, `project_id`, `kind`). On a
  launch refusal the status is `denied` and `reason` is the launch refusal
  code, including for a `400` such as a blank model. A malformed or oversized
  body writes no `session.launch` event.
- **Audit row:** `session_launch`, with `outcome` `ok`, `denied` or `error`.
- **CLI equivalent:** `relay session start`.

### POST /api/sessions/{$}

The trailing-slash form of `POST /api/sessions`: same listeners, credential,
request, response, errors and event.

### POST /api/terminals

- **Listeners:** socket. **Credential:** `execute`. **Gate:** none.
- **Request:** JSON body.

  | Field | Type | Meaning |
  |---|---|---|
  | `projectId` | string | Project (required) |
  | `templateId` | string | Terminal template; must be in the project's `allowed_templates` |
  | `name` | string | Terminal name |
  | `directory` | string | Working directory inside the project |
  | `cols`, `rows` | number | Terminal size |
  | `persist_session` | string | Persistent tmux session name (hosted projects) |
  | `extraArgs` | string[] | Appended to a local terminal's argv (at most 64 entries, 64 KiB) |

- **Response `201`:** `terminalId`, `templateId`, `name`, `directory`, `host`
  (an object, empty for this Mac).
- **Errors:** as `POST /api/sessions`; an unavailable template is `403`.
- **Event:** `session.launch` (`kind` is `pty`). **Audit row:** `session_launch`.
- **CLI equivalent:** `relay terminal start`.

### POST /api/terminals/{$}

The trailing-slash form of `POST /api/terminals`: same contract.

### POST /api/sessions/{id}/resume

- **Listeners:** socket. **Credential:** `execute`. **Gate:** none.
- **Request:** path `id` is the session ID; no body.
- **Response `200`:** `{"session_id": "<id>", "resumed": <boolean>}`.
  `resumed` is `false` when the session was already live.
- **Errors:** `404` unknown session; `409` a resume of the same session is in
  progress; `403` or `400` the launch is refused; `502` the session host failed.
- **Event:** `session.resume` (`session_id`, `project_id`).
- **Audit row:** `session_resume`.
- **CLI equivalent:** `relay session resume`.

### POST /api/sessions/{id}/drop-in

Stops a headless Claude session and starts a terminal that resumes its
conversation.

- **Listeners:** socket. **Credential:** `execute`. **Gate:** none.
- **Request:** path `id`; optional body `{"cols": <number>, "rows": <number>}`
  (defaults 120 by 40).
- **Response `201`:** `sessionId`, `claudeSessionId`, `host` (set for an SSH
  host), `terminal` (the terminal object of `POST /api/terminals`).
- **Errors:** `{"error": "<code>", "message": "<text>"}` with `404`
  `session_not_found`, `409` `not_claude` or a handoff conflict, `502`
  `unavailable`, or the launch refusal's own status.
- **Event:** `session.drop_in`. **Audit row:** `session_launch`.
- **CLI equivalent:** `relay drop-in` (needs an interactive terminal).

### GET /api/sessions

- **Listeners:** socket. **Credential:** `proxy`. **Gate:** none.
- **Request:** none.
- **Response `200`:** forwarded from the session host; shape in
  `proxy:GET /api/sessions`.
- **Errors:** `503` `{"error":"session host unavailable"}` or `session host
  ledger unavailable`.
- **Event:** `session.list` (`count`). **CLI equivalent:** `relay session list`.

### GET /api/terminals

- **Listeners:** socket. **Credential:** `proxy`. **Gate:** none.
- **Response `200`:** forwarded; shape in `proxy:GET /api/terminals`.
- **Errors:** as `GET /api/sessions`.
- **Event:** `terminal.list` (`count`). **CLI equivalent:** `relay terminal list`.

### POST /api/chief-of-staff/messages

Sends one message to a live session as the Chief of Staff. The origin is fixed
by relay; a body cannot set it.

- **Listeners:** socket. **Credential:** `chief_of_staff` (needs
  `X-Relay-Scope: chief-of-staff` and a credential holding `proxy`).
  **Gate:** none.
- **Request:** JSON body, at most 64 KiB, `{"sessionId": "<id>", "text": "<text>"}`
  (both required).
- **Response `202`:** `{"sessionId", "origin": "chief-of-staff", "at"}` (`at` is RFC 3339).
- **Errors** (`{"error": "<code>", "message": "<text>"}`):

  | Status | `error` | Cause |
  |---|---|---|
  | `400` | `invalid_body`, `session_id_required`, `text_required` | Bad body |
  | `413` | `body_too_large` | Over 64 KiB |
  | `404` | `session_not_found` | Unknown session |
  | `409` | `already_processing`, `resume_required`, `dropped_in` | Session cannot take a message now |
  | `502` | `session_host_unavailable` | Host unreachable or delivery failed |
  | `503` | `audit_unavailable` | Auditing is off or cannot record |

- **Event:** `chief_of_staff.send` (`session_id`, `origin`).
- **Audit row:** `session_message` with `phase` `intent`, then `completion`;
  the intent row is durable before delivery.

### POST /api/chief-of-staff/sessions

Starts a headless agent or a Claude terminal on the Chief of Staff's behalf.

- **Listeners:** socket. **Credential:** `chief_of_staff`. **Gate:** none.
- **Request:** JSON body, at most 64 KiB.

  | Field | Type | Meaning |
  |---|---|---|
  | `projectId` | string | Registered local project (required) |
  | `folder` | string | Relative folder inside the project; no `..`, not absolute |
  | `prompt` | string | First prompt (required, at most 8000 characters) |
  | `model` | string | Model ID (required) |
  | `mode` | string | `"headless"` (default) or `"terminal"` (a Claude model only) |

- **Response `201`:** `sessionId`, `name`, `projectId`, `directory`, `mode`,
  `kind`, `origin` (`"chief-of-staff"`), `at`.
- **Errors** (`{"error": "<code>", "message": "<text>"}`): `400`
  `invalid_body`, `project_id_required`, `prompt_required`, `prompt_too_long`,
  `model_required`, `mode_invalid`, `folder_invalid`, `folder_not_found`,
  `terminal_on_host`, `terminal_needs_claude`; `403` `project_not_available`
  (unknown or remote project), `model_not_allowed`, `directory_outside_project`;
  `413` `body_too_large`; `502` `launch_failed`, `prompt_not_delivered`;
  `503` `audit_unavailable`.
- **Event:** `chief_of_staff.start` (`session_id`, `project_id`, `kind`).
- **Audit row:** `session_launch` with `origin: chief-of-staff`; a refusal
  after the audit check also writes one with `outcome: denied`.

### GET /api/chief-of-staff/config

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** `{"configured": <boolean>, "projectId", "model", "dailyModelCalls"}`;
  the last three are absent when not configured.
- **Event:** `chief_of_staff.config.get`.

### PUT /api/chief-of-staff/config

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request** (strict; unknown fields refused, at most 4 KiB):
  `{"projectId": string, "model": string, "dailyModelCalls": number}`; all
  three required. `dailyModelCalls` is a whole number in the allowed range.
- **Response `200`:** the config view of `GET`, with `configured: true`.
- **Errors:** `400` `invalid_body` or a validation code such as
  `daily_model_calls_invalid`; `413` `body_too_large`; `500` `save_failed`.
  Bodies are `{"error": "<code>", "message": "<text>"}`.
- **Event:** `chief_of_staff.config.set` (`project_id`).

### DELETE /api/chief-of-staff/config

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `200`:** `{"configured": false}`.
- **Event:** `chief_of_staff.config.clear`.

### GET /api/terminal/templates

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Request:** query `project` (project ID). With no project, or an unknown
  one, the list is empty. A hosted project gets its host's templates.
- **Response `200`:** array of terminal template objects the project allows.
- **Event:** `template.list` (`count`).

### GET /api/terminal/templates/{id}

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** a terminal template object. **Errors:** `404`.
- **Event:** `template.get` (`template_id`).

### POST /api/terminal/templates

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** a terminal template object (`id` and `name` required).
- **Response `201`:** the template as saved.
- **Errors:** `400` invalid id or field; `409` the ID exists.
- **Event:** `template.create` (`template_id`).

### PUT /api/terminal/templates/{id}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** a terminal template object; the path `id` overrides any `id` in the body.
- **Response `200`:** the template as saved. **Errors:** `400`, `404`.
- **Event:** `template.update` (`template_id`).

### DELETE /api/terminal/templates/{id}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `204`.** **Errors:** `404`.
- **Event:** `template.remove` (`template_id`).

## 2.2 Projects and grants (G2)

### GET /api/projects

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** array of project objects (`[]` when none).
- **Event:** `project.list` (`count`). **Audit row:** none beyond `control_decision`.

### GET /api/projects/{id}

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** a project object. **Errors:** `404` `{"error":"project not found"}`.
- **Event:** `project.get` (`project_id`).

### POST /api/projects

- **Listeners:** socket, tcp. **Credential:** `configure`.
  **Gate:** `project.grant`.
- **Request:** JSON body.

  | Field | Type | Meaning |
  |---|---|---|
  | `name` | string | Display name (required) |
  | `path` | string | Absolute directory (required for a local project) |
  | `kind` | string | `"local"` (default) or `"remote"` (an access profile) |
  | `host_id` | string | Host the path lives on |
  | `mode` | string | `"home"`, `"work"` or `"both"` |
  | `allowed_mcp_ids`, `allowed_models`, `allowed_templates` | string[] | Grants; `["*"]` means all |
  | `chat_templates` | object[] | See the project object |
  | `permission_policy` | object | `default_mode` is one of `""`, `default`, `acceptEdits`, `plan`, `bypassPermissions` |
  | `generate_skill` | boolean | |
  | `disabled_tools`, `allowed_tools` | object | MCP ID to tool names |
  | `access` | object | MCP ID to `"read"` or `"write"` |
  | `allow_external` | object | MCP ID to boolean |
  | `context` | object | MCP ID to scope values |
  | `session_folders` | string[] | |
  | `mounts` | object[] | `id`, `path`, `access` (remote kind only) |

- **Response `201`:** the new project object. The project's token is created
  and sealed; it is not in the response.
- **Errors:** `400` invalid body, invalid permission policy, or a grant that
  fails validation; `403` the prompt was refused (or no session, or auditing
  off); `500` the save failed.
- **Event:** `project.create` (`project_id`, `kind`). Presence refusals write
  the event with `status: denied`.
- **Audit row:** `config_change` (credential `project`, subject the project ID).
- **CLI equivalent:** `relay project create`.

### PUT /api/projects/{id}

A patch: a field absent from the body is left alone; a field present replaces
the stored value.

- **Listeners:** socket, tcp. **Credential:** `configure`.
  **Gate:** `project.grant`, only when the change widens a grant. Narrowing, a
  rename or `files_read_only` never prompts.
- **Request:** the fields of `POST /api/projects` plus `files_read_only`
  (boolean), all optional.
- **Response `200`:** the updated project object.
- **Errors:** `400` validation; `403` prompt refused; `404` `{"error":"project not found"}`;
  `409` the project changed while the prompt was open; `500` save failed.
- **Event:** `project.update` (`project_id`, `gated`).
- **Audit row:** `config_change` when the change widened a grant.
- **CLI equivalent:** `relay project update` (flags), `relay project edit` (JSON file).

### DELETE /api/projects/{id}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `204`.** **Errors:** `404`; `500` save failed.
- **Event:** `project.remove` (`project_id`). Live sessions of the project end.
- **CLI equivalent:** `relay project remove`.

### PUT /api/default_project/{mode}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** path `mode` is `home` or `work`. Strict body
  `{"project_id": "<id>"}`; the key is required and `""` clears the default.
- **Response `200`:** `{"home": "<id>", "work": "<id>"}` (`""` means none).
- **Errors:** `400` missing key, unknown mode, or unknown project.
- **Event:** `project.default.set` (`mode`, `project_id`).

### POST /api/projects/{id}/rotate_token

- **Listeners:** socket, tcp. **Credential:** `grant`.
  **Gate:** `project.rotate_token`.
- **Request:** path `id`; no body.
- **Response `200`:** `{"token": "<new plaintext>"}`. This is the only response
  in relay that carries a project token. The old token stops working at once.
- **Errors:** `403` prompt refused, or the rotation could not be recorded (no
  token is returned then); `404` project not found.
- **Event:** `project.rotate_token` (`project_id`).
- **Audit row:** `credential_issued` (credential `project_token`, never the token).
- **CLI equivalent:** `relay project rotate-token`.

### POST /api/projects/{id}/regen_skill

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `200`:** `{"path": "<skills directory>"}`.
- **Errors:** `404`; `400` hosted project or a project with no path; `503` skill
  regeneration is unavailable in this process; `500` other failures.
- **Event:** `project.regen_skill` (`project_id`).
- **CLI equivalent:** `relay project regen-skill`.

## 2.3 MCP servers and tools (G3)

### GET /api/mcps

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** array of `{"id", "display_name"}`.
- **Event:** `mcp.list` (`count`). **CLI equivalent:** `relay mcp list`.

### POST /api/mcps

- **Listeners:** socket. **Credential:** `execute`. **Gate:** `mcp.register`.
- **Request:**

  | Field | Type | Meaning |
  |---|---|---|
  | `display_name` | string | Required |
  | `id` | string | Optional; default is a slug of `display_name` |
  | `transport` | string | `"stdio"` or `"http"` |
  | `command`, `args`, `env` | string, string[], object | stdio server |
  | `url` | string | http server (checked against the SSRF guard) |
  | `tcc_services` | string[] | macOS privacy services the server needs |

- **Response `201`:** an MCP object; `auth_required: true` when the server
  answered `401` during discovery (the record is saved).
- **Errors:** `400` invalid; `502` discovery failed; refusal as in "Gated routes" (`500`).
- **Event:** `mcp.register` (`mcp_id`, `transport`). **Audit row:** `config_change`
  (credential `external_mcp`).
- **CLI equivalent:** `relay mcp register`.

### DELETE /api/mcps/{id}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `204`.** **Errors:** `404` `{"error":"mcp not found: <id>"}`.
- **Event:** `mcp.unregister` (`mcp_id`). **Audit row:** `config_change`.
- **CLI equivalent:** `relay mcp unregister`.

### GET /api/mcps/{id}/tools

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** array of `{"name", "description", "category"}`.
- **Errors:** `404` the MCP is not registered or not connected; `503` no tool
  provider in this process.
- **Event:** `mcp.tools.list` (`mcp_id`, `count`).

### GET /api/mcps/{id}/scope_fields

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** array of scope fields the MCP lets an operator narrow:
  `name`, `type`, `item_type`, `description`, `source`, `applies_to`,
  `enumerable`, `depends_on`.
- **Errors:** `404` the MCP is not registered or not connected.
- **Event:** `mcp.scope_fields.get` (`mcp_id`).
- **CLI equivalent:** `relay mcp scope-fields`.

### POST /api/mcps/{id}/enumerate

Asks the MCP for the real values of one scope field. A `read` route: it
discloses and changes nothing.

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Request:** `{"field": "<name>", "values": {"<dependency field>": <chosen value>}}`
  (`values` optional).
- **Response:** `{"mcp_id", "field", "status", "values": [{"value", "label"}], "error"}`.
  `values` is `null` when nobody could look and `[]` when there is nothing.

  | `status` | HTTP |
  |---|---|
  | `ok`, `unsupported` | `200` |
  | `not_enumerable` | `400` |
  | `unknown_mcp` | `404` |
  | `invalid_field` | `502` |
  | `unavailable` | `503` |

- **Event:** `mcp.scope_field.enumerate` (`mcp_id`, `field`).

## 2.4 Audit (G4)

### GET /api/audit

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Request:** query, all optional: `project_id`, `mcp_id`, `outcome`
  (`ok`, `error`, `tool_error`, `denied`, `unauthorized`, `throttled`,
  `pending`), `event` (an audit event name such as `control_decision`), `kind`
  (the actor kind: `project`, `service`, `remote`, `control`, `operator`,
  `relay`, `unknown`), `text`, `limit` (integer), `deep` (boolean, searches the
  file on disk).
- **Response `200`:** array of audit rows, newest first (`id`, `ts`, `dur_ms`,
  `event`, `actor`, `outcome`, `error`, `method`, `path`, `class`, `transport`,
  and the event's own fields). Secrets are redacted.
- **Errors:** `400` `limit` or `deep` unparseable, or an unknown `outcome` or `kind`.
- **Event:** `audit.query` (`count`). **CLI equivalent:** `relay audit`.

### GET /api/audit/log

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** `{"path": "<audit log file>"}`. **Event:** `audit.path.get`.

### POST /api/audit/export

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** the query fields of `GET /api/audit` as a JSON body.
- **Response `201`:** `{"path": "<export file>"}`.
- **Errors:** `400` invalid query; `404` no log; `500`.
- **Event:** `audit.export` (`count`).

## 2.5 Services (G5)

### GET /api/services

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** array of service objects. **Event:** `service.list` (`count`).
- **CLI equivalent:** `relay service list`.

### GET /api/services/{id}

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** a service object. **Errors:** `404` `service not found: <id>`.
- **Event:** `service.get` (`service_id`).

### POST /api/services

- **Listeners:** socket. **Credential:** `execute`. **Gate:** `service.register`.
- **Request:**

  | Field | Type | Meaning |
  |---|---|---|
  | `display_name` | string | Required |
  | `command` | string | Required |
  | `id` | string | Default is a slug of `display_name` |
  | `args` | string[] | |
  | `env` | object | A `null` value removes a key (on update) |
  | `working_dir`, `url` | string | |
  | `autostart` | boolean | |
  | `capabilities` | string[] | Absent leaves stored value alone on update; empty on create |
  | `allowed_models` | string[] | As `capabilities` |

- **Response `201`:** a service object (with `process_error` when the start failed).
- **Errors:** `400` invalid; `409` the record changed while the prompt was open;
  refusal as in "Gated routes" (`500`).
- **Event:** `service.create` (`service_id`).
- **Audit row:** `config_change` (credential `service`).
- **CLI equivalent:** `relay service register`.

### PUT /api/services/{id}

- **Listeners:** socket. **Credential:** `execute`. **Gate:** `service.register`,
  when the request changes a stored value of `display_name`, `command`, `args`,
  `env`, `working_dir`, `url`, `autostart`, `capabilities` or `allowed_models`.
  A request that changes nothing does not prompt.
- **Request:** as `POST /api/services`.
- **Response `200`:** a service object. **Errors:** as `POST`; `404`.
- **Event:** `service.update` (`service_id`). **Audit row:** `config_change`.
- **CLI equivalent:** `relay service register` (same ID updates).

### DELETE /api/services/{id}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `204`.** **Errors:** `404`.
- **Event:** `service.unregister` (`service_id`). **Audit row:** `config_change`.
- **CLI equivalent:** `relay service unregister`.

### POST /api/services/{id}/start

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `200`:** the service object. **Errors:** `404`.
- **Event:** `service.start` (`service_id`). **CLI equivalent:** `relay service start`.

### POST /api/services/{id}/stop

- As `start`. **Event:** `service.stop`. **CLI equivalent:** `relay service stop`.

### PUT /api/services/{id}/autostart

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** `{"autostart": <boolean>}`. **Response `200`:** the service object.
- **Errors:** `400`, `404`. **Event:** `service.autostart.set` (`service_id`, `autostart`).

### PUT /api/services/{id}/position

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** `{"index": <number>}` (required, zero based).
- **Response `200`:** the full list of service objects in the new order.
- **Errors:** `400` `index is required`; `404`.
- **Event:** `service.move` (`service_id`, `index`).

### PUT /api/services/{id}/menu

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** `{"hidden": <boolean>}`. **Response `200`:** the service object.
- **Errors:** `400`, `404`. **Event:** `service.menu.set` (`service_id`, `hidden`).

## 2.6 Remote clients (G10)

### GET /api/enrolments

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** array of enrolment objects. **Event:** `enrolment.list` (`count`).
- **CLI equivalent:** `relay enrol list`.

### GET /api/enrolments/{id}

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** an enrolment object. **Errors:** `404`.
- **Event:** `enrolment.get` (`client_id`).

### POST /api/enrolments

Creates a client certificate bundle on this Mac.

- **Listeners:** socket, tcp. **Credential:** `grant`. **Gate:** `enrolment.create`.
- **Request:** `{"client_id": string (required), "project_ids": string[], "budget": object}`
  (`budget` fields as the enrolment object).
- **Response `201`:** an enrolment object with `dir`. The private key is in the
  bundle directory, never in the response.
- **Errors:** `400` invalid client ID or budget; refusal as in "Gated routes"
  (`500`); the issuance is refused when auditing is off.
- **Event:** `enrolment.create` (`client_id`).
- **Audit row:** `credential_issued` (credential `enrolment`). It is written
  before the bundle is announced; an unrecorded create is undone.
- **CLI equivalent:** `relay enrol create`.

### DELETE /api/enrolments/{id}

- **Listeners:** socket, tcp. **Credential:** `grant`. **Gate:** `enrolment.revoke`.
- **Response `204`.** Live connections of that client close. **Errors:** `404`; refusal `500`.
- **Event:** `enrolment.revoke` (`client_id`). **Audit row:** `credential_revoked`.
- **CLI equivalent:** `relay enrol revoke`.

### GET /api/remote

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:**

  | Field | Type | Meaning |
  |---|---|---|
  | `configured` | boolean | A `remote` block exists |
  | `enabled` | boolean | Tool-plane listener on |
  | `listen` | string | Configured address; `""` when unset |
  | `effective` | string | Address that would be bound |
  | `audit_enabled` | boolean | Auditing is on |
  | `ca_fingerprint` | string | CA certificate SHA-256; absent until a CA exists |
  | `enrolment_requests` | boolean | Enrolment-request listener on |
  | `enrolment_listen` | string | Configured address |
  | `enrolment_effective` | string | Address that would be bound |

- **Event:** `remote.config.get`. **CLI equivalent:** `relay remote show`.

### PUT /api/remote

- **Listeners:** socket. **Credential:** `execute`. **Gate:** `remote.configure`,
  on any change of a listener switch or address, and on `remove`.
- **Request:** `{"remove": boolean, "enabled": boolean, "listen": string,
  "enrolment_requests": boolean, "enrolment_listen": string}`. The body is the
  full record: an omitted field reads as `false` or `""`. `remove: true` deletes
  the block. Addresses must be loopback. `enrolment_requests: true` needs
  `enabled: true`.
- **Response `200`:** the view of `GET /api/remote`.
- **Errors:** `400` invalid; `409` the block changed while the prompt was open;
  refusal `500`.
- **Event:** `remote.configure` (`enabled`). **Audit row:** `config_change`
  (credential `remote`).
- **CLI equivalent:** `relay remote set`.

## 2.7 SSH hosts (G11)

### GET /api/hosts

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** array of host objects. **Event:** `host.list` (`count`).

### GET /api/hosts/{id}

- **Response `200`:** a host object. **Errors:** `404` `host not found: <id>`.
  **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Event:** `host.get` (`host_id`).

### POST /api/hosts

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** `{"name": string (required), "target": string (required), "port": number,
  "identity_file": string, "tmux_path": string}`.
- **Response `201`:** a host object (a probe runs as part of the create).
- **Errors:** `400` `host name is required` and similar. **Event:** `host.create` (`host_id`).
- **Audit row:** `host.probe`.

### PUT /api/hosts/{id}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** the fields of `POST /api/hosts`, all optional.
- **Response `200`:** the host object. **Errors:** `400`; `404`.
- **Event:** `host.update` (`host_id`). **Audit row:** `host.probe` when the connection fields changed.

### DELETE /api/hosts/{id}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `204`.**
- **Errors:** `404`; `409` the host is used by projects:
  `{"error": "host is used by one or more projects", "projects": [...]}`.
- **Event:** `host.remove` (`host_id`).

### POST /api/hosts/{id}/probe

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `200`:** the host object with a fresh `probe`. **Errors:** `404`.
- **Event:** `host.probe` (`host_id`). **Audit row:** `host.probe`.
- **CLI equivalent:** `relay host probe`.

### POST /api/hosts/{id}/disconnect

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `200`:** the host object. **Errors:** `404`.
- **Event:** `host.disconnect` (`host_id`). **CLI equivalent:** `relay host disconnect`.

### GET /api/hosts/{id}/templates

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** array of terminal template objects. **Errors:** `404`.
- **Event:** `host_template.list` (`host_id`, `count`).

### POST /api/hosts/{id}/templates

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** a terminal template object. **Response `201`:** the template.
- **Errors:** `400`; `404` unknown host; `409` the ID exists.
  **Event:** `host_template.create` (`host_id`, `template_id`).

### PUT /api/hosts/{id}/templates/{tid}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** a terminal template object; path `tid` overrides its `id`.
- **Response `200`:** the template. **Errors:** `400`, `404`.
- **Event:** `host_template.update`.

### DELETE /api/hosts/{id}/templates/{tid}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `204`.** **Errors:** `404`. **Event:** `host_template.remove`.

### GET /api/projects/{id}/persistent-sessions

Lists the tmux sessions relay started for a hosted project's persist templates.

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** array of `{"name", "template_id", "n", "created", "attached", "attached_here"}`
  (`created` is Unix seconds).
- **Errors:** `404` the project is not a hosted project, or no such session;
  `409` tmux is missing on the host; `502` the host is unreachable.
- **Event:** `session.persistent.list` (`project_id`).
- **CLI equivalent:** `relay terminal persistent-list`.

### DELETE /api/projects/{id}/persistent-sessions/{name}

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Response `204`.** **Errors:** `400` invalid name; `404`; `409`; `502`.
- **Event:** `session.persistent.kill` (`project_id`).
- **CLI equivalent:** `relay terminal persistent-kill`.

### POST /api/hosts/{id}/pastetmp

Writes a pasted blob to a temporary file on the host.

- **Listeners:** socket. **Credential:** `execute`. **Gate:** none.
- **Request:** `{"name": string, "data_b64": string}`; the body is at most 16 MiB,
  and the decoded data at most 10 MiB.
- **Response `200`:** `{"path": "<temp path on the host>"}`.
- **Errors** (`{"error", "code"}`): `404` `HOST_NOT_FOUND`, `400` `INVALID`,
  `413` `TOO_LARGE`, `503` `HOST_UNREACHABLE`.
- **Event:** `host.pastetmp` (`host_id`).

## 2.8 Project files (G13)

Every file route works on a project's directory, whether it is on this Mac or
on an SSH host. All are socket-only, class `execute`, ungated. A path is
relative to the project root. The rules are in
[`project-files.md`](project-files.md): a `..` segment is `TRAVERSAL`, a
symbolic link in any component is `SYMLINK`, and a read-only project refuses
changes with `READ_ONLY`. The body of every route except write is at most 64 KiB.

**File error codes** (`{"error": "<text>", "code": "<CODE>"}`):

| Code | HTTP | Meaning |
|---|---|---|
| `PROJECT_NOT_FOUND`, `HOST_NOT_FOUND`, `ENOENT` | `404` | Unknown project, host or path |
| `NOT_AVAILABLE`, `TRAVERSAL`, `SYMLINK`, `READ_ONLY`, `EACCES` | `403` | Refused (`NOT_AVAILABLE`: an access profile or a project with no path) |
| `INVALID`, `EISDIR`, `ENOTDIR` | `400` | Bad request |
| `EEXIST` | `409` | The name is taken |
| `TOO_LARGE` | `413` | Over a limit (`size` is set when known) |
| `HOST_UNREACHABLE`, `AUDIT_UNAVAILABLE` | `503` | Host down, or a mutation could not be recorded |
| `TIMEOUT` | `504` | |
| `ERROR`, `GIT_MISSING` | `500` | |

All file routes share the **events** `file.<op>` (`project_id`; an unopenable
project writes the same key with the refusal reason) and, for mutations only,
a `file_op` audit row (`outcome` `ok`, `denied` or `error`; `reason`
`symlink`, `read_only`, `traversal`). Reads write no `file_op` row.
**CLI equivalent:** none, except `relay files watch` for watching.

### POST /api/projects/{id}/files/list

- **Request:** `{"path": "<dir, '' for the root>", "show_hidden": boolean}`.
- **Response `200`:** `{"entries": [{"name", "type", "size", "mtime_ms"}]}`.
  `type` is `file`, `directory` or `symlink`. A link is listed and not followed.
- **Event:** `file.list`.

### POST /api/projects/{id}/files/stat

- **Request:** `{"path": string}`. **Response `200`:** `{"type", "size", "mtime_ms"}`.
- **Event:** `file.stat`.

### POST /api/projects/{id}/files/read

- **Request:** `{"path": string, "max_bytes": number}` (`max_bytes` optional;
  hard cap 10 MiB).
- **Response `200`:** `{"content": "<text>", "size": <bytes read>}`.
  A file over the cap is `413` `TOO_LARGE`.
- **Event:** `file.read`.

### GET /api/projects/{id}/files/stream

- **Request:** query `path`. Honors `Range` on a console file.
- **Response `200` (or `206`):** the raw bytes, `Content-Type:
  application/octet-stream`. Errors arrive as the JSON error body before any byte.
- **Event:** `file.stream`, written when the body has been fully sent or failed.

### POST /api/projects/{id}/files/write

- **Request:** `{"path": string, "content": string, "encoding": "utf8"|"base64",
  "create_only": boolean}`. `encoding` defaults to `utf8`. `create_only`
  refuses an existing file with `EEXIST`. The body is at most 16 MiB and the
  decoded content at most 10 MiB.
- **Response `200`:** `{"path": "<path as written>"}`.
- **Event:** `file.write`. **Audit row:** `file_op`.

### POST /api/projects/{id}/files/mkdir

- **Request:** `{"parent": "<dir, '' for the root>", "name": string}`.
- **Response `200`:** `{"path": "<new directory>"}`. **Event:** `file.mkdir`. **Audit row:** `file_op`.

### POST /api/projects/{id}/files/rename

- **Request:** `{"path": string, "new_name": string}` (a bare name).
- **Response `200`:** `{"path": "<new path>"}`; `409 EEXIST` when the name is taken.
- **Event:** `file.rename`. **Audit row:** `file_op`.

### POST /api/projects/{id}/files/move

- **Request:** `{"path": string, "dest_dir": string}`.
- **Response `200`:** `{"path": "<new path>"}`; `409 EEXIST` when taken.
- **Event:** `file.move`. **Audit row:** `file_op`.

### POST /api/projects/{id}/files/delete

- **Request:** `{"path": string}`.
- **Response `200`:** `{"trashed": <boolean>}`. `true` on this Mac (moved to the
  Trash); `false` on a host (deleted).
- **Event:** `file.delete`. **Audit row:** `file_op`.

### POST /api/projects/{id}/files/search

- **Request:** `{"query": string, "regex": boolean, "word": boolean,
  "case_sensitive": boolean|null, "globs": string[], "max_matches": number}`.
  `case_sensitive` absent means smart case. At most 5 globs and 500 matches.
- **Response `200`:** `{"matches": [{"path", "line", "col", "len", "text"}], "truncated": boolean}`.
  `line` and `col` are one-based; `col` and `len` count UTF-16 code units.
- **Event:** `file.search`.

### POST /api/projects/{id}/files/git

Read-only git, sandboxed, with an argument allowlist and a 10 second limit.

- **Request:** `{"cwd": string, "args": string[], "max_bytes": number}`.
- **Response `200`:** `{"exit_code": number, "stdout_b64": string, "stderr": string}`.
  A non-zero exit is a result, not an error.
- **Event:** `file.git`.

### GET /ws/files

WebSocket upgrade. Class `execute`, socket only. Frames are in section 4.
- **Event:** `file.ws.close`, at connection end. **CLI equivalent:** `relay files watch`.

## 2.9 Eve's passkey mirror (G7)

Eve (the web front end) uses these three doors to open a one-time window for a
second browser and to mirror its passkey list. The design is in
[`eve-passkey-enrolment.md`](eve-passkey-enrolment.md).

### GET /api/eve/passkey-enrolment

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** `{"open": false}` or `{"open": true, "expires": "<RFC 3339>"}`.
- **Event:** `eve.enrolment.status`, written on failure only.

### POST /api/eve/passkey-enrolment/consume

Spends an open window for one browser.

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** `{"ip": string, "label": string}`.
- **Response `200`:** `{"expires": "<RFC 3339>"}`.
- **Errors:** `409` the window is closed, already spent, or could not be
  saved; `400` malformed body.
- **Event:** `eve.enrolment.consume`.

### PUT /api/eve/passkeys

- **Listeners:** socket, tcp. **Credential:** `configure`. **Gate:** none.
- **Request:** `{"passkeys": [{"id", "label", "created", "last_used"}]}`.
- **Response `200`:** `{"revocations": ["<passkey id>"]}` (pending revocations).
- **Event:** `eve.passkey.report` (`count`).

### GET /api/eve/passkeys/revocations

- **Listeners:** socket, tcp. **Credential:** `read`. **Gate:** none.
- **Response `200`:** `{"revocations": ["<passkey id>"]}`.
- **Event:** `eve.passkey.revocations`, written on failure only.

## 2.10 The `/` mount

### /

The catch-all mount. It has no method: it matches every method and every path no
other door claims.

- **Listeners:** socket. **Credential:** `proxy`. **Gate:** none.
- **What it does:** looks the request path up in the manifests of the services
  relay supervises and reverse-proxies the request to the matching service,
  with relay's own credential stripped and the service's internal bearer added.
  A WebSocket upgrade is proxied the same way, with `X-Trace-Id` carried to
  the service.
- **Responses:** the service's own. `404` with the text body `no service
  registered for this path` when no manifest claims the path.
- **Session create guard:** a `POST` that creates a session or terminal and
  reaches this mount (for example a trailing-slash variant) is checked against
  the project's model allowlist first; a disallowed model is `403`.
- **Event:** none. **Audit row:** `control_decision` only.

The relay-sessions routes behind this mount are section 3.

## 3. Behind the `/` mount (relay-sessions)

The session host publishes these routes through its manifest. A caller reaches
them through the `/` mount (`proxy` class, socket only). Relay never inspects
the answers. A credential holding `proxy` can call all of them. The events they
write come from relay-sessions and arrive after the response.

### proxy:GET /api/sessions

Also relay's own door `GET /api/sessions` (section 2.1), which forwards here.

- **Response `200`:** `{"sessions": [ ... ]}`; each row:

  | Field | Type | Meaning |
  |---|---|---|
  | `id` | string | Session ID |
  | `projectId`, `name`, `directory`, `model` | string | |
  | `folder` | string | Optional grouping label |
  | `live` | boolean | A process is running |
  | `createdAt` | string | RFC 3339 |
  | `messageCount` | number | |
  | `lastMessageAt` | string | RFC 3339; absent when none |
  | `host` | object | SSH host chip; absent for this Mac |
  | `headless` | boolean | Present for headless agents |
  | `origin` | string | Set when someone other than the person started it (`chief-of-staff`) |
  | `attention` | object | `{"state", "since"}` for a live tracked session; `state` is `starting`, `running`, `idle`, `asking`, `errored`, `stalled` |

- **Event:** `session.list` (relay's door writes it).

### proxy:DELETE /api/sessions/{id}

- **Response `204`**, also for an unknown ID. **Event:** `session.delete` (`session_id`).
- **CLI equivalent:** `relay session stop`.

### proxy:POST /api/sessions/{id}/message

Sends a message and waits for the whole reply.

- **Request:** `{"text": string, "files": [{...attachment}]}`.
- **Response `200`:** `{"text": "<reply>", "stats": {"inputTokens", "outputTokens",
  "cacheReadTokens", "cacheCreationTokens", "costUsd", ...}}`.
- **Errors:** `400` malformed JSON; `404` unknown session; `409` with
  `{"error": "resume_required"|"dropped_in", "message"}`, or an empty body for
  an already-processing session; `500`.
- **Event:** `session.message` (`session_id`). **CLI equivalent:** `relay session message`.

### proxy:GET /api/terminals

Also relay's own door `GET /api/terminals`.

- **Response `200`:** `{"terminals": [{"id", "templateId", "name", "directory",
  "state", "exitCode", "host", "origin"}]}`. `state` is the terminal's process state.
- **Event:** `terminal.list`.

### proxy:GET /api/terminals/{id}/log

- **Response `200`:** `text/plain`, the terminal's replay log. Works after the
  terminal exits while its log files remain.
- **Errors:** `404` no log; `400` an unreadable log.
- **Event:** `terminal.log` (`terminal_id`). **CLI equivalent:** `relay terminal log`.

### proxy:DELETE /api/terminals/{id}

- **Response `204`.** **Errors:** `404` unknown terminal.
- **Event:** `terminal.delete` (`terminal_id`). **CLI equivalent:** `relay terminal stop`.

### proxy:GET /api/models

The merged model catalog a session picker shows: the Claude aliases, pi and
Codex models and the model broker's models. System-only models are left out.

- **Response `200`:** `{"models": [{"label", "value", "group", "provider",
  "supportsPermissions", "supportsAttachments"}], "providerSettings": {...}}`.
- **Event:** `model.list` (`count`). **CLI equivalent:** `relay model list`.

## 4. WebSocket frames

Two sockets carry frames. Frames are JSON text messages with a `type` field.

## `/ws` (relay-sessions, through the `/` mount)

Connect with `GET /ws`, `Authorization: Bearer <token>`, class `proxy`. The
protocol is in [`session-host.md`](session-host.md). A test reads these frames:

### ws:/ws join_session

Client to server. Binds the connection to a session and replays it.

- **Frame:** `{"type": "join_session", "sessionId": "<id>"}`.
- **Answer:** `session_joined` (with the session's history and `live`), or
  `{"type": "error", "message": "session not found: <id>"}`.

### ws:/ws session_state

Server to client. Broadcast to every connection, joined or not, on each change
of a tracked agent session's state.

- **Frame:** `{"type": "session_state", "sessionId": "<id>", "state": "<state>",
  "since": "<RFC 3339 with milliseconds>"}`. `state` is `starting`, `running`,
  `idle`, `asking`, `errored`, `stalled` or `ended`.
- **Event:** `session.state` (background, written by relay-sessions).

### ws:/ws turn_done

Server to client. Broadcast once when a turn ends, before that transition's
`session_state`.

- **Frame:** `{"type": "turn_done", "sessionId", "excerpt": "<last 500 runes of the reply>", "at": "<RFC 3339>"}`.
- **Event:** `chat.turn` (`/ws` turn, relay-sessions).

### ws:/ws set_permission_mode

Client to server. Changes a Claude session's permission mode.

- **Frame:** `{"type": "set_permission_mode", "sessionId": "<id>", "mode": "<mode>"}`.
  `mode` is one of `default`, `acceptEdits`, `plan`, `bypassPermissions`.
- **Answer:** `mode_changed`, or `error`; a session that needs a restart answers
  `resume_required`.
- **CLI equivalent:** `relay session mode`.

### ws:/ws mode_changed

Server to client, to the session's joined connections.

- **Frame:** `{"type": "mode_changed", "sessionId": "<id>", "mode": "<mode>"}`.

### ws:/ws error

Server to client, to the connection that sent the failing frame.

- **Frame:** `{"type": "error", "message": "<text>"}`, with `"code"` and
  `"sessionId"` on coded errors (`provider_error`, `resume_required`).
- **Close event:** `session.ws.close`, at connection end.

## `/ws/files` (relay's own door `GET /ws/files`)

Class `execute`, socket only. One connection serves watches for any number of
projects; one filesystem watcher serves a project for all connections. Inbound
frames need a non-empty `project_id`; anything else is ignored.
Design: [`project-files.md`](project-files.md).

### ws:/ws/files watch

Client to server: `{"type": "watch", "project_id": "<id>"}`. Answered by
`watch_ok` or `watch_error`. `unwatch` (same shape) stops it.

### ws:/ws/files host_status

Server to client. Sent for every known SSH host on connect, then on each change.

- **Frame:** `{"type": "host_status", "host_id", "name", "status", "error"}`.
  `status` is `connecting`, `connected` or `unreachable`; `error` is set when unreachable.

### ws:/ws/files watch_ok

Server to client, after the watch is started and before any `fs_event` for it.

- **Frame:** `{"type": "watch_ok", "project_id": "<id>"}`.

### ws:/ws/files watch_error

Server to client.

- **Frame:** `{"type": "watch_error", "project_id", "code", "error"}`. `code` is
  `ENOENT`, `UNSUPPORTED`, `HOST_UNREACHABLE`, `PROJECT_NOT_FOUND`,
  `NOT_AVAILABLE`, `ERROR`, or `PROJECT_CHANGED` when the project was deleted or
  its path or host changed under a live watch. Relay compares watched projects
  with settings every two seconds.

### ws:/ws/files fs_event

Server to client, one per change under a watched project.

- **Frame:** `{"type": "fs_event", "project_id", "path": "<relative path>", "kind": "change"|"rename"}`.

## 5. Login routes (TCP only)

Three routes let a browser register and use a passkey. Their class is `public`:
no credential. They exist only on the TCP listener, and only when it is bound,
because a WebAuthn ceremony is verified against an origin and a socket has
none. The relying party is `localhost`. A bodied request must send
`Content-Type: application/json`, is at most 64 KiB, and is strict (unknown
fields are refused). Binary values are unpadded base64url strings.

### GET /relay/login

- **Listeners:** tcp. **Credential:** `public`. **Gate:** none.
- **Response `200`:** the login page, `text/html`, with a nonce-based
  Content-Security-Policy and `Cache-Control: no-store`.
- **Event:** `login.page`.

### POST /relay/login/challenge

- **Listeners:** tcp. **Credential:** `public`. **Gate:** none.
- **Request:** `{"ceremony": "register"|"assert"}`.
- **Response `200`:**

  | Field | Type | Meaning |
  |---|---|---|
  | `challenge` | string | base64url challenge |
  | `rp_id` | string | `localhost` |
  | `origin` | string | The listener's origin |
  | `user_handle` | string | Register only |
  | `user_name` | string | Register only |
  | `credentials` | string[] | Registered passkey IDs for `assert`; `[]` for `register` |

- **Errors:** `400` bad body or ceremony; `429` too many outstanding challenges
  or the rate limit, with a `Retry-After` header.
- **Event:** `login.challenge`. A `429` is `denied` with reason `throttled`.

### POST /relay/login/verify

- **Listeners:** tcp. **Credential:** `public`. **Gate:** `webauthn`: the
  ceremony is verified against the registered passkey, not a presence prompt.
- **Request:**

  | Field | Type | Meaning |
  |---|---|---|
  | `ceremony` | string | `"register"` or `"assert"` (required) |
  | `client_data_json` | string | Required |
  | `code` | string | Register: the one-time code from `relay login enrol` |
  | `attestation_object` | string | Register |
  | `credential_id`, `authenticator_data`, `signature` | string | Assert |
  | `user_handle` | string | Assert, optional |

- **Response `201` (register):** `{"credential_id": "<id>", "name": "<label>"}`.
- **Response `200` (assert):** `{"token": "<bearer>", "expires": "<RFC 3339>",
  "classes": ["read", "configure"]}`. The token is a control-plane credential
  that lasts 12 hours and never holds `grant`, `execute` or `proxy`.
- **Errors:** `400` malformed or unknown ceremony; `403` any failed
  verification (a wrong code, a bad signature, a counter that did not
  increase); `429` rate limited, with `Retry-After`; `500` the credential could
  not be saved or recorded.
- **Event:** `login.passkey.register` (`passkey_id`) or `login.sign_in`
  (`passkey_id`, `credential_id`); a body that is neither writes `login.sign_in`
  with reason `invalid`. A `403` is `denied` with reason `unauthorized`.
- **Audit row:** `credential_issued` (credential `passkey` on register,
  `api_credential` on assert) and a `control_decision` row with `path`
  `/relay/login/verify`.
- **CLI equivalent:** `relay login enrol` mints the register code.

## 6. Model endpoint

The model endpoint serves OpenAI-style and Anthropic-style model calls to
sessions and tools. It is not part of the control-plane API and shares none of
its credentials. Design and auth order: [`model-endpoint.md`](model-endpoint.md).

| Listener | Address |
|---|---|
| `model` socket | `sockets.model` in `ready.json`; always served |
| `model` TCP | `listeners.model` in `ready.json`; only when `model_endpoint.listen` is set |

**Credential.** One of, checked in this order:

1. `X-Relay-Key: <key>`: a model key `rmk_` plus 64 hex characters, minted at
   session launch, scoped to one project. When present it is the only
   credential considered.
2. `Authorization: Bearer <token>` or `x-api-key: <token>` (not both, unless
   equal): a model key or a project token.
3. On the socket with no header: the connecting process's launch identity,
   when it holds the `models` capability.

On TCP, no header is always `401`. A remote-kind project is `403`. The
project's `allowed_models` is read live; a model outside it is `404`,
byte-identical to a model that does not exist.

**Error bodies.** OpenAI-shaped routes answer `{"error": {"message", "type",
"code"}}`; Anthropic-shaped routes (`/v1/messages`) answer `{"type": "error",
"error": {"type", "message"}}`.

| Status | `message` | When |
|---|---|---|
| `401` | `unauthorized` | No or unknown credential. Identical for every cause. |
| `403` | `permission denied` | Remote project |
| `404` | `model not found` / `route not found` | Not granted or unknown model; a path off the allowlist |
| `413` | `request body too large` | Body over the limit |
| `400` | `request does not name a model` / `request body is not a single JSON value` | |
| `429` | `too many concurrent requests` | Relay is already holding its body budget |
| `503` | `model host unavailable` | No registered model host, or unreachable |

**Event and audit.** Every call writes `model.request` (`method`, `path`,
`http_status`, `transport`, `caller_kind`, `caller`, `session_id`, `model`;
absent fields are omitted). A successful poll of `GET /health`, `GET /props`,
`GET /models` or `GET /v1/models` writes no event. The audit row is
`model_call`, or `model_list` for a listing, and a listing is recorded only
when `audit.log_lists` is on. `X-Trace-Id` is honored. There is no CLI
equivalent.

### model:GET /v1/models

- **Response `200`:** `{"object": "list", "data": [{"id", "object": "model",
  "owned_by", "system", "context_length"}]}`: the models the credential's grant
  allows. `system` and `context_length` are present only when set. Alias
  targets are never shown.
- **Errors:** `401`; `503`.

### model:GET /models

As `model:GET /v1/models`, and every row also carries `"status": {"value": "loaded"}`.

### model:POST /v1/chat/completions

- **Request:** an OpenAI chat-completions body. `model` is required and must be
  granted; the body is forwarded with the model name normalized.
- **Response:** the provider's answer, streamed when the request asks for it.
- **Errors:** `400`, `401`, `403`, `404`, `413`, `429`, `503`, as above.

The routes below share this contract: a JSON body naming `model`, the same
auth, grant check and errors. The bare forms (no `/v1`) are accepted for
clients that post to a bare base URL.

### model:POST /chat/completions

Bare form of `model:POST /v1/chat/completions`.

### model:POST /v1/completions

As `model:POST /v1/chat/completions`.

### model:POST /completions

Bare form of `model:POST /v1/completions`.

### model:POST /v1/embeddings

As `model:POST /v1/chat/completions`.

### model:POST /embeddings

Bare form of `model:POST /v1/embeddings`.

### model:POST /v1/responses

As `model:POST /v1/chat/completions`.

### model:POST /responses

Bare form of `model:POST /v1/responses`.

### model:POST /v1/audio/speech

As `model:POST /v1/chat/completions`.

### model:POST /v1/audio/transcriptions

As above, but the body is `multipart/form-data` and the model is its `model`
form field.

### model:POST /v1/messages

An Anthropic Messages body with `model`. Relay chooses a branch by the model:
a model relay manages is served locally under the rules above. A Claude model
relay does not manage is **passed through**: relay needs no credential of its
own, forwards the client's credential and bytes untouched, and strips relay's.
A model that neither branch claims is `401` for a caller with no relay
credential and `404` otherwise. A catalog that cannot be read is `503`.

### model:POST /v1/messages/count_tokens

As `model:POST /v1/messages`.

### model:GET /health

- **Response `200`:** proxied from the model host. **Errors:** `401`, `503`.

### model:GET /props

- **Response `200`:** `{"models_autoload": false}`, answered by relay itself.

### model:/api/{path}

A fixed passthrough prefix: any method, with at least one path segment after
`/api/`. Forwarded to the matching provider with the client's own credential;
relay's credential is stripped and none is required. Nothing is brokered,
granted or filtered. A path with a `.` or `..` element is not a passthrough and
is handled as an unknown route (`404`).

### model:/chatgpt/{path}

Passthrough prefix `/chatgpt/`; same contract as `model:/api/{path}`.

### model:/openai/{path}

Passthrough prefix `/openai/`; same contract as `model:/api/{path}`.

## 7. Enrolment listener

The enrolment-request listener lets a remote machine ask the operator for a
client certificate. It is plain TCP carrying newline-delimited JSON frames,
with no certificate and no credential. It exists only when `remote.enrolment_requests`
is `true` (and the remote listener is enabled and auditing is on); its address
is `listeners.enrolment` in `ready.json`.

Limits: 16 concurrent connections; 64 frames per connection, then the server
closes it; 64 KiB per frame; 10 seconds to the first frame and 30 seconds idle;
8 pending requests at most; a request lives 15 minutes to be approved and 15
more to be collected. Each frame is one JSON object with `type`; decoding is
strict. The server answers each frame with one JSON line:
`{"type": "Result", "result": {...}}` or `{"type": "Error", "code": <number>,
"message": "<text>"}`.

Codes: `-32602` invalid params, `-32601` unknown request type, `-32603` internal,
`-32000` throttled (the `result` then holds `{"retry_after_seconds": <n>}`).
An operator approves or refuses a request with `relay enrol approve` or
`relay enrol refuse`; there is no approve door on this listener.

### enrol:lodge

Wire type `EnrolmentRequest`. Lodges a request.

- **Request:**

  | Field | Type | Meaning |
  |---|---|---|
  | `type` | string | `"EnrolmentRequest"` |
  | `csr_pem` | string | A certificate signing request (required) |
  | `label` | string | Untrusted display label |
  | `requested_profile` | string | A hint, 1 to 64 of `A-Za-z0-9._-` |
  | `sas_commit` | string | 64 lowercase hex: commitment to a comparison nonce |

- **Result:** `request_id`, `spki_sha256`, `poll_after_seconds`,
  `expires_in_seconds`, and when the CA exists `ca_pem`; with `sas_commit` also
  `sas_nonce`.
- **Errors:** `-32602` bad request or invalid CSR; `-32000` throttled (per
  source, per request rate or a full table); `-32603` no CA yet.
- **Event:** `enrolment.request.lodge` (`request_id`). A throttle is `denied`,
  reason `throttled`.

### enrol:poll

Wire type `EnrolmentRequestPoll`. Asks about a lodged request.

- **Request:** `{"type": "EnrolmentRequestPoll", "request_id": string (required),
  "sas_open": string}`. `sas_open` is 32 lowercase hex, sent on the first poll.
- **Result:** `status` is `pending`, `approved`, `refused` or `unknown`.
  `pending` adds `poll_after_seconds` and `expires_in_seconds`. `approved` adds
  `client_id`, `project_ids`, `relay_addr`, `cert_pem`, `ca_pem` and `projects`
  (`[{"id", "name"}]`).
- **Errors:** `-32602` a malformed poll or a refused comparison code.
- **Event:** `enrolment.request.poll`, on failure only.

## 8. Remote listener (mTLS)

The tool-plane listener for enrolled remote clients. Mutual TLS against relay's
own CA; the client certificate is resolved to an enrolment before any frame is
read, and an unenrolled certificate is closed without a reply. Frames are
newline-delimited JSON, strictly decoded (`type`, `name`, `arguments`,
`project_id`, `args_sha256`; any other key is refused). There is no token and no
working directory on the wire. The address is `listeners.remote` in `ready.json`
and the listener refuses to run while auditing is off. `project_id` is optional when the enrolment holds exactly one profile; with
several it is required (`-32602`), and a profile the enrolment does not hold is
`-32001`. Each request gets a new trace ID and writes `remote.request` (`request_type`;
never arguments). Budget refusals are audited `throttled`; a revoked client's
live connections close. Design: [`access-profiles.md`](access-profiles.md).

An answer is `{"type": "Tools"|"Result"|"Error", ...}`. An error carries `code`
and `message`: `-32001` unauthorized, `-32700` parse error, `-32601` request
type not available (including a config request without the `cli_admin`
permission), `-32602` invalid params, `-32603` internal.

### remote:ListTools

- **Request:** `{"type": "ListTools", "project_id": string}`.
- **Answer:** `{"type": "Tools", "tools": [{...tool}]}` limited to what the
  grant allows.
- **Event:** `tool.list` (`project_id`, `transport`, `count`).

### remote:CallTool

- **Request:** `{"type": "CallTool", "name": "<tool>", "arguments": {...},
  "project_id": string, "args_sha256": "<hex sha-256 of arguments>"}`.
  `args_sha256` is forwarded to the MCP and never recomputed by relay.
- **Answer:** `{"type": "Result", "result": <tool result>}`; a tool's own error is
  inside `result`. A tool outside the grant, a colliding tool name, a budget
  overrun or a write on a read-only grant is an `Error`.
- **Event:** `tool.call` (`project_id`, `mcp_id`, `tool`, `transport`,
  `tool_error`). **Audit rows:** `call_tool` `intent` and `completion`, with
  outcome `ok`, `denied`, `throttled` or `error`.

### remote:DescribeGrant

Only for an enrolment with `cli_admin` (`relay enrol update --client-id ID
--cli-admin`); otherwise `-32601`.

- **Request:** `{"type": "DescribeGrant", "project_id": string}`.
- **Answer:** `{"type": "Result", "result": {"id", "name", "kind", "path",
  "mcps": [...], "mounts": [...]}}`: the same view as `relay grant --json`.
- **Event:** `grant.describe` (`project_id`, `client_id`).

### remote:NarrowGrant

Only for an enrolment with `cli_admin`. It can only narrow.

- **Request:** `{"type": "NarrowGrant", "project_id": string, "arguments":
  {"allowed_mcp_ids": string[], "allowed_tools": object, "access": object,
  "allow_external": object, "mounts": object[]}}`. The arguments are strict;
  an unknown key is `-32602`; empty arguments change nothing.
- **Answer:** `{"type": "Result", "result": {"changed": string[], "grant": {...}}}`.
- **Event:** `grant.narrow` (`project_id`, `client_id`, `changed`).

## Bridge requests

The bridge socket (`sockets.bridge` in `ready.json`, mode 0600) carries the
requests a service, a client library or a test sends to relay. A test reaches
the brokered operations through the CLI and the HTTP doors instead; this section
lists only the frames a test sends on the socket itself.

**Framing.** Newline-delimited JSON: one request and one reply per line, on a
connection the client may reuse. A frame is at most 10 MiB.

| Request field | Meaning |
|---|---|
| `type` | The request type (required) |
| `name` | The launch name, MCP id, service id or operation name, by type |
| `arguments` | A JSON object, by type |
| `token` | The credential the type needs: the admin secret, a project token or a launch secret |
| `project_id` | Accepted and not used for a decision on this socket |
| `trace_id` | Optional; `[A-Za-z0-9_-]{8,64}`. Relay adopts it, or makes one |
| `kind` | `Hello` only: the identity kind the caller expects (`service` or `project_session`) |

A reply has `type`: `OK`, `Result`, `Tools` or `Error`. `OK` may carry `data`,
`Result` carries `result`, `Tools` carries `tools`, and `Error` carries `code`
and `message`. Every request writes one `bridge.request` event (`request_type`,
`status`, `error`): `denied` for `-32001`, `error` for the other codes.

| Code | Meaning |
|---|---|
| `-32001` | Unauthorized: a wrong, missing or unbound credential |
| `-32700` | The line is not valid JSON |
| `-32601` | Unknown request type |
| `-32602` | Invalid params |
| `-32603` | Internal error |

The error reply is `{"type":"Error","code":-32001,"message":"<text>"}`. A test
asserts the `code`, never the `message`.

### bridge:Hello

`{"type":"Hello","name":"<service id>","token":"<launch secret>"}`, with an
optional `kind`. Binds a live launch to the connection's peer. A test cannot
make one: the secret arrives on the launched process's fd 3. Reply: `OK` with
`data` `{"kind","service_id","relay_pid"}`, or `-32001` with one fixed message
for every refusal. See [`launch-identity.md`](launch-identity.md).

### bridge:RegisterManifest

`{"type":"RegisterManifest","arguments":{"serviceId","manifest","internalSocket","internalToken"}}`.
No `token`: the caller's launch identity must hold the capability. Reply: `OK`;
`-32602` for a missing or invalid field; `-32001` for a caller with no identity.
See [`service-manifest.md`](service-manifest.md).

### bridge:RegisterModelHost

`{"type":"RegisterModelHost","arguments":{"service_id","router_socket"}}`.
`router_socket` is an absolute path. No `token`: the caller's launch identity
must hold `model_host`, and register under its own service id. Reply: `OK`;
`-32602` for a missing field or a relative path; `-32001` otherwise. See
[`model-endpoint.md`](model-endpoint.md).

### bridge:admin frames

`ReloadExternalMcp` (`name` is the MCP id), `ReconcileExternalMcps` (no
`name`) and `ReloadService` (`name` is the service id). Each carries the admin
secret in `token`. A wrong or missing secret answers `-32001`, and the handler
does not run. Reply: `OK`; a reload of an unknown id answers an `Error`. Events:
`mcp.reload` (`mcp_id`), `mcp.reconcile`, and the service's own events for a
service reload.

The admin secret is `settings.json` `admin_secret`. A plaintext string planted
there before start is accepted as the legacy shape, and the next save seals it
([`sealed-config.md`](sealed-config.md)). Once sealed, no process outside the
tray can read it back to present here, which is why the CLI reaches these
operations through `admin_op` instead.

### bridge:admin_op

`{"type":"admin_op","name":"<operation>","arguments":{...}}`. No `token`: the
0600 socket is the credential, and each operation applies its own gate. A test
reaches an operation through its CLI command or HTTP route; the `bridge:admin_op:`
refs in [`FEATURES.md`](FEATURES.md) name the operations. Reply: `Result`, or an
`Error`.

### bridge:ListTools and bridge:CallTool

`{"type":"ListTools","token":"<project token>"}` answers `Tools`.
`{"type":"CallTool","name":"<tool>","arguments":{...},"token":"<project token>"}`
answers `Result`. `DescribeProject` takes the same token and answers
`ProjectDescription` with `data`. Arguments are forwarded as the original bytes.

### Frames a test never sends

- `SessionExited` is relay-sessions' report that a session ended. It needs the
  launch identity of the built-in session host.
- `SandboxAttach` and `DropInAttach` turn the connection into a terminal byte
  stream. They are reached with `relay sandbox` and `relay drop-in`
  ([`sandbox-command.md`](sandbox-command.md)).
- `MountAttach` is the mount plane's preamble and is refused on this socket.

## 9. Background and not doors

### bg:service supervision

Not a door: relay starts, watches and restarts the services in `settings.json`
with no call. Its trace is `service.state` and `mcp.state` events (background,
no trace ID); read them with `relay logs --follow --event service.state
--timeout 30s --json`. Listed here so the `bg` ref has one name.

**Not doors.** The session host's own API (`/launch`, `/send`, `/permission`, `/handoff`,
`/handback`, `/terminate`) is on a Unix socket between relay and relay-sessions.
These are not doors: each is relay-internal, reached with an internal bearer
only, and a test never drives one.

| Path | What it is for |
|---|---|
| `/launch` | Relay asks the host to start a session or terminal |
| `/send` | Relay delivers a message (Chief of Staff) |
| `/permission` | A Claude tool-approval hook asks for a decision |
| `/handoff` | Relay takes a headless session for a drop-in |
| `/handback` | Relay returns a session after a failed drop-in |
| `/terminate` | Relay ends a session (for example when its project is removed) |
