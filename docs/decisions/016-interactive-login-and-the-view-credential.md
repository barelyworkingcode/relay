# ADR-016: A Login Is a Ceremony Anchored on the Host, and the View Holds a Credential of Its Own

**Status:** Proposed
**Date:** 2026-08-28

## Context

ADR-015 classed the control plane and replaced the single bearer with
credentials that name their classes. `relay credential mint` issues one, which
is the right shape for a script or an MCP client: a long-lived secret, handed
to a process, presented on every call. Two things it decided are still
missing, and they are the same gap seen from two ends.

**Decision 4 is not implemented.** It says the settings view is the client
whose credential is most exposed — rendered into a page, resident in a browser
relay does not control — and must therefore hold the smallest class set that
lets it work. Nothing issues the view a credential. It authenticates with
`RELAY_FRONTEND_TOKEN`, which `migrateFrontendTokenToCredential` records as
`legacy-frontend-token` holding `read`+`configure`. That class set is what the
legacy token happened to reach, not a measurement of what a view needs, and it
is shared with Eve and relayScheduler. The client ADR-015 singles out as the
most exposed is the one holding the widest credential relay issues itself.

**There is no interactive login at all.** Every credential in `docs/tokens.md`
is minted for a *process* and injected into it. A human reaching
`RELAY_API_LISTEN` in a browser has no way to say who they are. The only way
to get a token into a browser today is to paste one, which turns a
non-recoverable secret printed once by a CLI into a value living in a page —
the exact exposure decision 4 exists to prevent.

Three properties of the surrounding system shape what may be built here.

- **The loopback bind is reachable by any page the owner visits.** A browser
  will send a cross-site request to `http://localhost:PORT` from any origin.
  This is not a hypothetical: it is the ordinary behaviour of the web
  platform, and it is what makes a browser-facing control plane categorically
  different from a Unix socket. Nothing in ADR-014 or ADR-015 had to reason
  about it, because neither added a surface a *page* could reach.

- **The settings view is still an IPC client.** `web/src/app.js` talks to Go
  through `window.webkit.messageHandlers.ipc`; `ipcHandlers` still holds all
  27 commands. ADR-014's migration order puts the view last, so the view that
  needs a credential does not exist yet. That is not a reason to defer: the
  credential's shape decides what the view may be built to do, and building it
  first and narrowing after is how the legacy token got its class set.

- **The proxied surface is unresolved and is now load-bearing.** ADR-015
  decision 6 classed the `/` catch-all `configure` and deferred the real
  question as issue #50. The view's credential is the forcing function: if the
  view holds `configure` and `configure` reaches every route an enhanced
  service registers — relayLLM's sessions, terminals and `/ws` included — then
  the view's class set is not small, and decision 4 cannot be satisfied by
  choosing one.

## Decision

### 1. The login is a passkey ceremony; a password is refused

Relay authenticates its owner with WebAuthn, ES256, user verification
required. There is no password, no password file, and no set-a-password
flow.

The argument is not that passkeys are modern. It is that a password would put
a **new kind of secret** in `settings.json`. Every other value in that file is
either a hash of something relay issued, or a token whose compromise is bounded
by relay — a stolen `settings.json` today yields project tokens, which is bad
and known, and yields no remote access at all, which is ADR-010 decision 2's
result. A password verifier is different in kind: it is offline-guessable, it
is chosen by a human, and humans reuse it, so its compromise reaches systems
relay has never heard of. Adding one would make `settings.json` worth more to
an attacker than the machine it sits on.

A password also drags a subsystem behind it that nothing else in relay has:
attempt counters, lockout state that must survive a restart without becoming a
denial-of-service lever, a strength policy, a reset path, and a
set-on-first-run flow that needs its own anchor anyway — so choosing a password
does not avoid decision 2, it duplicates it.

And it is phishable. A page on any origin can render a convincing relay login
and collect a password. It cannot collect an assertion: WebAuthn binds the
credential to an origin, and the browser, not relay, enforces that binding.
Given that the *only* new attack surface this ADR opens is "a page in the
owner's browser", refusing the phishable option is the whole point.

What it costs, stated plainly because it is not small:

- **A passkey needs an authenticator, and this machine may not have one.**
  Relay's development box is an Apple Virtual Machine with no Touch ID and no
  Secure Enclave. With user verification required (decision 7), a passkey here
  means a security key with a PIN, or a synced credential from another device
  — not a fingerprint. A password would have worked on any keyboard. This is a
  real regression in reachability and it is accepted, because the credential's
  job is to authenticate *the owner* and not *whoever is at the browser*.
