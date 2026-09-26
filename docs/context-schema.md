# The MCP context schema

An MCP tells relay what may be narrowed about it by returning a `contextSchema`
in its `initialize` response, beside its name and version:

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
      "description": "Directories this client may write files into",
      "scope": "restrict", "source": "project_path",
      "applies_to": ["mail_save_attachment", "mail_get_source"]
    }
  }
}
```

Relay stores the values an operator (or the derivation below) supplies, injects
them into `_meta` on every `tools/call`, renders them, and refuses a call whose
required value is missing. It does all of that **without knowing what any field
means**. Every field name is an opaque map key from relay's side, start to
finish. See ADR-011 (resource scope: relay tracks values, never their meaning) for why.

## The shape is flat

`contextSchema` is `{fieldName: {fragment}}` — a map from field name to its
declaration. It is **not** a JSON Schema, so there is no `"type": "object"` and
no `"properties"` wrapper.

This is stated because the ambiguity is live: relay's own fixtures once used
the nested form and `schemaHasField` had to learn both shapes (issue #17), and
the failure direction of getting it wrong is fail-open — a restrict field relay
does not see is a grant permitted and nothing enforced. Relay still consults a
`"properties"` object as a rescue, but only when the flat reading found no
`scope: "restrict"` field at all. Do not rely on it.

## The keywords

| keyword | values | what relay does with it |
|---|---|---|
| `scope` | `"restrict"` | this field narrows access; the rules below apply. Absent means an ordinary context value relay injects and otherwise ignores. |
| `source` | `"operator"` \| `"project_path"` | who supplies the value |
| `applies_to` | tool-name patterns | which of this MCP's tools the field governs |
| `enumerable` | bool | the MCP can list this field's valid values |
| `depends_on` | field names | enumeration ordering |
| `disclose` | `"value"` \| `"count"` \| `"none"` | what a **set** field's value looks like in the scope note a client reads. Absent means `"value"` — see below. |

Plus the ordinary fragment relay uses to validate a value: `type`, `items`,
`description`. This is a deliberate JSON-Schema **subset** — array-of-string
and string are what is validated, and anything else is accepted as long as it
is present and non-empty.

### Keywords and their values are read under their exact spelling

Every key and value above is matched exactly. A key relay has never heard of is
ignored — that is how a later vocabulary lands on an older relay, and it is why
fsMCP's leftover `ui` costs nothing. But a key or value that differs from a
keyword only in **case** is a typo of *this* vocabulary, and it is an **error**:

    {"Scope": "restrict"}     refused — the keyword is "scope"
    {"scope": "RESTRICT"}     refused — the value is "restrict"
    {"source": "Project_Path"} refused
    {"ui": "directory-list"}  ignored, as before

Read it exactly and nothing here can bite you. The reason it cannot be lenient
in either direction is that both readings are wrong and neither is visible:
accepting `Scope` agrees with a spelling no document defines, and ignoring
`RESTRICT` disagrees with what a reviewer reading your published schema sees.

### A fragment relay cannot read disables the whole schema

If **any** field fragment fails to decode — a type slip such as
`"applies_to": "mail_*"` written as a string, or a near miss above — relay
refuses **every call to that MCP**, for every grant, and lists none of its
tools. It logs one line naming the field and the reason when your server
connects; that line is your signal.

This is total rather than per-field on purpose. A fragment relay could not read
is a fragment relay cannot bound: the field it failed on may have been the one
governing everything, so "apply the parts I understood" would be a claim about
the parts it did not. Dropping the field alone is worse than it sounds — relay
would stop requiring a value for it, stop governing the tools it names, and
strip the operator's stored value on the way to the wire, all silently.

### A value relay cannot PLACE in the live schema is refused too

The mirror image of the rule above, and it fails closed the same way. If a
grant carries a value for a field the MCP's **live** schema does not declare,
relay refuses every call to that MCP under that grant, names the field and the
MCP, and lists none of its tools.

There, relay could not read what the MCP declared. Here, it cannot place what
the *operator* declared — and the consequence is identical: relay does not
know what it would be handing over, so it hands over nothing.

Relay used to **drop** such a value and dispatch the call anyway. That reads
like the conservative option and is not: relay asserted a confinement in the
operator's profile, did not deliver it, and recorded the omission as
`scope=(none declared)` — the audit string for "this MCP was never scoped".
In the reproduction the only thing that stopped an unconfined filesystem call
was fsMCP's own fail-closed rule. Relay cannot assume the next MCP has one
(issue #42).

The question is asked of every stored key, not only of ones that were
restrictions when they were written, because once the declaration is gone
relay cannot tell the difference — the field it can no longer place may have
been the one governing everything. A key whose value is **empty** (`[]`,
`null`, `""`) is exempt: empty is absent everywhere else in this model, and a
leftover empty key asserts no confinement anybody could fail to deliver.

Reaching this state means something changed underneath a grant: a schema that
legitimately changed, a downgrade, a rebuild, a different binary at the same
path, a schema that failed to publish, or a hand-edited `settings.json`. Note
that this is a **v2 rule**: a v1 or absent declaration has its context blob
injected verbatim, so nothing is dropped there and nothing is refused.

### `scope: "restrict"` means fail closed

A restrict field that is **missing** from `_meta` means the server **refuses
every call it governs**. Not "falls back to a default", not "no restriction".
`null` and `""` are absent too — there is no way to distinguish them from a
genuinely missing key, and both refuse exactly as missing does.

There is deliberately no keyword letting an MCP declare otherwise. A field that
says it restricts and then defaults open is not a restriction, and relay could
never verify the claim either way.

An array-typed field's value can be exactly one of two special shapes instead
of an ordinary list of names, each meaning something and neither expressible
by omitting the field (ADR-011 addendum, "A star and an empty array" —
resource-scope fields only; `allowed_tools`, `allowed_mcp_ids` and
`allowed_models` have their own, separate wildcard rules):

- **`[]`, present and explicit** — the confirmed-empty grant. Distinct from
  the field being absent: an operator (or a client editor acting on their
  behalf) looked at the field's real values — possibly zero of them — and
  said so. A well-behaved MCP resolves this to "the confined set is empty",
  which for a read is an ordinary, successful empty result and for a write is
  an ordinary "nothing in scope to act on", never a refusal. Relay's own
  call-time gate (`checkScopePresence`) treats it as present, not missing —
  denying it there would mean the MCP's own correct handling of it is never
  reached.
- **`["*"]`, and only as the array's sole element** — the wildcard: every
  value this field could ever name, resolved fresh by the enforcing MCP on
  every call, never a value relay resolves or snapshots itself. Mixed with a
  named value (`["*", "Bob"]`) it is not recognised as the wildcard at all and
  is refused at save time, because a mixed array cannot be reviewed as
  "everything". A stored `"*"` is disclosed to the client exactly as an
  unrestricted `file_dirs` entry (`/`) already is — loudly, on every surface,
  regardless of `disclose` — because it is not really a confinement and
  hiding that would defeat the one thing `disclose` is supposed to protect
  against.

Anything else empty-shaped — `{}`, an array containing only empty strings —
is still refused exactly as before. And the underlying caution stands: an
operator who wants every account named individually still lists every
account individually: `"*"` and enumerating are two different, deliberate
choices with two different disclosure profiles, not two spellings of one
thing.

### `source` decides who fills it in

- **`project_path`** — relay derives the value from the project's `path`,
  because the schema asked it to. An array-typed field gets `[path]`; a
  string-typed one gets the bare path. A **remote-kind record (an access
  profile) has no path**, so such a field is absent for one and every tool it
  governs refuses. Relay never derives one for a profile, unconditionally,
  whatever validation decided earlier. Because relay owns the value, the
  presence gate ignores a stored derived field on a local, unhosted project
  when it asks whether an update widens `context`, provided the stored value
  is exactly what relay derives from `path`.

  A derived value stays relay's after the record stops having a path. An
  update to a record that is already remote or hosted, and that sends no
  `context`, drops every stored derived field (v2 `source: "project_path"`,
  or v1 `allowed_dirs`) for each MCP whose schema relay holds, and deletes an
  entry left with no fields. It does so whether or not the update names a
  permission field (`access`, `allowed_tools`, `allow_external`); one that
  does validates the permission set against the context the drop leaves, and
  a refusal leaves the stored context as it was. A conversion to remote or
  hosted that names no permission field drops the same way. The correct
  derived value for a record with no path is "absent", so dropping only
  narrows: an absent restrict field refuses every tool it governs. Refusing
  instead would ask the operator to remove a value no request can name.
  Operator fields beside it survive, and an entry with nothing to drop keeps
  its bytes.

  An update that names a permission field validates the context the result
  would hold, including stored context the request did not send. When the
  result is local and not hosted, relay's own values pass through: with no
  `context` in the request, every stored v2 `project_path` field and every
  stored v1 blob is carried unchanged. The write keeps them as stored, and
  re-derives them from `path` only when the same request names `path`,
  `allowed_mcp_ids` or `context`. A conversion from local to remote or
  hosted derives nothing, so a stored derived field is judged as if it had
  been sent: a conversion that names a permission field while one is stored
  is refused, naming the field or, for a v1 blob, the MCP, and neither
  converts nor drops. Send the conversion without a permission field first,
  which drops the derived value, then the permission edit.

  An MCP that is not connected (no entry in the live surfaces, as distinct
  from connected with no schema) cannot be judged: keeping its stored value
  fails open, and dropping the whole entry deletes operator values because a
  process is down. So a local-to-remote conversion that carries non-empty
  stored context for a granted, unconnected MCP is refused, naming it. The
  way out is to connect the MCP, remove it from `allowed_mcp_ids`, or send
  its context in the same request. The refusal applies to conversions only;
  an edit to an existing profile while an MCP is down leaves that MCP's
  context as it is, and the next edit with the MCP connected that names no
  permission field cleans it.
- **`operator`** — an operator sets it explicitly, local and remote alike. A
  restrict field with no declared `source` is treated as operator-supplied,
  because that is the reading that leaves the value un-derivable: relay
  inventing a value for a field it does not understand is the failure this
  mechanism exists to prevent. While relay holds the MCP's live, usable v2
  schema, narrowing an operator restrict field does not raise the presence
  gate: removing a value, clearing the field, or replacing `["*"]` with a
  list is not a widening of `context`. Adding a value, or setting one where
  the field was unset (`[]` included), is. The wildcard for this purpose is
  `["*"]` only; an asserted bare `"*"` compares strictly. Without that schema any
  change to the entry prompts.

A grant is refused at edit time if a `project_path` field's `applies_to` covers
**every** tool the grant names and the record has no path — the grant would buy
nothing. fsMCP is refused to a profile for that reason; macMCP is not, unless
the profile's `allowed_tools` happens to name only tools that field governs.
(The question is asked about the granted tools, not the MCP's whole surface: a
profile granted `allowed_tools: ["mail_save_attachment"]` alone can call
nothing. A grant that names no tool of the MCP yet is judged against the whole
surface, which is the fail-closed reading of an incomplete profile.)

Tools such a field governs are also **withheld from a profile's `tools/list`**,
not merely refused when called — there is no configuration under which they
work, so advertising them would hand a client a capability it cannot have and
put it into the generated agent skill. A field whose value is merely *unset*
behaves the opposite way: the tool stays listed, its description says which
field has no value, and the call is refused loudly.

### `applies_to` selects tools, anchored

Patterns are matched with Go's `path.Match`, **anchored**: the pattern must
match the whole tool name. `mail_*` matches `mail_send` and not `xmail_send`,
and a pattern with no metacharacter is an exact match. The same matcher decides
`allowed_tools` on a project, so the two can never disagree about anchoring.

An **absent or empty** `applies_to` governs every tool the MCP exposes — the
domain-blind default. A pattern relay cannot compile governs everything too,
which is the fail-closed reading for a restriction, and so does an **empty
string** entry: `[""]` names no tool, exactly as an uncompilable pattern names
none, so it governs everything rather than nothing. A stray `""` beside a real
`"mail_*"` therefore widens the restriction to every tool — it does not void
the list. (It used to be skipped, which made `applies_to: [""]` a field that
declared itself a restriction and governed nothing at all.)

### `enumerable` and `depends_on`

`enumerable: true` says the MCP can list this field's valid values, so the
operator UI can offer a picker over real values rather than a free-text box
where the easiest failure is a confinement that does not confine. `depends_on`
names fields whose already-chosen values must be sent as parameters, so the UI
fills in dependency order (mailboxes cannot be listed without an account).
Neither affects enforcement.

### `disclose` controls what the client's scope note says about a value

Every tool a v2 schema governs gets a "Scope: …" sentence appended to its
description in `tools/list` (decision 8), naming each governing field and
either its value or, for a field with none, that every call is refused. That
value is the field's **content**, not the fact of the restriction — and for a
field like `allowed_dirs`, the content is host topology: an absolute path
names an account and a directory layout, on top of whatever the client already
learns from being told it is confined at all. A field like `mail_accounts` has
no such second layer — a mailbox name discloses nothing about the machine —
which is why the mechanism is opt-in per field rather than a blanket redaction.

`disclose` says what the note may show about a **set** value, once relay has
already decided the field governs the tool and has something to show:

| `disclose` | the note says |
|---|---|
| `"value"` | the value, exactly as it always has — **the default when `disclose` is absent** |
| `"count"` | the value's shape and nothing else, e.g. "confined to 2 values" |
| `"none"` | that the field is set and nothing else |

### `disclose` governs the CLIENT's surface and nothing else

This is stated as its own rule because getting it wrong is what issue #41 was.
There are two audiences for a scope value and they are owed different things:

| audience | surfaces | what it sees |
|---|---|---|
| the **client** | the `Scope:` note in `tools/list`, and the generated `SKILL.md` | whatever `disclose` says |
| the **operator** | Settings → Projects, `relay grant`, `relay audit --authority`, the `_meta` relay injects, the audit JSONL | the real value, **always** |

The operator is entitled to the coordinates: it is their machine and their
grant, and a review that cannot see what was granted is not a review. So no
operator surface consults `disclose`, ever, and `ScopeFieldView` deliberately
does not carry it — a value withheld from an operator is a value nobody
checks.

Getting this wrong does not look like a leak; it looks like nothing. Relay
shipped `disclose: "count"` and then pointed the operator guide's first
verification step at `relayremote list`, a client-side tool. For
`allowed_dirs: ["/"]` it printed *"confined to 1 value"*, and for a grant of
one project folder it printed the same eleven bytes — while the first grant
read `/etc/passwd` and listed `~/.ssh`. The documented way to check a scope
returned the same answer whether you got it right or catastrophically wrong.

### A value that is not a confinement is named, at every `disclose` setting

**A count is not a measure of confinement.** "1 value" is true of
`/Users/me/project` and equally true of `/`. So one fact outranks `disclose`:
when a scope entry resolves to a **filesystem root**, the note says so.

    Scope: Directories this client may read, search and modify within — unrestricted (the whole filesystem).

That holds for `"count"`, for `"none"`, and for `"value"` (which prints the
phrase *and* the value). It costs the client nothing it does not learn the
moment it lists `/`, which is the test `disclose` exists to apply — and
withholding it would mean the one mechanism that tells a client its own limits
telling it something false about them.

The rule is a question about the **value**, never about the field name: relay
holds no registry of known field names (ADR-011 decision 3), so it asks the
same question of every value of every field. An entry is classified by its
cleaned form, so `/`, `//`, `/..` and `/Users/me/../..` are one value rather
than four spellings. The cost is a false positive on a non-path field whose
value is literally `/` — announced as unrestricted, refused by nothing. That
is the right direction to be wrong in.

