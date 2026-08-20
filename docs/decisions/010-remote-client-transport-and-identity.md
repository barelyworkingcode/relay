# ADR-010: A Remote Caller Is a Certificate on a Narrow Listener

**Status:** Proposed
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

### 2. The certificate is the identity; the project token names the grant

A remote connection requires mutual TLS. The client certificate answers *who
is calling* and the project token answers *which capability grant to apply*.
Both are required; neither substitutes for the other.

A bearer token alone cannot carry this weight. Project tokens are long-lived,
stored in plaintext in `settings.json`, replayable in full by anyone who
observes one, and carry no expiry — properties that are acceptable when the
only way to observe one is to already be the local user, and unacceptable
across a network. The certificate is what makes a call attributable to a
*machine* rather than to a secret that may have been copied off one.

The token is not demoted to a formality. It remains the thing that selects the
grant, so revoking a grant and revoking a device stay independent operations:
rotating a project token cuts capability without re-enrolling hardware, and
revoking a certificate cuts a machine without disturbing any project.

`RemoteServer` resolves the certificate to an enrolled client identity before
the token is examined. A connection whose certificate does not resolve is
closed without reading a request, so an unenrolled caller cannot probe token
validity.

### 3. Only remote-kind project tokens are accepted, and service tokens never are

The remote listener refuses any token that does not resolve to a project with
`IsRemote()` true. A service token presented on a remote connection is
rejected before use, not merely unhelpful.

Both halves matter and neither is redundant. Refusing service tokens closes
god-mode. Requiring `IsRemote()` means the ADR-009 guards — no filesystem
scope, no wildcard MCP grants, no sessions, no PTY — are load-bearing for
every remote call rather than incidental to which token the caller happened to
present. Without it, a leaked *local* project token would grant a remote
caller a filesystem-scoped project, and every protection ADR-009 built would
be bypassed by presenting the wrong credential rather than by defeating any of
them.

### 4. Directory auth is structurally impossible on the remote listener

`RemoteServer` never populates the caller-cwd context value, and a remote
request carrying `Cwd` is rejected rather than ignored.

Rejecting rather than ignoring is the whole point. Silently dropping the field
would leave a client that believes it is authenticating by directory and is
in fact authenticating by token — the two produce identical success today,
so the divergence would surface only when the token was removed and the call
unexpectedly kept working, or stopped. An explicit error at the door makes a
client that tries this fail immediately and visibly.

The condition ADR-009 identified for remote projects — "a remote caller's cwd
is a path on a *different* machine, with nothing on the host to compare it
against" — is a property of the *connection*, not only of the project. Enforcing
it at the transport means it holds even for a request that never resolves to a
project at all.

### 5. Audit is fail-closed for remote callers

`audit.required` (default true for remote, unchanged fail-open for local)
refuses any remote tool call that cannot be durably recorded. If the audit
channel is saturated or the log cannot be written, the call returns an error
instead of proceeding unlogged.

ADR-008 accepted fail-open with a precise consequence: "the log is not
admissible as proof that something did *not* happen." That is a fair trade for
a local laptop, where the alternative is your own tooling breaking. It is the
wrong trade for the one caller whose activity the log exists to establish. The
cost is real and is accepted here: a remote agent will occasionally see a tool
call fail because relay could not write a line about it.

Local behaviour does not change. The same drop-and-count path stays exactly as
ADR-008 specified it, because the reasoning that justified it there is
untouched by any of this.

### 6. `AuditActor` gains attested remote identity

Three fields, all derived from the connection rather than asserted by the
caller: the enrolled client identity, the certificate fingerprint that
authenticated it, and the remote address. A new `AuditAuthMTLS` constant joins
the existing auth methods.

This preserves ADR-008's second property — "attributed… from relay's own
knowledge, not from anything the caller asserts." `PeerPID` is the local
expression of that property and is meaningless across a network; the
certificate fingerprint is its remote equivalent, and is strictly stronger,
since a pid is reusable and racy while a fingerprint is not.

Recording the fingerprint and not only the resolved identity is what makes
revocation auditable after the fact: it answers which *key* made a call, so a
compromised device's history stays legible once its enrolment is deleted.

### 7. Remote grants carry budgets, enforced at the router

A remote project may declare a call-rate limit and a cumulative result-volume
budget per rolling window. Both are enforced in `appRouter.CallTool` — the
same chokepoint ADR-008 chose, for the same reason: it is the one place every
transport already funnels through, and the place where the project is known.

Exceeding a budget is refused with an audit outcome of its own, distinct from
`denied` (a tool the grant never included) and from `tool_error` (a boundary
inside the MCP). The distinction is the point: a rate refusal is the only one
of the three that says *the grant was legitimate and the pattern of use was
not*, which is precisely the exfiltration signal this ADR exists to surface.

