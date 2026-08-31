# ADR-019: Registering a Machine Is One Command and One Comparison

**Status:** Accepted. **Implemented** across both repositories.
**Date:** 2026-08-30

The build is specified in
[019-implementation-spec.md](019-implementation-spec.md), which was measured
against the code where this document was not. Where the two disagreed on a
*fact*, the spec won and this document has been corrected below; every such
correction is marked. Where they disagreed on a *decision*, this document
won — with one exception, spec §9.1, where §3's original security argument
did not hold and the protocol changed to make this decision's own claim true.
That change is folded into §3 below rather than left in a footnote, because
the mechanism is the decision here.

The operator-facing consequences live in
[`docs/install-remote-machine.md`](../install-remote-machine.md),
[`docs/access-profiles.md`](../access-profiles.md) and relayRemote's
`README.md`.

## Context

Enrolling a second machine works and nobody would call it easy. Today it costs
five steps across two machines, three of which exist only to move a value by
hand:

1. Mac — turn on `enrolment_requests`.
2. Mac — `relay enrol ca-fingerprint`, carry 64 hex characters out of band.
3. New machine — `relayremote request --addr host:9911 --ca-fingerprint sha256:… --label … --bundle DIR`.
4. Mac — `relay enrol requests`, compare a public-key hash by eye, `relay enrol approve --id … --client-id … --grant …`.
5. New machine — poll collects the certificate; every call afterwards carries
   `--bundle` and `--addr`, and `--project` once a second grant exists.

Nothing on the Mac announces that step 4 is waiting, and step 5 leaves the
client with no notion of *which* registration it is using — the bundle
directory is the whole identity model on the client side.

Three separate things make this heavy, and only one of them is security:

- **The CA fingerprint is carried by hand** because the request channel is
  plain TCP and pinning is the only thing that closes a man-in-the-middle
  (ADR-018 §8). That control is real and is not being removed here.
- **The approval is invisible** because ADR-018 §8 property P1 makes "a lodge
  raises nothing on your screen" structural rather than rate-limited.
- **The client has no registration store.** `--bundle` is a path, not a name,
  so two enrolments on one machine are two environment variables the operator
  keeps straight in their head.

The target is a machine that joins with one command and one comparison:

    relayremote register "Hermes Mail" --host 192.168.64.1

## Decision

### 1. Registration returns no token, because the identity is the key

`register` generates the client private key on the machine being registered
and it never leaves. What crosses the wire in either direction stays exactly
what ADR-018 §8 property P2 established: a CSR inbound, a client certificate
and a CA certificate outbound — public artifacts, useless without a key the
network never sees.

**No token is minted and none is needed.** ADR-010 §2 removed the bearer token
from the remote path so a stolen `settings.json` grants no remote access at
all; reintroducing one here would place a replayable long-lived secret at the
single point in the system where the result is a *new identity* rather than a
single call. `register` succeeds by writing a private key to disk at `0600`,
not by receiving something.

The command's stdout is therefore a confirmation and never a credential: the
registration name relay assigned, and what the grant can actually reach.

### 2. A tool call presents the certificate, and nothing else exists to present

Unchanged from ADR-010 §2 and §4, and restated here because it is the answer
to "what does the next call send." Relay resolves the client certificate to an
enrolment record before the first request byte is read. `RemoteRequest` has no
token field and no cwd field, so there is no second thing a caller could
present even in error.

`ProjectID` is the one further value, and only when an enrolment holds more
than one grant. It is an id, not a secret; relay honours it only if that
enrolment actually holds the grant. A registration records a default project
so the flag disappears in the common case.

### 3. One short code replaces two comparisons

The pin does not go away; **the two hand-carried comparisons collapse into one
six-character code neither machine had to be told in advance.** The client
prints it while waiting, the host's approval sheet shows it beside the
requested name, and the human compares two short strings on two screens.

**The code must be built as a commitment exchange, and a naive hash of the two
public keys is not good enough.** The obvious construction —
`truncate(H(CA SPKI ‖ CSR SPKI), 30 bits)` — is broken, and the reasoning that
made it look sound is worth recording so nobody reintroduces it. Both of its
inputs are public: the attacker on the path reads the CSR, and it obtains
relay's CA certificate by lodging a request of its own, since §8 P2 of ADR-018
makes that certificate public and freely handed out. So the attacker can
compute the value the host will display, exactly, and then grind its *own* CA
keypair until its substitute produces the same six characters. At roughly a
microsecond per candidate that is about eighteen minutes on one core and
seconds on a GPU — inside the ten minutes the client waits and the fifteen the
request lives.