- **Losing the authenticator loses the login.** There is deliberately no
  second factor, no backup code, and no recovery question — each of those is a
  phishable shared secret wearing a different hat. The recovery path is the
  host: `relay login enrol` registers another passkey, and
  `relay credential mint` bypasses the browser entirely. Anyone who can run
  those already owns the config dir, so recovery grants nothing that was not
  already granted.
- **The relying-party identity is pinned to `localhost`.** An RP ID must be a
  domain, so `http://127.0.0.1:PORT` cannot carry this ceremony and
  `http://localhost:PORT` can. The bind address stays what it is; the URL the
  owner types is part of the design. Decision 9 says what that means for a
  non-loopback bind.

### 2. Registering a passkey is a host-side act anchored by a short-lived code

ADR-010 decision 8 refused self-service enrolment and made it an operator act
on the host, because "an enrolment endpoint reachable by presenting a secret
would reintroduce precisely the replayable credential decision 2 removed, and
would do it at the one point in the system where the result is a *new identity*
rather than a single call." Registering a passkey is that same point. The same
rule applies.

The anchor is a **code minted by a CLI subcommand running as the owning user**:

    $ relay login enrol
    login code: 7K2P-QX4M   (valid for 2 minutes, single use)
    open http://localhost:8790/relay/login and register a passkey

The code's SHA-256 and its expiry are written to `settings.json` — never the
code itself, matching every other credential in that file — and the record is
deleted the moment a registration consumes it. The tray reads it through
`freshSettings`, which is what makes a code minted by a CLI process usable on
the API's very next request with no restart and no poll interval (issue #21).
The code is required for **registration only**; it is never accepted in place
of an assertion, so it can never become a password.

What the anchor is and is not:

- **It binds the owning user, not a process.** A process running as the owner
  can read `settings.json`, and therefore every project token in it, and can
  write a bootstrap record of its own. Relay has never defended the config dir
  against the user who owns it — `docs/tokens.md` makes exactly this argument
  for directory auth — and does not start here.
- **What it does defend against is the new surface.** A page in the owner's
  browser, or anything that reaches the loopback port through a tunnel, cannot
  read the config dir and cannot produce the code. That is the whole
  difference between "the owner registered a passkey" and "the first thing to
  reach the port did".

Three alternatives, considered and answered:

- **No anchor (trust on first use).** Refused. The window is not the harmless
  few seconds it sounds like. Any page the owner visits can POST to the
  loopback bind, so the race is not between the owner and an attacker who has
  to get there first — it is between the owner and every tab. The window also
  *reopens*: `docs/tokens.md` documents deleting `settings.json` as how an
  operator locks the control plane out, and under TOFU that act would silently
  re-arm self-registration. A boundary that a documented recovery procedure
  disarms is not a boundary.
- **A code in the tray menu.** Accepted as a second *presentation*, refused as
  the anchor. Showing the same code under a "Show login code" item is the same
  act by the same user and makes the flow usable without a terminal, so it
  should exist. It cannot be the only source: the tray is `LSUIElement`, its
  menu is unreachable over SSH and in the hermetic tier, and ADR-014's whole
  direction is that a capability reachable only from a mouse is a capability
  half-built.
- **A file in the config dir.** Refused. Relative to printing the code it buys
  nothing — the reader in both cases is the owner's eyes — and it costs
  persistence. A printed code lives in a terminal; a file lives until
  something deletes it, which means relay must build the expiry machinery
  anyway *and* a file to go with it. Storing the hash in `settings.json`, which
  relay already re-reads on every authorization decision, is the same machinery
  with one fewer artifact.

### 3. A login produces a short-lived credential, one per login, held in memory

The ceremony's output is an `APICredential`, so there stays exactly one thing
the API authenticates and one place an operator revokes. Four properties.

**Its class set is `read` + `configure`, and nothing else — ever.** Not
`grant`: a view that can rotate a project token or issue an enrolment is a
view whose compromise issues credentials, and ADR-015 already moved
`POST /api/projects/{id}/rotate_token` out of the legacy token's reach for that
reason. Not `execute`: decision 2 of ADR-015 makes that unroutable on TCP
regardless, and stating it on the credential as well means the refusal survives
someone later deciding the view should be served over the socket. Not `proxy`
(decision 4 below), which is what makes `configure` mean what it says.

