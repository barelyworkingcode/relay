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

**A tool name is resolved inside the grant.** Tool names are not unique across
MCPs — two filesystem MCPs, two mail MCPs, or one server registered twice under
different scopes will collide — and the grant decides which of them a call
means. A candidate is an MCP that this profile allows AND on which this profile
allows this tool: layers 1, 2 and 6, the three you typed. If exactly one
candidate remains, it serves. If **two** do, the call is refused and the error
names both, because the request carries only the bare tool name and the two
MCPs may have different resource scopes. Narrow `allowed_tools` on one of them,
disable the tool there, or drop the MCP from the profile.

Two details worth knowing, both deliberate:

- **The mode (layer 3) and the outbound grant (layer 4) do not narrow a route,
  even though either can refuse the call.** Both are decided from the MCP's own
  `readOnlyHint` / `openWorldHint`, and a route must never be a function of a
  value an MCP controls — otherwise a server could make itself the only
  candidate for a name by editing its own annotations. Everything that narrows
  a route is something a human wrote in `settings.json`.
- **A colliding name is withheld from `tools/list` and from generated skills.**
  Every call to it is refused, so advertising it would hand an agent a tool that
  can never work. Relay logs the withheld names when it does this.

A project whose `allowed_mcp_ids` is the wildcard `["*"]` allows every
registered MCP, so a name two of them expose is ambiguous under it unless
`allowed_tools` or `disabled_tools` narrows it. That is the wildcard behaving as
written: "all of them" answers *which MCPs*, and it does not answer *which one
did you mean*.

Ambiguity is judged over MCPs that are **connected**. If one collider is down,
the survivor serves the call under its own scope — relay refuses to choose
between two servers, not between a server and an absence.

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
- **Drafting still works, and what it buys is that nothing is delivered.**
  `mail_send` reaches an arbitrary address; `mail_create_draft` cannot reach a
  recipient at all — a person has to open the draft and send it. That is the
  property worth granting, and it is real.

  **What it does not buy is that the draft stays on this Mac.** Mail
  IMAP-APPENDs a draft to the account's own mail server, so anyone who can read
  that account reads it: the provider, a synced phone, a backup, anyone holding
  the credentials. Reproduced on the shipped `hermes-alice` profile, unchanged,
  with the audit line reading `allow_external: false` beside a message that had
  been handed to a server. The body size is uncapped.

  So with **Outside this Mac** refused, a write profile still has an outbound
  write **to its own account**. If that matters, the answer is a read-only
  profile, which has no compose tool at all.
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

**Resource scope** — one control per field the MCP declares. macMCP declares
nine, across four services:

| field | governs | what it means |
|---|---|---|
| `mail_accounts` | `mail_*` | accounts this client may read from or send as. `On My Mac` is a valid entry. |
| `mail_mailboxes` | `mail_*` | mailbox **paths** within those accounts. `Projects/Archive`, not `Archive`. |
| `file_dirs` | `mail_save_attachment` | directories it may write files into and read attachments from. See below — a profile can never have one. |
| `calendar_accounts` | `calendars_*` | the sources Calendar files calendars under: iCloud, On My Mac, a CalDAV server. |
| `calendars` | `calendars_*` | calendars, each written `Account/Calendar` — `iCloud/Work`. The account is part of the value because a title alone does not identify one. |
| `contact_accounts` | `contacts_*` | the containers Contacts files cards under. This bounds **cards**: every card in one of these accounts is reachable, group or no group. |
| `contact_groups` | the four group tools only | groups, written `Account/Group`. This bounds **groups**, not cards — deliberately not `contacts_*`, or "every card in this account, group or not" would be inexpressible. |
| `reminder_accounts` | `reminders_*` | the sources Reminders files lists under. |
| `reminder_lists` | `reminders_*` | lists, written `Account/List` — `iCloud/Groceries`. |

