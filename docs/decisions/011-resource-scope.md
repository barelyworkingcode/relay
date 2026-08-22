# ADR-011: A Client Is an Identity, an Access Profile, and a Resource Scope

**Status:** Proposed
**Date:** 2026-08-22

## Context

ADR-009 made the project model coherent for a client on another machine.
ADR-010 made that client reachable and gave it an identity. Neither bounded
*what it may reach once connected*, and the live configuration on this host
shows the gap exactly:

    enrolment hermes-vm-2  ->  project "Hermes Mail"  ->  allowed_mcp_ids: ["macmcp"]
                                                          context: (none)
                                                          disabled_tools: (none)

That grant reads **both** fixture accounts and **every** folder in each, sends
mail as either identity, moves messages between mailboxes, writes files to
arbitrary absolute paths on the host — and reaches all 47 of macMCP's tools,
because the grant's unit is the MCP and macMCP is thirteen domains wide. It
holds screen capture, microphone capture, arbitrary Shortcut execution, an
outbound HTTP channel, the whole address book, the whole calendar, and
`messages_send`. Relay's permission model stops at the MCP and, below that, at
a per-tool *denylist*. There is no way to say *this client may read Bob's
INBOX, and nothing else, and may not write.*

The owner's target is several agents on one VM, each authenticating with its
own identity and each confined differently: client A reaches these mailboxes
with these operations, client B reaches others, possibly read-only. The threat
model is unchanged from ADR-009 — **exfiltration, not privilege escalation** —
with one exception this ADR closes, where it is escalation (finding 1).

Three constraints, stated in the owner's order of priority:

1. **Security.** A client can only do what it is allowed to do.
2. **Operability.** An operator must be able to express that without it being
   easy to make a mistake. A configuration UI that is hard to get right
   defeats constraint 1 rather than trading against it.
3. **Simplicity.** Tight, not defensive sprawl. Every mechanism here has to
   pay for itself.

Scope of this ADR is **MCP tool access and mail resources**. Calendars,
contacts and iMessage share the shape and are deliberately not built here.

## What a review of the current tree found

Verified against `relay` at `df8683a`, `macMCP` at `df8683a`, `fsmcp` at HEAD.
Findings 2–8 restate issue #16's, re-verified; findings 1 and 9 are new.

1. **macMCP hands a mail-only remote client an arbitrary filesystem write.**
   `mail_save_attachment` takes `destination` — "Absolute POSIX path … (created
   if missing)", **required** — and `mail_get_source` takes `save_to`, both
   unscoped (`MailService.swift:5859`, `:5882`). ADR-009's premise is that "the
   VM never touches the host filesystem, so whole tool classes (`fs_*`,
   `bash*`) do not apply"; macMCP reopens that class from inside a mail grant.
   A remote client can write `~/.zshrc` or `~/Library/LaunchAgents/*.plist`.
   That is **escalation, not exfiltration**, and it is reachable in the live
   configuration today — the audit log shows `mail_save_attachment` already
   called through `hermes-vm-2`.

   Note the corollary: a remote client cannot *read back* a file it wrote (it
   holds no filesystem grant), so these parameters have close to zero utility
   remotely and are pure liability.

2. **macMCP declares no `contextSchema`.** `main.swift:60-71` returns
   `serverInfo` with `name` and `version` only. It cannot be narrowed and would
   ignore a narrowing if relay attempted one.

3. **macMCP drops `_meta` on the floor.** `main.swift:95-101` reads
   `params.name` and `params.arguments`; a grep for `_meta` across `Sources/`
   returns nothing. `ToolHandler` is `(JSONObject?) -> MCPCallResult` — there is
   no side channel to a handler at all. Relay has been injecting
   `_meta.project_id` into every macMCP call since ADR-007 and macMCP has never
   read it.

4. **macMCP's mail tools default to everything.** `resolveTargets`
   (`MailService.swift:1596`) expands an omitted `account` to every configured
   account plus the local pass; `mail_search`'s `mailbox` defaults to `all`.

5. **`account` and `mailbox` are already ordinary tool arguments** on all 11
   mail tools. Scoping is therefore not "inject and done": the MCP must
   reconcile a caller-supplied argument against a relay-supplied scope, and
   that rule has to be stated once rather than invented per tool.

6. **There is no operator path to `context` at all.** `projectCreateFields` and
   `projectUpdateFields` (`project_apply.go:8-39`) carry no `context` field;
   `web/src/app.js` never mentions it. `Project.Context` is only ever *derived*,
   by the one hardcoded rule in `SyncProjectToken`.

7. **`ValidateProjectGrants` matches a literal field name** —
   `schemaHasField(schemas[mcpID], "allowed_dirs")` (`settings.go:493`, `:527`).
   ADR-006's principle is that service-specific knowledge lives in services.
   More importantly it is the *wrong rule*: it encodes "filesystem" where it
   means "derived from the project's path".

8. **The one enforcing MCP fails open on empty, and separately escapes its own
   bound.** `validatePath` (`fsmcp/src/security.ts:13`): `if
   (allowedDirs.length === 0) return null; // no restrictions`. And its
   `realpath`-with-lexical-fallback resolution lets a symlink inside an allowed
   directory escape it whenever the target does not exist yet — which for
   `fs_write` creating a file is the normal case. Confirmed against the shipped
   build: a write to `<allowed>/link/new.txt` reports success and lands outside.
   Two further fail-open shapes were found alongside the first: `fs_glob` and
   `fs_grep` fell back to `process.cwd()` when `path` was omitted, bypassing
   `validatePath` altogether. See decision 10.

