# fakerelay

fakerelay is a pure Go stand-in for relay. It serves the frontend socket
routes that a client such as eve uses, from an in-memory world, on any OS.
It is a test tool. It is not a relay feature, and no person reaches it in
everyday use.

This document is the only spec for fakerelay. The people who build it read
this file, relay's other `docs/` files and eve's `docs/api.md`. They do not
read relay's code. Where relay's docs leave a wire shape out, this file
carries it and names the doc gap (G1 to G7). Those entries say "checked
against relay at `59cdf9e`": the shape was read from relay's code once, for
this file only.

Sections:

| Section | State |
|---|---|
| Install and run | written |
| Surface | written |
| Wire reference | written |
| World spec | written |
| Instance and ready file | written |
| CLI | written |
| Control socket | written |
| Faults | written |
| Presence | written |
| Launch | written |
| Host simulation | written |
| Fake semantics | written |
| Not faked | written |
| Seam for the e2e fakes | written |

## Install and run

A client pins one commit and installs the binary. It needs Go and no cgo.

```sh
go install github.com/barelyworkingcode/relay/fakerelay/cmd/fakerelay@<sha>
```

Then, per test instance:

1. Make a fresh `DIR` and write `DIR/world.json`.
2. Run `fakerelay --config-dir DIR serve`. The only stdout line is the path
   of `ready.json`. Wait for that line.
3. Read `ready.json`. Use `listeners.api` for bearer clients and
   `sockets.frontend` for socket clients.
4. Drive the instance. The CLI verbs take the same `--config-dir`, and
   `fakerelay --config-dir DIR ctl ...` sets presence, faults, host status
   and the clock.
5. Send SIGTERM. fakerelay removes `ready.json` and its sockets and exits 0.

`DIR` must be short. A socket path over 103 bytes is refused.

## Surface

### Doors

| Door | Carries | Auth |
|---|---|---|
| Bridge socket (`relay.sock`) | newline-delimited JSON, one request and one response per line | `Hello` binds a launch secret to the peer |
| Frontend socket (`relay-frontend-<pid>.sock`) | HTTP and WebSocket | no `Authorization`: the peer's launch identity. With `Authorization: Bearer`: a credential |
| TCP listener (`listeners.api`) | HTTP and WebSocket | bearer only |
| Control socket (`fakerelay-control.sock`) | fake-only HTTP under `/v1/` | file mode 0600 |

Rules for the HTTP doors:

- A route of class `execute`, `proxy` or `chief_of_staff` is absent from the
  TCP mux. A request for it on TCP is not refused by a handler. It is not
  routed.
- A frontend socket request with no `Authorization` header is resolved from
  the peer pid. The peer must hold a live launch identity that grants
  `frontend`. Otherwise the answer is 401.
- A frontend socket request with a header that is not a known bearer is 401.
  There is no distinction between absent, malformed and unknown bearers.
- A launch identity with `frontend` holds the classes `read`, `configure`,
  `proxy` and `execute`. It never holds `grant`.
- A bearer holds exactly the classes on its credential record.
- `X-Relay-Scope: chief-of-staff` narrows one request. See "Scope" below.

### Route classes

Class is the capability a caller needs. The bodies for each route are in the
Wire reference.

| Group | Route | Class |
|---|---|---|
| Projects | `GET /api/projects`, `GET /api/projects/{id}` | read |
| Projects | `POST /api/projects`, `PUT /api/projects/{id}`, `DELETE /api/projects/{id}` | configure |
| Projects | `PUT /api/default_project/{mode}` | configure |
| Chief of Staff | `GET /api/chief-of-staff/config` | read |
| Chief of Staff | `PUT` and `DELETE /api/chief-of-staff/config` | configure |
| Chief of Staff | `POST /api/chief-of-staff/messages`, `POST /api/chief-of-staff/sessions` | chief_of_staff |
| Hosts | `GET /api/hosts`, `GET /api/hosts/{id}`, `GET /api/hosts/{id}/templates` | read |
| Hosts | `POST /api/hosts`, `PUT` and `DELETE /api/hosts/{id}`, `POST /api/hosts/{id}/probe`, `POST /api/hosts/{id}/disconnect` | configure |
| Hosts | `POST /api/hosts/{id}/templates`, `PUT` and `DELETE /api/hosts/{id}/templates/{tid}` | configure |
| Persistent terminals | `GET /api/projects/{id}/persistent-sessions` | read |
| Persistent terminals | `DELETE /api/projects/{id}/persistent-sessions/{name}` | configure |
| Files | every `/api/projects/{id}/files/*` route, `POST /api/hosts/{id}/pastetmp`, `GET /ws/files` | execute |
| Sessions | `POST /api/sessions`, `POST /api/sessions/{id}/resume`, `POST /api/sessions/{id}/drop-in` | execute |
| Sessions | `GET /api/sessions` | proxy |
| Terminals | `POST /api/terminals` | execute |
| Terminals | `GET /api/terminals`, `GET /api/terminals/{id}/log` | proxy |
| Templates | `GET /api/terminal/templates`, `GET /api/terminal/templates/{id}` | read |
| Templates | `POST /api/terminal/templates`, `PUT` and `DELETE /api/terminal/templates/{id}` | configure |
| Dispatch | `GET /ws`, `GET /api/models`, `DELETE /api/sessions/{id}`, `DELETE /api/terminals/{id}`, and every other path a manifest claims | proxy |
| MCPs | `GET /api/mcps`, `GET /api/mcps/{id}/tools` | read |
| Eve | `GET /api/eve/passkey-enrolment`, `GET /api/eve/passkeys/revocations` | read |
| Eve | `POST /api/eve/passkey-enrolment/consume`, `PUT /api/eve/passkeys` | configure |
| Audit | `GET /api/audit`, `GET /api/audit/log` | read |

`DELETE /api/sessions/{id}`, `DELETE /api/terminals/{id}`, `GET /api/models`
and `/ws` are not relay routes. In relay they reach the session host through
manifest dispatch. fakerelay serves them itself, and they keep class `proxy`.
Any other path under `/api/sessions/` or `/api/terminals/` is class `proxy` and
answers the session host's 404 (see Sessions).

### Scope

`X-Relay-Scope: chief-of-staff` narrows one request. It is not a credential.

| Request | Result |
|---|---|
| No header | class `chief_of_staff` is refused |
| Header, caller lacks `proxy` | 403 |
| Header, caller holds `proxy` | only `GET /api/sessions`, `GET /ws`, `POST /api/chief-of-staff/messages` and `POST /api/chief-of-staff/sessions` run |
| Header, any other route | 403 |
| Header with an empty, repeated or other value | 403 |

A scoped `/ws` is listen-only. It receives hub broadcasts only:
`session_state`, `turn_done`, `session_ended` and `terminal_closed`. The first
data frame the client sends closes the connection with close code 1008 and
reason `chief-of-staff scope is read-only`.

### Fake-only names

Everything that does not exist in relay is namespaced: the control socket and
`/v1/` routes, `ctl` verbs, `fakerelay.*` event keys, `sockets.control` in the
ready file, and `DIR/world.json`. A scenario that avoids those names runs
against real relay.

## Wire reference

### Conventions

All HTTP bodies are UTF-8 JSON unless a row says otherwise. JSON responses
carry `Content-Type: application/json`. A 204 has no body.

Four error families exist. Each route below says which one it uses.

| Family | Shape | Used by |
|---|---|---|
| Door refusal | plain text, see "Door refusals" | any route, before the handler |
| Simple | `{"error":"<message>"}` | projects, hosts, templates, audit, sessions, terminals, eve, presence refusal |
| Coded file error | `{"error":"<message>","code":"<CODE>"}`, plus `size` on `TOO_LARGE` | file routes |
| Coded with message | `{"error":"<code>","message":"<text>"}` | Chief of Staff routes, session host conflicts |

A client branches on the `code` or the `error` code string. It never branches
on a `message`.

A route's JSON body that fails to decode answers 400
`{"error":"invalid JSON: <decoder text>"}`, except the file routes, which use
the coded file error (`INVALID`, `invalid JSON body`).

IDs: project ids and host ids come from the world file. A project created
over HTTP gets a generated id. A session or terminal id is a lowercase UUID v4.

### Door refusals

These are written by the door, before any handler runs.

| Case | Status | Body (text/plain, trailing newline) |
|---|---|---|
| No header, peer holds no `frontend` identity | 401 | `unauthorized` |
| Header present but not a known bearer, or no credentials exist | 401 | `unauthorized` |
| Class check fails on a route | 403 | `Forbidden` |
| No credential on a route (class check) | 401 | `Unauthorized` |
| Path matches no route and no manifest prefix | 404 | `no service registered for this path` |
| Upstream of a dispatched route is unreachable | 502 | `bad gateway` |

The reason behind a 403 (`class not granted`, `outside chief-of-staff scope`,
`unknown scope`) is not in the body. It is in the `error` field of the
`control_decision` audit row.

Every request to a registered route writes a `control_decision` audit row
before the handler runs, allowed or refused. Row shape:

```json
{"id":"…","ts":"2026-10-09T10:00:00.000Z","event":"control_decision",
 "actor":{"kind":"control","auth":"token","cred_id":"c1"},
 "method":"POST","path":"/api/projects","class":"configure","transport":"socket",
 "outcome":"ok"}
```

A refusal has `"outcome":"denied"` and `"error":"<reason>"`. fakerelay writes
these rows only for presence refusals and the world's seeded rows. See the
fake semantics section.

### Presence refusal (G6)

A gated operation that the presence gate refuses answers 403 with the Simple
family:

```json
{"error":"presence was refused"}
```

The `error` text is the refusal reason, verbatim:

| Cause | `error` |
|---|---|
| The person cancels, or the gate refuses | `presence was refused` |
| No session can show a prompt | `no session can display a presence prompt` |
| Presence checking is unavailable | `presence checking is unavailable` |
| The operation has no gate wired | `presence gate is not wired for this operation` |

When the caller leaves before an answer, there is no response. A refusal also
writes one `control_decision` row with `outcome: "denied"` and `via`, and no
`path`, `class` or `transport`:

```json
{"id":"…","ts":"…","dur_ms":4210,"event":"control_decision",
 "actor":{"kind":"control","auth":"token","cred_id":"c1"},
 "outcome":"denied","error":"presence was refused","scope":null,
 "method":"project.grant","subject":"Acme","via":"http",
 "presence_approver":"testapprover"}
```

`id` is a UUID. `presence_approver` is there because relay's test build writes
it on every answer of its test approver, and fakerelay stands in for that
build.

`via` is `http`, `cli`, `ipc` or `tray`. The HTTP routes that run a gated
operation also use 409 `{"error":"project changed during approval"}` when
the record moved while the prompt was up, and 500 when settings cannot be
saved. checked against relay at `59cdf9e`.

### Bridge socket

Framing: each request and each response is one JSON object on one line,
ending in `\n`. A line may be up to 10 MiB. The connection stays open for many
requests. Unknown JSON keys are ignored.

Request:

```json
{"type":"Hello","name":"acme-svc","token":"<64 lowercase hex>","kind":"service","trace_id":"…"}
```

| Key | Meaning |
|---|---|
| `type` | request type, required |
| `name` | `Hello`: the launch name (the service id) |
| `token` | `Hello`: the launch secret. Other types: a credential, which `Hello` and `RegisterManifest` must leave empty |
| `kind` | `Hello` only, optional. If present and different from the launch's kind, the Hello is refused before the secret is spent |
| `arguments` | JSON payload for types that take one |
| `trace_id` | optional trace id, `[A-Za-z0-9_-]{8,64}` |
| `cwd` | accepted and ignored |

Response:

| Key | Meaning |
|---|---|
| `type` | `OK` or `Error` |
| `data` | `Hello`: the result below |
| `code` | on `Error`: a JSON-RPC style integer |
| `message` | on `Error`: text |

Error codes:

| Code | Name | When |
|---|---|---|
| -32700 | parse error | the line is not JSON, or `arguments` does not decode |
| -32601 | method not found | unknown `type` |
| -32602 | invalid params | `RegisterManifest` payload invalid or in conflict |
| -32001 | unauthorized | `Hello` refused, or the caller lacks the capability |
| -32603 | internal error | anything else |

Unknown request type:

```json
{"type":"Error","code":-32601,"message":"unknown request type: Foo"}
```

#### Hello

Success:

```json
{"type":"OK","data":{"kind":"service","service_id":"acme-svc","relay_pid":4242}}
```

`relay_pid` is fakerelay's own pid. `project_id` appears only for a
`project_session` identity, which fakerelay does not issue.

Every refusal has one fixed answer, whatever the reason:

```json
{"type":"Error","code":-32001,"message":"hello refused"}
```

