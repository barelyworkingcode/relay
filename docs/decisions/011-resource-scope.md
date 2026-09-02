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

   **Reopened on the read side**, after this ADR's own review missed it —
   see Consequences, "Finding 1 is reopened, on the read side."
   `mail_send` and `mail_create_draft`'s `attachments` read a file from this
   host to send it out, which this finding's write-only framing did not cover
   and which is worse: an unscoped read wired to an outbound channel is
   exfiltration, not the escalation this finding named.

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

### 2. Authority has five layers, and relay decides four of them

For each `(profile, MCP)` pair:

| layer | field | who decides | what relay can stand behind |
|---|---|---|---|
| which MCP | `allowed_mcp_ids` (exists) | relay | the whole thing |
| which tools | `allowed_tools` (**new**, decision 2b) | relay | the whole thing |
| which operations | `access: "read" \| "write"` (**new**) | relay | the rule, not the input |
| which side of the host | `allow_external` (**new**, decision 2c) | relay | the rule, not the input |
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
explicitly `true`, **under that exact spelling**. Absent, malformed, `false`,
or a case variant all mean *mutating* — the hint is read out of a map rather
than decoded into a struct, because `encoding/json` matches struct fields
case-insensitively and `{"ReadOnlyHint": true}` admitted a tool to a read
profile under a key the MCP specification does not define. The whole claim of
this decision is that the mode is decided from a declaration an operator can
read and diff, which a near-miss key is not. This is
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
patterns permitted (`mail_*`), an over-broad pattern refused, absent meaning
none.**

*Over-broad* is a property of the matcher, not a list of spellings. The first
implementation refused the literal string `"*"` while matching with
`path.Match`, and a tool name contains no `/` — so `**`, `?*`, `*_*`, `[a-z]*`
and `*e*` each match **every** tool of an MCP and not one of them is the string
`"*"`. A pre-merge review built a read-only "mail" profile with
`allowed_tools: {"macmcp": ["**"]}`, was served 26 tools across 11 of macMCP's
domains, and exfiltrated through `web_fetch` — restoring the outbound channel
this decision claims to remove. Blacklisting `**` would have been answered by
the next spelling. The rule asked instead is *does this pattern select tools by
name, or by shape?*, and a pattern is refused if it requires no literal
character at all (`*`, `**`, `?*`, `[a-z]*`) or if it matches a probe name no
MCP exposes (`*_*`, `*e*` — an underscore or a letter is in every identifier).
It is enforced in `validateToolPattern` **and** in `toolAllowedByPatterns`, so
a record that reached `settings.json` by a route validation did not cover
cannot widen a grant either; the matcher ignoring such an entry is the same
direction the editor refuses it in. A context field's `applies_to` runs through
the same matcher and is deliberately **not** subject to this: a field that
governs everything is a restriction that applies to everything, which is the
fail-closed reading there.

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

The layers are then allowlists, each failing closed, each answering a
different question, and none able to widen another: **which MCP, which tools,
which operations, which side of the host, which resources.**

### 2c. A second axis: does this tool reach outside the host

The mode reads one axis — *does this tool change anything*. There is a second,
**orthogonal** one it cannot see, and MCP already defines the field for it:
`openWorldHint`.

Two costs, both measured on this tree rather than imagined:

1. **`web_fetch` is `readOnlyHint: true` and open-world.** It is honestly
   read-only — it changes nothing — so decision 2 admits it to a `read`
   profile, and a profile created to *read one mailbox* held an outbound HTTP
   channel unless `allowed_tools` happened to exclude it. Decision 2b's own
   pre-merge review exfiltrated through exactly that, which is why the sentence
   "a read-only mail profile has no outbound channel at all" in the
   Consequences below had to be qualified with *once decision 2b has taken
   `web_fetch` away from it*. A grant that depends on someone remembering to
   leave one tool out of a list is not a control.

2. **`mail_send` and `mail_create_draft` are both just "write".** An operator
   cannot say *compose a draft a human reviews and sends*, which is the
   owner's request and the reason this decision exists. Both mutate; only one
   posts.

