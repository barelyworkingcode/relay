# ADR-014: Every Capability Is an HTTP Capability, and the View Is Just a Client

**Status:** Proposed
**Date:** 2026-08-27

## Context

Relay has three front doors onto the same state, and they cover different
subsets of it. The settings WebView reaches Go through
`window.webkit.messageHandlers.ipc`; Eve and relayScheduler reach it over the
frontend socket; the CLI reaches it over the bridge. Measured against `main`,
the IPC dispatch table (`ipcHandlers`, `ipc_handlers.go`) holds 27 commands and
the HTTP surface answers 9 of them:

| area | commands | on HTTP |
|---|---|---|
| projects | create, update, remove, rotate_token, regen_skill, disabled_tools, list_mcp_tools, enumerate_scope | all 8 |
| MCPs | add, authenticate, remove, reset_permissions | read-only routes only; no mutations |
| services | add, remove, update, autostart, start, stop | none |
| service inspector | action, config | none |
| audit | query, export, reveal_log | none |
| enrolments | create, revoke, update_remote_config | none |

The missing column is not a set of cosmetic conveniences. Starting and stopping
a service, revoking a remote client's certificate, and querying the audit log
are the operations an operator most wants to reach from a script, and the only
way to reach any of them today is to click. ADR-010 built a remote listener and
ADR-008 built an audit log precisely so that relay's behaviour could be
*inspected* rather than asserted; leaving the inspection surface reachable only
from a mouse undercuts both.

Projects escaped this because ADR-004 required it: the native tab and Eve's
dialog had to be co-equal, so both were made to call the same
`Settings.*Project*` mutators. That produced the shape this ADR generalises —
`applyProjectCreate` is the whole behaviour, and `ipcCreateProject` and
`POST /api/projects` are two envelopes around it. The envelopes are duplicated
and the code says so out loud (*"mirrors the HTTP POST route"*,
*"same pattern as the HTTP route"*). Duplicated envelopes are the cheap half of
the problem. The expensive half is the 18 capabilities that were never given a
second envelope at all, because nothing forced the question.

### Why this is not a re-routing job

The IPC layer is not request/response. It is fire-and-forget with event
fan-out:

```go
func ipcCreateProject(ctx *IPCContext, raw json.RawMessage) {
    ...
    ctx.UI.EmitEvent("onProjectAdded", marshalForUI(created))   // no return value
}
```

There are 31 such events. A caller gets no status, no body, and no correlation
between a command and the `onProjectError` that may or may not answer it — the
error is broadcast to whoever is listening, and the sender identifies its own
failure by inference. This is sound when the WebView is the only client and
shares the process: there is exactly one listener, so a broadcast *is* a reply.
It stops being sound the moment a second client exists, which is the entire
point of the change.

So the work is not moving handlers between transports. It is giving 27
capabilities a result, and demoting the 31 events to what they should always
have been: notification that state changed, addressed to every connected view,
carrying no command outcome.

## Decision

**The HTTP API is the only front door. The view is a client of it with no
privileged access.**

Four parts.

### 1. One core per capability, no behaviour in an envelope

Every capability is a function over `*Settings` and the process registry that
returns a value and an error. `applyProjectCreate` is the existing example;
`ServiceOps` is the first one built deliberately. Envelopes decode, call, and
encode. A rule that makes this checkable: **an envelope may not contain a
branch that changes what happens** — only one that changes how the result is
spelled. Validation that decides whether an operation is legal lives in the
core, not beside each door, because a check in an envelope is a check the other
door does not have.

### 2. Commands answer their caller

Every capability becomes a route returning a status and a body. A failure is a
4xx/5xx with a reason, not an event. The event stream stops carrying command
outcomes entirely.

### 3. Events are change notifications, not replies

The surviving stream answers one question: *what changed?* Any connected view
re-reads what it needs. This is what makes multiple clients coherent — a
service started from `curl` must move the tray menu and any open view, and
under the current design it cannot, because the notification is a direct
consequence of the IPC call rather than of the state change.

`onServiceStatusBatch` shows what the current shape costs. It is a 2-second
poll pushed by evaluating JavaScript into the WebView, with a digest compared
on every tick to suppress redundant repaints (`lastStatusBatchDigest`,
`trayapp.go`). On an API that is a `GET` and a stream, and the digest exists
only as long as the push does.

### 4. The native residue is named, not wished away

Some capabilities can be *triggered* by an API call but cannot be *implemented*
behind one. `Relay.entitlements` gates Calendar, Contacts and Microphone behind
hardened-runtime declarations, and TCC prompts must be fired from a signed,
`LSUIElement` app bundle by `cocoa_request_tcc_*` (ADR-005). The tray and menu
are how a background daemon is reachable at all. Roughly 500 lines of
Objective-C stay, and this ADR does not pretend otherwise. What changes is that
they stop being the *only* way to reach anything.

### Three capabilities are deliberately NOT routes