9. **`disabled_tools` is a denylist, so it fails open twice over.** Granting
   `macmcp` grants all 47 tools minus whatever was enumerated *at grant time*.
   Add a tool to macMCP tomorrow and every existing remote grant silently gains
   it. This is the same fail-open shape ADR-009 decision 3 named for
   `allowed_dirs`, one level up.

   **It also fails open on the tools that already exist**, which is the more
   urgent half and was measured rather than reasoned. Every one of the
   following was called through the live `hermes-vm-2` enrolment — a grant
   whose name is "Hermes Mail" — and **relay authorized every one**:
   `capture_screenshot` (reached `/usr/sbin/screencapture` and failed only on a
   missing Screen Recording TCC grant, not on any permission relay holds),
   `capture_audio`, `shortcuts_run`, `web_fetch`, `contacts_list_groups`,
   `shortcuts_list`. The ones that returned nothing returned nothing because
   the host has no shortcuts and no contacts, not because anything refused
   them.

   So a profile created to read one mailbox holds screen capture, microphone
   capture, arbitrary macOS automation, an outbound network channel, the
   address book, the calendar, and the ability to send iMessage as the user.
   The access mode in decision 2 does **not** close this: `contacts_*`,
   `calendars_*`, `messages_get_chat` and `web_fetch` are all legitimately
   read-only, so a `read` profile keeps every one of them. An allowlist is the
   only thing that does (decision 2b).

   Relay already receives what it needs to fix this and throws it away:
   `mcp.Tool.Annotations` (`mcp/types.go:25`) is carried as `json.RawMessage`
   and read by nothing. macMCP populates `annotations.readOnlyHint`
   (`main.swift:85`) — but **not** on `mail_save_attachment`, `mail_move` or
   `mail_mark_read`, two of which mutate, and it sets `readOnlyHint: true` on
   `mail_get_source`, which writes a file when `save_to` is given.

Issue #16's finding 6 (`schemaHasField` missing the nested schema shape) is
already closed by issue #17, commit `934325b`, and is not restated.

## Decision

### 1. The unit of authority stays two records, and the remote one is renamed

A client's authority is the pair `(enrolment, access profile)`: the enrolment
is the certificate and the budget, the profile is what may be done. The obvious
simplification is to collapse them — put the capabilities directly on the
enrolment, so one client is one record.

**Rejected, for one concrete reason: certificate rotation must not require
re-authoring the permission set.** `hermes-vm-1` was revoked and `hermes-vm-2`
created during ADR-010's own testing; that will happen again. Under a collapsed
model every rotation retypes every mailbox name, which is a fresh opportunity
to make constraint 1's mistake at exactly the moment nobody is reviewing
carefully. Keeping the profile separate also lets several agents share one
reviewed confinement, which is the `hermes-mail` / `hermes-cal` /
`hermes-triage` shape ADR-010 already draws.

**What is wrong is the word.** A remote project is not a project: it has no
directory, no skills, no shell, no models, no session. Calling it one invites
the reader to expect all of those, and it is the reason the model reads as a
poor fit. Every operator-facing surface — Settings UI, `relay enrol`, the audit
CLI, this ADR — calls a remote-kind record an **access profile**. `kind:
"remote"` on disk is unchanged: renaming a storage key is migration risk for no
security gain, and `IsRemote()` is already the only place the distinction is
made.

The enrolment listing gains the profile's *effective* authority inline — MCPs,
mode, scope — so "what can this client do" is answered in one place without
mentally joining two records. That is the real cost of two records and it is
paid in the UI, not in the model.

**This decision is the one most worth overruling**, and it is cheap to reverse:
it changes naming and the UI, not the enforcement path.

### 2. Authority has three layers, and relay enforces the two it can verify

For each `(profile, MCP)` pair:

| layer | field | who decides | what relay can stand behind |
|---|---|---|---|
| which MCP | `allowed_mcp_ids` (exists) | relay | the whole thing |
| which tools | `allowed_tools` (**new**, decision 2b) | relay | the whole thing |
| which operations | `access: "read" \| "write"` (**new**) | relay | the rule, not the input |
| which resources | `context` → injected `_meta` (mechanism exists) | the MCP | nothing |

The third row needs its qualifier stated rather than rounded up. Relay applies
the mode itself, at its own chokepoint, and the decision is visible in the
audit log and in what `ListTools` returns — so an operator can see it was
made. But the *classification* of a tool as read-only comes from the MCP's own
`readOnlyHint`. An MCP that mislabels a mutating tool defeats the mode.

That is still meaningfully stronger than the resource layer, in two ways worth
being precise about. Relay can tell you with certainty that it applied the rule
and what it decided; for scope it cannot tell whether the MCP did anything at
all. And a false `readOnlyHint` is a lie told in the MCP's *published tool
list*, which an operator can read, `relay mcp list --schema` prints, and a
reviewer can diff — whereas an ignored `_meta` leaves no trace anywhere.

The third layer is the one issue #16 proposed and it is genuinely
unverifiable — relay cannot check that a returned message came from INBOX
without parsing mail results, which is the ADR-006 line. The second layer is
new here, and it is the one relay *can* decide by itself, at its own
chokepoint, from a declaration the MCP already publishes. It is also the direct
answer to "read-only or read-write", which is how an operator actually thinks
about a client.

**`access` is enforced in `checkToolAccess` and fails closed.** A tool is
admitted to a `read` profile only if its `annotations.readOnlyHint` is
explicitly `true`. Absent, malformed, or `false` all mean *mutating*. This is
the rule that makes finding 9 safe: a tool added to an MCP after a grant was
written is denied to every read-only profile until someone annotates it, rather
than silently granted.

`write` implies `read`. There is deliberately no third mode: no one has named
one, and an enum with a speculative member is a migration cost paid in advance.

