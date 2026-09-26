# The presence gate

Sealing secrets at rest (`docs/sealed-config.md`) stops an agent from reading
a plaintext off disk. It does nothing about an agent that instead asks the
running relay to *issue* it something new — mint itself a control-plane
credential, register an MCP that runs whatever it likes, widen a project's own
grant. At a `0600` socket, the caller reaching that request is, by
construction, the machine's owner: `PeerPID` cannot rescue this (ADR-015
already refused pid as an authorization input, for reasons that have not
changed), and a determination that only asks "is this caller the owner" has
nothing to check, because the answer is always yes. The presence gate is the
mechanism that makes the determination rest on something the caller cannot
supply: a user physically present at the keyboard, deciding, per act. This
document is the durable reasoning behind it; see
ADR-017 (a config directory alone is not a security boundary) decisions 3 and 4 for
the decision itself and the ADR-017 implementation spec §6 for the
wire-level detail.

## Why the gate lives in the operation core, never on a transport or in the CLI

State it as the rule it is, because it is the rule someone will eventually
break: a gate that fires on `relay credential mint` is skipped by talking to
the bridge socket directly, and a gate that fires on the socket is skipped by
whatever tomorrow's door turns out to be — an HTTP route, an IPC handler, a
new bridge request type nobody has written yet. The only place a check
survives every door is the one place every door necessarily converges: the
operation core itself, per ADR-014's "one core per capability."

Concretely, `CredentialOps.Mint`, `ProjectOps.Update`, `McpOps.Add`, and their
siblings each carry a `*presence.Gate` field and call `Require` on themselves,
before touching the store. A door — CLI, IPC, HTTP, the bridge's `admin_op`
dispatch — cannot opt out of this, because it never had the option to call
anything but the core method. A nil `Gate` refuses every gated method rather
than allowing it (`errPresenceGateNotWired`), so a build that forgot to wire
one fails closed instead of silently granting everything. A `go/ast`
call-graph test walks every route, IPC handler and bridge handler in
`package main` and fails, naming the file, if any of them reaches a gated
mutator without going through its core — this is ADR-015's "make the route
unreachable, don't register a check that can be missed" argument, turned into
a build-time assertion instead of a runtime one.

## Which operations are gated, and why each one qualifies

The rule is: **any operation that issues a credential, widens one, or chooses
what runs.** Concretely:

- **Minting or revoking a control-plane credential** (`credential.mint`,
  `credential.revoke`) — the first issues authority outright; revocation is
  gated on the same footing because an unforgeable act must not be
  reversible by an agent that merely holds the socket.
- **Creating, signing, updating, or revoking an enrolment**
  (`enrolment.create`, `enrolment.sign`, `enrolment.update`,
  `enrolment.revoke`) — issues or destroys a remote identity.
  `enrolment.update` is gated even though it only *replaces* an existing
  grant list, because replacing a grant list is exactly "widens one."
  `enrolment.sign`'s digest binds one field `enrolment.create` does not: the
  CSR's own public key, so a presence grant answered for one key is not
  redeemable for another (ADR-018 decision 6 step 1). `enrolment.sign` now
  has a second door onto the same gate — approving a network-lodged
  enrolment request (ADR-018 decision 8) — deliberately the **same** op with
  the **same** digest, not a new `enrolment.approve` entry: a second op here
  would be exactly the second door into issuance the ADR forbids.
- **Minting a login bootstrap code, or revoking a passkey**
  (`login.bootstrap.mint`, `login.passkey.revoke`) — issues or destroys a
  login identity.
- **Opening an eve passkey enrolment window** (`eve.enrolment.open`) — issues
  a second browser's window onto eve's own passkey registration
  ([`docs/eve-passkey-enrolment.md`](eve-passkey-enrolment.md)). Consuming it
  is not gated: the operator already answered this prompt to open the
  window, and a browser spending the slot narrows what it reaches rather
  than widening anything, the same footing "What is not gated, and why
  removal is not escalation" gives below.