That set is a **ceiling fixed by this ADR and a floor to be narrowed by
measurement**, not a guess to be widened by one. `ControlDecision` already
records the method, path, class, transport and credential id of every
authorization decision, and `relay audit --event control_decision --json`
reads them back. So the narrowing is the method ADR-010 decision 7 used for
budgets and ADR-015 decision 3 instructed for credentials: run the view
against a credential holding the ceiling, take the distinct
(method, path, class) tuples it actually reached, and mint the login
credential with the union of the classes in that set. If the view turns out to
need `configure` for three routes, the right end state is three routes, not a
class — but a per-route grant is a second permission vocabulary and ADR-015
decision 3 refused to invent one. So the class set stays the unit, and the
measurement's job is to catch the case where the answer is `read` alone.

**It expires, and expiry is a new field.** `APICredential` carries `Created`
and no expiry, and `enrolment_ca.go` chose revocation over expiry deliberately:
"a short lifetime would need an authenticated renewal path, and any credential
that can be replayed to obtain a fresh certificate reintroduces a bearer secret
at the one point where the result is a new identity." That argument does not
transfer. A login credential's renewal path is *another ceremony*, which is an
unforgeable user-presence act rather than a replayable secret — so expiry is
affordable here precisely where it was not there. `APICredential` gains
`Expires string` (RFC3339, `omitempty`). **Absent means no expiry**, so every
record written before this field existed round-trips unchanged, the same
zero-value discipline `Project.Kind` follows and for the same reason.
`AuthenticateAPICredential` refuses an expired record and refuses it
*identically* to an unknown one — a distinguishable answer is an oracle for
which credentials exist. Expired records are reaped lazily, on the next mint
and on the next successful login, never by a timer: a background goroutine
rewriting `settings.json` on a schedule is a writer nothing asked for, against
a file `docs/tokens.md` already documents as having more writers than it wants.

Twelve hours. That number is a first value and will be wrong, in the register
ADR-010 decision 7 used for budgets; what matters is that expiry exists and
that the audit log can show whether it is being hit.

**The browser holds it in memory and nowhere else.** Not `localStorage`, which
is readable by every script the page ever loads and survives a restart — that
is the exposure decision 4 names, written down as a design. Not a cookie: a
cookie is ambient authority, sent by the browser on any request any page
causes, which would hand a CSRF surface to the one credential a page can
reach. Relay refuses ambient authority consistently — `PeerPID` is refused as
an authorization input, the remote wire has no field to authenticate with, and
directory auth is documented as a convenience rather than a boundary — and a
cookie is the same mistake in a browser. The token is returned in the response
body, held in a closure, and sent as `Authorization: Bearer`. A reload runs
the ceremony again.

That cost is real and is the reason to state it: **on a machine with no
platform authenticator, every page reload is a physical act.** It is accepted
because the alternative is persistence, and a persisted credential in a browser
is the thing ADR-015 decision 4 was written about.

**One credential per login.** Each ceremony mints its own record, named for
the browser session that produced it. This is what makes "sign this browser
out" a real operation, and what makes `ControlDecision.CredID` attribute a
session rather than a role. Reuse would mean a credential that outlives the
ceremony justifying it, which is the property being removed. The cost is that
`api_credentials` accumulates — hence lazy reaping, and hence
`relay credential list` must show `EXPIRES` and default to hiding expired
records.

### 4. The proxied surface gets a class of its own, and the view does not hold it

Issue #50 asks whether a proxied route that starts a terminal is `execute`.
Answering it is no longer deferrable, because ADR-015 decision 4 and decision 6
cannot both stand as written: a view holding `configure` reaches a terminal.

Under decision 1's own test the answer is yes — the caller supplies what runs.
But classing the catch-all `execute` is not available: `execute` is socket-only,
Eve and relayScheduler reach the proxied surface *over the socket* with the
legacy credential, and that credential must never hold `execute` — the
migration says so in as many words, because `execute` would also hand it
`POST /api/mcps`. Classing the mount `execute` would either break every
enhanced service or force the migration to grant the one class ADR-015
withholds from it.

So: **a fifth class, `proxy`.** It is not a dodge, and it survives decision 1's
test — a class is a property of what a capability can cause, and what this
mount can cause is *whatever a manifest declares*, which relay cannot see and
therefore cannot classify. That is a distinct blast radius from the other four
precisely because it is the only one relay does not define. Naming it lets
relay say the true thing: not "this is configuration", and not "this is
execution", but "this reaches a surface relay has not classified".

