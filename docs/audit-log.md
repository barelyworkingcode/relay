# Tool-call audit log

Every tool call that passes through relay is recorded: what was called, by which
project, from which process, with what arguments, and whether it was allowed.

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
(see [below](#control-plane-authorization-decisions)), and — for records relay
writes about itself rather than about a caller — `mcp_down` / `mcp_up` (see
below).

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
- `auth` is how the caller was identified: `token` (a project token was
  presented), `cwd` ([directory auth](tokens.md#directory-auth-allow_cwd_auth)),
  `service` (a full-access service token), or `mtls` (a client certificate on
  the remote listener).
- `cwd` appears only for directory auth. That grant has no deliberate credential
  hand-off to point at afterwards, so the log is its audit trail.
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
  `annotations.readOnlyHint`. Absent for a service token, which is not scoped
  by it.
- **`allow_external`** is the other half of what relay decided by itself
  (ADR-011 decision 2c): whether this grant could call a tool that reaches
  outside the host. It is written as an explicit `true` or `false`, never
  omitted for a scoped call, because **false is the value that matters** — it
  is the resting state, and the one a `denied` on that layer was decided by. An
  omitted key would make "the grant was not given" and "nobody recorded a
  grant" the same record. It is absent only where there was no authority to
  record: a service token, and events that name no MCP.
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

Optional `audit` block in `settings.json`. Absent means defaults, so an install
predating this feature starts logging with no migration. Changes take effect on
relay restart.

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
  `--kind control` selects the set.
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
relay audit --project proj_7f2a --mcp fsmcp
relay audit --grep read_file --json  # JSONL, oldest first, for piping
relay audit --path                   # print the log path and exit
```

`relay audit` reads the file directly rather than going over the bridge, so it
works when the tray is stopped — which is when you are most likely to want it.

## Where it hooks in

One place: `appRouter.CallTool` in `router.go`. Every tool invocation in the
ecosystem funnels through it — `relay mcp`, `relay mcp call`, relayLLM's MCP
client, project shells — because that is also where auth is resolved and the
permission check is made. `ListTools` and `ListSkillBuckets` are instrumented
the same way.

Instrumentation goes through nil-safe helpers on `*auditCall` (`audit_call.go`),
so a router built without a recorder behaves exactly as it did before auditing
existed, and the router code has no `if audit != nil` noise.