The fix is a commit–reveal, and it costs one field on the lodge, two on the
acknowledgement, one on the poll, and no extra round trip:

    R_C     16 random bytes, chosen by the client
    R_R     16 random bytes, minted by relay after the commitment arrives

    commit  = H( "relay.sas.commit.v1\0" ‖ H(csrSPKI) ‖ R_C )
    SAS     = enc30( H( "relay.sas.v1\0" ‖ H(caSPKI) ‖ H(csrSPKI) ‖ R_C ‖ R_R ) )

The client commits to `R_C` when it lodges and opens it only on its first poll;
relay mints `R_R` only once the commitment is in hand. Neither side can compute
the comparison value while it still has freedom to choose an input, so neither
value is grindable and the attacker is reduced to one blind guess per lodged
row. The bounded pending table caps its parallelism at eight, which puts the
margin at roughly one in 134 million per approval.

**`maxPendingEnrolmentRequests = 8` is therefore load-bearing for the security
bound, not only for availability.** It was sized for a human walking to a Mac;
it is now also the attacker's parallelism against a 30-bit comparison, and the
claimed margin is the product of the two. Raising it for convenience degrades
that margin linearly, and the constant carries a comment saying so. Anyone
proposing a larger table is proposing a weaker comparison and should say which
number they are trading.

This is the construction Bluetooth numeric comparison actually uses. An earlier
draft of this decision cited Bluetooth as the precedent while omitting the
commitment — which is the part that does the work. Six characters is safe
*because* of the commitment, not because the number is short and watched.

The code binds both halves of the exchange, so both substitutions a
man-in-the-middle can attempt change it:

| Attack | Host shows | Client computes |
|---|---|---|
| attacker lodges its own key | over `CA_relay`, `SPKI_attacker` | over `CA_attacker`, `SPKI_client` |
| attacker forwards the real CSR | over `CA_relay`, `SPKI_client` | over `CA_attacker`, `SPKI_client` |

Either way the two codes differ and the human refuses. This matters because
comparing the *key* hash alone does not close the man-in-the-middle —
`docs/access-profiles.md` warns about it at length: an attacker can pass the
real CSR through to the real relay, so the hash the operator approves matches
in good faith, and then answer the waiting client with its own CA certificate.
Every check the client runs against what it receives passes, because the
certificate really is valid, just for the wrong CA.

**This narrows ADR-018 §8**, which requires `--ca-fingerprint` or an explicit
`--tofu` with no default. Under this decision the SAS comparison is the
mandatory control and carries the same weight: `register` refuses to complete
without an answer, there is no flag that skips it, and a non-interactive caller
must supply `--ca-fingerprint` exactly as before. What is retired is the
*hand-carried* fingerprint as the only path, not the pin as a control.

**As built**, that resolves into four rules, and `relayremote request` keeps
ADR-018 §8's wording untouched in all four:

- interactive `register` prints the code at lodge and requires a typed `y` to
  *"the Mac showed `7K3P4Q`. Did it match? [y/N]"* before a byte reaches disk;
- `register --ca-fingerprint sha256:…` checks the pin programmatically and
  reads no confirmation. It is a *stronger* check supplied out of band, not a
  way to skip one — prompting a human to eyeball a code a machine has already
  proven equal is how you train people to answer `y` without looking;
- `register` with a non-terminal stdin and no `--ca-fingerprint` is refused
  before a key is generated and before anything is dialled;
- `--tofu` does not exist on `register` and is refused at flag parse with a
  message naming the code as what replaced it and `relayremote request` as
  where `--tofu` still lives.

**And the host enforces the comparison independently of the client.** A row
that carries a commitment but was never opened, or whose opening failed, is
refused by `EnrolmentOps.Approve` from every door — CLI, IPC, HTTP — *before*
the presence gate is reached, and the pending panel renders no enabled Approve
control for it. An operator cannot click past a comparison the client never
completed, and is not asked for Touch ID for an act that is going to be
refused. A legacy `relayremote request` row carries no commitment, shows `-`
in the SAS column, and approves exactly as it always did.

### 4. A lodge may raise a notification, never a prompt