Four consequences follow directly:

- `ClassReachableOn(ClassProxy, TransportTCP)` is **false**. The mount is
  socket-only, for `execute`'s reason: it can start a terminal, and a browser
  door must not reach one. This is the fix issue #50 asks for, and it closes
  the near-miss absorption named in `registerFrontendRoutes` — with no
  catch-all on TCP, `POST /api/services` there is a 405 from `http.ServeMux`
  rather than a proxied request.
- `migrateFrontendTokenToCredential` grants `read` + `configure` + `proxy`, so
  Eve and relayScheduler are unaffected, which is what ADR-015 decision 6
  wanted from `configure` and got only by conflating two things.
- `configure` stops silently meaning "and also every route relayLLM
  registers". A minted `configure` credential becomes strictly less able than
  it is today, which is the direction ADR-015 moves in.
- The login credential does not hold `proxy`, so the browser view cannot reach
  a session, a terminal or `/ws`. A future "open a terminal from Settings" is
  refused by construction rather than by a check someone can invert.

This is still a **floor, not a verdict.** The right answer remains classing
what the mount reaches, which needs the manifest to describe per-route blast
radius — a protocol change across repositories, and still deliberately not
made here. What changes is that the floor is now honest: the mount is named as
unclassified rather than mislabelled as configuration, and issue #50's
guarantee (a browser-based view can never make relay execute an arbitrary
command) holds again. Issue #50's second half stays open:
`EnhancedServiceRegistry.checkRouteConflictsLocked` checks services against
each other and never against relay's own patterns, so a service can still
claim a path relay serves. Decision 5 depends on relay owning `/relay/`, which
makes that gap worth closing, and it is not closed here.

### 5. The socket does not change, and 0600 is not spent twice

There is a tempting simplification: the frontend socket is 0600, so the peer is
the owner, so authentication on it is ceremony. It is refused.

**Relay already distinguishes among processes running as the same user, and
that distinction is live.** `service register --no-frontend-creds` exists so a
backend relay spawned itself never receives `RELAY_FRONTEND_SOCKET` /
`RELAY_FRONTEND_TOKEN`, keeping the front-door bearer out of an env that could
leak it into a spawned shell. It is a deliberate, per-service opt-out — and
fragile enough that a service *edit* silently revoked it until
`ServiceOps.Update` was fixed this week to carry `FrontendConsumer` forward.
Trusting the socket blanket-wise would erase that opt-out entirely: every
backend deliberately holding no front-door credential would gain the whole
control plane by connecting to a socket it can already open.

**The 0600 property is already spent.** It is the entire justification for
routing `execute` onto the socket at all (ADR-015 decision 2): the kernel
refuses a connection from another user, checked before relay sees a byte.
Spending it a second time — as a reason to stop authenticating — would leave
`execute` reachable by anything that can open the socket, which is the flat
interior ADR-015 exists to end.

**A second authentication path is the bug ADR-015 already had to fix.**
`frontendCredentialAuth` says it is "the ONLY bearer check in front of either
mux, deliberately", because an outer gate admitting one fixed value made every
other credential unreachable and the class model dead code. A socket exemption
is that shape again.

So interactive login **adds a door and removes none**. `frontendCredentialAuth`
and `RouteRegistrar` are unchanged in what they demand.

The login routes are the one exception, and they are shaped so the exception
cannot spread. They are not registered through `RouteRegistrar` — there is no
class that means "none" and inventing one would put a hole in the vocabulary
— but into a **separate, exact-match table consulted before**
`frontendCredentialAuth`, holding exactly three patterns:

    GET  /relay/login             the login document
    POST /relay/login/challenge   issue a challenge
    POST /relay/login/verify      verify a registration or an assertion

Three entries, in one place, visibly a security boundary — the shape ADR-010
decision 1 chose for `remoteHandlers` and ADR-015 decision 2 imitated, for the
same reason: a fourth unauthenticated route has to be added to a list someone
reviews, not discovered to already work. `/relay/` is reserved to relay and the
dispatcher may not claim it.

### 6. No WebAuthn library; the refusals are what keep the surface small

`github.com/go-webauthn/webauthn` is the standard choice and it is refused.
Relay verifies assertions directly against `crypto/ecdsa`, `crypto/sha256` and
a strict CBOR reader of its own.

