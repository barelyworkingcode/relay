# Confining a remote client — an operator's guide

How to give an agent on another machine access to some of your mail and nothing
else, how to check that it worked, and how to take it away.

The design and its reasoning are in
[ADR-011](decisions/011-resource-scope.md). This is the procedure.

---

## The two records, and why there are two

| | what it is | what it carries |
|---|---|---|
| **Access profile** | what may be done | which MCPs, which tools, which operations, whether it may reach outside this Mac, which resources |
| **Enrolment** | who is doing it | one client certificate, a call/volume budget |

An enrolment names one or more profiles. Several enrolments may name the same
profile — three agents sharing one reviewed confinement is the expected shape.

They are separate so that **rotating a certificate does not mean re-authoring the
permission set**. Revoke an enrolment and issue a new one; the profile it points
at is untouched and never has to be retyped.

A profile is *not* a project. It has no directory, no skills, no shell, no
models, no sessions. The Settings UI calls it an access profile everywhere for
that reason; on disk it is `kind: "remote"`.

---

## What you are setting

Five independent allowlists. Each answers a different question, each fails
closed, and **none can widen another** — so you can reason about them one at a
time.

    1  which MCP           allowed_mcp_ids     relay enforces
    2  which tools         allowed_tools       relay enforces
    3  which operations    access: read|write  relay enforces
    4  outside this Mac?   allow_external      relay enforces
    5  which resources     context             the MCP enforces

**Layers 3 and 4 are separate questions and neither answers the other.**
`web_fetch` changes nothing and reaches the internet; `mail_create_draft`
changes something and touches nothing beyond this Mac. So "read-only" does not
mean "cannot send data out", and "may write" does not mean "may post". You set
them independently, and a tool needs to clear both.

**Empty never means "everything".** For a profile, an unset `allowed_tools`
means *no tools*; an unset scope value means *every tool that field governs is
denied*. There is no wildcard for resources: to allow several accounts, list
several accounts.

The one place empty means "all" is a **picker filter** while you are choosing —
before you have picked an account, the mailbox list shows all of them. That is a
query, not an authorisation.

---

## Creating a confined client

### 1. Create the profile

**Settings → Projects → + New → Access profile.**