- **Revoking an eve passkey** (`eve.passkey.revoke`) — gated on the same
  footing as `login.passkey.revoke`, even though relay never touches eve
  directly: it records a pending revocation eve will apply on its own next
  poll or login check, and an unforgeable act reaching another program's
  credential store must not be reversible by an agent that merely holds the
  socket. Reporting eve's own list and reading pending revocations are not
  gated: both are eve narrating or asking about its own state, never
  choosing what relay issues.
- **Registering an MCP or a service, and starting an MCP's OAuth ceremony**
  (`mcp.register`, `mcp.oauth.start`, `service.register`) — the caller
  chooses what runs, or what bearer relay will hold and present upstream on
  the caller's say-so. `mcp.oauth.start` earns its place on this list for
  the same reason `oauth_state` values are sealed: it persists a bearer
  relay presents to a third party, and it opens a browser at a URL the
  caller chose. Unregistering either kind is deliberately **not** gated —
  see "What is not gated, and why removal is not escalation" below.
  `service.register`'s update path applies decision 1 to one field within
  itself: `command`, `args`, `working_dir`, `url` and `autostart` have no
  narrower reading (any actual change to what runs, or how, gates), and a
  request that changes nothing at all (the Settings window resends the whole
  record on every save) needs no gate either
  (`serviceUpdateNeedsGate`, `cmd/relay/service_ops.go`). `display_name`,
  `capabilities` and `allowed_models` gate on ANY actual change, in either
  direction — an earlier revision treated dropping a capability or a model
  id as narrow-safe and never inspected `display_name` at all, correct while
  only an operator-minted credential could reach this route; since eve's
  frontend launch identity was granted `execute` (F1/SP8, below), any
  frontend-capable service can reach this route for ANY service's record,
  not just its own, so a silent rename or a silent capability/model
  narrowing became a real integrity/availability exposure and both now gate
  (`STATUS-relay-security.md`; `setEqual`, `serviceCapabilitiesChanged`,
  `serviceAllowedModelsChanged`, `cmd/relay/service_ops.go`). The one
  exception: if both the stored and requested `allowed_models` already hold
  the wildcard `"*"`, nothing else listed alongside it changes what's
  reachable, so that comparison alone does not gate.
  docs/launch-identity.md's "Editing capabilities from the Settings window"
  has the detail.
- **Rotating a project token** (`project.rotate_token`) — issues the security
  boundary itself.
- **Widening a project's grant shape** (`project.grant`) — see below.
- **Changing relay's remote-listener configuration** (`remote.configure`,
  `PUT /api/remote` / IPC `update_remote_config`, both onto
  `EnrolmentOps.SetRemoteConfig`) — the caller chooses what reaches the host:
  the bind address either listener answers on, and whether either is
  reachable at all. `execute` was classed at the route (ADR-015) on the
  strength of `POST /api/mcps`, `POST /api/services` and
  `PUT /api/services/{id}` alone; `PUT /api/remote` carried the same class
  but no gate at its core, the one `execute` route a caller holding only
  that class could reach with nothing standing in front of it. `listen` and
  `enrolment_listen` gate on any actual change in either direction — there
  is no generic rule for when one bind address is "narrower" than another
  (`remoteConfigChangedFields`, `cmd/relay/enrolment_ops.go`). `enabled` and
  `enrolment_requests`, and `Remove` (clearing the block entirely), now gate
  on ANY change, turning off and removing included — an earlier revision
  read "off only closes a listener that was already reachable" and "Remove
  is strictly narrower than any state it could replace" as never needing a
  gate, correct while only an operator-minted credential could reach this
  route; since eve's frontend launch identity was granted `execute` (F1/SP8,
  below), any frontend-capable service can silently disable or wipe relay's
  remote configuration, so both now gate too (`removePresenceDigest`,
  `STATUS-relay-security.md`). Removing an already-absent block, and a
  resend of the exact stored record, still need no gate either — neither
  changes anything. Deliberately **not**
  behind `requireIssuanceAuditor`, unlike every other entry on this list: the
  remote listener already has its own, separate audit rule (ADR-010) that
  gates whether it actually *serves* traffic, checked later by
  `RemoteSupervisor` at reconcile time, not whether the setting can be saved
  — an operator must be able to persist `enabled: true` (and see it read back
  as configured) while auditing is off, exactly as before this operation was
  gated.