`go.mod` has five direct dependencies, three of them build-time (esbuild,
goja, jsonc). The discipline is deliberate and this is exactly the case it
exists for: a relying party with **one user, one origin, one RP ID, one
algorithm and no attestation policy** needs a small fraction of what a general
library implements, and the library's tree is mostly the parts relay refuses.

What relay actually needs is bounded. Registration: read the attestation
object, require `fmt == "none"`, take `authData`, and extract a COSE key that
must be `kty: 2 (EC2), alg: -7 (ES256), crv: 1 (P-256)` — two 32-byte
coordinates. Assertion: verify an ASN.1 ECDSA signature over
`authData || SHA256(clientDataJSON)` with `ecdsa.VerifyASN1`. The CBOR
required is a strict subset — definite-length maps, byte strings, text strings,
small integers — and the decoder is written to **refuse more than it accepts**:
no indefinite lengths, no tags, no floats, no unrecognised keys, a depth cap
and a size cap, and an error on trailing bytes. That is the same discipline
`DisallowUnknownFields` gives the remote wire (ADR-010 decision 4), applied to
a format a caller supplies.

To keep it that small, relay refuses, permanently and by name:

- **Every attestation format but `none`.** `packed`, `tpm`, `android-key`,
  `android-safetynet`, `apple`, `fido-u2f` are refused at registration.
  Attestation answers "what kind of authenticator is this", and relay has no
  policy that consults the answer. Refusing it also means relay never parses an
  X.509 chain or a TPM structure handed to it by an unauthenticated caller —
  which is where the interesting parsing bugs in this space live.
- **Every algorithm but ES256.** `pubKeyCredParams` advertises `-7` alone.
  RS256 would add RSA key parsing and a second signature path for
  authenticators that overwhelmingly also do ES256; EdDSA the same.
- **Resident keys and discoverable-credential flows.** `residentKey:
  "discouraged"`, and every assertion supplies `allowCredentials`. The cost is
  stated rather than hidden: `POST /relay/login/challenge` must return the
  registered credential IDs to an unauthenticated caller, so that endpoint
  discloses *that* passkeys are registered and how many. The login page
  discloses the first fact anyway; the IDs are opaque and are not verifiers.
- **More than one origin, and any origin from configuration.** The expected
  origin is derived from the listener relay actually bound, at bind time. There
  is no origin list, no wildcard, and no setting — a typo in a config file must
  not be the thing that accepts an assertion from another site.
- **Extensions.** Relay requests none, and refuses `authData` carrying
  extension data.

The cost is named plainly: **relay owns cryptographic verification code, and a
bug in it is a login bypass rather than a crash.** That is why decision 7
enumerates the policy instead of describing it, and why decision 8 makes the
negative cases the bulk of the test suite. The trade flips the moment relay
needs a second algorithm or a real attestation format; at that point the
library is the answer and this decision should be revisited rather than
patched.

### 7. What is verified on every assertion

Local WebAuthn implementations fail by checking eight of these and shipping.
All of them are required, each is a distinct refusal, and each has a negative
test (decision 8).

1. **Challenge.** 32 bytes from `crypto/rand`. Held in memory in the tray
   process — never in `settings.json`, because an in-flight ceremony must not
   survive a restart. Bound to its ceremony type, expiring in 60 seconds, and
   **single use**: consumed by a lookup-and-delete under one lock before
   verification begins, so a replay races nothing. Not found, expired and
   already used are refused identically. The table is bounded and evicts
   expired entries first, so an unauthenticated caller cannot grow it without
   limit.
2. **`clientDataJSON.type`.** Exactly `webauthn.create` for registration,
   `webauthn.get` for assertion. This is what stops a registration ceremony's
   signature being presented as a login.
3. **Origin.** Exact string equality against the origin derived from the bound
   listener. Not a prefix, not a suffix, not scheme-insensitive.
4. **`crossOrigin`.** Absent or `false`. An assertion produced in an iframe on
   another origin is refused.
5. **RP ID hash.** `authData[0:32]` equals `SHA256("localhost")`, compared with
   `crypto/subtle` — not because it is secret, but because there is no reason
   for any comparison on this path to be variable-time.
6. **UP flag** (bit 0). Required, always. An assertion with no user presence is
   not a login.
