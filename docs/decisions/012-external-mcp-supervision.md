# ADR-012: An external MCP child is supervised, and an over-long frame is one bad answer

**Status:** Accepted
**Date:** 2026-08-26

## Context

An external stdio MCP is a child process relay spawns and talks JSON-RPC to over
a pipe. One connection per MCP **id**, shared by every access profile that names
that MCP — which is the right shape (a filesystem MCP is a filesystem MCP
whoever is asking) but means its failure mode is shared too.

Relay spawned those children and never looked at them again. `readLoop` read
stdout until the stream ended, failed every pending call, and stopped. Nothing
watched for the goroutine's exit, nothing restarted the child, and nothing said
anywhere that the capability was gone. Whatever killed a child — a crash, an
OOM, an operator's `kill`, a machine waking from sleep with a stale pipe — the
MCP was down until someone relaunched Relay.app.

Issue #39 found the cheapest route to that state. `readLoop` capped a line at
`bridge.MaxMessageSize` (10 MiB) with a `bufio.Scanner`. The cap is right: child
stdout is untrusted and an unbounded read lets one child OOM the tray. But a
scanner that hits `bufio.ErrTooLong` is finished — the error is terminal and the
reader is left in the middle of the offending line — so a single over-long
response ended the connection. In the reported reproduction, one oversized
`fs_read` on one access profile simultaneously killed four unrelated enrolments
on four unrelated profiles, and every subsequent call of every tool from every
client returned `read response: EOF` until the app was restarted.

Three defects, and they are separable:

1. An over-long frame is treated as fatal to the connection. It answered one
   request badly; it is not evidence the child is broken.
2. Nothing respawns a dead child, for any cause. This is the load-bearing half:
   an MCP process is a supervised child, and supervision that never restarts is
   just spawning.
3. A dead MCP is invisible. The only signal is every client getting
   `read response: EOF` — the report this project's own docs say to trust last.

While instrumenting (2), a fourth thing fell out. `finalizeConnection` published
the connection into `m.conns` **first**, then set the discovered tools, then
stored the context schema. A connection reachable through `m.conns` is one the
router will dispatch to, and `appRouter.CallTool` decides what a call is
confined to from that schema. `ParseContextSchema(nil, 0)` is not a narrow
schema, it is *no* schema: `checkScopePresence` finds no field to require and
passes every tool, `filterKnownContextFields` finds nothing declared and strips
every stored context key off `_meta`. So there was a window, on every start and
on every reload, in which an MCP was callable and every call to it ran as though
the grant were empty. A spinning reader observes it on the first start, every
run — it is not a theoretical race. Respawn would have multiplied that window by
the number of restarts, which makes fixing it a precondition of (2) rather than
an aside.

## Decision

### 1. An over-long frame fails one call and the stream resyncs

`bufio.Scanner` is replaced by `readMcpFrame`, a bounded reader over
`bufio.Reader.ReadSlice`. The cap is unchanged and is not negotiable. What
changes is the response to breaking it: the frame's bytes are **counted and
discarded as they arrive**, never buffered, and the reader is left positioned on
the next newline — which is a frame boundary by definition, so the following
response is read intact.

Resync correctness is the whole point here. Reading the tail of a discarded
frame as though it were a message would hand a fragment to `json.Unmarshal`, and
the frame after it is a real response somebody is waiting for. Discarding to the
next newline is the only resync that is sound without parsing the payload, and
it is exactly what the framing already guarantees.

### 2. The discarded frame's call is named from its prefix, or not at all

The first 64 KiB of an over-long frame is kept so the call it answered can be
failed immediately with an error that says what happened and what to do:

> response of 11534361 bytes exceeds relay's 10485760-byte per-message limit and
> was discarded; ask for less at a time, or have the tool page, stream, or
> return a reference instead of the whole payload

The id is recovered by a **token walk that accepts an id only at the top level**
of the object (`peekFrameResponseID`). A substring search for `"id":` would find
one inside the very result that made the frame oversized and fail an unrelated
in-flight call, which is the one outcome here worse than not attributing the
frame at all. A JSON-RPC response conventionally puts its id beside `jsonrpc`
and ahead of `result`, so the prefix carries it in practice; when it does not,
the walk runs off the end of the truncated prefix and reports nothing, the frame
is dropped, and that one call falls to its own timeout. An honest miss, still
bounded, and still only that one call.

### 3. A stdio child is supervised, and supervision restarts it