**The `configure` subset, and why `allow_external` is in it.**
`PUT /api/projects/{id}` is classed `configure` under ADR-015, which
measures blast radius by HTTP verb and route. But an agent holding a token for
a project it can also edit does not need to mint anything new: it can simply
widen the grant it already has by editing `allowed_mcp_ids`, `allowed_tools`,
`access`, the resource scope, or `allow_external`. Editing a
grant has the blast radius of the grant it edits, not the blast radius of a
generic `configure` call, so this narrow slice of `configure` is gated on the
same footing as `grant` and `execute`.
`allow_external` is ADR-011's second axis of the same permission set — an
agent that can turn on outbound access for an MCP it already reaches does not
need a mint either, and is gated identically to `allowed_tools`. A rename, a
chat-template edit, or any other field outside that named set does not widen
what the project's token reaches, and does not prompt — `disabled_tools` in
particular is a denylist that can only narrow, and narrowing is never gated.
(The retired `allow_cwd_auth` field used to be named on its own here, for the
same reason: turning it on handed the project's entire tool set to any
process merely standing in the project's directory, with no token check at
all. plan-broker-and-sessions.md §2 C3 removes the field and the mechanism
entirely rather than continuing to gate it.)

## Why `proxy` is not gated

The `/` catch-all reaches whatever an enhanced service's manifest declares —
an interactive terminal, a scheduler's status poll, anything in between —
and relay cannot see through that mount to tell which kind of traffic is
which. Gating it would put a presence prompt in front of every session a user
opens *and* every automated poll a scheduler runs, which is not a security
control, it is a machine that stops working. This is a **named hole, not an
oversight**: closing it properly needs per-route classing in the service
manifest — describing blast radius at the route level rather than at the
mount level — which is a cross-repository protocol change tracked separately
and is explicitly out of scope here. `proxy` stays reachable exactly as it
was before this work, with no new gate and no removed one.

## What is not gated, and why removal is not escalation

The rule stated above is deliberately narrower than "every mutation":
**obtaining or widening** a capability is the privileged act; **using** one,
including narrowing or destroying it, is not (ADR-018 decision 1). That is
already relay's practice — a project's `NarrowForEnrolment` path is ungated
because a widening is unrepresentable before it is ever reached
(`internal/project/narrowing.go`) — and ADR-018 step 3 applies the same rule to two
operations that used to be gated by an earlier, more conservative reading:
**`mcp.unregister`** (`McpOps.Remove`) and **`service.unregister`**
(`ServiceOps.Remove`).

Removing an MCP or a service registration only shrinks what a caller
already reaches. It cannot be used to escalate: re-registering under the
same id, pointed at a different command, still has to pass through
`mcp.register` or `service.register`'s gate — the digest binds the id, so a
grant approved for one command can never be redeemed for another
(`mcpRegisterReason` exists to surface exactly this substitution). Stopping
a running service is already ungated `configure`
(`POST /api/services/{id}/stop`), so unregistering it removes only the
record, not a control that was ever behind a prompt.

**What does not change:** `requireIssuanceAuditor` still runs first, and
`recordConfigChange` still writes a `config_change` record naming the event,
the id, and how the act was reached. Only `presence_id` comes back empty —
the record still exists and is still greppable, and `docs/audit-log.md`
names both ops as the two that legitimately carry no `presence_id`. Removing
the gate must not turn "refuses when auditing is off" into "proceeds
silently": both methods still refuse with `errIssuanceAuditingRequired` when
there is no sink to record into, before anything is touched.