The refusal reasons are: a wrong secret, a second Hello on a spent launch, a
peer that already holds an identity, no live launch for `name`, an
unreadable peer, or a `kind` mismatch. The reason is logged by relay and
never sent. See the launch section for which refusals spend the launch.

#### RegisterManifest (G5)

Request:

```json
{"type":"RegisterManifest","arguments":{
  "serviceId":"acme-svc",
  "manifest":{"routes":["/api/probe/report","/ws/probe"]},
  "internalSocket":"/tmp/fr.Ab12/a/acme-svc.sock",
  "internalToken":"svc-internal-bearer"}}
```

`token` stays empty. The call is authenticated by the peer's launch identity.

| Field | Rule |
|---|---|
| `serviceId` | required, and equal to the identity's own name |
| `internalSocket` | required. A Unix socket path the service listens on |
| `internalToken` | required. The bearer fakerelay presents to the service |
| `manifest.routes` | required, non-empty. Each starts with `/`, none duplicated |
| `manifest.status` | optional `{"path":"/api/status"}`, path starts with `/` |
| `manifest.actions` | optional list of `{id,label,method,pathTemplate,forEach?}` |
| `manifest.config` | optional `{path,format?,label?,help?,applyMode?,schema[]}` |

A route that ends in `/` is a prefix. Any other route matches exactly. Match
is longest route wins. The caller needs the `manifest` capability.

Success is `{"type":"OK"}`. A second registration for the same id replaces
the first.

Refusals, all with `type: "Error"`:

| Code | `message` | Cause |
|---|---|---|
| -32001 | `RegisterManifest requires a launch identity holding that capability` | no identity, or no `manifest` capability |
| -32001 | `RegisterManifest requires the calling service's launch identity, not a token` | `token` was sent |
| -32001 | `RegisterManifest: service "<id>" may not register a manifest for "<other>"` | `serviceId` is not the caller's own |
| -32602 | `register_manifest: missing arguments` | no `arguments` |
| -32602 | `register_manifest: serviceId is empty` (or `internalSocket`, `internalToken`) | empty field |
| -32602 | `manifest: routes is empty` | no routes |
| -32602 | `manifest: routes[<i>] "<route>" must start with "/"` | bad route |
| -32602 | `manifest: routes[<i>] "<route>" is duplicated` | duplicate route |
| -32602 | `manifest: routes[<i>] "<route>" is reserved to relay's internal session-host API` | route is, or is under, `/launch`, `/terminate`, `/handoff`, `/handback` or `/send` |
| -32602 | `manifest registry: route "<route>" is reserved to relay (/relay/)` | route is `/relay` or under `/relay/` |
| -32602 | `manifest registry: route "<route>" collides with "<path>", which relay serves` | overlap with a route fakerelay serves |
| -32602 | `manifest registry: route "<route>" already claimed by service "<other>"` | another service has the same route string |

Overlap rule: a prefix route that contains a served path collides, and a
route inside a served prefix collides. A served route with a wildcard
reserves its whole subtree, cut at the first `{`. The `/` catch-all is not
reserved. checked against relay at `59cdf9e` (G5).

Registering writes the event `service.manifest.register`. The routes are
dropped when the service's process exits.

### Dispatch

The path is matched against registered manifests after every fakerelay route.
No match answers the 404 door refusal.

For a match, fakerelay forwards the request to `internalSocket`:

- It removes the inbound `Authorization`.
- It sets `Authorization: Bearer <internalToken>`.
- It forwards the trace header `X-Trace-Id` when the request has one.
- It passes the method, path, query, other headers and body unchanged.
- It streams the response back. WebSocket upgrades are bridged frame by frame
  to a WebSocket dialed on the same socket with the same bearer.

If the dial fails, an HTTP request answers 502 `bad gateway` (text). A
WebSocket client gets close code 1011 with reason `upstream unreachable`.

### Projects

Project JSON (the `projectView`). Keys use relay's `settings.json` names.

```json
{"id":"p_acme","name":"Acme","path":"/home/acme/app","kind":"local",
 "host_id":"h_box","mode":"work","default_for":["work"],
 "allowed_mcp_ids":["fsmcp"],"allowed_models":["*"],
 "chat_templates":[],"allowed_templates":["shell"],
 "created_at":"2026-10-09T10:00:00Z",
 "permission_policy":{"default_mode":"default","allowed_tools":[],"denied_tools":[]},
 "files_read_only":true}
```

| Key | Rule |
|---|---|
| `id`, `name`, `path`, `mode`, `allowed_mcp_ids`, `allowed_models`, `allowed_templates`, `created_at` | always present |
| `mode` | `home`, `work` or `both`. A stored empty mode reads as `both` |
| `kind`, `host_id`, `default_for`, `chat_templates`, `disabled_tools`, `context`, `allowed_tools`, `access`, `allow_external`, `permission_policy`, `generate_skill`, `session_folders`, `mounts`, `files_read_only` | omitted when empty or false |
| `kind` | omitted for a local project. `remote` marks an access profile |

The plaintext token and its hash are never present.

| Route | Request | Success | Errors |
|---|---|---|---|
| `GET /api/projects` | none | 200, array of project JSON, `[]` when none | |
| `GET /api/projects/{id}` | none | 200, project JSON | 404 `{"error":"project not found"}` |
| `POST /api/projects` | create fields | 201, project JSON | 400 invalid, 403 presence |
| `PUT /api/projects/{id}` | patch fields | 200, project JSON | 400, 403 presence, 404 |
| `DELETE /api/projects/{id}` | none | 204 | 404 `{"error":"project not found"}` |
| `PUT /api/default_project/{mode}` | `{"project_id":"p_acme"}` | 200 `{"home":"","work":"p_acme"}` | 400 |

Create fields: `name`, `path`, `kind`, `host_id`, `mode`, `allowed_mcp_ids`,
`allowed_models`, `chat_templates`, `allowed_templates`, `permission_policy`,
`generate_skill`, `disabled_tools`, `session_folders`, `allowed_tools`,
`access`, `context`, `allow_external`, `mounts`. There is no `id` field.

Patch fields: the same names, all optional. A missing key means no change.
`files_read_only` is a patch field and not a create field. A set key replaces
the stored value whole.

Validation errors are 400 `{"error":"<text>"}`. Texts include:

- `project name is required`
- `project path is required`
- `project path must be an absolute path: "<path>"`
- `project path must not contain '..': "<path>"`
- `project cannot set both host_id and kind: "remote": …`
- `invalid default_mode: <mode>` (permission policy)
- `invalid JSON: <decoder text>`

A host project may use a path that is absolute on the host, including a
drive-letter path.

`PUT /api/default_project/{mode}`: `mode` is `home` or `work`. The key
`project_id` is required and `""` clears the default. A missing key answers
400 `{"error":"project_id is required; send \"\" to clear the default"}`. An
unknown key answers 400 `invalid JSON: …`. An unknown project answers 400
`{"error":"invalid default project: no project with id \"<id>\""}`. The
response always has both keys.

Presence applies to every `POST` and to a `PUT` that widens a grant. A `PUT`
widens when it adds an id to `allowed_mcp_ids`, widens `allowed_tools`,
`access`, `context`, `allow_external` or `mounts`, or changes `kind`,
`host_id` or `path`. Narrowing, a rename and a `files_read_only` change never
prompt. The gated operation name is `project.grant`. A `POST` with an empty
name answers 400 `{"error":"project name is required"}` before the gate. A
`POST` or widening `PUT` while auditing is off answers 403, because the gate
needs an audit trail.