Implementation found three that can be *triggered* by an API call but not
sensibly *implemented* behind one. Each stays on the IPC path, with its logic
in the ops core so only the desktop side effect lives in the envelope:

| capability | why it stays native |
|---|---|
| `reveal_audit_log` | opens Finder. A caller producing a window on someone's desktop is not an API. HTTP exposes the log's PATH (`GET /api/audit/log`) instead. |
| `authenticate_mcp` | runs an OAuth flow that opens a browser and needs a local callback listener. Half-exposing it — a route returning an authorization URL — invites someone to finish the job badly. |
| `reset_mcp_permissions` | fires TCC prompts from relay's bundle and bumps the app from `.accessory` to `.regular` to do it. Desktop-bound by construction (ADR-005). |

These are section 4's native residue, made concrete. A test pins the absence of
the MCP routes so the exclusion is a decision rather than an oversight.

### The sharpest edge this API now exposes

`POST /api/mcps` with a stdio transport takes a **command relay will execute**.
That is remote code execution by design on this surface. It is not new — the
Settings UI has always done it — but it was previously reachable only from a
process already inside the trust boundary. It is now reachable by anything
holding the frontend bearer, which is a single credential granting everything.

`POST /api/mcps` with an HTTP transport is the second edge, and it was
mis-described during implementation as passing an SSRF guard. It does not:
`validateMcpURL` checks the scheme is http/https and that a host is present,
and nothing else. A caller therefore chooses an address relay will connect to
— including link-local metadata endpoints and RFC1918 hosts. That was true
before this ADR, reachable from the Settings UI; it is now reachable by
anything holding the frontend bearer.

`PUT /api/remote` is the same shape one step removed: it sets the mTLS
listener's bind address, and `validateRemoteListen` deliberately permits
non-loopback (that is how real remote clients connect). The API can therefore
open a network listener.

Neither is an argument against the migration. Both are the concrete reason the
authorization follow-up is blocking rather than cosmetic: a uniform admin token
in front of a capability set that includes "run this command" is not a boundary.

### What this ADR does not decide

**Authorization is deferred, and the deferral is the risk.** The IPC path has
no authentication because it is in-process and the WebView is trusted by
construction. Every capability moved onto HTTP inherits the frontend bearer
token, which is a single credential granting everything — adequate for Eve
today and *not* adequate as the model for a control plane that can revoke
enrolments and stop services. Relay's whole argument is that its boundaries
hold; a uniform admin token across a uniform admin API is a boundary that has
been flattened rather than drawn.

The migration proceeds anyway, because the API shape and the credential model
are separable and getting the shape wrong is more expensive to undo. It carries
one guard in the meantime: **any listener beyond the 0600 Unix socket is opt-in
and defaults to absent** — the same rule ADR-010 applied to the remote listener,
for the same reason. A loopback TCP bind for browser access exists only when
configuration explicitly asks for it. A follow-up ADR must settle authorization
before that bind is anything but a devbox convenience.

## Consequences

**Migration order.** Services, then enrolments, then audit, then MCP mutations,
then collapsing the duplicated project envelopes, then the view switches to
`fetch()`. Each slice ships independently with the app working; the view moves
last and once.

**Cost.** Roughly 1,500–2,000 lines of handler code for the 18 uncovered
capabilities, against deleting most of the 1,780-line IPC layer and its ~1,860
lines of tests. Net negative on lines. That is a side effect, not the argument.

**What it buys.** CLI and remote parity arrive as a consequence rather than as
separate work: `RemoteServer` already exists, so a capability on the frontend
API is reachable over mTLS the moment it is written. Tests assert against
request and response rather than against emitted-event sequences and UI
payloads, which is why `project_ui_payload_test.go` and
`settings_*_ui_test.go` shrink rather than move.

**The unlocked capabilities are administrative.** `POST /api/services/{id}/stop`
and `DELETE /api/enrolments/{id}` are the operations an attacker who reaches the
API most wants — stopping the audited path, or revoking the enrolment that
records them. They are not new powers; they were always available to anyone who
could click. But the reachability of a power is part of its threat model, which
is the whole reason the authorization question above is a blocking follow-up and
not a cleanup.

**What gets worse before it gets better.** During migration a capability has two
doors and one core. That is strictly better than two doors and two cores, and
strictly worse than one door. The window is bounded by the migration order
above; leaving a capability with two doors permanently is the failure mode to
watch for, because it is comfortable and it is how the current state was
reached.

## See also

- [ADR-004](004-project-mgmt-in-relay.md) — the two-envelopes-one-core shape,
  arrived at for projects alone. This ADR generalises it.
- [ADR-005](005-tcc-permissions.md) — why an app bundle is load-bearing and the
  native residue cannot be removed.
- [ADR-008](008-tool-call-audit-log.md) — the audit log this ADR makes queryable
  over HTTP.
- [ADR-010](010-remote-client-transport-and-identity.md) — the opt-in,
  absent-by-default listener rule reused here, and the mTLS path that inherits
  every new capability.
