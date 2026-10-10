# Relay feature map

What a person uses relay for, the features that serve each goal, and the proof
and test that go with each feature. A row is the contract between this file
and the end-to-end suite: a test writer reads a row, drives the doors it
names, reads the proof it names, and writes the test it names. A PR that adds
or changes a feature updates its row and that test in the same change.

The tables are read by a program as well as by people, so their form is fixed.
[How to read a row](#how-to-read-a-row) is the whole format.

## How to read a row

Every goal below has one table with exactly this header. Only tables with this
header are read as feature rows.

```
| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G1.01` | Example | The observable behaviour, including the refusal. | Settings > Projects | `relay project create` | `http:POST /api/projects` `cli:relay project create` | `project.grant` | `event:project.create=ok#project_id` `event:project.create=denied/presence_refused` `code:http:POST /api/projects#403` | `e2e:TestExample` `deny e2e:TestExampleDenied@http:POST /api/projects` |
```

| Column | Meaning | Read by the program |
|---|---|---|
| ID | Stable row ID, in a code span. | yes |
| Feature | Short name. | no |
| Claim | One sentence: the behaviour a test asserts, including the refusal case. | no |
| Simple door | The screen path an everyday person uses, `none` or `n/a`. | no |
| Power door | The `relay` verb, `settings.json` key path, other config file key, global flag or header a power user uses; `n/a` for background or API-only rows; `exception: <why>` for rows that only a screen reaches. Never empty. | `exception:` only |
| Doors | Every door that reaches the feature, or `none`. | yes |
| Gate | The owner gates the feature passes, or `none`. | yes |
| Proof | What a test reads to know the feature worked or was refused. | yes |
| Test | The tests that prove the row. | yes |

A **door** is anything in `relay doors --json`: an `http` route, an `ipc` op, a
`bridge` request or a `cli` verb. A **gate** is an op in `presence.GatedOps`
(see [Owner gates](#owner-gates)) or `webauthn`.

### Lexing

- A row is one line that starts and ends with `|`.
- Cells split on `|` not preceded by `\`. No door name, key or path contains `|`.
- The items of a cell are the contents of its code spans, in order. Text
  outside a code span is comment and the program ignores it.
- The literal `none` and `n/a` cells hold no code span.
- Fenced code blocks are not read.

### Grammar

```
ID          = "G" goal "." NN                 ; ^G[1-9][0-9]?\.[0-9]{2}$
Doors       = "none" / 1*DoorRef
DoorRef     = CatRef / OffRef
CatRef      = CatKind ":" DoorName            ; DoorName is byte for byte a "name" from relay doors --json
CatKind     = "http" / "ipc" / "bridge" / "cli"
OffRef      = OffKind ":" OffName             ; documented in docs/routes.md; not in relay doors
OffKind     = "proxy" / "ws" / "model" / "enrol" / "remote" / "bg"
Gate        = "none" / 1*GateOp               ; a presence.GatedOps name, or "webauthn"
Proof       = 1*ProofItem
ProofItem   = "event:" EventKey [ "=" Status [ "/" Reason ] ] [ "#" Field ]
            / "audit:" AuditEvent [ "=" Outcome ] [ "#" Field ]
            / "out:" DoorRef "#" Path
            / "code:" DoorRef "#" 1*DIGIT     ; HTTP status for http, proxy, model and enrol refs; exit code for cli
Status      = "ok" / "error" / "denied"
Reason      = snake_case code from docs/events.md section 4, or an event-specific code
Field       = snake_case name of a field of that event line or audit row
Path        = "." / 1*( "." Ident [ "[]" ] )  ; a jq subset over the --json stdout or the response body
Test        = 1*TestItem
TestItem    = [ "deny" SP ] TestRef [ "@" CatRef ]
TestRef     = "e2e:" GoTestName               ; ^Test[A-Z0-9_][A-Za-z0-9_]*$, unique across the e2e module
            / "journey:" JourneyName          ; a devboxverify journey id from the areas list below
            / "pending:#" IssueNumber
            / "ci:" ScriptPath                ; only on the rows listed under Rows proven by CI
            / "screen-only"                   ; only on rows whose Power door is exception:
```

An event key is a first-column key of a table in [`events.md`](events.md)
section 7. An audit event is a name from [`audit-log.md`](audit-log.md). An
`out:` path reads the `--json` output of a verb, or the response body of a
route; its fields are in [`cli.md`](cli.md) and [`routes.md`](routes.md).

### Rules

The coverage check applies these. A miss is a failure.

- **R1** Every door in `relay doors --json` is a `CatRef` in the Doors cell of at least one row. A door may sit in several rows.
- **R2** Every `CatRef` anywhere in this file names a door in that output.
- **R3** Every row has an accepted test: an `e2e:` item, or a `pending:#N` that is still listed. An `exception:` row may have a `journey:` or `screen-only` instead. A row listed under Rows proven by CI may have its `ci:` item.
- **R4** Every `func Test…` in the e2e feature packages is named by an `e2e:` item, and every `e2e:` item names an existing function.
- **R5** A row whose Gate is not `none` has a refusal proof (an `event:` or `audit:` with `=denied`, or a `code:` of 401, 403 or a non-zero exit), and for each gated `cli` or `http` door in its Doors cell a `deny … @<that door>` test item. An `ipc` door is proven through the shared core by the `cli` or `http` deny test. A `bridge` `admin_op:` door is covered by the `cli` verb whose `calls` names it. A row that lists an owner-gated door lists its gate.
- **R6** Every `event:` key is in `events.md` section 7.
- **R7** IDs are unique, match the pattern, and none is on the retired line.
- **R8** Every promise in the [threat-model table](#threat-model-promises) maps to a row that has a refusal proof.
- **R9** Every row, except one proven by CI, has an `event:`, `audit:` or `out:` item. A `code:` item alone never proves a row.

Every `http` door also has a heading in [`routes.md`](routes.md) with its door
name, and every `cli` door a heading in [`cli.md`](cli.md).

### IDs

- `G<goal>.<NN>`, assigned in row order when the row is first written.
- A new row takes the next unused NN in its goal.
- An ID never changes and is never reused. A moved row keeps its ID, whatever section it sits in.
- A removed row's ID goes on the [retired line](#retired-ids).

### Off-catalogue doors

These surfaces are not in `relay doors`. [`routes.md`](routes.md) documents each; the program does not check their names.

| Kind | Surface | Example |
|---|---|---|
| `proxy` | relay-sessions routes behind the `/` mount | `proxy:POST /api/sessions/{id}/message` |
| `ws` | frames on `/ws` and `/ws/files` | `ws:/ws session_state` |
| `model` | the model endpoint | `model:POST /v1/chat/completions` |
| `enrol` | the enrolment listener | `enrol:lodge` |
| `remote` | the mTLS listener's request types | `remote:CallTool` |
| `bg` | background work with no door | `bg:service supervision` |

### Tests that do not exist yet

A row with no end-to-end test yet reads `pending:#N`, where N is the issue that
writes it. Never `none`. The program reads `e2e/coverage/pending.txt`, a list
of issue numbers that only shrinks, and a `pending:#N` counts as a test only
while N is listed. A test issue removes its own number when its rows have
tests. Adding a number weakens a gate and needs the owner.

`journey:` items name an existing devboxverify journey and stay beside the
`e2e:` item. A journey that always reads NOTRUN is noted after the code span.

Priority is per goal: **must-have** means used daily and a silent break
strands the user; **should** means weekly or a break is loud; **later** means
rare.

## Goals

### G1 · Run an agent session in a project — must-have
Intent: start a chat, Claude, pi, Codex or terminal session in a project and work in it.
It worked: the session starts in the project folder, answers, and stops when told; it cannot reach another project.
Why must-have: eve's everyday path; every chat goes through it.
Areas: sessions, sandbox, templates, audit.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G1.01` | Launch a session | A client that holds the execute class starts a chat, claude, pi or codex session in a granted project and gets its id; a launch for an ungranted project is refused. | eve > New Session > New > chat card > Start Chat | `relay session start` | `http:POST /api/sessions` `http:POST /api/sessions/{$}` `cli:relay session start` `bridge:admin_op:session.start` | none | `event:session.launch=ok#session_id` `event:session.launch=denied` `audit:session_launch=ok` | `pending:#290` `journey:session-chat-lifecycle` `journey:model-list-and-completion` `e2e:TestFakeClaudeAnswers` `e2e:TestFakePiAnswers` `e2e:TestFakeCodexAnswers` |
| `G1.02` | Blank-model launch refused | A launch with no model answers 400 and records a refusal row; no session starts. | n/a | n/a | `http:POST /api/sessions` | none | `event:session.launch=denied` `audit:session_launch=error` | `pending:#290` `journey:blank-model-refused` |
| `G1.03` | System-model chat launch refused | A chat launch on a model that relayLLM marks system answers 403; no session starts. | n/a | n/a | `http:POST /api/sessions` | none | `event:session.launch=denied` `code:http:POST /api/sessions#403` | `pending:#290` |
| `G1.04` | List sessions | A caller lists every live session with its attention state; a caller without the class is refused. | eve > Agents board | `relay session list` | `http:GET /api/sessions` `cli:relay session list` `bridge:admin_op:session.list` | none | `event:session.list=ok#count` `code:http:GET /api/sessions#401` | `pending:#290` `journey:session-agent-state` |
| `G1.05` | Message a session | A message sent to a live session is delivered to it; a message to an unknown session id fails. | eve > session > chat input | `relay session message` | `proxy:POST /api/sessions/{id}/message` `cli:relay session message` `bridge:admin_op:session.message` | none | `event:session.message=ok#session_id` `audit:session_message=ok` | `pending:#290` `journey:session-chat-lifecycle` |
| `G1.06` | Stop a session | A stop ends the session and removes it from the list; stopping an unknown id fails. | eve > session > Stop | `relay session stop` | `proxy:DELETE /api/sessions/{id}` `cli:relay session stop` `bridge:admin_op:session.stop` | none | `event:session.delete=ok#session_id` | `pending:#290` `journey:session-chat-lifecycle` |
| `G1.07` | Resume a session | A resume brings back a stopped session with its history; a resume of an unknown or live session id is refused. | eve > New Session > Resume > session | `relay session resume` | `http:POST /api/sessions/{id}/resume` `cli:relay session resume` `bridge:admin_op:session.resume` | none | `event:session.resume=ok#session_id` `audit:session_resume=ok` `audit:session_resume=denied` | `pending:#290` `journey:session-chat-resume` |
| `G1.08` | Start a terminal | A client that holds execute starts a terminal in a granted project from an allowed template; a template the project does not allow is refused. | eve > New Session > New > terminal card | `relay terminal start` | `http:POST /api/terminals` `http:POST /api/terminals/{$}` `cli:relay terminal start` `bridge:admin_op:terminal.start` | none | `event:session.launch=ok#session_id` `event:session.launch=denied` `audit:session_launch=ok` | `pending:#290` `journey:terminal-lifecycle` `journey:terminal-extra-args` |
| `G1.09` | List terminals | A caller lists every terminal with its project and origin. | eve > Agents board | `relay terminal list` | `http:GET /api/terminals` `cli:relay terminal list` `bridge:admin_op:terminal.list` | none | `event:terminal.list=ok#count` | `pending:#290` `journey:terminal-lifecycle` |
| `G1.10` | Read a terminal's log | A caller reads the tail of a terminal's output; an unknown terminal id fails. | eve > Agents board > log | `relay terminal log` | `proxy:GET /api/terminals/{id}/log` `cli:relay terminal log` `bridge:admin_op:terminal.log` | none | `event:terminal.log=ok#terminal_id` | `pending:#290` |
| `G1.11` | Stop a terminal | A stop ends the terminal and removes it from the list; an unknown terminal id fails. | eve > Agents board > Stop | `relay terminal stop` | `proxy:DELETE /api/terminals/{id}` `cli:relay terminal stop` `bridge:admin_op:terminal.stop` | none | `event:terminal.delete=ok#terminal_id` | `pending:#290` `journey:terminal-lifecycle` |
| `G1.12` | List persistent sessions | A caller lists the persistent sessions on a project's SSH host; a project without a host fails. | eve > New Session > New > Remote sessions | `relay terminal persistent-list` | `http:GET /api/projects/{id}/persistent-sessions` `cli:relay terminal persistent-list` `bridge:admin_op:terminal.persistent.list` | none | `event:session.persistent.list=ok#project_id` | `pending:#290` `journey:slow-route-keepalive` |
| `G1.13` | Kill a persistent session | A kill ends one named persistent session on the host; an unknown name fails. | eve > New Session > New > Remote sessions > Kill | `relay terminal persistent-kill` | `http:DELETE /api/projects/{id}/persistent-sessions/{name}` `cli:relay terminal persistent-kill` `bridge:admin_op:terminal.persistent.kill` | none | `event:session.persistent.kill=ok#project_id` | `pending:#290` |
| `G1.14` | Routes behind the session mount | A call to a relay-sessions route through the `/` mount reaches the session host with the caller's class checked; a slow route does not break the next call on the same connection. | n/a | n/a | `http:/` `proxy:GET /api/models` | none | `event:model.list=ok#count` `code:http:/#401` | `pending:#290` `journey:slow-route-keepalive` |
| `G1.15` | Run a command in a sandbox | `relay sandbox <template>` run from a project folder starts the command inside that project's sandbox; a folder that is not a registered project is refused. | none | `relay sandbox <template>` | `cli:relay sandbox` `bridge:SandboxAttach` | none | `event:sandbox.attach=ok#template` `event:sandbox.attach=denied` | `pending:#290` `journey:acme-sandbox-reach` |
| `G1.16` | Drop in to a headless session | A drop-in attaches the operator's terminal to a headless claude session; a tool call from the attached terminal that the session refuses stays refused. | none until eve's Drop in button | `relay drop-in <session-id>` | `http:POST /api/sessions/{id}/drop-in` `cli:relay drop-in` `bridge:DropInAttach` | none | `event:session.drop_in=ok#session_id` `event:session.drop_in=denied` | `pending:#290` `journey:session-drop-in` `journey:session-drop-in-host` `journey:session-drop-in-tool-refused` |
| `G1.17` | Sandbox containment (own project only) | A sandboxed session reads and writes inside its own project and nothing else; a read of another project's folder fails. | n/a | `relay sandbox <template>` | `cli:relay sandbox` `bridge:SandboxAttach` | none | `event:sandbox.attach=ok#project_id` `event:sandbox.attach=denied` `code:cli:relay sandbox#1` | `pending:#290` `journey:acme-sandbox-reach` |
| `G1.18` | Launch audit row, size-capped | Every launch, allowed or refused, writes one `session_launch` row, and a request-supplied string on a row is capped; a body over 1 MiB answers 413 and writes no row. | n/a | `relay audit --event session_launch` | `http:POST /api/sessions` `cli:relay audit` | none | `audit:session_launch=ok` `audit:session_launch=denied` `code:http:POST /api/sessions#413` | `pending:#290` `journey:oversized-launch-audit-capped` |
| `G1.19` | Change a session's permission mode | A mode change restarts or flips a live session to the requested permission mode; an unknown mode is refused. | eve > session > chat input > Toggle plan mode | `relay session mode` | `cli:relay session mode` `bridge:admin_op:session.mode` | none | `event:session.mode=ok#session_id` | `pending:#290` `journey:permission-mode-restart` (always NOTRUN: the world has no hosts) |
| `G1.20` | List and read terminal templates | A caller lists the terminal templates and reads one by id; an unknown id answers 404. | Settings > Templates | `settings.json` `terminal_templates` | `http:GET /api/terminal/templates` `http:GET /api/terminal/templates/{id}` `ipc:list_templates` | none | `event:template.list=ok#count` `event:template.get=ok#template_id` `event:template.get=error/not_found` | `pending:#290` |
| `G1.21` | Add, edit and remove a terminal template | A template change shows in the list; a malformed template is refused. | Settings > Templates > + Add template; row > Edit / Remove | `settings.json` `terminal_templates` | `http:POST /api/terminal/templates` `http:PUT /api/terminal/templates/{id}` `http:DELETE /api/terminal/templates/{id}` `ipc:create_template` `ipc:update_template` `ipc:remove_template` | none | `event:template.create=ok#template_id` `event:template.update=ok#template_id` `event:template.remove=ok#template_id` `event:template.create=error/invalid` | `pending:#290` `journey:terminal-extra-args` |
| `G1.22` | Web chat tool search | A web chat whose relay tools cost more than a tenth of the model's context hides them behind `tool_search` and `call_tool`. | none (turns on by itself) | `sessions/chat.json` `toolSearch` | none | none | `event:chat.turn=ok` | `pending:#290` `journey:chat-tool-search-tokens` |
| `G1.23` | Codex session | A launch on a `codex/<slug>` model starts a codex session; a model outside the project's allowed list is refused. | eve > New Session > Codex model | `settings.json` `terminal_templates` `codex`, `projects[].allowed_templates`; for a host, `hosts[].terminal_templates` `codex` | `http:POST /api/sessions` | none | `event:session.launch=ok#kind` `event:session.launch=denied` | `pending:#290` `journey:session-codex` |
| `G1.24` | Agent state and turn excerpts | A live claude, pi or codex session reports `session_state` and `turn_done` frames on `/ws`; codex reports six of the seven states, never `asking`. | eve > Agents board | n/a | `ws:/ws session_state` `ws:/ws turn_done` | none | `event:session.state=ok` `event:chat.turn=ok` | `pending:#290` `journey:session-agent-state` |
| `G1.25` | Chief of Staff scope: read every session, send marked | A request in the chief-of-staff scope reaches the session list, a read-only `/ws` and one send route; any other door answers 403. | n/a | `X-Relay-Scope: chief-of-staff` header | `http:POST /api/chief-of-staff/messages` | none | `event:chief_of_staff.send=ok#session_id` `audit:control_decision=denied` `code:http:POST /api/sessions#403` | `pending:#290` `journey:chief-of-staff-send` |
| `G1.26` | Chief of Staff start | A start launches an agent in a registered project root or a folder inside it and shows it in the session list with `origin: chief-of-staff`; a start outside the root, in a remote project or a terminal start on a host is refused; a malformed body or field answers 400. | n/a | `X-Relay-Scope: chief-of-staff` header | `http:POST /api/chief-of-staff/sessions` | none | `event:chief_of_staff.start=ok#session_id` `event:chief_of_staff.start=denied` `event:chief_of_staff.start=error/invalid` `audit:session_launch=ok` | `pending:#290` `journey:cos-start` `journey:cos-start-outside-root` `journey:cos-start-host` |
| `G1.27` | Read-only project access for a claude session | A session launched with `readOnlyProjects: true` reads every local project and writes none; a write to a project folder fails, a launch with the option on a non-claude session is refused, and a project add, move or remove ends the session. | n/a | n/a | `http:POST /api/sessions` | none | `event:session.launch=ok#kind` `event:session.launch=denied` `audit:session_launch=ok` `event:session.exited=ok#session_id` | `pending:#290` `journey:cos-read-only-profile` |
| `G1.28` | Session exit recorded | When the session host reports a session gone, relay writes a `session_end` row and the session leaves the list. | n/a | n/a | `bridge:SessionExited` | none | `event:session.exited=ok#session_id` `audit:session_end=ok` | `pending:#290` `journey:session-chat-lifecycle` |

### G2 · Give an agent access to one project and nothing else — must-have
Intent: grant a project its folder, mail account and chosen tools, and nothing wider.
It worked: `relay grant` shows exactly what was granted, and a session in it is refused everything else.
Why must-have: the product's security promise; a silent widening is the worst failure relay has.
Areas: projects, grants, sandbox.

Creating a project and widening a grant are `project.grant`, an owner gate. Journeys cover the gate itself (the harness creates Verify Grant as the owner; a session has no door to it), the effect of an approved grant (preflight, acme-sandbox-reach) and the ungated edits: narrowing that takes effect in a live session, and re-saves that change nothing.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G2.01` | Create a project | An approved create adds the project with a narrow default grant; a create the owner does not approve adds none. | Settings > Projects > + New > Create | `relay project create` | `http:POST /api/projects` `ipc:create_project` `cli:relay project create` `bridge:admin_op:project.create` | `project.grant` | `event:project.create=ok#project_id` `event:project.create=denied` `audit:config_change=ok` `audit:control_decision=denied` `code:http:POST /api/projects#403` `code:cli:relay project create#1` | `pending:#289` `journey:gate-project-grant-pos` `journey:gate-project-grant-neg` |
| `G2.02` | Widen a grant | A save that adds an MCP, a tool, wider access or a scope value, or converts a local project to remote, needs the owner; without approval the project is unchanged. | Settings > Projects > Edit > Save | `relay project edit` | `http:PUT /api/projects/{id}` `ipc:update_project` `cli:relay project edit` `bridge:admin_op:project.edit` | `project.grant` | `event:project.update=ok#gated` `event:project.update=denied` `audit:control_decision=denied` `code:http:PUT /api/projects/{id}#403` `code:cli:relay project edit#1` | `pending:#289` `journey:gate-project-grant-neg` `journey:v1-conversion-refusal` (always NOTRUN) |
| `G2.03` | Narrow a grant or make an ungated edit | A save that only narrows a grant or changes a non-grant field takes effect with no owner prompt and reaches a live session, even when the owner would refuse `project.grant`; the same door refuses a widening. | Settings > Projects > Edit > Save | `relay project edit`; `relay project update --files-read-only`; `settings.json` `projects[]` | `http:PUT /api/projects/{id}` `ipc:update_project` `cli:relay project edit` `bridge:admin_op:project.edit` `cli:relay project update` `bridge:admin_op:project.update` | `project.grant` | `event:project.update=ok#gated` `event:project.update=denied` `audit:config_change=ok` `audit:control_decision=denied` (a narrowing leaves no presence or approval audit row) | `pending:#289` `journey:grant-narrowing-live` `journey:stale-derived-access-edit` `journey:context-number-resave` |
| `G2.04` | Remove a project | A removal deletes the project and ends its sessions; an unknown id answers 404. | Settings > Projects > Delete | `relay project remove` | `http:DELETE /api/projects/{id}` `ipc:remove_project` `cli:relay project remove` `bridge:admin_op:project.remove` | none | `event:project.remove=ok#project_id` `event:project.remove=error/not_found` | `pending:#289` `journey:verify-fixtures-removed` |
| `G2.05` | List and read projects | A read credential lists every project and reads one by id; a caller without a credential answers 401. | Settings > Projects | `relay grant --json` | `http:GET /api/projects` `http:GET /api/projects/{id}` | none | `event:project.list=ok#count` `event:project.get=ok#project_id` `code:http:GET /api/projects#401` | `pending:#289` |
| `G2.06` | Show a project's grant | `relay grant` prints exactly what each project was granted: MCPs, tools, access and scope. | Settings > Projects > Edit | `relay grant --json` | `cli:relay grant` `bridge:admin_op:grant.view` | none | `event:grant.view=ok#count` `out:cli:relay grant#.` | `pending:#289` |
| `G2.07` | Scope values (`contextSchema`) | A scope field lists its allowed values from the MCP; a widening save of a scope value is gated like any widening. | Settings > Projects > Edit > scope field | `relay mcp scope-fields` (read), `relay project edit` (write) | `http:GET /api/mcps/{id}/scope_fields` `http:POST /api/mcps/{id}/enumerate` `ipc:enumerate_scope_field` `cli:relay mcp scope-fields` `bridge:admin_op:mcp.scope_fields` | none | `event:mcp.scope_fields.get=ok#mcp_id` `event:mcp.scope_field.enumerate=ok#field` | `pending:#289` `journey:context-number-resave` |
| `G2.08` | Default project (home or work) | A mode's default project is saved and read back; an unknown mode is refused. | Settings > Projects > Default projects > Home / Work | `settings.json` `default_project` | `http:PUT /api/default_project/{mode}` `ipc:set_default_project` | none | `event:project.default.set=ok#mode` `event:project.default.set=error/invalid` | `pending:#289` |
| `G2.09` | Chief of Staff project, model and daily limit | A read credential reads the Chief of Staff config, a configure credential sets or clears it, and the chief-of-staff scope is refused all three routes. | Settings > Projects > Chief of Staff | eve's `data/settings.json` `chiefOfStaff` | `http:GET /api/chief-of-staff/config` `http:PUT /api/chief-of-staff/config` `http:DELETE /api/chief-of-staff/config` `ipc:set_chief_of_staff` | none | `event:chief_of_staff.config.get=ok` `event:chief_of_staff.config.set=ok#project_id` `event:chief_of_staff.config.clear=ok` `audit:control_decision=denied` `code:http:PUT /api/chief-of-staff/config#403` | `pending:#289` `journey:cos-settings` |
| `G2.10` | Rotate a project token | An approved rotate returns a new token and kills the old one; a rotate the owner does not approve changes nothing. | Settings > Projects > Edit > Bearer Token > Rotate | `relay project rotate-token` | `http:POST /api/projects/{id}/rotate_token` `ipc:rotate_project_token` `cli:relay project rotate-token` `bridge:admin_op:project.token.rotate` | `project.rotate_token` | `event:project.rotate_token=ok#project_id` `event:project.rotate_token=denied` `audit:control_decision=denied` `code:http:POST /api/projects/{id}/rotate_token#403` `code:cli:relay project rotate-token#1` | `pending:#289` `journey:gate-project-rotate-token-pos` `journey:gate-project-rotate-token-neg` |
| `G2.11` | Reveal a project token | An approved reveal prints the project's token to the operator's terminal; a reveal from a session or without approval prints nothing. | Settings > Projects > Edit > Bearer Token > Show (the eye icon is ungated) | `relay project token` | `cli:relay project token` `bridge:admin_op:project.token.reveal` | `project.reveal_token` | `event:project.reveal_token=ok#project_id` `event:project.reveal_token=denied` `audit:control_decision=denied` `code:cli:relay project token#1` | `pending:#289` |
| `G2.12` | Regenerate SKILL.md | A regenerate rewrites the project's SKILL.md from its current grant. | Settings > Projects > Regen Skill | `relay project regen-skill` | `http:POST /api/projects/{id}/regen_skill` `ipc:regen_project_skill` `cli:relay project regen-skill` `bridge:admin_op:project.skill.regen` | none | `event:project.regen_skill=ok#project_id` | `pending:#289` |

### G3 · Add a tool and let a project use it — must-have
Intent: register an MCP server and have a project's agents call its tools.
It worked: the tools appear in the project's session, a call returns, and a disabled tool is refused.
Why must-have: every mail, calendar and file action an agent takes goes through the bridge.
Areas: mcps, grants, audit.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G3.01` | Register an MCP (stdio or HTTP) | An approved register adds the MCP and publishes its tools; a register the owner does not approve adds nothing. | Settings > MCP Servers > + New MCP Server | `relay mcp register` | `http:POST /api/mcps` `ipc:add_external_mcp` `cli:relay mcp register` `bridge:admin_op:mcp.register` | `mcp.register` | `event:mcp.register=ok#mcp_id` `event:mcp.register=denied` `audit:control_decision=denied` `code:http:POST /api/mcps#500` `code:cli:relay mcp register#1` | `pending:#289` `journey:gate-mcp-register-pos` `journey:gate-mcp-register-neg` |
| `G3.02` | Authenticate an HTTP MCP (OAuth) | An approved authenticate opens the provider's sign-in; one the owner does not approve starts nothing. | Settings > MCP Servers > card > Authenticate | `relay mcp authenticate` | `ipc:authenticate_mcp` `cli:relay mcp authenticate` `bridge:admin_op:mcp.authenticate` | `mcp.oauth.start` | `event:mcp.oauth.start=ok#mcp_id` `event:mcp.oauth.start=denied` `audit:control_decision=denied` `code:cli:relay mcp authenticate#1` | `pending:#289` `journey:gate-mcp-oauth-start-neg` `journey:gate-mcp-oauth-start-pos` `e2e:TestFakeMCPOAuthAuthenticates` |
| `G3.03` | List MCPs | The list names every registered MCP with its state. | Settings > MCP Servers | `relay mcp list` | `http:GET /api/mcps` `cli:relay mcp list` `bridge:admin_op:mcp.list` | none | `event:mcp.list=ok#count` | `pending:#289` `journey:gate-mcp-register-pos` |
| `G3.04` | List an MCP's tools | A read credential lists the tools an MCP publishes; an unknown MCP id answers 404. | Settings > MCP Servers > card > tools | `relay mcp list` | `http:GET /api/mcps/{id}/tools` `ipc:list_mcp_tools` | none | `event:mcp.tools.list=ok#count` `event:mcp.tools.list=error/not_found` | `pending:#289` `e2e:TestFakeMCPStdioListsTools` `e2e:TestFakeMCPHTTPListsTools` |
| `G3.05` | Tool listing and calls through the bridge | A project token lists and calls only the tools its project may use; a tool the project does not hold, or another project's token, is refused. | none | `relay mcp --token`, `relay mcp call --token` | `cli:relay mcp` `cli:relay mcp call` `cli:relay mcpExec` `bridge:ListTools` `bridge:CallTool` `bridge:DescribeProject` | none | `event:tool.list=ok#count` `event:tool.call=ok#tool` `event:tool.call=denied` `event:project.describe=ok#project_id` `audit:call_tool=denied` | `pending:#289` `journey:acme-tools-through-bridge` |
| `G3.06` | Disable tools per project | A tool unticked for a project is refused when that project calls it. | Settings > Projects > Edit > tool picker > untick | `settings.json` `projects[].disabled_tools` | `ipc:update_project_disabled_tools` | none | `event:project.disabled_tools.set=ok#count` `audit:call_tool=denied` | `pending:#289` `journey:disabled-tool-refused` |
| `G3.07` | Unregister an MCP | An unregister removes the MCP and its tools from every project; an unknown id answers 404. | Settings > MCP Servers > Remove | `relay mcp unregister` | `http:DELETE /api/mcps/{id}` `ipc:remove_external_mcp` `cli:relay mcp unregister` `bridge:admin_op:mcp.unregister` | none | `event:mcp.unregister=ok#mcp_id` `event:mcp.unregister=error/not_found` | `pending:#289` `journey:verify-fixtures-removed` |
| `G3.08` | Reset MCP permissions (macOS TCC) | A reset clears the macOS privacy grants the MCP's binary holds. | Settings > MCP Servers > Reset Permissions | `relay mcp reset-permissions` | `ipc:reset_mcp_permissions` `cli:relay mcp reset-permissions` `bridge:admin_op:mcp.permissions.reset` | none | `event:mcp.permissions.reset=ok#mcp_id` | `pending:#289` |
| `G3.09` | Reload and reconcile external MCPs | The reload and reconcile requests restart one external MCP or bring the set in line with settings; only an admin token may send them; any other caller gets the bridge error code -32001 and no `mcp.reload` line. | n/a | n/a | `bridge:ReloadExternalMcp` `bridge:ReconcileExternalMcps` | none | `event:mcp.reload=ok#mcp_id` `event:mcp.reconcile=ok` `out:bridge:ReloadExternalMcp#.code` | `pending:#289` `e2e:TestBridgeAdminFrameNeedsSecret` |
| `G3.10` | MCP health and restart | A published MCP reports `up`, and a crashed one is restarted or abandoned, each change in the log. | Settings > MCP Servers > card state | n/a | none | none | `event:mcp.state=ok#state` `audit:mcp_down=error` | `pending:#289` |

### G4 · See what an agent touched — must-have
Intent: after a session, look up which tools it called, on what, and what was refused.
It worked: every call and refusal is in the log with its project, tool and outcome, and nothing is missing.
Why must-have: the only after-the-fact check on an agent; a dropped row is invisible.
Areas: audit.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G4.01` | Tail and filter the log | `relay audit` prints rows filtered by event, project, outcome or text, newest last. | none | `relay audit` | `cli:relay audit` | none | `out:cli:relay audit#.event` `out:cli:relay audit#.outcome` | `pending:#289` `journey:context-number-resave` |
| `G4.02` | Query and filter in Settings | A read credential queries the audit log with filters; a caller without a credential answers 401. | Settings > Tool Calls > filters > Refresh | `relay audit --event --project …` | `http:GET /api/audit` `ipc:query_audit` | none | `event:audit.query=ok#count` `code:http:GET /api/audit#401` | `pending:#289` |
| `G4.03` | Tool-call rows | A local tool call writes one row; a remote call writes an intent row and then a completion row; a refused call writes a denied row. | n/a | `relay audit --event call_tool` | `bridge:CallTool` | none | `audit:call_tool=ok` `audit:call_tool=denied` `audit:call_tool=pending` | `pending:#289` `journey:tool-call-audited` |
| `G4.04` | Issuance and config-change rows | Each passed owner gate writes an issuance or config-change row carrying the presence id. | n/a | `relay audit --event config_change` | none | none | `audit:credential_issued=ok` `audit:credential_revoked=ok` `audit:config_change=ok` | `pending:#289` `journey:gate-credential-mint-pos` `journey:gate-mcp-register-pos` `journey:gate-project-grant-pos` `journey:gate-service-register-pos` `journey:gate-project-rotate-token-pos` `journey:gate-eve-enrolment-open-pos` `journey:gate-credential-revoke-pos` |
| `G4.05` | Session launch rows | Each launch, allowed or refused, writes a `session_launch` row with project, kind and outcome. | n/a | `relay audit --event session_launch` | `http:POST /api/sessions` `http:POST /api/terminals` `cli:relay audit` | none | `audit:session_launch=ok` | `pending:#289` `journey:session-chat-lifecycle` `journey:terminal-lifecycle` |
| `G4.06` | Refusal rows | A refused launch or a refused control-plane call writes a denied row with the reason. | n/a | `relay audit --outcome denied` | `cli:relay audit` | none | `audit:session_launch=denied` `audit:control_decision=denied` | `pending:#289` `journey:blank-model-refused` `journey:oversized-launch-audit-capped` |
| `G4.07` | Test-approver answer rows | The test build records each presence answer as a `control_decision` row carrying `presence_approver` before it acts. On a non-default config dir it answers each gate per op from `X/test-presence.json`; on the default config dir it approves only `project.grant`. | n/a | `relay audit --event control_decision` | `cli:relay audit` | none | `audit:control_decision=ok#presence_approver` `audit:control_decision=denied` | `pending:#289` |
| `G4.08` | Chief of Staff send rows | A scoped send writes an intent row and then a completion row; a send that cannot be recorded is refused. | n/a | `relay audit --event session_message` | `http:POST /api/chief-of-staff/messages` | none | `audit:session_message=pending` `audit:session_message=ok` `event:chief_of_staff.send=denied/audit_unavailable` | `e2e:TestChiefOfStaffSendAuditOffWritesEvent` `e2e:TestChiefOfStaffSendRefusalsWriteEvent` `pending:#289` `journey:chief-of-staff-send` |
| `G4.09` | Chief of Staff start rows | A scoped start writes a `session_launch` row with `origin`, `prompt_bytes` and, for a hosted project, `host_id`; a start that cannot be recorded is refused. | n/a | `relay audit --event session_launch` | `http:POST /api/chief-of-staff/sessions` | none | `audit:session_launch=ok` `event:chief_of_staff.start=denied/audit_unavailable` | `pending:#289` `journey:cos-start` `journey:cos-start-outside-root` `journey:cos-start-host` |
| `G4.10` | Export the log | An export writes the filtered rows to a file named `toolcalls-export-<timestamp>.jsonl` beside the audit log and answers its path. | Settings > Tool Calls > Export | `relay audit --json` | `http:POST /api/audit/export` `ipc:export_audit` | none | `event:audit.export=ok#count` `out:http:POST /api/audit/export#.path` | `pending:#289` |
| `G4.11` | Reveal the log file | The reveal returns the path of the audit file. | Settings > Tool Calls > Reveal Log | `relay audit --path` | `http:GET /api/audit/log` `ipc:reveal_audit_log` | none | `event:audit.path.get=ok` | `pending:#289` |

### G5 · Keep background services running, including scheduled work — must-have
Intent: run relayLLM, eve, the scheduler and other services under relay, started at login and restarted on a crash.
It worked: the services are up after login, a crashed one comes back, and the tray and `relay service list` show the true state.
Why must-have: eve, chat and scheduled runs all sit on this; a service that stays down after a crash is noticed late. Scheduling itself lives in relayScheduler; relay's part is keeping it running.
Areas: services, tray.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G5.01` | Register a service | An approved register adds the service and starts it if autostart is set; a register the owner does not approve adds nothing. | Settings > Services > + New Service > Add Service | `relay service register` | `http:POST /api/services` `ipc:add_service` `cli:relay service register` `bridge:admin_op:service.register` | `service.register` | `event:service.register=ok#service_id` `event:service.create=ok#service_id` `event:service.register=denied` `audit:control_decision=denied` `code:http:POST /api/services#500` `code:cli:relay service register#1` | `pending:#291` `journey:gate-service-register-pos` `journey:gate-service-register-neg` |
| `G5.02` | Edit a service | A save that changes the command or capabilities needs the owner; without approval the service is unchanged. | Settings > Services > card > Edit > Save | `relay service register --name <existing>` | `http:PUT /api/services/{id}` `ipc:update_service` | `service.register` | `event:service.update=ok#service_id` `event:service.update=denied` `audit:control_decision=denied` `code:http:PUT /api/services/{id}#500` | `pending:#291` |
| `G5.03` | Start and stop | A start brings a stopped service up and a stop takes it down; the state shows in the list. | tray > service row; Settings > Services > Start / Stop | `relay service start`, `relay service stop` | `http:POST /api/services/{id}/start` `http:POST /api/services/{id}/stop` `ipc:start_service` `ipc:stop_service` `cli:relay service start` `cli:relay service stop` `bridge:admin_op:service.start` `bridge:admin_op:service.stop` | none | `event:service.start=ok#service_id` `event:service.stop=ok#service_id` | `pending:#291` `journey:service-start-stop` `journey:settings-window-services` |
| `G5.04` | Restart a service | A restart stops and starts the service and the session host reloads on request. | none | `relay service restart` | `cli:relay service restart` `bridge:admin_op:service.restart` `bridge:ReloadService` | none | `event:service.restart=ok#service_id` | `pending:#291` `journey:session-host-restart` |
| `G5.05` | List and read services | The list shows every service with its real state; an unknown id answers 404. | tray > service rows; Settings > Services | `relay service list` | `http:GET /api/services` `http:GET /api/services/{id}` `cli:relay service list` `bridge:admin_op:service.list` | none | `event:service.list=ok#count` `event:service.get=ok#service_id` `event:service.get=error/not_found` | `pending:#291` |
| `G5.06` | Autostart at login | A service set to autostart is started when relay starts. | Settings > Services > card > Start with Relay | `relay service register --autostart` | `http:PUT /api/services/{id}/autostart` `ipc:update_service_autostart` | none | `event:service.autostart.set=ok#autostart` | `pending:#291` |
| `G5.07` | Restart on crash, then `failed` after max attempts | A service that exits unrequested is restarted; after the maximum attempts it reads `failed`. | tray > service row | `relay service list` (STATE column) | `bg:service supervision` | none | `event:service.state=ok#phase` | `pending:#291` `journey:service-restart-on-crash` (the restart only, not the failed phase) |
| `G5.08` | Unregister a service | An unregister stops and removes the service; an unknown id answers 404. | Settings > Services > Remove | `relay service unregister` | `http:DELETE /api/services/{id}` `ipc:remove_service` `cli:relay service unregister` `bridge:admin_op:service.unregister` | none | `event:service.unregister=ok#service_id` `event:service.unregister=error/not_found` | `pending:#291` `journey:verify-fixtures-removed` |
| `G5.09` | Menu visibility and order | A hidden service leaves the tray menu, and a move changes its position in the list. | Settings > Services > card > Show in menu; ↑ / ↓ | `settings.json` `services[].hide_from_menu`, `services[]` order | `http:PUT /api/services/{id}/menu` `http:PUT /api/services/{id}/position` `ipc:update_service_menu_hidden` `ipc:move_service` | none | `event:service.menu.set=ok#hidden` `event:service.move=ok#index` | `pending:#291` |
| `G5.10` | Service actions and config (manifest) | An action a service's manifest declares runs and reports; a config save rewrites the file and restarts the service when asked. | Settings > Service Inspector > service > action / Configuration | `relay service action`, `relay service config` | `ipc:service_action` `ipc:service_config` `cli:relay service action` `cli:relay service config` `bridge:admin_op:service.action` `bridge:admin_op:service.config.get` `bridge:admin_op:service.config.save` | none | `event:service.action=ok#action_id` `event:service.config.get=ok#service_id` `event:service.config.save=ok#restarted` | `pending:#291` |
| `G5.11` | Reveal a service's log | The service's log file is at `paths.logs` plus `<id>.log`. | Settings > Services > card > Logs | `relay status` (`paths.logs`) | `ipc:reveal_service_log` | none | `out:cli:relay status#.paths.logs` | `pending:#291` |
| `G5.12` | Bridge hello and manifest with a wrong launch secret refused | A service says hello with its launch secret and registers its manifest under its launch identity; a wrong secret or another service's identity is refused. | n/a | n/a | `bridge:Hello` `bridge:RegisterManifest` | none | `event:bridge.hello=ok#service_id` `event:bridge.hello=denied` `event:service.manifest.register=ok#service_id` `event:service.manifest.register=denied` | `pending:#291` |

### G6 · Let a session use a model — must-have
Intent: pick a model for a project or service and have its sessions reach it through relay.
It worked: the model list shows the configured models, and a chat gets an answer from the chosen one.
Why must-have: every chat turn crosses the model endpoint.
Areas: models, sessions, audit.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G6.01` | Model list | The list names every model a caller may use and omits system-only models. | Settings > Projects > Edit > Allowed Models; eve > New Session > Web Chat > Model | `relay model list` | `ipc:list_models` `cli:relay model list` `bridge:admin_op:model.list` `proxy:GET /api/models` | none | `event:model.list=ok#count` `out:cli:relay model list#.models[]` | `pending:#290` `journey:model-list-and-completion` `e2e:TestFakeModelHostServesCatalogue` |
| `G6.02` | Pick a model per project or service | A launch with a model outside the project's allowed list is refused. | Settings > Projects or Services > Edit > Allowed Models | `relay service register --allowed-model`; `settings.json` `projects[].allowed_models` | none | none | `event:session.launch=denied` `audit:session_launch=denied` | `pending:#290` |
| `G6.03` | Model endpoint | A session or relayLLM calls `/v1/chat/completions`, `/v1/models`, the router-dialect `/models` and `/props`, or a passthrough, with a model key, and the call is recorded. | n/a | n/a | `model:GET /v1/models` `model:GET /models` `model:GET /props` `model:POST /v1/chat/completions` | none | `event:model.request=ok` `audit:model_call=ok` | `pending:#290` `journey:model-list-and-completion` |
| `G6.04` | Model keys (`rmk_`) for sessions | A model key minted at session launch lets that session call the endpoint; a missing or wrong key is refused. | n/a | n/a | `model:POST /v1/chat/completions` | none | `event:model.request=denied/unauthorized` `audit:model_call=unauthorized` | `pending:#290` |
| `G6.05` | Register a model host | relayLLM registers as the model host under its launch identity; any other caller is refused. | n/a | n/a | `bridge:RegisterModelHost` | none | `event:model.host.register=ok#service_id` `event:model.host.register=denied` | `pending:#290` |

### G7 · Sign in from a browser — should
Intent: reach relay's and eve's web pages from a browser with a passkey.
It worked: a registered passkey signs in; a revoked one no longer does.
Why should: used when away from the Mac; a break is loud, since the sign-in fails in front of the user.
Areas: login.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G7.01` | Mint a login code | An approved mint prints a one-time code that registers a passkey; a mint the owner does not approve prints none. | tray > Show Login Code... | `relay login enrol` | `cli:relay login enrol` `bridge:admin_op:login.bootstrap.mint` | `login.bootstrap.mint` | `event:login.bootstrap.mint=ok` `event:login.bootstrap.mint=denied` `audit:control_decision=denied` `code:cli:relay login enrol#1` | `deny e2e:TestLoginCodeMintDenied@cli:relay login enrol` `journey:gate-login-bootstrap-mint-neg` `journey:gate-login-bootstrap-mint-pos` (NOTRUN: the code is redeemed only by the browser ceremony) `e2e:TestPasskeyRegisterAndSignIn` |
| `G7.02` | Open the login page | The login page loads over TCP from any peer and offers sign-in and registration. | /relay/login | exception: browser page, no CLI | `http:GET /relay/login` | none | `event:login.page=ok` | `e2e:TestLoginPageServed` |
| `G7.03` | Register a passkey | A challenge and a verify with a valid code register the passkey; a missing or reused code registers none, and challenges are throttled. | /relay/login > code > Register a passkey | exception: browser ceremony, no CLI | `http:POST /relay/login/challenge` `http:POST /relay/login/verify` | `webauthn` | `event:login.challenge=ok` `event:login.passkey.register=ok#passkey_id` `event:login.challenge=denied/throttled` `event:login.passkey.register=denied` `code:http:POST /relay/login/verify#403` | `deny e2e:TestPasskeyRegisterBadCodeRefused@http:POST /relay/login/verify` `e2e:TestLoginChallengeThrottled` `e2e:TestPasskeyRegisterAndSignIn` |
| `G7.04` | Sign in with a passkey | A verify signed by a registered passkey opens a session; a revoked or unknown passkey does not. | /relay/login > Sign in | exception: browser ceremony, no CLI | `http:POST /relay/login/challenge` `http:POST /relay/login/verify` | `webauthn` | `event:login.sign_in=ok#credential_id` `event:login.sign_in=denied` `code:http:POST /relay/login/verify#403` | `e2e:TestPasskeyRegisterAndSignIn` `deny e2e:TestPasskeyUnknownCredentialRefused@http:POST /relay/login/verify` |
| `G7.05` | List passkeys | The list names every registered relay passkey. | Settings > Passkeys | `relay login list` | `ipc:list_passkeys` `cli:relay login list` `bridge:admin_op:login.list` | none | `event:login.list=ok#count` The login list verb has no JSON form; the test asserts the event's count. | `e2e:TestPasskeyList` |
| `G7.06` | List and sign out browser sessions | The list shows each signed-in browser, and a sign-out ends that session. | Settings > Passkeys > Signed-in Browsers > Sign out | `relay login sessions`, `relay login sign-out` | `ipc:sign_out_login` `cli:relay login sessions` `cli:relay login sign-out` `bridge:admin_op:login.session.list` `bridge:admin_op:login.session.sign_out` | none | `event:login.session.list=ok#count` `event:login.session.sign_out=ok#credential_id` `out:cli:relay login sessions#.sessions[]` | `e2e:TestBrowserSessionsListAndSignOut` |
| `G7.07` | Revoke a passkey | An approved revoke stops that passkey signing in; a revoke the owner does not approve leaves it working. | Settings > Passkeys > Revoke | `relay login revoke --id` | `ipc:revoke_passkey` `cli:relay login revoke` `bridge:admin_op:login.passkey.revoke` | `login.passkey.revoke` | `event:login.passkey.revoke=ok#passkey_id` `event:login.passkey.revoke=denied` `audit:control_decision=denied` `code:cli:relay login revoke#1` | `e2e:TestPasskeyRevoke` `deny e2e:TestPasskeyRevokeDenied@cli:relay login revoke` `journey:gate-login-passkey-revoke-neg` `journey:gate-login-passkey-revoke-pos` (NOTRUN: there is no disposable relay passkey) |
| `G7.08` | Open eve passkey enrolment | An approved open starts eve's five-minute enrolment window; an open the owner does not approve starts none. | tray > Allow Eve Passkey Enrolment… | `relay eve enrol` | `cli:relay eve enrol` `bridge:admin_op:eve.enrolment.open` | `eve.enrolment.open` | `event:eve.enrolment.open=ok` `event:eve.enrolment.open=denied` `audit:control_decision=denied` `code:cli:relay eve enrol#1` | `e2e:TestEveEnrolmentOpen` `deny e2e:TestEveEnrolmentOpenDenied@cli:relay eve enrol` `journey:gate-eve-enrolment-open-pos` `journey:gate-eve-enrolment-open-neg` |
| `G7.09` | List eve passkeys | The list names the eve passkeys eve last reported. | Settings > Passkeys > Eve passkeys | `relay eve list` | `cli:relay eve list` `bridge:admin_op:eve.list` | none | `event:eve.list=ok#count` | `e2e:TestEvePasskeyList` |
| `G7.10` | Revoke an eve passkey | An approved revoke queues the revocation for eve; an unknown or last id is refused before the gate. | Settings > Passkeys > Eve passkeys > Revoke | `relay eve revoke` | `ipc:revoke_eve_passkey` `cli:relay eve revoke` `bridge:admin_op:eve.passkey.revoke` | `eve.passkey.revoke` | `event:eve.passkey.revoke=ok#passkey_id` `event:eve.passkey.revoke=denied` `audit:control_decision=denied` `code:cli:relay eve revoke#1` | `e2e:TestEvePasskeyRevoke` `deny e2e:TestEvePasskeyRevokeDenied@cli:relay eve revoke` `journey:gate-eve-passkey-revoke-neg` `journey:gate-eve-passkey-revoke-pos` (NOTRUN: no revocable verify passkey) |
| `G7.11` | Eve passkey enrolment window | eve reads the window's status and consumes it once; a second consume or one after expiry is refused. | eve > Settings > Passkeys > Enrol | n/a | `http:GET /api/eve/passkey-enrolment` `http:POST /api/eve/passkey-enrolment/consume` | none | `event:eve.enrolment.consume=ok` `event:eve.enrolment.consume=error/conflict` | `e2e:TestEveEnrolmentWindowConsume` |
| `G7.12` | Eve passkey report and revocations | eve reports its passkeys and reads the pending revocations; a caller without configure is refused. | eve > Settings > Passkeys | n/a | `http:PUT /api/eve/passkeys` `http:GET /api/eve/passkeys/revocations` | none | `event:eve.passkey.report=ok#count` `out:http:GET /api/eve/passkeys/revocations#.revocations[]` `code:http:PUT /api/eve/passkeys#403` A read credential is refused by the bearer middleware before any handler: it writes no route event, only a denied control-decision audit row. | `e2e:TestEvePasskeyReport` |

### G8 · Operate relay from the tray and Settings — should
Intent: see at a glance that relay is healthy and get to what needs attention.
It worked: Settings opens, Overview's tiles and attention list match reality, and the tray shows each service's state.
Why should: used daily, but a break is visible at once.
Areas: tray, settings-ui.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G8.01` | Open Settings | The tray opens the Settings window on the screen the operator last used. | tray > Settings... (⌘,) | exception: tray menu, no CLI | none | none | `event:server.ready=ok#pid` | `screen-only` `journey:settings-window-services` |
| `G8.02` | Overview tiles and attention list | The Overview tiles and the attention list match `relay status`: version, seal status, paths, MCP health and service state. | Settings > Overview | `relay status` | `cli:relay status` `bridge:admin_op:status.view` | none | `event:status.view=ok` `out:cli:relay status#.seal_status` `out:cli:relay status#.service_runtime` | `e2e:TestStatusMatchesState` |
| `G8.03` | Recent tool calls | Overview lists the latest `call_tool` rows and nothing else. | Settings > Overview > Recent tool calls | `relay audit --tail` | `cli:relay audit` | none | `out:cli:relay audit#.event` | `e2e:TestRecentToolCallsFilter` |
| `G8.04` | Reveal config and logs folders | The reveal buttons open the folders `relay status` names in `paths`. | Settings > Overview > Reveal / Reveal logs | `relay status` (`paths`) | `ipc:reveal_config_dir` `ipc:reveal_logs_dir` | none | `out:cli:relay status#.paths.config` `out:cli:relay status#.paths.logs` | `e2e:TestStatusPaths` |
| `G8.05` | Service rows with state | The tray shows each service with the state `relay service list` reports. | tray > service row | `relay service list` | `cli:relay service list` | none | `event:service.state=ok#phase` | `e2e:TestServiceStateInStatus` `journey:settings-window-services` |
| `G8.06` | Pending enrolment line and notification | The tray line counts the requests `relay enrol requests` lists and opens Remote Clients. | tray > Pending enrolment requests line, or the banner | `relay enrol requests` | `cli:relay enrol requests` | none | `out:cli:relay enrol requests#.` | `e2e:TestPendingEnrolmentCount` |
| `G8.07` | Sealed-store warning | The tray and Overview warn when `seal_status` is not healthy. | tray > Sealed store line; Overview > Needs attention | `relay status` (`seal_status`) | `cli:relay status` | none | `out:cli:relay status#.seal_status` | `e2e:TestKeychainProviderFaults` |
| `G8.08` | Quit Relay | The tray quits relay and its services stop. | tray > Quit Relay | exception: tray menu, no CLI | none | none | `out:cli:relay status#.version` (no event marks a quit: the service state event has no stopped phase and the service stop event is written only by the service operations; after a quit, the status verb exits 1) | `screen-only` |

### G9 · Let a script or another tool drive relay — should
Intent: give a script a scoped credential for relay's control plane.
It worked: the credential's class allows what it should and nothing more; a revoked one gets 401.
Why should: every journey and several clients depend on it, but a break is loud.
Areas: credentials, logging, doors.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G9.01` | Mint a credential | An approved mint prints a token once with the requested classes; a mint the owner does not approve prints none. | none | `relay credential mint` | `cli:relay credential mint` `bridge:admin_op:credential.mint` | `credential.mint` | `event:credential.mint=ok#credential_id` `event:credential.mint=denied` `audit:credential_issued=ok` `audit:control_decision=denied` `code:cli:relay credential mint#1` | `pending:#291` `journey:gate-credential-mint-pos` `journey:execute-credential-renewal` `journey:gate-credential-mint-neg` |
| `G9.02` | List credentials | The list names each credential with its classes and expiry, never its token. | none | `relay credential list` | `cli:relay credential list` `bridge:admin_op:credential.list` | none | `event:credential.list=ok#count` | `pending:#291` `journey:gate-credential-mint-pos` |
| `G9.03` | Revoke a credential | An approved revoke makes the token answer 401 at once; a revoke the owner does not approve leaves it working. | none | `relay credential revoke` | `cli:relay credential revoke` `bridge:admin_op:credential.revoke` | `credential.revoke` | `event:credential.revoke=ok#credential_id` `event:credential.revoke=denied` `audit:credential_revoked=ok` `audit:control_decision=denied` `code:cli:relay credential revoke#1` | `pending:#291` `journey:gate-credential-revoke-pos` `journey:gate-credential-revoke-neg` |
| `G9.04` | Class enforcement on `/api/*` | A route answers 401 with no credential and 403 with a credential whose classes do not include the route's class, and records the 403 decision. | n/a | `Authorization: Bearer` header | `http:GET /api/projects` | none | `audit:control_decision=denied` (the 403 class refusal only; a 401 is written before any handler and records neither an event nor a control decision row) `code:http:GET /api/projects#401` `code:http:POST /api/projects#403` | `pending:#291` |
| `G9.05` | Filter and follow relay's events | `relay logs` prints the structured events filtered by key, trace and time, and follows new ones until its timeout. | none | `relay logs` | `cli:relay logs` | none | `out:cli:relay logs#.msg` `out:cli:relay logs#.trace_id` | `pending:#291` |
| `G9.06` | List every door | `relay doors --json` lists every HTTP route, IPC op, bridge request and CLI verb of the live server with its credential class and gates. | none | `relay doors` | `cli:relay doors` `bridge:admin_op:doors.list` | none | `event:doors.list=ok#count` `out:cli:relay doors#.doors[]` | `pending:#291` `e2e:TestDoorsListsLiveCatalogue` |
| `G9.07` | Name the trace of a call | A verb run with `--trace T` writes its events with `trace_id` T. | none | `--trace`, `X-Trace-Id` | `cli:relay logs` | none | `out:cli:relay logs#.trace_id` | `pending:#291` |
| `G9.08` | Operator-only verbs refused inside a session or sandbox | A verb that needs the operator is refused when run from a relay session or a sandbox, and the refusal is audited; nothing it would have changed changes. | none | any `relay` operator verb from a session | `cli:relay status` `bridge:admin_op:status.view` | none | `audit:control_decision=denied` `code:cli:relay status#1` | `pending:#291` |

### G10 · Give a remote machine access — later
Intent: let another machine reach chosen projects over mTLS.
It worked: an approved client reaches its projects and only those; a revoked one is refused.
Why later: set up rarely; almost every step is an owner gate.
Areas: remote.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G10.01` | View the remote listener | The view shows whether remote is enabled and the address it listens on. | Settings > Remote Clients > Remote Listener | `relay remote show` | `http:GET /api/remote` `cli:relay remote show` `bridge:admin_op:remote.view` | none | `event:remote.config.get=ok` `out:cli:relay remote show#.effective` | `e2e:TestRemoteShow` |
| `G10.02` | Configure the remote listener | An approved change saves the listener settings; a change the owner does not approve leaves them as they were. | Settings > Remote Clients > Remote Listener > Save | `relay remote set` | `http:PUT /api/remote` `ipc:update_remote_config` `cli:relay remote set` `bridge:admin_op:remote.set` | `remote.configure` | `event:remote.configure=ok#enabled` `event:remote.configure=denied` `audit:control_decision=denied` `code:http:PUT /api/remote#500` `code:cli:relay remote set#1` | `e2e:TestRemoteSet` `deny e2e:TestRemoteSetDeniedCLI@cli:relay remote set` `deny e2e:TestRemoteSetDeniedHTTP@http:PUT /api/remote` `journey:gate-remote-configure-neg` `journey:gate-remote-configure-pos` (NOTRUN: it changes the live listener the VM stack uses) |
| `G10.03` | Lodge an enrolment request | A remote machine lodges a request with a CSR and polls for the answer; a request flood is throttled, and the listener has no approve door. | n/a | n/a | `enrol:lodge` `enrol:poll` | none | `event:enrolment.request.lodge=ok#request_id` `event:enrolment.request.lodge=denied/throttled` | `e2e:TestEnrolmentLodgeAndThrottle` `e2e:TestEnrolClientLodgesRequest` |
| `G10.04` | List enrolment requests | The list shows each pending request with its label and fingerprint. | Settings > Remote Clients > Pending requests | `relay enrol requests` | `ipc:list_enrolment_requests` `cli:relay enrol requests` `bridge:admin_op:enrolment.request.list` | none | `event:enrolment.request.list=ok#count` `out:cli:relay enrol requests#.` | `e2e:TestEnrolmentRequestsList` |
| `G10.05` | Refuse an enrolment request | A refusal removes the request and the poll answers `refused`. | Settings > Remote Clients > Pending requests > Refuse | `relay enrol refuse` | `ipc:refuse_enrolment_request` `cli:relay enrol refuse` `bridge:admin_op:enrolment.request.refuse` | none | `event:enrolment.request.refuse=ok#request_id` | `e2e:TestEnrolmentRefuse` |
| `G10.06` | Approve an enrolment request | An approved sign issues the client its certificate; an approval the owner does not give issues none. | Settings > Remote Clients > Approve… | `relay enrol approve` | `ipc:approve_enrolment_request` `cli:relay enrol approve` `bridge:admin_op:enrolment.request.approve` | `enrolment.sign` | `event:enrolment.request.approve=ok#client_id` `event:enrolment.request.approve=denied` `audit:control_decision=denied` `code:cli:relay enrol approve#1` | `e2e:TestEnrolmentApprove` `deny e2e:TestEnrolmentApproveDenied@cli:relay enrol approve` `journey:gate-enrolment-sign-neg` `journey:gate-enrolment-sign-pos` (NOTRUN: remote identities are out of scope) |
| `G10.07` | Create an enrolment | An approved create issues a client identity with its profile; a create the owner does not approve issues none. | Settings > Remote Clients > + New Enrolment | `relay enrol create` | `http:POST /api/enrolments` `ipc:create_enrolment` `cli:relay enrol create` `bridge:admin_op:enrolment.create` | `enrolment.create` | `event:enrolment.create=ok#client_id` `event:enrolment.create=denied` `audit:credential_issued=ok` `audit:control_decision=denied` `code:http:POST /api/enrolments#500` `code:cli:relay enrol create#1` | `e2e:TestEnrolmentCreate` `deny e2e:TestEnrolmentCreateDeniedCLI@cli:relay enrol create` `deny e2e:TestEnrolmentCreateDeniedHTTP@http:POST /api/enrolments` `journey:gate-enrolment-create-neg` `journey:gate-enrolment-create-pos` (NOTRUN: remote identities are out of scope) |
| `G10.08` | Sign an enrolment | An approved sign issues a certificate for a CSR; a sign the owner does not approve issues none. | none | `relay enrol sign` | `cli:relay enrol sign` `bridge:admin_op:enrolment.sign` | `enrolment.sign` | `event:enrolment.sign=ok#client_id` `event:enrolment.sign=denied` `audit:control_decision=denied` `code:cli:relay enrol sign#1` | `e2e:TestEnrolmentSign` `deny e2e:TestEnrolmentSignDenied@cli:relay enrol sign` `journey:gate-enrolment-sign-neg` `journey:gate-enrolment-sign-pos` (NOTRUN: as above) |
| `G10.09` | Update an enrolment | An approved update changes a client's profile and grants; an update the owner does not approve changes none. | none | `relay enrol update` | `cli:relay enrol update` `bridge:admin_op:enrolment.update` | `enrolment.update` | `event:enrolment.update=ok#client_id` `event:enrolment.update=denied` `audit:control_decision=denied` `code:cli:relay enrol update#1` | `e2e:TestEnrolmentUpdate` `deny e2e:TestEnrolmentUpdateDenied@cli:relay enrol update` `journey:gate-enrolment-update-neg` `journey:gate-enrolment-update-pos` (NOTRUN: as above) |
| `G10.10` | Revoke an enrolment | An approved revoke ends the client's access, live connections included; a revoke the owner does not approve leaves it working. | Settings > Remote Clients > Revoke | `relay enrol revoke` | `http:DELETE /api/enrolments/{id}` `ipc:revoke_enrolment` `cli:relay enrol revoke` `bridge:admin_op:enrolment.revoke` | `enrolment.revoke` | `event:enrolment.revoke=ok#client_id` `event:enrolment.revoke=denied` `audit:control_decision=denied` `code:http:DELETE /api/enrolments/{id}#500` `code:cli:relay enrol revoke#1` | `e2e:TestEnrolmentRevokeEndsLiveConnection` `deny e2e:TestEnrolmentRevokeDeniedCLI@cli:relay enrol revoke` `deny e2e:TestEnrolmentRevokeDeniedHTTP@http:DELETE /api/enrolments/{id}` `journey:gate-enrolment-revoke-neg` `journey:gate-enrolment-revoke-pos` (NOTRUN: as above) |
| `G10.11` | List and read enrolments | The list shows each enrolled client with its profile; an unknown client id answers 404. | Settings > Remote Clients | `relay enrol list` | `http:GET /api/enrolments` `http:GET /api/enrolments/{id}` `cli:relay enrol list` `bridge:admin_op:enrolment.list` | none | `event:enrolment.list=ok#count` `event:enrolment.get=ok#client_id` `event:enrolment.get=error/not_found` | `e2e:TestEnrolmentListAndGet` |
| `G10.12` | CA fingerprint | The command prints the fingerprint of relay's enrolment CA; before a CA exists it exits 1. | Settings > Remote Clients > CA fingerprint > Copy | `relay enrol ca-fingerprint` | `cli:relay enrol ca-fingerprint` | none | `out:cli:relay enrol ca-fingerprint#.` `code:cli:relay enrol ca-fingerprint#1` The item reads the one stdout line, sha256: and 64 hex characters; the verb has no JSON form. | `e2e:TestCAFingerprint` |
| `G10.13` | Remote calls (fail-closed, audited) | An enrolled client lists and calls exactly the tools its profile allows; any other tool is refused. With auditing off the remote listener does not start, so there is no listener to call. | n/a | n/a | `remote:ListTools` `remote:CallTool` | none | `event:tool.list=ok#count` `event:tool.call=ok#tool` `event:tool.call=denied` `audit:call_tool=denied` | `e2e:TestRemoteCallsScopedAndAudited` `e2e:TestRemoteClientListsProfileTools` |
| `G10.14` | Remote grant view and narrowing | A remote client reads its own grant and can only narrow it; a request that would widen answers `invalid`. | n/a | n/a | `remote:DescribeGrant` `remote:NarrowGrant` | none | `event:grant.describe=ok#client_id` `event:grant.narrow=ok#changed` `event:grant.narrow=error/invalid` | `e2e:TestRemoteGrantNarrowOnly` `e2e:TestNarrowGrantWideningIsInvalidParams` |
| `G10.15` | Remote client budget | A client over its rate or volume budget is refused and the refusal is audited. | n/a | n/a | `remote:CallTool` | none | `audit:call_tool=throttled` `event:tool.call=denied/throttled` | `e2e:TestRemoteBudgetThrottles` |

### G11 · Work on a remote directory over SSH — later
Intent: treat a folder on another machine as a project.
It worked: sessions and tools run on the host through one SSH connection.
Why later: the devbox world has no hosts; only slow-route-keepalive adds one,
an unreachable host it removes again.
Areas: hosts.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G11.01` | List and read hosts | The list names each SSH host; an unknown id answers 404. | Settings > Hosts | `settings.json` `hosts[]` | `http:GET /api/hosts` `http:GET /api/hosts/{id}` `ipc:list_hosts` | none | `event:host.list=ok#count` `event:host.get=ok#host_id` `event:host.get=error/not_found` | `e2e:TestHostListAndGet` |
| `G11.02` | Add a host | An add saves the host; a host with a malformed address is refused. | Settings > Hosts > + Add host | `settings.json` `hosts[]` | `http:POST /api/hosts` `ipc:create_host` | none | `event:host.create=ok#host_id` `event:host.create=error/invalid` | `e2e:TestHostAdd` `journey:slow-route-keepalive` |
| `G11.03` | Edit a host | An edit saves the change; an unknown id answers 404. | Settings > Hosts > row > Edit | `settings.json` `hosts[]` | `http:PUT /api/hosts/{id}` `ipc:update_host` | none | `event:host.update=ok#host_id` `event:host.update=error/not_found` | `e2e:TestHostEdit` |
| `G11.04` | Remove a host | A removal deletes the host and ends its file agent. | Settings > Hosts > row > Remove | `settings.json` `hosts[]` | `http:DELETE /api/hosts/{id}` `ipc:remove_host` | none | `event:host.remove=ok#host_id` | `e2e:TestHostRemove` `journey:slow-route-keepalive` `journey:verify-fixtures-removed` |
| `G11.05` | Probe a host | A probe reports whether relay reaches the host over SSH; an unreachable host reads `unreachable`. | Settings > Hosts > row > Probe | `relay host probe` | `http:POST /api/hosts/{id}/probe` `ipc:probe_host` `cli:relay host probe` `bridge:admin_op:host.probe` | none | `event:host.probe=ok#host_id` `out:cli:relay host probe#.probe.ok` `out:cli:relay host probe#.status` | `e2e:TestHostProbe` `e2e:TestSSHHostProbeReachable` |
| `G11.06` | Disconnect a host | A disconnect closes the host's shared SSH connection; the next use reconnects. | Settings > Hosts > row > Disconnect | `relay host disconnect` | `http:POST /api/hosts/{id}/disconnect` `ipc:disconnect_host` `cli:relay host disconnect` `bridge:admin_op:host.disconnect` | none | `event:host.disconnect=ok#host_id` | `e2e:TestHostDisconnect` |
| `G11.07` | Paste a file to a host | A paste writes the bytes to a temporary file on the host and answers its path. | eve > session > paste an image | n/a | `http:POST /api/hosts/{id}/pastetmp` | none | `event:host.pastetmp=ok#host_id` | `e2e:TestHostPasteTmp` `e2e:TestHostPasteTmpRefusesOtherNames` |
| `G11.08` | List host templates | The list shows the terminal templates one host adds. | Settings > Hosts > row > Edit > Terminal templates | `settings.json` `hosts[].terminal_templates` | `http:GET /api/hosts/{id}/templates` `ipc:list_host_templates` | none | `event:host_template.list=ok#count` | `e2e:TestHostTemplateList` |
| `G11.09` | Add a host template | An add saves the template on the host; a malformed template is refused. | Settings > Hosts > row > Edit > Terminal templates > Add | `settings.json` `hosts[].terminal_templates` | `http:POST /api/hosts/{id}/templates` `ipc:create_host_template` | none | `event:host_template.create=ok#template_id` `event:host_template.create=error/invalid` | `e2e:TestHostTemplateAdd` |
| `G11.10` | Edit a host template | An edit saves the change; an unknown template id answers 404. | Settings > Hosts > row > Edit > Terminal templates > Edit | `settings.json` `hosts[].terminal_templates` | `http:PUT /api/hosts/{id}/templates/{tid}` `ipc:update_host_template` | none | `event:host_template.update=ok#template_id` `event:host_template.update=error/not_found` | `e2e:TestHostTemplateEdit` |
| `G11.11` | Remove a host template | A removal deletes the template from the host. | Settings > Hosts > row > Edit > Terminal templates > Remove | `settings.json` `hosts[].terminal_templates` | `http:DELETE /api/hosts/{id}/templates/{tid}` `ipc:remove_host_template` | none | `event:host_template.remove=ok#template_id` | `e2e:TestHostTemplateRemove` |

### G12 · Recover from a broken sealed store — later
Intent: start over when the keychain key is lost.
It worked: relay names what it destroys, and starts clean.
Why later: break-glass only; it is an owner gate by design.
Areas: sealed.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G12.01` | Reset Sealed Store… | An approved reset names what it destroys and starts a clean store; a reset the owner does not approve changes nothing. | tray > Reset Sealed Store... | `relay sealed reset` | `cli:relay sealed reset` `bridge:admin_op:sealed.store.reset` | `sealed.reset` | `event:sealed.reset=ok` `event:sealed.reset=denied` `audit:control_decision=denied` `code:cli:relay sealed reset#1` | `pending:#291` `journey:gate-sealed-reset-neg` `journey:gate-sealed-reset-pos` |

### G13 · Open, edit and search a project's files in eve — should
Intent: browse and change a project's files, console or SSH host, from eve.
It worked: the change lands, a link or `..` is refused, a read-only project stays unchanged, and the audit lists each change.
Why should: weekly; a break is loud in eve's file errors.
Areas: files.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G13.01` | Read operations on a project's files | A caller with execute lists, stats, reads, searches and runs git status in a granted project folder. | eve > Files | `relay audit --event file_op` | `http:POST /api/projects/{id}/files/list` `http:POST /api/projects/{id}/files/stat` `http:POST /api/projects/{id}/files/read` `http:POST /api/projects/{id}/files/search` `http:POST /api/projects/{id}/files/git` | none | `event:file.list=ok#project_id` `event:file.stat=ok#project_id` `event:file.read=ok#project_id` `event:file.search=ok#project_id` `event:file.git=ok#project_id` | `pending:#291` `journey:file-plane-contained` |
| `G13.02` | Change operations on a project's files | A caller with execute writes, makes folders, renames, moves and deletes files in a granted project, and each change is audited. | eve > Files > open, edit, save, delete | `relay audit --event file_op` | `http:POST /api/projects/{id}/files/write` `http:POST /api/projects/{id}/files/mkdir` `http:POST /api/projects/{id}/files/rename` `http:POST /api/projects/{id}/files/move` `http:POST /api/projects/{id}/files/delete` | none | `event:file.write=ok#project_id` `event:file.mkdir=ok#project_id` `event:file.rename=ok#project_id` `event:file.move=ok#project_id` `event:file.delete=ok#project_id` `audit:file_op=ok` | `pending:#291` `journey:file-plane-contained` |
| `G13.03` | File operations with containment and audit | A path through `..` or a planted symlink is refused, and the refusal is audited. | eve > Files | `relay audit --event file_op` | `http:POST /api/projects/{id}/files/read` `http:POST /api/projects/{id}/files/write` | none | `event:file.read=denied/not_granted` `event:file.write=denied/symlink` `event:file.read=denied/symlink` `audit:file_op=denied` | `pending:#291` `journey:file-plane-contained` |
| `G13.04` | Read-only project files | A project marked files-read-only refuses every change through the file routes and records the refusal. | none | `settings.json` `projects[].files_read_only`; `relay project update --files-read-only` | `http:POST /api/projects/{id}/files/write` `cli:relay project update` | none | `event:file.write=denied/read_only` `audit:file_op=denied` | `pending:#291` `journey:file-plane-contained` |
| `G13.05` | Stream a file | A stream returns the file's bytes once and records its end or failure. | eve > Files > open a large or binary file | n/a | `http:GET /api/projects/{id}/files/stream` | none | `event:file.stream=ok#project_id` | `pending:#291` |
| `G13.06` | Watch events and host status | A watch on `/ws/files` delivers `host_status`, `watch_ok` and `fs_event` frames as files change, and `relay files watch` prints the same stream. | eve > Files (tree updates on change) | `relay files watch` | `http:GET /ws/files` `ws:/ws/files fs_event` `ws:/ws/files host_status` `cli:relay files watch` `bridge:admin_op:files.watch` | none | `event:files.watch=ok#project_id` `event:file.ws.close=ok` | `pending:#291` `journey:file-plane-contained` `e2e:TestWebSocketFilesWatch` |

### G14 · Run several relays side by side, each named by its config dir — later
Intent: start a relay that is picked by its config dir alone, so a test harness or a second profile runs next to the tray without touching it.
It worked: `relay serve --config-dir X` prints `X/ready.json` once every listener is up; a verb with the same dir (or `RELAY_CONFIG_DIR`) reaches that instance only; a dir with no server fails naming the dir and is not created.
Why later: a harness and power-user path; the tray's everyday behaviour is unchanged.
Areas: instance, sandbox, remote, models.

| ID | Feature | Claim | Simple door | Power door | Doors | Gate | Proof | Test |
|---|---|---|---|---|---|---|---|---|
| `G14.01` | Headless server | `relay serve --config-dir X` prints one stdout line (the path of `X/ready.json`), writes the file once every listener is up, and exits 0 and removes it on SIGTERM or SIGINT. | none | `relay serve` | `cli:relay serve` | none | `event:server.ready=ok#pid` `event:server.ready=ok#ready_file` | `e2e:TestServeWritesReadyFile` `e2e:TestInstancesIsolated` `journey:gate-sealed-reset-pos` |
| `G14.02` | Pick the instance for any verb | A verb with `--config-dir X` (or `RELAY_CONFIG_DIR`) reaches only that instance; a dir with no server fails exit 1 naming the dir and is not created. | none | `--config-dir`, `RELAY_CONFIG_DIR` | `cli:relay doors` | none | `out:cli:relay doors#.doors[]` `code:cli:relay doors#1` | `e2e:TestClientWithoutServerFails` `e2e:TestVerbReachesOnlyItsInstance` |
| `G14.03` | Listener addresses from settings, port 0 allowed | `api.listen`, `model_endpoint.listen`, `remote.listen` and `remote.enrolment_listen` (with `remote.enabled` and `remote.enrolment_requests` both true, and auditing on) bind the requested address, and the `listeners` object of the file at `ready_file` holds the bound ones; `remote show` reports the configured address. | n/a | `settings.json` `api.listen`, `model_endpoint.listen`, `remote.listen`, `remote.enrolment_listen` | none | none | `event:server.ready=ok#ready_file` `out:cli:relay remote show#.listen` | `e2e:TestListenerAddressesFromSettings` |
| `G14.04` | Loopback ports a sandboxed session may not reach | A sandboxed session cannot connect to a denied loopback port: the list replaces the default `[3000, 8181]`, the instance's own API port is always denied, and a bad entry refuses the launch. | n/a | `settings.json` `session_sandbox.denied_loopback_ports` | `bridge:SandboxAttach` | none | `event:sandbox.attach=ok#template` `event:session.launch=denied` | `e2e:TestSandboxDeniedLoopbackPorts` |
| `G14.06` | Release build carries no test seam | A release binary answers `unknown command: debug` with exit 1, and holds no test-approver code. | n/a | `scripts/check-test-build.sh` | `cli:relay debug clock` `bridge:admin_op:debug.clock` | none | `code:cli:relay debug clock#1` | `ci:scripts/check-test-build.sh` |
| `G14.07` | File keychain and its faults | A test build serving a non-default config dir keeps its sealing key in `X/test-keychain.json`, and `X/test-keychain-fault.json` (`locked`, `missing`, `corrupt`, `slow`) makes the next sealed-store operation that goes back to the keychain fail the way the login keychain fails. | n/a | `X/test-keychain.json`; `X/test-keychain-fault.json` | none | none | `event:debug.keychain.fault=ok#fault` `event:debug.keychain.fault=ok#keychain_op` `out:cli:relay status#.seal_status` | `e2e:TestKeychainProviderFaults` |
| `G14.08` | Bad keychain file refuses startup | `relay serve` on a config dir whose keychain store or fault file is not a private regular file, or whose fault file is invalid, exits 1 with `test keychain in DIR cannot be used`; once the file is fixed, the server starts. | n/a | `relay serve` | `cli:relay serve` | none | `code:cli:relay serve#1` `event:server.ready=ok#ready_file` | `e2e:TestKeychainOpenRefusal` |
| `G14.09` | Test clock moves time-dependent features | `relay debug clock set` and `advance` move the time the server judges by, so a credential expires without waiting, and `relay debug clock` reads it back as `{now, offset_ms}`; the default config dir refuses. | n/a | `relay debug clock [set <RFC3339> \| advance <duration>] [--json]` | `cli:relay debug clock` `bridge:admin_op:debug.clock` | none | `event:debug.clock.set=ok#now` `event:debug.clock.advance=ok#offset_ms` `out:cli:relay debug clock#.now` `out:cli:relay debug clock#.offset_ms` | `e2e:TestClockMovesCredentialExpiry` |
| `G14.10` | Presence outcome file | On a non-default config dir of a test build, `X/test-presence.json` answers each gated op `approve`, `deny` or `timeout` without a dialog, read afresh on every request. | n/a | `X/test-presence.json` | none | none | `event:debug.presence.answer=ok#answer` `event:debug.presence.answer=ok#gated_op` | `e2e:TestPresenceOutcomeFile` |
| `G14.11` | Outcome file refuses what it does not list | An op the file does not list, any op when the file is absent, and every op when the file is invalid is refused without a dialog. On the default config dir the test approver approves only `project.grant`, as before, and reads no outcome file. | n/a | `X/test-presence.json` | none | none | `event:debug.presence.answer=ok#source` `event:debug.presence.answer=error/invalid` `audit:control_decision=denied` | `e2e:TestPresenceRefusesUnlistedOp` `e2e:TestPresenceRefusesOpsNotListed` |

## Owner gates

An owner gate is a step that needs the owner's credential or presence. An
agent inside the product (a relay session, or eve's chat agent) must never
complete one. The devbox harness may, with the operator's test credentials:
`devboxpresence` answers relay's presence prompt with the devbox admin
password. A release build has no API or flag that skips a gate. The
test build (`./build.sh --test-build`, tag `relaytest`, run in place of the
release tray) answers each owner gate per op from `X/test-presence.json` on a
non-default config dir, and on the default config dir approves only
`project.grant`. It is checked absent from every release binary, and its
approvals are audited with `presence_approver`. See
[`docs/testing.md`](testing.md#the-test-build).

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
| `mcp.oauth.start` | authenticating an HTTP MCP | gate-mcp-oauth-start-pos (runs the installed binary as `relay serve` on its own dir against a loopback OAuth provider; the token is sealed and reused after a restart) | gate-mcp-oauth-start-neg (no door from a session) |
| `service.register` | a command relay will run as a service | gate-service-register-pos | gate-service-register-neg |
| `project.grant` | creating a project or widening a grant | gate-project-grant-pos | gate-project-grant-neg (no door from a session) |
| `project.rotate_token` | a project's bearer token | gate-project-rotate-token-pos | gate-project-rotate-token-neg (no door from a session) |
| `project.reveal_token` | disclosing a project's bearer token to the operator (`relay project token`) | none (a real-app check; the test approver refuses it) | none (every new verb refuses a session caller) |
| `remote.configure` | the mTLS listener | gate-remote-configure-pos: NOTRUN, it changes the live listener the VM stack uses | gate-remote-configure-neg (no door from a session) |
| `enrolment.create` | issuing a remote identity | gate-enrolment-create-pos: NOTRUN, remote identities are out of scope until G10 | gate-enrolment-create-neg |
| `enrolment.sign` | signing a remote client's certificate | gate-enrolment-sign-pos: NOTRUN, as above | gate-enrolment-sign-neg |
| `enrolment.update` | changing a remote client's grants | gate-enrolment-update-pos: NOTRUN, as above | gate-enrolment-update-neg |
| `enrolment.revoke` | revoking a remote client | gate-enrolment-revoke-pos: NOTRUN, as above | gate-enrolment-revoke-neg |
| `login.bootstrap.mint` | a code that registers a browser passkey | gate-login-bootstrap-mint-pos: NOTRUN, the code is redeemed only by the browser ceremony | gate-login-bootstrap-mint-neg |
| `login.passkey.revoke` | revoking a relay passkey | gate-login-passkey-revoke-pos: NOTRUN, there is no disposable relay passkey | gate-login-passkey-revoke-neg |
| `eve.enrolment.open` | eve's five-minute passkey enrolment window | gate-eve-enrolment-open-pos (closes the window after) | gate-eve-enrolment-open-neg |
| `eve.passkey.revoke` | revoking an eve passkey | gate-eve-passkey-revoke-pos: NOTRUN, relay keeps one eve passkey mirror that each eve's report replaces, so no verify passkey can be revoked through it | gate-eve-passkey-revoke-neg: NOTRUN while the mirror holds no revocable passkey, since relay refuses an unknown or last id before the gate |
| `sealed.reset` | the sealed store | gate-sealed-reset-pos (resets the store of a `relay serve` instance with its own keychain item; the installed store is never reset) | gate-sealed-reset-neg (no door from a session) |

## Threat-model promises

Each promise in [`THREAT-MODEL.md`](THREAT-MODEL.md) has an ID and the rows that
prove it. Every mapped promise has a row with a refusal proof. Attackers are
numbered as in the threat model (`T` is the test-approver build); assets are
A grants, B credentials, C the audit trail, D data outside a grant. A change to
a promise there updates this table in the same PR.

| Promise | Attacker | Asset | Promise (short quote) | Rows |
|---|---|---|---|---|
| `TM1.1` | 1 A sandboxed session | A, D | It cannot read or write outside its grants | `G1.17` |
| `TM1.2` | 1 A sandboxed session | A | It reaches no relay-managed service it was not granted | `G14.04` |
| `TM1.3` | 1 A sandboxed session | B | It reaches no listener that trusts loopback in place of a credential | `G14.04` `G6.04` |
| `TM1.4` | 1 A sandboxed session | B | It obtains or keeps no credential it was not issued | `G9.08` |
| `TM1.5` | 1 A sandboxed session | C | It cannot act without an audit row | `G1.18` `G4.03` |
| `TM1.6` | 1 A sandboxed session | A, D | The file plane never follows a planted symlink | `G13.03` |
| `TM1.7` | 1 A sandboxed session | C | Every file mutation, permitted or refused, is audited | `G13.02` `G13.03` |
| `TM2.1` | 2 An enrolled remote client | A | It reaches exactly its profile | `G10.13` |
| `TM2.2` | 2 An enrolled remote client | A | It stays within its budget | `G10.15` |
| `TM2.3` | 2 An enrolled remote client | B | Revocation ends it, live connections included | `G10.10` |
| `TM3.1` | 3 A prompt-injected agent | A | Grants default narrow | `G2.01` `G3.06` |
| `TM3.2` | 3 A prompt-injected agent | D | Anything reaching outside this Mac is refused unless granted | `G1.17` |
| `TM3.3` | 3 A prompt-injected agent | A | Budgets cap volume | `G10.15` |
| `TM3.4` | 3 A prompt-injected agent | C | Every call is in the audit log with its outcome | `G4.03` `G4.06` |
| `TM3a.1` | 3a Agent output steering the Chief of Staff | A | The scope reaches only its four doors | `G1.25` |
| `TM3a.2` | 3a Agent output steering the Chief of Staff | A | The scope cannot change where the Chief of Staff runs | `G2.09` |
| `TM3a.3` | 3a Agent output steering the Chief of Staff | A | A start goes only to a registered project root or a folder inside it; a remote project and a terminal start on a host are refused | `G1.26` |
| `TM3a.4` | 3a Agent output steering the Chief of Staff | C | A start or send that cannot be recorded is refused | `G4.08` `G4.09` |
| `TM3a.5` | 3a Agent output steering the Chief of Staff | A, C | One send per turn, marked with its origin | `G4.08` |
| `TM3b.1` | 3b A read-only-projects session | A | It writes no project | `G1.27` |
| `TM3b.2` | 3b A read-only-projects session | D | It reads nothing outside the project folders, the template's grants and the CLI's paths | `G1.27` |
| `TM3b.3` | 3b A read-only-projects session | A | A project change ends the sessions that hold one | `G1.27` |
| `TM4.1` | 4 A peer on the network or in the browser | A, B | It gets no tool call or config change without a credential | `G9.04` |
| `TM4.2` | 4 A peer on the network or in the browser | B | It gets no enrolment without the operator's approval | `G10.06` |
| `TM4.3` | 4 A peer on the network or in the browser | B | It can trigger no prompt on the operator's screen | `G9.04` `G7.03` |
| `TM5.1` | 5 Other code running as the user | B | It cannot borrow a service's launch identity | `G5.12` |
| `TM5.2` | 5 Other code running as the user | A | It cannot act through the bridge as another project | `G3.05` |
| `TM5.3` | 5 Other code running as the user | B | It cannot complete a presence-gated action without the operator | `G2.01` `G9.01` |
| `TMT.1` | T The test-approver build | A, B | The test seams are absent from release | `G14.06` |
| `TMT.2` | T The test-approver build | B | The test approver answers only its listed ops | `G14.11` |
| `TMT.3` | T The test-approver build | C | Each approval is recorded before the act | `G4.07` |

`TM5.3` is proven by every gated row's `deny` test; the two rows listed are the
create row in G2 and the mint row in G9.

## Rows proven by CI

`G14.06` is proven by a CI check script, not an end-to-end test. Its `ci:` item
is allowed on this row alone.

## Retired IDs

`G14.05`

A removed row's ID goes here as a code span, and no row reuses it.

## Areas

Each area has a stable name, the code it owns and the journeys that
exercise it. Goals and features above use these names.

```yaml
areas:
  sessions:
    code: [cmd/relay/session_*.go, cmd/relay/router_sessions.go, cmd/relay/sessionhost_client.go, cmd/relay/persistent_session_*.go, cmd/relay/mount_session.go, cmd/relaysessions/**, internal/sessions/**]
    journeys: [session-drop-in, session-drop-in-host, session-drop-in-tool-refused, blank-model-refused, permission-mode-restart, oversized-launch-audit-capped, acme-sandbox-reach, session-chat-lifecycle, terminal-lifecycle, terminal-extra-args, model-list-and-completion, session-chat-resume, chat-tool-search-tokens, session-agent-state, session-codex, slow-route-keepalive, session-host-restart, verify-fixtures-removed, chief-of-staff-send, cos-start, cos-start-outside-root, cos-start-host, cos-read-only-profile]
  sandbox:
    code: [cmd/relay/sandbox_*.go, cmd/relay/session_sandbox*.go, internal/bridge/sandbox*.go, internal/sessions/sandbox/**]
    journeys: [oversized-launch-audit-capped, acme-sandbox-reach, acme-tools-through-bridge, gate-credential-mint-neg, gate-credential-revoke-neg, gate-mcp-register-neg, gate-service-register-neg, gate-eve-enrolment-open-neg, gate-eve-passkey-revoke-neg, gate-enrolment-create-neg, gate-enrolment-sign-neg, gate-enrolment-update-neg, gate-enrolment-revoke-neg, gate-login-bootstrap-mint-neg, gate-login-passkey-revoke-neg, gate-project-grant-neg, gate-project-rotate-token-neg, gate-remote-configure-neg, gate-mcp-oauth-start-neg, gate-sealed-reset-neg, grant-narrowing-live, cos-read-only-profile]
  templates:
    code: [cmd/relay/template_*.go, cmd/relay/ipc_templates.go]
    journeys: [terminal-lifecycle, terminal-extra-args, verify-fixtures-removed]
  projects:
    code: [cmd/relay/project_*.go, cmd/relay/ipc_projects.go, cmd/relay/skills.go, internal/project/**]
    journeys: [v1-conversion-refusal, gate-project-grant-neg, gate-project-rotate-token-neg, gate-project-grant-pos, stale-derived-access-edit, context-number-resave, grant-narrowing-live, gate-project-rotate-token-pos, verify-fixtures-removed, cos-read-only-profile, cos-settings]
  grants:
    code: [cmd/relay/grant_cmd.go, cmd/relay/router.go, internal/project/apply.go, internal/project/grant_widening.go, internal/membership/**]
    journeys: [acme-sandbox-reach, acme-tools-through-bridge, gate-project-grant-neg, gate-project-grant-pos, stale-derived-access-edit, context-number-resave, disabled-tool-refused, grant-narrowing-live]
  mcps:
    code: [cmd/relay/mcp_*.go, cmd/relay/ipc_mcp*.go, cmd/relay/exec_cmd.go, internal/mcp/**, internal/mcpbroker/**, internal/bridge/**, internal/jsonrpc/**]
    journeys: [acme-tools-through-bridge, tool-call-audited, gate-mcp-register-neg, gate-mcp-oauth-start-neg, gate-mcp-oauth-start-pos, gate-mcp-register-pos, disabled-tool-refused, verify-fixtures-removed]
  audit:
    code: [cmd/relay/audit_*.go, cmd/relay/ipc_audit.go, internal/audit/**]
    journeys: [blank-model-refused, oversized-launch-audit-capped, tool-call-audited, gate-credential-mint-pos, gate-mcp-register-pos, gate-project-grant-pos, gate-service-register-pos, context-number-resave, session-chat-lifecycle, terminal-lifecycle, model-list-and-completion, session-chat-resume, chat-tool-search-tokens, gate-project-rotate-token-pos, gate-eve-enrolment-open-pos, gate-credential-revoke-pos, chief-of-staff-send, cos-start, cos-start-outside-root, cos-start-host]
  services:
    code: [cmd/relay/service_*.go, cmd/relay/cli_service.go, cmd/relay/enhanced_services.go, cmd/relay/ipc_service*.go, internal/service/**]
    journeys: [gate-service-register-neg, gate-service-register-pos, service-start-stop, service-restart-on-crash, settings-window-services, session-host-restart, verify-fixtures-removed]
  models:
    code: [cmd/relay/model_*.go, cmd/relay/router_model_host.go, cmd/relay/frontend_model_guard.go, cmd/relay/relay_llm_channel.go, cmd/relay/ipc_models.go, internal/modelbroker/**]
    journeys: [model-list-and-completion]
  login:
    code: [cmd/relay/login_*.go, cmd/relay/ipc_login.go, cmd/relay/eve_*.go, internal/login/**, internal/ceremonylimit/**]
    journeys: [gate-eve-enrolment-open-neg, gate-eve-passkey-revoke-neg, gate-login-bootstrap-mint-neg, gate-login-passkey-revoke-neg, gate-login-bootstrap-mint-pos, gate-login-passkey-revoke-pos, gate-eve-enrolment-open-pos, gate-eve-passkey-revoke-pos]
  credentials:
    code: [cmd/relay/credential_*.go, cmd/relay/api_credential.go, cmd/relay/frontend_*.go, internal/control/**, internal/peertoken/**]
    journeys: [gate-credential-mint-pos, execute-credential-renewal, gate-credential-mint-neg, gate-credential-revoke-neg, slow-route-keepalive, gate-credential-revoke-pos, chief-of-staff-send, cos-start, cos-start-outside-root, cos-start-host]
  remote:
    code: [cmd/relay/enrol*.go, cmd/relay/ipc_enrolments.go, cmd/relay/remote_*.go, internal/enrolment/**]
    journeys: [gate-enrolment-create-neg, gate-enrolment-sign-neg, gate-enrolment-update-neg, gate-enrolment-revoke-neg, gate-remote-configure-neg, gate-enrolment-create-pos, gate-enrolment-sign-pos, gate-enrolment-update-pos, gate-enrolment-revoke-pos, gate-remote-configure-pos]
  hosts:
    code: [cmd/relay/host_*.go, cmd/relay/ipc_host*.go, internal/sshhost/**]
    journeys: [permission-mode-restart, slow-route-keepalive, verify-fixtures-removed]
  files:
    code: [cmd/relay/file_*.go, cmd/relay/audit_file.go, cmd/relay/project_cmd.go, internal/projectfs/**]
    journeys: [file-plane-contained]
  presence:
    code: [cmd/relay/presence_gate.go, cmd/relay/presence_provider*.go, cmd/relay/admin_ops.go, cmd/relay/admin_read_ops.go, internal/presence/**]
    journeys: [gate-credential-mint-pos, execute-credential-renewal, gate-credential-mint-neg, gate-credential-revoke-neg, gate-mcp-register-neg, gate-service-register-neg, gate-eve-enrolment-open-neg, gate-eve-passkey-revoke-neg, gate-enrolment-create-neg, gate-enrolment-sign-neg, gate-enrolment-update-neg, gate-enrolment-revoke-neg, gate-login-bootstrap-mint-neg, gate-login-passkey-revoke-neg, gate-project-grant-neg, gate-project-rotate-token-neg, gate-remote-configure-neg, gate-mcp-oauth-start-neg, gate-sealed-reset-neg, gate-enrolment-create-pos, gate-enrolment-sign-pos, gate-enrolment-update-pos, gate-enrolment-revoke-pos, gate-login-bootstrap-mint-pos, gate-login-passkey-revoke-pos, gate-mcp-oauth-start-pos, gate-remote-configure-pos, gate-sealed-reset-pos, gate-mcp-register-pos, gate-project-grant-pos, gate-service-register-pos, gate-project-rotate-token-pos, gate-eve-enrolment-open-pos, gate-eve-passkey-revoke-pos, gate-credential-revoke-pos]
  sealed:
    code: [cmd/relay/sealed_reset.go, cmd/relay/sealed_verbs.go, cmd/relay/keystore*.go, internal/sealed/**, internal/config/**]
    journeys: [gate-sealed-reset-neg, gate-sealed-reset-pos, gate-mcp-oauth-start-pos]
  doors:
    code: [cmd/relay/doors.go, cmd/relay/cli_verbs.go, internal/bridge/operator_caller.go]
    journeys: []
  logging:
    code: [cmd/relay/logs_cmd.go, cmd/relay/trace_flag.go, cmd/relay/events.go, internal/logging/**]
    journeys: []
  instance:
    code: [cmd/relay/server_core.go, cmd/relay/serve_cmd.go, cmd/relay/platform_headless.go, cmd/relay/config_dir.go, cmd/relay/main.go, cmd/relay/clock*.go]
    journeys: [gate-mcp-oauth-start-pos, gate-sealed-reset-pos]
  tray:
    code: [cmd/relay/trayapp.go, cmd/relay/tray_notify.go, cmd/relay/cocoa_darwin.go, cmd/relay/native_view.go, cmd/relay/icon.go, cmd/relay/platform.go]
    journeys: [settings-window-services]
  settings-ui:
    code: [web/**, cmd/relay/settings_html.go, cmd/relay/ipc_handlers.go, cmd/relay/ipc_overview.go, cmd/relay/overview_seed.go, cmd/relay/status_verbs.go, internal/webassets/**]
    journeys: [settings-window-services, cos-settings]
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
  host, and the extra-args terminal template, whose fixed id is
  `devboxverify-extra-args` and whose name carries the nonce) carry the run nonce and are removed by verify-fixtures-removed,
  including a crashed run's leftovers. So are the
  terminals any journey opened under a `grant-*` state folder or the World
  root, stopped or not.
- `screen` features have no door but the Settings window or tray. They are
  the last candidates for a journey; an API door is preferred when one exists.
- `permission-mode-restart` and `v1-conversion-refusal` are listed for
  completeness. They always read NOTRUN and cover nothing today.
