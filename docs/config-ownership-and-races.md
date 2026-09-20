# Configuration ownership and race reduction

## Decision in one paragraph

Relay should own its configuration completely. The tray process should be the
long-lived configuration manager. Every normal read and write should go through
one in-process configuration boundary, and every write should be serialized in
that boundary. Other Relay commands and components should ask the tray for a
snapshot or submit a command; they should not open `settings.json`, keep their
own store, or make decisions from a stale cache.

The idea is good. It removes the hardest class of bugs: two processes doing
read-modify-write against the same file. But a mutex around the file is not the
final design. A mutex protects persistence; it does not stop callers from
reading an old snapshot, doing work based on it, and acting after the
configuration has changed. The target should be a small configuration actor
inside the tray: one owner of the current state, one ordered command queue, and
immutable snapshots for readers.

Do not make the whole application single-threaded. Network calls, subprocesses,
OAuth, UI work, and listener I/O must remain concurrent. Only configuration
state transitions and the short persistence step belong on the configuration
lane.

## Implementation progress

This proposal is being implemented on the `feat/config-command-queue` branch.

Current decision: use a FIFO command queue for configuration mutations and the
dependent runtime action that must be ordered with them. The caller waits for a
result. Ordinary reads use the latest committed snapshot. FIFO ordering is the
ordering mechanism. A command that does its lookup, validation, persistence and
ordered side effect entirely inside one queued step needs no revision check.

Revision or generation checks are needed only where work must leave the queue
between reading state and committing a result. Host probes are the precedent:
they reserve and commit a per-host probe generation on the queue
(`internal/config/settings.go`, `host_ops.go`), so a stale probe result is
dropped. The same rule applies to a presence approval that cannot run inside
the queued step. Service-owned config file edits need no check: they run
entirely inside one queued step. Revisions for UI
freshness are optional.

A global monotonic revision counter and a `ConfigManager` snapshot API are not
part of the current scope. They appear below only as a possible future
direction.

Lane-duration assumption: queued steps are expected to be fast, well under a
second. Anything that waits on a human (a presence prompt), the network, or an
unbounded subprocess wait must not hold the lane.

### Completed on this branch

- The running tray is the only reader of configuration for normal CLI
  commands. `credential list`, `service list`, `mcp list`, `login list`,
  `eve list`, `enrol list` and `grant` are admin ops (`credential.list`,
  `service.list`, `mcp.list`, `login.list`, `eve.list`, `enrolment.list`,
  `grant.view`) that answer with a purpose-specific projection of a fresh
  snapshot (`config.FreshSettings`), never a Settings value, so no hash,
  sealed value, environment value or key coordinate can cross the bridge.
  They are not presence-gated and do not use the config lane: one snapshot is
  a consistent read. `service unregister/restart` and `mcp unregister`
  resolve `--name` inside their own op, so no stale CLI-side read feeds a
  mutation. With the tray stopped these commands refuse by name, as the
  mutating ones do; there is no offline read mode.

- Sealed-store reset runs its destructive sequence (deletes, keychain destroy,
  `Reresolve`, `EnsureInitialized`) as one queued step after the presence
  prompt, which stays off the lane. The step re-checks the approved key ids
  against the live ones and refuses with `errSealedKeysChangedDuringApproval`
  ("sealing keys changed during approval, retry"; the tray logs it), deleting
  nothing.

- Added `internal/config.CommandQueue`, with bounded admission, FIFO execution,
  caller waiting, cancellation, shutdown, panic recovery, and deterministic
  tests.
- Wired one queue into the tray's `ServiceOps`.
- Service create/update/remove/autostart/start/stop/restart now run as complete
  queued operations, including persistence and process effects.
- `service register` now decides create versus update inside the queue instead
  of reading first in the CLI bridge handler.
- Router restarts and tray service toggles use the same queue.
- Credential mint and revoke now use the same queue, including their required
  issuance record, so the caller does not return before the mutation and its
  audit side effect complete.
- MCP add/remove and the OAuth-start state write use the same queue. Discovery,
  browser flows, and bridge notifications remain outside the queue.
- The broker's OAuth token-refresh persistence also goes through
  `McpOps.PersistOAuthState` on the queue. The refresh callback runs on a
  request goroutine, never inside a queued step: no queued step calls into the
  MCP manager's network paths, so the callback cannot wait on the lane it is
  already holding. A refresh for an MCP removed in the meantime is dropped
  instead of written.
- Project create/update/remove/token-rotation mutations now use the same queue;
  project skill cleanup and session cleanup remain after the committed delete.
