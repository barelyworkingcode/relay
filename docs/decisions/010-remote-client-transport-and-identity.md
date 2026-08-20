# ADR-010: A Remote Caller Is a Certificate on a Narrow Listener

**Status:** Accepted
**Date:** 2026-08-20

## Context

Step 2b of letting a semi-trusted LLM agent run on a separate VM and reach
relay for tool access. ADR-009 made the *project model* coherent for that
client and stopped there, deliberately: "it does not add the listener, client
certificates, or the enrollment flow that would let one actually connect."
This ADR is that missing half.

The concrete target is an agent in a VM asking relay to read Apple Mail on the
host. The threat model is unchanged from ADR-009 — **exfiltration, not
privilege escalation**. The VM cannot escape to the host, so the risk worth
designing against is an agent reading an entire mailbox and shipping it
somewhere, not an agent breaking out of a sandbox.

Everything above the transport is ready. The transport does not exist, and the
authentication model underneath it assumes it never will. Three properties of
the current design are correct locally and become wrong the moment a socket is
reachable from another machine:

1. **Reachability equals authority.** The bridge dispatches ten request types.
   Two (`ListTools`, `CallTool`) are project-scoped. The other eight —
   `ResolvePtyEnv`, `RegisterManifest`, `ListProjects`, `GetProject`,
   `ResolveProjectTemplate`, `ReconcileExternalMcps`, `ReloadExternalMcp`,
   `ReloadService` — are separated from a tool call only by *which token was
   presented*, and a service token is documented god-mode ("bypasses all
   per-project tool filtering"). Locally that separation is sound, because the
   0600 socket already means same-user. Over a network it makes a single token
   comparison the only thing between a remote VM and relay's control plane.

2. **The cwd argument inverts.** `docs/tokens.md` justifies trusting a
   caller-asserted working directory because "anything able to lie here can
   already read every token out of the 0600 settings.json." That reasoning is
   airtight for a local process and false for a remote one: a remote caller
   cannot read `settings.json`, so the thing the argument trades away is no
   longer already lost. A tokenless remote caller asserting a cwd inside any
   local project with `allow_cwd_auth` would authenticate as that project.

3. **Fail-open audit stops being harmless.** `docs/audit-log.md` already says
   so: dropping an event rather than delaying a call is "a deliberate choice
   for local use… the wrong choice for a remote caller, where the trust
   boundary justifies refusing a call that cannot be recorded."

A fourth property was never right, and only remote access makes it urgent:
there is no rate limit, quota, throttle, or volume budget anywhere in the
codebase. `MaxMessageSize` (10 MiB) bounds a single frame, not a session.
ADR-008 bought detection; nothing bought interdiction. A remote grant of
`mail_search` + `mail_get_emails` can drain a mailbox as fast as Mail.app
answers, fully logged and wholly unimpeded — the exact threat ADR-009 names,
with the audit log as a faithful record of it happening.

## Decision

### 1. A remote caller gets its own listener, and that listener can only call tools

`RemoteServer` is a second listener, separate from `BridgeServer`, whose
dispatch table contains exactly `ListTools` and `CallTool`. The other eight
request types are not refused by a check inside a shared handler — they are
absent from the remote dispatch table, so there is no code path from a remote
connection to `ResolvePtyEnv` or `RegisterManifest` at all.

This is deliberately *not* implemented as `if isRemote { … }` guards added to
`bridge/server.go`. A shared handler with per-request transport checks makes
every future request type a decision someone must remember to get right, and
the failure mode of forgetting is silent: a new admin op is reachable from the
VM the day it is added, with nothing in review to catch it. Two dispatch
tables make the same mistake loud — a new op is unreachable remotely until
someone deliberately adds it to a list that is visibly a security boundary.

This mirrors the belt-and-braces reasoning ADR-009 applied to `allowed_dirs`,
and for the same stated reason: the failure mode is a *silent* widening of
scope rather than a loud error.

### 2. The certificate is the identity; the enrolment record is the grant

A remote connection requires mutual TLS, and **relay is its own certificate
authority** — self-signed, no external CA. A two-machine deployment does not
warrant the ceremony of anything else.

The certificate answers *who is calling*. An **enrolment record** on the host
binds that certificate to the grants it may use. There is no bearer token on
the remote path at all.

An earlier draft of this ADR required a project token alongside the
certificate. That was wrong. Once a certificate is mandatory, the token adds a
second long-lived plaintext secret to the remote path and buys nothing: a
leaked token is already useless without a certificate. Removing it means a
copy of `settings.json` grants an attacker no remote access whatsoever.

An enrolment holding more than one grant selects among them by sending a
project **id**, which is not a secret. Relay honours it only if that enrolment
actually holds the grant.

**The enrolment is keyed by certificate, not by machine.** One host may hold
many enrolments, which is the expected shape: several agents on one VM, each
with its own certificate and its own grants, audited and revoked
independently.

    hermes-mail    sha256:9f2a...  [proj_mail]
    hermes-cal     sha256:41c7...  [proj_calendar]
    hermes-triage  sha256:8b03...  [proj_mail, proj_calendar]

**The limit of that separation is stated rather than pretended away.** Relay
distinguishes those agents only by the key each presents. Agents running as
the same user on the same VM can read each other's private keys, so the
separation is exactly as strong as the filesystem isolation on the client
side — different users, containers, or key permissions make it real. This is
the same reasoning `docs/tokens.md` applies to local processes, reappearing
one boundary out. Relay cannot enforce it from the host and does not claim to.

Device and capability revocation stay independent: editing an enrolment's
grants changes what a machine may reach without touching its certificate;
deleting the enrolment cuts the machine without disturbing any project.

`RemoteServer` resolves the certificate to an enrolled client before any
request is read. A connection whose certificate does not resolve is closed
without processing, so an unenrolled caller cannot probe for valid grants.

### 3. An enrolment may only grant remote-kind projects

Every grant on an enrolment must name a project with `IsRemote()` true.

Without this, every protection in ADR-009 — no filesystem scope, no wildcard
grants, no sessions, no PTY — would be bypassed by *pointing at the wrong
project* rather than by defeating any of them. Service tokens need no
equivalent rule here: decision 2 leaves no token field on the remote wire, so
god-mode is not refused, it is unrepresentable.

Enforced at three points, for the reason ADR-009 gave when it defended
`allowed_dirs` twice — the failure mode is a silent widening of scope, not a
loud error:

1. **At enrolment.** A grant naming a local project is refused outright.
2. **At conversion.** Converting a project remote→local is refused while any
   enrolment references it, naming the offending enrolments. This mirrors
   ADR-009's constrained local→remote conversion: the project model refuses
   an edit that would strand a credential, rather than allowing it and
   cleaning up afterwards.
3. **At call time.** The resolved project is re-checked. This is what makes a
   grant that went stale by any route relay did not anticipate fail closed.

**Remoteness is a property of the grant's shape, not of the caller's
location.** `validateProjectShape` constrains path, cwd auth, skills,
templates, wildcards, and models — nothing in it consults a host or address.
A remote project is reachable from `127.0.0.1` exactly as it is from another
machine, so the whole model is developed and tested on one box with every
guard live. No loopback exemption exists, and none is needed: a dev-only
bypass on the security boundary is the thing most likely to survive into
production, and it would invert ADR-009's principle of refusing incoherent
combinations rather than degrading.

One consequence shapes local testing. `ValidateProjectGrants` refuses any MCP
declaring `allowed_dirs`, so a remote project cannot be granted fsMCP. Testing
therefore runs against macMCP — mail, calendar, contacts — which is the actual
target use case rather than a filesystem stand-in.

### 4. The remote wire has no field to authenticate with

`RemoteRequest` is a distinct type carrying `Type`, `Name`, `Arguments`, and
`ProjectID`. There is no `Token` field and no `Cwd` field.

Directory auth is already unreachable by construction: it fires only when no
token is present, and identity on this path comes from the certificate before
a request is read. The separate type is what makes that visible in the code
rather than emergent from a chain of reasoning.

Decoding uses `DisallowUnknownFields`. Go's default is to ignore unrecognised
JSON keys silently, which would leave a client sending `cwd` believing it had
authenticated by directory while it had in fact authenticated by certificate —
the two succeed identically today, so the divergence would surface only later,
in the confusing direction. Strict decoding turns that into an error at the
door. The cost is that adding a wire field requires deploying both ends
together; both ends are ours, so that is a coordination step rather than a
compatibility break.

### 5. Audit is fail-closed for remote callers, and written before the call

A remote tool call is recorded in two phases. An **intent** record — actor,
tool, redacted arguments — is written *and flushed* before the MCP is invoked.
A **completion** record keyed to the same event `id` follows when the call
returns. If the intent write fails, the call is refused and the MCP is never
invoked.

The ordering is the whole point, and an earlier draft of this ADR got it
wrong. Today's audit event is written *after* the call completes, because
`doneResult` needs the result bytes. Under that ordering "refuse a call that
cannot be logged" protects nothing: by the time the write fails the mailbox has
already been read, and the data has left the MCP. Refusing afterwards is
theatre.

ADR-008 named exactly what fail-open forfeits — "the log is not admissible as
proof that something did *not* happen." Buying that back requires writing
before the side effect, not after. The cost is a second line per remote call,
which for an agent checking mail on a schedule is nothing.

An intent record with no matching completion is itself a signal: it means relay
invoked an MCP and never learned the outcome — a crash, a kill, or a hang —
and it is worth alerting on rather than reconciling away.

This also settles how streaming interacts with fail-closed auditing:
`RespProgress` frames need no separate guarantee, because the intent record
already establishes that the call happened before any frame is emitted.

Local behaviour is untouched. The bounded channel, the drop-and-count, and the
visible drop warning all stay exactly as ADR-008 specified, because the
reasoning that justified them there is unaffected by any of this.

### 6. `AuditActor` gains attested remote identity

A remote call records `kind: "remote"` and `auth: "mtls"`, plus three fields
derived from the connection and never asserted by the caller: the enrolled
client id, the full certificate fingerprint, and the remote address.
`PID` / `Proc` / `Parent` are omitted rather than zero-filled — they are
`omitempty`, so an absent field reads as "not applicable" instead of "unknown".

    "actor": {
      "kind": "remote",
      "project_id": "proj_mail",
      "project_name": "Mail",
      "auth": "mtls",
      "client_id": "hermes-mail",
      "fingerprint": "sha256:9f2a...",
      "remote_addr": "127.0.0.1:52233"
    }

`kind` is a distinct value rather than a reuse of `project` so that "show me
everything any VM did" is a first-class filter rather than an inference from
which fields happen to be populated. `ProjectID` stays populated alongside it:
the caller is remote *and* is acting as a project grant, and both facts matter.

This preserves ADR-008's second property — attribution "from relay's own
knowledge, not from anything the caller asserts." `PeerPID` is the local
expression of that property and is meaningless across a network; the
certificate fingerprint is its remote equivalent and is strictly stronger,
since a pid is reusable and racy while a fingerprint is not.

The fingerprint is recorded in full, not truncated, and alongside the resolved
client id rather than instead of it. That is what keeps a revoked device's
history legible: it answers which *key* made a call, after the enrolment naming
that key has been deleted.

### 7. Budgets live on the enrolment

A remote enrolment carries a call-rate limit and a cumulative result-volume
limit per rolling window. Both are enforced in `appRouter.CallTool` — the
chokepoint ADR-008 chose, for the same reason: every transport already funnels
through it, and the actor is known by the time it runs. `ResultBytes` is
already measured for the audit log, so the accounting exists; only the budget
did not.

**The enrolment is the unit of compromise, so it is the unit that carries the
cap.** If `hermes-mail` is compromised, the attacker holds `hermes-mail`'s key
and nothing else; its budget is what bounds the damage. A budget on the
*project* would only bind an attacker who already held several certificates,
by which point they have broader access anyway — and it would let one noisy
agent starve its neighbours sharing the same grant.

A project-level ceiling, bounding total drain across every enrolment holding a
grant, is a coherent later addition. It protects the resource rather than
bounding any single compromise, and it is not what buys the primary defence.

Exceeding a budget is refused with its own audit outcome, `throttled`, distinct
from `denied` (a tool the grant never included) and `tool_error` (a boundary
inside the MCP). The distinction is the signal: `throttled` is the only one of
the three that says *the grant was legitimate and the pattern of use was not*,
which is precisely what exfiltration looks like from the host's side.

Rate and volume are budgeted together because they fail differently. Rate alone
does not stop a slow drain, and a mailbox exfiltrated over six hours is
exfiltrated.

Defaults are conservative and per-enrolment, because there is no meaningful
global default: an agent that checks mail hourly and one that answers
interactive questions have nothing in common. The first values will be wrong;
`throttled` is distinguishable in the log precisely so that tuning is driven by
evidence rather than by guessing twice.

### 8. Enrolment is an explicit host-side act; revocation cuts live connections

Relay is its own CA. An operator creates an enrolment on the host, relay signs
a client certificate, and relay emits a bundle — client key, client
certificate, and the CA certificate the client needs to verify the server —
which is copied to the client machine.

There is no self-service enrolment and no bootstrap token. An enrolment
endpoint reachable by presenting a secret would reintroduce precisely the
replayable credential decision 2 removed, and would do it at the one point in
the system where the result is a *new identity* rather than a single call.

The private key is generated on the host and travels to the client once. That
is a deliberate simplification and the weakest step in this design: a CSR flow,
where the client generates its key and only a signing request crosses the gap,
never exposes the key at all. It is the obvious upgrade if these machines ever
stop being the same person's.

Client certificates are long-lived, and **revocation rather than expiry is the
control**. Short lifetimes would need an authenticated renewal path, which is
the replayable-secret problem again wearing a different hat. This is the right
trade for a handful of enrolments and the wrong one for a fleet; it is the
first thing to revisit if the number of enrolments grows.

Revocation deletes the enrolment, and **closes any live connection holding that
certificate**. Taking effect on the next connection is not enough: the wire
protocol holds persistent connections in a scanner loop, so a compromised agent
that never reconnects would keep working indefinitely.

Enrolments are listed in the Settings UI beside the grants they reach — a
credential you cannot see is one you will not revoke.

### 9. Tunnels are a network path, never an identity

`tunnel` and `wiretunnel` may carry `RemoteServer` traffic. Neither may
substitute for it, and the `BridgeServer` Unix socket must never be forwarded
through one.

Forwarding the bridge socket would satisfy reachability while silently
defeating every decision above: relay would see a local connection, so
`PeerPID` would attribute calls to the tunnel process, directory auth would
become reachable from off-host, the full ten-request surface would be exposed,
and there would be no per-client identity to record or revoke. It is the
fastest way to build this and it produces the exact system this ADR exists to
avoid.

`RemoteServer` is **disabled unless configured**, and its listen address
defaults to `127.0.0.1`:

    "remote": {
      "enabled": true,
      "listen": "127.0.0.1:9910"
    }

An absent block means no listener. The default binds loopback so that
misconfiguration cannot expose the control plane to a LAN, and so that
same-machine development needs no configuration at all. Reaching relay from a
VM is then a deliberate act — widening the bind address, or running a tunnel —
rather than something that happens by leaving a field unset.

`RemoteServer` sets read and write deadlines on every accepted connection.
`BridgeServer` sets none, which is harmless on a local socket where the peer is
same-user and a slowloris vector on a network listener.

## Consequences

- **A remote client cannot do anything but call tools.** No project listing, no
  manifest registration, no PTY, no admin. If a future remote client needs one
  of those, it is a deliberate addition to a two-entry dispatch table with its
  own review, not a discovery that it already worked.

- **A stolen `settings.json` grants no remote access.** Decision 2 removed the
  bearer token from this path entirely, so the file that holds every project
  token in plaintext is not a remote credential. The certificate is, and it
  lives only where an operator put it.

- **Enrolment is manual, and that is a cost.** Standing up a new agent requires
  a host-side action and a file copy. Accepted: the alternative is an automated
  flow gated by a replayable secret, at the one point in the system where the
  result is a new identity rather than a single call.

- **The private key travels once, and that is the weakest step.** A CSR flow
  would avoid it. This is the first thing to change if these machines stop
  being the same person's.

- **Remote tool calls can fail because auditing failed.** The one place this
  ADR knowingly trades availability for evidence. An intent record with no
  completion is a signal, not noise.

- **Budgets will be tuned by being hit.** First values will be wrong. That is
  why `throttled` is a distinct outcome: tuning is driven by evidence rather
  than by guessing twice.

- **Agent separation on one VM is only as strong as that VM.** Relay
  distinguishes co-located agents by the key each presents and cannot enforce
  that they keep those keys from each other.

- **`relayClient` is taken** — it is the Capacitor iOS wrapper for Eve. The new
  client needs its own name; `relayRemote` and `relayAgent` are both free.

- **Per-tool tri-state permissions remain out of scope**, as ADR-009 left them.
  A remote grant's enumerated `allowed_mcp_ids` plus `disabled_tools` is
  sufficient for 2b.

### Deferred, deliberately

Each of these is a decision already taken in a direction, not an unknown:

- **A project-level volume ceiling** across every enrolment holding a grant.
  Protects the resource rather than bounding a single compromise (decision 7).
- **A CSR enrolment flow**, so a client key never transits (decision 8).
- **Short-lived certificates with automated renewal**, which need an
  authenticated renewal path that does not become a replayable secret
  (decision 8). Revisit when the number of enrolments outgrows manual
  revocation.

## See also

- ADR-009 — the remote project model this completes
  (`docs/decisions/009-remote-projects.md`); "the listener, client
  certificates, and enrollment flow are 2b" is the sentence this ADR answers,
  and `validateProjectShape` is why remoteness is a shape rather than a
  location.
- ADR-008 — the audit chokepoint, and the fail-open trade decision 5 reverses
  for remote callers (`docs/decisions/008-tool-call-audit-log.md`).
- ADR-007 — the token brokering model that decision 2 deliberately does *not*
  extend to the remote path (`docs/decisions/007-project-token-brokering.md`).
- `docs/tokens.md` — the credential inventory. Its directory-auth section
  states the local-only reasoning that decision 4 makes unrepresentable, and
  its same-user argument is the one decision 2 relocates to the client machine.
- `bridge/server.go`, `bridge/client.go` — the Unix-socket listener and dialer
  `RemoteServer` sits beside rather than replaces.
- `bridge/types.go` — `BridgeRequest`, and the ten request-type constants
  decision 1 narrows to two.