Events: `project.list`, `project.get`, `project.create` (with `project_id` and `kind`; `kind` is the body's, so `""` for a local project, and both are `""` on a refusal), `project.update`,
`project.remove`.

### Chief of Staff

The HTTP body keys are camelCase, unlike the world file's snake_case.

`GET /api/chief-of-staff/config`, `PUT` and `DELETE` all answer this view:

```json
{"configured":true,"projectId":"p_acme","model":"sonnet","dailyModelCalls":200}
```

An unconfigured view is `{"configured":false}`. Other keys are omitted.

`PUT` body: `{"projectId","model","dailyModelCalls"}`, all three required, no
other keys, at most 4 KiB. `dailyModelCalls` must be a whole number from 1 to
10000. `model` is `haiku`, `sonnet` or `opus`. `DELETE` answers 200
`{"configured":false}`.

Errors use the "coded with message" family:

| Status | `error` | `message` |
|---|---|---|
| 400 | `invalid_body` | `body must be JSON {"projectId","model","dailyModelCalls"}` |
| 400 | `invalid_body` | `projectId, model and dailyModelCalls are all required` |
| 400 | `project_id_required` | `project_id is required` |
| 400 | `model_invalid` | `model must be haiku, sonnet or opus` |
| 400 | `daily_model_calls_invalid` | `dailyModelCalls must be a whole number from 1 to 10000` |
| 400 | `project_not_found` | `no project with id "<id>"` |
| 400 | `project_unsuitable` | `<project name>: <reason>` |
| 413 | `body_too_large` | `request body is larger than 4 KiB` |

`POST /api/chief-of-staff/messages`

- Request: `{"sessionId":"…","text":"…"}`, at most 64 KiB. The origin is never
  read from the body.
- Success: 202 `{"sessionId":"…","origin":"chief-of-staff","at":"2026-10-09T10:00:00.000Z"}`.
- Errors, coded with message:

| Status | `error` | `message` |
|---|---|---|
| 400 | `invalid_body` | `body must be JSON {"sessionId","text"}` |
| 400 | `session_id_required` | `sessionId is required` |
| 400 | `text_required` | `text is required` |
| 404 | `session_not_found` | `session not found` |
| 409 | `already_processing` | `the session is already processing a message` |
| 409 | `resume_required` | `the session is not running; resume it first` |
| 409 | `dropped_in` | `a terminal holds this session; close it first` |
| 413 | `body_too_large` | `request body is larger than 64 KiB` |
| 502 | `session_host_unavailable` | `the session host could not be reached` |
| 503 | `audit_unavailable` | `auditing is off; the Chief of Staff cannot send` |

The delivered text appears on the session as a `user_message` frame with
`"origin":"chief-of-staff"`.

`POST /api/chief-of-staff/sessions`

- Request: `{"projectId","folder"?,"prompt","model","mode"?}`, at most 64 KiB.
  `mode` is `headless` (default) or `terminal`.
- Success: 201
  `{"sessionId","name","projectId","directory","mode","kind","origin":"chief-of-staff","at"}`.
- Errors, coded with message: `invalid_body`, `project_id_required`,
  `prompt_required`, `prompt_too_long` (over 8000 characters),
  `model_required`, `mode_invalid`, `folder_invalid`, `audit_unavailable`
  (503), `body_too_large`. A launch refusal uses the launch refusal code
  and text (see Sessions), still in this family. `502 launch_failed`
  (`the session could not be started`) and `502 prompt_not_delivered` close
  the list.

checked against relay at `59cdf9e`.

### Hosts

Host JSON (the `hostView`):

```json
{"id":"h_box","name":"testbox","target":"acme@testbox","port":22,
 "identity_file":"~/.ssh/id_acme","tmux_path":"/usr/bin/tmux",
 "created_at":"2026-10-09T10:00:00Z",
 "probe":{"at":"2026-10-09T10:00:00Z","ok":true,"os":"Linux","arch":"x86_64",
          "home":"/home/acme","shell":"/bin/bash","node_path":"/usr/bin/node",
          "node_version":"v22.0.0","claude_path":"/usr/bin/claude",
          "claude_version":"2.0.0","tmux_path":"/usr/bin/tmux"},
 "status":"idle",
 "ssh_argv":["ssh","-o","BatchMode=yes","-o","ConnectTimeout=10",
             "-o","ServerAliveInterval=15","-o","ServerAliveCountMax=3",
             "-o","ControlMaster=auto","-o","ControlPath=<DIR>/run/ssh/%C",
             "-o","ControlPersist=600","acme@testbox"],
 "terminal_templates":[]}
```

- `port`, `identity_file`, `tmux_path` and `probe` are omitted when empty.
- `probe.at` is always present. A failed probe has `"ok":false` and `error`.
- `terminal_templates` is always an array.
- `ssh_argv` is the full fixed-option prefix that `ssh-hosts.md` gives, then
  `-p`, `-i` and the target. `<DIR>/run/ssh` is the control directory.
- Eve strips `ssh_argv` before the browser sees it.
- `status` is `connected`, `idle`, `unreachable` or `unknown`. The rule for
  fakerelay is in the host simulation section.

| Route | Request | Success | Errors |
|---|---|---|---|
| `GET /api/hosts` | none | 200, array of host JSON, `[]` when none | |
| `GET /api/hosts/{id}` | none | 200, host JSON | 404 `{"error":"host not found: <id>"}` |
| `POST /api/hosts` | `{name,target,port?,identity_file?,tmux_path?}` | 201, host JSON, probed once | 400 `{"error":"host name is required"}` or `host target is required` |
| `PUT /api/hosts/{id}` | any of the same keys | 200, host JSON | 400, 404 |
| `DELETE /api/hosts/{id}` | none | 204 | 404, 409 |
| `POST /api/hosts/{id}/probe` | none | 200, host JSON with a new `probe` (see Hosts rules) | 404 `{"error":"host not found"}` |
| `POST /api/hosts/{id}/disconnect` | none | 200, host JSON | 404 |

`DELETE` with a project still on the host answers 409
`{"error":"host is used by one or more projects","projects":["Acme"]}`, the
`projects` being project names.

Host templates use the terminal template shape (see Terminals):

| Route | Success | Errors |
|---|---|---|
| `GET /api/hosts/{id}/templates` | 200, array in insertion order | 404 `{"error":"host \"<id>\" not found"}` (the template routes all word it so) |
| `POST /api/hosts/{id}/templates` | 201, the template | 400, 404, 409 |
| `PUT /api/hosts/{id}/templates/{tid}` | 200, the template | 400, 404 |
| `DELETE /api/hosts/{id}/templates/{tid}` | 204 | 404 |

Persistent terminals:

```json
[{"name":"relay-p_box000-shell-1","template_id":"shell","n":1,
  "created":1789000000,"attached":0,"attached_here":false}]
```

`created` is Unix seconds. `attached` is the tmux client count.
`attached_here` is true when a live terminal on this relay carries that name.

| Case | Status | Body |
|---|---|---|
| Project not found, or not a host project | 404 | `{"error":"hosted project \"<id>\" not found"}` |
| Session not found | 404 | `{"error":"session \"<name>\" not found on host <host name>"}` |
| Bad session name | 400 | `{"error":"\"<name>\" is not a relay persistent session name"}` |
| Host has no tmux | 409 | `{"error":"host has no tmux"}` |
| Host unreachable | 502 | `{"error":"host unreachable"}` |
| Kill succeeded | 204 | none |

Events: `host.list`, `host.get`, `host.create`, `host.update`, `host.remove`,
`host.probe`, `host.disconnect`, `host_template.list`, `session.persistent.list`.

checked against relay at `59cdf9e`.

### File routes (G1)

Every file route is class `execute`. checked against relay at `59cdf9e` (G1),
and equal to the eve file-plane contract.

Check order, first failure wins: project, kind, lexical path, read-only,
audit intent, backend.

All paths in a request and a response are root-relative POSIX paths.

- A leading `/` is stripped. `""`, `/` and `.` mean the root. `.` segments
  are dropped.
- A `..` segment answers 403 `TRAVERSAL`. A NUL byte answers 400 `INVALID`.
- `name` and `new_name` are one segment: not empty, not `.` or `..`, no `/`
  and no NUL. Otherwise 400 `INVALID`, `invalid name`.
- Response paths have no leading slash.
- `type` is `file`, `directory` or `symlink`, read with `lstat`.
- `mtime_ms` is an integer, milliseconds since the epoch.

| Op | Route | Request body | 200 body |
|---|---|---|---|
| list | `POST /api/projects/{id}/files/list` | `{"path":"src","show_hidden":false}` | `{"entries":[{"name":"a.js","type":"file","size":12,"mtime_ms":1789000000000}]}` |
| stat | `POST …/files/stat` | `{"path":"src/a.js"}` | `{"type":"file","size":12,"mtime_ms":1789000000000}` |
| read | `POST …/files/read` | `{"path":"src/a.js","max_bytes":10485760}` | `{"content":"…","size":12}` |
| stream | `GET …/files/stream?path=<urlencoded>` | none | raw bytes |
| write | `POST …/files/write` | `{"path":"a.md","content":"…","encoding":"utf8","create_only":false}` | `{"path":"a.md"}` |
| mkdir | `POST …/files/mkdir` | `{"parent":"src","name":"lib"}` | `{"path":"src/lib"}` |
| rename | `POST …/files/rename` | `{"path":"src/a.js","new_name":"b.js"}` | `{"path":"src/b.js"}` |
| move | `POST …/files/move` | `{"path":"src/a.js","dest_dir":"lib"}` | `{"path":"lib/a.js"}` |
| delete | `POST …/files/delete` | `{"path":"old.txt"}` | `{"trashed":true}` |
| search | `POST …/files/search` | see below | `{"matches":[…],"truncated":false}` |
| git | `POST …/files/git` | `{"cwd":"repo","args":["status","--porcelain=v2"],"max_bytes":8388608}` | `{"exit_code":0,"stdout_b64":"…","stderr":""}` |
| pastetmp | `POST /api/hosts/{id}/pastetmp` | `{"name":"eve-paste-1789000000000-ab12cd34.png","data_b64":"…"}` | `{"path":"/tmp/eve-paste-1789000000000-ab12cd34.png"}` |

Op details:

- **list.** `entries` is `[]` for an empty directory, never null. A symlink
  shows as `type: "symlink"` and list never descends into it. Hidden entries
  (names starting with `.`) are listed only when `show_hidden` is true.
  Order is not specified; the client sorts.
- **read.** `content` is the file as UTF-8 text. `max_bytes` of 0 or less, or
  above 10 MiB, means 10 MiB. A file over the limit answers 413 `TOO_LARGE`
  with `size`.
- **stream.** Content type `application/octet-stream`. A console project
  honours `Range`: it sends `Accept-Ranges: bytes` and `Content-Length`, and
  206 with `Content-Range` for a satisfiable range. A host project always
  answers 200 with a chunked body and ignores `Range`. Errors before the
  first byte are the coded file error as JSON.
- **write.** `encoding` is `utf8` (default when empty) or `base64`. Any other
  value answers 400 `encoding must be utf8 or base64`. A base64 body that
  does not decode answers 400 `content is not base64`. The decoded size is
  at most 10 MiB, else 413. With `create_only: true` an existing target
  answers 409 `EEXIST`. A path of `""` answers 400 `EISDIR`,
  `Path is a directory`.
- **mkdir.** An existing target answers 409 `EEXIST`.
- **rename and move.** An existing destination answers 409 `EEXIST` on both
  console and host projects. A case-only rename of the same entry is allowed.
  The project root cannot be renamed, moved or deleted: 400 `INVALID`
  (`cannot rename the project root`, `cannot move the project root`,
  `cannot delete the project root`). A symlink source is refused with 403
  `SYMLINK`.
- **delete.** `trashed` is true for a console project (the entry moves to the
  Trash) and false for a host project (permanent). A symlink is refused.
- **search.** Request:

```json
{"query":"TODO","regex":false,"word":false,"case_sensitive":null,
 "globs":["*.js"],"max_matches":500}
```

Match row:
`{"path":"src/a.js","line":3,"col":5,"len":4,"text":"// TODO x"}`.

  - `line` and `col` are one-based. `col` and `len` count UTF-16 code units.
  - `case_sensitive: null` means smart case: case-sensitive only when the
    query holds an uppercase letter.
  - `word` wraps the pattern in word boundaries.
  - A glob matches the root-relative path. `*` crosses `/`. A leading `!`
    excludes. At most 5 globs of at most 200 characters; none starts with `/`
    or holds `..`. The query is at most 1000 characters and not empty. A bad
    regex, an empty query or a bad glob answers 400 `INVALID`.
  - File set: inside a git work tree, `git ls-files -co --exclude-standard -z`.
    Elsewhere a walk that skips hidden entries and `node_modules`. Symlinks
    and binary files (a NUL in the first 8000 bytes) are always skipped.
  - Limits: 500 matches in total, 50 per file, files over 5 MiB skipped,
    10 MiB scanned in all. A cut-off sets `truncated: true`. It is not an
    error. `max_matches` can only lower the 500.
  - `matches` is `[]` when nothing matched.
- **git.** relay prepends the fixed arguments
  `-c core.quotepath=off -c core.fsmonitor=false -c core.hooksPath=/dev/null`.
  The request sends from the subcommand on.
  - `args[0]` must be one of `rev-parse`, `worktree`, `symbolic-ref`,
    `for-each-ref`, `merge-base`, `status`, `rev-list`, `diff`, `ls-files`,
    `cat-file`. Else 400 `INVALID`, `git subcommand not allowed`.
  - `worktree` is allowed only as `worktree list`. `symbolic-ref` with more
    than one positional argument is refused.
  - An argument that starts with any of these is refused with 400 `INVALID`:
    `--output`, `--ext-diff`, `--textconv`, `--exec`, `--upload-pack`,
    `--receive-pack`, `-c`, `--config`, `--git-dir`, `--work-tree`,
    `--namespace`, `-C`, `-O`, `--open-files-in-pager`, `--no-index`,
    `--filters`. A NUL in an argument is refused too.
  - Environment: no inherited `GIT_*` variables, plus `GIT_OPTIONAL_LOCKS=0`,
    `GIT_TERMINAL_PROMPT=0` and `LC_ALL=C`. Time limit 10 s, then 504
    `TIMEOUT`. `max_bytes` defaults to 8 MiB, at most 32 MiB.
  - A non-zero exit code is still a 200. `stdout_b64` is standard base64.
  - A folder that is not a repository gives `exit_code: 128` and a `stderr`
    that begins `fatal: not a git repository`.
- **pastetmp.** `name` must match
  `eve-paste-<digits>-<hex>.(png|jpg|gif|webp)`, else 400 `INVALID`,
  `invalid paste file name`. `data_b64` must decode, else 400 `INVALID`,
  `data_b64 is not base64`. Decoded size at most 10 MiB. An unknown host
  answers 404 `HOST_NOT_FOUND`, `host not found`. The route has no project,
  so no read-only check. The file lands in the host's temp directory.

Size limits for request bodies: 64 KiB for every route except `write` and
`pastetmp`, which allow 16 MiB. A larger body answers 413 `TOO_LARGE`,
`request body too large`, with `size` when the length is known.

#### File errors

Body: `{"error":"<message>","code":"<CODE>"}`, plus `"size":<bytes>` on
`TOO_LARGE`.

| Status | `code` | `error` text (relay's) | When |
|---|---|---|---|
| 404 | `PROJECT_NOT_FOUND` | `project not found` | unknown project id |
| 404 | `HOST_NOT_FOUND` | `host not found` | pastetmp, unknown host |
| 403 | `NOT_AVAILABLE` | `files are not available for this project` | `kind: remote`, or no path |
| 400 | `INVALID` | varies | bad body, name, query, regex, git argv |
| 403 | `TRAVERSAL` | `Path traversal not allowed` | a `..` segment |
| 403 | `SYMLINK` | `Symbolic links are not opened` | any component is a symlink, on every op except list and search |
| 403 | `READ_ONLY` | `This project is read-only` | write, mkdir, rename, move or delete with `files_read_only: true` |
| 403 | `EACCES` | `Permission denied` | permission denied |
| 404 | `ENOENT` | `Not found` on the console, `No such file or directory` on a host | path missing |
| 400 | `EISDIR`, `ENOTDIR` | `Path is a directory`, `Not a directory` | wrong kind of entry |
| 409 | `EEXIST` | `Already exists` | destination exists, on write, mkdir, rename and move |
| 413 | `TOO_LARGE` | `File too large` | over a limit |
| 500 | `GIT_MISSING`, `ERROR` | `file operation failed` | git not installed, or any other failure |
| 503 | `HOST_UNREACHABLE` | `host is not connected` | the host agent is not connected |
| 503 | `AUDIT_UNAVAILABLE` | `audit log unavailable; the change was not made` | the intent row cannot be written |
| 504 | `TIMEOUT` | `timed out` | host request or git over its limit |

A client matches `code`. The texts above are relay's, and fakerelay words them
the same. A bad search query answers `Search query is empty` or
`Invalid regex: <Go's parse error>`. A refused git argument answers
`git argument not allowed: <the whole argument>`.

The project is read from the current world state on every request. A change
to `files_read_only`, a path or a host applies to the next call.

#### File audit rows

Mutations (write, mkdir, rename, move, delete, pastetmp) write `file_op`
rows. Reads write none. With auditing off, operations run and write
nothing. Shape:

```json
{"id":"a1","ts":"2026-10-09T10:00:00.000Z","dur_ms":0,"event":"file_op",
 "phase":"intent",
 "actor":{"kind":"control","auth":"token","cred_id":"launch:service:eve",
          "project_id":"p_acme","project_name":"Acme"},
 "tool":"write","mcp_root":"/home/acme/app",
 "args":{"path":"notes/a.md","host_id":"","bytes":42,"encoding":"utf8","create_only":false},
 "outcome":"pending"}
```

| Tool | `args` |
|---|---|
| `write` | `path`, `host_id`, `bytes`, `encoding`, `create_only` |
| `mkdir` | `path`, `host_id` |
| `rename`, `move` | `path`, `new_path`, `host_id` |
| `delete` | `path`, `host_id`, `trashed` |
| `pastetmp` | `host_id`, `name`, `bytes` (no project fields, no `mcp_root`) |

| Row | Fields |
|---|---|
| Intent | `phase: "intent"`, `outcome: "pending"` |
| Completion | same `id`, `phase: "completion"`, `outcome: "ok"` or `"error"`, `error` is the code, `dur_ms` set |
| Refusal | one row, no `phase`, `outcome: "denied"`, `error` is `TRAVERSAL`, `SYMLINK` or `READ_ONLY` |

`cred_id` is the credential id for a bearer, or `launch:service:<service id>`
for a launch identity. `mcp_root` is the project path. `args.path` is the
cleaned path, except on a `TRAVERSAL` refusal, where it is the path as sent.
Argument and availability errors write no refusal row. An intent that cannot
be written refuses the operation with 503 `AUDIT_UNAVAILABLE` before it runs.

Events: `file.list`, `file.stat`, `file.read`, `file.stream`, `file.write`,
`file.mkdir`, `file.rename`, `file.move`, `file.delete`, `file.search`,
`file.git`, `host.pastetmp`, `file.ws.close`.

### /ws/files (G2)

`GET /ws/files` upgrades to a WebSocket with JSON text frames. Class
`execute`, socket only. checked against relay at `59cdf9e` (G2).

Client to server:

```json
{"type":"watch","project_id":"p_acme"}
{"type":"unwatch","project_id":"p_acme"}
```

A frame that is not JSON, has no `project_id`, or has another `type` is
ignored with no answer.

Server to client:

```json
{"type":"watch_ok","project_id":"p_acme"}
{"type":"watch_error","project_id":"p_acme","code":"HOST_UNREACHABLE","error":"host is not connected"}
{"type":"fs_event","project_id":"p_acme","path":"src/a.js","kind":"change"}
{"type":"host_status","host_id":"h_box","name":"testbox","status":"connected"}
{"type":"host_status","host_id":"h_box","name":"testbox","status":"unreachable","error":"disconnected"}
```

| Frame | Fields | Notes |
|---|---|---|
| `watch_ok` | `type`, `project_id` | the watcher is live. Changes after this frame are delivered |
| `watch_error` | `type`, `project_id`, `code`, `error` | the connection holds no watch for that project afterwards |
| `fs_event` | `type`, `project_id`, `path`, `kind` | `path` is root-relative with no leading slash. `kind` is `change` or `rename` |
| `host_status` | `type`, `host_id`, `name`, `status`, `error`? | `status` is `connecting`, `connected` or `unreachable`. `error` is omitted when empty |

`watch_error.code` is one of `ENOENT`, `UNSUPPORTED`, `HOST_UNREACHABLE`,
`PROJECT_NOT_FOUND`, `NOT_AVAILABLE`, `PROJECT_CHANGED` or `ERROR`. Any other
cause is reported as `ERROR`.

Behaviour:

- `watch` is idempotent per connection. A repeated `watch` for a watched
  project answers `watch_ok` again.
- Frames for one project are handled in order, so a `watch` then an `unwatch`
  cannot reorder.
- One watcher serves a project for every connection. It stops after the last
  `unwatch` or disconnect. A closed connection drops all its watches.
- Right after the upgrade the server sends one `host_status` for each host
  agent it holds, then one on every status change.
- When a project is deleted, or its path or host changes, each watching
  connection gets `watch_error` with code `PROJECT_CHANGED` and `error`
  `project changed`, and loses that watch.
- No debounce and no filtering. Events under `.git` and `node_modules` are
  delivered.
- A connection that cannot keep up (1024 queued frames) is closed. The client
  reconnects and watches again.
- The server sends WebSocket ping frames and closes a connection that sends
  no pong or frame within 60 s.

### Sessions (G4)

Session ids are UUIDs. `model` is a value from `GET /api/models`. Session
kind comes from the model: `haiku`, `sonnet` and `opus` are `claude`; a model
that starts `pi/` is `pi`; one that starts `codex/` is `codex`; any other is
`chat`.

`POST /api/sessions` (class `execute`, body at most 1 MiB):

```json
{"projectId":"p_acme","directory":"","name":"Fix the build","model":"haiku",
 "settings":{"permissionMode":"default"},"systemPrompt":"","appendClaudeMd":false}
```

`projectId` is required. `directory` defaults to the project path and must
stay inside it. `settings` is an object and is passed to the session host.
`settings.headless: true` makes a headless session. With `settings.agent: true`
it is tracked and listed; without `agent` it is neither tracked nor listed in
`GET /api/sessions`, though drop-in still reaches it. A value that is not a
boolean counts as false. A turn of a headless `agent` session keeps only the
user message in its history, so `messageCount` is 1 after one turn. Success is 201 with the new session:

```json
{"sessionId":"3f0c…","projectId":"p_acme","name":"Fix the build",
 "directory":"/home/acme/app","model":"haiku","providerType":"claude",
 "createdAt":"2026-10-09T10:00:00Z","messages":[],
 "stats":{"inputTokens":0,"outputTokens":0,"cacheReadTokens":0,
          "cacheCreationTokens":0,"costUsd":0}}
```

`folder`, `settings`, `systemPrompt`, `headless`, `agent`, `origin`,
`thinkingLevel`, `policy` and `permissionMode` are omitted when empty, and
`permissionMode` appears only when the request set one. `host` appears only for
a host project, as `{"id","name","ssh_argv",…}`; a console project has no
`host` key.

A refusal is `{"error":"<message>"}` (Simple family, no code). Statuses and
texts:

| Status | `error` |
|---|---|
| 403 | `caller does not hold execute on the frontend socket` |
| 403 | `<kind> sessions require a project` |
| 403 | `project is not available for a session launch` |
| 403 | `requested directory is outside the project` |
| 403 | `model is not allowed for this project` |
| 403 | `model "<id>" is reserved for system use and cannot host a chat session` |
| 400 | `chat session has no model; choose a model and try again` |
| 400 | `unknown session kind "<kind>"` |
| 400 | `invalid JSON: <decoder text>` |
| 413 | `request body too large` |
| 502 | `launch failed` |

`GET /api/sessions` (class `proxy`) answers 200
`{"sessions":[<row>,…]}`, sorted by `id`. Row:

```json
{"id":"3f0c…","projectId":"p_acme","name":"Fix the build","directory":"/home/acme/app",
 "model":"haiku","live":true,"createdAt":"2026-10-09T10:00:00Z",
 "messageCount":2,"lastMessageAt":"2026-10-09T10:00:05Z",
 "attention":{"state":"idle","since":"2026-10-09T10:00:05.123Z"}}
```

| Key | Rule |
|---|---|
| `live` | the provider process is running. A world session with `state: "dormant"` is `false` |
| `lastMessageAt` | omitted when no messages |
| `folder`, `host`, `headless`, `origin` | omitted when empty |
| `attention` | only for a live session of kind `claude`, `pi` or `codex` that is not headless, or is headless with `agent`; a session a drop-in has taken stays attended while held (`running`) and after hand-back (`idle`), though `live` is false. Absent otherwise. `since` equals the `since` of the last `session_state` frame |
| `headless` | `true` for a headless session launched with `agent`. A headless launch without `agent` is not listed at all |

`POST /api/sessions/{id}/resume` (class `execute`):

| Case | Status | Body |
|---|---|---|
| Unknown id | 404 | `{"error":"session not found"}` |
| Already live | 200 | `{"session_id":"3f0c…","resumed":false}` |
| Dormant, resumed | 200 | `{"session_id":"3f0c…","resumed":true}` |
| A resume is already running | 409 | `{"error":"a resume for this session is already in progress"}` |
| Launch refused | 403 or 400 | `{"error":"<launch refusal text>"}` |

A resumed session is live again. Its `session_joined` frame then has
`live: true`.

`DELETE /api/sessions/{id}` answers 204 always, including for an unknown id.
It removes the session. Event `session.delete`.

A request under `/api/sessions/` or `/api/terminals/` that matches no route
above answers 404, `Content-Type: text/plain; charset=utf-8`, body
`404 page not found\n` (the session host's mux). This covers `.../stop` and
`.../delete`. It needs class `proxy`, writes no event and changes nothing. Any
other unclaimed path keeps the dispatch 404. relay-sessions answers 405 for a
known path with the wrong method; fakerelay does not (D20).

`POST /api/sessions/{id}/drop-in` (class `execute`, socket only) takes a
headless Claude session over for an echo terminal. Optional body
`{"cols","rows"}`; 0 or less (or no body) means 120 by 40. A refusal is
`{"error":"<code>","message":"<text>"}`. Checks run in this order:

| # | Case | Status | `error` | `message` |
|---|---|---|---|---|
| 1 | unknown session | 404 | `session_not_found` | `no session <id>` |
| 2 | kind is not `claude` | 409 | `not_claude` | `only Claude sessions can be taken over; this is a <kind> session` |
| 3 | the `claude-code` terminal launch is refused (console templates, or the host's for a host project) | the launch refusal's status | the launch code, such as `template_unavailable` | the text `POST /api/terminals` gives |
| 4 | not headless | 409 | `not_headless` | `this session is not headless; continue it in eve` |
| 5 | already held | 409 | `dropped_in` | `a terminal already has this session; close it first` |
| 6 | a turn waits on a permission | 409 | `tool_running` | `a tool is running (<tool>); wait for it to finish or stop the turn, then try again` |
| 7 | a turn is running | the request waits for the turn to end or the caller to leave, then checks again | | |
| 8 | no turn has run yet | 409 | `no_conversation` | `the session has not run a turn yet; there is nothing to take over` |

`turn_timeout` is not faked. Success is 201:

```json
{"sessionId":"3f0c…","claudeSessionId":"9d2e…","host":"testbox",
 "terminal":{"terminalId":"7a1b…","templateId":"claude-code",
             "name":"Fix the build (drop-in)","directory":"/home/acme/app","host":null}}
```

- `claudeSessionId` is a lowercase UUID v4 made at the session's first turn.
  It is not the session id.
- `host` is the SSH host's name, omitted for a console project.
- `terminal` is the `POST /api/terminals` 201 body. `name` is the session's
  create-request name plus ` (drop-in)`, or `session (drop-in)` without one.
- On success, in order: the hold is set; the agent stops (`live` becomes
  false); a tracked session broadcasts `session_state` `running`; the echo
  terminal starts.
- While held, `/ws` `send_message` gets the coded `dropped_in` error and a
  second drop-in gets 409 `dropped_in`.
- When the drop-in terminal ends (exit, `terminal_close`, or
  `DELETE /api/terminals/{id}`), the hold clears and a tracked session
  broadcasts `session_state` `idle` with a new `since`. The session stays
  dormant, so the next send gets `resume_required`.
- Event `session.drop_in` (in `relay.log`): `session_id`, `host` (host name or
  `console`), `terminal_id` (`""` on refusal). A 4xx refusal is `denied` with
  the code as `reason`; a 5xx is `error`. No audit row is written.

Events: `session.list`, `session.launch`, `session.resume`, `session.delete`,
`session.drop_in`.
Session host events go to `relaysessions.log`.

### /ws frames (G3)

`GET /ws` upgrades to a WebSocket with JSON text frames, class `proxy`. One
hub serves every connection. A client frame with an unknown or missing `type`
is dropped with no answer.

Notation: `sessionId` and `terminalId` are strings. Times are RFC 3339 UTC with
three-digit milliseconds, such as `2026-10-09T10:00:00.123Z`. A "viewer" is a
connection that joined the session or terminal.

#### Client to server, sessions

| `type` | Fields | Effect |
|---|---|---|
| `join_session` | `sessionId` | adds the connection as a viewer, answers `session_joined`. Unknown id: `error` frame |
| `leave_session` | `sessionId` | removes the viewer. No answer |
| `send_message` | `sessionId`, `text`, `files`?, `trace_id`? | starts a turn. `trace_id` (`[A-Za-z0-9_-]{8,64}`) is the trace of the `chat.turn` event, else a new id is made |
| `stop_generation` | `sessionId` | ends the running turn |
| `permission_response` | `permissionId`, `approved`, `reason`? | resolves a pending permission request. See below |
| `end_session` | `sessionId` | stops the provider. The session stays listed as dormant. No `session_ended` frame is sent |
| `delete_session` | `sessionId` | removes the session and broadcasts `session_ended` |
| `rename_session` | `sessionId`, `name` | sends `session_renamed` to viewers |
| `clear_session` | `sessionId` | clears history. Sends `clear_messages`, `stats_update` and `system_message` to viewers |
| `set_session_folder` | `sessionId`, `folder` | sends `session_folder_changed` to viewers |
| `set_permission_mode` | `sessionId`, `mode` | sends `mode_changed` to viewers. Claude sessions only |

A `sessionId` that is missing or empty gets `{"type":"error","message":"sessionId required"}`,
except on `join_session`, `leave_session` and `permission_response`, which are
dropped silently.

`permission_response` rules:

- An unknown, resolved or timed-out `permissionId` is ignored with no answer.
- The connection must have joined the request's session. Otherwise it gets
  `{"type":"error","message":"permission response refused: this connection has not joined session <id>"}`.
- `approved: true` allows. `approved: false` denies, with `reason` or
  `Denied by user`.

#### Server to client, sessions

`session_joined`, sent to the joiner only:

```json
{"type":"session_joined","sessionId":"3f0c…","projectId":"p_acme",
 "directory":"/home/acme/app","model":"haiku","name":"Fix the build","folder":"",
 "history":[{"timestamp":"2026-10-09T10:00:00Z","role":"user","content":"hello"}],
 "stats":{"inputTokens":0,"outputTokens":0,"cacheReadTokens":0,
          "cacheCreationTokens":0,"costUsd":0},
 "headless":false,"protocolVersion":"2","host":null,"live":true}
```

- `history` is always an array. A `user` message has `content` as a JSON
  string. An `assistant` message has `content` as an array of blocks such as
  `{"type":"text","text":"echo: hello"}`. A message may carry `files`,
  `toolName`, `toolUseId` and `origin`.
- `protocolVersion` is the string `"2"`.
- `live` says whether `send_message` would work now or answer
  `resume_required`.

Frames sent to every viewer of a session:

```json
{"type":"user_message","sessionId":"3f0c…","text":"hello"}
{"type":"llm_event","sessionId":"3f0c…","event":{"v":2,"type":"assistant","message":{"id":"msg_0b8f722ad2df1cee","role":"assistant","content":[]}}}
{"type":"stats_update","sessionId":"3f0c…","stats":{"inputTokens":3,"outputTokens":4,"cacheReadTokens":0,"cacheCreationTokens":0,"costUsd":0}}
{"type":"message_complete","sessionId":"3f0c…"}
{"type":"message_complete","sessionId":"3f0c…","isError":true,"apiErrorStatus":529}
{"type":"permission_request","sessionId":"3f0c…","permissionId":"perm1","toolName":"Bash","toolInput":"{\"command\":\"ls\"}","toolUseId":"tu1"}
{"type":"session_renamed","sessionId":"3f0c…","name":"New name"}
{"type":"process_exited","sessionId":"3f0c…"}
{"type":"error","sessionId":"3f0c…","code":"resume_required","message":"session: provider not running; resume required"}
{"type":"error","message":"session not found: 3f0c…"}
```

- `user_message` carries `origin` only when the message was not typed by the
  person (`"chief-of-staff"`).
- `message_complete` has `isError` and `apiErrorStatus` only on a failed turn.
- `permission_request.toolInput` is a string holding JSON (`{}` when empty).
- `llm_event.event` is the canonical event envelope. Every event has `v: 2`
  and a `type`. The events a turn uses:

| `type` / `subtype` | Extra keys |
|---|---|
| `system` / `init` | `model`, `cwd`, `tools`, `mcp_servers` |
| `assistant` (message start) | `message:{id,role:"assistant",content:[]}`, `error`? and `apiErrorStatus`? on an API failure |
| `assistant` (block start) | `index`, `content_block:{type:"text"}` or `{type:"tool_use",id,name,input}` |
| `assistant` (block delta) | `index`, `delta:{type:"text_delta",text}` |
| `assistant` (block stop) | `index`, `content_block_stop:true`, `content_block`? for tool_use |
| `result` / `tool_result` | `tool_use_id`, `tool_name`?, `content`, `is_error`, `scope_violation`? |
| `system` / `permission_request` | `permission_id`, `tool_name`, `tool_use_id`?, `tool_input`? |

`error` frames: a coded one has `sessionId`, `code` and `message`. The
`code` is `resume_required` or `dropped_in`. All other errors have only
`message`. A viewer is not required for the sender of a command to get its
`error`.

`error` messages for `send_message`:

| Cause | Frame |
|---|---|
| `sessionId` empty | `{"type":"error","message":"sessionId required"}` |
| Unknown session | `{"type":"error","message":"session: not found"}` |
| A turn is running | `{"type":"error","message":"session: already processing a message"}`. No `code` |
| Dormant | coded `resume_required` |
| A terminal holds the session | coded `dropped_in`, `message` `session: a terminal holds this session` |
| Other | `{"type":"error","message":"<text>"}` |

Hub broadcasts go to every connection, joined or not:

```json
{"type":"session_state","sessionId":"3f0c…","state":"running","since":"2026-10-09T10:00:00.123Z"}
{"type":"turn_done","sessionId":"3f0c…","excerpt":"echo: hello","at":"2026-10-09T10:00:00.456Z"}
{"type":"session_ended","sessionId":"3f0c…"}
{"type":"terminal_closed","terminalId":"7a1b…"}
```

`state` is `starting`, `idle`, `running`, `asking`, `errored`, `stalled` or
`ended`. `excerpt` is the last 500 runes of the reply.

#### One echo turn, in wire order

For a live, tracked session (kind `claude`, `pi` or `codex`, not headless or
headless with `agent`), a viewer sees:

1. `user_message`
2. `session_state` `running`
3. `llm_event` frames (message start, block start, one or more deltas, block
   stop), and `stats_update`
4. `turn_done`
5. `session_state` `idle`
6. `message_complete`

The attention frames (2, 4, 5) are produced before the `message_complete`
frame is sent. An untracked session (kind `chat`, or a headless session
without `agent`) sends 1, 3 and 6 only, and has no `attention` field. A turn
that fails sends `session_state` `errored` and `message_complete` with
`isError: true`. A permission wait sends `permission_request` and
`session_state` `asking`. After the answer the state returns to `running`.
checked against relay at `59cdf9e` (G3).

The event `chat.turn` is written once per turn, before `message_complete`
leaves the fake. It carries `session_id` and the trace. It never carries the
text.

#### Client to server and back, terminals

| `type` | Fields | Effect |
|---|---|---|
| `join_terminal` | `terminalId` | adds a viewer, answers `terminal_joined` |
| `terminal_reconnect` | `terminalId`, `cols`?, `rows`? | resizes when both are positive and different, then acts as `join_terminal` |
| `leave_terminal` | `terminalId` | removes the viewer. No answer |
| `terminal_input` | `terminalId`, `data` | `data` is base64 of the bytes to write. Input to a stopped terminal is dropped. An echo terminal can end on it (see Echo terminal) |
| `terminal_resize` | `terminalId`, `cols`, `rows` | resizes the terminal |
| `terminal_close` | `terminalId` | closes it and broadcasts `terminal_closed` |
| `terminal_list` | none | answers `terminal_list` |
| `terminal_create` | any | retired. Answers an `error` |
| `terminal_templates` | any | retired. Answers an `error` |

Replies and pushes:

```json
{"type":"terminal_joined","terminalId":"7a1b…","templateId":"shell","name":"Shell",
 "directory":"/home/acme/app","state":"running","cols":80,"rows":24,
 "scrollback":"<base64>","host":null}
{"type":"terminal_output","terminalId":"7a1b…","data":"<base64>"}
{"type":"terminal_exit","terminalId":"7a1b…","exitCode":0}
{"type":"terminal_closed","terminalId":"7a1b…"}
{"type":"terminal_list","terminals":[{"id":"7a1b…","templateId":"shell","name":"Shell","directory":"/home/acme/app","state":"running"}]}
```

- `terminal_output` goes to viewers only. `terminal_input` is routed by id
  from any connection.
- `state` is `running` or `stopped`. A stopped terminal's `terminal_joined`
  is followed by a `terminal_exit`.
- `terminal_closed` is a hub broadcast.
- `host` is `null` for a console terminal, or `{"id","name"}` for a host one.
- Errors, all `{"type":"error","message":"<text>"}`:

| Cause | `message` |
|---|---|
| Unknown terminal on join or reconnect | `terminal not found: <id>` |
| Write or resize to an unknown terminal | `terminal not found: <id>` |
| Bad base64 in `terminal_input` | `invalid base64 data` |
| `terminal_create` | `terminal_create over WebSocket is retired; POST /api/terminals instead` |
| `terminal_templates` | `terminal_templates over WebSocket is retired; GET /api/terminal/templates instead` |

A frame with an empty `terminalId` is dropped with no answer.

Echo terminal: input bytes come back as `terminal_output` data, with each
`\r` becoming `\r\n`. It ends as a real terminal does, with no `ctl` verb.
A "line" is the bytes since the last `\r`.

- `\x04` on an empty line: exit 0 with no output. `\x04` in the middle of a
  line is dropped.
- A `\r` that ends a line whose trimmed text is `exit` or `exit N` (N from 0 to
  255): the line echoes as usual, then the terminal exits with N (bare `exit`
  is 0). Any other line, such as `exit 300`, echoes as today.
- Bytes after the exit point are dropped.

On exit, in order: state `stopped` with the exit code set; the event
`session.exited` `{session_id: <terminal id>}` in `relay.log` (status `ok`, no
caller trace); `{"type":"terminal_exit","terminalId","exitCode"}` to each
viewer. Afterwards `GET /api/terminals` and `terminal_list` show
`state: "stopped"` and `exitCode` only when not 0. A join answers
`terminal_joined` with `state: "stopped"`, then `terminal_exit`. The terminal
stays listed until `terminal_close` or `DELETE`.

checked against relay at `59cdf9e` (G3).

### Terminals (G4)

`POST /api/terminals` (class `execute`, body at most 1 MiB):

```json
{"templateId":"shell","name":"Shell","directory":"","projectId":"p_acme",
 "cols":80,"rows":24,"persist_session":"","extraArgs":[]}
```

`projectId` is required. `directory` defaults to the project path and must
stay inside it. `templateId` must be allowed for the project (see
`allowed_templates`; `["*"]` allows all). `persist_session` is `snake_case`
and applies only to a persist template of a host project. `extraArgs` is not
supported for a host project and has at most 64 entries.

Success is 201:

```json
{"terminalId":"7a1b…","templateId":"shell","name":"Shell",
 "directory":"/home/acme/app","host":null}
```

This is the `terminal_created` frame of the old WebSocket flow, minus
`type`. `host` is `null` or `{"id","name"}`.

Refusals are `{"error":"<message>"}`, with the same statuses and texts as
session create, plus:

| Status | `error` |
|---|---|
| 403 | `terminal template "<id>" is not available for this project` |
| 403 | `persist_session applies only to a persist template of a host project` |
| 400 | `extraArgs are not supported for host terminals` |
| 400 | `extraArgs exceed the cap of 64 entries and 65536 bytes` |

`GET /api/terminals` (class `proxy`) answers 200 `{"terminals":[<row>,…]}`
sorted by `id`. Row:

```json
{"id":"7a1b…","templateId":"shell","name":"Shell","directory":"/home/acme/app",
 "state":"running","exitCode":0,"host":{"id":"h_box","name":"testbox"},"origin":"chief-of-staff"}
```

`exitCode`, `host` and `origin` are omitted when empty or zero. `state` is
`running` or `stopped`.

`DELETE /api/terminals/{id}` answers 204, or 404 with an empty body for an
unknown id. It closes the terminal. Events: `terminal.list`, `session.launch`,
`terminal.delete`, `terminal.log`.

`GET /api/terminals/{id}/log` (class `proxy`, socket only):

| Case | Status | Body |
|---|---|---|
| a terminal that has run | 200, `Content-Type: text/plain; charset=utf-8` | every byte the terminal sent as `terminal_output`, in order |
| a UUID-shaped id with no log | 404 | empty |
| an id that is not 36 characters in 8-4-4-4-12 hex (either case) | 400 | empty |

- The log is kept after exit, after `terminal_close` and after
  `DELETE /api/terminals/{id}`, for the life of the instance. The id match is
  exact, so an upper-case form of a lower-case id has no log.
- The cap is 1 MiB per terminal. Past it, the log keeps the first 64 KiB and
  the newest bytes.
- Bytes are added to the log before their `terminal_output` frame is sent.
- Event `terminal.log` (`terminal_id`) in `relaysessions.log`. A 404 is
  `error`/`not_found` with error text `Not Found`; a 400 is `error`/`invalid`.

Terminal templates. Shape (omit empty keys):

```json
{"id":"shell","name":"Shell","command":"/bin/zsh","args":["-l"],
 "env":{"TERM":"xterm-256color"},"description":"A login shell","icon":"terminal",
 "idleTimeout":1440,"persist":false}
```

Other keys relay accepts are `env_passthrough`, `sandbox`, `read`,
`read_write`, `deny` and `model_key`. fakerelay stores and returns them
unchanged and applies none of them.

| Route | Request | Success | Errors |
|---|---|---|---|
| `GET /api/terminal/templates?project=<id>` | none | 200, array of templates | |
| `GET /api/terminal/templates/{id}` | none | 200, template | 404 `{"error":"template not found"}` |
| `POST /api/terminal/templates` | template | 201, the template | 400, 409 |
| `PUT /api/terminal/templates/{id}` | template | 200, the template, `id` from the path | 400, 404 |
| `DELETE /api/terminal/templates/{id}` | none | 204 | 404 |

- The list is for one project, named by the `project` query parameter.
  Without it, or with an unknown id, the list is `[]`. A console project gets
  the console templates its `allowed_templates` permits. A host project gets
  its host's templates, and never the console's. Both lists are sorted by `id`.
- An `id` must match `^[A-Za-z0-9][A-Za-z0-9._-]*$`. Else 400
  `{"error":"template id must be letters, digits, '.', '_' or '-'"}`.
- A duplicate id on create answers 409 `{"error":"template \"<id>\" already exists"}`.
  A missing id on update or delete answers 404
  `{"error":"template \"<id>\" not found"}`.
- A name that is empty, or other validation failures, answer 400
  `{"error":"<text>"}`.

Events: `template.list`, `template.get`, `template.create`, `template.update`,
`template.remove`.

checked against relay at `59cdf9e` (G4).

### Models (G4)

`GET /api/models` (class `proxy`) answers 200:

```json
{"models":[{"label":"Claude Haiku","value":"haiku","group":"Claude",
            "provider":"claude","supportsPermissions":true,"supportsAttachments":true}],
 "providerSettings":{"claude":[],"codex":[],
   "pi":[{"key":"thinkingLevel","label":"Thinking Level","type":"select","default":"medium",
          "options":["off","minimal","low","medium","high","xhigh"],"hint":"…"}],
   "chat":[{"key":"useRelayTools","label":"Use Relay Tools","type":"boolean",
            "default":false,"hint":"…"}]}}
```

- Each row has `label`, `value`, `group`, `provider`, `supportsPermissions`
  and `supportsAttachments`. Both flags are always present.
- The flags derive from `provider`:

| `provider` | `supportsPermissions` | `supportsAttachments` |
|---|---|---|
| `claude` | true | true |
| `chat`, `openai`, `ollama`, `pi` | false | true |
| any other, including `codex` | false | false |

- `value` is unique. A client resolves a model by the first row with that
  `value`.
- `providerSettings` maps a provider to a list of fields
  `{key,label,type,default,min?,max?,step?,options?,placeholder?,hint?}`.
  `type` is `number`, `boolean`, `string`, `string[]` or `select`. fakerelay
  returns the four keys above, and may leave the field lists as shown.
  The `pi` and `chat` hints are relay's words: "Reasoning depth for models that
  support it. xhigh is OpenAI codex-max only." and "Let this chat session call
  relay's own tools (email, calendar, ...)."
- A message id in an `llm_event` is `msg_` and 16 random hex digits, as relay's
  agents give. The `stats` of a finished turn also carry `timeToFirstToken` and
  `tokensPerSecond`, fixed non-zero values here where relay measures them.
- `eve list` with no passkeys prints `no eve passkeys reported`. `eve revoke`
  prints `eve passkey ID: revocation pending` and two indented lines. Its
  refusals read `error: bridge error (code -32603): <message>`.
- Claude's rows `haiku`, `sonnet` and `opus` are fixed in relay. A world
  models list adds to them.

Event: `model.list`.

### MCPs

`GET /api/mcps` (class `read`) answers 200, an array that is `[]` when none:

```json
[{"id":"fsmcp","display_name":"fsMCP"}]
```

Only these two keys.

`GET /api/mcps/{id}/tools` (class `read`):

| Case | Status | Body |
|---|---|---|
| Known and connected | 200 | `[{"name":"fs_read","description":"…","category":"Fs"}]` |
| Unknown or not connected | 404 | `{"error":"MCP not registered or not connected"}` |
| No tool provider | 503 | `{"error":"tool list not available"}` |

A tool row has `name`, and `description` and `category` when set. Relay
returns no input schema here. `category` is the tool's own category, else the
part of the name before the first `_` with an upper-case first letter
(`fs_read` gives `Fs`), else empty. A connected MCP with no tools answers 200
`[]`.

Events: `mcp.list`, `mcp.tools.list`.

checked against relay at `59cdf9e`.

### Eve passkey doors

All four are described in `eve-passkey-enrolment.md`. The bodies:

| Route | Request | Success | Errors |
|---|---|---|---|
| `GET /api/eve/passkey-enrolment` | none | 200 `{"open":true,"expires":"2026-10-09T10:05:00Z"}` or `{"open":false}` | |
| `POST /api/eve/passkey-enrolment/consume` | `{"ip":"203.0.113.9","label":"Phone"}` | 200 `{"expires":"…"}` | 409 `{"error":"eve passkey enrolment is not open"}`, 400 |
| `PUT /api/eve/passkeys` | `{"passkeys":[{"id","label","created","last_used"}]}` | 200 `{"revocations":["id1"]}` | 400, 500 |
| `GET /api/eve/passkeys/revocations` | none | 200 `{"revocations":["id1"]}` | |

`revocations` is always an array. `expires` is RFC 3339.

### Audit (G4)

`GET /api/audit` (class `read`) answers 200 with a bare JSON array of audit
rows, newest first. It is `[]` when auditing is off or nothing matches.

Query parameters, all optional:

| Parameter | Match |
|---|---|
| `project_id` | `actor.project_id` equals the value |
| `mcp_id` | `mcp_id` equals the value |
| `outcome` | `outcome` equals the value. Valid: `ok`, `error`, `tool_error`, `denied`, `unauthorized`, `throttled`, `pending`, `scope_violation` |
| `event` | `event` equals the value, such as `file_op` or `control_decision` |
| `kind` | `actor.kind` equals the value. Valid: `project`, `service`, `remote`, `unknown`, `relay`, `control`, `operator` |
| `text` | case-insensitive substring over `tool`, `mcp_id`, `error`, `actor.project_name`, `actor.proc`, `actor.parent`, `args`, `method`, `path`, `class`, `transport`, `actor.cred_id`, `credential`, `subject`, `subject_name`, `via` and `grants` |
| `limit` | integer, at most this many rows. Default 200. 0 means the default. Negative is invalid |
| `deep` | boolean (`true`, `false`, `1`, `0`). Read the on-disk log instead of the in-memory ring |

All given parameters must match. Errors are the Simple family:

| Status | `error` |
|---|---|
| 400 | `limit: "x" is not an integer` |
| 400 | `deep: "x" is not a boolean` |
| 400 | `limit must be >= 0, got -1` |
| 400 | `unknown outcome "x"` |
| 400 | `unknown actor kind "x"` |

The `kind` list does not include `project_session`. A query for it answers
400 even though rows with that kind exist in real relay.

`GET /api/audit/log` answers 200 `{"path":"<path of the JSONL file>"}`.

A row is the audit row shape. Keys by event:

| Key | Notes |
|---|---|
| `id`, `ts`, `dur_ms`, `event`, `actor`, `outcome` | always present. `ts` is RFC 3339 with milliseconds |
| `actor` | `kind`, `auth`, and where set `project_id`, `project_name`, `cred_id`, `pid`, `proc`, `parent`, `session_id`, `service_id`, `client_id`, `fingerprint`, `remote_addr` |
| `phase`, `mcp_id`, `tool`, `args`, `args_bytes`, `error`, `mcp_root`, `method`, `path`, `class`, `transport` | omitted when empty |
| `scope` | always present, `null` when not set |

The rows fakerelay writes or seeds are `file_op`, `control_decision` and the
world's verbatim rows. Row shapes for `file_op` and for presence refusals are
above. Events: `audit.query`, `audit.path.get`.

checked against relay at `59cdf9e` (G4).

### relay grant --json (G7)

`relay grant [--project X] [--json]` prints a JSON array, indented two
spaces, with a trailing newline. Each element is one project or access
profile, sorted by `name`:

```json
[
  {
    "id": "p_acme",
    "name": "Acme",
    "kind": "project",
    "path": "/home/acme/app",
    "mcps": [
      {"mcp": "fsmcp", "access": "read", "outbound": "blocked", "tools": "all tools",
       "scope": {"allowed_dir": "\"/home/acme\""}}
    ]
  }
]
```

| Key | Rule |
|---|---|
| `id`, `name`, `kind` | always. `kind` is `project`, or `access profile` for `kind: remote` |
| `path` | omitted when empty |
| `mcps` | always an array. One element per granted MCP, sorted by `mcp`. `*` expands to every MCP |
| `mcps[].mcp` | the MCP id |
| `mcps[].access` | `read` or `write` |
| `mcps[].outbound` | `blocked` or `allowed` |
| `mcps[].tools` | text: the joined allowed patterns, `all tools`, `all tools except <n>` or `no tools` |
| `mcps[].scope`, `mcps[].warnings` | omitted when empty. `scope` maps a field to its JSON text |
| `mounts` | omitted when empty. Elements: `{id,access,path,warnings?}` |
| `enrolments` | omitted when empty. Elements: `{client_id,cli_admin}` |

Exit and output edge cases:

- `--project` matches the id or the name. No match exits 1 with
  `error: no project or access profile matching "<x>"` on stderr.
- With no records at all, the command prints the text
  `no projects or access profiles` on stdout and exits 0, **even with
  `--json`**. The output is then not JSON.

checked against relay at `59cdf9e` (G7).

### Gaps and sources

| Gap | Covers | Source of the shape | Checked against relay code |
|---|---|---|---|
| G1 | file route bodies and errors | the eve file-plane contract, equal to relay's file routes | yes, `59cdf9e` |
| G2 | `/ws/files` frames | the same contract and relay's watch hub | yes, `59cdf9e` |
| G3 | `/ws` session and terminal frames | `session-host.md` for `session_state` and `turn_done`; the session host handlers for the rest. relayLLM's event protocol is not available | yes, `59cdf9e` |
| G4 | session and terminal rows, models, templates, `/api/audit` | session host list handlers, template routes, audit routes | yes, `59cdf9e` |
| G5 | `RegisterManifest` envelope | `service-manifest.md` for the flow; the bridge types for names and refusals | yes, `59cdf9e` |
| G6 | presence refusal body | the project routes' gate error mapping | yes, `59cdf9e` |
| G7 | `relay grant --json` | the grant command | yes, `59cdf9e` |
| G8 | the terminal log route, how an echo terminal ends, the session-host 404 for unmatched paths, drop-in and the headless/agent launch settings | `routes.md`, `session-host.md` and `events.md` for the doors; the planner's check of relay for the bodies and messages the docs leave out | yes, `5af6c45` |

Other entries marked "checked against relay" (projects, hosts, Chief of
Staff, door refusals, MCP tools, persistent sessions) fill smaller holes the
docs leave: the field list is in code, not in a doc.

### Where docs and code disagree

These are findings for the real-relay parity work. fakerelay follows the code
(the second column) unless a row says otherwise. Each was checked against
relay at `59cdf9e`.

| # | Docs say | Code does |
|---|---|---|
| D1 | `tokens.md`: a class or scope refusal is "403 `outside chief-of-staff scope`", "403 `class not granted`" | the body is the plain text `Forbidden` (and `Unauthorized` for 401). The reason is only in the `control_decision` row's `error` |
| D2 | the outer 401 and the per-route 401 look alike | the outer gate writes `unauthorized`, a route's class check writes `Unauthorized`, both text with a trailing newline |
| D3 | eve `api.md`: the `resume_required` error frame has "no `message`" | the frame carries `message` (`session: provider not running; resume required`). `dropped_in` does too |
| D4 | `session-host.md`: `already_processing` is a coded answer | on `/send` and over HTTP it is a code. On `/ws`, `send_message` while a turn runs sends an `error` frame with only `message` (`session: already processing a message`), no `code` |
| D5 | the fake contract: `message_complete`, then `turn_done` and `session_state` | for a tracked session the attention frames go first: `session_state` `running`, then `turn_done`, `session_state` `idle`, and `message_complete` last |
| D6 | the fake contract: `session_ended` comes with `end_session` | `end_session` stops the provider and sends no `session_ended`. Only `delete_session` broadcasts it. A tracked session's `session_state` becomes `ended` |
| D7 | `service-manifest.md`: `/launch` and `/terminate` are the reserved session-host routes | five are reserved: `/launch`, `/terminate`, `/handoff`, `/handback`, `/send`, and anything nested under them |
| D8 | the fake contract: a manifest needs an absolute socket | relay checks only that `internalSocket` is not empty |
| D9 | the fake contract fault table: `bad_gateway` is 502 `{"error":"upstream unreachable"}` | an unreachable dispatch upstream writes the text `bad gateway` |
| D10 | eve file-plane contract: hosts overwrite on rename and move | the owner's decision landed: both backends refuse an existing destination with 409 `EEXIST`. `project-files.md` already says so. The fake contract's host row in "Fake semantics" must follow the code |
| D11 | `project-files.md`: console `git` runs under `sandbox-exec` | not reproducible off macOS. fakerelay runs the same allowlist without it |
| D12 | the fake contract: `GET /api/terminal/templates` returns "the host's templates" for a host project | the list is for the project in the `project` query parameter. Without it the list is empty. Host templates also have their own CRUD at `/api/hosts/{id}/templates` |
| D13 | the fake contract names `probe`, `disconnect` and file routes with one class | probe and disconnect are class `configure`. A comment in relay's code says `execute`; the registration is `configure`, and `ssh-hosts.md` says so |
| D14 | the world host `probe` example has no `at` | a relay probe always has `at`. fakerelay should fill it when the world omits it |
| D15 | `auditValidKinds` vs `audit-log.md` | `--kind project_session` is documented in places but the query filter refuses it |
| D16 | `relay grant --json` implies JSON always | with no records it prints plain text and exits 0 |
| D17 | tool listing in the fake contract carries `inputSchema` | `GET /api/mcps/{id}/tools` returns only `name`, `description` and `category`. The input schema stays in the catalogue |
| D18 | the world's `chief_of_staff` uses snake_case | the HTTP view and body use camelCase (`projectId`, `dailyModelCalls`). Error `message` texts use snake_case names (`project_id is required`) |
| D19 | world projects carry an `id` | `POST /api/projects` accepts no `id`. The id is generated, and a create needs an absolute `path` |
| D20 | relay-sessions answers a known path with the wrong method with 405 | fakerelay answers it with the session host's 404, `404 page not found`, as for any unmatched path under `/api/sessions/` or `/api/terminals/`. The 405 is not faked |

## World spec

The world is `DIR/world.json`. `serve` reads it once. A missing file is the
empty world. An unknown key or an invalid value makes `serve` exit 1 before
ready, with `error: world.json: <json path>: <problem>`.

Records use relay's `settings.json` field names where one exists. Snake_case
throughout, including `chief_of_staff`. The HTTP view of Chief of Staff is
camelCase (D18). A world project carries an `id`, which is fine for a seed.
A project created over HTTP gets a generated id (D19).

Schema 1:

| Key | Shape | Default |
|---|---|---|
| `schema` | `1` | required |
| `listeners` | `{api, model}`: loopback `host:port`, or `""` for off | `api: "127.0.0.1:0"`, `model: ""` |
| `credentials[]` | `{id, name, classes[], token, expires?}`, a bearer for socket or TCP | none |
| `presence` | `{<op>: "approve"\|"deny"\|"timeout"}` for `project.grant`, `eve.enrolment.open`, `eve.passkey.revoke` | an absent op is deny |
| `faults[]` | a fault, see Faults | none |
| `projects[]` | `id, name, path, kind, host_id, mode, files_read_only, allowed_mcp_ids, allowed_models, allowed_templates, chat_templates, permission_policy`, plus `files` and `repos` | `path ""` is `DIR/projects/<id>` |
| `projects[].files` | `{rel: string \| null (dir) \| {"base64"} \| {"symlink": target}}`, written after `repos`, so it sets the worktree state | |
| `projects[].repos[]` | `{dir, branch, commits: [{message, files}]}`. Real `git init`, `add` and `commit` | |
| `host_path` | the `PATH` a host probe looks `node`, `claude` and `tmux` up on | fakerelay's own `PATH` |
| `default_project` | `{home, work}` | absent |
| `chief_of_staff` | `{project_id, model, daily_model_calls}` | absent |
| `hosts[]` | `id, name, target, port, identity_file, probe (with node_version and claude_version), tmux_path, terminal_templates`, plus the fake fields `root`, `agent`, `persistent_sessions` | `root ""` is `DIR/hosts/<id>`. `agent "none"` |
| `mcps[]` | `{id, name, transport, catalogue}`, plus `command`, `args` (stdio) or `url` (http). `catalogue` is the e2e `Catalogue` JSON exactly. `command`, `args` and `url` are only what `mcp list` prints in `ENDPOINT`; fakerelay never starts them | `ENDPOINT` is `-` |
| `models[]` | a `/api/models` row `{value, label, group, provider}`, plus `reply` | `reply {"kind": "echo"}` |
| `terminal_templates[]` | relay's terminal template | none |
| `sessions[]` | `{id, project_id, name, model, state: "idle"\|"dormant", messages: [{role, text}]}` | |
| `eve` | `{passkeys[], revocations[], enrolment_open: false}` | |
| `audit[]` | verbatim audit rows, appended at start | |
| `services[]` | `id, name, command, args, env, working_dir, capabilities, autostart` | |

Rules:

- A host project's files live at `<host root><project path>`. In the example
  below that is `DIR/hosts/h_box/home/acme/app`.
- A host `probe` may omit `at`. fakerelay fills it with the clock's time,
  because a real probe always has `at` (D14).
- `reply.kind` is one of:
  - `echo`: the answer is `echo: <text>`.
  - `text`: the answer is the fixed `text`.
  - `permission`: raises a `permission_request` for `tool`, then echoes on
    approve.
  - `fail`: `message_complete` with `isError: true`.
- `agent` is the host's initial `host_status`: `none`, `connecting`,
  `connected` or `unreachable`.
- `mcps[].catalogue` keeps `inputSchema` for each tool. `GET
  /api/mcps/{id}/tools` does not return it (D17).
- `eve.enrolment_open: true` starts with the passkey enrolment window open.

Example:

```json
{"schema":1,
 "credentials":[{"id":"c1","name":"acme-ops","classes":["read","configure"],"token":"test-token-acme-0001"}],
 "presence":{"project.grant":"approve"},
 "projects":[{"id":"p_acme","name":"Acme","mode":"work",
   "repos":[{"dir":"","branch":"main","commits":[{"message":"initial","files":{"README.md":"# Acme\n"}}]}],
   "files":{"README.md":"# Acme changed\n","src/":null,"src/a.js":"// TODO x\n","link":{"symlink":"README.md"}}},
  {"id":"p_box","name":"Box app","host_id":"h_box","path":"/home/acme/app","files":{"main.go":"package main\n"}}],
 "hosts":[{"id":"h_box","name":"testbox","target":"acme@testbox",
   "probe":{"at":"2026-10-09T10:00:00Z","ok":true,"os":"Linux","arch":"x86_64","home":"/home/acme","shell":"/bin/bash","node_path":"/usr/bin/node","claude_path":"/usr/bin/claude","tmux_path":"/usr/bin/tmux"},
   "agent":"connected","terminal_templates":[{"id":"shell","name":"Shell"}],
   "persistent_sessions":[{"name":"relay-p_box000-shell-1","created":1789000000,"attached":0}]}],
 "models":[{"value":"claude-haiku-5-5","label":"Haiku","group":"Claude","provider":"claude"}],
 "mcps":[{"id":"fsmcp","name":"fsMCP","transport":"stdio","catalogue":{"tools":[{"name":"fs_read","inputSchema":{"type":"object"}}]}}],
 "services":[{"id":"eve","name":"Eve","command":"/usr/bin/node","args":["server.js"],"working_dir":"/tmp/fr.Ab12/eve","capabilities":["frontend"],"autostart":true}]}
```

## Instance and ready file

- **Serve:** `fakerelay --config-dir DIR serve`, or `RELAY_CONFIG_DIR=DIR`.
  The grammar is `relay serve`'s.
- **No default dir.** Every verb refuses to run without a config dir, so
  fakerelay never touches a live relay's dir.
- **Lock:** `flock` on `DIR/serve.lock`. A second serve on the same DIR fails
  with relay's message.
- **Socket paths:** at most 103 bytes, else relay's message.
- **Start-up order:**
  1. Bind the sockets and listeners.
  2. Start the autostart services.
  3. Write `ready.json` (mode 0600, atomic).
  4. Print its path as the only stdout line.
  5. Write the `server.ready` event.
- **Shutdown:** SIGTERM or SIGINT removes `ready.json`, stops the services
  (SIGTERM, then SIGKILL after 10 s), closes the sockets and exits 0.
- **Exit codes:** a start failure exits 1 and names DIR. A usage error exits 2.

`ready.json`:

```json
{"schema":1,"pid":4242,"version":"fakerelay","config_dir":"/tmp/fr.Ab12/a",
 "sockets":{"bridge":"/tmp/fr.Ab12/a/relay.sock","frontend":"/tmp/fr.Ab12/a/relay-frontend-4242.sock","control":"/tmp/fr.Ab12/a/fakerelay-control.sock"},
 "listeners":{"api":"127.0.0.1:53001"}}
```

This is relay's schema-1 shape plus `sockets.control`. A listener that is not
bound has no key. A client that waits for the instance waits for the stdout
line, or for `ready.json` to appear. It never sleeps.

Everything the instance owns sits under DIR:

- `logs/relay.log`, `logs/relaysessions.log`, `logs/<service-id>.log`,
  `logs/audit/toolcalls.jsonl`
- `projects/`, `hosts/`, and `trash/` (console deletes)
- `fakes/<name>.jsonl`, the call logs of the in-process fakes
- `home/`. Git runs with `HOME` set to it and `GIT_CONFIG_NOSYSTEM=1`.

Nothing is shared across instances. Every TCP port is `:0` unless the world
names one. Any number of instances run at once, each in its own DIR.

## CLI

The grammar, the `--json` shapes and the exit codes are relay's. Global flags
are `--config-dir` and `--trace`.

| Verb | Notes |
|---|---|
| `serve` | see above |
| `logs [--json] [--event KEY] [--since T] [--follow [--timeout D]]` | reads the files. `--follow` wakes on the server's append notice on the control socket, never on a timer. With no server it waits out its own `--timeout`. Without `--event`, `--follow` prints matches as they are added, until `--timeout` or a signal. With `--event KEY`, it prints the first match, one already written or a new one, and exits 0, as `docs/cli.md` says. A client that needs the first line a predicate accepts subscribes to `GET /v1/follow` and re-reads `logs --json --event KEY` on each notice |
| `audit [--tail N] [--project ID] [--event E] [--outcome O] [--json] [--path]` | reads the file |
| `grant [--project X] [--json]` | see "relay grant --json" |
| `project update --id ID --files-read-only=true\|false` | |
| `project create (--name N --path P \| --file F) [--json]` | the core of `POST /api/projects`, gated by `project.grant`. Text: `created project NAME (ID)`; `--json`: the 201 project view on one line. Both forms, or neither, exit 1 (`--file cannot be combined with --name or --path`, `pass --name and --path, or --file`) |
| `project edit --id ID --file F [--json]` | the core of `PUT /api/projects/{id}`. Gated as `PUT` is: a widening body asks `project.grant`, a narrowing one does not. Text: `updated project NAME (ID)`. Missing `--id` or `--file` exits 1 (`--id is required`, `--file is required`) |
| `eve enrol`, `eve list`, `eve revoke --id ID` | `eve enrol` is gated by `eve.enrolment.open`. `eve revoke` is gated by `eve.passkey.revoke` and refuses the last passkey (exit 1), as relay does |
| `service list`, `service restart --id ID\|--name N`, `service stop --id ID\|--name N [--json]` | `stop` ends the service process and prints `stopped service "ID"` (`{"id"}` with `--json`) |
| `mcp list` | `ENDPOINT` is a stdio MCP's command with its arguments, an HTTP MCP's URL, else `-` |
| `ctl ...` | fake-only, see Control socket |

Rules:

- Every verb except `logs` and `audit` forwards over `POST /v1/verb`. With no
  server it exits 1 with relay's refusal word for word:
  ``error: relay is not running at DIR; `relay <verb>` requires the service.``
  plus its five indented lines. `<verb>` is the words before the first flag.
  It does not create DIR.
- A relay flag fakerelay does not support exits 2:
  `error: fakerelay does not support --X`.
- Any other verb exits 2 and points to this file.
- `project create` and `project edit` read `--file F` in the CLI process (`-` is
  stdin, up to 8 MiB) and make a relative `--path` absolute against the CLI's
  working directory; the service has neither. A file that cannot be opened,
  read or is too large exits 1 (`error: open F: ...`, `error: read F: ...`,
  `error: F is larger than 8388608 bytes`). The body rides in `POST /v1/verb`
  as `body`; argv keeps the absolute `--path` and `--file F`. A core refusal
  exits 1 as `error: bridge error (code -32603): MESSAGE`, a presence denial
  with `presence was refused` and a denied `control_decision` row with
  `via: cli`.
- relay has no `project list` verb. A client reads projects with
  `grant --json` and `logs --event project.create`.
- Verbs that relay adds later are added here after they land.

## Control socket

The control socket is `sockets.control` in `ready.json`. It speaks HTTP and
JSON, with file mode 0600. Every route sits under `/v1/`. It carries no
credential and is not on any TCP listener. `ctl` is the client.

| Endpoint | `ctl` verb |
|---|---|
| `GET /v1/state` | `ctl state [--json]` |
| `PUT /v1/presence` `{op: outcome}`, replaces the whole map | `ctl presence OP=OUTCOME...` |
| `POST /v1/faults` gives 201 `{id}`; `DELETE /v1/faults[/{id}]`; `POST /v1/faults/{id}/release` | `ctl fault add\|clear\|release` |
| `PUT /v1/hosts/{id}/status` `{status, error}` | `ctl host status --id --status [--error]` |
| `POST /v1/projects/{id}/fs-events` `{path, kind}` gives `{delivered}` | `ctl fs-event --project --path [--kind]` |
| `GET` and `POST /v1/clock` (`set`, or `advance_ms`) | `ctl clock show\|set\|advance` |
| `POST /v1/verb` `{argv, trace, body}` gives `{code, stdout, stderr}`; `body` is base64 bytes, the file of `project create\|edit` | the CLI forwarder |
| `GET /v1/follow`: NDJSON `{file, size}` per append | inside `logs --follow` |

- `ctl state` returns the live state. `--json` prints it as JSON.
- `ctl host status` answers only after the `host_status` frame has gone to
  every `/ws/files` connection.
- `ctl fs-event` answers `{delivered}`, the number of connections that got the
  frame.
- The clock moves Eve window expiry and credential `expires`.

## Faults

A fault is `{id?, route, mode, times?, delay_ms?, name? | (status, body)}`. The
world's `faults[]` and `POST /v1/faults` take the same shape. The response to
`POST` is 201 `{"id":"…"}`.

- **`route`** is one of:
  - a registered pattern, such as `POST /api/projects`;
  - `*`, every frontend and TCP route;
  - `BRIDGE <Type>`, such as `BRIDGE Hello`;
  - `PROXY <manifest prefix>`, a dispatched manifest route.
- **`down`:** the connection is closed with no answer. On a WebSocket route it
  also closes that route's open connections.
- **`slow`:** the request is held until `release`, or for `delay_ms` when
  given. Each hold writes `fakerelay.fault` with `action: "held"`.
- **`error`:** the explicit `status` and `body`, or a `name` from the table.
- **`times`:** the number of applications. `0`, or absent, holds until cleared.

Named errors:

| Name | Status | Body |
|---|---|---|
| `HOST_UNREACHABLE` | 503 | `{"error":"host is not connected","code":"HOST_UNREACHABLE"}` |
| `TIMEOUT` | 504 | `{"error":"timed out","code":"TIMEOUT"}` |
| `AUDIT_UNAVAILABLE` | 503 | `{"error":"audit log unavailable","code":"AUDIT_UNAVAILABLE"}` |
| `ERROR` | 500 | `{"error":"internal error","code":"ERROR"}` |
| `unavailable` | 503 | `{"error":"service unavailable"}` |
| `bad_gateway` | 502 | text `bad gateway`, as in the door refusal (D9) |
| `presence_refused` | 403 | `{"error":"presence was refused"}` |
| `not_found` | 404 | `{"error":"not found"}` |

- `BRIDGE Hello` with mode `error` answers `hello refused`.
- The fault and the class check run before the route handler.
- Every application writes `fakerelay.fault` with `fault_id`, `route`, `mode`
  and `action` (`applied`, `held` or `released`).
- A client waits for a hold on the `held` event, read by
  `logs --follow --event fakerelay.fault`.

## Presence

The world's `presence` map, or `ctl presence`, sets the outcome for each gated
op. The ops are `project.grant`, `eve.enrolment.open` and
`eve.passkey.revoke`. An absent op is `deny`.

| Outcome | Result |
|---|---|
| `approve` | the op runs |
| `deny` | 403 `{"error":"presence was refused"}`. The op's event has status `denied` and reason `presence_refused`. One `control_decision` row with `outcome: "denied"` is written (see Presence refusal) |
| `timeout` | the request is held until the caller leaves. There is no response. The op's event has reason `presence_timeout` |

- Every answer writes `debug.presence.answer` with `presence_op` (the gated op; `op` is
  the event name on every event line) and `answer`. A client
  that waits for a timeout waits for that event with `answer: timeout`.
- The prompt is not shown anywhere. The outcome is the whole gate.
- The `eve enrol` verb and `POST /api/projects` go through the gate. So does a
  widening `PUT` (see Projects).
- A change to the map takes effect on the next request.

## Launch

fakerelay starts `services[]` with `autostart: true`, in the order listed, as
relay does. It follows `launch-identity.md` section 1.

1. Generate 32 random bytes as 64 lowercase hex. Write them to a pipe and
   close the write end.
2. Pass the read end as `ExtraFiles[0]` (fd 3) with `RELAY_LAUNCH_FD=3`.
3. Close the parent's read end after `Start`.

The child's environment, in order:

1. fakerelay's own environment, minus `RELAY_SERVICE_TOKEN`,
   `RELAY_MCP_TOKEN`, `RELAY_FRONTEND_TOKEN` and `RELAY_LAUNCH_FD`.
2. The record's `env`.
3. `RELAY_BRIDGE_SOCKET`, `RELAY_SERVICE_ID`, `RELAY_LAUNCH_FD=3`,
   `RELAY_CONFIG_DIR=DIR` and `RELAY_MCP_COMMAND` (the fakerelay path).
4. `RELAY_FRONTEND_SOCKET`, exactly when the record holds `frontend`.

The process:

- runs directly, with no `$SHELL -l -c`, in `working_dir`;
- writes stdout and stderr to `logs/<id>.log`;
- on Linux gets `Pdeathsig` SIGTERM, so a killed fakerelay leaves no orphan.

Hello rules, compared in constant time:

- A wrong secret does not spend the launch.
- Success spends it and binds the peer pid.
- Refused: a second Hello, a peer that already holds an identity, no live
  launch, or an unreadable peer.
- The identity ends when the process exits.
- `service restart` mints a fresh launch.
- The peer pid is read with `SO_PEERCRED` on Linux and `LOCAL_PEERPID` on
  macOS.

A service is ready on its `bridge.hello` or `service.manifest.register` event.
An exit that nobody requested writes `service.state` with phase `failed` and
the exit code. There is no restart supervision.

## Host simulation

- The host's `root` folder stands in for the remote machine.
- `host_status` uses the documented states `connecting`, `connected` and
  `unreachable`, with `error`.
- With `agent: "none"`, the first file request or watch on a host project
  moves the host to `connecting`, then `connected`.
- `ctl host status` sets any state. The response comes after the frame has
  gone to every `/ws/files` connection.
- `/ws/files` sends one `host_status` per held agent right after the upgrade.
- While a host is not `connected`, file ops on its projects answer 503
  `HOST_UNREACHABLE`, and a watch answers `watch_error`.
- `POST /api/hosts/{id}/disconnect` moves the host to `unreachable` with
  error `disconnected`.
- `POST /api/hosts/{id}/probe` probes again, as relay does. It is class
  `configure`, like `disconnect` (D13). Its result:
  - host `unreachable`: `ok: false` and `error: "ssh: connect to host
    <target> port <port or 22>: Connection refused"`, with no other field;
  - otherwise `ok: true`. `os`, `arch`, `home` and `shell` keep the world
    probe's values. `node_path`, `claude_path` and `tmux_path` come from a
    lookup on `host_path` (the world's, else fakerelay's own `PATH`), with
    `node_version` and `claude_version` from `--version`. A tool the lookup
    misses keeps the world probe's value.
  - `POST /api/hosts` runs the same probe once.
  - A successful probe of a host with no templates seeds `shell`/`Shell`,
    and `claude-code`/`Claude Code` with `command` set to the probed
    `claude_path` when there is one (`ssh-hosts.md`).
- `hostView.status` comes from state:
  - agent `connected`: `connected`;
  - probe ok: `idle`;
  - probe failed: `unreachable`;
  - no probe: `unknown`.
- `GET /api/hosts/{id}/templates` and the host template CRUD are separate from
  `GET /api/terminal/templates`, which takes `?project=<id>` (D12).
- Host file ops run on the real folder under `root`. Stream ignores `Range`.
  Delete is permanent.

## Fake semantics

Where fakerelay differs from relay on purpose:

- **Echo agent and echo terminals.** An agent answers per the model's `reply`.
  Terminal input comes back as output, with `\r` becoming `\r\n`. A terminal
  ends on `\x04` at an empty line or on an `exit N` line (see Echo terminal).
  Drop-in starts an echo terminal, not a Claude process.
- **Events come before the answer, sessions included.** For example,
  `chat.turn` is written before `message_complete`.
- **Turn order.** For a tracked session the order is `user_message`,
  `session_state` running, `llm_event`s, `stats_update`, `turn_done`,
  `session_state` idle, then `message_complete` last (D5). A client's turn
  ends at `message_complete`.
- **`end_session`** stops the provider and sends no `session_ended`. Only
  `delete_session` does (D6).
- **`fs_event` comes after a mutating response, or from `ctl fs-event`.** Disk
  is not watched.
- **Console delete moves the entry to `DIR/trash`.**
- **Rename and move refuse an existing destination** with 409 `EEXIST`, on
  console and host projects alike (D10). A case-only rename of the same entry
  is allowed.
- **Git keeps relay's argument allowlist and fixed `-c` prefix.** It runs
  without `sandbox-exec` (D11).
- **Identity is bound to the peer pid**, not to pid plus pidversion.
- **Manifest registration** checks that `internalSocket` is not empty, not that
  it is absolute (D8). Five route roots are reserved: `/launch`,
  `/terminate`, `/handoff`, `/handback` and `/send` (D7).
- **An unreachable dispatch upstream** answers the text `bad gateway` (D9).
- **Class and scope refusals** answer the plain text `Forbidden` or
  `Unauthorized`. The reason is only in the audit row (D1, D2).
- **`already_processing` on `/ws`** is an `error` frame with no `code` (D4).
- **Audit rows** are `file_op` rows, presence-refusal `control_decision` rows
  and the world's seeded rows. Nothing else is written.

## Not faked

- the remote mTLS and enrolment listeners;
- `model.sock` and the model endpoint, which arrive with the swap below;
- the passkey login page;
- `frontend.request` lines;
- drop-in's `turn_timeout` refusal;
- tool calls through the bridge;
- tasks. A service that registers a manifest for them serves them, and
  fakerelay dispatches to it;
- the tray and the presence prompt UI.

A route not listed in the Surface section is a gap. Report it as a new issue
with the route and the client that needs it.

## Seam for the e2e fakes

The seam is `fakerelay/internal/fakes`. It lets the in-process stand-ins for
an agent, an MCP and a model host be swapped for the real fakes in `e2e/`
(relay#287) without touching a route.

```go
package fakes
type Agent interface { // one per session
    Turn(ctx context.Context, text string, emit func(Frame)) error // returns at turn end
    Answer(permissionID string, approved bool)
}
type AgentFactory func(model world.Model, session SessionInfo) (Agent, error)
type MCPServer interface{ Tools(ctx context.Context) ([]json.RawMessage, error) }
type ModelHost interface{ Models(ctx context.Context) ([]json.RawMessage, error) }
```

What ships now, in process:

- an echo or scripted `Agent`;
- a catalogue-backed `MCPServer` that reads the e2e `Catalogue` format;
- a world-backed `ModelHost`.

All three write the e2e call-log line shape to `DIR/fakes/<name>.jsonl`.

When relay#287 lands, the swap is a follow-on:

1. Add exec adapters that implement the same three interfaces and run the e2e
   fake binaries (`fakeagent`, the MCP fake, `fakemodelhost`).
2. Add `RegisterModelHost` and `model.sock`, so `listeners.model` serves.
3. Keep the routes, frames and `Deps` as they are.

Shapes agreed with the e2e fakes:

1. The fakes' flags are stable under the e2e additive rule.
2. Each runs with only its flags and documented env, never the harness env.
3. `fakeagent` takes its persona from the basename of argv[0].
4. `fakemodelhost` speaks Hello and `RegisterModelHost` exactly as
   `model-endpoint.md` documents.
5. The `Catalogue` and `Call` JSON shapes are shared verbatim.
6. The fakes stay buildable with `go build -C e2e ./fakes/<name>`, because
   `go install @sha` cannot fetch the e2e module. Reuse is by running the
   binaries, never by import.
