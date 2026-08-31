# ADR-019: Registering a Machine Is One Command and One Comparison

**Status:** Proposed. Design agreed with the owner; not yet built.
**Date:** 2026-08-30

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
six-character code neither machine had to be told in advance.**

    SAS = truncate(H(relay CA SPKI ‖ lodged CSR SPKI), 6 characters)

The client prints it while waiting. The host's approval sheet shows it beside
the requested name. The human compares two short strings on two screens.

This is stronger than what it replaces, not weaker, and the reason is what
`docs/access-profiles.md` already warns about at length: comparing the *key*
hash alone does not close a man-in-the-middle, because an attacker can pass
the real CSR through to the real relay — so the hash the operator approves
matches in good faith — and then answer the waiting client with its own CA
certificate. Every check the client runs against what it receives passes,
because the certificate really is valid, just for the wrong CA.

Binding both halves into one value defeats both substitutions:

| Attack | Host shows | Client computes |
|---|---|---|
| attacker lodges its own key | `H(CA_relay ‖ SPKI_attacker)` | `H(CA_attacker ‖ SPKI_client)` |
| attacker forwards the real CSR | `H(CA_relay ‖ SPKI_client)` | `H(CA_attacker ‖ SPKI_client)` |

Either way the two codes differ and the human refuses. Six characters is
sufficient because the attacker gets one guess per approval against a value it
cannot observe — this is Bluetooth numeric comparison's argument, and it does
not become an offline grind: the target lives on the host's screen.

**This narrows ADR-018 §8**, which requires `--ca-fingerprint` or an explicit
`--tofu` with no default. Under this decision the SAS comparison is the
mandatory control and carries the same weight: `register` refuses to complete
without an answer, there is no flag that skips it, and a non-interactive
caller must supply `--ca-fingerprint` exactly as before. What is retired is
the *hand-carried* fingerprint as the only path, not the pin as a control.

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
- notifications are rate-limited independently of the table;
- once the bounded pending table is full, lodging is refused and no further
  notification is raised — the existing availability residual is unchanged and
  gains no new phishing dimension.

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

    ~/.config/relayremote/
      config.json                 { "default": "hermes-mail" }
      registrations/
        hermes-mail/
          client.key   0600
          client.crt
          ca.crt
          registration.json
        hermes-cal/
          …

`registration.json` records the name, the host-assigned client id, the tool and
enrolment addresses, the CA fingerprint, the grants observed at registration,
and a default project.

    relayremote register "Hermes Mail" --host 192.168.64.1
    relayremote register "Hermes Cal"  --host 192.168.64.1

    relayremote registrations
      NAME          CLIENT ID     HOST                 PROFILES              DEFAULT
    * hermes-mail   hermes-mail   192.168.64.1:9910    mail                  yes
      hermes-cal    hermes-cal    192.168.64.1:9910    calendar, contacts

    relayremote --as hermes-cal call --tool calendar_list_events
    relayremote use hermes-cal          # move the default pointer

Selection resolves in one order, most explicit first: `--as NAME`, then
`RELAY_REMOTE_REGISTRATION`, then the `default` pointer, then — if exactly one
registration exists — that one. With several registrations and no selector the
command **errors and lists the names**; it does not pick. Guessing which
identity to act as is the one thing this store must never do, because the
identities differ precisely in what they may reach.

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
- `register` ends by reporting what the grant actually reaches, not "done".

`register` may carry a requested-profile hint, which is **displayed to the
human as a request and never honoured automatically**. Nothing about a lodged
request names its own grants; that choice stays the operator's, exactly as
`docs/access-profiles.md` already states.

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
  enrolment listener is enabled. Coalesced, rate-limited, and bounded by the
  pending table, but real.
- **The operator-carried path is unchanged and remains the fallback that
  always works.** It needs no listener, no network path, and nothing to
  compare. Everything here is about making the network path pleasant, not
  about making it the only one.
- **The client gains persistent state it did not have.** A registration store
  is a thing that can be stale, half-written, or out of sync with the host —
  `registration.json` is a cache of the host's answer, not a source of truth,
  and any grant list in it is advisory. Relay re-checks at call time (ADR-010
  §3, point 3), which is what makes a stale local record fail closed.
- **`--tofu` loses its reason to exist on the interactive path** and should be
  reconsidered rather than carried forward by default: the SAS comparison is
  what it was approximating, done better.

## See also

- [ADR-010](010-remote-client-transport-and-identity.md) — the certificate-as-
  identity model §1 and §2 restate, and the enrolment-keyed-by-certificate
  reasoning §5 extends.
- [ADR-018](018-configuration-is-a-capability-of-an-identity.md) §8 — the
  enrolment-request channel this narrows in two places, and the P1/P2
  properties §3 and §4 are argued against.
- [`docs/access-profiles.md`](../access-profiles.md) — the operator walkthrough
  this replaces on the normal path, and the man-in-the-middle warning §3's
  short code is designed to answer.
