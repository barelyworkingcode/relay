# ADR-017: The Config Dir Is Not a Boundary Against Its Owner, and the Keychain Is the One That Can Be

**Status:** Proposed. **Not implemented, and not being built today.** The owner
has decided the design and deferred the work. This record exists so the
decision does not have to be re-reached, and so the things currently resting on
a boundary that is not there are written down while they are known.
**Date:** 2026-08-28

## Context

Every protection relay has built is real against one of two callers. ADR-010
defends against a **remote** client: a certificate on a two-entry listener, no
bearer on the wire, budgets on the enrolment. ADR-011 and ADR-015 defend
against an agent **confined to relay-granted tools**: four allowlists that fail
closed, capability classes that decide what a credential reaches, `execute`
absent from the TCP mux rather than refused on it.

Neither is a boundary against code running as the owning user, and relay has
never claimed otherwise. `docs/tokens.md` says it plainly for directory auth —
"anything that could lie about its cwd can already read the token it would be
forging" — and ADR-016 decision 2 leans on the same argument for the bootstrap
anchor. That argument is correct. What has changed is what it now buys an
attacker, because the thing running as the owning user is no longer a
hypothetical script. It is an LLM agent the owner started themselves, holding a
project token, running from their own shell, with a working directory the owner
chose.

The facts, measured on this machine rather than assumed:

- `settings.json` is `0600` and holds **five project tokens in plaintext**,
  alongside `admin_secret` — a plaintext bearer the bridge accepts for
  `ReloadService` and `ReloadExternalMcp` — and every stored OAuth refresh
  token. `ca.key` (`0600`, 227 bytes) sits beside it. `0600` excludes other
  users. It excludes nothing running as the owner.
- Escalation therefore has **three doors, not one**. Read the file directly.
  Run `relay credential mint --class grant --class execute`. Or read `ca.key`
  and issue yourself a client certificate. The first needs no relay code at
  all and works while relay is stopped.
- Relay holds the personal-information entitlements —
  `personal-information.calendars`, `addressbook`, `device.audio-input` — and
  declares `automation.apple-events` so it is the responsible parent for its
  children's Apple Events (ADR-005). So what escalation inherits is not merely
  tool access. It is **the personal-information grants the user gave relay**,
  which the user gave to a tray icon and not to an agent.
- Relay's binary carries the hardened runtime
  (`flags=0x10002(adhoc,runtime)`). So relay's **memory** is a genuine
  same-user boundary — another process running as the owner cannot attach to it
  — even though relay's **files** are not. That asymmetry is the whole
  opportunity this ADR takes.
- Measured: `sandbox-exec` with `(deny file-read* (subpath "…/relay"))` blocks
  the file read **and** makes `relay credential list` answer "no credentials".
  The CLI has no authority of its own; denying the file neutralises doors one
  and two at once. That is a useful measurement and decision 6 says why relay
  cannot generally apply it.

The gap between "relay does not defend the config dir against its owner" and
"relay is a boundary against a semi-trusted agent" was invisible while the two
statements never had to be true at the same time. They do now.

## Decision

### 1. What is encrypted is what relay hands out; what relay only checks stays readable

The high-value contents are the values relay would have to **give to somebody**:
project token plaintexts, `admin_secret`, stored OAuth refresh tokens, and
`ca.key`. Everything else in `settings.json` is either structure or a
**verifier** — a SHA-256 of a credential, a passkey's public key coordinates, a
certificate fingerprint, a bootstrap code's hash.

That line is the decision, and it is not a compromise between security and
convenience. It falls out of how relay already authenticates: **authentication
needs only the hash; only brokering needs the plaintext.** `AuthenticateProject`
finds a project by token hash. `frontendCredentialAuth` resolves a bearer to a
credential by hash. `RemoteServer` resolves a certificate by fingerprint. Every
one of those keeps working with the secrets sealed. What needs the plaintext is
the narrow set of places relay *issues* — `ResolvePtyEnv`, relayLLM resolving a
project token just-in-time by `projectId` (ADR-007), `rotate_token`'s one
response that carries a token, the Projects tab's reveal — and all of those run
inside the tray.