**What does change:** both commands now work over SSH (§"Why the peer's
audit session decides..." below no longer applies to them — there is no
prompt for a session to fail to display), and a local process can call
either without a password. This is a genuine, named cost, not an oversight:
see ADR-018's step-3 record for the escalation analysis
(`docs/decisions/018-configuration-is-a-capability-of-an-identity.md`) and
`docs/decisions/017-implementation-spec.md` §6.4, whose rows for these two
ops are marked narrowed rather than deleted — the record of what used to be
gated, and why, is kept.

**`project.grant` stays gated**, unlike these two — an update to any of its
ten fields *can* widen the grant, so decision 1's rule keeps every one of
them a candidate. What decision 1 also requires, and what the operator door
did not do until it was fixed as part of the Settings-window capability
work below, is judging each *value* the same way: a field the request
carries but does not actually change (the Settings window resends the whole
record on every save), and a field that changes only by narrowing (fewer
MCPs, a smaller tool pattern, write moved to read), must not gate, and the
prompt must name only the field or fields that genuinely widen —
`project.UpdateWidensGrant` (`internal/project/grant_widening.go`) is that
per-field comparison, called from `ProjectOps.Update` before
`projectUpdateDigest` and the reason string are built. For `context` it
ignores MCP entries with no fields (empty, `null`, `{}`), and, on the stored
side only, ignores a field a v2 schema derives from the project path
(`source: "project_path"`) when the project is local and not hosted and the
stored value is exactly what relay derives from `path`: the Settings form
cannot resend it, and the write re-derives it from `path`, which is gated on
its own row. What remains widens unless every MCP entry is equal or
narrower. An entry whose MCP has a live, usable v2 schema is compared field
by field, and an operator-supplied restrict field (`scope: "restrict"`,
`source: "operator"` or no source) has a narrowing order. Leaving it unset
(absent, `null`, `""`, `{}`) never widens, because an unset restrict field
refuses every call it governs, and for the same reason any asserted value
where it was unset widens, including `[]`: an empty array turns a refusing
field into one that accepts calls. Between two asserted arrays, a request
whose every element is already stored narrows, so removing a value,
reordering, de-duplicating and `[]` do not prompt. The wildcard here is
`["*"]` only: it narrows to any array, and requesting it widens from
anything else, including a mixed `["*","Bob"]`. A bare string `"*"`, or any
other asserted non-array value, compares strictly. Outside operator restrict fields
everything compares strictly, by decoded value with numbers compared by
exact decimal value (so `1.0` and `1e0` equal `1`, and integers above 2^53
that differ stay different): non-restrict fields,
`project_path` fields, stale keys (so `null` against a missing key, or a
reordered array, still counts as a change there), every field of an MCP
whose schema is v1, unusable or not live, a blob that does not decode, a
derived field whose stored value differs from the derivation, a
derived-looking field on a remote or hosted project, and a derived field
the request itself carries. Validation agrees with this comparison on v1:
on a local, unhosted project it accepts a v1 blob exactly when the stored
record has one for that MCP and the two are equal by the same decoded-value
comparison, so an echo that validation accepts never prompts. The gate runs
before validation, so a v1 blob that validation refuses may still prompt
first; the refusal then follows the approval and nothing is written. With
no live schema for the MCP (or no surfaces at all) nothing is stripped and
the entry compares strictly. The digest itself is unchanged by this: it
still binds all ten fields' presence exactly as before (see `TestProjectUpdateFields_DigestBindsAllNineGrantShapeFields`'s
own comment for why shrinking the digest to the gating subset would be
wrong), so a grant answered for one shape still cannot be redeemed for a
different one.

## The nonce model: single-use, operation-and-argument-bound, 120 seconds

A presence check, on success, mints a nonce bound to **both** the operation
name and a digest of that operation's normalised arguments. It is single-use
— burned on the first successful redemption — and expires 120 seconds after
issue. There is no ambient session, no sudo-style "remembered for N minutes,"
and no per-class window.