Every stdio MCP gets an `mcpSupervisor`: a goroutine that waits on
`readerDone` and brings the child back. It exits on exactly three things — the
supervisor being cancelled (`Stop`, `Reload`, `StopAll`, app shutdown), being
superseded by a newer supervisor for the same id, and spending its restart
budget. It never exits because the child died.

The supervisor is the *unit of ownership* for an id, and it is compared by
**identity, not presence**. A respawn publishes its connection only if it is
still the supervisor of record, so a restart that raced a `Stop` or a `Reload`
closes the child it just spawned instead of installing one nothing owns.
`Stop` cancels the supervisor **before** killing the child, so the death it is
about to observe reads as "an operator stopped this" rather than as an incident.

The supervisor's context is rooted at `Background`, not at whatever context the
caller was holding. A start arrives down four routes and only one carries the
app's lifetime: `StartAll` gets it, but `Reconcile` and `Reload` are bridge
requests, and `bridge.BridgeServer` hands each handler a **per-connection**
context cancelled the moment the client disconnects. `relay mcp register` is
one such client and it exits immediately, so a supervisor derived from that
context would be dead before the MCP it was meant to watch finished starting —
supervision that silently applied to some MCPs and not others, decided by which
command last touched them. The manager's own lifecycle is the right owner and
already exists: `Stop`, `Reload` and `StopAll` each end supervision explicitly,
and the tray calls `StopAll` during cleanup.

The supervisor holds a **copy** of the settings entry taken at install time. It
restarts the child it was told to start; picking up an edited command on a
respawn would make a settings change take effect at a moment nobody chose.
`Reload` is how a new command reaches a running MCP, and it installs a new
supervisor.

### 4. A respawn is a full start, and there is only one code path for it

`connectStdio` spawns, runs the handshake, runs the context-schema discovery
that comes with it, and publishes — in that order, once, for the first start and
for every respawn alike. There is deliberately no second entrypoint that skips a
step. A respawned MCP that served calls before its schema was known would be a
worse bug than the outage this ADR exists to fix, because it fails *open*: the
call runs, unconfined, and the audit records no scope.

### 5. The connection is published last, and never without its schema

`finalizeConnection` installs the tools on the connection while it is still
unreachable, then writes the schema and publishes the connection **in the same
critical section**. No reader holding `m.mu` can observe one without the other;
the fail-open window described above is not narrowed, it is unrepresentable.

The schema is **replaced, never merged** — deleted first and rewritten only if
this handshake carried one. The respawn path does not go through `Stop`, so
without the delete a child that stopped declaring a schema (a downgraded build,
a server that failed to send it) would leave relay enforcing its predecessor's
declaration against a process that no longer honours it. This is the same class
of split state ADR-011's `storedSurfaceLocked` note describes, reached by a new
route.

### 6. The budget caps restart *intensity*, not lifetime restarts

Backoff is exponential from `MCPRestartBaseDelay` (250 ms), doubling to
`MCPRestartMaxDelay` (30 s). There is a delay before the **first** attempt too:
a child that dies on spawn would otherwise be respawned in a tight loop for as
long as the budget lasted.

`MCPRestartMaxAttempts` (8) bounds one crash-loop streak, and a child that stays
up for `MCPRestartStableWindow` (2 min) **resets the counter**. That is the
difference between capping how fast relay gives up on a child that will not stay
up and capping how many times it will ever restart a healthy one. An MCP that
dies once a week is recovered forever; one that dies on every spawn is abandoned
after a bounded number of tries and says so loudly.

Abandonment is not permanent. `Reconcile` restarts an MCP whose connection is
dead with no supervisor left to bring it back (`needsStartLocked`), so any
settings change installs a fresh supervisor — as does an explicit `Reload`.
Without that, "abandoned" would mean the same relaunch-the-tray recovery this
ADR exists to remove, just further down the road.

All four are `var`, not `const`, for the same reason `MCPRequestTimeout` is one
(ADR-002): the supervision tests have to drive a crash loop to its cap without
waiting out the real backoff. Nothing in production writes them.

### 7. In-flight calls are failed, never replayed

`readLoop` has already failed every pending request on the dead connection
before the supervisor sees the death, and the supervisor does not re-send them.
A tool call is not idempotent and relay cannot know whether the child sent the
mail before it died. The caller sees the failure and decides. **Relay restores
the capability, not the call.**

A call that is issued during the outage window fails against the dead connection
with its reader error rather than being queued: the dead connection is left in
`m.conns` until a successful respawn replaces it atomically, so the MCP's tool
list and context schema — which project validation and grant scoping read —
stay stable across a restart instead of flickering out of existence.