**CORRECTION (post-merge, measured): a draft does not stay on this Mac.** This
decision, the operator guide and the `allow_external` panel copy all argued
that refusing the outbound grant was safe for composition because
`mail_create_draft` "writes a draft on this Mac for a person to read and send".
That is false. Mail **IMAP-APPENDs** a draft to the account's own mail server.
Reproduced on the shipped `hermes-alice` profile, unchanged, with the audit line
reading `allow_external: false` beside a message that had been handed to a
server; the body size is uncapped (macMCP has tested 300,000-character bodies),
so it is a high-bandwidth channel rather than a side channel.

What survives the correction, and it is the property worth granting: **the
message is not delivered to any recipient** without a person opening the draft
and sending it. `mail_send` reaches an arbitrary address; `mail_create_draft`
cannot reach one at all. What does not survive is the containment claim: with
`allow_external` refused, a write profile still has an outbound write **to its
own account**, readable by the provider, a synced phone, a backup, or anyone
holding the credentials. Where that matters the answer is a read-only profile,
which has no compose tool at all.

**The annotation is not what is wrong; the sentence was.**
`mail_create_draft` stays `openWorldHint: false`. Flipping it would make
draft-but-not-send inexpressible and would delete the feature to fix a
sentence — and the same reading would have to be applied to
`calendars_create_event`, `contacts_create`, `mail_move` and `mail_mark_read`,
every one of which is annotated closed-world and every one of which writes to a
server when the account behind it is server-backed. Which generalises the
finding: **`openWorldHint` is a constant per tool, but whether a tool reaches
off-host is a property of the RESOURCE it acts on.** A grant cannot currently
say *may write, but only to local stores*; this axis bounds which tools may
run, not which resources they may touch. That gap is filed separately and named
in the operator guide as a known limit rather than left to be discovered.

**A record carries `allow_external` per MCP: a boolean gating any tool whose
`openWorldHint` is not explicitly `false`, defaulting to refused for an access
profile and allowed for a local project.**

**It is a separate field, not a third mode.** "Draft" is mail-specific and
relay must never learn what drafting means (the ADR-006 line). The two
questions genuinely cross — read-only and open-world (`web_fetch`), mutating
and local (`mail_create_draft`), and both remaining combinations — so a single
enum would need a member per cell and each member would be a claim about a
domain. Crossing two booleans needs none.

**The default is inverted from `readOnlyHint`, and both are fail-closed.** MCP
specifies `readOnlyHint` as defaulting to *false* and `openWorldHint` as
defaulting to *true*, so the same silence means "mutating" on one axis and
"open-world" on the other — and denies on both. `readOnlyHintTrue` asks *is it
explicitly true?*; `toolIsOpenWorld` asks *is it explicitly false?*. The
asymmetry is written at both functions because it is what a later reader will
try to tidy into one helper, and the tidied version admits every unannotated
tool to every grant. Everything else is decision 2's discipline unchanged: the
exact key spelling read out of a map rather than a struct (`encoding/json`
matches struct fields case-insensitively, and `{"OpenWorldHint": false}` is not
a declaration the specification defines), a malformed blob denying rather than
panicking, and a definition relay could not find denying too.

**The default is asymmetric, and it is the same asymmetry decision 2 has,
reached from the same place — the threat model.**

- An **access profile** defaults to **refused**. A remote client has no network
  path off this host except through relay. For it `web_fetch` and `mail_send`
  are genuinely new capability — a channel out of a machine it cannot otherwise
  reach — and that channel is what ADR-009 and ADR-010 are written against.
  This is the case the axis exists for.
- A **local project** defaults to **allowed**. Its agent already has the host's
  network: it runs as the user, on this machine, usually with a shell, so
  `web_fetch` gives it nothing it could not do with `curl`.

**A first draft of this decision said there was no asymmetry**, on the grounds
that "an outbound channel is an outbound channel". That is true about the
channel and wrong about the grant, for exactly the reason decision 2's
asymmetry is right: what differs between the two kinds is not the channel but
whether relay is the only way to it. It was measured before the flip — 63 tests
refused, and fsMCP's entire tool surface unreachable from the local `Relay`
project, because an absent hint reads as open-world and fsMCP annotates
nothing. Refusing there protects nothing and costs everything, which is
operability defeating security by the route constraint 2 names.