The reason this shape matters is a specific, otherwise-plausible attack:
an agent that mints a request the instant it sees the owner about to approve
something else, hoping the human — who is genuinely mid-approval and believes
they are approving their own act — answers the prompt without noticing it
authorizes a different one. Binding the nonce to the argument digest, not
just the operation name, is what defeats this: a prompt answered for
`credential mint --name x --class read` cannot be redeemed for
`credential mint --name x --class grant --class execute`, and a prompt
answered while creating an enrolment cannot be spent minting a credential.
Binding it to *only* the operation name would have stopped the second case
and missed the first.

`Gate.Require` is the only path production code uses: it requests and
redeems in the same call, so the live window between a successful prompt and
the act it authorizes is microseconds, not the full 120 seconds. That bound
is the outer limit for a caller that splits request from redemption — nothing
does that today.

## Why every value inside a collection is length-prefixed

The digest is built field by field, and every collection — a set of classes,
a map of env values, a sequence of argv — is encoded with an explicit length
prefix ahead of its contents, never joined with a delimiter. The reason is
concrete: without a length prefix, the two-element sequence `["ab", "c"]` and
the single-element sequence `["a", "bc"]` encode to the identical byte string
under naive concatenation. A digest that collided this way would let a prompt
answered for one argument list authorize a different one — exactly the
forgery the digest exists to prevent. Length-prefixing every string, every
collection, and every field name closes this: no value, however constructed,
can be crafted to impersonate a field boundary.

The same discipline extends to presence versus absence. Relay distinguishes
"this field was not supplied" from "this field was supplied as empty"
everywhere in its data model — a nil `allowed_tools` map means "no change," an
empty one means "clear everything" — and the digest has to preserve that
distinction rather than flattening both to the same bytes. Every field
carries an explicit presence byte ahead of its encoded value, so an absent
duration, an absent map, and their zero-valued-but-present counterparts never
collide.

## Presence, not a consent dialog — and why the distinction is load-bearing

A keychain consent dialog and a LocalAuthentication presence prompt look
similar and are not the same thing. A consent dialog is answered by clicking
Allow, and anything holding an Accessibility grant can click a button.
Requiring the login password — which is what
`LAPolicyDeviceOwnerAuthentication` does on hardware with no Secure Enclave
and no Touch ID, which is what this machine has — cannot be answered by
something that does not know the password, no matter what it can click.

**This is not a reversal of ADR-016's refusal to store a password.** That
decision refused to put a new, offline-guessable secret into
`settings.json`. Asking the operating system to *verify* the login password
stores nothing new, adds no verifier of relay's own, and adds no lockout
machinery relay would have to build — relay never sees the password and
never holds anything an offline attacker could run against. Refusing to
*store* a password and asking the system to *check* one are different acts;
only the first was ever refused.

**Honest limitation, stated rather than hidden:** the presence prompt is not
attributed to the process that requested it. macOS titles the dialog from the
requesting process's name — never its code signature — so any local process
capable of raising a LocalAuthentication prompt can name itself convincingly
and phish the same password a genuine relay prompt would ask for. This is a
known, accepted residual risk (tracked outside this repository) and it places one
negative requirement on every document and every string this feature writes:
**nothing may claim a user can tell a genuine relay prompt from a fake one**,
because they cannot.

**On a machine where Accessibility has been granted for automation — which is
this machine, since it is where relay is built and where the owner runs
agents — a consent dialog is not a boundary at all**, because an
Accessibility-capable process can click through one. On a production install,
an LLM agent typically has no such grant and cannot click anything. Requiring
presence rather than mere consent for the gated set is what keeps the
guarantee true in both places, rather than only the one relay happens not to
be developed on.

## Why the peer's audit session decides whether a prompt can be shown

Over SSH, LocalAuthentication does not refuse on its own — an SSH-launched
process can still raise a real password prompt onto the physical console and
block there, which would mean an owner sitting at their Mac gets interrupted
by a prompt a remote shell triggered, with no way to tell why. Relay refuses
this itself, and the refusal has to rest on something the caller cannot fake.
`SSH_TTY` and `SSH_CONNECTION` are environment variables the caller
controls — a rogue agent unsets them — so relay never reads them anywhere in
this feature.