### 8. Supervision is audited, which is the one place ADR-008's remit widens

`mcp_down` and `mcp_up` join `call_tool` / `list_tools` / `list_skills` as event
kinds, with a new actor kind `relay` — the only records relay writes about
itself rather than about a caller, so every caller-derived field is absent
rather than zero-filled. A new `supervision` field names the transition
(`down`, `restarted`, `abandoned`) beside the `error` that caused it, and
`dur_ms` on the closing row is the **outage length**, which is the question
these rows exist to answer: not "did it flap" but "for how long was every grant
naming this MCP dead".

This is a deliberate widening of ADR-008, which scoped the log to what a
credential attempted. It is justified by what the log is *for*: an operator is
told `relay audit` is the ground truth for anything relay gates, and a dead MCP
is precisely the state in which every gated call fails for a reason that has
nothing to do with the grant. Without these rows the log shows a run of `error`
outcomes and no cause.

A failed individual restart attempt produces **no** record — it is a step inside
an outage the `mcp_down` row already opened, and one line per retry would bury
the two lines that bound it. Every attempt is in the app log regardless.

## What we deliberately did NOT do

- **No supervision of an MCP that never started.** A child that failed its first
  spawn or its first handshake is a configuration error, not a supervised
  process that died, and it is already logged loudly as one. The supervisor is
  retired when the first connect fails. A settings change or a `Reload`
  restarts it, as it always did. Restarting a mistyped command on a backoff
  forever would turn a legible startup error into a recurring one.

- **No supervision on the HTTP transport.** There is no child process to
  restart. An HTTP MCP's failures are per-request and its session is
  re-established by the transport; `finalizeConnection` takes a nil supervisor
  on that path.

- **No tray-UI health indicator.** The MCP list has no liveness surface at all
  today (`IsConnected` has no production caller), so this would be a new feature
  rather than a fix, and it lands in a 218 KB committed JS bundle with a regen
  step. The audit log is where this project's own docs send an operator for
  ground truth, and that is where the fact now is. The seam for the UI half is
  already in place: `SetHealthObserver` takes any observer, and a second one can
  push to the settings window without the manager learning what a WebView is.

- **No per-profile connection.** Issue #39 asks whether a per-MCP connection
  should be shared across access profiles at all, given its failure mode is
  shared. Not here: N profiles times M MCPs of child processes multiplies TCC
  prompts, memory, and spawn cost, and the sharing is not what made the outage
  permanent — the missing supervision was. With a supervisor, a shared
  connection's worst case is a bounded outage all profiles recover from
  together.

- **No replay of failed calls.** See decision 7.

## Consequences

- **Good:** The reported outage is bounded by a backoff instead of by an
  operator noticing. An over-long frame costs one call.
- **Good:** The fail-open publish window is gone from the start path as well as
  the respawn path, and it was reachable on every launch.
- **Good:** `relay audit --event mcp_down` answers "was this MCP up when that
  call failed?" from the file, without correlating app-log timestamps.
- **Trade-off:** A dead connection stays in `m.conns` during an outage, so a
  call issued in that window fails with the reader error rather than a crisper
  "not connected". The alternative — removing it — churns the MCP surface that
  grant validation and project scoping read, which trades a clearer message for
  a worse failure mode.
- **Trade-off:** A child that is killed deliberately by something *other* than
  relay (an operator's `kill` on the MCP process) is now restarted. That is the
  intended behaviour and it is what "supervised" means, but it does mean an
  operator who wants an MCP to stay down must stop it through relay.
- **Trade-off:** `readMcpFrame` allocates per frame where the scanner reused one
  buffer. Frames are small and short-lived; the alternative is a shared buffer
  whose lifetime crosses into the progress-delivery goroutines.

## See also

- ADR-002 — production test seams (why the restart knobs are `var`)
- ADR-008 — the tool-call audit log this widens by two event kinds
- ADR-011 — resource scope; decision 4 on why an absent schema is not a narrow
  one, and `storedSurfaceLocked` on schema/version as one fact
- `external_mcp.go` — `readMcpFrame`, `peekFrameResponseID`, `mcpSupervisor`,
  `connectStdio`, `finalizeConnection`
- `timeouts.go` — `MCPRestart*`
- `external_mcp_supervision_test.go`, `external_mcp_oversize_test.go`,
  `external_mcp_publish_order_test.go`
- barelyworkingcode/relay#39; fsMCP's half is barelyworkingcode/fsmcp#19
