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