- Login bootstrap mint, passkey revoke, and browser-session sign-out now use the
  same queue, including their required issuance records.
- Enrolment create, sign, approve, update, revoke, refusal, and remote-config
  mutations now use the same queue. Their settings, issuance/audit work, and
  approval payload resolution complete before the caller returns.
- Remote-config mutation runs on that queue; its presence decision follows the
  approval rule below.
- Eve enrolment-window open/consume and Eve passkey report/revoke/unrevoke
  mutations now use the same queue, including persistence and audit effects.
- Host create, update, remove, and probe reserve and commit persisted probe
  generations on the queue; SSH discovery remains outside it, and stale probe
  results cannot overwrite a newer connection shape or a removed host.
- Project update approval is re-checked against the live record inside the
  queue. The prompt stays outside the lane; the queued step recomputes
  `UpdateWidensGrant` on the live record and, when it names a field the
  approval did not cover, commits nothing and returns
  `errProjectChangedDuringApproval` (HTTP 409, IPC `onProjectError`).
- Terminal template create, update, and remove now use the same queue; the
  Settings window (IPC) and the HTTP routes share one `TemplateOps`.
- Service create/register/update and remote-config set no longer hold the lane
  during their presence prompt. The rule, stated once: a presence prompt never
  runs on the lane; approval is obtained outside the queue from a snapshot, and
  the queued commit re-derives the decision against the live record and refuses
  on divergence. The refusals are `errServiceChangedDuringApproval` and
  `errRemoteConfigChangedDuringApproval` (HTTP 409, IPC and CLI surface the
  message): nothing is written, the caller retries. A stale snapshot that
  over-prompted is tolerated. `service register` decides create versus update
  again inside the queue; both bind the same digest, so an approval for either
  covers the other, while an unprompted update whose record vanished (now a
  create) is refused.
- The login HTTP routes (passkey registration with bootstrap-code consumption,
  its undo when the issuance record cannot be written, and the sign-count
  update, reap and session-credential mint on assertion) write through
  `LoginOps` on the same queue. Each write is one short queued step; the
  issuance record is still written after the mint and before the token leaves.
- The IPC project disabled-tools toggle now runs through
  `ProjectOps.SetDisabledTools` on the queue; the generic IPC
  `withSettings`/`withSettingsNotify` helpers, which wrote unqueued, are gone.