`file_dirs` is the one whose `source` is `project_path`: relay derives it from a
project's directory, and **a profile has none**, so no value for it can exist.
Tools it governs are therefore not offered to a profile at all — they are left
out of `relayremote list` and out of the generated `SKILL.md`, and refused if
called. That is the intended outcome, not a gap to fill in. The optional
parameters it governs elsewhere (`mail_send`'s and `mail_create_draft`'s
`attachments`, `mail_get_source`'s `save_to`) are refused by macMCP without
costing you the tool.

`messages_*` declares no scope field; see *What this does not protect against*.

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

### Approving a request from the machine itself

Step 2 above assumes you generate the enrolment from this Mac and carry the
result to the client. A client machine can instead lodge its own request
over the network and wait for you to approve it — worth reaching for when
handing a CSR file between machines is the inconvenient part, e.g. a fresh
VM with no shared filesystem yet.

**The operator-carried path above is the fallback that always works, and it
is documented first for that reason.** It needs no listener, no network path
between the two machines, and nothing to compare — you already hold the CSR.
Reach for the request channel when carrying a file is the friction; reach
for the fallback whenever the network path is down, untrusted, or not worth
opening for one enrolment.

**1. Turn the channel on**, if it is not already — absent or `false` means
no enrolment listener at all, exactly like `remote.enabled` itself:

    "remote": { "enabled": true, "listen": "127.0.0.1:9910",
                "enrolment_requests": true, "enrolment_listen": "127.0.0.1:9911" }

**2. Read the CA fingerprint off this Mac** — the tray shows it in
Settings → Remote Clients, or read it directly:

    relay enrol ca-fingerprint

This is the value the client pins. Write it down or copy it to wherever the
new machine can reach it; it never needs to touch the network between them.

**3. On the new machine, lodge the request:**

    relayremote request --addr 10.0.0.2:9911 --ca-fingerprint sha256:41c7… --label vm-mail-a

It prints its own public-key hash on stderr and waits, polling every couple
of seconds. Neither `--ca-fingerprint` nor `--tofu` has a default — the
command refuses at flag parse without one of them, naming both. Use
`--tofu` only from a terminal you are watching and prepared to compare by
eye; a headless VM needs `--ca-fingerprint`, which is one string read off
step 2.

**4. Compare the public-key hash, then approve** from either door:

    relay enrol requests
    relay enrol approve --id req_9f2a41c7 --client-id vm-mail-a --grant <profile-id>

or Settings → Remote Clients → Pending requests, which opens the same
create-enrolment form pre-filled — you pick grants and budget the way you
already know. Nothing about the request names its own grants; that choice is
yours to make at approval, same as it always is.

**5. The client collects the certificate on its next poll** and is told
where to point `relayremote list` / `call` afterward.

---

**Lodging a request raises no prompt, ever — this is structural, not
rate-limited.** A network peer that reaches the enrolment port can add a row
to a bounded table (capped at 8) and nothing else. No dialog and no
notification appears on your screen because a stranger lodged a request; the
human always initiates approval, and *that* act is what raises the existing
presence prompt — the same one `relay enrol sign` already asks for.

**The transport is plain TCP, and that is deliberate.** Nothing on this wire
is a secret in either direction: inbound is a CSR (public by construction,
proof of possession of a key that never leaves the client), outbound is a
certificate and a CA certificate (both public). TLS here would be decoration
that reads as a security property it cannot provide — the client has no CA
to verify a handshake against yet, so "encrypted" would not mean
"authenticated," and a reviewer skimming past a TLS connection would assume
it did. Plain TCP forces the real control, the fingerprint comparison, to be
visible and mandatory instead of implied — which is why `--ca-fingerprint`
or `--tofu` has no default: without one of them, nothing stands between this
channel and an attacker on the network path.

**Comparing the public-key hash alone does not close the man-in-the-middle
— this is the one thing not to skim.** An attacker sitting on the network
path can pass the real CSR straight through to the real relay, so the hash
you compare at step 4 matches and you approve in good faith — and then hand
its *own* CA certificate back to the waiting client instead of relay's.
Every check the client runs against what it receives (chain, key-pairing)
passes, because the certificate really is valid, just for the wrong CA.
**Only the CA-fingerprint pin closes this gap.** Comparing the key hash
proves the host is approving *this machine's* key; only the client's own
comparison of relay's CA fingerprint — step 3's `--ca-fingerprint`, checked
against step 2's value — proves the client is talking to the real relay.
Skip the CA pin (use `--tofu` and click through without truly reading it)
and every subsequent tool call, arguments included, goes to whoever answered
the request instead of to your relay host.

**The honest residual.** A flood of lodged requests holds all 8 table slots
full for as long as it runs, and — because the table has no notion of
identity to reserve a slot against — your own legitimate request is refused
a slot for that same span. No prompt is involved anywhere in this (lodging
never raises one), so this is an availability cost, not a phishing one, and
recovery needs no channel at all: fall back to the operator-carried path
above, which never touches this table.

---

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

**And do not ask the client what its scope is.** This used to be step 1 here,
and it was a check that could not fail. `relayremote list` is a CLIENT-side
tool showing a CLIENT-facing string, and for a field declaring
`disclose: "count"` that string names the number of roots and never the roots.
Against `allowed_dirs: ["/"]` it said:

    Scope: Directories this client may read, search and modify within — confined to 1 value.

and against a grant of one project folder it said the same thing, byte for
byte, while the first grant read `/etc/passwd` and listed `~/.ssh` (issue #41).
A count is not a measure of confinement. Relay now says *unrestricted (the
whole filesystem)* rather than "1 value" whenever a scope entry resolves to a
filesystem root, on that surface and every other — but the general rule stands:
**ask relay what it granted, not the client what it was told.**

Four checks, in order. The first two are host-side and answer "what did I
grant"; the last two are behavioural and answer "what happened".

**1. What did I actually grant?**

    relay grant --project hermes-bob

    ACCESS PROFILE  Hermes — Bob INBOX  (id: hermes-bob)
      macmcp         access=read   outbound=blocked  tools=mail_*
                     scope: mail_accounts = ["Bob"]
      enrolments:
        hermes-bob           cli-admin: off

This is the operator's view and it prints the **real values**, always,
whatever any field's `disclose` says — it is your machine and your grant. A
scope that reaches further than a folder is called out on its own line:

    ACCESS PROFILE  Probe  (id: probe)
      fsmcp          access=write  outbound=blocked  tools=fs_*
                     scope: allowed_dirs = ["/"]
                     ** ALLOWED_DIRS IS UNRESTRICTED (THE WHOLE FILESYSTEM) **
      no enrolments reach this profile

Run it with no `--project` to sweep every record on the machine, and `--json`
for a shape you can diff between reviews. Like `relay audit`, it reads
`settings.json` directly, so it works with the tray stopped. What it cannot do
is ask a live MCP whether it still declares these fields — check 4 answers
that.

The same information is on the Settings → Projects card, which is where a
scope is authored: the real value inline, an `UNRESTRICTED` badge beside it if
the value reaches a filesystem root or a whole home directory, and a
confirmation dialog before a grant like that is saved. fsMCP documents
`--allowed-dir /` as a deliberate opt-out that must be spelled out explicitly;
relay holds the same line, and the dialog is where you spell it out.

**2. What can it see?**

    relayremote list --bundle <dir> --addr <addr>

This is still worth running — it is the definitive answer to **which tools**,
which is the question it can answer honestly. A read-only mail profile should
list only the read mail tools. If you see `web_fetch`, `capture_screenshot` or
`contacts_*`, your `allowed_tools` is wider than you think. If you see
`mail_send` on a profile you meant to keep to drafting, **Outside this Mac** is
set to Allow. Just do not read the `Scope:` sentence as the answer to **which
resources**: that sentence is written for the client, under the field's
`disclose` setting, and check 1 is the one written for you.

**What the list shows is what the profile can call.** A tool a profile could
never call — one governed by `file_dirs`, which is derived from a project's
path and so can never have a value for a profile — is not listed at all, and is
not written into the generated `SKILL.md` either. A tool whose scope you simply
have not filled in yet *is* listed, and its description says which field is
missing; that one becomes callable the moment you type a value. A tool of an
MCP that no longer declares a field your profile scopes is not listed either,
for the same reason: relay refuses every call to that MCP under that grant
(see check 4).

**3. What happens when it reaches outside?**

    relayremote call --tool mail_get_emails --args '{"account":"<other>"}'

Must be an **error**, not an empty result. A scope violation never silently
narrows and never returns nothing — if you get an empty list, that is a
different problem.

**4. What did relay actually see?**

    relay audit --tail 20
    relay audit --outcome denied

A refusal names the layer right there in DETAIL — relay's own refusal messages
already say which check fired, so the log does not need a second taxonomy on
top of them. Driving "Hermes Mail" (`access: read`, **Outside this Mac**
refused, `allowed_tools: mail_*, web_fetch`, `mail_accounts: [Bob]`) at
`capture_screenshot`, `mail_send`, `web_fetch`, and finally `mail_get_email`
for Alice's mailbox, `relay audit --tail 4` shows:

    TIME      OUTCOME     PROJECT      MCP     TOOL                MS  CALLER  DETAIL
    14:03:11  denied      Hermes Mail  macmcp  capture_screenshot  0   -       access denied: tool 'capture_screenshot' is not in the allowed tools for MCP 'macmcp'
    14:03:15  denied      Hermes Mail  macmcp  mail_send           0   -       access denied: tool 'mail_send' is not annotated read-only and this grant is read-only for MCP 'macmcp'
    14:03:19  denied      Hermes Mail  macmcp  web_fetch           0   -       access denied: tool 'web_fetch' reaches outside this host and this grant does not allow external access for MCP 'macmcp'
    14:03:24  tool_error  Hermes Mail  macmcp  mail_get_email      0   -       scope_violation: true  {"account":"Alice"}

Each refusal names the layer that produced it. The six you can see, in the
order they are checked:

- *"is not in the allowed tools"* — layer 2. The grant never named this tool.
- *"is not annotated read-only and this grant is read-only"* — layer 3.
- *"reaches outside this host and this grant does not allow external access"* — layer 4.
- *"scopes tool X by \"field\" and this grant supplies no value for it"* — layer 5,
  and the field is simply **unset**. Set it and the tool works.
- *"scoped by \"file_dirs\", which relay derives from a project's directory"* —
  layer 5, and **unsatisfiable**: an access profile has no directory, so that
  tool can never work for one. It is withheld from the tool listing for that
  reason, rather than offered and then always refused.
- *"this grant scopes MCP 'X' by \"field\", which 'X' does not declare in its
  live context schema"* — layer 5, and the value is **unplaceable**: you wrote
  a scope and the MCP relay is talking to right now has no such field, so
  relay cannot enforce it. Every call to that MCP under that grant is refused,
  and its tools are withheld from the listing. Something changed underneath
  the profile — the MCP was downgraded, rebuilt, or replaced by a different
  binary at the same path, or the value was hand-edited into `settings.json`.
  Fix the MCP or re-author the scope against what it declares now
  (Settings → Projects re-reads the live schema every time you open it).
  Relay used to **drop** the value here and dispatch the call anyway, which
  meant the only thing standing between a client and an unconfined filesystem
  call was whether that particular MCP happened to fail closed on its own
  behalf (issue #42).

`tool_error` with `scope_violation: true` is different from all of them: the
grant was in order and the **MCP** refused, because the client named a resource
outside its scope. That is the line to alert on.

One more `denied` is not about your grant at all: *"publishes a context schema
relay cannot read"* means that MCP's own `contextSchema` has a malformed field
declaration, so relay refuses every call to it, for every grant. Relay logs the
field and the reason when the MCP connects. It is the MCP author's bug, not
yours.

(CALLER is `-` here because this was driven directly against the router in a
test harness with no attached process; a real remote call names the enrolled
client, e.g. `hermes-bob`.)

Reading it top to bottom: the first three are `denied` — relay refused the
call itself, before any MCP ran — and DETAIL names which of the five controls
did it: **which tools** (`capture_screenshot` is outside `mail_*, web_fetch`),
**which operations** (`mail_send` is not annotated read-only, and this grant
is read: only), **outside this Mac?** (`web_fetch` reaches out and this grant
refuses that). The fourth is `tool_error`, a different kind of refusal: relay
allowed the call — `mail_get_email` matches `mail_*`, is annotated read-only,
and does not reach outside — and macMCP itself refused it, which is exactly
what a resource-scope violation looks like (**which resources**, control 5,
ADR-011 decision 7). The `scope_violation: true` marker leads DETAIL so it
reads the same whether you are looking at this table or `grep`ping the JSONL
for the field it names.

**`scope_violation: true` is the signal worth watching**, and it is also a
query you can run directly, even though it is a field rather than a stored
outcome:

    relay audit --outcome scope_violation

    TIME      OUTCOME     PROJECT      MCP     TOOL            MS  CALLER  DETAIL
    14:03:24  tool_error  Hermes Mail  macmcp  mail_get_email  0   -       scope_violation: true  {"account":"Alice"}

It means a client probed a resource boundary and the MCP refused it. A
misconfigured scope — a mailbox that does not exist — is an ordinary error and
deliberately *not* marked, so operator typos do not fill the security signal.
The Settings → Tool Calls pane shows the same thing visually: a `scope`
badge next to the outcome pill, so a scope violation stands out from an
ordinary `tool_error` without expanding the row.

**Every `call_tool` record carries the authority it ran with** — the mode
(`access`), the outbound grant (`allow_external`), and the resource scope
that was injected (`scope`) — not just whether the call was allowed, because
an operator may have edited the profile since (ADR-011 decision 7). `--json`
has always carried these three fields; `--authority` puts them in the table
too, as a second line under each row that has one:

    relay audit --tail 4 --authority

    TIME      OUTCOME     PROJECT      MCP     TOOL                MS  CALLER  DETAIL
    14:03:11  denied      Hermes Mail  macmcp  capture_screenshot  0   -       access denied: tool 'capture_screenshot' is not in the allowed tools for MCP 'macmcp'
                                                                               authority: access=read  outbound=blocked  scope=mail_accounts=["Bob"]
    14:03:15  denied      Hermes Mail  macmcp  mail_send           0   -       access denied: tool 'mail_send' is not annotated read-only and this grant is read-only for MCP 'macmcp'
                                                                               authority: access=read  outbound=blocked  scope=mail_accounts=["Bob"]
    14:03:19  denied      Hermes Mail  macmcp  web_fetch           0   -       access denied: tool 'web_fetch' reaches outside this host and this grant does not allow external access for MCP 'macmcp'
                                                                               authority: access=read  outbound=blocked  scope=mail_accounts=["Bob"]
    14:03:24  tool_error  Hermes Mail  macmcp  mail_get_email      0   -       scope_violation: true  {"account":"Alice"}
                                                                               authority: access=read  outbound=blocked  scope=mail_accounts=["Bob"]

Notice `scope=mail_accounts=["Bob"]` on **every** row, including the first
three — `mail_accounts` governs `mail_*` tools, not `capture_screenshot` or
`web_fetch`, yet the record still shows it. `scope` is the authority the whole
call ran with, not only the part of it that happened to matter for this
tool: it is every `scope: "restrict"` field the MCP declares, taken from what
was actually injected into `_meta`, regardless of which fields govern which
tool. That is deliberate — the record answers "what could this call have
reached", and the grant's mailbox scope is part of that answer whether or not
the specific refusal turned on it.

**An absent scope and an empty one are different facts, and look different.**
A profile with no `mail_accounts` value at all is refused before it reaches
macMCP — `checkScopePresence`, ADR-011 decision 4's third defence — and that
denial's own authority line shows the difference from the populated one above:

    relay audit --tail 1 --authority

    TIME      OUTCOME  PROJECT      MCP     TOOL         MS  CALLER  DETAIL
    14:04:02  denied   Hermes Mail  macmcp  mail_search  0   -       access denied: MCP 'macmcp' scopes tool 'mail_search' by "mail_accounts" and this grant supplies no value for it
                                                                     authority: access=read  outbound=blocked  scope=(declared, none injected)

`scope=(declared, none injected)` is itself the finding on that record: macMCP
declares `mail_accounts` and the grant supplied nothing for it. That reads
differently on purpose from `scope=(none declared)`, which is what an MCP with
no `scope: "restrict"` field at all would show — there being nothing to
inject is a different fact from there being something and it being empty, and
conflating the two (as an earlier version of this field did) would make a
`denied`-for-missing-scope record indistinguishable from an ordinary MCP that
was never scoped in the first place. The JSON record carries the same
distinction (`"scope":{}` vs. `"scope":null`), and so does the Settings pane.

**A third fact used to hide inside `scope=(none declared)`, and it is the loud
one.** When your profile sets a value for a field the MCP's live schema does
not declare, that is not "this MCP was never scoped" — it is "the scope you
wrote is not the scope in effect". The record says so, and the call is denied:

    relay audit --tail 1 --authority

    TIME      OUTCOME  PROJECT  MCP    TOOL      MS  CALLER      DETAIL
    14:07:31  denied   Probe    fsmcp  fs_read   0   hermes-bob  access denied: this grant scopes MCP 'fsmcp' by "allowed_dirs", which 'fsmcp' does not declare in its live context schema — …
                                                                 authority: access=write  outbound=blocked  scope=(none declared)  SCOPE NOT APPLIED: this grant sets "allowed_dirs", which this MCP does not declare — call denied

The JSON record carries it as `"scope_unplaced": ["allowed_dirs"]`, which is
the field to alert on: it means an MCP changed underneath a grant. Before
issue #42 this record read `scope=(none declared)` with outcome `ok`, on a
call that had been dispatched with the operator's scope removed.

**The authority line also names a scope that is not a confinement.** A value
can be present, injected and enforced and still be the whole machine, so a
scope entry that resolves to a filesystem root or a whole home directory is
called out beside the value rather than instead of it:

    authority: access=write  outbound=blocked  scope=allowed_dirs=["/"]  SCOPE BREADTH: allowed_dirs is unrestricted (the whole filesystem)

`--authority` is off by default: the eight-column table and `--outcome` /
`--kind` / `--grep` / `--tail` keep exactly the shape scripts already parse,
whether or not the authority line is ever requested.

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
may want to grant an MCP before deciding the scope. It is flagged in four
places: a banner over the list, a warning in the editor, the tool's own
description in `relayremote list` and the generated `SKILL.md`, and a `denied`
at call time naming the missing field. It is never silently permissive.

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

## Letting the client narrow its own grant (cli-admin)

Normally only you edit a profile. `cli-admin` (ADR-018) lets one enrolment's
own certificate reach a second, small door over the remote listener and
narrow **its own** profile — without you re-typing the change yourself:

    relay enrol update --client-id hermes-bob --cli-admin

This is a grant change like any other and prompts for presence, in both
directions — turning it off prompts too, because a compromised host process
could otherwise flip it back on silently.

With the bit on, `hermes-bob`'s own certificate may reach two requests over
the remote listener that `list`/`call` never touch — `relayremote grant
describe` and `relayremote grant narrow`, run from the enrolled machine
itself:

    relayremote grant describe --bundle <dir> --addr <addr>
    relayremote grant narrow --read-only macmcp --bundle <dir> --addr <addr>

- `describe` asks its own posture back (`DescribeGrant`) — the same view
  `relay grant` shows you;
- `narrow` replaces its own `allowed_mcp_ids` / `allowed_tools` / `access` /
  `allow_external` with a **strictly narrower** set (`NarrowGrant`), via
  `--mcp-ids`, `--tools`, `--read-only` and `--no-external` (at least one is
  required; each narrows exactly one axis).

It can never:

- widen anything, on any axis — `--read-only` and `--no-external` are bare,
  comma-separated MCP-id sets with no value syntax for anything but `read` /
  `false`, so a widening on either axis cannot be *expressed* from this flag
  surface, let alone sent; `--mcp-ids` and `--tools` can still name something
  wider than the stored grant, and relay's own check refuses that, naming the
  field and the offending value;
- touch another enrolment's profile, register anything, mint a credential, or
  flip `allow_cwd_auth` (structurally absent from the wire request, and
  refused on a remote profile even if it were sent).

So `cli-admin` is a **sandbox on a sandbox**: the certificate can shrink what
it already holds, on its own, and cannot grow it or reach anything else.
Restoring a narrowed profile is your act — edit it by hand, the same as any
other change in this document.

`relay grant --project <profile>` names every enrolment reaching a profile
and marks a `cli_admin` one loudly, so the posture is visible without
cross-referencing `relay enrol list`. Every narrowing is a `config_change` in
`relay audit`, attributed to the enrolment's certificate, not to you — see
[`docs/audit-log.md`](audit-log.md).

Turn it off the moment the client's own setup is done:

    relay enrol update --client-id hermes-bob --cli-admin=false

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

## What a credential can reach, and how to store it

Three files reach the client machine, and they answer different questions.

**`client.key` + `client.crt`, together, are the credential.** Presenting
them over the remote listener authenticates as the enrolment that was
signed — every call runs *as that client*, in the audit log, reaching
exactly the access profiles that enrolment names and nothing wider. There is
no narrower identity underneath: two agents on the same VM are separated
only by which key each one holds (see *Co-located agents are only as
separate as the client machine makes them*, below).

- **`client.key` is unrecoverable.** Relay never sees it on the CSR path
  (`relay enrol sign` / `relayremote request`) and never keeps a copy on the
  legacy path (`relay enrol create`) once the bundle is handed over. Lose it
  and the remedy is the same as every other credential in
  [`docs/tokens.md`](tokens.md) whose plaintext exists once: `relay enrol
  revoke` and a fresh enrolment.
- **It must stay `0600`, and the client refuses to run otherwise.**
  `relayremote` checks the key's mode before it dials anything and refuses a
  group- or world-readable key rather than warning and continuing — a
  client that ran happily with a `0644` key would teach the operator that
  `0644` is fine, and the lesson would be learned once and misapplied to
  every bundle afterwards. `scp`, an archive extracted under a permissive
  umask, or a copy through a shared directory are all ordinary ways for a
  key to arrive wrong; `chmod 600` and rerun.
- **`ca.crt` is a verifier, not a secret.** It is relay's own certificate
  authority, public by construction — anyone may read it, and reading it
  grants nothing. Its job is the opposite of secrecy: it is the value the
  client pins so it can tell the real relay from an impostor on the network
  (see *Approving a request from the machine itself*, above, and
  [ADR-010](decisions/010-remote-client-transport-and-identity.md)).

**A `cli-admin` enrolment reaches one thing more, and only to narrow it.**
With the bit on, the certificate may call `DescribeGrant` and `NarrowGrant`
over the remote listener — `relayremote grant describe` and `relayremote
grant narrow`, from the enrolled machine — to read its own posture back, and
replace its own `allowed_mcp_ids` / `allowed_tools` / `access` /
`allow_external` with a *strictly narrower* set. It can never widen anything
on any axis, touch another enrolment's profile, register anything, or mint a
credential. See *Letting the client narrow its own grant (cli-admin)*, below,
for the full shape of what narrowing means here.

Stepping a client up and back down is two discrete, gated acts:

    relay enrol update --client-id hermes-bob --cli-admin

is a grant change like any other and demands presence, exactly as if you had
widened the profile by hand. So does turning it back off:

    relay enrol update --client-id hermes-bob --cli-admin=false

**Both directions are gated on purpose, not just the one that widens** — the
human is being told what changed, not asked to approve only the direction
that grows it. A toggle that only prompted going up would let something that
already reached the socket flip the bit back on without a fresh prompt the
moment it had been switched off quietly; gating every transition, whichever
way, keeps every change of this bit a moment a human sees and answers.
`cli-admin` is read fresh on every request relay resolves, so **off is live
on the client's very next request, with no session to tear down** — there is
no lingering grant to revoke, no connection to close, nothing left running
under the wider bit once the toggle lands.

**Native secret storage is a TODO, not built.** `client.key` stays a plain
`0600` file on every platform today. Storing it in the OS keychain instead
is a real future improvement — the macOS Keychain is a one-platform answer
for a client that also targets Linux VMs and Windows, and the Linux
equivalent (Secret Service, `libsecret`) needs a D-Bus session daemon that a
headless VM typically does not have running. Building it for one platform
and falling back to a file on the others is more surface than this
zero-dependency client currently carries for the benefit, so it stays a
`0600` file everywhere until that changes.

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
- **iMessage is not scoped.** Mail, calendars, contacts and reminders all are,
  and all four are enforced inside macMCP. `messages_*` has no resource axis
  short of per-chat, so a profile granted those tools is bounded by
  `allowed_tools`, the mode and the outbound grant — three layers rather than
  five. (This entry used to say *only* mail was scoped and to advise granting
  `mail_*` and nothing else. That has been false since macMCP #63.)
- **`openWorldHint` is a constant per tool, but whether a tool reaches off-host
  is a property of the resource it acts on.** This is the general form of the
  drafting correction above, and it is not mail-specific.
  `calendars_create_event` into a CalDAV calendar, `contacts_create` into
  CardDAV, `mail_move`, and even `mail_mark_read` — a `\Seen` flag is one bit
  per message, and one bit per message is still a channel — are all annotated
  closed-world, and all write to a server whenever the account behind them is
  server-backed. A grant cannot currently say *may write, but only to local
  stores*: **Outside this Mac** bounds which tools may run, not which resources
  they may touch. Where that distinction matters, the control that holds is
  `access: read`.
- **Co-located agents are only as separate as the client machine makes them.**
  Relay distinguishes them by the key each presents; agents running as the same
  user can read each other's keys.

---

## See also

- [install-remote-machine.md](install-remote-machine.md) — the step-by-step
  version of this document: which command runs on which machine, and what
  each one prints
- [install-single-machine.md](install-single-machine.md) — getting relay
  doing something useful on one Mac first
- [ADR-011](decisions/011-resource-scope.md) — the design and its reasoning
- [ADR-010](decisions/010-remote-client-transport-and-identity.md) — the
  certificate, the listener, the budget
- [context-schema.md](context-schema.md) — for MCP authors: how to declare a
  scope relay can carry
- [audit-log.md](audit-log.md) — every field on a record

## An MCP that declares no scope at all

Not every MCP takes a scope value. One may instead be **launched** already
confined — fsMCP is spawned with a `--root` and serves that one directory for
its whole lifetime — in which case it publishes no `contextSchema` and there is
nothing for a grant to set.

Grant these per MCP, not per value: register one instance per directory
(`fsmcp-documents`, `fsmcp-projects`), and a profile's `allowed_mcp_ids` decides
which it can reach. `relay grant` shows `scope: (none set)` for such a profile,
which is accurate rather than a gap — and `relay audit --authority` still names
the directory, from `mcp_root`.

Do **not** leave a stale scope field in a grant for an MCP that has stopped
declaring it. Relay refuses such a call outright and says so
(`SCOPE NOT APPLIED`), which is the correct fail-closed behaviour but reads as
a puzzling denial if the grant was simply never cleaned up.