So the sealed set moves the secrets from a place where `0600` is not a boundary
(disk) to a place where the hardened runtime is one (relay's address space).
That is the entire mechanism. It is worth stating in one sentence because
everything else here is machinery around it.

**Encrypting the whole file is refused.** The cost is not the decryption; it is
that `settings.json` stops being a file an operator can read. Three documented
behaviours die with it:

- `relay grant` — "the operator-side *what did I actually grant?*" — reads
  `settings.json` directly and works with the tray stopped. It prints MCPs,
  mode, tools and the **real** scope values, and it is the surface that names a
  scope reaching a filesystem root. It needs no plaintext token. Under
  field-level sealing it is unaffected. Under whole-file encryption it becomes
  a client of the thing it exists to audit.
- **A hand-edit is a documented recovery.** `docs/tokens.md` documents deleting
  `settings.json` as how an operator locks the control plane out, and
  `EnsureInitialized`'s refusal-to-repair behaviour assumes a human can look at
  the file and see what is wrong with it. An opaque blob turns "the file is
  corrupt" into "the file is corrupt or the key is wrong or the keychain is
  locked", with no way to tell from the outside.
- `relay audit` reads the log, not settings, and is unaffected either way — but
  it is the surface `CLAUDE.md` names as ground truth, and ground truth that
  needs a key is not ground truth.

The cost of the narrower choice, stated rather than skipped: **the structure
leaks.** Project names and paths, every MCP's command line and env keys,
enrolment client ids and their grants, service commands, scope values. An
attacker reading the file learns the shape of the machine and every place worth
attacking. That is disclosure, and this ADR is about escalation; the trade is
taken deliberately and it is the thing to revisit first if disclosure turns out
to matter more than it looks.

Two things sealing the file does **not** do, and both are load-bearing later:

- It does not close door two. `relay credential mint` writes a *hash*. Minting a
  `grant`+`execute` credential never touches a plaintext, so it works fine
  against a sealed file. Only decision 3's gate closes that door, which is
  precisely why the gate is on the operation and not on the file.
- It does not seal copies the design did not make. Encryption at rest secures
  a *path*, and secrets do not stay on one. `atomicWriteFile` stages every
  write through a uniquely-named `settings.json.*.tmp` in the same directory,
  and `docs/tokens.md` already records that a process killed between create and
  rename leaves one behind with nothing to sweep it. Time Machine and any other
  backup reads whatever bytes were there when it ran. An operator copying the
  file aside before an upgrade is doing the sensible thing. **A design that
  seals the live file and not its copies has sealed one of them**, and the
  place to fix that is the write path — plaintext must never be what lands on
  disk, at any stage, rather than something re-secured after the fact. An
  implementation that leaves a plaintext residue path open has not implemented
  this ADR.

### 2. The service is the sole broker of its own credentials, and the sole decision point

**Relay already does this, and has since ADR-007.** A project token is the
security boundary for MCP tool access, and relay does not let anyone else hold
one: Eve references a project by **id** only and `projectView` strips the token
from every response but `rotate_token`; relayLLM resolves it just-in-time from
the bridge by `projectId`, injects it into the child it spawns, and never
stores it and never accepts it from Eve. The invariant ADR-007 states is not
"never in memory" — the spawning parent necessarily holds it transiently — it
is **never stored, and never obtained from a peer that should not have it.**
Relay is the single source of truth, and everyone else asks.

This decision is that pattern applied to relay's own control-plane credentials.
It is not a new idea and it should not be argued as one. The secrets live
sealed; the running service holds them; the CLI does not mutate anything itself
— it **asks the service, the service makes a determination, and returns the
result.** Every credential relay issues then has exactly the shape a project
token already has, which is also the answer to "why should the CLI give up
writing the file": because nothing else in relay that holds a secret is allowed
to, and `settings.json` is the last place the rule was not applied.

**The determination has to rest on something the caller cannot supply.** This
is the crux, and getting it wrong makes the whole design ceremony.

If what the service determines is *"is this caller authorised"*, there is
nothing to check. The caller reached a `0600` socket, so the caller is the
owning user, and the owning user is exactly the principal this ADR opens by
establishing is not a boundary. `PeerPID` cannot rescue it — ADR-015 refused
that as an authorization input for reasons that have not changed, and a pid
would only name a process the same user chose to run anyway. A determination
that cannot fail is a rubber stamp with extra steps, and an agent that would
have written the file simply asks the service to write it instead, arriving at
the same place through the front door and leaving a nicer log.

So the determination is **user presence, per privileged operation** — a thing
the caller cannot produce, cannot forge, and cannot obtain by being the owner.
Decision 4 says which kind of presence and what it costs. What matters here is
that brokering is not what makes this safe; brokering is what gives the
presence check exactly one place to live.

**What the Secure Enclave buys, and what it does not.** The enclave holds a key
that cannot be extracted, by root or by anyone: an attacker with the sealed
file and no live machine has nothing to attack offline. That closes the *at
rest* half completely, and it is worth having. It does nothing for the *in use*
half. Relay must hold project tokens decrypted in memory to inject them into
the MCPs it spawns — ADR-007's "never stored, not never in memory" applies to
relay itself — and what guards those bytes is the hardened runtime, not the
enclave. And it does nothing at all for the operation: a key that cannot be
extracted still decrypts on request for whoever the service decides to decrypt
for.

**Enclave for the key at rest, presence for the operation. Either one alone
leaves open the door this ADR exists to close** — the enclave alone yields a
sealed file that the service will happily unseal for an agent that asks nicely,
and presence alone yields a gate in front of a file an attacker can read around.
On hardware with no enclave the key is an ordinary keychain item, so the
at-rest half degrades from "unextractable" to "bounded by the login keychain's
own encryption", and only that half degrades.

**The same-binary problem does not decide this, but it is settled by it.** The
tray and the CLI are one executable —
`/Applications/Relay.app/Contents/MacOS/relay credential list` is the app
bundle's own binary — so a trusted-application ACL naming the tray names the
CLI too. Under brokering the question does not arise: the tray is the only
process that ever asks the keychain for the key, so the ACL has one trusted
application and the question "can this binary unlock silently" has one answer
rather than two that must agree.

**Ship a separately signed CLI instead.** Available, and refused. A shipping
build carries a Developer ID, so a second identity really can be signed and the
ACL really can tell the two apart — this is a choice on merits, not a limit of
what signing can express. It buys the ACL a distinction to make and then makes
it the wrong one: a second binary that can silently unlock is door two with an
extra step, since a process running as the owner can invoke it exactly as
easily as it can invoke the first, and it would still need decision 3's gate in
front of it — at which point the separate identity has bought nothing the gate
did not already have to buy. It costs a second identity to sign, ship and keep
in step with the tray at every future change. And it fails where relay most
needs the CLI: over SSH the login keychain is locked (decision 7), so a CLI
that needs the key does not work at all, while a CLI that asks a service does.

**A property worth recording, not an accident:** the ACL is keyed on the tray's
code signature, so tampering with `/Applications/Relay.app` breaks the
signature, the ACL stops matching, and the silent unlock degrades to a prompt.
Failure lands in the safe direction — a modified relay asks, where an unmodified
one does not.

That property is what makes the grant a one-time grant rather than a recurring
prompt. A shipped relay is signed with a Developer ID, so its designated
requirement is an identifier plus an anchor: the ACL keeps matching across
every update the same identity signs, and stops matching the moment the bundle
is altered by anything else. Silent forever for relay, a prompt for a modified
relay, with no maintenance in between — the grant is asked for once and holds.

**A development build behaves differently, and that is an inconvenience to
expect rather than a property of the design.** An unsigned or ad-hoc build's
requirement is a bare hash of that exact binary, so a locally rebuilt relay is a
different application to the keychain and re-prompts on the next unlock. The
thing to watch is not the clicking; it is the temptation the clicking creates,
to loosen the ACL so the dev loop is quiet.

#### What it costs

**Relay must be running to change anything.** Every CLI command works today
with relay stopped. For two of them that is a stated affordance — `CLAUDE.md`
says `relay audit` and `relay grant` read their file directly "so it works with
the tray stopped" — and for the rest it is simply what writing `settings.json`
yourself gets you. Under brokering the surface splits, and the line is the one
decision 1 already drew:

- `relay audit` survives untouched. It reads the log, not settings.
- `relay grant` survives too, and that is decision 1 paying for itself rather
  than luck. It reads `settings.json` directly and prints MCPs, mode, outbound
  grant, tools and the real scope values — and it touches **no plaintext**: it
  builds a `StoredToken` purely to read the permission methods the router uses,
  so the CLI cannot drift from the rule. Grant shape is exactly what decision 1
  leaves unsealed, so the operator's *what did I actually grant?* surface keeps
  working against a stopped relay. Had decision 1 sealed the whole file, this
  command would have become a client of the thing it exists to audit.
- **Every mutation requires the service**: `relay credential mint`,
  `relay enrol create|revoke`, `relay login enrol`, `relay mcp register`,
  `relay service register`, project token rotation. With relay stopped they
  say so by name rather than failing with a settings error.

The general rule is **anything touching sealed data needs the service**. Today
no read command does, which is why the read half survives whole; that is a
property of where decision 1 drew its line and it stops being true the day
something seals more.

That sharpens rather than replaces the SSH question decision 7 raises. The
socket is a filesystem object, so an SSH session as the same user can reach a
tray already running with its keychain already unlocked — the transport is
fine. What is not fine is that the presence prompt lands on the GUI session's
screen. So over SSH the read half works and the mutating half refuses, and
`relay login enrol` — which ADR-016 decision 2 kept specifically *because* the
tray menu is unreachable over SSH — is on the wrong side of that line. Still
open, still the owner's call.

**And relay cannot be configured before it runs.** A machine where the tray
will not start is a machine where the operator cannot mint themselves a
credential to find out why. The break-glass is the one
`docs/tokens.md` already documents — delete `settings.json`, which locks the
control plane out and starts over — and it now has a second half, because a
fresh `settings.json` and an orphaned key are a state relay must detect rather
than fail obscurely in. **How that is detected is open**, and what settles it is
whether the recovery path is allowed to require the GUI session; if it is not,
the sealed-file design needs an offline escape hatch that is itself a door, and
that is worth knowing before building.

**The socket becomes the single point of authority, and every request on it is
same-user by definition.** That is a concentration, and it is only acceptable
because of two things that are not properties of the socket: the determination
is **per operation**, so there is no session or grant that persists past the act
it authorised, and it is **recorded**, so an operation that happened without one
is visible afterwards. The second of those is issuance auditing, and it is a
**prerequisite of this design rather than a companion to it** — the Consequences
say why, and what it still leaves open.

#### One writer, as a consequence

Not a motivation for this design, and worth naming because it is the part that
will be felt daily.

`settings.json` has more than one writer today: the tray holds it for the life
of the app, and `relay credential mint`, `relay enrol create`,
`relay service register` and `relay mcp register` each write it from a process
that exits seconds later. Nearly every settings-store defect of the last week
traces to that single fact — `With` re-reading under its own lock so a callback
derives from disk rather than a cache; every ops core resolving the record it
is about to change *inside* the callback because resolving outside is a TOCTOU
window on a file two processes write; `atomicWriteFile` staging through a
unique name after a fixed one let two writers share a staging file and tear it;
`WithDeclinable` existing at all because `With` saves whatever the callback
leaves behind, *including nothing*. And after all of it, `docs/tokens.md` still
has to say the honest thing: **it is still last-writer-wins**, because nothing
takes a cross-process lock on that file.

Brokering makes one process the only writer, and that whole class of defect
stops existing rather than being fixed one site at a time. Note precisely what
delivers it: cross-process last-writer-wins is a **write** problem, so a single
writer is the whole win — the read half of the CLI keeping its direct reads
costs nothing here, which is why decision 1's unsealed structure and this
decision's single writer compose instead of trading off.


### 3. The gate is on the operation, never on the transport

**State it as a rule, because it is the rule someone will break:** a gate that
fires on `relay credential mint` is skipped by talking to the `0600` socket
directly, and a gate that fires on the socket is skipped by whatever tomorrow's
door is. The gate belongs at the operation — in the one core each capability
already has, per ADR-014 — so that every door reaches it and no door can be
added that misses it. This is ADR-015 decision 2's argument turned inward: that
decision made a capability unreachable by *not registering the route*; this one
makes a capability un-invokable by putting the check where the act happens
rather than where the caller arrived.

The privileged operations:

| operation | why |
|---|---|
| mint a control-plane credential | issues authority |
| create or revoke an enrolment | issues or destroys a remote identity |
| rotate a project token | issues the security boundary itself |
| register or update an MCP, service or anything carrying a `command` | the caller chooses what runs |
| register a passkey / mint a bootstrap code | issues a login identity |
| widen a project's grant shape | see below |

**Is the gate simply "any `grant` or `execute` operation"? Nearly, and the gap
matters.** ADR-015's classes are a measure of what a *remote or browser* caller
can cause; escalation is a question about what a *local same-user* caller can
cause. They coincide on `grant` and `execute` — which are precisely the classes
that reach past the API's own boundary — and they diverge twice.

**`configure` contains escalation.** `PUT /api/projects/{id}` is `configure`,
and it edits `allowed_mcp_ids`, `allowed_tools`, `access`, the resource scope,
and `allow_cwd_auth`. An agent holding a project token whose project it can
edit does not need to mint anything: it widens the grant it already has.
ADR-015 classed by the blast radius of the API call; editing a grant has the
blast radius of the grant it edits. So the gate covers **project grant-shape
mutations** even though their class is `configure`. `allow_cwd_auth` deserves
its own line: turning it on hands the project's whole tool set to any process
standing in the directory, with no token at all.

**`proxy` reaches a terminal, and is not gated.** ADR-016 decision 4 says in as
many words that the catch-all can start one. Gating it would put a prompt in
front of every Eve session and every relayScheduler poll, which is unusable, so
it is not gated — and that is a **hole named rather than hidden**. Relay cannot
tell a terminal from a status poll behind that mount, because the mount is the
one whose blast radius relay does not define. This is the strongest argument yet
for the per-route classing in the manifest that ADR-015 decision 6 deferred and
ADR-016 decision 4 deferred again: it is no longer only about what a browser
credential reaches, it is about whether the escalation gate can see anything at
all through the one door it cannot classify.

So the rule is: **any operation that issues a credential, widens one, or chooses
what runs.** That is `grant` + `execute` + a named subset of `configure`, minus
a `proxy`-shaped hole that stays open until the manifest can describe itself.

### 4. The gate demands user presence, not a click

Decision 2 establishes *that* the determination must be user presence, because
at a `0600` socket there is nothing else left to check. This decides which kind
of presence, and what it costs.

A **keychain consent dialog** and a **LocalAuthentication prompt** look alike
and are not alike. A consent dialog is answered by clicking Allow; anything
holding an Accessibility grant can click it. A LocalAuthentication prompt
requiring the login password or biometry cannot be answered by something that
does not know the secret, whatever it can click.

This ADR requires:

- **The keychain's own consent dialog** for decryption by anything that is not
  the tray. This is the ACL's fallback and it needs nothing built: the trusted
  application unlocks silently, everything else asks. It is the boundary
  protecting decision 1's sealed values.
- **LocalAuthentication user presence** for every privileged operation in
  decision 3. Not a consent dialog. The gate exists to stop an agent from
  issuing itself authority, and a clickable dialog does not stop one that can
  click.

**The threat model this rests on, because it is what makes a dialog sufficient
at all:** on a production client, an LLM agent has **no Accessibility grant and
cannot click a consent dialog**. There, a dialog is a genuine boundary and the
distinction above is belt-and-braces. On a **developer machine where
Accessibility has been granted for automation** — which is this machine, and is
where relay is built and where the owner runs agents — a dialog is not a
boundary at all. The guarantee this ADR offers is therefore **not uniform
across installs**, and saying so is the point of this paragraph. Requiring
presence rather than consent for the privileged set is what keeps the guarantee
true in both places instead of only the one relay is not developed on.

Two costs.

**This machine has no Secure Enclave and no Touch ID** — the same fact ADR-016
decision 1 priced when it accepted that a passkey here means a security key or
a cross-device prompt. So user presence degrades to a **login-password prompt**.
The boundary holds, because a password prompt is not clickable; what it costs is
that every enrolment creation and every mint becomes a typed password.

**This is not a reversal of ADR-016 decision 1.** That decision refused a
password because it would put a new kind of secret — offline-guessable, chosen
by a human, reused elsewhere — into `settings.json`. Asking the OS to verify
the login password stores nothing, adds no verifier, and adds no lockout
machinery: relay never sees the password and never holds anything an offline
attack could run against. Refusing to *store* a password and asking the system
to *check* one are different acts, and only the first was refused.

### 5. This stops escalation. It does not stop use

Someone will read this ADR as a sandbox. It is not one.

An agent that already holds a project token keeps every tool that grant carries,
for as long as that grant lasts, with no prompt and no record beyond the ordinary
tool-call audit. That is by design — prompting per tool call is exactly what
relay's grant model exists to avoid, and ADR-011's four allowlists are the
control on *what* a grant reaches. Nothing here narrows a grant that was already
issued.

Nor does it touch exfiltration, which is ADR-009's and ADR-010's named threat
and is unaffected: a granted `mail_search` is a granted `mail_search`. Nor
denial of service — revocation is gated, but `rm` is not, and deleting
`settings.json` remains free to anything running as the owner and remains the
documented way to lock the control plane out.

What it changes is that an agent with a mailbox cannot turn itself into an agent
with a certificate.

### 6. Sandboxing the agent is the complementary answer, and relay cannot reach the case that matters

The measurement in the Context is the tempting one: `sandbox-exec` denying reads
under the config dir closes doors one and two simultaneously, because the CLI
has no authority of its own. It is a better boundary than anything in this ADR,
and relay can apply it to **processes relay spawns** — the `relay mcp` child,
external MCPs, enhanced services.

The owner's actual case is an agent they start themselves, from their own shell,
which relay never sees. **Relay cannot defend against a process the user starts
with their own privileges.** That is an account boundary, and macOS puts it at
the user. A separate service account would put a real one there, and
`sysadminctl` refuses to create a user without root or interaction, so it costs
an admin prompt at install — which is a decision about what relay is willing to
ask for on first run, not a technical obstacle, and it is not decided here.

The complementary direction, recorded as **deliberately unresolved**: relay
offers to *launch* the agent — `relay run --project X -- <command>` — spawning
it under a sandbox profile with a scoped token already in its environment. The
lever is not enforcement, which relay does not have on this path; it is that
**the confined path becomes the convenient one**, which is the only thing that
works against a process relay never sees. What that needs is a profile that is
tight enough to matter and loose enough that an agent can still work, and
nobody has written one.

`sandbox-exec` is **deprecated and functional** — it has warned on every
invocation for years and still does exactly what it says. Building on it is a
real bet, and it should be named as one: the supported replacement is App
Sandbox entitlements, which apply to a bundled application and not to an
arbitrary command, so there is no drop-in and no migration path if it is
withdrawn.

### 7. The keychain unlocks at GUI login, and that decides what relay can do before one

The login keychain is unlocked by the login window, with the login password. A
tray app started as a login item unlocks silently **after** that; nothing can
unlock before it.

**Starting at login is fine, starting before it is not.** Relay may be a login
item. Relay may not be a `LaunchDaemon` or a pre-login `LaunchAgent` that comes
up serving requests, because on that path it comes up holding a locked keychain
and cannot decrypt anything decision 1 sealed. This is a constraint on a
deployment relay does not currently have, and recording it now is cheaper than
discovering it during one.

**Over SSH the keychain is locked.** Confirmed on this machine:
`security find-generic-password` returns nothing over SSH until the keychain is
unlocked with the password it holds. Brokering is what rescues the transport,
and it is an independent reason for decision 2: an SSH session as the same user
can open the socket and reach a tray already running in the GUI session with its
keychain already unlocked, where a CLI holding the key itself would simply not
have worked.

**But a privileged operation over SSH is refused, not queued.** Decision 4's
presence prompt appears on the GUI session's screen, where the SSH caller
cannot see it. Blocking on it is a timeout wearing a security decision's
clothes, so relay says what happened and refuses.

That cost lands directly on something ADR-016 decision 2 valued out loud: it
refused a tray-only bootstrap code because "the tray is `LSUIElement`, its menu
is unreachable over SSH", and kept `relay login enrol` for exactly that reason.
Under this ADR, `relay login enrol` over SSH stops working — for a different
reason, but with the same result. **This is the sharpest conflict in the design
and it is not resolved here.** What would settle it is whether a presence prompt
can be *satisfied* from a non-GUI session at all; if it cannot, then either the
SSH path or the presence requirement gives way, and that is the owner's call
rather than an implementation detail.

## Consequences

**Detection is what covers whatever declines to use the front door, and it has
to land first.** The issuance auditing arriving now — `CredentialIssuance` over
`credential_cmd.go`, `enrol_cmd.go`, `login_cmd.go` and the HTTP and IPC doors —
is not a companion to this design, it is a **prerequisite for it**. A single-door
design is only as good as the evidence that a second door was used, and
`CredentialIssuance.Via` (`cli` / `ipc` / `tray` / `http`) is precisely the axis
that shows a gate being walked around: an `api_credential` issuance with no
matching presence event is the signal this ADR's entire mechanism exists to
produce. `RecordIssuance` being **durable and fail-closed** — the bytes are on
disk before the caller is told it succeeded, and `recordEnrolmentIssued` revokes
what it could not record — is what makes "the record exists or the credential
does not" a true sentence rather than an aspiration.

One hole in that, which this ADR's dependence on it makes worth naming:
`recordIssuance` returns `nil` when auditing is *off*, deliberately and for a
good reason — refusing to mint in a configuration relay supports would make
relay unusable. But an operator who set `"enabled": false` has no issuance
detection, and this design leans on issuance detection. ADR-010 faced the same
shape and made auditing a **hard dependency** of the remote listener rather than
a courtesy. If this ADR is built, issuance auditing should get the same
treatment, and that is a change to ADR-010's rule applied to a new surface
rather than a new rule.

**`settings.json` stops being fully readable, and partly stays readable on
purpose.** Decision 1 keeps `relay grant`, `relay audit` and a hand-edit alive,
and pays for it by leaving project paths, MCP command lines and enrolment grants
in the clear. An operator reading the file will see structure and no secrets,
which is the intended shape and is worth saying out loud so nobody "fixes" it
into whole-file encryption without re-reading decision 1.

**The CLI stops mutating, and stops being self-sufficient with it.** Every
command that changes anything will require relay running — a documented
affordance withdrawn, and the cost of making relay the sole broker of its own
credentials the way ADR-007 made it the sole broker of project tokens. The read
half is kept whole on purpose, so the operator's diagnostic surface never
depends on the thing being diagnosed.

**`settings.json` gets one writer, and a class of defect stops existing.** The
reload-inside-`With`, the resolve-inside-the-callback discipline in every ops
core, the unique staging name, `WithDeclinable` — all of it exists because two
processes write one file, and `docs/tokens.md` still has to admit it is
last-writer-wins regardless. A single writer retires the class rather than
patching its members. This is a consequence of the design and was not a reason
for it; it is named here because it is the part that will be felt daily.

**A locally rebuilt relay re-prompts.** Shipped builds carry one signing
identity and hold their grant across updates; a development build does not look
like the same application twice. This is a development-ergonomics cost with no
security content, and the failure mode to watch for is a loosened ACL committed
to make it go away.

**Prompts appear on a screen, so relay becomes less usable headless.** Over
SSH, and in the hermetic tier, privileged operations refuse. The hermetic suite
must never see a prompt, which means the gate needs a seam, and ADR-002's
criteria for a production seam are the bar it has to clear — a seam that
disables the gate is the weakening most likely to survive, exactly as ADR-016
decision 8's rule says.

### What an attacker can still do after all of this

**Read the whole shape of the machine.** Project names, paths and scopes; every
registered MCP's command and env keys; every enrolment's client id and grants;
every service's command. Decision 1 seals values, not structure.

**Use every tool the grant already carries, indefinitely.** A project token in a
process's environment is a project token, and a grant with no expiry is a
standing invitation. Nothing here shortens one.

**Reach the frontend socket with a credential it can find.** `0600` admits it,
ADR-016 decision 5 declines to treat that as authentication, and the credential
it needs is in the environment of any frontend consumer it can read — which,
running as the owner, is all of them. `--no-frontend-creds` remains the only
control there, and it remains a per-service opt-out that an edit path has
already silently revoked once.

**Take everything through the proxied surface.** `proxy` is not gated, the
catch-all can start a terminal, and relay cannot see through the mount to know
that it did. This is the largest remaining hole and it is the same hole issue
#50 has been open on since ADR-015.

**Wait for a prompt the owner is about to answer anyway.** Presence is a
boundary at the moment it is demanded; a process that mints while the owner is
mid-`enrol` is answered by a human who believes they are approving their own
act. Nothing in relay attributes a prompt to a request.

**Destroy things.** Revocation is gated; `rm` is not.

**On a developer machine with Accessibility granted, click through everything.**
Decision 4 narrows this to the keychain consent path and leaves presence
standing, which is the difference between "reduced" and "closed", and it is
reduced.

## See also

- [ADR-015](015-control-plane-authorization.md) — the class model decision 3
  measures escalation against, and finds nearly but not exactly sufficient; and
  the "route not registered rather than check inverted" argument decision 3
  turns inward.
- [ADR-016](016-interactive-login-and-the-view-credential.md) — decision 5's
  refusal to spend `0600` twice, which decision 2 relies on; decision 1's
  refusal of a stored password, which decision 4 does not reverse; decision 4's
  `proxy` class, which is the hole decision 3 names; and decision 2's SSH
  reachability, which decision 7 damages.
- [ADR-010](010-remote-client-transport-and-identity.md) — the enrolment CA
  whose `ca.key` is door three; the host-side-operator-act rule decision 3's
  gate is the same-user form of; and the hard-dependency-on-audit rule the
  Consequences argue should extend to issuance.
- [ADR-007](007-project-token-brokering.md) — relay as sole broker, which
  decision 2 applies to relay's own control-plane credentials; and the
  "never stored, not never in memory" invariant that decides what the enclave
  can and cannot buy.
- [ADR-008](008-tool-call-audit-log.md) — the chokepoint whose issuance
  counterpart is this design's prerequisite, and the fail-open/fail-closed split
  it established.
- [ADR-005](005-tcc-permissions.md) — why escalation inherits calendars,
  contacts, the microphone and responsible-parent Apple Events, and not merely
  tool access.
- `docs/tokens.md` — the same-user argument this ADR does not overturn, the
  plaintext-token inventory decision 1 seals, and the "no settings file means no
  credentials" recovery decision 2 has to keep working.