**Defaults are asymmetric, deliberately.** An access profile with no `access`
set for an MCP defaults to `read`; a local project defaults to `write`. The
asymmetry is the same one ADR-009 and ADR-010 apply everywhere else — the
threat model differs — and it means this ADR landing turns the live `Hermes
Mail` profile read-only until an operator says otherwise. That is the safe
direction, it is loud in the UI, and it is the whole point.

### 2b. An access profile names the tools it may call, and a denylist cannot do it

The MCP is the wrong unit of grant. macMCP is one MCP and thirteen domains, and
finding 9 measured what that costs: a profile called "Hermes Mail" holds screen
capture and `messages_send`. Nothing in decisions 2, 3 or 5 addresses it —
scope confines mail *within* the mail tools, and the mode filters mutation
across all of them, but neither keeps a mail client out of the address book.

**An access profile carries `allowed_tools` per MCP: an explicit enumeration,
patterns permitted (`mail_*`), the bare `*` refused, absent meaning none.**

This is ADR-009 decision 4's reasoning applied one level down. It refused
`allowed_mcp_ids: ["*"]` on a remote grant because "registering a new MCP on
the host silently widens what another machine can reach — with no action taken
against the project itself and no diff to review." Registering a new *tool* is
the same event at finer grain, and it happens far more often: `macmcp` grew
from 46 tools to 47 during ADR-010's own testing.

**Patterns are permitted, and that is not a hole**, because the layers compose.
`mail_*` does admit a future `mail_delete_everything` — but that tool is still
denied by `access: "read"` unless someone annotates it read-only, and still
confined to the profile's mailboxes by `mail_accounts`, whose `applies_to` is
the same `mail_*`. A pattern that widens the tool list cannot widen the
resources or the operations. Without patterns an operator maintains eleven tool
names by hand and the feature goes unused, which is constraint 2 defeating
constraint 1 by a slower route.

**`disabled_tools` stays for local projects and is not extended to profiles.**
Subtracting from everything is coherent when the caller is same-user on the
same machine and the denylist is a convenience; it is not coherent as the
boundary against another machine. Two mechanisms for one concept is a cost, and
it is paid deliberately rather than by migrating every existing local project's
denylist into an allowlist it did not ask for. A profile that sets
`disabled_tools` is refused at validation, naming `allowed_tools` — an inert
control is the thing ADR-009 decision 2 refuses at the door.

The four layers are then four allowlists, each failing closed, each answering a
different question, and none able to widen another: **which MCP, which tools,
which operations, which resources.**

### 3. The context schema carries five keywords and no field names relay knows

Relay must store, inject, render and refuse a scoping value without knowing
what it scopes. It can, if the schema describes each field's **role in the
permission model** rather than its meaning. A `contextSchema` field keeps its
JSON-Schema-ish fragment (`type`, `items`, `description`) and gains:

| keyword | values | what relay does with it |
|---|---|---|
| `scope` | `"restrict"` | this field narrows access; the rules below apply. Absent means an ordinary context value relay injects and otherwise ignores. |
| `source` | `"operator"` \| `"project_path"` | who supplies the value (decision 5) |
| `applies_to` | tool-name globs | which of this MCP's tools the field governs |
| `enumerable` | bool | the MCP can list this field's valid values (decision 6) |
| `depends_on` | field names | enumeration ordering |

Relay learns "this field restricts access, an operator sets it, and it governs
`mail_*`". It never learns what a mailbox is. The field name is an opaque map
key from relay's side start to finish.

The rejected alternative is a registry inside relay mapping known field names
to known handling. That is what `schemaHasField(…, "allowed_dirs")` is today in
miniature, and it does not survive a second MCP.

**The shape is fixed as the flat form** — `{fieldName: {fragment}}` — and
documented, because issue #17 showed the ambiguity is live and its failure
direction is fail-open. `contextSchemaVersion: 2` in `serverInfo` marks a
schema using these keywords. An absent version means v1 and is handled exactly
as today, literal `allowed_dirs` branch included, for one release.

**Five keywords, not eight.** The proposal in issue #16 also carried `absent`,
`wildcard` and `ui`. Each is dropped with a reason:

- **`absent`** declared whether a missing value means deny or unrestricted. A
  field that says `scope: "restrict"` and then defaults open is not a
  restriction, and relay could never verify the claim either way. So
  `scope: "restrict"` *means* fail closed, and the keyword that let an MCP say
  otherwise is a knob whose only setting is the wrong one.
