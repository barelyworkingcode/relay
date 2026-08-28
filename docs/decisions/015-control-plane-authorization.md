# ADR-015: The Control Plane Is Classed by Blast Radius, and Transport Is Part of the Grant

**Status:** Proposed
**Date:** 2026-08-28

## Context

ADR-014 moved 24 of relay's 27 capabilities onto HTTP and said plainly that it
was deferring authorization and that the deferral was the risk. This ADR pays
that debt. Nothing here is a new discovery — the migration made an existing
weakness reachable in a new way, which is exactly when it has to be settled.

Every route on the frontend server is gated by one credential:
`RELAY_FRONTEND_TOKEN`, checked by `frontendBearerAuth`. It is a single bearer
that grants everything, minted once per install, injected into every frontend
consumer. That was adequate when the API served Eve's project dialog and
session creation. It is not adequate now, because the same token reaches:

| route | what a holder can do |
|---|---|
| `POST /api/mcps` (stdio) | make relay execute an arbitrary command with arbitrary args and env |
| `POST /api/services`, `PUT /api/services/{id}` | write a `command` into settings, then start it |
| `POST /api/mcps` (http) | choose an address relay will connect to — `validateMcpURL` checks scheme and host presence only, so link-local and RFC1918 are reachable |
| `PUT /api/remote` | set the mTLS listener's bind address, including non-loopback |
| `POST /api/enrolments` | issue a client certificate granting a remote machine host tool access |
| `DELETE /api/enrolments/{id}` | revoke the credential whose calls the audit log attributes |

The first row is the one that settles the argument. Relay's entire thesis is
that a semi-trusted agent can be given exactly one mailbox and nothing else.
A control plane where the credential that reads a project list is the same
credential that runs a command as the host user does not have boundaries; it
has one boundary, on the outside, and a flat interior.

Two properties of the existing design are load-bearing and must not be
discarded while fixing this:

- **`PeerPID` is not an authorization input.** `bridge/peer_darwin.go` says so
  in as many words: a pid is reusable and racy, fine for "who called this" and
  not fine for "may they call it". Any scheme resting on identifying the
  calling *process* is refused here for the reason already written down.
- **The frontend socket is 0600.** Its authority comes from the filesystem:
  only the owning user can open it. That is a real, kernel-enforced property,
  and it is a property of the *listener*, known at bind time — not an inference
  about a peer.

## Decision

### 1. Every capability carries a class, and the class is about blast radius

Four classes. A capability's class is a property of what it can cause, never of
which tab it appears on:

| class | meaning | examples |
|---|---|---|
| `read` | discloses configuration or history | `GET /api/projects`, `GET /api/audit` |
| `configure` | changes relay's own state | `PUT /api/projects/{id}`, `POST /api/services/{id}/stop`, `POST /api/audit/export` |
| `grant` | issues or revokes a credential another party holds | `POST /api/enrolments`, `DELETE /api/enrolments/{id}` |
| `execute` | the CALLER supplies what runs or what is exposed | `POST /api/mcps` (stdio), `POST`/`PUT /api/services` (the `command` field), `PUT /api/remote` |

The line for `execute` is not "a process starts" — `POST /api/services/{id}/start`
launches one and is merely `configure`. The line is **who chose what runs**.
Starting an already-configured service acts on a decision the operator already
made; writing a `command` field makes the decision. That distinction is the
whole classification, and it is why service create/update sit in `execute`
alongside MCP registration while service start/stop do not.

### 2. Transport is part of the grant, and `execute` is socket-only

An `execute`-class capability is **not routed onto any TCP listener at all.**
Not gated behind a stronger token on that listener — absent from it. The
registration of those routes is conditional on the listener the mux is being
built for.

This is deliberate and it is the core of the ADR. A policy check inside a
handler is code someone can later invert, wrap, or forget; a route that was
never registered on that listener has no code path to reach. Relay already
applies exactly this reasoning to `RemoteServer`, whose dispatch table holds
only `ListTools` and `CallTool` so that a new admin operation is unreachable
from a VM until someone deliberately adds it to a list that is visibly a
security boundary (ADR-010). The same shape, for the same reason.