What it reads instead is a kernel-attested property of the connection's
*peer*: `getsockopt(LOCAL_PEERTOKEN)` resolves to an audit session id, and
`auditon(A_GETSINFO_ADDR)` reports whether that session has graphic (console)
access. This is unprivileged and unforgeable — a process cannot join or forge
another audit session (`setaudit_addr` returns `EPERM` for a non-root caller),
and `sudo` inherits the invoking session rather than escaping it. Critically,
this must be asked about the **peer**, never about relay's own process: the
tray is always running in the Aqua session with graphic access, so a check
that asked about itself would always answer "yes, I can prompt" and fail
open regardless of where the request actually came from.

**Named limitation, not designed around:** `open -a Relay.app` launched from
an SSH shell re-enters the console session and reports graphic access, so
this mechanism answers "can this process display UI," not "did this
connection originate over SSH." That gap is accepted on purpose. The refusal
this check produces is **honesty and prompt-spam reduction** — it stops the
ordinary case where an SSH session obviously cannot see a prompt on someone
else's screen — and it is explicitly **not a security boundary** of its own.
The boundary is the password inside the prompt, exactly as it is for a local
caller; a caller that finds a way to make the graphic-access check pass still
has to answer the same LocalAuthentication challenge everyone else does.

A caller with no determinable session at all — the WebView IPC, the tray
menu, the loopback TCP mux — is treated as *able* to prompt, not refused: the
only doors with no peer to interrogate are doors that are, by construction,
already local to the tray itself, and refusing them would break the Settings
window and the browser login view for no security gain. A peer whose session
genuinely could not be resolved (a probe error, a non-Unix-socket
connection) is folded into "cannot display a prompt," never into "no
information, so allow" — those are different failure directions and only one
of them fails safely.

## Why gated calls never run on the Cocoa main thread

`evaluatePolicy` is asynchronous and delivers its result through a completion
block on a queue LocalAuthentication itself chooses. If the calling goroutine
were the one that also owns the tray's Cocoa run loop, the dialog would need
that same run loop pumped to deliver the very completion the calling code is
blocked waiting for — a deadlock, not a slow prompt. Every gated call
therefore runs its `Evaluate` off the main thread, in a tracked goroutine that
blocks on a channel until the callback fires or the context is cancelled.

## The hermetic seam, and why it cannot exist in a shipped build

The test suite must never raise a real password prompt — a developer running
`go test ./...` who gets a login-password dialog has found a bug, not a
feature. The fake provider (`presence/presencetest`) lives in a package of
its own, and `package main`'s non-test code never imports it — checkable, and
checked: a source-level test parses every non-test `.go` file in the module
and fails on any import, transitive or direct, of `presencetest`. Because Go
links only what is actually imported, that passing test is a proof the
compiled `relay` binary does not contain the fake at all, not merely evidence
that nothing currently calls it. `presencetest` additionally panics in its
own `init()` unless the process is running under `go test` — a second,
independent line of defense if the import guard is ever weakened.

There is no environment variable, settings field, build tag, or
`SetPresenceForTest`-shaped global anywhere in this feature that could
disable or weaken the gate; a grep-based test enforces that absence directly.
This is deliberate and total, in the spirit of the house rule that a
weakening introduced to make a test convenient is the weakening most likely
to survive into production: rather than build a seam and discipline everyone
never to flip it, there is no seam to flip. Tests wire a fake provider the
same way they wire a sandboxed config directory — by constructing the thing
under test with it directly — which adds no production-visible API at all.

## Residual risks, named rather than solved

**The presence prompt is impersonable.** macOS titles a LocalAuthentication
dialog from the requesting process's display name, never from its code
signature, so a local process with the right entitlement can present a
convincing fake and phish the same login password a genuine relay prompt
would request. This is accepted, tracked separately, and the only thing this
work requires of it is negative: no comment, document, or UI string may claim
otherwise.

**`proxy` remains ungated.** Named above, and repeated here because it is the
single largest remaining hole in what this gate covers: relay cannot see
through the manifest-driven catch-all to tell a terminal from a status poll,
so nothing behind that mount gets a presence check at all.