**An explicit value wins in both directions**, so the case the local default is
wrong for — a confined local agent with no shell and no other way out — is
expressible by storing `false` rather than by hoping a default could know. That
is why the field is `map[string]bool` and not a set of allowed MCP ids, and why
the mutator keeps a `false` instead of collapsing it into the absent case. The
editor stores only *dissent* from the kind's default, so a local project's
"allowed" is an absence rather than a value, and converting that record into a
profile lands on the profile default instead of carrying a channel across.

**Rejected: reading the axis off the tool's name or its MCP.** A registry of
"tools relay knows are network tools" is finding 7 in miniature, one level
further from home — it would not survive a second MCP and it is precisely the
domain knowledge ADR-006 keeps out of relay. `openWorldHint` is a declaration
the MCP already publishes, an operator can read, `relay mcp list --schema`
prints, and a reviewer can diff, which is the same standing decision 2 gives
`readOnlyHint` and comes with the same honest qualifier: relay applies the
rule at its own chokepoint and records what it decided, but the
*classification* is the MCP's word.

**Rejected: gating only tools that declare `openWorldHint: true`.** That is
the reading the field name suggests and it fails open on exactly the tools
that matter — an MCP that annotates nothing keeps every outbound channel it
has. It also inverts on upgrade: a tool added tomorrow joins every existing
grant, which is finding 9's shape.

**What it costs is stated rather than hidden: an access profile loses every
tool of every MCP that does not annotate `openWorldHint`, until the MCP
annotates itself or an operator grants that MCP outbound access.** Since an
absent hint reads as open-world, that is *every* tool of an unannotated MCP and
not only its networked ones. It is loud, closed, and the same trade decision 2
already made for `readOnlyHint` — and it is confined to profiles, which is
where the capability is real. It is also why macMCP's annotation audit lands
with this rather than after it. See the consequence below.

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

**AMENDED (post-merge): a v1 schema had no call-time defence at all.**
`checkScopePresence`, `filterKnownContextFields` and `scopeFromMeta` each open
with their own `!cs.V2()` early return, and `ValidateProjectGrants` runs at save
time only — so a `settings.json` edited by hand to give an access profile a v1
filesystem MCP, with any directory it liked in `context`, had that value
injected and honoured. Reproduced by reading `~/.ssh/authorized_keys`. The
belt-and-braces principle this decision states was honoured for `allowed_tools`
and for v2 scope and had no v1 equivalent. A restrict field whose value relay
*derives* — v2's `source: "project_path"`, and v1's `allowed_dirs`, which is
the same thing under the one field name relay still knows — can never be
satisfied by a record with no path, so every tool it governs is refused before
the presence check. It is a **refusal rather than a strip**, because for a v1
filesystem MCP an absent `allowed_dirs` is exactly what fsMCP reads as
*unrestricted*: removing the forged value and letting the call proceed would
turn a confinement relay disbelieves into no confinement at all.

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

**AMENDED (post-merge): the question is asked about the GRANTED tools, and such
a tool is withheld rather than merely refused.** As first built,
`ValidateProjectGrants` measured `applies_to` against the MCP's whole published
surface, never against the profile's `allowed_tools` — so
`allowed_tools: {"macmcp": ["mail_save_attachment"]}` saved cleanly and could
call nothing, macMCP's `file_dirs` governing one tool out of 47. It now asks
about the tools the grant names, falling back to the whole surface when the
grant names none yet, which is the fail-closed reading of an incomplete
profile. And because such a field can *never* hold a value for a profile —
unlike an operator field, which is merely unset — the tools it governs are left
out of `ListTools` and `ListSkillBuckets` as well as refused by `CallTool`; see
decision 8's amendment.

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
outbound grant *plus* the injected scope. `allow_external` is recorded as a
JSON `false` rather than an omitted key, because false is the resting state
and the one a refusal on that layer was decided by — an absent key would make
"the grant was not given" and "nobody recorded a grant" the same record. A record carrying the tool and the args but not those cannot
answer "was this call confined?" once an operator has since edited the profile,
and re-reading `settings.json` at query time answers a different question. The
values recorded are the ones actually injected, taken from `meta` where
`CallTool` assembles it, on the single record for a local call and on the
**intent** record for a remote one.