7. **UV flag** (bit 2). **Required, and enforced server-side.** Relay requests
   `userVerification: "required"` *and* checks the bit — asking without
   checking is the classic failure, since the flag is set by the
   authenticator and the request parameter is only a hint. The cost is decision
   1's: an authenticator with no PIN and no biometric cannot log in, which on
   the current development VM means no built-in authenticator qualifies.
8. **Signature.** ES256 over `authData || SHA256(clientDataJSON)`, verified
   with `ecdsa.VerifyASN1` against the stored coordinates. `VerifyASN1` is used
   rather than a hand-parsed `(r,s)` because it rejects non-canonical and
   trailing-byte encodings for free.
9. **Credential-to-user binding.** The assertion's `rawId` must resolve to a
   stored credential for this relying party. If `userHandle` is present it must
   equal the owner's stored handle; if absent, the credential ID lookup is the
   binding. A credential ID that resolves to nothing is refused identically to
   a bad signature.
10. **Signature counter.** The stored counter is compared to the received one.
    If **both are zero**, this authenticator does not implement counters —
    which is the normal case for a synced passkey — and that fact is recorded
    on the credential at registration, so the policy is fixed per credential
    rather than re-derived per assertion. Otherwise a counter that **increases**
    is accepted and stored, and a counter that is **equal or lower** is a
    cloned-authenticator signal: **relay refuses the assertion and writes an
    audit record naming the credential.** It does not disable the credential.
    Refusing is right because this is a login and the cost of a wrong refusal
    is one retry, against an attacker holding a clone. Not auto-disabling is
    right because a regression also occurs when a legitimate provider
    replicates a credential, and auto-disabling would let one replayed stale
    assertion lock the owner out of their own machine.
11. **`authData` shape.** Exactly 37 bytes on an assertion — 32 rpIdHash, 1
    flags, 4 counter — with the AT flag clear and no trailing bytes. On a
    registration, exactly the length its flags imply. Trailing bytes are a
    refusal, not something to ignore.
12. **Ceremony rate.** Failed verifications are counted and delayed, and the
    number of registered passkeys is capped (five), each named, listed in
    Settings and individually revocable. Registering a credential ID that
    already exists is refused rather than treated as an update.

### 8. The verifier is tested exhaustively without a browser, and the ceremony is tested once with a real one

Two tiers, deliberately separate, because they answer different questions.

**The hermetic tier owns the verifier.** A **software WebAuthn client in Go**,
in the default tier with no spawned binary and no user files, generates an
ES256 key with `ecdsa.GenerateKey(elliptic.P256(), rand.Reader)` and assembles
exactly what a browser would POST: `clientDataJSON`, `authData`, the `none`
attestation object, and the signature. It reads the expected origin and RP ID
from the server under test rather than from a constant, so no production seam
is required — the seam is the client, which is test code, and ADR-002's
criteria for a production seam are not met and not stretched.

Every one of decision 7's twelve checks gets a negative case that flips
**exactly one** thing — wrong origin, wrong `type`, replayed challenge, expired
challenge, UP clear, UV clear, wrong RP ID hash, signature over the wrong
bytes, unknown credential ID, a credential bound to a different user handle,
non-increasing counter, trailing `authData` bytes, `fmt: "packed"`,
`alg: -257`, indefinite-length CBOR, a map key the decoder does not know — and
asserts the specific refusal. This is where the negative cases belong and the
only place they scale: they need no browser, they run on every commit under
`go test ./...`, and the exhaustive suite is what decision 6's cost buys back.
It is the deliverable, not a supplement to it.

**What that tier structurally cannot answer is whether relay agrees with a
real user agent**, because the software client and the verifier are written by
the same person from the same reading of the specification. A misreading is
present on both sides and cancels: every test passes and every real browser
fails. The divergences that actually bite live in exactly the places a
hand-rolled client is most likely to mirror rather than test — how a real
`clientDataJSON` is serialised (key order, whitespace, unpadded base64url, and
whether `crossOrigin` is present at all), how a real CTAP2 stack encodes the
attestation object and the COSE key inside it, the AAGUID and flag bits a real
platform authenticator sets, and the DER shape of a signature relay did not
produce.

**So a second tier runs the ceremony in a real browser.** Google Chrome
152 is installed on this host, headless with `--remote-debugging-port` works,
and the DevTools Protocol `WebAuthn` domain is available:
`WebAuthn.addVirtualAuthenticator` with
`{protocol: "ctap2", transport: "internal", hasResidentKey: true,
hasUserVerification: true, isUserVerified: true,
automaticPresenceSimulation: true}` yields a platform authenticator that
reports user-verified — which is what makes decision 7's UV requirement
testable at all without hardware.

