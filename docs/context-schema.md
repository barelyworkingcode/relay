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
    "write_dirs": {
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
finish. See [ADR-011](decisions/011-resource-scope.md) for why.

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

Plus the ordinary fragment relay uses to validate a value: `type`, `items`,
`description`. This is a deliberate JSON-Schema **subset** — array-of-string
and string are what is validated, and anything else is accepted as long as it
is present and non-empty.

### `scope: "restrict"` means fail closed

A restrict field that is missing from `_meta`, or present and empty, means the
server **refuses every call it governs**. Not "falls back to a default", not
"no restriction". `[]`, `null`, `""` and `{}` are all absent.

There is deliberately no keyword letting an MCP declare otherwise. A field that
says it restricts and then defaults open is not a restriction, and relay could
never verify the claim either way.

There is also no wildcard. "No restriction" is not expressible as emptiness and
is not expressible at all — it is spelled by enumerating, or by not granting
the MCP. An operator who wants every account lists every account, and an
account added later does not silently join the grant.

### `source` decides who fills it in

- **`project_path`** — relay derives the value from the project's `path`,
  because the schema asked it to. An array-typed field gets `[path]`; a
  string-typed one gets the bare path. A **remote-kind record (an access
  profile) has no path**, so such a field is absent for one and every tool it
  governs refuses. Relay never derives one for a profile, unconditionally,
  whatever validation decided earlier.
- **`operator`** — an operator sets it explicitly, local and remote alike. A
  restrict field with no declared `source` is treated as operator-supplied,
  because that is the reading that leaves the value un-derivable: relay
  inventing a value for a field it does not understand is the failure this
  mechanism exists to prevent.

A grant is refused at edit time if a `project_path` field's `applies_to` covers
**every** tool the MCP exposes and the record has no path — the grant would buy
nothing. fsMCP is refused to a profile for that reason; macMCP is not, and
loses exactly the two tools its `write_dirs` governs.

### `applies_to` selects tools, anchored

Patterns are matched with Go's `path.Match`, **anchored**: the pattern must
match the whole tool name. `mail_*` matches `mail_send` and not `xmail_send`,
and a pattern with no metacharacter is an exact match. The same matcher decides
`allowed_tools` on a project, so the two can never disagree about anchoring.

An **absent or empty** `applies_to` governs every tool the MCP exposes — the
domain-blind default. A pattern relay cannot compile governs everything too,
which is the fail-closed reading for a restriction.

### `enumerable` and `depends_on`

`enumerable: true` says the MCP can list this field's valid values, so the
operator UI can offer a picker over real values rather than a free-text box
where the easiest failure is a confinement that does not confine. `depends_on`
names fields whose already-chosen values must be sent as parameters, so the UI
fills in dependency order (mailboxes cannot be listed without an account).
Neither affects enforcement.

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
- an operator-supplied value for a `source: "project_path"` field — relay
  derives those, and one written by hand would be replaced at the next resync;
- a context value for an MCP that declares a **v1** schema, for the same
  reason: the v1 branch replaces the whole blob;
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