- A structural test (`cmd/relay/config_queue_structural_test.go`) parses the
  production `cmd/relay` sources and fails on any function that calls
  `config.WithDeclinable` or `store.With` without submitting to the queue
  (`runQueued`, `runCommitted` or `Queue.Do`), unless it is on a small explicit
  allowlist (startup, and helpers that run inside their caller's queued step).
  It also asserts it still finds known queued writes, so it cannot pass empty.
- The service config editor's save runs through `ServiceOps.SaveConfigFile` as
  one queued step: it re-resolves the live service record, the allowed root and
  the path, validates, writes the file, and restarts only a service that is
  running at that moment (`ApplyMode: live` writes only). A save queued behind a
  remove, stop or working-directory update sees the result of it; two saves
  restart in admission order. A restart failure after the write wraps
  `errServiceProcess`, so "written, restart failed" stays distinct from
  "nothing written". The editor has no HTTP route.

The queue migration and the scoped tray-mediated CLI read migration are
implemented. The status below is authoritative for the remaining ownership
work. It deliberately records partial areas instead of claiming completion
from passing race tests alone.

### Status summary

| Area | Status | Meaning |
| --- | --- | --- |
| Queued mutations | Mostly complete | The major settings mutation domains now use the tray queue, including ordered service effects and the documented generation checks. |
| Normal CLI reads | Complete for the scoped commands | The listed commands use tray admin reads and refuse when the tray is stopped. |
| Direct production reads | Complete with named exceptions (tasks 2–3) | `config.FreshSettings` and `config.DisplaySettings` are the normal read paths. `cmd/relay/settings_read_boundary_test.go` rejects direct `Get`/`Reload`/store construction outside startup seams; `TestDecisionReadsSeeACommittedChangeWithoutWaitingForThePoll` covers freshness. The scan is a guardrail, not proof against indirect aliases or helper paths. |
| Single tray ownership | Complete (task 1) | `config.AcquireTrayOwnership` takes a non-blocking `flock` on `tray.lock` before store and bridge setup; a second tray fails with `ErrOwnedByAnotherTray`; cleanup releases it. Tests cover release, simultaneous startup, and setup ordering. |
| Post-commit events | Complete with documented error rule (task 4) | `CommandQueue.SetCommitObserver` publishes once when the store commit counter advances, before the caller is released. Failure before persistence publishes nothing; persistence followed by a later error still publishes because committed state changed. The focused tests cover both cases. Legacy callbacks remain only for runtime-only or bridge-driven changes. |
| Event-driven convergence | Complete for the normal path (task 5) | `statusPoller` no longer reads `settings.json`; commit events drive UI, listener, and model-endpoint reconciliation, with a 30-second recovery reconciliation. The recovery poll is not a normal settings-change trigger. |
| External writers | Complete with documented recovery limitation (task 6) | Policy is import through `config.WatchSettingsFile` and `FileSettingsStore.ImportFile`; invalid or unsealable edits are rejected without replacing current state, queue admission defines ordering, and stopped-tray command semantics are documented in `docs/cli.md`. A missed kqueue event is not recovered until a later file event or restart; this is an explicit limitation, not a second normal reader. |
| Ordering and freshness tests | Partial (task 7) | Focused tests cover queue commits, direct-file import, ownership startup, restart-behind-remove, reverse-order service updates, and selected HTTP/IPC/tray paths. MCP/enrolment cross-door coverage and several listed listener/delete/reset/concurrent-mutation cases remain. A clean `go test ./...` and `go test -race ./...` have not been established in this checkpoint. |
| Completion | Incomplete | Tasks 1–6 are complete with the documented limitations above. Task 7 remains partial because several deterministic ordering cases and repository-wide verification are still missing. |

### Outstanding task breakdown / resume next

These are the remaining acceptance items for the full migration. They are
ordered by dependency, not implementation size. Tasks 1–3 are retained below
as historical acceptance descriptions; their current status is Complete in the
table above.

1. **Enforce one tray owner — Complete.** Add an exclusive per-config-dir ownership lock
   before opening the writable store or removing the bridge socket. A second
   tray must fail clearly and the lock must remain held for the tray lifetime.
   Add a deterministic startup-race test.

2. **Finish the production-read audit — Complete with named exceptions.** Classify every production
   `SettingsStore.Get()` and `Reload()` call as display-only, authorization or
   routing, mutation precondition, lifecycle convergence, or explicitly
   isolated recovery. Replace the non-display cases with snapshots or
   purpose-specific queries. The target is zero direct cached reads outside
   the config implementation and documented test seams.

3. **Make the read boundary enforceable — Complete for the current syntax guard.** Extend structural checks beyond
   writes so new production callers cannot introduce direct store reads or
   construct independent settings stores. Keep narrowly documented startup and
   recovery exceptions.

4. **Add a universal post-commit event — Complete with documented error rule.** Have every queued
   configuration command publish exactly one event when persistence succeeds;
   a command that fails before persistence publishes none, while a later error
   after persistence still publishes because committed state changed. Move UI
   refresh and reconciliation triggers toward that event rather than
   operation-specific callbacks.

5. **Replace polling as the normal coordination path — Complete for the normal path.** Use the post-commit
   event to trigger UI refresh, remote-listener reconciliation, model-endpoint
   reconciliation, and service configuration refresh. Retain only a slow
   recovery/health poll for missed events, crashed children, and external
   repair.

6. **Resolve the external-writer policy — Complete with documented recovery limitation.** Either reject live edits to
   `settings.json` while Relay owns the configuration directory, or provide an
   explicit import/repair command with clear stopped-tray semantics. Manual
   edits must not continue as an undocumented second writer.

7. **Complete ordering and freshness tests — Partial.** Add deterministic coverage for
   listener rebinds, service delete versus delayed start/update, reverse-order
   external work, second-tray startup, reset versus write, failed persistence,
   restart snapshot consistency, and direct-file edits. Run focused race tests
   plus `go test -race ./...` after the remaining phases.

### Verification so far

- `go test ./internal/config` passes.
- `go test -race ./internal/config -run 'TestCommandQueue' -count=10` passes.
- `go test -race ./cmd/relay -run 'TestServiceOpsRace'` passes.
- `go test ./cmd/relay -count=1` passes.

The repository-wide pass was run before the structural-test adjustments and
failed only in the command package's gate-name expectations and malformed IPC
fixture; those failures were fixed, then the full `cmd/relay` package passed.

### Current checkpoint

The current branch contains the queue, ownership lock, read boundary, commit
observer, listener convergence, and file-import foundation. The post-commit
error rule and stopped-tray import semantics are settled and documented. The
next bounded work is the missing deterministic ordering coverage and a
repository-wide verification run; the missed-file-event limitation remains
explicit rather than silently treated as normal coordination. The focused
verification run for this checkpoint was `go test ./internal/config -count=1`
(pass).
`go test ./internal/config ./cmd/relay -count=1` did not complete in the
available run because the `cmd/relay` package entered a long-running test; it
produced no failure output before it was stopped. No repository-wide race
result is claimed here.

Since this checkpoint, the focused event/import/ownership tests pass with:

```text
go test ./internal/config -run 'TestCommitEvent|TestHandEdit|TestSettingsWatcher|TestOwnedStore|TestRestartFirstSnapshot' -count=1
go test ./cmd/relay -run 'TestProductionSettingsReadsAndConstructionStayBehindTheBoundary|TestSettingsBoundaryScanBitesOnEachViolation|TestTrayOwnershipIsAcquiredBeforeStoreAndBridgeSetup|TestCommitEvent|TestMutationsAcrossHTTPIPCAndTrayDoorsSurviveInAdmissionOrderWithOneEventEach' -count=1
```

`prompt.md` is now the shipping handoff for the remaining partial Task 7 work.
It explicitly keeps tasks 1–6 closed, limits new work to deterministic coverage
and final verification, and forbids architectural expansion. Task 7 remains
`Partial` until its listed tests and the repository-wide verification gates
have passed.

## Foundation and rationale

This section records the foundation and design rationale that led to the
current implementation. Where it conflicts with the status summary above, the
status summary is authoritative.

Relay has already made a substantial move in this direction:

- `internal/config/FileSettingsStore` has a mutex around `Get`, reload, and
  writes.
- `WithDeclinable` reloads before applying a mutation and writes atomically.
- The tray is the only production process with a sealing key, so normal writes
  are already intended to be brokered through it.
- Mutating CLI commands such as credential, service, MCP, and enrolment changes
  already use the bridge's `admin_op` path.
- Authorization on the remote path uses `config.FreshSettings`, which avoids
  making an authorization decision from the two-second poll cache.
- The tray poller reconciles the remote and model listeners after configuration
  changes.

That is a good foundation, not a finished ownership model.

The pre-migration design had several competing meanings of “the configuration”:

1. The file on disk.
2. A `FileSettingsStore` cache.
3. A fresh read through `FreshSettings`.
4. A component's copied settings passed into a long-lived operation.
5. A second CLI process' read-only store.

Those views could disagree by design. That was the race surface this work was
intended to reduce.

## Where it fell short before the current checkpoint

The following sections preserve the original problem analysis and rationale.
They are historical context, not a second current-status list; the status
summary and checkpoint above are authoritative.

### 1. The public store API is too easy to misuse

`SettingsStore` exposes `Get`, `Reload`, `ReloadIfChanged`, and `With` to every
caller. The codebase rule says authorization must use `FreshSettings`, but the
production code still has many direct `Get` calls, including in:

- `cmd/relay/model_endpoint.go`
- `cmd/relay/enrolment_ops.go`
- `cmd/relay/service_ops.go`
- `cmd/relay/mcp_ops.go`
- `cmd/relay/project_ops.go`
- `cmd/relay/frontend_model_guard.go`
- `cmd/relay/router.go`
- `cmd/relay/project_routes.go`
- `cmd/relay/ipc_handlers.go`
- `cmd/relay/ipc_service_config.go`
- `cmd/relay/template_routes.go`
- `cmd/relay/eve_passkey_ops.go`

Some of these are harmless display reads. Some choose a record before a
mutation or decide what a listener should do. The API does not make that
distinction visible, so a new caller can accidentally use a cached view in a
security or lifecycle decision.

### 2. The tray is not yet the only normal reader

Several read-only CLI commands intentionally construct a new store and read
`settings.json` while the tray is stopped. The documented examples include
`relay credential list`, `relay grant`, `relay login list`, `relay mcp list`,
`relay service list`, `relay enrol list`, and parts of `relay eve list`.

That is useful operationally, but it means the system still has two
configuration authorities: the running tray's view and an independent disk
reader. A read-only CLI cannot corrupt the file, but it can report a state that
the tray has not observed, or a state that the tray has already superseded in
memory. It also makes it harder to state one simple rule to future contributors.

### 3. The current writer is serialized, but the decision and the effect can be split

`FileSettingsStore.WithDeclinable` serializes the callback and save. It does not
serialize work that happens before or after the callback. The repository already
has a concrete example in `ServiceOps.Start`: process-registry state and config
state are separate resources, so a service can be removed or changed while a
start/restart decision is in flight.

The same shape exists anywhere code does this:

1. Read settings.
2. Do external work or wait.
3. Read settings again or write a result.
4. Apply a side effect based on both observations.

The file mutex cannot solve that. The operation needs an explicit command
boundary (one queued step that reads, validates, persists and applies the
ordered effect), or, where work must leave the queue, a generation check at
commit or a reconciliation loop that treats the current configuration as the
source of truth.

### 4. File polling is being used as a coordination mechanism

The tray's two-second `ReloadIfChanged` poll is doing three jobs:

- noticing an external edit;
- refreshing cached settings;
- triggering listener reconciliation and UI refresh.

That is workable while external edits are supported, but it is an awkward
control plane. It introduces an intentional delay and makes “configuration
changed” an observation rather than an event. If the tray owns configuration,
the normal path should be an immediate command result plus a change
notification. Polling should become a recovery/compatibility mechanism, not the
main coordination path.

### 5. The file is still treated as an editable database

The current code explicitly supports hand-editing `settings.json`. The docs also
describe the remaining cross-process case as a hand-edit racing a save, with
last-writer-wins semantics. That is a reasonable compatibility decision, but it
is incompatible with the strongest version of the proposed ownership rule.

If Relay owns configuration, direct edits should be unsupported during normal
operation. Provide an explicit import/repair command or stop Relay before a
manual edit. Do not quietly preserve a second writer and then claim that the
tray is the sole authority.

### 6. Some important decisions happen before the serialized write

The most serious logical race is not a data race. `ProjectOps.Update` and
`ServiceOps.Update` can decide whether a presence approval is required before
the settings mutation is serialized. Another request can narrow the record
between those two points. The first request can then commit a broader change
under an approval decision made against older state.

The rule must be: approval, current-record lookup, validation, and commit are
one short queued step, which needs no revision check. If approval necessarily
happens outside that step (a human prompt must not hold the lane), the approval
must carry what it was based on, such as the record's generation or a hash of
the fields it approved, and the commit must refuse or re-check when that
changed.

`ProjectOps.Update`, `ServiceOps.Create/Update/Register` and
`EnrolmentOps.SetRemoteConfig` are resolved this way: the prompt runs before the
queue, and the queued step re-derives the decision against the live record and
refuses when it needs an approval that was not obtained (or a different digest
than the one approved). A stale snapshot that over-prompted is tolerated.

### 7. Persisted configuration and running processes can diverge

Service updates currently commit configuration and then restart the process.
That leaves an ordering window: an update can finish its external work after a
delete, or two updates can apply runtime changes in the opposite order from
their commits. The result can be a service running with a configuration Relay
no longer owns.

Runtime application is ordered by running it inside the same queued step as the
commit. Where it cannot, it needs its own ordered reconciliation worker that
consumes committed changes, coalesces obsolete work, and refuses to start a
service that the current configuration no longer names. Report “saved,” “applied,”
and “failed to apply” as different states.

### 8. Single ownership is not currently enforced at process startup

The tray opens the writable store and the bridge startup removes an existing
socket path, but there is no clear process ownership lock around the whole
tray. A second Relay process could therefore interfere with the socket and
configuration ownership assumptions.

Acquire an exclusive per-config-dir ownership lock before opening the writable
store or removing the bridge socket. A second process should fail clearly and
must never become a second manager. The lock must remain held for the tray's
entire lifetime.

### 9. Reset and adjacent configuration files need the same owner

Sealed-store reset deletes settings, CA material, and the keychain item across
multiple steps. It now owns the configuration lane for that whole destructive
sequence, after an off-lane presence prompt (`resetSealedStore`,
`commitSealedReset`): a concurrent queued write cannot use the old sealing key
mid-reset, and commands queued behind it see the post-reset store. The queued
step re-checks that the key ids bound in the approved digest are still the live
ones and otherwise deletes nothing (`errSealedKeysChangedDuringApproval`).
Not queued: the poller's `ReloadIfChanged` and other readers take only the
store's own mutex, so they can observe the store between the file deletes and
`Reresolve` (settings resolve to empty; they never write).

Also include configuration files that are not fields in `settings.json`. The
service configuration editor writes service-owned files separately, and host
probe results can be saved after network work against an older host record.
Those paths need an owner, a generation check where work leaves the queue, and a clear definition
of whether the file belongs to Relay or to the service itself. Atomic file
replacement prevents torn bytes; it does not prevent stale content winning.

## Recommended target design

The design is the tray-owned command queue described under "Implementation
progress": one ordered lane, complete operations inside it, and a generation
check only where work leaves the queue. The tray owns the store and every write
goes through the queue.

### Possible future direction: a `ConfigManager` with revisions

This subsection is not part of the current scope and is not a success
criterion. It records what a fuller manager could add if the queue proves
insufficient, for example if many readers need cheap consistent snapshots or
change notifications. A `ConfigManager` (name can change) would be owned by
`App` and created once at tray startup.

Its responsibilities would be deliberately narrow:

- load and validate the sealed settings file;
- hold the current immutable snapshot and a monotonic revision;
- serialize configuration commands;
- persist successful mutations using the existing atomic/sealed writer;
- publish a change event after persistence succeeds;
- expose read-only snapshots and command/query methods to the rest of Relay.

The conceptual API should look like this:

```go
type ConfigSnapshot struct {
    Revision uint64
    Settings *config.Settings // immutable to callers
}

type ConfigManager interface {
    Snapshot(ctx context.Context) (ConfigSnapshot, error)
    Apply(ctx context.Context, command ConfigCommand) (ConfigSnapshot, error)
    Subscribe(buffer int) (<-chan ConfigChanged, func())
}
```

The exact Go types would be less important than the rules:

- Callers receive a deep-copied or immutable snapshot.
- Callers cannot call `Get`, `Reload`, or `With` directly.
- A command reads the manager's current state, validates it, applies one
  mutation, persists it, increments the revision, and returns the committed
  snapshot.
- A failed command does not publish a new revision.
- Side effects such as listener rebinds and service starts consume the committed
  revision and reconcile asynchronously. They never write configuration behind
  the manager's back.
- A side effect that discovers the revision changed while it was working drops
  its stale result and reconciles again.

In such a design the persistence store would become an implementation detail of
the manager. Keep its good properties—sealed fields, reload-before-write during migration,
atomic rename, fsync, refusal to overwrite unreadable state—but stop injecting
it into every operation core in production.

## Why this is better than “put a bigger mutex around the store”

A bigger mutex would reduce some races but leave the important ambiguity:
callers could still hold old snapshots and act on them later. It also invites
long blocking work inside a lock, which creates UI stalls and deadlocks.

An actor-style configuration lane gives Relay one ordering point without
serializing unrelated work. It makes the behavior easy to explain:

```text
caller -> queued command -> lookup, validate, persist, ordered side effect
                                      |
                                      v
                    async work that leaves the queue commits with a generation check
```

A database would provide stronger transactions, but it is not the right first
move here. Relay has one small sealed document, already has atomic persistence,
and needs macOS keychain ownership more than it needs a general database. SQLite
could be reconsidered if configuration becomes large, multi-user, or needs
historical transactions; it would not by itself fix stale in-memory decisions.

## Phased migration

### Phase 0 — close the immediate correctness gaps

Before a larger API migration, fix the races with the highest consequence:

- Re-check approval requirements at commit: inside the queued step when the
  approval can run there, otherwise against what the approval was based on.
- Add deterministic ordering for service lifecycle application; a delete must
  prevent an older delayed update from restarting the service.
- Enforce one tray owner before store initialization and bridge socket setup.
- Make sealed-store reset exclusive across settings, CA files, and keychain
  changes.
- Service-owned configuration saves already run inside one queued step
  (`ServiceOps.SaveConfigFile`), so they need no generation check. Delayed
  host-probe writes reserve and commit a generation on the queue.

These are correctness fixes, not a reason to serialize network or subprocess
work on the configuration lane.

### Phase 1 — make the contract explicit

Write and enforce these invariants:

- The tray is the only normal configuration owner.
- Production code never constructs `FileSettingsStore` except at tray startup
  and in the explicitly documented offline recovery path.
- Production code never calls `SettingsStore.Get` or `With` outside the config
  package and its queue adapter.
- `settings.json` is not a supported live second writer.
- Every committed configuration change produces one notification.

Add a structural test that fails on new production uses of the old store API.
Keep the existing tests, but stop treating a green `-race` run as proof of
correct configuration ordering; race freedom and freshness are different
properties.

### Phase 2 — snapshots for readers

Give readers snapshots or purpose-specific query methods over the tray-owned
store, backed by the current mutex-backed implementation.

- Load once at startup.
- Replace direct reads in lifecycle and authorization paths with
  `Snapshot`/purpose-specific query methods.
- Keep the two-second file poll temporarily, but convert an observed external
  change into a reload and a change notification.

Monotonic global revisions, and a revision in reconciliation logs, are optional
extras here (see the future direction above), not requirements.

This phase gives visibility without changing all mutation paths at once.

### Phase 3 — move all writes behind commands

Replace `Store.With` calls in operation cores with named queued commands, for
example `UpdateService`, `RemoveMCP`, `SetRemoteConfig`, and
`RotateProjectToken`.

Each command must resolve the target record inside the queued step,
which preserves the current TOCTOU lessons in `docs/tokens.md`. The command
returns the committed record or a precise refusal. Do not let command handlers
return success until persistence has succeeded.

The bridge `admin_op`, IPC handlers, HTTP routes, and tray menu should all call
the same command methods. CLI mutations are already close to this shape; make
the rule universal rather than operation-specific.

### Phase 4 — remove stale reads from normal paths

Audit every production `Get()` call. Classify each one:

- display-only: use a snapshot;
- authorization or routing: use a current purpose-specific query;
- mutation precondition: move inside the serialized command;
- lifecycle convergence: run the ordered effect in the queued step, or reconcile
  from current state;
- offline recovery: isolate behind an explicitly named read-only API.

The target is zero direct `Get()` calls outside the config implementation and
test seams. In particular, fix the model endpoint, remote/enrolment views,
service/MCP/project operations, route handlers, IPC views, and model guards.

### Phase 5 — make the tray the normal CLI authority (done, as scoped)

Normal read-only CLI commands go through the tray too. This makes the CLI
report the same snapshot the running system uses and removes the second reader.

Done for `credential list`, `service list`, `mcp list`, `login list`,
`eve list`, `enrol list`, `grant` and the name resolution in
`service unregister/restart` and `mcp unregister`. The owner's decision is
that there is no offline recovery read (`relay config export --offline` is not
built): a stopped tray is an explicit unavailable state. Two commands still
read a file of their own and need no tray: `relay audit` (the audit log) and
`relay enrol ca-fingerprint` (the public `ca.crt`); neither reads
`settings.json`. A source-text guard
(`TestReadCommands_NeverConstructASettingsStore`) keeps the scoped commands
from constructing a settings store.

### Phase 6 — replace polling with event-driven convergence

Once all supported changes go through the queue, make the commit
event the trigger for UI refresh, remote listener reconciliation, model endpoint
reconciliation, and service configuration refresh.

Retain a slow health/recovery poll for crashed children, external keychain/file
repair, and missed events. It should reconcile from current committed state; it
should not be a second configuration reader.

### Phase 7 — lock down and test the ordering contract

Add deterministic tests for:

- two concurrent commands: both changes survive and run in admission order;
- work that leaves the queue (host probe, out-of-step approval, service-owned
  file edit): a changed generation causes the stale result to be discarded or
  re-run;
- a listener rebind racing a configuration change;
- service removal racing start/restart;
- a service delete racing delayed update application;
- two service updates whose external work completes in reverse order;
- an approval decision racing a narrowing or widening update;
- a second tray startup racing socket setup;
- sealed-store reset racing a write;
- a failed persistence operation: no event and no committed change;
- a tray restart: the first snapshot exactly matches the last committed file;
- direct-file edits: either rejected as unsupported or imported through one
  explicit path, never silently merged.

Run the focused race tests and `go test -race ./...` after each concurrency
phase. Also test freshness with deterministic barriers; a race detector cannot
prove that a logically stale snapshot was not used.

## Post-commit event, convergence and the external-writer policy

**One event per committed command.** `CommandQueue.SetCommitObserver` takes a
monotonic save counter (`FileSettingsStore.Commits`, advanced inside `save`)
and one publish function. The worker samples the counter before a command and
publishes once after it returns if the counter moved, before the caller is
released. So: a command that saves twice publishes once; a command that fails,
is declined, or persists nothing (a runtime-only start or stop) publishes
nothing; a command that persisted and then failed (for example "written,
restart failed") still publishes, because committed state changed and every
view must learn it. The publisher runs on the worker and must only hand work
off. The counter counts every successful save, including the sealed reset's
`EnsureInitialized`, which runs inside its queued step.

**The tray subscribes once.** `App.onConfigCommitted` refreshes the Settings
window and menu on the main thread and converges the remote and model-endpoint
listeners from tray-owned state in a tracked goroutine. Service configuration
refresh needs no separate trigger: service saves and restarts already run
inside their queued step (`SaveConfigFile`), so the event only refreshes views.

**Polling is a health poll, not a settings reader.** `statusPoller` reaps dead
children, samples memory and pushes service status; it never reads
`settings.json`. A slower recovery tick (`RecoveryPollInterval`) re-runs the
listener reconcile so a missed event or a listener that died converges from the
tray's own state.

**Legacy callbacks that remain.** The per-core `OnChange` fields on the
enrolment, login, Eve passkey, Eve enrolment, MCP, project and host cores are
gone; the commit event replaces them. `ServiceOps.OnChange` stays because it
also reports runtime-only state (start, stop) that no commit covers, as does
`appRouter.onChange` (`onExternalChange`, a bridge-driven reconcile) and
`EnhancedServiceRegistry`'s manifest hook. `RegisterProjectRoutes` and the
frontend server still accept an unused project-refresh hook. Non-queued
persistence (startup migration and `EnsureInitialized` outside a command) has
no event; nothing is serving yet.

**External-writer policy: import through the queue.** While the tray runs it
owns `settings.json`: the store never re-reads the file on its own
(`FileSettingsStore.OwnExclusively`), so normal reads stay tray-owned
snapshots. A hand edit is picked up by a filesystem watcher, the sole ingress,
and enters as one queued command (`ImportFile`), ordered with every other
mutation:

- The watcher (`config.WatchSettingsFile`) is a kqueue watch, no timer: on the
  directory, so an atomic-rename save is seen, and on the file, so an in-place
  edit is seen; the file watch is re-armed after every event because a replace
  leaves it on the unlinked old file. It reads no settings; it only prompts an
  import.
- The queued command re-reads the file and validates it as the store's own load
  does. It applies the file only if it parses and every sealed value opens;
  otherwise the current state is kept and the reason is logged. A missing file
  is ignored. Nothing falls back to defaults, so an edit can never widen.
- The tray's own write is ignored by digest: the store remembers the hash of
  the bytes it last wrote or read, and an identical file is not an edit. A
  whitespace-only edit is absorbed the same way. Only an edit that changes state
  advances the commit counter, so it publishes exactly one commit event and the
  usual UI refresh and listener reconcile follow.
- Ordering is by admission, and no mutation saves over an unimported edit:
  every save inside the store first checks the file's digest against the last
  one it wrote or read and, if it differs, imports the file (same validation;
  an invalid file is refused and the mutation proceeds on the current state),
  then applies the mutation on top. The mutation's single commit event covers
  both. The watcher's later import finds its own write and does nothing.
- The recovery tick never reads `settings.json`. `Reload` remains only as the
  sealed reset's explicit primitive.

Reasons: one ingress through the one queue keeps a second writer's changes
ordered and validated; a merge policy would revive last-writer-wins; polling
the file would make it a second normal reader. Tests:
`TestValidHandEditIsPickedUpThroughTheQueueWithOneEvent`,
`TestInvalidHandEditIsRejectedAndTheCurrentStateKept`,
`TestTheTraysOwnWriteIsNotImported`,
`TestHandEditIsOrderedWithQueuedMutationsByAdmission`,
`TestSettingsWatcherSeesInPlaceEditsAndRepeatedAtomicReplaces`.

## Success criteria

The migration is complete when these statements are all true:

- There is one tray-owned configuration queue in a running Relay.
- The only production persistence code is behind it.
- Normal reads come from snapshots or purpose-specific queries.
- Normal mutations are queued commands, not arbitrary callbacks.
- Every committed change has a post-commit event.
- Work that leaves the queue between reading and committing carries a
  generation and is re-checked at commit; everything else runs whole inside one
  queued step.
- Listener and service side effects converge from current state.
- A stopped tray is an explicit unavailable state for normal commands, not an
  invitation for each command to become its own configuration manager.
- Offline recovery, if retained, is visibly separate and cannot write.

## Bottom line

Yes: Relay owning configuration and the tray being the persistent manager is the
right direction. The better version is not “all of Relay runs on one thread.” It
is “configuration has one owner and one ordered lane; everything else talks to
that owner.”

The current implementation has paid much of the cost, especially for sealed
writes, brokered mutations, normal CLI reads, ownership, and event-driven
convergence. The migration is not yet complete: the remaining work is the
partial event contract, missed-file-event recovery and stopped-tray import
semantics, plus the missing deterministic ordering tests. The task breakdown
and checkpoint above are the current implementation backlog.