ADR-018 §8 property P1 stands where it matters — **no code path from an
unauthenticated lodge reaches `presence.Gate`.** What changes is that a lodge
may now raise a dismissible tray notification. Clicking it opens the approval
sheet; the sheet raises the existing, unchanged `enrolment.sign` presence
prompt. The human still initiates approval, and approval is still the only act
that prompts.

**This weakens a stated property and the weakening is written down rather than
argued away.** ADR-018 §8 and `docs/access-profiles.md` both say that no
notification appears because a stranger lodged a request. After this decision,
a network peer that reaches the enrolment port can make a notification appear.
That is the cost of the flow the owner asked for, and it is bounded rather than
unbounded:

- the enrolment listener is opt-in and absent by default, so the surface
  exists only where an operator turned it on;
- notifications coalesce to one regardless of how many requests are pending,
  so a flood produces one banner, not a stream;
- notifications are rate-limited independently of the table — **as built, at
  most one a minute and six in a rolling hour**, and a flood that trips the
  limit is swallowed rather than queued to fire later, because a queue turns a
  cap into a delay;
- only a **new** row can raise one: the banner is driven by a lodge-generation
  counter, so re-lodges, polls, refusals and throttles move nothing;
- none is raised while the Settings window is open, since the panel already
  updates live and a banner over the window you are looking at is noise;
- once the bounded pending table is full, lodging is refused and no further
  notification is raised — the existing availability residual is unchanged and
  gains no new phishing dimension;
- the body carries a count and nothing else. No label, no requested profile,
  no comparison code, no request id: nothing an unauthenticated peer supplied
  reaches an operator's screen through it.

Worst case for an attacker holding an open enrolment port: **six dismissible
banners an hour, permanently.**