It belongs in the **`-tags=live` tier**, not in a script. `CLAUDE.md` already
defines that tier as the one that spawns real binaries end to end, and Chrome
is a real binary spawned end to end; the alternative — a shell script under
`scripts/` — would put the only evidence that relay accepts a real assertion
outside the test tiers, outside `go test`, and outside anything a pre-push hook
or a future CI could gate on. A Go test also gets to assert against relay's own
verifier in-process rather than by parsing output, which is the whole point.

It **skips gracefully when Chrome is absent**, exactly as the existing live
tests `t.Skip` when `../relayLLM` is not built: stat
`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`, and skip with a
message naming it if it is not there. A developer without Chrome must see a
skip, never a failure, and never a silently-passing test.

The mechanics are ordinary and need no new dependency —
`github.com/gorilla/websocket` is already direct:

1. Bind relay's loopback API on an ephemeral port and register a bootstrap
   code, so the test drives the real `/relay/login` document over the real
   listener.
2. Launch Chrome with `--headless=new`, `--remote-debugging-port=0` and a
   `--user-data-dir` under the test's temp dir; read the WebSocket endpoint
   from `DevToolsActivePort` in that directory.
3. Dial the endpoint with `gorilla/websocket`, attach to a page target, then
   `WebAuthn.enable` and `WebAuthn.addVirtualAuthenticator` with the options
   above.
4. `Page.navigate` to `http://localhost:PORT/relay/login`, then
   `Runtime.evaluate` with `awaitPromise: true` to run the page's own
   registration and assertion — the real `navigator.credentials.create` and
   `.get`, not a re-implementation of them.
5. Assert that relay minted a login credential, that it carries the class set
   decision 3 fixes, and that it authenticates on a subsequent request.

That is the assertion the hermetic tier cannot make: a real user agent's
`clientDataJSON`, attestation object and assertion are accepted by relay's
verifier, and the browser's own enforcement — secure context, RP ID against
page origin — agrees with relay's. One passing ceremony is enough; the tier
exists to catch divergence, not to re-run twelve negative cases a browser
would refuse to produce anyway.

**What neither tier covers**, stated so it is not assumed away:

- **A real hardware authenticator.** The virtual authenticator is Chrome's
  software CTAP2 implementation. This host is an Apple Virtual Machine with no
  Touch ID, no Secure Enclave and no USB passthrough, so no genuine
  authenticator — platform or roaming — has ever produced an assertion relay
  has seen. A virtual authenticator that always reports `isUserVerified: true`
  in particular proves that relay *checks* the UV bit, never that a real
  authenticator would have set it.
- **Anything Safari does.** Safari is the only other browser here and is the
  one the owner would actually use. Its consent UI, its passkey storage in
  iCloud Keychain, and its own view of `localhost` as a secure context are
  exercised by nothing. A Chrome-green suite is evidence about WebAuthn, not
  about Safari.

Both gaps belong in `docs/testing-roadmap.md` as named gaps rather than as
silence.

The rule that follows is the important one, and Chrome does not soften it:
**no accommodation is added to the server to make either tier pass.** No
development mode that skips user verification, no configurable extra origin, no
test-only bypass. A weakening introduced for a test is the weakening most
likely to survive — ADR-010 made exactly this argument when it refused a
loopback exemption on the remote path, and it applies unchanged.

### 9. `RELAY_API_LISTEN` becomes a supported local surface and stays an unsupported remote one

ADR-015 §5 said the bind stays a devbox affordance until decision 4 lands, and
that what blocked it was "the view holding a credential narrower than the one
relay hands its own services." This ADR lands that. So the bind changes
meaning: a human at `http://localhost:PORT` can now authenticate as the
machine's owner and be handed a `read`+`configure` credential that reaches no
`grant` route, no `execute` route, and — after decision 4 — no proxied route.
It is a local surface with an authentication story, which it did not have
before.

It is still **opt-in, absent by default, and loopback-only**. `loopbackOnly`
is not relaxed. Three things block anything more, and only the first is new:

1. **The passkey does not travel.** The RP ID and origin are pinned to
   `localhost` at bind time. A non-loopback bind changes both, invalidating
   every registered credential and requiring a real hostname and a certificate
   the browser trusts. The login flow this ADR specifies is *specifically* a
   loopback affordance.