Volume is budgeted alongside rate because they fail differently. Rate alone
does not stop a slow drain, and a mailbox exfiltrated over six hours is
exfiltrated. Result bytes are already measured for the audit log
(`ResultBytes`), so the accounting exists; only the budget does not.

Defaults are conservative and per-project, because there is no meaningful
global default: a grant that exists to check mail hourly and one that exists
to answer interactive questions have nothing in common.

### 8. Enrolment is an explicit host-side act, and revocation is immediate

A client is enrolled from the host — a deliberate operator action producing a
certificate bound to one client identity. There is no self-service enrolment
and no bootstrap token that can be replayed into a new identity: the whole
value of decision 2 is that a certificate is harder to copy than a token, and
an enrolment flow reachable by presenting a secret would reintroduce exactly
the property being designed out.

Revocation takes effect on the next connection, not the next restart, and
enrolments are listed in the Settings UI beside the projects they can reach —
a credential you cannot see is one you will not revoke.

### 9. Tunnels are a network path, never an identity

`tunnel` and `wiretunnel` may carry `RemoteServer` traffic. Neither may
substitute for it, and the existing `BridgeServer` Unix socket must not be
exposed through one.

Forwarding the bridge socket would satisfy reachability while silently
defeating every decision above: relay would see a local connection, so
`PeerPID` would attribute calls to the tunnel process, directory auth would
become reachable from off-host, the full ten-request surface would be exposed,
and there would be no per-client identity to record or revoke. It is the
fastest way to build this and it produces the exact system this ADR is written
to avoid.

## Consequences

- **A remote client cannot do anything but call tools.** No project listing,
  no manifest registration, no PTY, no admin. If a future remote client needs
  one of those, it is a deliberate addition to the remote dispatch table with
  its own review, not a discovery that it already worked.

- **Enrolment is manual, and that is a cost.** Standing up a new VM requires a
  host-side action. This is accepted: the alternative is an automated flow
  gated by a replayable secret, which would undo decision 2.

- **Remote tool calls can fail because auditing failed.** New failure mode,
  accepted deliberately, and the one place where this ADR knowingly trades
  availability for evidence.

- **Budgets will be tuned by being hit.** A first guess at a mail-checking
  agent's rate will be wrong. The refusal is audited and distinguishable, so
  tuning is driven by evidence rather than by guessing twice.

- **`relayClient` is taken.** It is the Capacitor iOS wrapper for Eve. The new
  client needs a different name; `relayRemote` and `relayAgent` are both free.

- **Connection deadlines become mandatory.** `BridgeServer` sets no read or
  write deadlines on accepted connections — harmless on a local socket where
  the peer is same-user, a slowloris vector on a network listener.
  `RemoteServer` sets both.

- **Per-tool tri-state permissions remain out of scope**, as ADR-009 left
  them. Nothing here depends on that model, and a remote grant's enumerated
  `allowed_mcp_ids` plus `disabled_tools` is sufficient for 2b.

### Open questions

These are the parts most likely to change under review, and are called out
rather than settled so that accepting this ADR does not implicitly accept a
guess:

- **Certificate authority shape.** Whether relay acts as its own CA or
  consumes an external one. Self-signed per-host is simplest and matches how
  `eve.lan` is already handled in `relayClient`; an external CA is more
  ceremony than a two-machine deployment warrants.

- **Certificate lifetime and renewal.** Short-lived certificates with
  automated renewal are stronger but require a renewal path that is itself
  authenticated — which risks recreating the replayable-secret problem
  decision 8 avoids.

- **Whether budgets belong on the project or the enrolment.** A project is the
  capability; an enrolment is the machine. Two VMs sharing one grant is
  plausible, and it is not obvious which should hold the budget.

- **Streaming under fail-closed audit.** `RespProgress` frames are emitted
  during a call. Whether a mid-stream audit failure aborts an in-flight call
  or is recorded and tolerated is unresolved.

## See also

- ADR-009 — the remote project model this completes
  (`docs/decisions/009-remote-projects.md`); "the listener, client
  certificates, and enrollment flow are 2b" is the sentence this ADR answers.
- ADR-008 — the audit chokepoint and the fail-open trade this narrows for
  remote callers (`docs/decisions/008-tool-call-audit-log.md`).
- ADR-007 — the token brokering model the grant half rests on
  (`docs/decisions/007-project-token-brokering.md`).
- `docs/tokens.md` — the credential inventory; the directory-auth section
  states the local-only reasoning that decision 4 inverts.
- `bridge/server.go`, `bridge/client.go` — the Unix-socket listener and dialer
  that `RemoteServer` sits beside rather than replaces.
- `bridge/types.go` — `BridgeRequest`, and the ten request-type constants that
  decision 1 narrows to two.