**On a refusal too, and that is where it matters most.** The authority was
first recorded where the call is handed to the MCP, which is *after* the tool
check, the scope-presence check and the budget — so `denied` and `throttled`
records, the two a security review reads first, carried no `access` and no
`scope` at all. "Which layer refused this, and under what mode?" was
unanswerable from exactly the records `relay audit --outcome denied` returns.
It is recorded immediately after the MCP's live surface is read, before the
first thing that can refuse. On a refusal nothing goes on the wire, so what is
recorded is the authority the call was judged against — the same set of values,
and the question the record is being asked.

**AMENDED (post-merge): this property was false for every v1 MCP.**
`scopeFromMeta` opened `if !cs.V2() { return nil }`, so a call relay had
confined with a value relay *derived itself* was recorded as `scope: null` —
the same line an MCP with no scope concept produces. The one question the field
exists to answer was unanswerable for exactly the MCP whose confinement relay
writes. It now reads v1's derived field too; an MCP declaring no schema still
records nil, because that distinction is the whole value of the field.

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

**AMENDED (post-merge): the note names every governing field, and a tool that
can never be satisfied is not listed at all.** As first built, `ListTools`
applied layers 1–4 and then only *annotated* scope, while the presence check
ran in `CallTool` alone — so a tool governed by a field the grant cannot supply
was listed, written into the `SKILL.md` `relayremote skill` generates, and then
refused on every call. Worse, the note skipped any field with no value, so
`mail_save_attachment` on the live `hermes-alice` profile was described as
confined by `mail_accounts` and `mail_mailboxes` and never by `file_dirs` —
the field that was the reason. A note that lists two of three restrictions and
omits the disqualifying one is read as complete, which makes it worse than no
note.

Two rules, and the distinction between them is the whole of it. A value that is
merely **not set yet** keeps its tool listed and keeps the loud `denied` naming
the missing field, because that is more diagnostic to an operator than silent
absence and it closes the moment somebody types a value — and the note now says
which field has none. A value that can **never** be set — a
`source: "project_path"` field on a profile, or a v1 filesystem MCP granted to
one — is withheld from both listings, because there is no configuration under
which the tool works and a client must not plan around a capability it cannot
have. `ListTools`, `ListSkillBuckets` and `CallTool` go through one object so
they cannot disagree again.

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

  Two details make that a door rather than a wall. An **empty** policy is not a
  policy: the update path already reads one as "clear it", so a refusal using a
  different reading of empty would refuse the very request that clears one —
  which is why one definition (`permissionPolicyIsEmpty`) serves the refusal,
  the candidate that is validated and the mutation that stores it, so the shape
  validated is the shape stored. And an existing local project carrying either
  can still be **converted**, by clearing them in the same request, which is the
  escape hatch `disabled_tools` already has
  (`TestProjectConvertLocalToRemote_CannotInheritFilesystemScope`). Without it
  an operator who had ever set a policy could never turn that project into a
  profile. Both are checked on the create candidate too: each is applied by a
  sub-mutation *after* the record exists, so otherwise a profile could be
  created carrying one and never rechecked.