A **home directory** (`~`, `/Users/<someone>`, `/home/<someone>`, and the
parent of either) is deliberately **not** named to the client. It is genuinely
confined, so "confined to 1 value" is not false there, and saying "this is
somebody's home" would disclose exactly the host topology `disclose` exists to
withhold. It is loud on every operator surface instead — the profile card, the
save-time confirmation, `relay grant` and the audit authority line — because
that is where the question "did I mean to grant that?" is asked.

`"value"` has to be the absent-default for the same reason every other keyword
here defaults to the reading that changes nothing: an MCP that predates this
keyword must render byte-for-byte as it always has, not discover its scope
note has quietly gone quiet. `disclose` never touches **enforcement** — a
withheld value is still injected into `_meta` on every call, still validated,
still required by `scope: "restrict"` — and it never touches the **audit
log** or an **operator surface**, both of which show the real value
unconditionally; it changes exactly one string, the one a remote client reads
off `tools/list`.

The unset-value branch of the note — "no value is set for X, so every call to
this tool is refused" — is **not** subject to `disclose`, at any setting.
There is no value to disclose there, and it is the client's only warning that
the tool is dead on arrival; making it optional would trade the one real
disclosure risk `disclose` exists to close (a set value's content) for a
silent one (a dead tool a client believes is live).

**An unrecognised `disclose` value does the same thing, silently.** `disclose:
"hidden"` is not a near miss of any keyword here — it differs from `"count"`
by more than case — so it is ignored rather than guessed at, which is the rule
that lets a later vocabulary land on this relay. The consequence for *this*
keyword is that the note renders the value, and the MCP author who wrote
`"hidden"` believes they have withheld it. Verified: `Disclosure()` answers
`"value"` for `"hidden"`, `"redact"`, `"secret"` and anything else outside the
three spellings above.

That is the same fail-open direction as the older-relay case below and is
accepted for the same reason — but it is worth knowing that the two failures
look identical from the MCP's side and neither is detectable from there. If
you declare `disclose`, spell it exactly, and read the note a real client
receives before believing it.

**An older relay ignores `disclose` and renders the value anyway.** That
follows from the rule above ("a key relay has never heard of is ignored") the
same way it applies to every keyword this document adds after some relay is
already running — but it is worth saying plainly here, because `disclose` is
the first keyword whose whole purpose is to withhold something from a surface
outside relay's own process. An MCP declaring `disclose: "count"` cannot
detect which version of relay it is talking to and cannot fail closed on the
answer: the guarantee is a property of the relay instance actually running,
not of the schema an MCP declares. Do not read a `disclose: "count"`
declaration as a promise the declaring MCP is in a position to keep — it is a
request relay may or may not be new enough to honour.

## `context/enumerate`

An MCP declaring `enumerable: true` on a field must answer this JSON-RPC
method. Relay sends it for those fields and no others.

```json
{"jsonrpc":"2.0","id":7,"method":"context/enumerate",
 "params":{"field":"mail_mailboxes","values":{"mail_accounts":["Bob"]}}}
```

```json
{"jsonrpc":"2.0","id":7,"result":{"field":"mail_mailboxes",
 "values":[{"value":"INBOX","label":"INBOX"},
           {"value":"Projects/Archive","label":"Projects/Archive (Bob)"}]}}
```

`value` is what goes into `_meta` verbatim; `label` is display only. An empty
`values` list is a valid answer and means there are none.

**`values` carries only the fields this one declares in `depends_on`**, and only
those the operator has already chosen. An absent key — and an empty one, which
relay never sends — means **across everything**, never "match nothing". A server
that reads an empty filter as matching nothing returns an empty list in exactly
the state a picker opens in, which is indistinguishable from a host that holds
nothing.

It is **not a tool call**. It carries no `_meta`, spends no ADR-010 budget, and
never reaches the audited tool chokepoint — routing an operator-UI read through
`CallTool` would put a call nobody made in the audit log and run it with relay's
own unscoped authority (ADR-011 decision 6).

### Failing it

Relay recognises exactly two error codes and treats every other outcome the
same way. The three cases are kept apart because an operator's next action
differs in each.

| the server answers | relay | the editor |
|---|---|---|
| `-32601` method not found | records it for the life of the connection and stops asking | falls back to text entry, silently and permanently for that MCP |
| `-32602` invalid params | surfaces it | says relay asked for a field it should not have — a relay bug — and offers text entry |
| anything else, or no answer at all | surfaces it as retryable | says the values could not be listed *right now*, offers a retry, and keeps text entry |

"Anything else" includes JSON-RPC's implementation-defined server-error range
(`-32000` to `-32099`), which is what macMCP answers when Mail itself will not
answer, along with a transport failure, a timeout, and a malformed result. None
of them is ever rendered as an empty list: **a failure and "there are none" must
not look the same**, or a profile gets saved against a host the operator was
shown nothing about. Relay enforces that on the wire — `"values": []` is an
answer, `"values": null` is a failure.

Enumeration is **disclosure**: the list of every mail account on the host. It is
served only on relay's admin-authenticated surfaces (the frontend socket and the
tray's IPC channel) and is unreachable from the remote listener, whose dispatch
table holds `ListTools` and `CallTool` and nothing else.

## Who fills a value in, and where

An `operator` field is set in the Settings UI's per-MCP permission panel
(Projects tab → edit a record → the MCP's panel), or over the HTTP routes eve
uses:

    GET  /api/mcps/{id}/scope_fields   -> the restrict fields this MCP declares
    POST /api/mcps/{id}/enumerate      -> { "field": …, "values": {…} } (see below)
    POST /api/projects                 -> { "context": { "<mcp>": { "<field>": … } } }
    PUT  /api/projects/{id}            -> the same, as a patch

`enumerate` is a POST for a read because `values` is a map from field name to
that field's value, whose shape is whatever the MCP declared — there is no
query-string encoding of that which does not quietly assume array-of-string.
It answers with `{mcp_id, field, status, values, error}`: `200` for a real
answer and for an MCP that does not implement enumeration (a true, final answer
about that MCP), `404` for an MCP relay has never connected to, `400` for a
field it never declared enumerable, `502` when the MCP refused the request relay
built, and `503` when it could not answer. The `status` field is the precise
answer; the code is there so a client reading only codes cannot mistake a
failure for an empty list.

`scope_fields` is the projection an editor renders from: name, `type`,
`item_type`, `description`, `source`, `applies_to`, `enumerable`, `depends_on`.
`source` is **normalised** there — a field that declared none comes back as
`operator`, so no consumer re-derives that rule. An MCP relay has never
connected to is a **404**, not an empty list: "scopes nothing" and "cannot say"
are different answers and only the first lets an editor safely offer no fields.

**Every value is validated on save, whichever surface produced it**, and an
invalid one is refused rather than stored (`validateProjectPermissions`). The
refusals, each naming the problem:

- a value for a field the MCP does not declare — the refusal lists the ones it
  does, because a refusal that says a name is wrong without saying which are
  right is one an operator answers by guessing;
- a value of the wrong type for the declared fragment;
- an empty value for a `scope: "restrict"` field;
- a blob that repeats a key, at any depth — some parsers keep the first and
  others the last, so the gate would compare a value the MCP may not see;
- a value for a `source: "project_path"` field in a request that carries
  `context` — relay derives those, and one written by hand would be replaced
  at the next resync;
- a context value for an MCP that declares a **v1** schema, for the same
  reason: the v1 branch replaces the whole blob. The one exception is an
  echo: on a local, unhosted project, a v1 blob is accepted when the stored
  record has one for that MCP and the two are equal by decoded value (the
  presence gate's own comparison, so reordered keys and whitespace still
  match). That is what the Settings form resends on every save. A changed
  value, a new blob, any v1 blob on create and any on a remote or hosted
  project are refused, and the duplicate-key check runs first;
- an `access` that is not `read` or `write`;
- an `allowed_tools` pattern that will not compile, which would match no tool
  at all in *that* list (the same pattern in a field's `applies_to` governs
  every tool — the two callers of the shared matcher fail closed in opposite
  directions);
- an `allowed_tools` pattern that is **over-broad**: one requiring no literal
  character (`*`, `**`, `?*`, `[a-z]*`) or matching a probe name no MCP exposes
  (`*_*`, `*e*`). Such a pattern selects by shape rather than by name, so a
  tool registered tomorrow would join the grant unreviewed. The matcher refuses
  one at call time as well, so a record that reached `settings.json` by another
  route cannot widen a grant either.

A value is judged the same whether the operator changed it or the Settings
form sent it back as stored: an invalid stored value (a `null` restrict field,
a string where an array is declared, an empty element) blocks the save with a
refusal naming the MCP and the field, and the way out is to re-enter or clear
that field. Skipping unchanged values would let an invalid one survive every
save unseen.

An MCP relay has never connected to is **permitted** with nothing but an
emptiness check, exactly as `ValidateProjectGrants` permits a grant it cannot
qualify: this is a coherence check an operator sees at edit time, not the
boundary, and refusing on missing information would make an MCP that is merely
not running unconfigurable. The call-time presence re-check still denies.

A grant that is missing a value is named in the UI — on the record's row, in
the editor, and in a banner over the list — because the other half of
loud-and-closed is a `denied` at call time, which is silent from the operator's
side.

## Versioning

`contextSchemaVersion: 2` marks a schema using these keywords.

**Absent or lower means v1** and is handled exactly as it was before ADR-011:
relay looks for a field literally named `allowed_dirs`, in either the flat or
the nested shape, and derives the project path into it. That is the last
domain-specific field name anywhere in relay; a test asserts it is the last one,
and it is scheduled for removal one release after every MCP relay serves
declares v2. Connecting an MCP that declares v1 logs a deprecation line.

No v2 rule fires on a v1 declaration, even one that happens to carry a key
spelled like a keyword — reading v2 semantics off an un-versioned declaration
would let an MCP change how relay enforces without saying so.

## What the MCP must do

Declaring a field is an assertion that the server enforces it. Relay injecting a
scope an MCP ignores is **worse than no scope at all**, because the UI then
asserts a confinement that does not exist. Relay cannot verify enforcement and
does not pretend to.

The reconciliation rule, so every scoping MCP implements it identically:

- An **absent or default-valued** scope-relevant argument resolves **to the
  scope**, not to everything.
- A **tool-level wildcard** argument (`mailbox: "all"`) means "everything I am
  allowed to see" and resolves to the scope. It does not error.
- An **explicit** argument outside the scope is an **error**, not a silent
  narrowing. Silent narrowing lets an agent build a false model of what it can
  reach.
- **Enumerators are scoped too.** A tool that lists accounts or mailboxes
  reports only what is in scope — otherwise it is both a disclosure and a map
  of what to try next.
- **An array restrict field's values are a union.** Each value adds reach
  and none removes it. Relay's presence gate treats a subset as narrower on
  that basis, so an MCP that let one value subtract from another would let
  an unprompted edit widen what it reaches.
- **An unset operator restrict field grants nothing, on every tool that reads
  it.** Absent, `null`, `""` and `{}` all mean unset. This holds for every
  tool that consults the field, not only the ones its `applies_to` names:
  relay refuses calls only for tools inside `applies_to`, and its presence
  gate lets an operator clear a field without a prompt because unset is
  assumed to refuse. An MCP that read an unset field as "unrestricted" on
  some other tool would turn that unprompted clear into a widening.

`_meta` being present at all is a reliable signal that a chokepoint mediated the
call: relay injects `_meta.project_id` on every mediated call and has since
ADR-007. An MCP can therefore require its own declared restrict fields whenever
`_meta` is present, needing nothing from relay beyond the fact of mediation. An
absent `_meta` means nobody mediated — an operator running the MCP over stdio by
hand, which is same-user local access.

A server that refuses a call for scope reasons may say so on the error result:

```json
{"content": [...], "isError": true, "_meta": {"scope_violation": true}}
```

Relay records that as `scope_violation: true` in the audit log with `outcome`
staying `tool_error`. It changes no outcome and gates no decision; it exists so
alerting has something to select on.