> **Do this in this order.** Granting an MCP re-renders the form and currently
> clears the name field, and Create then refuses without saying why
> ([#25](https://github.com/barelyworkingcode/relay/issues/25)). Until that is
> fixed: **grant the MCP first, fill the name last, then Create.**

**Operations** — Read or Write. Unset defaults to `read` for a profile.
Only tools the MCP annotates `readOnlyHint: true` are admitted to a read grant;
a tool that is unannotated, malformed, or added later is refused. That is what
keeps a new mutating tool out of an old grant.

**Outside this Mac** — Refuse or Allow. **Unset defaults to Refuse for a
profile** and to Allow for a local project — the same asymmetry Operations has,
and for the same reason: a client on another machine has no way off this Mac
except through relay, while an agent running here already has your network and
usually a shell, so refusing `web_fetch` there would cost you tools and protect
nothing.

- With it **refused**, tools that reach outside this Mac are denied —
  `mail_send`, `web_fetch`, anything that talks to a network or a mail server.
- **Drafting still works.** `mail_create_draft` writes a draft on this Mac for
  a person to read and send. A profile with `access: write` and this refused
  can compose and cannot post, which is usually what you want from an agent.
- A tool whose MCP declares no `openWorldHint` **counts as reaching outside** —
  that is the MCP specification's own default and relay follows it rather than
  guessing. So while an MCP is unannotated, refusing this costs a profile
  *every* tool of that MCP, not only the networked ones. **If a profile is
  emptier than you expect, this is the first thing to check.**
- You can refuse it for a **local project** too, and the control is there for
  it. It is worth doing only when that project's agent genuinely has no other
  way out — no shell, no `curl`, nothing but relay. Otherwise you are closing a
  door in a wall that isn't there.

**Tools** — one name or pattern per line, e.g. `mail_*`.

- Patterns are **anchored**: `mail_*` admits `mail_send` and not `xmail_send`.
- A pattern that selects by *shape* rather than by name is refused — `*`, `**`,
  `?*`, `[a-z]*`, `*_*` and similar all match every tool, so all are refused.
  If you want every mail tool, write `mail_*`.
- **Empty means no tools at all.**

**Resource scope** — one control per field the MCP declares. For macMCP:

| field | what it means |
|---|---|
| `mail_accounts` | accounts this client may read from or send as. `On My Mac` is a valid entry. |
| `mail_mailboxes` | mailbox **paths** within those accounts. `Projects/Archive`, not `Archive`. |
| `file_dirs` | directories it may write files into and read attachments from. Derived from a project's path — **a profile can never have one**, so it renders read-only as *(nothing derived)* and the parameters it governs are refused. That is the intended outcome, not a gap to fill in. |

Use **Choose values…** rather than typing. It asks the MCP for the real account
and mailbox names, so you cannot typo a mailbox into existence. `mail_mailboxes`
depends on `mail_accounts`, so pick the account first and the mailbox list
narrows to it.

Accounts and mailboxes combine as a **cross-product**: accounts `[Alice, Bob]`
with mailboxes `[INBOX]` means both INBOXes. "Alice's INBOX and Bob's Archive"
is two profiles.

> If the MCP cannot enumerate, or cannot answer right now, the field falls back
> to a text box and says which. **An empty list is never shown for a call that
> failed** — "there are none" and "nothing was read" are different answers.

### 2. Create the enrolment

    relay enrol create --client-id hermes-bob --grant <profile-id>

Relay signs a client certificate and writes a bundle — `client.key` (0600),
`client.crt`, `ca.crt`. **Move** it to the client machine; don't copy it.

> An enrolment created this way does not appear in an already-open Settings
> window until you reopen it
> ([#26](https://github.com/barelyworkingcode/relay/issues/26)). It works
> immediately; only the display is stale.

**Every enrolment also carries a budget** — a call-rate limit and a
cumulative result-volume limit, both over the same rolling window. They are
budgeted together because they fail differently: a call cap alone does not
stop a *slow* drain (a mailbox read out over six hours a message at a time is
still exfiltrated), and a byte cap alone does not stop a client hammering a
cheap tool. Exceeding either is refused with its own audit outcome,
`throttled` — distinct from `denied` (a tool the grant never included) and
`tool_error` (a boundary inside the MCP) — because it says something none of
the others do: *the grant was legitimate and the pattern of use was not*,
which is what exfiltration looks like from the host's side. See ADR-010
decision 7 for the full design.

The budget is per **enrolment**, not per profile: the enrolment is the unit
of compromise (a stolen key is one enrolment's key and nothing else), so it
is the unit that carries the cap. Two agents sharing a grant have independent
budgets, and a noisy one cannot starve its neighbour.

The defaults — `--window-seconds 3600`, `--max-calls 120`,
`--max-result-bytes 67108864` (64 MiB) — are sized for a single-user host: an
hour is the natural unit for an agent that checks or triages mail, 120
calls/hour is comfortable sustained use, and 64 MiB/hour is enough for real
work including a handful of large attachments while a bulk drain still takes
days and stays loud in the audit log. They are a starting point to tune from
evidence (a `throttled` record in `relay audit` is the signal), not a
considered ceiling — set your own at creation with the three flags above, or
retune them later without touching the certificate (see "Changing a rule"
below).

### 3. Point the client at it

    export RELAY_REMOTE_BUNDLE="…/enrolments/hermes-bob"
    export RELAY_REMOTE_ADDR=127.0.0.1:9910
    relayremote list

To generate an agent skill from what the grant actually exposes:

    relayremote skill --out ~/.hermes/skills/relay

The generated `SKILL.md` carries the scope on each tool's description, so the
agent reads its own limits before choosing arguments.

---

## Checking that it worked

**Do not trust the client's report.** During this feature's own verification a
capable agent concluded *"Alice's mail is accessible through every tool"* when
it had in fact been refused — it had read mail Alice *sent to Bob*, which is
Bob's mail. Ground truth is the audit log.

Three checks, in order:

**1. What can it see?**

    relayremote list --bundle <dir> --addr <addr>

A read-only mail profile should list only the read mail tools. If you see
`web_fetch`, `capture_screenshot` or `contacts_*`, your `allowed_tools` is wider
than you think. If you see `mail_send` on a profile you meant to keep to
drafting, **Outside this Mac** is set to Allow.

**2. What happens when it reaches outside?**

    relayremote call --tool mail_get_emails --args '{"account":"<other>"}'

Must be an **error**, not an empty result. A scope violation never silently
narrows and never returns nothing — if you get an empty list, that is a
different problem.

**3. What did relay actually see?**

    relay audit --tail 20
    relay audit --outcome denied

Every record carries the authority in force. A refusal names the layer:

    denied  capture_screenshot  not in the allowed tools for MCP 'macmcp'      ← layer 2
    denied  mail_send           not annotated read-only, and this grant is
                                read-only for MCP 'macmcp'                     ← layer 3
    denied  web_fetch           reaches outside this host and this grant does
                                not allow external access for MCP 'macmcp'     ← layer 4
    tool_error  mail_get_email  scope_violation: true                          ← layer 5

Every `call_tool` record carries `access` and `allow_external` — the two
authorities relay decided by itself — so a refusal on either is answerable from
the log alone, months later, whatever the profile says by then.

**`scope_violation: true` is the signal worth watching.** It means a client
probed a resource boundary and the MCP refused it. A misconfigured scope — a
mailbox that does not exist — is an ordinary error and deliberately *not*
marked, so operator typos do not fill the security signal.

---

## Changing a rule

Edit the profile; it takes effect on the **next call**. There is nothing to
restart and no need to re-issue a certificate — relay re-resolves the grant per
request.

Two things to know:

- **Narrowing takes effect immediately.** Removing a mailbox stops the next call
  that would have used it. So does setting Outside this Mac back to Refuse — the
  next `mail_send` is denied, and any already in flight is not.
- **Editing a scope re-derives what relay owns.** Changing a mail scope will not
  silently drop a local project's `file_dirs`.

If a grant is left without a required scope value, the profile still saves — you
may want to grant an MCP before deciding the scope. It is flagged in three
places: a banner over the list, a warning in the editor, and a `denied` at call
time naming the missing field. It is never silently permissive.

**Retuning a budget is a separate operation from editing a profile — it
touches the enrolment, not the access profile — and it does not disturb the
credential**:

    relay enrol update --client-id hermes-bob --max-calls 240

Each of `--window-seconds`, `--max-calls` and `--max-result-bytes` is
independent: naming one changes only that field, and any left out keep
whatever they were set to before (not the default — an unset flag never means
"reset this"). `relay enrol update` also accepts `--grant`, which replaces the
whole grant list exactly as `create` does (so drop a profile by naming the
ones you want to keep, or `--clear-grants` to withdraw every profile at once);
it runs the same check `create` does, so a grant naming a local project or an
unknown profile id is refused. Every changed field prints as `before -> after`
so you see the actual effect.

Before this verb, changing a budget meant `revoke` + `create` — which
reissues the certificate, because that is the only thing `create` knows how
to do. Rotating a credential and retuning a limit are different concerns:
`relay enrol update` changes the number without moving the identity, so
raising a call cap costs nothing more than reading the new limit off the
output, and the client's `client.crt`/`client.key` stay exactly what they
already have on disk.

---

## Taking it away

**Revoke the credential**, leaving the profile alone:

    relay enrol revoke --client-id hermes-bob

This **closes any live connection** holding that certificate — not just future
ones. A compromised agent that never reconnects does not keep working.

**Withdraw a capability**, leaving the certificate alone: edit the profile.
Next call.

Deleting a profile that an enrolment still names leaves a dangling grant. It
fails closed at call time, but see
[#23](https://github.com/barelyworkingcode/relay/issues/23) — revoke the
enrolment first.

---

## What this does not protect against

Stated plainly, because a control you think you have is worse than one you know
you lack.

- **A profile you have granted an outbound channel can exfiltrate, and that is
  now your choice rather than the model's.** It used to be inherent: a `write`
  mail profile held `mail_send` and that was that. Today sending takes *two*
  grants — `access: write` **and** Outside this Mac — so the honest statement is
  narrower and it is about what you granted, not about what the design can
  express.

  If you do grant it, the channel is real and unbounded in the direction that
  matters: `mail_accounts` scopes the identity a message is sent **as**, never
  who it is sent **to**, so the profile can mail anything it can read to any
  address. There is no recipient allowlist yet.

  Two profiles that do *not* have that channel, and both are what you get
  without asking: a **read-only** profile has none at all — `web_fetch` is
  refused by this layer whatever `allowed_tools` says, so there is nothing to
  leave out and nothing to forget. And a **write profile with Outside this Mac
  refused** can draft but not post. Prefer either.
- **The scope confines by mailbox, not by correspondent.** A grant on Bob's
  INBOX necessarily exposes everyone who wrote to Bob.
- **Relay cannot verify that an MCP honoured the scope.** Layer 5 is the MCP's
  word. The mitigations are containment — the per-enrolment budget bounds the
  drain regardless — and the end-to-end tests, not verification.
- **Only mail is scoped.** Calendars, contacts and iMessage have no resource
  scoping yet. A profile granted those tools is bounded by `allowed_tools`, the
  mode and the outbound grant, which is three layers rather than five. Grant `mail_*` and
  nothing else until that changes.
- **Co-located agents are only as separate as the client machine makes them.**
  Relay distinguishes them by the key each presents; agents running as the same
  user can read each other's keys.

---

## See also

- [ADR-011](decisions/011-resource-scope.md) — the design and its reasoning
- [ADR-010](decisions/010-remote-client-transport-and-identity.md) — the
  certificate, the listener, the budget
- [context-schema.md](context-schema.md) — for MCP authors: how to declare a
  scope relay can carry
- [audit-log.md](audit-log.md) — every field on a record
