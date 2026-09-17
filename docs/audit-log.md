# Tool-call audit log

Every tool call that passes through relay is recorded: what was called, by which
project, from which process, with what arguments, and whether it was allowed.
So is every act that issues or revokes a credential — see
[Issuance and revocation](#issuance-and-revocation).

Read it in the tray under **Settings → Tool Calls**, or from a terminal with
`relay audit`.

## What is recorded

Every one of these produces a record, because a refusal is usually the more
interesting event:

| Outcome | Meaning |
|---|---|
| `ok` | The call reached the MCP and returned |
| `error` | The call failed (transport error, unknown tool, or the MCP errored) |
| `tool_error` | The call completed and the MCP answered `isError` |
| `denied` | A resolved credential was refused a tool it may not use |
| `unauthorized` | The credential itself did not resolve |
| `throttled` | A remote enrolment's rate or volume budget was exceeded |
| `pending` | An intent record, written before the call ran and awaiting its completion |

Event kinds are `call_tool`, `list_tools`, `list_skills`, `control_decision`
(see [below](#control-plane-authorization-decisions)), `credential_issued` /
`credential_revoked` (see [below](#issuance-and-revocation)), `model_call` /
`model_list` (see [below](#the-model-endpoint)), `session_launch` /
`session_end` / `session_resume` (see [below](#session-host-events);
`session_bound` is a reserved fourth kind nothing writes yet), and — for
records relay writes about itself rather than about a caller — `mcp_down` /
`mcp_up` (see below).

`throttled` is deliberately distinct from `denied` and `tool_error`: it is the
only one of the three that says the grant was legitimate and the *pattern of
use* was not, which is what exfiltration looks like from the host's side.

One JSONL line per event:

```json
{
  "id": "9c1f…",
  "ts": "2026-08-19T10:22:31.412Z",
  "dur_ms": 412,
  "event": "call_tool",
  "actor": {
    "kind": "project",
    "project_id": "proj_7f2a",
    "project_name": "relay",
    "auth": "token",
    "pid": 41221,
    "proc": "relay",
    "parent": "claude"
  },
  "mcp_id": "fsmcp",
  "tool": "read_file",
  "args": { "path": "/Users/me/notes.md" },
  "args_bytes": 34,
  "outcome": "ok",
  "result_bytes": 20431
}
```

### The actor

Everything in `actor` comes from relay's own resolution or from the kernel, not
from anything the caller asserted:

- `project_id` / `project_name` are taken from the authenticated `StoredToken`,
  the same value relay injects into `_meta.project_id`.
- `auth` is how the caller was identified for a tool call: `token` (a
  project token was presented), `session` (a `project_session` launch
  identity, or a kernel-verified C3 descendant of one — see immediately
  below), or `mtls` (a client certificate on the remote listener). There is
  no `service`-auth tool-call actor: `resolveAuth` (`cmd/relay/router.go`)
  refuses outright any bound launch identity whose kind isn't
  `project_session`, so a service identity never reaches
  `appRouter.CallTool` to be recorded here. A `service`-kind actor is real
  on the model endpoint's own records (`model_call`/`model_list`,
  [below](#the-model-endpoint)) and on `session_end`
  ([below](#session-host-events)); there it carries `actor.service_id`,
  never a pid — attribution by pid is for a per-call caller, and a service
  identity is a fixed name relay resolved, not a process. The retired
  token-less directory-auth mechanism (`cwd`, keyed on a caller-asserted
  working directory) is gone from every path that writes a record; the
  field name is kept only so an old on-disk record still reads correctly.
- **`session` auth and the `project_session` actor kind are real and live**
  (plan-broker-and-sessions.md §2 C3, [`docs/session-host.md`](session-host.md)):
  a tokenless caller admitted either as a session-host root process's own
  bound launch identity, or as a kernel-verified process-tree descendant of
  one, is recorded with `actor.kind: "project_session"`, `actor.auth:
  "session"`, and `actor.session_id` naming the session that vouched for it
  — set by `cmd/relay/audit_call.go`'s `setActor` for `call_tool`/
  `list_tools`/`list_skills`, and by `cmd/relay/audit_model.go`'s
  `modelAuditActor` for `model_call`/`model_list`. This is the actual
  replacement for the retired `cwd` value above, not merely its planned one.
- `pid` is read off the bridge socket with `getsockopt(LOCAL_PEERPID)`, so it
  cannot be forged by the caller. `proc` and `parent` are resolved from it via
  `proc_pidinfo`.

For a caller on the remote listener the actor looks different, because a pid
means nothing across a network:

- `kind` is `remote` and `auth` is `mtls`.
- `client_id` names the enrolment the client certificate resolved to,
  `fingerprint` is that certificate's fingerprint **in full**, and
  `remote_addr` is the peer address. All three come from the connection and
  none of them can be asserted by the caller.
- `pid` / `proc` / `parent` are **absent**, not zero — an omitted field reads
  as "not applicable" rather than "unknown".
- `project_id` / `project_name` are still populated: the caller is remote *and*
  is acting as a project grant, and both facts matter.

Filter on it with `relay audit --kind remote`, or the caller dropdown in the
Tool Calls tab.

`parent` is usually the field you want. `relay mcp` opens a fresh connection —
and often a fresh process — per call, so `proc` names a throwaway subprocess
while `parent` names the agent that actually asked for the tool.

The pid is for attribution only. It is never consulted for an authorization
decision: pids are reusable and racy, which is fine for "who called this" and
not fine for "may they call it".

### The authority a call ran with

A record that carries the tool and the arguments but not the authority cannot
answer *was this call confined?* once an operator has since edited the grant,
and re-reading `settings.json` at query time answers a different question. So a
`call_tool` record carries what was actually in force (ADR-011 decision 7):

```json
{
  "mcp_id": "macmcp",
  "tool": "mail_search",
  "access": "read",
  "allow_external": false,
  "scope": { "mail_accounts": ["Bob"], "mail_mailboxes": ["INBOX"] },
  "outcome": "tool_error",
  "scope_violation": true
}
```

- **`access`** is the operation mode the call ran under — `read` or `write`.
  Relay applies this rule itself and this field is the record of what it
  decided; the *input* (whether a tool is read-only) is the MCP's own
  `annotations.readOnlyHint`. Absent for a call by a service's launch identity (`auth: service`), which
  is not scoped by it.
- **`allow_external`** is the other half of what relay decided by itself
  (ADR-011 decision 2c): whether this grant could call a tool that reaches
  outside the host. It is written as an explicit `true` or `false`, never
  omitted for a scoped call, because **false is the value that matters** — it
  is the resting state, and the one a `denied` on that layer was decided by. An
  omitted key would make "the grant was not given" and "nobody recorded a
  grant" the same record. It is absent only where there was no authority to
  record: a call by a service's launch identity, and events that name no MCP.
- **`scope`** is the resource scope relay injected, taken from the `_meta` it
  assembled rather than from the project, so what is recorded is what went on
  the wire. It carries **only** the fields the MCP declared as
  `scope: "restrict"` in its `contextSchema` — never the whole per-MCP context
  map, because `_meta` is a general channel and a future MCP may pass an API
  key through it. Filtering to declared restrict-fields is both safer and
  domain-blind.
- **`mcp_root`** is the directory relay spawned this MCP with (`--root`), when
  it did. It is a **different fact from `scope`** and must not be read as
  filling in for one: `scope` is what the MCP declared and the grant supplied,
  whereas this is what relay itself put on the command line. An MCP that
  declares no scope at all still has an answer to "which directory did this
  touch", and this is it.

- **`scope_unplaced`** names the fields **the grant set a value for** that the
  MCP's live schema does not declare, so relay could not place them and refused
  the call. It is a field of its own rather than a fourth reading of `scope`,
  because `scope`'s three readings (`null`, `{}`, populated) are all about what
  the *MCP* declares and this is about what the *operator* declared. Folding it
  in would have let `"scope": null` — "this MCP declares no scope field at all"
  — do double duty for "this MCP declares none of the fields your profile set",
  which is exactly the conflation that let a call dispatched with the
  operator's scope removed read, in the log, as an ordinary unscoped MCP
  (issue #42). A record carrying it is always a `denied`. **This is the field
  to alert on**: it means an MCP changed underneath a grant.
- All three are on a **refusal** as well as a completion. A `denied` or
  `throttled` record carries the mode that was in force, the outbound grant,
  and the scope the grant carried, because "which layer refused this, and under what mode?" is the
  question those records exist to answer. Nothing went on the wire for a
  refused call, so what `scope` shows there is the authority the call was
  judged against; an empty `scope` on a `denied` record is itself the finding —
  a grant with no value for a field its MCP declares.
  The one exception is a tool name exposed by more than one MCP the grant
  allows (issue #35): relay refuses without resolving an MCP, and all three
  fields — like `mcp_id` — are per-MCP, so none of them has a single true
  value. The colliding ids are in `error`. Because `--mcp` matches on
  `mcp_id`, reach those records with `--outcome denied`.
- For a **remote** call they are on the **intent** record as well as the
  completion — the intent is the one written before the MCP runs, and an
  authority recorded only on the completion would be missing from exactly the
  record that survives a crash mid-call.

**`scope_violation`** is a field and not an outcome. `tool_error` already means
"the call completed and the MCP answered no", which is what a scope refusal is;
promoting it would inflate a small enum that `--outcome`, the CLI table and the
UI pill all key on. It is set when an MCP marks its own error result:

```json
{"content": [...], "isError": true, "_meta": {"scope_violation": true}}
```

The key `relay/scope_violation` is accepted as an equivalent spelling. The
marker is honoured only when `isError` is true and the value is boolean `true`;
anything else leaves the flag off and the record is an ordinary `tool_error`.
Relay trusts the marker rather than parsing the error text — reading the text
would put domain knowledge inside relay — so the flag says what the MCP said
about itself. It changes no outcome and gates no decision; it exists so
alerting has something to select on. A refusal relay made itself (a tool
outside `allowed_tools`, a read-only grant meeting a mutating tool, a missing
scope value) is `denied`, not this.

### Arguments and results

**Arguments** are recorded with values under credential-like keys replaced by
`[redacted]`. Keys are matched as case-insensitive substrings, so `mcp_token`,
`X-Api-Key`, and `userPassword` are all caught. Redaction walks nested objects
and arrays. The built-in set is `token`, `secret`, `password`, `passwd`,
`apikey`, `api_key`, `api-key`, `authorization`, `credential`, `privatekey`,
`private_key`, `cookie`, `bearer`, `passphrase`; add your own with
`audit.redact_keys`.

**Redaction is the only thing that is rewritten.** What is stored otherwise is
the caller's own bytes: the same bytes the MCP received, with insignificant
whitespace removed and nothing else. Key order, duplicate keys, the spelling of
a number, and every escape are all as the client wrote them (ADR-012). This
matters when you are reading the log as evidence — `args` is a quote, not a
paraphrase. In particular a `\ud800` in the record is a lone surrogate the
client actually sent, not a rendering artefact, and relay passed it through to
the MCP for the MCP to accept or refuse on its own terms.

Over `max_arg_bytes` (4 KiB by default), arguments are stored as a truncated
string with `args_truncated: true` and the original size in `args_bytes`. Every
line stays valid JSON either way. The cap is what makes storing the caller's
bytes affordable: arguments are unbounded (a file write carries its whole
content) and this file is append-only, so the record is bounded first and
faithful within that bound.

**Results** are recorded as size and `isError` only. Tool results carry file
contents, mail bodies, and calendar entries; storing them by default would make
this the most sensitive file on the machine. Set
`audit.max_result_preview_bytes` to store a capped prefix when an investigation
needs one.

`isError` is probed from the MCP result rather than the Go error: a tool that
fails *inside* the protocol returns a normal result with that flag set, so
without the probe every application-level failure would read as a success.

## Configuration

**Auditing is on by default, including on a fresh install.** The `audit` block
in `settings.json` is optional and an absent one means the defaults below —
enabled, with the rotation caps applied — so an install that predates this
feature and one that simply never wrote the block both come up recording, with
no migration. A new install writes the block out explicitly, so what an
operator reads in the file is what relay is doing rather than something they
have to know the code to infer. Changes take effect on relay restart.

```jsonc
{
  "audit": {
    "enabled": true,
    "log_args": true,
    "log_lists": false,
    "max_arg_bytes": 4096,
    "max_result_preview_bytes": 0,
    "ring_size": 1000,
    "max_file_bytes": 33554432,
    "generations": 5,
    "redact_keys": []
  }
}
```

### Turning it off, and what it costs

Only an explicit `"enabled": false` turns auditing off. An absent block never
does, and the two are deliberately distinguishable: an operator who chose off
stays off across an upgrade, while an install that never had the block starts
recording. `enabled` is a nullable field for exactly this reason — a plain
boolean would make "off" and "never said" the same value on disk, and a rewrite
of settings.json would silently turn auditing back on for the one operator who
had decided otherwise.

What is given up by setting it to false:

- **The remote listener will not start.** Auditing is a hard dependency of
  remote access (ADR-010 decision 5): the case for letting a VM reach host
  tools rests on detection, so relay refuses to serve remote traffic
  unrecorded rather than treating it as a degraded mode. This is not weakened
  by the default being on — it is now simply a rule an operator can only reach
  deliberately. It also applies at runtime, not just at launch:
  `RemoteSupervisor` closes a live listener when auditing stops being live.
- **Control-plane authorization decisions go unrecorded.** Every
  `control_decision` above — creating a service, issuing an enrolment, a
  credential refused a class it does not hold — leaves no trace, which is the
  state ADR-015 argues against.
- **Issuance and revocation go unrecorded.** Every `credential_issued` and
  `credential_revoked` below is lost too. Minting still works: turning
  auditing off is a deliberate act written into settings.json, and refusing to
  mint in a configuration relay supports would make it unusable. This is the
  one state in which a credential can be issued with nothing in the log, and
  it is deliberately reachable only on purpose.
- **A passkey login leaves no record.** `/relay/login` is the one surface an
  unauthenticated caller can obtain a credential from, and its outcomes are
  recorded here and nowhere else.
- **`relay audit` stops being ground truth.** It shows an empty log, and an
  empty log is indistinguishable from a quiet one at the command line; the
  Tool Calls tab says the state outright instead.

Local tool calls keep working with auditing off. That is the whole of what
stays unaffected.

`log_lists` covers `list_tools` and `list_skills` events — what tool surface a
credential was shown. Off by default because skill regeneration lists the tool
surface for every project on every MCP reconcile, which buries the calls that
matter.

## Storage and retention

`<config-dir>/logs/audit/toolcalls.jsonl`, mode 0600, size-rotated. At the
defaults that is 32 MiB per file with five backups (`.1` … `.5`), so roughly
160 MiB. Retention is by size rather than by time: no scanning pass, and the
bound is the one that actually matters on a laptop.

A bounded in-memory ring of the most recent `ring_size` events backs the Tool
Calls tab's first paint and its live tail, so opening the tab never re-reads the
file.

## Two records for a remote call

A local call is one record, written after the call completes. A call from the
remote listener is two, sharing one event `id`:

| `phase` | When | Holds |
|---|---|---|
| `intent` | before the MCP is invoked | actor, tool, redacted arguments, `outcome: "pending"` |
| `completion` | when the call returns | the same, plus outcome, duration and result metadata |

A record with no `phase` at all is a single-record (local) event, which is
every line written before this existed.

**An intent with no matching completion is a signal, not noise.** It means relay
invoked an MCP and never learned the outcome — a crash, a kill, or a hang. It is
worth alerting on rather than reconciling away. Since ADR-012 the usual cause
has a record of its own beside it: see `mcp_down` below.

## The mount plane

The 9P mount plane (a `relay-9p/1` ALPN connection to the remote listener)
records under three event kinds of its own. None of the rows it writes is a
tool call: `mcp_id` carries `mount:<id>` for a project's mount grant, and
`mcp_root` the host path the mount exposes — an operator-visible fact, audit-
only, never sent to the client.

| `event` | When | How |
|---|---|---|
| `mount_attach` | once, when a mount session attaches | written **durably before any 9P byte is served**; a session whose attach cannot be recorded is refused, not started |
| `mount_op` | for mutations and refusals while the session is live | see below — reads never appear here |
| `mount_detach` | once, when the session closes | best effort, plus the session's running totals in `bytes_read`, `bytes_written` and `ops` and the reason in `error` |

**Reads are never audited.** A `mount_op` row exists only for a mutation
or a refusal of one: the read plane's confinement is the mount boundary
itself, and a row per read would be volume the log cannot justify. What a
session's reads did is answered by the `bytes_read` total on its
`mount_detach` row.

**A write session is one intent + one completion, not one row per syscall.**
A 9P write opens a handle, writes to it, and closes it — the pair is around
the whole session, with `phase: "intent"` written durably (refusing a
mutation that cannot be recorded, the same rule as ADR-010 decision 5 for
tool calls) and `phase: "completion"` when it ends, sharing the event `id`.
A refused mutation that never opens — no grant, a write on a read mount —
is a single `denied` row: nothing was attempted, so there is no pair.

**`throttled` rows are coalesced.** The first budget refusal in a session
writes a row immediately; further refusals within a minute of the last
written one write nothing. Every refusal is counted whether or not it
produced a row; the rows are the signal, and one per minute is enough
of it.

No new CLI flags: the existing filters select the new kinds, so
`relay audit --kind remote --event mount_op` and
`relay audit --mcp mount:<id>` already find these rows.

## Records relay writes about itself

Two event kinds are not calls. `mcp_down` and `mcp_up` record that an external
MCP's child process died and that it came back (ADR-012):

```json
{"id":"…","ts":"…","dur_ms":0,"event":"mcp_down","actor":{"kind":"relay","auth":"none","proc":"relay"},
 "mcp_id":"fsmcp","outcome":"error","supervision":"down","error":"read response: EOF","scope":null}
{"id":"…","ts":"…","dur_ms":1204,"event":"mcp_up","actor":{"kind":"relay","auth":"none","proc":"relay"},
 "mcp_id":"fsmcp","outcome":"ok","supervision":"restarted","scope":null}
```

- `actor.kind` is `relay`: this is the one record relay writes about itself
  rather than about a caller, so there is no project, no pid, and no
  credential — those fields are absent rather than zero-filled. Select the set
  with `relay audit --kind relay`.
- `supervision` names the transition: `down`, `restarted`, or `abandoned`.
  `abandoned` is an `mcp_down` row too, and means the restart budget is spent
  and relay has stopped trying — that one needs a human.
- `dur_ms` on the closing row is the **outage length**, which is the question
  these rows exist to answer: not "did it flap" but for how long every grant
  naming this MCP was dead.
- A failed individual restart attempt gets no row. It is a step inside an outage
  the `mcp_down` row already opened; every attempt is in the app log.

They are here, and not only in the app log, because of what this file is for. An
operator is told `relay audit` is the ground truth for anything relay gates, and
a dead MCP is exactly the state in which every gated call fails for a reason
that has nothing to do with the grant. Without these rows the log shows a run of
`error` outcomes and no cause, and the only other signal is the client's own
`read response: EOF`.

```
relay audit --event mcp_down          # every external-MCP outage
relay audit --kind relay --tail 200   # outages and recoveries together
```

## Control-plane authorization decisions

`control_decision` is not a tool call. It is written by `RouteRegistrar.authorize`
(`capability.go`) for every request to a frontend control-plane route — the
API ADR-015 classes by blast radius (`read` / `configure` / `grant` /
`execute`) — one record per request, allowed or refused alike, before the
handler runs:

```json
{"id":"…","ts":"…","event":"control_decision","actor":{"kind":"control","auth":"token","cred_id":"5e2a…"},
 "method":"POST","path":"/api/enrolments","class":"grant","transport":"tcp","outcome":"ok"}
{"id":"…","ts":"…","event":"control_decision","actor":{"kind":"control","auth":"token","cred_id":"5e2a…"},
 "method":"POST","path":"/api/mcps","class":"execute","transport":"socket","outcome":"denied","error":"class not granted"}
```

- `method` and `path` name the route the decision was about; `class` is the
  ADR-015 class it was checked against and `transport` is the listener the
  request arrived on (`socket` or `tcp`) — the same axis `ClassReachableOn`
  gates registration on, so a row here and a route's absence from a listener
  are two views of the same boundary.
- `actor.kind` is `control`, a fourth actor alongside `project` / `service` /
  `remote`: this credential is capability-classed, not a tool caller, and
  `--kind control` selects the set. A request refused on the remote
  listener's configuration plane for want of `cli_admin` (ADR-018) is the
  exception: it carries `actor.kind: "remote"` with `client_id`/
  `fingerprint` instead of `cred_id`, since the enrolment's certificate is
  the identity there, not a control-plane credential.
- `actor.cred_id` names the credential the bearer resolved to. It is attached
  as soon as the bearer resolves — before the class check — so a credential
  that authenticates but lacks the class it asked for is still named in its
  own refusal; the whole point of this record is that "a known credential
  attempted something it does not hold" gets the same standing as a denied
  tool call. It is empty only when no credential resolved at all (no bearer,
  or a bearer matching nothing) — there is no credential to name, and this is
  the one case indistinguishable from `unauthorized` on a tool call.
- `outcome` is `ok` for `Allowed: true` and `denied` for `Allowed: false` —
  reusing the existing outcome enum rather than adding a control-plane-only
  value, because a refusal here is the same kind of fact a tool-call `denied`
  is. The refusal reason (`errNoCredential` vs. `errClassNotGranted`) rides in
  `error`, same as any other outcome this log records.

`method` and `path` are read straight off the request line, before
`RouteRegistrar.authorize` has resolved a credential or checked its class —
they are caller-shaped on *every* request that reaches a registered route,
including one from a credential holding no class at all, and including a
refusal, which is the record this log most needs to keep. Left uncapped,
either field would let such a caller write an arbitrarily large row at will:
the same amplification `max_arg_bytes` exists to prevent for tool-call
arguments, except reachable here by a caller with no class to check, purely
by being refused over and over. A credential ADR-015 decision 3 calls "inert
rather than omnipotent" must not be able to erase this file's retention
window through its own refusals.

Both are capped at the point the record is built, so nothing downstream (the
bounded queue, the ring, the rotating file) ever sees an unbounded value:
`path` at 1024 bytes, `method` at 32 — generously past any real relay route or
proxied-service path, and past any real or WebDAV-style HTTP verb. Over the
cap, the value is truncated on a rune boundary and `path_truncated` /
`method_truncated` is set `true`, the same marker shape `args_truncated` uses
for arguments, so a truncated value is never mistaken for a short, genuine
one.

`relay audit`'s table has no columns for method, path, class or transport —
those live in DETAIL, and CALLER shows the credential id:

```
TIME      OUTCOME  PROJECT  MCP  TOOL  MS  CALLER    DETAIL
08:08:46  ok       -        -    -     0   5e2a…     POST /api/enrolments  class=grant  transport=tcp
08:08:47  denied   -        -    -     0   5e2a…     POST /api/mcps  class=execute  transport=socket  class not granted
```

```
relay audit --kind control              # every control-plane authorization decision
relay audit --event control_decision --outcome denied   # refusals only
```

A `control_decision` and an issuance record are both written for the routes
that issue, and neither replaces the other: one says the caller was allowed
through the door, the other says what came out of it. See
[Issuance and revocation](#issuance-and-revocation).

## The model endpoint

`model_call` records one finished call to a model route
([`docs/model-endpoint.md`](model-endpoint.md)); `model_list` records one
`GET /v1/models` listing, gated by `log_lists` exactly as `list_tools` and
`list_skills` are — a caller lists far more often than it calls, and logging
every listing by default would bury the calls that matter. Both go through
the ordinary fail-open path (`Record`, not `RecordDurable`): a model call is
never delayed by, or refused because of, a full or broken audit sink, the
same policy tool calls get.

```json
{"id":"…","ts":"…","dur_ms":238,"event":"model_call",
 "actor":{"kind":"project","project_id":"proj_7f2a","project_name":"relay","auth":"token"},
 "transport":"socket","method":"POST","path":"/v1/chat/completions",
 "model":"vCode","model_canonical":"vCode","model_target":"ep/gpt-x",
 "request_bytes":812,"response_bytes":4096,
 "prompt_tokens":120,"completion_tokens":340,
 "status":200,"outcome":"ok"}
```

- **`actor.kind`** is `project` for a caller authenticated by its own project
  token or by an `rmk_`-prefixed model key minted for it — `actor.auth` is
  `token` or `model_key` respectively, and `model_key_label` (below) names
  the key's label when it was one. It is `service` for a launch identity on
  `model.sock` with no bearer header, the tokenless path
  (`docs/model-endpoint.md`'s Auth order §2) — `actor.service_id` names the
  identity there, since a model-endpoint call carries no comparable per-call
  pid the way a tool call does. `project_session` (`actor.auth: session`) is
  live: a session-host root or one of its C3-verified descendants calling
  the model endpoint for its own project (`plan-broker-and-sessions.md` §2
  C1/C2, [`docs/session-host.md`](session-host.md)) is recorded with
  `actor.kind: "project_session"` and `actor.session_id` set
  (`cmd/relay/audit_model.go`'s `modelAuditActor`). `relay audit --kind
  project_session` selects the set.
  A caller that never resolved at all — a bad bearer, or no credential
  presented on a listener that requires one — is `actor.kind: unknown`
  rather than a caller-shaped guess, the same rule a tool call's unresolved
  credential gets. `actor.auth` still names what was *attempted* (`token`,
  `model_key`, or the identity path), or is absent when nothing was
  presented at all (no header, on TCP).
- **`model_key_label`** names the `rmk_` key's label the caller authenticated
  with. **Never the key itself** — there is no field on this record able to
  carry it, a project token's plaintext, or either one's hash.
- **`model`**, **`model_canonical`** and **`model_target`** are three
  distinct facts, not one field read three ways: `model` is what the caller
  asked for; `model_canonical` is what relay resolved it to against its own
  catalog (`docs/model-endpoint.md`'s normalisation); `model_target` is
  relayLLM's own account of which managed alias, endpoint or resolved
  virtual candidate actually served the call, read from its
  `X-Relay-Model-Target` response header. A call relay refused before ever
  reaching relayLLM carries the first two and never the third.
  `request_bytes` / `response_bytes` are counted in relay, and
  `prompt_tokens` / `completion_tokens` are parsed from the upstream
  response when present (`internal/modelbroker/usage.go`) — **never the
  request or response content itself**: no prompt, message, instruction,
  tool definition, audio, or completion text is ever in this record.
- **`outcome`** extends the existing vocabulary with two values
  (`not_found`, `client_abort`) and otherwise carries the model endpoint's
  own reason strings verbatim (`ok`, `denied`, `unauthorized`,
  `remote_project`, `route_not_found`, `host_unavailable`, `bad_request`,
  `body_too_large`, `trailing_data`, `error`, `rate_limited` —
  `docs/model-endpoint.md`'s Audit section has the full list and what
  triggers each). `denied` (outside the caller's grant) and `not_found`
  (absent from the catalog entirely) answer the byte-identical 404 on the
  wire — a caller must not be able to enumerate models outside its grant by
  the shape of the error — and this field is the only place the two are
  told apart.

```
relay audit --event model_call                    # every finished model call
relay audit --event model_list                     # every /v1/models listing (needs log_lists on)
relay audit --event model_call --outcome denied     # refused by grant, as opposed to not_found
relay audit --kind service --event model_call       # what a service identity (TTS, STT, ...) called
```

## Session-host events

Four event kinds record the session host's own lifecycle
(`plan-broker-and-sessions.md` §2 C4; full design:
[`docs/session-host.md`](session-host.md)). Three of them are real and
written today; the fourth is a reserved constant with nothing behind it yet
— named here rather than left silent, since a doc that only describes what
works would misstate exactly the fact this section exists to get right.

| event | written by | when |
|---|---|---|
| `session_launch` | *built* by `cmd/relay/session_launch.go`'s `AuthorizeLaunch`/`newSessionLaunchAuditEvent`; *written* by `cmd/relay/session_routes.go`'s `launchAndRespond` (`d.auditor.Record`) | every `POST /api/terminals` or `POST /api/sessions` create request that reaches `AuthorizeLaunch`, allowed or refused — with two exceptions: a refusal on the *resume* path is restamped `session_resume` instead (`resumeAuditEvent`), never `session_launch`; and two early refusals that never reach `AuthorizeLaunch` at all — a pre-authorize `503` when the session ledger isn't wired (`sessionRoutesUnavailable`) and a request-body decode failure (`400`/`413`, `decodeSessionBody`) — write no record of either kind |
| `session_bound` | **nobody.** The constant exists in `internal/audit/audit.go`; nothing in this repo constructs one. A known, real gap — see below. | — |
| `session_end` | `cmd/relay/router_sessions.go`'s `recordSessionExited`, called from the `SessionExited` bridge handler in the same file | every time relay-sessions reports one of its sessions gone |
| `session_resume` | `cmd/relay/session_routes.go` (`resumeAuditEvent`, `auditResume`) | every `POST /api/sessions/{id}/resume`, allowed or refused, including the two outcomes `AuthorizeLaunch` never sees at all (unknown/deleted session, already-live session) |

`session_launch` and `session_resume` share one args shape
(`sessionLaunchAuditArgs`, `cmd/relay/session_launch.go`) and one actor
shape (`callerAuditActor`: `kind: "control"`, `auth: "token"`, `cred_id`
naming the frontend launch identity or bearer credential that asked for the
launch — this is the *caller*, e.g. eve, never the session itself):

```json
{"id":"…","ts":"…","event":"session_launch",
 "actor":{"kind":"control","auth":"token","cred_id":"launch:service:eve","project_id":"proj_7f2a","project_name":"relay"},
 "args":{"session_id":"3af1…","session_kind":"claude","directory":"/Users/me/relay","sandbox":true},
 "outcome":"ok"}
{"id":"…","ts":"…","event":"session_resume",
 "actor":{"kind":"control","auth":"token","cred_id":"launch:service:eve","project_id":"proj_7f2a"},
 "args":{"session_kind":"claude","sandbox":false},
 "outcome":"denied","error":"session is not a dormant session of this project"}
```

The `session_resume` example above is `session_not_resumable`, the
refusal `AuthorizeLaunch` (`cmd/relay/session_launch.go`) raises before it
ever sets `baseFields.Sandbox` or `baseFields.SessionID` — so the real args
for this particular refusal never carry `session_id` (an `omitempty` field,
left unset) or a `sandbox` value truer than its `false` zero value; a
resume that gets further before being refused, or one that succeeds, can
carry both.

`session_end` carries a `service` actor — relay-sessions itself reported
this, tokenlessly, through the `sessions` capability its own built-in
launch identity holds — naming the session that ended, not the caller's own
scope:

```json
{"id":"…","ts":"…","event":"session_end",
 "actor":{"kind":"service","auth":"none","service_id":"relaysessions","session_id":"3af1…"},
 "args":{"session_id":"3af1…","root_pid":41221,"exit_status":0,"reason":"exit"},
 "outcome":"ok"}
```

**`session_bound` is a known, currently real gap, not an oversight left
undocumented.** The constant was added to `internal/audit/audit.go` up
front, alongside the other three, specifically so a later unit would never
have to edit that file again — but the event itself, which would mark a
`project_session` launch identity successfully binding at Hello (distinct
from `session_launch`, which records relay *authorizing* a launch before
relay-sessions has even spawned anything), has no writer anywhere in this
tree — a real, open gap, not an oversight, per
[`docs/session-host.md`](session-host.md#what-is-not-built-yet).

```
relay audit --event session_launch                 # every launch attempt, allowed or refused
relay audit --event session_end                     # every relay-sessions exit report
relay audit --kind project_session                  # every tool/model call made from inside a session
```

## Issuance and revocation

`credential_issued` and `credential_revoked` record that a credential came into
existence or stopped existing. They are a **different fact from a
`control_decision`**, which says a caller was allowed to reach a route: most
issuance is initiated from a CLI process that reaches no route at all, and the
two HTTP routes that issue would otherwise record "this caller may call
rotate_token" and never "a project token was rotated".

A third event, `config_change`, shares this section and the same
fail-closed path (`RecordIssuance`): a gated act that mutates settings but
issues nothing a holder could authenticate with — registering an MCP or
service, widening a project's grant shape, toggling an enrolment's
`cli_admin` bit, or an enrolment narrowing its own grant over the remote
listener. Calling one of these `credential_issued` would be a lie; leaving
it unrecorded would leave a gap in the one place ADR-017's detection
argument rests on.

```json
{"id":"…","ts":"…","event":"credential_issued","actor":{"kind":"operator","auth":"none","pid":41221,"proc":"relay","parent":"claude"},
 "credential":"api_credential","subject":"cred_5e2a","subject_name":"ci-deploy","grants":["read","grant"],"via":"cli","outcome":"ok"}
{"id":"…","ts":"…","event":"credential_revoked","actor":{"kind":"control","auth":"token","cred_id":"5e2a…"},
 "credential":"enrolment","subject":"hermes-mail","grants":["proj_mail"],"via":"http","outcome":"ok"}
```

- **`credential`** is what was issued or revoked: `api_credential` (a
  control-plane credential, whether minted by `relay credential mint` or by a
  completed login ceremony), `enrolment`, `passkey`, `bootstrap_code`, or
  `project_token`.
- **`subject`** is its identifier. A `bootstrap_code` has none — only its
  SHA-256 is stored — so its `subject` is the anchor's **expiry**, which is the
  only non-secret fact that tells one anchor from the next and is what matches
  a minted code to the registration that later consumed it.
- **`subject_name`** is the human-readable label where the kind has one
  distinct from its identifier: a credential's `--name`, a passkey's display
  name.
- **`grants`** is the ADR-015 class set for an `api_credential` and the granted
  access-profile ids for an `enrolment`. Absent for the kinds that have
  neither.
- **`via`** is how the act was initiated: `cli`, `ipc` (the Settings window),
  `tray` (the menu item), or `http`.
- **`actor.kind`** is `operator` for every door authorized by ownership of the
  config dir — the CLI and the tray's own windows — `control` with a
  `cred_id` for an HTTP door, matching the `control_decision` beside it, and
  `remote` with `client_id`/`fingerprint` for an act reached over the remote
  listener's configuration plane (ADR-018) — an enrolment narrowing its own
  grant via `NarrowGrant`. For an `operator` record `actor.parent` is the
  field that matters: it names the shell or the agent that ran `relay
  credential mint`.
- `subject`, `subject_name` and `grants` are capped at 256 bytes each, with at
  most 64 grant entries, and a cut record carries `issuance_truncated: true`.
  An enrolment's client id arrives in the body of `POST /api/enrolments`, so
  these are caller-shaped in the same way `path` and `method` are, and are
  bounded for the same reason.

A `cli_admin` toggle and a remote narrowing both land here, and both are
greppable by name — the entry in `grants` carries the **resulting state**
(`cli_admin=on`), not just the field name, because for a boolean the
direction IS the content:

```json
{"id":"…","ts":"…","event":"config_change","actor":{"kind":"operator","auth":"none","pid":41221,"proc":"relay","parent":"claude"},
 "credential":"enrolment","subject":"hermes-mail","grants":["cli_admin=on"],"via":"cli","presence_id":"p_9a2…","outcome":"ok"}
{"id":"…","ts":"…","event":"config_change","actor":{"kind":"remote","auth":"mtls","client_id":"hermes-mail","fingerprint":"sha256:91f6…"},
 "credential":"project_grant","subject":"477d9a17-da03-45eb-a433-764f93fe96fc","grants":["allowed_tools"],"via":"remote","outcome":"ok"}
```

The second row is deliberately **not** attributed to the tray process —
`actor.pid` is absent, `actor.client_id` names the certificate that acted,
and `presence_id` is absent too: `NarrowForEnrolment` is ungated by design
(a widening is unrepresentable before this record is ever written), so there
is no presence grant to name. The first row still carries one, because the
toggle that grants `cli_admin` in the first place is a human, gated act.

Two more ops carry no `presence_id`, for the same reason as
`NarrowForEnrolment` rather than by omission: `mcp.unregister` and
`service.unregister` (ADR-018 step 3) are pure removals, and decision 1's
rule is that obtaining or widening a capability is the privileged act,
never narrowing or destroying one. Both still write this same
`config_change` record — the event, the id, and `via` — with
`requireIssuanceAuditor` still enforced ahead of the write, so a `config_change`
for either op with no matching presence event is not the detection signal
it would be for `mcp.register` or `service.register`; it is simply what an
unregister looks like now. `docs/presence-gate.md#what-is-not-gated-and-why-removal-is-not-escalation`
has the escalation argument in full.

```
relay audit --grep cli_admin        # every toggle and every remote narrowing
relay audit --kind remote --event config_change   # only what a VM changed about itself
```

**Nothing here is ever a plaintext, a hash, or key material.** The record is
built from one struct (`CredentialIssuance`) that has no field able to carry
one, so a leak would have to be added there — visibly, in one place — rather
than at one of the doors.

```
relay audit --event credential_issued           # everything relay handed out
relay audit --event credential_revoked
relay audit --kind operator                     # everything done from the CLI or the tray
relay audit --grep hermes-mail                  # one client's whole issuance history
```

### Issuance is fail-closed; revocation is not

ADR-010 decision 5 refuses a remote tool call that cannot be recorded, and the
same argument reaches issuance: `settings.json` is 0600, so anything running as
the owner can already mint `--class grant --class execute`. If the record were
fail-open, that adversary's escalation path would be to break the sink first
and mint silently — the one attack this record exists to detect would be the
one it cannot see.

So **the record is written and synced before the act's point of no return**,
which is the moment the secret reaches a holder:

| act | order | if the record cannot be written |
|---|---|---|
| `credential mint`, `login enrol`, a login ceremony's credential | act, then record, then disclose | the secret is never printed or returned; the command fails and names the inert record it left behind |
| `enrol create` | act, then record, then hand back the bundle path | the enrolment is **revoked**, which also removes the emitted bundle — the client private key is already on disk, so nothing less would be a refusal |
| passkey registration | act, then record, then answer 201 | the stored passkey is removed, which is what makes it unable to sign in |
| `rotate_token` | rotate, then record, then return | the new token is not returned; the old one is already dead either way, so rotate again |

A credential whose secret was never disclosed grants nothing to anybody, which
is what makes each of these a real refusal rather than the theatre ADR-010
warns about. The inert `api_credential` record is deliberately left in
`settings.json` rather than swept: the machine has just shown it cannot be
written to reliably, and the command names the one line that cleans it up.

**Revocation is the other way round.** A revocation narrows a grant, so
refusing to narrow one because the log is broken would make a failing disk the
reason a compromised credential stays live. The act stands, the record is
attempted, and a failure is loud — a non-zero exit from the CLI, an
`slog.Error` from the tray and the API — but never a refusal.

`audit.enabled: false` is neither of these. It is a state an operator chose,
and issuance proceeds in it, unrecorded and legible in `settings.json`.

### How a CLI process records

`relay credential`, `relay enrol` and `relay login` run in a **separate process
from the tray**, and both processes write one rotating log. A CLI process
therefore opens the log itself, `O_APPEND`, and **never rotates it**.

Rotation is the only cross-process-destructive operation there is: it renames
the log out from under every other open descriptor, and the tray holds one for
the life of the app. A CLI process that rotated would leave the tray writing
into a file it had already moved, and repeated CLI rotations would shift that
file out of the generation window entirely. Appending cannot do that to
anybody — each record is one `write(2)` on an `O_APPEND` descriptor, which the
kernel serialises against every other appender, so the two processes interleave
whole lines and never half of one.

Handing the record to the tray over the bridge socket was the alternative, and
it is worse on three counts: the tray is **not running** for a large share of
these commands, which is a supported state, so the append path would have to
exist anyway; a "write this audit record" bridge request is a forgery primitive
spelled out in the protocol; and it would let a busy or wedged tray block a
mint.

The cost is a **soft cap** rather than a hard one. The tray's writer counts
only the bytes it has itself written since it opened the file, so appends it
did not make are invisible to its accounting and the log can exceed
`max_file_bytes` by whatever CLI processes added. Nothing is lost: the tray
rotates on its own accounting eventually and takes the whole file, CLI records
included, into the next generation, and relay's next start re-stats the file
and picks up its true size. Issuance is an operator act at human rate, so the
overshoot is a few hundred bytes per invocation.

`relay audit` reads the file directly, so a record written by one CLI process
is readable by the next with no tray involved at all.

## Fail-open, visibly

Events are handed to a single writer goroutine over a bounded channel. If that
channel is full the event is **dropped and counted** — a tool call is never
delayed by, and never fails because of, the audit sink. The drop count is shown
as a warning in the Tool Calls tab, because an incomplete log that looks
complete is worse than no log.

This is a deliberate choice for local use: logging must not be able to break
your tooling. It is the wrong choice for a remote caller, where the trust
boundary justifies refusing a call that cannot be recorded.

**Remote callers are therefore fail-closed.** The intent record is written and
flushed to disk before the MCP is invoked, and if that write fails the call is
refused and the MCP never runs. Writing *after* the call, as the local path
does, would make the refusal meaningless: by the time the write fails the
mailbox has already been read. The cost is that a remote tool call can fail
because auditing failed — the one place this design knowingly trades
availability for evidence (ADR-010 decision 5).

The guarantee ends where auditing does: with `audit.enabled` set to false there
is no sink to fail, and remote calls proceed unrecorded. The Tool Calls tab says
so outright rather than showing an empty table.

If the log file itself can't be opened at startup, relay logs the error and runs
with auditing off. The Tool Calls tab says so rather than showing an empty table.

## Reading it from a terminal

```
relay audit                          # 50 most recent, as a table
relay audit --tail 200 --outcome denied
relay audit --kind remote                 # everything any VM did
relay audit --kind relay                  # external-MCP outages and recoveries
relay audit --event credential_issued     # every credential relay handed out
relay audit --kind operator               # every act from the CLI or the tray
relay audit --project proj_7f2a --mcp fsmcp
relay audit --grep read_file --json  # JSONL, oldest first, for piping
relay audit --path                   # print the log path and exit
```

`relay audit` reads the file directly rather than going over the bridge, so it
works when the tray is stopped — which is when you are most likely to want it.

## Where it hooks in

One place for tool calls: `appRouter.CallTool` in `router.go`. Every tool
invocation in the ecosystem funnels through it — `relay mcp`, `relay mcp call`, relayLLM's MCP
client, project shells — because that is also where auth is resolved and the
permission check is made. `ListTools` and `ListSkillBuckets` are instrumented
the same way.

Instrumentation goes through nil-safe helpers on `*auditCall` (`audit_call.go`),
so a router built without a recorder behaves exactly as it did before auditing
existed, and the router code has no `if audit != nil` noise.

Issuance has no equivalent chokepoint and deliberately does not get a synthetic
one: there is no single function every mint and revoke passes through, and the
record has to name **which door** the act came from, which only the door knows.
So `audit_issuance.go` holds the record builder and the fail-closed rules, and
each door — the CLI subcommands, `LoginOps`, the IPC handlers, and the routes
registered through `RouteRegistrar` — calls it with its own `via`. Adding a new
way to issue a credential means adding a call there; nothing catches one that
forgets, which is why the list of doors is short and named here.
