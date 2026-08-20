# ADR-008: Tool-call audit log at the router chokepoint

**Status:** Accepted
**Date:** 2026-08-19

## Context

Relay brokers every MCP tool call in the ecosystem but kept no record of them.
The security model was entirely preventive: a project token scopes what a caller
may reach (ADR-007), `checkToolAccess` enforces it, and that was the end of it.
There was no way to answer, after the fact, what an LLM actually did — which
files it read, which tools it was refused, or which process asked.

That gap becomes load-bearing with the planned remote client: a machine outside
this host reaching relay for tool access is exactly the situation where "we
prevent the wrong calls" is not a sufficient answer on its own. Detection has to
exist before the attack surface widens.

Three properties had to hold:

1. **Complete.** No transport may be able to reach a tool without being logged.
2. **Attributed.** Each record must name the project and the calling process
   from relay's own knowledge, not from anything the caller asserts.
3. **Harmless.** Adding a log must not be able to break, slow, or fail a tool
   call, and must not itself become a new place secrets accumulate.

## Decision

### One instrumentation point

Log at `appRouter.CallTool`. Every path — the `relay mcp` stdio server,
`relay mcp call`, relayLLM's MCP client, project shells, and any future remote
client — funnels through the bridge into that method, and it is also where auth
is resolved and the permission check is made. Instrumenting anywhere else would
mean either duplicating the logic per transport or logging before relay knows
who the caller is.

`ListTools` and `ListSkillBuckets` are instrumented the same way, off by
default (`audit.log_lists`).

### Refusals are events

A `denied` (credential refused a tool) or `unauthorized` (credential did not
resolve) record is written with the same reliability as a success. The naive
shape of this feature logs completed calls and silently drops the refusals,
which are the records a security review is looking for.

### Attribution from relay and the kernel, never the caller

The project id comes from the resolved `StoredToken`, the same value relay
injects into `_meta.project_id`. The caller's pid is read off the Unix socket
with `getsockopt(LOCAL_PEERPID)` in the bridge server and carried in the request
context; process and parent names come from `proc_pidinfo`. Nothing self-reported
is trusted, with one marked exception: the working directory recorded for a
directory-auth grant, which is caller-asserted by construction and already
documented as such in `docs/tokens.md`.

The pid is attribution only and is never consulted for authorization. Pids are
reusable and racy — adequate for "who called this", not for "may they call it".

### Redacted arguments, metadata-only results

Arguments are recorded with credential-like keys redacted by case-insensitive
substring match, capped at 4 KiB. Results are recorded as size and `isError`
only, with a capped preview available behind an explicit opt-in.

The asymmetry is deliberate. Arguments are what the model *asked for* and are
the point of the log; results are what came back, and tool results carry file
contents, mail bodies, and calendar entries. Logging them by default would turn
the audit log into the highest-value file on the machine and quietly undo the
scoping the project token exists to provide.

### Fail open, and say so

Events cross to a single writer goroutine over a bounded channel. A full queue
drops the event and increments a counter; a broken sink at startup means relay
runs with auditing off. In both cases the state is surfaced in the Tool Calls
tab rather than hidden.

Fail-open is correct for the local case — an audit log that can wedge every
agent on the machine when a disk fills is a worse failure than a gap in the
record — but it is only defensible because the gap is *visible*. An incomplete
log that looks complete would be worse than no log at all.

### Nil recorder is the no-op

Instrumentation goes through nil-safe methods on `*auditCall`. A router built
without a recorder behaves exactly as before, and `router.go` carries no
`if audit != nil` branching.

## Consequences

- Relay gains a new persistent artifact under `<config-dir>/logs/audit/`,
  0600, size-rotated with several generations. `rotatingWriter` grew a
  `generations` field; existing callers pass 1 and are unchanged.
- `bridge.ToolRouter` did **not** change. Peer pid rides in the context, the
  same technique the caller-asserted cwd already used, so cross-repo
  implementers are unaffected.
- The audit log is a new confidentiality surface. It is 0600 and holds redacted
  arguments and no results by default, but "grep the audit log" is now a way to
  learn what a project has been doing, and `audit.max_result_preview_bytes`
  should be turned on deliberately and turned back off afterwards.
- Redaction is heuristic. A secret passed under a key that does not look like a
  credential (`"value"`, `"input"`) will be logged. The cap bounds the exposure;
  the heuristic does not eliminate it.
- Fail-open means the log is not admissible as proof that something did *not*
  happen. A future `audit.required` mode for remote callers is where that
  guarantee gets bought, at the cost of refusing calls that cannot be recorded.

## See also

- `docs/audit-log.md` — operator-facing reference.
- ADR-007 — the token brokering model the actor attribution rests on.
- `docs/tokens.md` — the credential inventory, including directory auth.