**The notification is a discoverability improvement and never a delivery
guarantee, and no document may promise it.** macOS notifications can be denied
in System Settings, suppressed by Focus, and are unavailable entirely to a
process with no bundle identifier — a bare `./relay` in a terminal, or the test
binary. There is no way to force one. The tray's `Pending enrolment requests:
N` line is the reliable surface and stays for that reason; it must not be
removed as redundant. The structural mechanism is also what keeps P1's proof
intact: the banner is raised by the tray's existing two-second poll *reading* a
counter, never by the lodge path *calling* anything.

The distinction that makes this acceptable: a notification is dismissible and
blocks nothing, while `presence.Gate` takes the screen and asks for a password.
Spamming the first is an annoyance; spamming the second was the attack ADR-018
§8 was written to make impossible, and it remains impossible.

### 5. The registration is named on the client and owned by the host

The name the operator types is a **label**: client-asserted, displayed to the
human at approval, and never an authentication factor. Two machines may both
claim "Hermes Mail"; the certificate distinguishes them and the human
disambiguates at approval. The host owns its own `client_id` namespace and
resolves collisions, so the id in `relay enrol list` and in every audit record
is the one the host assigned — which the client is told and records.

This is the same reasoning ADR-010 §2 applied to the enrolment being keyed by
certificate rather than by machine, one step out: the human-readable string is
for humans, and the key is the identity.

### 6. A client holds many registrations, and switching is two separate axes

**A machine is not limited to one registration.** Run `register` again with a
different name and you get a second keypair, a second certificate, and a second
row on the host — which is the shape ADR-010 §2 already calls expected: several
agents on one VM, each with its own certificate and its own grants, audited and
revoked independently.

There are two things an operator might mean by "switch profiles," they resolve
at different layers, and conflating them is what made the old `--bundle`-plus-
`--project` pairing confusing:

| | Selector | What actually changes |
|---|---|---|
| **Between registrations** | `--as NAME` | a different certificate is presented, so relay resolves a different enrolment: different grants, different budget, separately revocable |
| **Within one registration** | `--project ID` | the same certificate, choosing among the grants that one enrolment already holds |

The second only exists when an enrolment holds more than one grant, and relay
honours the id only if the enrolment actually holds it (ADR-010 §2). The first
is the real identity switch.

    $XDG_CONFIG_HOME/relayremote/          0700   (falls back to ~/.config)
      config.json                          0644   { "version": 1, "default": "hermes-mail" }
      registrations/                       0700
        hermes-mail/                       0700
          client.key                       0600
          client.csr                       0644
          client.crt                       0644
          ca.crt                           0644
          registration.json                0644
          pending.json                     0600   (only while a registration is in flight)
        hermes-cal/
          …

`registration.json` records the name, the host-assigned client id, the tool and
enrolment addresses, the CA fingerprint, the certificate fingerprint, the
grants observed at registration, a default project, and the code that was
compared. **A registration directory is deliberately shaped exactly like a
bundle directory**, so `--bundle` at that path also works and the existing
loader is reused unmodified rather than reimplemented.

    relayremote register "Hermes Mail" --host 192.168.64.1
    relayremote register "Hermes Cal"  --host 192.168.64.1

    relayremote registrations
      NAME         CLIENT ID    HOST            PROFILES                              DEFAULT
      hermes-cal   hermes-cal   127.0.0.1:9910  b0000000-0000-4000-8000-000000000001
    * hermes-mail  hermes-mail  127.0.0.1:9910  477d9a17-da03-45eb-a433-764f93fe96fc  yes

    relayremote --as hermes-cal call --tool fs_list
    relayremote use hermes-cal          # move the default pointer

(**Corrected against the build.** An earlier draft of this table showed
`PROFILES` as friendly names — `mail`, `calendar, contacts`. It shows the
profile **ids**, because the ids are what the client was told and what
`--project` takes; the client never learns relay's display names. A directory
holding a key but no certificate renders `(registration in progress)`, which is
a state the operator did not choose to create and must not read as a broken
row.)

Selection resolves in one order, most explicit first, and it is nine rows
rather than the four this decision first wrote — because `--bundle` and
`RELAY_REMOTE_BUNDLE` had to keep working untouched and they sit inside the
same order:

| # | condition | result |
|---|---|---|
| 0 | `--bundle` and `--as` both passed | **error** |
| 1 | `--bundle DIR` passed | bundle mode |
| 2 | `--as NAME` passed | that registration |
| 3 | `RELAY_REMOTE_REGISTRATION` set | that registration |
| 4 | `RELAY_REMOTE_BUNDLE` set | bundle mode |
| 5 | `config.json`'s `default` names one that exists | that one |
| 6 | exactly one registration exists | that one |
| 7 | several, no selector | **error**, listing every name |
| 8 | none, and no bundle | today's error, plus a line naming `register` |

Row 3 above row 4 is deliberate: `RELAY_REMOTE_REGISTRATION` is the more
specific variable and a user who sets it means it. Rows 1 and 2 above both
environment variables is the ordinary flag-beats-environment rule.

Row 7 is the one this store must never get wrong. It **errors and lists the
names**; it does not pick. Guessing which identity to act as is the one thing
this store must never do, because the identities differ precisely in what they
may reach — and a wrong guess is silent, since relay answers the wrong identity
perfectly well.

**Which axis to reach for.** One registration holding several grants is right
when one agent legitimately needs both surfaces — ADR-010's `hermes-triage
[proj_mail, proj_calendar]`. Separate registrations are right when the callers
should be independently revocable, independently budgeted, and separately
attributed in the audit log. The default answer for two different agents is two
registrations, because the enrolment is the unit of compromise and therefore the
unit that carries the budget (ADR-010 §7).

**What `--as` is not.** ADR-010 §2's limit applies here verbatim and is not
softened by giving the registrations names: agents running as the same user on
one machine can read each other's private keys, so `--as` buys audit separation
and revocation granularity, not isolation. It becomes a real boundary only when
the keys are, via different users, containers, or key permissions. Relay cannot
enforce that from the host and this store does not claim to.

`--bundle` keeps working, bypassing the store entirely. It is the path hermes's
current wiring uses, and the operator-carried enrolment still produces a bundle
directory with no registration record; both must keep working untouched.

`relayremote registrations`, `use NAME`, and `unregister NAME` manage the store.
**`unregister` is local only** and says so: it deletes the key and certificate on
this machine and prints that the host-side enrolment still exists and still
counts, and that `relay enrol revoke` on the Mac is what actually cuts access. A
client-side delete that read as a revocation would be the worst possible lie in
this system.

### 7. Approval assigns grants, and registration reports what it got

A newly signed enrolment holding no grants connects successfully and lists
zero tools, which reads as a broken install rather than an incomplete one. So:

- the approval sheet makes choosing an access profile a required step, with
  "none for now" available but never the silent default;
- **`relay enrol approve` takes the same rule**, which this decision did not
  originally say and the build made explicit: with no `--grant` it refuses,
  naming `--no-grant` as the way to say "no access, on purpose". It is a
  behaviour change to an existing command; the failure is loud and the fix is
  one flag. A door that could issue an empty grant silently would put the
  whole of this section behind whichever door the operator happened to use;
- `register` ends by reporting what the grant actually reaches, not "done".

`register` may carry a requested-profile hint, which is **displayed to the
human as a request and never honoured automatically**. Nothing about a lodged
request names its own grants; that choice stays the operator's, exactly as
`docs/access-profiles.md` already states.

**The address in that report comes from `--host`, never from the wire.** Relay
reports its own `remote.listen`, which is very often `127.0.0.1:9910` or
`0.0.0.0:9910` and meaningless to a remote machine; and following a host
supplied over an unauthenticated channel would be a redirection primitive even
after the comparison has closed, because it decides where the *next* connection
goes. Only the port is ever taken from the wire, and the port resolves
most-explicit-first: a `--port` the operator actually typed, then the port of
the reported `relay_addr`, then 9910. A `relay_addr` naming some other host is
printed as a note and not followed.

**A registration that is on disk but whose tool plane cannot be reached is a
success, not a failure**, and says so: the certificate is real and filed, and
the client exits with a code of its own (12) naming the two likely fixes —
widen `remote.listen` past loopback, or pass `--server-name`, since relay's
server certificate carries only the names in `remote.listen`. This is the
single most common way a first registration half-works, and a script needs to
tell it apart from a registration that did not happen.

## Consequences

- **Registering a machine is one command, one comparison, one approval.** The
  hand-carried fingerprint, the separate key-hash comparison, and the
  `relay enrol requests` polling step all disappear from the normal path.
- **Two written properties are narrowed, and both narrowings are listed in
  ADR-018's own terms.** §8's mandatory hand-carried pin becomes a mandatory
  short-code comparison (§3); §8's "no notification, ever" becomes "no
  *prompt*, ever" (§4). The prompt-spam property that P1 existed to buy is
  untouched.
- **A network peer can raise a banner on the operator's Mac** where the
  enrolment listener is enabled. Coalesced, rate-limited to six an hour, and
  bounded by the pending table, but real. The reverse is also true and matters
  more for the documents: **the operator may see no banner at all** — denied,
  suppressed by Focus, or unavailable to a process with no bundle identifier —
  so the tray's `Pending enrolment requests: N` line, not the banner, is the
  surface anything may rely on.
- **The operator-carried path is unchanged and remains the fallback that
  always works.** It needs no listener, no network path, and nothing to
  compare. Everything here is about making the network path pleasant, not
  about making it the only one.
- **The client gains persistent state it did not have.** A registration store
  is a thing that can be stale, half-written, or out of sync with the host —
  `registration.json` is a cache of the host's answer, not a source of truth,
  and any grant list in it is advisory. Relay re-checks at call time (ADR-010
  §3, point 3), which is what makes a stale local record fail closed.

  Two rules follow, and both are enforced rather than intended. **Nothing read
  from `registration.json` may relax a check**: TLS verifies against `ca.crt`
  on disk exactly as `--bundle` mode does, and the `ca_fingerprint` field is
  display only. And **a corrupt cache is still a usable registration** — a
  truncated or unparseable `registration.json` beside a valid key, certificate
  and CA warns and keeps working, because refusing to run on a damaged cache
  turns a cosmetic problem into an outage on the machine that is hardest to
  reach. What is lost with the file is the *routing*, not the identity: the
  address and the default project lived there, so that run needs `--addr`, and
  `--project` if the enrolment holds several grants.
- **`--tofu` is retired from the interactive path and kept where it still
  means something.** It does not exist on `register` and is refused at flag
  parse there, naming the comparison code as what replaced it; it stays on
  `relayremote request` unchanged, because on that verb it is still the only
  alternative to a hand-carried fingerprint. The comparison is what `--tofu`
  was approximating, done better — but only `register` has one to offer.

## See also

- [019-implementation-spec.md](019-implementation-spec.md) — the build, the
  wire changes field by field, the acceptance criteria, and §9's list of what
  this decision got wrong.
- [ADR-010](010-remote-client-transport-and-identity.md) — the certificate-as-
  identity model §1 and §2 restate, and the enrolment-keyed-by-certificate
  reasoning §5 extends.
- [ADR-018](018-configuration-is-a-capability-of-an-identity.md) §8 — the
  enrolment-request channel this narrows in two places, and the P1/P2
  properties §3 and §4 are argued against.
- [`docs/access-profiles.md`](../access-profiles.md) — the operator walkthrough
  this replaces on the normal path, and the man-in-the-middle warning §3's
  short code is designed to answer.