- **Editing one thing must not delete another.** Writing an operator's scope
  re-runs the derivation so `source: "project_path"` fields are put back;
  without that, editing a local project's mail scope would silently drop its
  `file_dirs` and disable `mail_save_attachment`, the one tool that field's
  `applies_to` governs.

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
    "file_dirs": {
      "type": "array", "items": {"type": "string"},
      "description": "Directories on this host this client may write files into and read attachments from",
      "scope": "restrict", "source": "project_path",
      "applies_to": ["mail_save_attachment"]
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

`file_dirs` was named `write_dirs` until it was found to govern a read as well
(Consequences, "finding 1 reopened on the read side"): `mail_save_attachment`'s
`destination` and `mail_get_source`'s `save_to` write a file to this host,
`mail_send`'s `attachments` and `mail_create_draft`'s `attachments` read one
from it. One field, four parameters, two directions — the name and the
description both had to stop implying "write" alone.

Its `applies_to` names exactly **one** tool, not four, and not the two the
field governed before this revision. `mail_save_attachment` cannot do anything
without `destination` — there is no call that succeeds without a value, so
relay's presence check (decision 4) is right to refuse the whole tool outright
when `file_dirs` is absent. The other three each have a *purpose that does not
touch the field*: `mail_get_source` without `save_to` is a plain read,
`mail_send` and `mail_create_draft` without `attachments` are plain mail. Naming
them in `applies_to` would make relay refuse a metadata-only `mail_get_source`
or an attachment-free `mail_send` for want of a value neither call needed —
gating the tool on a parameter it never used. So `applies_to` names only the
tool that cannot function without the field; a tool with a mere *parameter*
that needs it keeps working, and it is macMCP, not relay, that refuses the
parameter. This is unaffected by the narrower `applies_to`: `_meta` injection
is not gated by it (`filterKnownContextFields` keeps every field the live
schema still declares, for every tool call, whatever that field's `applies_to`
says), so `mail_get_source`, `mail_send` and `mail_create_draft` all receive
`file_dirs` on every call and can check `save_to` / each attachment path
against it — or refuse the specific argument, never the tool — themselves.

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
`file_dirs`. It still refuses fsMCP, whose every tool is governed by one.
`SyncProjectToken` derives nothing — `file_dirs` is `project_path` and this
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
  "allow_external": false,
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
  denies it before `file_dirs` is ever consulted — including for the inline
  read that writes nothing. The layers deny in order and the outer one wins.

  Keeping the annotation truthful is the right call anyway: the mode is relay's
  own check and does not depend on macMCP's scope enforcement being correct,
  which is exactly the independence decision 4 argued for. The clean fix is to
  split the writing half into its own tool so the reading half can be annotated
  honestly read-only; that is deferred rather than done here, because it is a
  tool-surface change and this ADR is already large. A profile that genuinely
  needs raw source today must be granted `write`, which also grants send — so
  the workaround is bad enough to be worth the eventual split.

- **`mail_save_attachment` loses its filesystem write for every access
  profile**, and gains a project-directory bound for every local project. It is
  unusable remotely by construction — it requires `destination` and there is no
  call that succeeds without one, so relay's presence check refuses the whole
  tool when `file_dirs` is absent. That is the correct outcome (finding 1) and
  it removes a capability that has been exercised, so it is a behaviour change
  and not only a hardening. `mail_get_source`'s `save_to`, `mail_send`'s
  `attachments` and `mail_create_draft`'s `attachments` are bounded
  differently, and deliberately not by relay refusing the tool — see finding 1,
  reopened, below.

- **Finding 1 is reopened, on the read side, and this ADR is what missed it.**
  Finding 1 named `mail_save_attachment`'s `destination` and
  `mail_get_source`'s `save_to` — both writes — and scoped exactly those two
  under `write_dirs`. `mail_send` and `mail_create_draft` both take an
  `attachments` list naming a file **on this host** to read and encode into the
  outgoing message, and that parameter was not scoped by anything: any client
  holding either tool could name any path this process could read. The two
  write parameters were scoped; the read parameter sitting right next to them,
  on the same field's obvious remit, was missed — not a different risk found
  later, an omission in this review.

  It is worse than the hole finding 1 closed, not merely a repeat of it. The
  write side was named **escalation, not exfiltration** (Context, above) — a
  remote client with no filesystem grant of its own cannot read back a file it
  wrote there, so the practical use of an arbitrary write with no arbitrary
  read is limited to damage, not theft. `attachments` on `mail_send` is a read
  wired directly to an outbound channel: whatever this process can read, a
  `write`-mode mail profile can mail out, which is exactly the exfiltration
  this ADR's threat model (Context) says is the thing being defended against —
  not the one exception finding 1 carved out for escalation, but the main
  case. Reproduced live, not theoretical: an adversarial validator read and
  exfiltrated `/tmp/zsec-secret.txt` — a file with no relationship to any
  mailbox — through `mail_send.attachments` via a real `write` access profile,
  before this field existed.

  This is why the field was renamed `write_dirs` -> `file_dirs` and its
  description stopped saying only "write": the same directory bound now gates
  both directions, on all four parameters, under the worked example above. The
  fix is a scope value macMCP checks the attachment path against, exactly as
  it already checks `save_to` — there is no new mechanism here, only a
  parameter that should have been named in the first pass and was not.

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

- **And a truthful `openWorldHint`, which is why macMCP's annotation audit had
  to happen first.** An **access profile** loses every tool of an MCP that does
  not publish the field — not only the networked ones, because an absent hint
  reads as open-world — until the MCP annotates itself or an operator grants
  that MCP outbound access. That is the fail-closed direction and it is where
  the capability is real, so it is the right place for the cost to land.

  **Local projects are unaffected**, which is the whole of decision 2c's
  asymmetry: the live wildcard `Relay` project keeps fsMCP, whose tools declare
  neither hint, and keeps every macMCP tool it has today. The first draft of
  this decision had no asymmetry and would have taken all of that away — 63
  tests refused and fsMCP dark — for no gain, since an agent running here can
  already reach the network with `curl`. The measurement is what corrected the
  decision, and it is recorded in 2c rather than quietly fixed.

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

- **A `write` mail profile is an exfiltration channel only if you grant it
  one.** This bullet used to end "that is inherent in granting send to a
  semi-trusted agent". It is not inherent any more; it is a choice, and
  decision 2c is what makes it one. The channel itself is unchanged —
  `mail_accounts` scopes the identity a message is *sent as* and says nothing
  about who it is sent *to*, so a profile holding `mail_send` can still mail
  anything it can read to any address. What changed is that holding
  `mail_send` now takes two grants rather than one: `access: "write"` **and**
  `allow_external`, neither of which a profile has by default. A write mail
  profile without the second drafts and does not post, which is the shape the
  owner asked for — with the qualification decision 2c's correction adds: the
  draft is uploaded to the account's own mail server, so "does not post" means
  "reaches no recipient", not "does not leave this Mac". A **read-only** profile has
  no outbound channel at all — not "once `allowed_tools` excludes `web_fetch`",
  which is what the old sentence had to qualify itself with, but by
  construction, because `web_fetch` is open-world and no read-only default
  grants it. A recipient allowlist remains a coherent later addition on the
  same `applies_to` machinery, and it is the thing that would bound a channel
  deliberately granted. The fixture hides the underlying channel — its SMTP
  server refuses non-fixture recipients with 550 — so that must still not be
  mistaken for a control that exists.

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
  machinery, bounding the channel a deliberate `allow_external` grant opens.
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

---

## Superseded in part — 2026-08-26

This ADR's worked example is fsMCP declaring `allowed_dirs` as a v2
`contextSchema` restriction. fsMCP v3 declares **no context schema**: it is
spawned with a `--root` and serves one directory for its lifetime, so there is
no value for a grant to supply.

The decisions here are unchanged and still govern every MCP that does declare a
scope. What changed is that "this MCP has no scope concept" became a real,
supported shape rather than a theoretical one — `derivedScopeFields` returns nil
for such an MCP, its tools stay listed and callable for a remote profile, and
decision 7's requirement that the log answer "what was attempted with what
authority" is met by the `mcp_root` field rather than by `scope`.

---

## Amended — 2026-09-01: A star and an empty array

Decision 3 dropped a stored `wildcard` keyword outright, and decision 4 made
absent and *empty* the same refusal on all three sides. Both were reopened for
the resource-scope fields specifically — `mail_accounts`, `calendars`, and the
rest of the `source: "operator"` array fields this ADR's mechanism governs,
not `allowed_tools`, `allowed_mcp_ids` or `allowed_models`, which already had
their own wildcards and are unaffected. Two independent usability problems
drove it, both real rather than theoretical:

1. **Naming every account and every mailbox by hand does not scale**, and gets
   worse with every mailbox macMCP's own scope work has since made
   addressable (CLAUDE.md's mailbox-path and calendar-path sections). An
   operator who means "this trusted client reads all my mail" has to
   enumerate every account and, per account, every mailbox — and re-does it
   by hand whenever the host's mail structure changes, or the grant silently
   narrows.
2. **A field can be empty for a true, boring reason** — a contacts account
   that genuinely has zero groups — and decision 4 made that indistinguishable
   from an operator having forgotten to configure it. `contacts_list_groups`
   refused rather than answering `[]`, for every client scoped to that
   account, permanently, with nothing an operator could do about it: there was
   no way to say "I looked, there is nothing here" that was not also the
   spelling for "nobody has looked yet".

### Why decision 3's reasoning does not carry over unchanged

Decision 3 rejected `wildcard` for two reasons: "on an access profile the
wildcard was already going to be refused (ADR-009 decision 4's reasoning — a
grant must be an enumeration someone typed, not a value that widens when the
host's configuration changes)", and "on the `project_path` side there is
nothing for it to mean". Neither is a blanket argument against a stored
wildcard anywhere in this model; both are arguments about *who is granting to
whom*.

ADR-009 decision 4's concern is a confused deputy: an operator managing
access for *someone else's* agent, where an account added to the host later
escapes the review that granted the profile in the first place. That is a
real threat model, and it is not the only one this mechanism serves. The
common case for a resource-scope field — an operator granting their own
trusted client access to their own data on their own Mac — has no second
party whose review is being bypassed; the operator IS the one who would add
the account, and the one who re-opens the grant. Applying the confused-deputy
threat model uniformly to that case is not caution, it is friction with
nothing behind it.

The evidence that this codebase already agrees, on the other two axes of the
same permission system: `allowed_mcp_ids: ["*"]` already allows every MCP
relay knows about, dynamically, and grows the moment a new one is registered
(`docs/access-profiles.md`). `allowed_models` carries a literal stored
wildcard too (`PROJ_MCP_WILDCARD` in the Settings UI). Decision 3's "no
wildcard, anywhere" was never quite true; it was "no wildcard for *resources
within* a granted MCP", the one axis that had not been asked yet.

The second piece of prior art this addendum leans on is `file_dirs` resolving
to `/`: the codebase's answer to "an operator grants something huge" was
never prohibition, it was **loud, unavoidable disclosure** — `scope_breadth.go`
names a filesystem root on the client's own tools/list note, the profile
card, `relay grant`, and `relay audit --authority`, regardless of `disclose`.
That mechanism generalises directly: `scopeBreadthWildcard` is a value of
exactly `["*"]`, classified and disclosed exactly like a filesystem root
(`renderScopeDisclosure` groups the two together, and says why — a wildcard
discloses nothing about the host a client could not already learn by calling
the field's own enumerator).

### What "*" actually costs, and why it is cheap rather than expensive

The obvious implementation mistake is assuming a stored `["*"]` needs relay,
or the MCP, to *resolve* it — go look at the host and materialise a concrete
list. It needs the opposite: every consumer of `Access.unrestricted`
(macMCP's `ResourceScope`) simply skips its filtering step, since there is
nothing to compare against. `ScopedRows.allowed` returns every row already
produced by the live EventKit read, unfiltered; `MailScope.accountTargets`
reuses the existing `.unscoped` decision, which already means "read
everything, live" for exactly this reason. No extra host round trip, no
caching, no staleness to design around — the underlying reads were already
live for every scoped call, wildcard or not. This makes `["*"]` cheaper to
enforce than an explicit list, not more expensive: nothing to fold, match, or
check for ambiguity.

**Silent widening is still real and still the reason to be loud about it.**
Cheap and safe are different axes. A live, per-call resolution that costs
nothing is exactly as reviewable as one that costs a host round trip — an
account added to the Mac next week joins either way. That is not a
performance question, which is why the disclosure requirement (above) is not
optional and does not get relaxed because the check turned out to be cheap.

`"*"` is recognised only as the **sole** element of the array. `["*", "Bob"]`
is refused at save time (`ContextField.ValidateValue`) rather than accepted
as an inert literal: a mixed array cannot be reviewed as "everything", and
guessing which of the two an operator meant is exactly the guessing this
mechanism exists to refuse elsewhere. Relay does not resolve the wildcard —
it validates the shape and disclosure of the value; what `"*"` *means* is the
enforcing MCP's business, as ADR-011 decision 3 already established for every
other keyword here.

### The empty array, and why it needed its own function

`hasScopeValue` collapsed absent and present-empty on purpose, and that half
of decision 4 is **unchanged**: an operator who never configured a field and
one who explicitly emptied it must keep looking identical to anything that
cannot tell a forgotten grant from a reviewed one. What changed is narrower —
whether an **explicit, present** empty array is itself indistinguishable from
absence, or is a third, storable state: the confirmed-empty grant, reachable
only by an operator (or a client-editor acting on their behalf) looking at a
real, possibly-empty enumeration and saying so.

That needed a second function, `hasScopeAssertion`, rather than a changed
`hasScopeValue`, because `hasScopeValue`'s callers split cleanly into two
questions that happen to share code by coincidence: "is this field's own
value a live authorisation" (`checkScopePresence`, the call-time gate; and
`scopeNoteFor`, the client's own "Scope: ..." text — both had to change, or a
confirmed-empty grant would be denied or misdescribed before the MCP's own,
correct `.confirmedEmpty` handling was ever reached) versus "is this key
present at all, for a reason that has nothing to do with authorisation"
(`dependencyValues`'s enumerate-filter semantics, which are deliberately the
*opposite* rule — absent-or-empty means "across everything" for a picker
query — and `unplaceableContextFields`, where an empty key asserting no
confinement is a claim that stays true whether or not the field is still
declared). Reusing one function for both would have meant either breaking the
picker's own documented contract or under-protecting the call-time gate; they
needed to keep disagreeing.

The Settings UI met the same split turned inside out: `scopeValueFromText`
always returns `[]` for blank array-field text, never `undefined` — there is
no way to spell "nothing typed" that survives a round trip through a text
box, which is what makes the picker and the free-text fallback interchangeable
in the first place. Before this addendum that was harmless, because `[]` and
"never touched" meant the same thing at harvest either way. Once `[]` became
a storable assertion, harvesting *every* field's blank text as `[]` the
moment a project was saved for any reason would have converted every
untouched governed field into a confirmed-empty grant — the opposite of
decision 4's default. `scopeFieldWasEverAsserted` is the fix: a field
neither touched this session nor previously stored stays omitted, full stop,
independent of what its (necessarily blank) text says. Within a session that
*did* touch the field, blank text is still ambiguous on its own — "typed then
deleted" and "clicked Confirm: nothing to grant here" both end up blank — so
the two buttons write different in-session text (`''` for Clear, a one-space
sentinel for Confirm) that resolves the ambiguity before harvest ever asks
"what is this field's value", and never reaches the wire either way.

### What this does not change

- The `wildcard` keyword itself stays dropped. There is no new schema
  keyword; `"*"` is a plain string value of an existing `type: "array"`
  field, exactly as ADR-011 decision 3's five keywords already permit relay
  to validate without understanding.
- `source: "project_path"` fields (`file_dirs`) get neither: no operator ever
  picks a value for one, so neither `["*"]` nor an explicit `[]` reads as a
  reviewed decision the way it does for an operator-set field. Both resolve
  to the same refusal `.refuse` already gave, on both sides — macMCP's
  `MailScope.confine` / `HostFileScope.resolve`, and relay's own validation,
  which still refuses an operator-supplied value for a `project_path` field
  outright before either spelling is even reached.
- Nothing about decision 4's *absent* case moved. A mediated call carrying no
  value for a field at all still refuses, unconditionally, on all three sides
  — relay's presence check, macMCP's own, and the operator-facing "needs a
  scope value" banner all read "the key is missing" exactly as before.

### See also

- ADR-011 decision 3 — the wildcard keyword dropped, and the two reasons this
  amendment answers to individually rather than overturning wholesale.
- ADR-011 decision 4 — "absent and empty are refusals, on all three sides";
  amended for the *present-and-empty* case on an operator-set field only.
- ADR-009 decision 4 — the confused-deputy reasoning decision 3 borrowed,
  and the reason this amendment is scoped to resource-scope fields rather
  than argued as a general principle.
- `relay/scope_breadth.go`, `relay/web/src/app.js` (`scopeValueBreadth` and
  its JS mirror) — the disclosure mechanism reused rather than reinvented.
- `relay/context_schema.go` — `hasScopeValue`, `hasScopeAssertion`,
  `ContextField.ValidateValue`.
- `relay/router.go` — `checkScopePresence`.
- `relay/web/src/app.js` — `scopeFieldWasEverAsserted`,
  `SCOPE_CONFIRMED_EMPTY_TEXT`, `selectAllScopeValuesAt`,
  `confirmScopeFieldEmpty`.
- `macMCP/Sources/macMCP/ResourceScope.swift` — `Access.unrestricted`,
  `Access.confirmedEmpty`, `ResourceScope.wildcard`.