- **`wildcard`** let an operator type an explicit "all". Dropped entirely: on
  an access profile the wildcard was already going to be refused (ADR-009
  decision 4's reasoning — a grant must be an enumeration someone typed, not a
  value that widens when the host's configuration changes), and on the
  `project_path` side there is nothing for it to mean. An operator who wants
  every account lists every account, and a new account added later does not
  silently join the grant.
- **`ui`** was a rendering hint. `type` plus `enumerable` already determines
  the widget, and fsMCP's existing `ui: "directory-list"` has never had a
  consumer.

**An attestation echo is deliberately not part of this.** Issue #16 proposed
that a compliant MCP echo the scope it applied so relay could warn when one
never came back. It defends against an MCP that ignores `_meta` through
negligence — but *declaring the field in `contextSchema` is already that
assertion*, and an MCP that stopped enforcing while still declaring would
happily still echo. The echo therefore adds no information relay does not
already hold. What catches a regression is an end-to-end test that asserts the
confinement, which decision 9 requires.

### 4. Absent and empty are refusals, on all three sides

Generalising ADR-009 decision 3 from one field to the mechanism:

**The MCP's contract.** A `scope: "restrict"` field missing from `_meta`, or
present and empty, means the server **refuses every call governed by it**
(`applies_to`). Not "falls back to a CLI default", not "no restriction". fsMCP
must change (finding 8); macMCP is being written to this contract from the
start.

**`_meta` present is what makes a call governed, and this is the MCP's own
test, not relay's.** The obvious reading — "a call is scoped if it carries one
of my restrict fields" — has a hole in the fail-open direction: relay failing to
inject a field for any reason produces a call that looks, to the MCP, exactly
like an unscoped one. The MCP would then be relying entirely on relay's
call-time presence check to have run, which is one check and not two.

Relay injects `_meta.project_id` on **every** mediated call and has since
ADR-007. So the presence of `_meta` at all is a reliable signal that a
chokepoint mediated this call, and the MCP can require its own declared
restrict fields on that basis — reading its own `applies_to`, needing nothing
from relay beyond the fact of mediation. An absent `_meta` means nobody
mediated: an operator running the MCP over stdio by hand, which is same-user
local access equivalent to opening Mail.app, and behaves exactly as today.

This is the belt-and-braces ADR-009 decision 3 called for, in the one place it
was still missing, and it is genuinely independent: relay's check lives in
`CallTool`, the MCP's lives in the MCP, and neither is derived from the other.

**It applies to local projects too, and that is the deliberate part.** A local
project granted an MCP that declares an operator-set restrict field must set a
value or lose the tools that field governs. The asymmetry in decision 2 is not
extended here, because the two cases are different in kind: a *mode* has a
defensible default in each direction (`read` is safe, `write` is what a local
project already had), whereas a *scope* has no default at all — there is no
answer to "which mailbox" that relay could pick and be right about. So mode
defaults asymmetrically and scope is always required, and the reason is that
one of them has a safe wrong answer and the other does not.

**Relay's contract.** Relay writes a non-empty, type-conformant value or it
refuses the operation. Never a placeholder, never `[]`, never `null`, never the
field omitted while the grant stands.

**The operator's contract.** "No restriction" is not expressible as emptiness,
and with `wildcard` dropped it is not expressible at all — it is spelled by
enumerating, or by not granting the MCP.

**What the MCP cannot tell is *who* mediated.** `_meta` present means some
client sent it, not that relay did. Any MCP client that sends `_meta` for its
own reasons therefore loses every governed tool. That is the fail-closed
direction and is accepted, but it is a compatibility surface rather than a
relay-only rule, and it is the reason this test belongs to the MCP: an MCP that
tried to identify relay specifically would be trusting a caller's assertion
about its own identity, which is the thing neither side may do.

**Presence is re-checked at call time**, in `CallTool`, against the MCP's
*live* schema, and a missing value is `denied`. This is the third defence and
the only one *in relay* that catches an MCP which grows a scope field *after* a
grant was validated — the runtime-discovery argument ADR-009 gave for defending
`allowed_dirs` twice, generalised. `denied` is the right outcome because relay
made the decision.

### 5. `source` replaces the hardcoded name, and unifies local with remote

This is the decision that marries the two models, and it is the reason the
directory case and the mailbox case are one mechanism rather than two.

- **`source: "project_path"`** — relay derives the value from `Project.Path`.
  An access profile has no path, so such a field is **absent** for one, and by
  decision 4 the tools it governs refuse.
- **`source: "operator"`** — an operator sets it explicitly. Local and remote
  alike. Nothing about a mail account depends on the caller having a
  filesystem.

`ValidateProjectGrants` stops asking "does this MCP declare `allowed_dirs`" and
asks the general question: **would this grant leave the MCP with no usable
tools?** For fsMCP that is yes — every tool is governed by a `project_path`
field that a profile cannot supply — so the grant is refused exactly as today,
by a derived rule instead of a hardcoded string. For macMCP it is no: only
`mail_save_attachment` and `mail_get_source` are governed by its `project_path`
field, so the MCP is grantable and precisely those two lose their filesystem
write. **This is finding 1's fix**, and it arrives as a consequence of the
model rather than as a special case.

It also improves the *local* side, which today is unbounded: a local project
granted macMCP can currently write an attachment anywhere on the host. Under
`source: "project_path"` it writes inside its own project directory and nowhere
else.

`SyncProjectToken` keeps its independent second defence in generic form: never
derive a `project_path` field for a remote-kind record, unconditionally, before
the loop. Belt-and-braces for the reason ADR-009 gave — the failure mode is a
silent widening, and schemas are discovered at runtime.

**Rejected: a third source, `"enrolment"`.** Scope on the certificate rather
than the profile. It is the same collapse decision 1 rejected, and it adds the
confused-deputy shape ADR-007 spent a PR removing: the same profile would mean
different things to different callers.

### 6. Enumeration is a separate request, and the UI is a picker over real values

Constraint 2. Typing `INBOX` by hand is the error-prone step, and under
decision 4 a typo now fails *closed* — the agent silently gets nothing, which
is safe and baffling. So the editor should not ask an operator to type a
resource name at all.

An MCP answering `context/enumerate` returns `{field: [{value, label}]}` for
fields declaring `enumerable: true`; relay honours it for those fields only and
sends already-chosen values back as parameters for fields declaring
`depends_on`, so the UI fills in dependency order (mailboxes cannot be listed
without an account). macMCP implements it by delegating to the code
`mail_list_accounts` and `mail_list_mailboxes` already call.

**Rejected: declaring an existing tool as the enumerator** (`"enumerate":
{"tool": "mail_list_accounts", "path": "$.accounts[*]"}`). Cheaper to build and
worse to live with: it routes an operator-UI read through `appRouter.CallTool`,
so it lands in the audit log as a tool call nobody made, consumes ADR-010
budget, and must run with relay's own unscoped authority through the chokepoint
that exists to constrain agents. It also makes relay extract values from a
free-form result, which is a path expression per MCP — domain knowledge by the
back door.

**Enumeration is itself disclosure** and should be named as one: the list of
every mail account on the machine becomes readable by anything that can open
the Settings UI or reach the project routes. That is host-local admin surface
today, so it is acceptable — but it is a new read path.

**The wire shape**, pinned so both halves interoperate:

    -> {"method":"context/enumerate",
        "params":{"field":"mail_mailboxes","values":{"mail_accounts":["Bob"]}}}
    <- {"result":{"field":"mail_mailboxes",
        "values":[{"value":"INBOX","label":"INBOX"},
                  {"value":"Projects/Archive","label":"Projects/Archive (Bob)"}]}}

`value` is what goes into `_meta` verbatim — for a mailbox, the full path,
never a leaf name. `label` is display only. `values` on the request carries
already-chosen values for `depends_on` fields.

Four outcomes that must stay distinct, because collapsing any two of them
produces a form that lies: `-32601` means the MCP does not enumerate and the
field degrades to text entry, permanently; `-32602` means relay asked for a
field the MCP will not enumerate, which is a relay bug and is surfaced;
`-32000..-32099` (JSON-RPC's implementation-defined server range), any
unrecognised code, or a transport failure all mean the MCP could not answer
*right now*, which is retryable and keeps text entry available so an operator is
never blocked; and an empty list is a valid answer meaning there are none.
**An empty list must never be rendered for a call that failed.**

**An empty `values` entry means "all", never "none".** The dependent field's
picker is opened before its dependency has been chosen — that is its normal
initial state — so reading an empty-but-present filter as "match nothing" shows
an operator zero mailboxes everywhere at exactly the moment they are trying to
choose one. Absent and empty are the same request here. Note this is the
opposite of decision 4's rule for a *scope value*, where empty is a refusal, and
the two are not in tension: a scope value is an authorisation and must fail
closed, while a picker filter is a query and must fail informative.

**A stored value that is no longer offered stays visible and selected**, marked
unrecognised. An account renamed on the host must not quietly widen or narrow a
profile by vanishing from a form — a permission that changes because a list
refreshed is the failure this whole picker exists to prevent.

**Enumeration is not scoped and is not a tool.** It runs for the operator
configuring a profile, not for any client, so it lists the whole host; and it
stays off `tools/list` so it can never become agent-callable or appear in a
generated `SKILL.md`. It is unreachable from a remote client by construction —
that dispatch table is `ListTools` and `CallTool` and nothing else — and it
sits behind the same admin authentication as the other project routes.

**A raw JSON editor is the fallback, not the plan.** The owner offered one as a
first draft. It is retained for MCPs that do not implement `context/enumerate`,
and for those it is the honest surface. Where the MCP *can* enumerate, a
free-text box would be a UI whose easiest failure is a confinement that does
not confine what the operator thought — constraint 2 defeating constraint 1
rather than trading against it. **Every value is validated against the declared
fragment on save, whichever surface produced it, and an invalid one is refused
rather than stored.**

### 7. The scope is audited, and a violation is a field rather than an outcome

**Record it.** ADR-008's property is that the log answers what was attempted
with what authority. The authority is the grant *plus* the mode *plus* the
injected scope. A record carrying the tool and the args but not those cannot
answer "was this call confined?" once an operator has since edited the profile,
and re-reading `settings.json` at query time answers a different question. The
values recorded are the ones actually injected, taken from `meta` where
`CallTool` assembles it, on the single record for a local call and on the
**intent** record for a remote one.

**Only declared `scope: "restrict"` fields, never the whole context map.**
`_meta` is a general channel and a future MCP may pass an API key through it.
Logging `Context[extID]` wholesale would make the audit file the place
credentials go to be archived. Filtering to declared restrict-fields is both
safer and domain-blind.

**No new outcome for a scope violation.** ADR-008 already places it:
`tool_error` means the call completed and the MCP answered no — a boundary was
probed and held. `throttled` earned its slot in ADR-010 because a budget
refusal is a decision *relay* makes with relay's numbers; a scope violation is
made inside the MCP and relay is relaying it. Promoting it would require relay
to distinguish it from any other `isError` by parsing a message (the ADR-006
line) or by trusting a marker (a relay decision wearing an outcome's clothes).
An optional structured marker surfaces as the audit **field**
`scope_violation: true` with `outcome` staying `tool_error`, which gives
alerting its signal without inflating a small enum that `--outcome`, the CLI
table and the UI pill all key on. A `denied` from the *mode* check is a
different thing and is already correctly `denied`, because relay decided it.

### 8. A client is told its own limits through `ListTools`

`renderBucketSkillMd` is the obvious place to say "this profile is limited to
Bob's INBOX, read-only" and it is the wrong *only* place: **access profiles
have no skills**, because `validateProjectShape` refuses `GenerateSkill`. The
agent this feature exists for is the one that would never see it.

So relay appends a scope note to the `description` of each governed tool inside
`ListTools`, built from the schema's own `description` and the operator's
value, with `applies_to` selecting which tools are governed (all of the MCP's
when absent — domain-blind by default, MCP-supplied precision when offered).
One implementation reaches the remote listener's `ListTools`, `relay mcp call
--list`, and `ListSkillBuckets`, which feeds the skill renderer; the two list
paths must not double-append. `renderBucketSkillMd` gains a short **Scope**
section from the same data.

The mode needs no note: a `read` profile simply does not see mutating tools,
because `checkToolAccess` already filters `ListTools`.

**The frontmatter `description` does not change.** `synthesizeDescription` is a
500-byte lazy-load *routing* signal. "Restricted to Bob's INBOX" does not help
a request route and consumes the budget that makes routing work.

### 9. Nothing ships until the confinement is demonstrated end to end

Relay injecting a scope that macMCP does not enforce is **worse than no scope
at all**, because the UI would then assert a confinement that does not exist.
The sequencing that prevents it is structural rather than procedural: relay
only offers scope for fields an MCP *declares*, and macMCP declares none until
it enforces them. Neither half can lie about the other.

The demonstration is not a unit test. Two access profiles, two enrolments, two
real clients, against the `testMail` fixture's real accounts:

- profile **A** — Bob only, `INBOX` only, `read`
- profile **B** — Alice only, `INBOX` + `Archive`, `write`

and the assertions that matter are the negative ones: A cannot read Alice at
all, cannot read Bob's `Archive`, cannot send, cannot move, cannot mark read,
cannot write a file; B cannot read Bob; neither can save an attachment to an
arbitrary path; an explicit out-of-scope argument is an **error** rather than a
silent narrowing. Ground truth is relay's audit log and the fixture's Maildir
on disk — never a client's own report of success. That standard is inherited:
ADR-010's client passed 95 tests and an adversarial review while its primary
interface was broken, because nobody had executed the string the generator
printed.

### 9b. What an editor may save, and what it must refuse

Building the editor forced four questions the ADR had not answered. The rule
that settles them: **refuse what cannot be made true, permit what is merely
incomplete, and never let a save silently delete something.**

- **An incomplete profile may be saved.** Refusing would make it impossible to
  grant an MCP before typing a scope. A missing value is a warning in the
  editor, a warning in the list, and a `denied` at call time — loud in three
  places without making the editor unusable.
- **A value for a v1-schema MCP is refused.** The v1 branch of
  `SyncProjectToken` replaces the whole context blob, so a stored value would
  vanish at the next path or grant edit. A silent disappearance is worse than a
  refusal.
- **`permission_policy` and `chat_templates` are refused on a profile**, joining
  path, cwd-auth, skills, shell templates and models. Both are inert on a record
  that can hold no session, and ADR-009 decision 2's argument — refusing at the
  door is more honest than a control that quietly no-ops — does not stop at the
  fields that ADR happened to list.
- **Editing one thing must not delete another.** Writing an operator's scope
  re-runs the derivation so `source: "project_path"` fields are put back;
  without that, editing a local project's mail scope would silently drop its
  `write_dirs` and disable the two tools that field governs.

### 10. Two seams that are not in the list, and one reference implementation that was wrong

Both were found while building the enforcement, not while writing this ADR, and
neither is a mail-specific accident — each is a shape any scoping MCP will have.

**A cache is a seam.** macMCP holds a fetched message source for 60 seconds
keyed on the message id and account. A cache hit returns bytes *without running
the script*, and the script is where the scope is checked — so a second,
differently-confined caller reaching the same process is served a message it
may not read, with nothing having decided that. The confinement is therefore
part of the cache key, with unscoped as its own bucket. Generally: **any
memoisation of a scope-governed result must be keyed on the scope**, and the
quietest bypass in this design is the one where no check runs at all.

**A path that does not exist yet is the dangerous one.** The obvious
implementation of a directory bound resolves symlinks with `realpath` and falls
back to a lexical resolve when the path is absent — and for a tool whose job is
*creating* a file, absent is the normal case. A symlink inside the allowed
directory then escapes it, because the lexical resolve keeps the symlink in the
string and the prefix check passes.

This ADR named fsMCP's `validatePath` as the reference implementation for that
comparison. **That was wrong and it is withdrawn.** fsMCP has exactly this bug,
confirmed against the shipped build: `fs_write` to `<allowed>/link/new.txt`,
where `link` points outside, reports success and writes outside. Resolution
must be **component by component**, following symlinks in the existing prefix
and carrying the not-yet-existing tail lexically. fsMCP is being fixed
separately; the correct reference is macMCP's `resolvedForContainment`.

A check-then-use of a path is still racy in principle. Whether that matters
depends on who can create a symlink inside an allowed directory — if that is
only the same user who already runs the MCP, it is not a new capability, and
write-time enforcement (`O_NOFOLLOW`) is the answer if it ever becomes one.

### 11. Three rulings on what a refusal says

**A found-but-out-of-scope message is a refusal, not a "not found".** Because
`byId` resolves globally, the id of a message in another account is a valid
handle, so the MCP knows the difference and has to choose which to say. Saying
"not found" is indistinguishable from a real miss and leaves an operator with
nothing to debug; saying "out of scope" discloses that *some* message holds
that id. The disclosure is real and is accepted: it is bounded by ADR-010's
per-enrolment budget, which makes id-space enumeration impractical, and the
refusal never names the account or mailbox the message is actually in. This is
the same reasoning the reconciliation rule uses to refuse rather than silently
narrow.

**A scope naming a mailbox that does not exist is an error and NOT a
violation.** The two are different events with different audiences: a violation
is a client probing a boundary and belongs in alerting; a nonexistent mailbox is
an operator typo and belongs in the editor. Conflating them would fill a
security signal with configuration mistakes. Note this used to be silent —
`total_messages: 0` with `scan_complete: true`, an affirmative claim of
emptiness about a mailbox that was never there.

**A tool's bookkeeping about the message it just wrote is not a read of the
user's mail.** macMCP's compose path sweeps the sending account's Drafts for
the copy Mail autosaves behind its back, and does so regardless of
`mail_mailboxes`. It matches only ids absent before the compose began that
carry this message's subject, so it can surface nothing but the client's own
message. Scoping it would break the sweep and protect nothing. The account it
runs in is already scope-checked by the sender guard.

## The reconciliation rule the MCP implements

Stated once, here, so every scoping MCP implements it identically:

- An **absent or default-valued** scope-relevant argument resolves **to the
  scope**, not to everything. `mail_search` with no `account` scans the allowed
  accounts.
- A **tool-level wildcard** argument (`mailbox: "all"`) means "everything I am
  allowed to see" and resolves to the scope. It does not error.
- An **explicit** argument outside the scope is an **error**, not a silent
  narrowing. Silent narrowing lets an agent build a false model of what it can
  reach and burn calls discovering the truth; ADR-009's principle is to refuse
  incoherent combinations rather than degrade.
- **Enumerators are scoped too.** `mail_list_accounts` and
  `mail_list_mailboxes` report only what is in scope. An enumerator that lists
  the machine's real account names to a confined client is a disclosure, and it
  is also how that client learns what to try next.

## Worked example

### What macMCP declares

```json
"serverInfo": {
  "name": "macmcp",
  "version": "1.1.0",
  "contextSchemaVersion": 2,
  "contextSchema": {
    "mail_accounts": {
      "type": "array", "items": {"type": "string"},
      "description": "Mail accounts this client may read from or send as",
      "scope": "restrict", "source": "operator",
      "applies_to": ["mail_*"], "enumerable": true
    },
    "mail_mailboxes": {
      "type": "array", "items": {"type": "string"},
      "description": "Mailbox paths within those accounts this client may reach",
      "scope": "restrict", "source": "operator",
      "applies_to": ["mail_*"], "enumerable": true,
      "depends_on": ["mail_accounts"]
    },
    "write_dirs": {
      "type": "array", "items": {"type": "string"},
      "description": "Directories this client may write files into",
      "scope": "restrict", "source": "project_path",
      "applies_to": ["mail_save_attachment", "mail_get_source"]
    }
  }
}
```

`mail_accounts` and `mail_mailboxes` combine as a **cross-product**: accounts
`[Alice, Bob]` with mailboxes `[INBOX]` means both INBOXes. "Alice's INBOX and
Bob's Archive" is not expressible in one profile and is two profiles. This is a
real limitation, accepted for now because the pair covers the stated case and a
per-account mailbox map is a structure the picker, the validator, the audit
line and the reconciliation rule would each have to learn. Mailbox values are
the **full paths** `mail_list_mailboxes` already returns (`Projects/Archive`,
not `Archive`) — the path work in macMCP is what makes a mailbox name
unambiguous enough to be a permission value at all.

### The resulting access profile

```json
{
  "id": "prof_hermes_bob_inbox",
  "name": "Hermes — Bob INBOX (read-only)",
  "kind": "remote",
  "allowed_mcp_ids": ["macmcp"],
  "allowed_tools": { "macmcp": ["mail_*"] },
  "access": { "macmcp": "read" },
  "context": {
    "macmcp": { "mail_accounts": ["Bob"], "mail_mailboxes": ["INBOX"] }
  }
}
```

`ValidateProjectGrants` permits it: macMCP retains usable tools without
`write_dirs`. It still refuses fsMCP, whose every tool is governed by one.
`SyncProjectToken` derives nothing — `write_dirs` is `project_path` and this
record has no path.

### What macMCP receives on `mail_search`

```json
{
  "name": "mail_search",
  "arguments": {"query": "invoice", "limit": 20},
  "_meta": {
    "project_id": "prof_hermes_bob_inbox",
    "mail_accounts": ["Bob"],
    "mail_mailboxes": ["INBOX"]
  }
}
```

`resolveTargets(account: nil)` yields `["Bob"]` rather than every account plus
the local pass; the scan's `mailbox: "all"` resolves to `["INBOX"]`;
`fmInScope` gains the scope as a second condition ANDed with the caller's own
`account` argument; `senderJXA` refuses a `from` whose owning account is out of
scope even though Mail owns the address; `mail_move` checks both the source it
found and the destination it resolved. `mail_get_emails {"account": "Alice"}`
returns `isError` with `scope_violation`, rather than quietly returning Bob's
mail or quietly returning nothing.

### The audit line

```json
{
  "id": "01J…", "ts": "2026-08-22T09:14:02Z", "event": "call_tool",
  "phase": "intent",
  "actor": {"kind": "remote", "auth": "mtls", "client_id": "hermes-bob",
            "project_id": "prof_hermes_bob_inbox", "fingerprint": "sha256:9f2a…"},
  "mcp_id": "macmcp", "tool": "mail_search",
  "args": {"query": "invoice", "limit": 20},
  "access": "read",
  "scope": {"mail_accounts": ["Bob"], "mail_mailboxes": ["INBOX"]},
  "outcome": "pending"
}
```

## Consequences

- **The live `Hermes Mail` profile becomes read-only** when this lands, and
  keeps reading both accounts until an operator narrows it. Decision 2's
  asymmetric default is what does the first half; the second half cannot be
  guessed and must be typed.

- **A read-only profile loses `mail_get_source` entirely**, which is a real
  capability loss and not the one the tool's name suggests. The tool can write
  a file (`save_to`), so it is annotated `readOnlyHint: false`, so the mode
  denies it before `write_dirs` is ever consulted — including for the inline
  read that writes nothing. The layers deny in order and the outer one wins.

  Keeping the annotation truthful is the right call anyway: the mode is relay's
  own check and does not depend on macMCP's scope enforcement being correct,
  which is exactly the independence decision 4 argued for. The clean fix is to
  split the writing half into its own tool so the reading half can be annotated
  honestly read-only; that is deferred rather than done here, because it is a
  tool-surface change and this ADR is already large. A profile that genuinely
  needs raw source today must be granted `write`, which also grants send — so
  the workaround is bad enough to be worth the eventual split.

- **Two macMCP tools lose their filesystem write for every access profile**, and
  gain a project-directory bound for every local project. `mail_save_attachment`
  is unusable remotely by construction — it requires `destination`. That is the
  correct outcome (finding 1) and it removes a capability that has been
  exercised, so it is a behaviour change and not only a hardening.

- **The live `Hermes Mail` profile loses 36 tools**, including screen capture,
  microphone capture, `shortcuts_run`, `web_fetch`, contacts, calendars and
  `messages_send` — everything outside `mail_*`. It never should have held
  them, and it holds them today. This is the largest behaviour change here and
  the one most likely to surprise: a client that quietly relied on any of those
  breaks loudly at the next call.

- **A denylist stops being the mechanism that bounds a remote client.**
  `disabled_tools` remains for local projects only. What bounds a profile is
  `allowed_tools`, and what keeps a *new mutating* tool away from a read-only
  profile is the mode — which works on tools that did not exist when the
  profile was written.

- **Every tool relay serves needs a truthful `readOnlyHint`.** A missing one
  now costs the tool its availability to read-only profiles. That is the
  fail-closed direction, and it makes the annotation load-bearing where it was
  previously decorative — including the three mail tools that lack it today and
  `mail_get_source`, whose `true` is wrong while `save_to` exists.

- **Relay learns five keywords and no field names.** After this, the only
  domain-specific string left in relay is the v1 `allowed_dirs` compatibility
  branch, kept for one release with a deprecation line and a test asserting it
  is the last one.

- **Local projects granted a scope-declaring MCP need a scope too.** The
  presence requirement is not remote-only (decision 4), so the existing local
  `Relay` project — which holds `allowed_mcp_ids: ["*"]` and therefore macMCP —
  loses the mail tools until someone sets one. Loud and closed, and the UI
  names it.

- **Profiles can break after an MCP upgrade**, loudly and closed: an MCP that
  adds a restrict-field makes existing grants unsatisfiable, and
  `SyncProjectToken` will not run again until someone edits settings. The
  call-time presence check (decision 4) is the catch. Accepted deliberately —
  the alternative is a profile that keeps working with no scope, which is the
  thing this ADR exists to prevent. The UI surfaces it as "N profiles need a
  value for `macmcp`" rather than leaving it to be discovered from a `denied`.

- **A `write` mail profile is an exfiltration channel and this ADR does not
  close it.** `mail_accounts` scopes the identity a message is *sent as*; it
  says nothing about who it is sent *to*. A profile holding `mail_send` can
  mail anything it can read to any address. That is inherent in granting send
  to a semi-trusted agent rather than a defect in the scope model, and it is
  why `read` is the default (decision 2) and why the layers are worth having
  separately: a read-only mail profile has no outbound channel at all, once
  decision 2b has taken `web_fetch` away from it. A recipient allowlist is a
  coherent later addition on the same `applies_to` machinery. The fixture hides
  this — its SMTP server refuses non-fixture recipients with 550 — so it must
  not be mistaken for a control that exists.

- **Relay still cannot tell whether an MCP honoured `_meta`.** There is no
  structural answer and this ADR does not pretend one. The mitigations are
  containment, not verification: MCPs are host-side code the operator installed
  and can read, ADR-010's per-enrolment budget bounds the drain regardless, and
  decision 9's end-to-end test is what catches a regression.

### Deferred, deliberately

- **Calendars, contacts and iMessage.** Same mechanism, no new decisions.
  `messages_*` has no resource axis short of per-chat, so a profile that needs
  it grants `messages_*` in `allowed_tools` and is confined by the mode alone.
  Until then decision 2b keeps it out, which is the change that matters.
- **A per-account mailbox map**, replacing the cross-product.
- **Splitting `mail_get_source`'s `save_to` into its own tool**, so the reading
  half can carry an honest `readOnlyHint: true` and stay available to
  read-only profiles. See the consequence above.
- **A recipient allowlist for `mail_send`**, on the same `applies_to`
  machinery, closing the channel named in the consequences.
- **`fs_bash` auto-disable moving into the schema** (`default_disabled_tools`).
  The same ADR-006 violation as finding 7, but it is not resource scoping.
- **Per-tool tri-state permissions** (`allow`/`prompt`/`deny`), still where
  ADR-009 left them. Orthogonal: no mode or scope value expresses "prompt
  before `mail_send`", and no tri-state value expresses "only INBOX". One
  caution for that design — `prompt` must never become a way to approve a scope
  violation interactively, which would move enforcement back inside relay.

## See also

- ADR-006 — "service-specific knowledge lives in services, not in relay", the
  constraint decision 3 answers to and finding 7 shows relay bending.
- ADR-007 — the permission model this extends; the confused-deputy shape
  decision 5 declines to reintroduce.
- ADR-008 — the audit chokepoint, and the outcome enum decision 7 declines to
  extend.
- ADR-009 — decision 3 is the `allowed_dirs` trap decision 4 generalises;
  decision 4 is the wildcard reasoning decision 3 reuses to drop the keyword.
- ADR-010 — decision 3 for the enrolment/grant split decision 1 relies on,
  decision 7 for the `denied`/`tool_error`/`throttled` distinction.
- `relay/settings.go` — `SyncProjectToken`, `ValidateProjectGrants`,
  `schemaHasField`.
- `relay/router.go` — `checkToolAccess`, `mergeProjectID`, `ListTools`.
- `relay/mcp/types.go` — `Tool.Annotations`, carried and unread.
- `relay/project_apply.go` — the DTOs that carry no `context` today.
- `macMCP/Sources/macMCP/main.swift` — `initialize`, and the `tools/call`
  dispatch that drops `_meta`.
- `macMCP/Sources/macMCP/Services/MailService.swift` — `resolveTargets`,
  `fmInScope`, `mailboxInAccountJXA`, `senderJXA`, `MailCall`.
- `fsmcp/src/security.ts` — the fail-open empty case.