2. **There is still no transport authentication.** ADR-015's Consequences
   already state that classing "does not make the API safe to expose beyond
   loopback," and point at `enrolment_ca.go` — relay's own CA, issuing client
   certificates, with revocation rather than expiry as the control. That
   remains the answer for a non-loopback control plane, and remains
   deliberately undecided. A bearer over cleartext HTTP is acceptable on
   loopback and is a plaintext credential on a wire anywhere else.
3. **The bind is an environment variable read at start** (`trayapp.go`), not a
   configured, reconciled listener like `remote`. `RemoteSupervisor` converges
   on settings changes and refuses to serve when auditing is off; the API
   listener does neither. A surface documented as a deployment needs the
   `remote` block's shape, and it does not have it.

## Consequences

**A login is a physical act, repeated.** With the credential held only in
memory, a page reload runs the ceremony again — a tap on a security key, or a
cross-device prompt on this hardware. That is the price of refusing
`localStorage` and refusing a cookie, and it is the price this ADR chooses.

**On a machine with no verifying authenticator there is no browser login at
all.** The development VM is currently such a machine. The CLI keeps working
and is the recovery path, but "relay has an interactive login" will read as
false on this box until a key is attached.

**Relay owns WebAuthn verification code.** A bug in it is a login bypass. The
mitigation is decision 6's refusals and decision 8's negative suite, and the
honest statement is that a reviewed library would have owned this instead.

**The evidence stops short of hardware.** Decision 8 buys an exhaustive
hermetic suite and one real-browser ceremony against Chrome's virtual
authenticator. No genuine authenticator, and no Safari, has ever produced an
assertion relay has accepted. That is a named gap in
`docs/testing-roadmap.md`, not a rounding error.

**The credential list grows.** One record per login, reaped lazily, means
`api_credentials` is no longer a short human-curated list. `relay credential
list` must show expiry and hide expired records by default, and the Settings
view needs somewhere to sign a browser out.

**`configure` narrows for everyone.** Splitting `proxy` out means an existing
minted `configure` credential loses the proxied surface. Nothing relay injects
is affected — the legacy migration grants `proxy` — but a hand-minted
credential that reached relayLLM through the catch-all must be re-minted.

**The loopback bind stops proxying.** `proxy` is socket-only, so an enhanced
service is no longer reachable at `RELAY_API_LISTEN`. Anyone using the devbox
bind to poke at relayLLM must use the socket.

**This does not fix the socket's flat interior.** Any process running as the
owner can open the frontend socket, and the only thing between it and the
control plane is whether it holds a credential. Decision 5 preserves that
boundary; it does not strengthen it, and `--no-frontend-creds` remains a
per-service opt-out that an edit path has already broken once.

**This does not settle the proxied surface.** Decision 4 names the mount
honestly and keeps it off the browser door. The real answer is per-route
classing in the manifest, which is a protocol change across repositories and
is still deferred, as is issue #50's second half — the dispatcher can still
claim a path relay serves.

**This does not authenticate the transport.** A non-loopback control plane
still needs mTLS or its equivalent, and the machinery for it still sits unused
in `enrolment_ca.go`.

## See also

- [ADR-015](015-control-plane-authorization.md) — the class model this extends
  by one, decision 4 this implements, decision 6's deferred question this
  answers, and §5's statement of what still blocks the bind.
- [ADR-014](014-every-capability-is-an-http-capability.md) — the migration
  that makes the view a client at all; the view moves last, and this ADR
  decides what it holds when it does.
- [ADR-011](011-resource-scope.md) — the fail-closed allowlist shape a
  credential's class set reuses, and the reason a fifth class is a class
  rather than a per-route grant.
- [ADR-010](010-remote-client-transport-and-identity.md) — enrolment as a
  host-side operator act, which decision 2 applies to passkey registration;
  the two-entry dispatch table decision 5's login table imitates; the
  revocation-over-expiry trade decision 3 declines to inherit; and the
  tune-from-evidence method decision 3 reuses.
- [ADR-002](002-test-seams.md) — the criteria decision 8's software
  authenticator deliberately does not need to meet, because the seam is in the
  test.
- `docs/tokens.md` — the credential inventory this adds a row to, and the
  same-user argument decision 2 relies on for the bootstrap anchor.