The justification for treating the two listeners differently is not vague
trust. The Unix socket is 0600: the kernel refuses a connection from any other
user, and that is checked before relay sees a byte. The TCP listener has no
equivalent property and, as ADR-014 established, currently has no
authorization model beyond a shared bearer. Until it does, the answer to "may
this listener carry a capability that runs a caller-supplied command" is no,
and after it does, the answer is still no — a credential can be stolen and a
filesystem permission is harder to borrow.

Consequence, stated plainly so it is not discovered later: **a browser-based
view can never register an MCP or create a service.** Those actions stay in
the tray, which is a real functional cost and the correct one. ADR-014 named
the native residue as capabilities that cannot be *implemented* behind an API;
this adds a second category — capabilities that can be implemented and should
not be *reachable* remotely.

### 3. A credential names its classes, and absent means none

The single bearer is replaced by credentials that carry an explicit class set.
This deliberately reuses ADR-011's allowlist shape rather than inventing a
second permission vocabulary:

- An absent class set grants nothing. Not "everything", not "read" — nothing.
  A credential minted by a tool that did not know about classes is inert
  rather than omnipotent.
- `execute` is not grantable to a TCP-borne credential at all, per decision 2.
  The class exists on the credential so the socket path can still distinguish
  a consumer that needs it from one that does not.
- Existing installs migrate by minting a `read`+`configure` credential with the
  current token's value, so Eve and relayScheduler keep working unchanged.
  They are then narrowed from evidence — the audit log already records which
  capability each caller reached, so the narrowing is measured rather than
  guessed. This is ADR-010 decision 7's method applied to the control plane.

### 4. The view is the least privileged client, not the most

The settings view is the client whose credential is most exposed — rendered
into a page, resident in a browser process relay does not control, and
reachable by anything else that page loads. It therefore gets the smallest
class set that lets it work, and it is the *only* client for which that is a
stated design goal rather than a consequence.

This inverts the current arrangement, where the WebView is the most trusted
caller in the system because it shares relay's address space. That trust was a
property of the transport, not of the view, and ADR-014 removed the transport.
The trust has to go with it.

### 5. `RELAY_API_LISTEN` stays a devbox convenience until this lands

ADR-014 introduced an opt-in loopback TCP bind, absent by default, and called
it a devbox convenience pending this ADR. It remains exactly that. When
decisions 1–4 are implemented, the bind becomes a supported deployment; until
then it is a development affordance and should not be documented as anything
else.

## Consequences

**What gets audited changes.** Every authorization decision on the control
plane becomes a recordable event with the same standing as a tool-call denial.
The audit log currently records what an agent reached through the router; it
does not record that someone with the frontend bearer created a service. That
gap closes here, because a control plane whose refusals are invisible cannot
be reasoned about after an incident — the same argument ADR-010 made for
making audit a hard dependency of remote access.

**Some things get less convenient, on purpose.** A script that today does
everything with one token will need a credential per role. The migration path
above means nothing breaks on upgrade, but the end state deliberately makes
"one token that does everything" unavailable rather than merely discouraged.

**This does not make the API safe to expose beyond loopback.** Classing and
scoping is necessary and not sufficient; a non-loopback control plane needs
transport authentication of its own, and relay already has the machinery for
that in `enrolment_ca.go` — its own CA, issuing client certificates, with
revocation rather than expiry as the control. Whether the control plane should
reuse that machinery instead of bearer credentials entirely is a real question
and is deliberately left open here, because answering it well needs the class
model to exist first.

**The three excluded capabilities stay excluded.** `reveal_audit_log`,
`authenticate_mcp` and `reset_mcp_permissions` remain IPC-only for the reasons
ADR-014 gives. Nothing in this ADR is an argument for exposing them.

## See also

- [ADR-014](014-every-capability-is-an-http-capability.md) — the migration that
  created this debt and named it as blocking.
- [ADR-010](010-remote-client-transport-and-identity.md) — the two-entry
  dispatch table this ADR's decision 2 imitates, the enrolment CA decision 3
  points at, and the tune-from-evidence method decision 3 reuses.
- [ADR-011](011-resource-scope.md) — the fail-closed allowlist shape decision 3
  reuses rather than duplicating.
- [ADR-005](005-tcc-permissions.md) — why some capabilities are desktop-bound
  regardless of any credential.
