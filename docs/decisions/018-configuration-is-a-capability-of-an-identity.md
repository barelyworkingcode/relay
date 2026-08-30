# ADR-018: Configuration Is a Capability of an Identity, and the Two Clients Are One

**Status:** Proposed. **Partially implemented.** This records a design the
owner and the assistant converged on in discussion, so the reasoning survives
and the open questions are written down while they are known. It **narrows a
decision ADR-017 just shipped** — see "What this reworks" — so it should be
reviewed against that record before any code is written. Step 3 (2026-08-29)
implemented the one slice the owner scoped as safe without the local
identity binding below: retiring `mcp.unregister` and `service.unregister`
from the gated set. `project.grant` — the operation this ADR's `cli-admin`
narrowing was mainly about — stays fully gated; see the Open questions entry
on local identity binding for why. Decision 8, the enrolment-request
channel, is also implemented — see [`docs/access-profiles.md`](../access-profiles.md#approving-a-request-from-the-machine-itself)
for the operator's walkthrough and [`docs/tokens.md`](../tokens.md#the-enrolment-request-channel-is-not-a-credential)
for why the request id it mints is not a credential.
**Date:** 2026-08-29

## Context

Relay has grown rich enough that a non-expert can misconfigure it into a foot-
gun: grants that reach a filesystem root, `allow_cwd_auth` handed to a
directory, an MCP registered to run the wrong command. The product answer the
owner wants is to let a trusted agent (Claude, via a droppable skill) configure
relay *correctly* on the user's behalf, with the user approving once rather than
learning the whole surface.

Two facts about what exists today shape this:

- **There are two clients.** `relay` the CLI talks to the local Unix socket
  (full control plane, brokered per ADR-017). `relayRemote` (~6,600 lines) is a
  separate binary that talks to the mTLS listener and reaches only `ListTools`
  and `CallTool`. The *backend* is already one process — the tray runs both
  listeners — so what is split is the clients, not the server.
- **ADR-017 gates every privileged operation on per-operation user presence**,
  with a single-use nonce bound to the operation and a digest of its arguments.
  That is the right boundary for a direct human at a CLI. It is the wrong shape
  for an agent performing a multi-step setup, which would face a password
  prompt on every project it creates and every MCP it registers.

The tension the owner surfaced: an agent that a user *trusts to configure relay*
should authenticate once and then work, locally or remotely, without a prompt
per act — while the acts that actually endanger the host stay behind a human.

## Decision

### 1. Authority is a capability of an identity. Presence gates changing an identity's capabilities, not exercising them.

This is the keystone, and everything else follows from it. Relay already models
authority as capabilities a *credential* carries (ADR-015 classes) and grants a
*project* holds (ADR-011). This decision states the rule those were instances
of: **what a caller may do is a property of its identity; obtaining or widening
that property is the privileged act; using it is not.**

Presence therefore moves from *every privileged operation* (ADR-017) to *the act
of granting an identity more authority*. An agent that holds configuration
authority configures without re-prompting; the human was consulted when the
authority was granted, and can withdraw it at any time (decision 5).

### 2. `cli-admin` is a capability like any tool, not a ceremony

Configuration authority is exposed as **a permission on an identity** —
`cli-admin` — toggled through the same grant surface that grants an MCP tool.
There is no separate credential type to mint, no session to open, no TTL to
manage. The permission set is read **per request** (relay already resolves an
enrolment by certificate fingerprint on every connection), so the toggle is
**live**: turning `cli-admin` off takes effect on the next request, with no
session to tear down. A live toggle the human flips off when done is simpler and
strictly more revocable than a time-boxed grant, which must guess a duration and
can expire mid-task.

**`cli-admin` binds to an identity, never to a transport.** Remotely it is a
property of the enrolment, exercisable only by presenting the certificate
(proving the private key). Locally it must bind to a specific credential the
agent holds, **not** to "whoever reached the `0600` socket" — otherwise every
local process would inherit configuration authority while the bit is on, which
is exactly the ambient window ADR-017 refused. This local binding is an open
question (see below); the principle is not.

### 3. Reachability is the existing class–transport matrix. Choose-what-runs never crosses the wire.

Unifying the client does **not** mean the same operations are reachable on both
transports. `ClassReachableOn` already encodes the line:

```
ClassExecute, ClassProxy               → socket only
ClassRead, ClassConfigure, ClassGrant  → socket or TCP
```

Registering an MCP or a service — *anything that chooses what command runs* — is
`ClassExecute` and is therefore **structurally local-only**, today, by the same
matrix that has kept `execute` and `proxy` off the network since ADR-010. This
is not new caution added for this design; it is the existing principle, and it
is the reason "full control plane over the wire" is not a choice on the table. A
route that registers a command, exposed to anything that can reach the listener,
is a network-reachable code-execution primitive. Over a `0600` socket it is a
local process running a command it could already run.

So the unified client aligns at the *client* and *permission-model* layers,
while the *reachable surface* is governed by the class–transport matrix. That
asymmetry is deliberate and principled, not an inconsistency.

### 4. Remote `cli-admin` is narrow: the enrolment's own sandbox, never the host

When `cli-admin` is on for an enrolment, the **`configure`/`grant` classes**
become reachable on the remote listener for that enrolment — but scoped to the
enrolment's **own** objects: it may read and adjust the tools it is granted
within projects it has already been given, and no more. It may **not**:

- register or update anything carrying a command (`execute`, already local-only
  by decision 3),
- mint credentials or touch **other** enrolments (`grant` over the wire is
  scoped to self, not to issuing authority to others),
- toggle `allow_cwd_auth` — which is `configure` by class but `execute` by blast
  radius (it hands a project's whole tool set to any process in a directory, the
  concern ADR-017 singled out and that sealing sharpened, issue #69). It stays
  **local-only** despite its class.

So even a remote with `cli-admin` on, even with a stolen certificate, can tweak
its own sandbox and can never introduce code execution on the host or escalate
beyond itself.

### 5. The rails that make standing authority acceptable

- **Visible.** `relay grant` shows the resulting posture in plain terms, with no
  secret, working with the tray stopped. This is the human's — or a reviewing
  agent's — check on what was configured. A skill's flow should end by showing
  it.
- **Revocable, instantly.** Flipping `cli-admin` off is effective on the next
  request. Revoking the enrolment or the credential is one act.
- **Audited by identity.** Every use is recorded against the enrolment or
  credential id, so "what did this identity do" is answerable after the fact.

### 6. The remote flow, end to end

1. The remote generates its **own** keypair and submits an enrolment request
   carrying only its public key (a CSR). The private key never leaves the remote
   machine; only public artifacts cross. This retires the persisted `client.key`
   liability, where relay generated the key and left a usable credential on disk
   (the exposure demonstrated against the live install).
2. The local human is prompted once and approves; relay signs and returns the
   certificate.
3. The human grants the enrolment tool access — the normal grant surface.
4. To let the remote set itself up, the human toggles `cli-admin` **on** for the
   enrolment (presence-gated, because changing a grant is the privileged act).
   The remote self-configures within its sandbox (decision 4). The human toggles
   it **off**. Off is live.

Steps 3 and 4 are two discrete, human-driven acts — the path of least
resistance for v1. A timed auto-off is a later convenience, not a correctness
requirement; the human's on-then-off is the bound.

### 7. One client

`relay` and `relayRemote` become one binary. It detects transport — local
socket present and openable → local; else → remote mTLS — and the *reachable
surface* falls out of the class–transport matrix (decision 3). Auto-detection is
routing, never trust: a local caller is still the untrusted same-user principal
ADR-017 is about, and privileged local acts still gate on presence per decision
1. The CLI becomes the skill's API, so its refusals must stay legible and its
reads machine-parseable (`--json`).

### 8. The enrolment-request channel is a mailbox, not a door

Decision 6 step 1 needed a way for an unenrolled remote to lodge its CSR so
the local human can approve it. What it is: **a third listener beside
`RemoteServer`, never a mode of it** — same shape as decision 1's own
argument for why the tool plane is a listener next to the bridge socket
rather than a request type folded into it, applied one step further out.
Its whole capability is two methods, `Lodge` and `Poll`, over a bounded,
in-memory, never-persisted table; it holds no reference to a router, the CA,
the sealer or `settings.json`, so there is nothing on this listener a
network peer could reach beyond adding a row to that table and reading it
back.

Two properties everything about this channel follows from:

- **P1 — lodging raises no prompt, ever.** A network peer can put a row in
  the table and nothing else; the human *initiates* approval, and that act
  alone raises the existing, unchanged `enrolment.sign` presence prompt. This
  is a structural answer to "cannot spam prompts," not a rate-limited one —
  no code path from an unauthenticated lodge reaches `presence.Gate` at all.
- **P2 — nothing on this channel is a secret, in either direction.** Inbound
  is a CSR, self-signed and therefore proof of possession by construction;
  outbound is a client certificate and a CA certificate, public verifiers
  useless without a private key that never leaves the client. A channel
  carrying only public artifacts in both directions is a mailbox, not a
  door, and is confined the way a mailbox is: bounded capacity, no admission
  requirement to use it, and nothing behind it worth stealing.

**The transport is plain TCP, deliberately.** Once P2 holds, TLS on this
listener buys nothing real: the client has no CA to verify a handshake
against at lodge time, so pinning one would need `InsecureSkipVerify` plus a
hand-rolled peer check — the exact pattern relay's own client-side guard
refuses to allow anywhere in non-test code, for the reason that a "just for
testing" escape hatch is the version of an insecure default that survives
into production. TLS here would be decoration that *reads* as a security
property without providing one, which is worse than its plain-TCP absence: a
reviewer skimming past an encrypted connection would assume the server was
authenticated, and it structurally cannot be at this point in the exchange.
Plain TCP keeps the real control — comparing relay's CA fingerprint against
a value obtained out of band — visible and mandatory instead of implied.
Approving a request still binds to the exact key that lodged it: the
digest a human's presence grant redeems is built over the CSR's own stored
public key, so an approval answered for one key can never be spent on
another, and comparing the public-key hash alone does not, on its own,
close a man-in-the-middle on the network path — only the client's own CA
pin does that, which is why `--ca-fingerprint` (or an explicit, watched
`--tofu`) is mandatory on the client with no default.

This closes the acceptance bar the open question below was written against,
verbatim: *"it must not become a way to spam prompts or a second door into
issuance."* Neither half is possible by construction — P1 removes the first,
and reusing `enrolment.sign`'s existing gate and digest for the approval
(no new `enrolment.approve` entry in `presence.GatedOps`) removes the
second.

## What this retires, adds, and reworks

**Retires or shrinks** (to be scoped precisely as part of building this, not
promised here): the `relayRemote` duplicate transport/auth stack (~6,600 lines,
of which the tool-calling core survives merged into the one client); and a
small slice of per-operation gate wiring in `package main` — **measured, not
the ~1,700-line/"roughly half" figure this ADR originally claimed.** Step 3's
own measurement (its record: `docs/decisions/017-implementation-spec.md` §6.4,
and the step-3 commit, `wc -l` over every file under `presence/`): 1,935 lines
total including `localauth_darwin.h`/`.m` (1,897 counting `.go` files only),
and 830 non-test production Go by filename convention — every `.go` file
under `presence/` whose name does not end in `_test.go`. That count still
includes `presencetest/presencetest.go` (83 lines): the file name does not
end in `_test.go` even though the package is test-only support never linked
into the release binary (`TestPresence_SeamIsNotLinkedIntoTheBinary`).
**Zero** functions or
types in `presence/` die under the full narrowing this ADR describes — only
`GatedOps` string literals, one line each. The per-op digest builders do not
collapse into a single check either (§2.4's argument, folded back in here):
every surviving op's digest must keep binding every field the request can
carry, not just the field that triggers the gate, so `projectUpdateFields.presenceDigest`
and its siblings survive at full size regardless of how few fields end up
gating. Recommended-scope deletion for step 3 (retiring `mcp.unregister` and
`service.unregister` only) was ~15 lines of production Go, all of it inline
`requireGate` calls and their matching `GatedOps` strings — no `presenceDigest`
method or reason function existed to delete for either op. The full narrowing
this ADR describes, including the `project.grant` change decision 2's open
question blocks on, is estimated at ~70-120 lines, not ~1,700, and it is not
concentrated in `presence/` — that package's shared machinery (`Gate`, the
nonce table, `DigestBuilder`, `CallerSession`, the LocalAuthentication bridge,
every seam guard) serves a gated set of size one exactly as it serves one of
sixteen.

**Adds:** the CSR signing path (`relay enrol sign` and the client's key
generation), the `cli-admin` permission plumbing, and the narrow
`configure`/`grant`-scoped routes on the remote listener.

**Reworks — stated plainly because it revises freshly-merged code:** ADR-017's
per-operation presence gate **narrows**. Presence no longer gates every
`configure`/`grant` operation; it gates *acquiring configuration authority* and
the genuine escalation acts that remain — registering a command (`execute`),
minting `execute`/`grant` credentials, and toggling `allow_cwd_auth`. The gate,
the nonce, and the argument digest all survive for that reduced set. This is the
simpler shape learned from having built the maximal one; it is revision, not
waste, but it is revision.

## Open questions

- **Local identity binding (decision 2).** Remotely `cli-admin` binds to the
  enrolment certificate, which is unforgeable without the private key. Locally
  there is no such per-caller identity on the `0600` socket. It must bind to a
  specific credential the agent holds, or a rogue local process inherits config
  authority while the bit is on. What that local credential is, and how it is
  delivered to the agent without becoming the next stealable secret, is not
  settled. **This is a confirmed blocker, not a remaining detail:** step 3
  (2026-08-29) measured it directly — `bridge/client.go`'s `AdminOp` carries no
  token and no cwd on the local socket, `grep CLIAdmin` returns nothing outside
  `remote_server.go`, and retiring `project.grant`'s gate locally today would
  move its authority check to nothing, handing any same-user process the exact
  ambient window this decision's own wording forbids. `project.grant` stays
  fully gated (unnarrowed) until this binding exists; the narrowing to
  `allow_cwd_auth`-only described above did not ship in step 3 and is blocked
  on this open question being closed first.
- **Timed auto-off** (decision 6) is deferred to a later iteration by choice.

## See also

- [ADR-010](010-remote-client-transport-and-identity.md) — the two-entry remote
  surface and the enrolment CA this narrows a hole in; the budgeted-enrolment
  shape the request channel should follow.
- [ADR-011](011-resource-scope.md) — project grants as held capabilities, the
  pattern decision 1 generalises.
- [ADR-015](015-control-plane-authorization.md) — the class model and the
  class–transport matrix decision 3 rests on, and the "route not registered
  rather than check inverted" argument decision 4 keeps for `execute`.
- [ADR-017](017-config-dir-is-not-a-boundary.md) — the per-operation presence
  gate this narrows, the sealing whose broker this reuses, and `allow_cwd_auth`
  (issue #69), which decision 4 keeps local-only.
