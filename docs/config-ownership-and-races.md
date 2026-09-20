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

The remaining work is to apply the same boundary to the other configuration
domains and route normal CLI reads through the tray. The branch is deliberately not calling this
finished until those paths are covered.

### Verification so far

- `go test ./internal/config` passes.
- `go test -race ./internal/config -run 'TestCommandQueue' -count=10` passes.
- `go test -race ./cmd/relay -run 'TestServiceOpsRace'` passes.
- `go test ./cmd/relay -count=1` passes.

The repository-wide pass was run before the structural-test adjustments and
failed only in the command package's gate-name expectations and malformed IPC
fixture; those failures were fixed, then the full `cmd/relay` package passed.

## What Relay has today

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

The current design still has several competing meanings of “the configuration”:

1. The file on disk.
2. A `FileSettingsStore` cache.
3. A fresh read through `FreshSettings`.
4. A component's copied settings passed into a long-lived operation.
5. A second CLI process' read-only store.

Those views can disagree by design. That is where the remaining race surface
comes from.

## Where it falls short

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

### Phase 5 — make the tray the normal CLI authority

Route normal read-only CLI commands through the tray too. This makes the CLI
report the same snapshot the running system uses and removes the second reader.

Keep a narrowly named offline command only if recovery value justifies it, for
example `relay config export --offline`. It must be clearly read-only, must not
pretend to describe live state, and should refuse to run while the tray is
active unless it can prove a consistent snapshot. Do not keep many ordinary
commands with hidden direct-file behavior.

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

The current implementation has already paid much of the cost, especially for
sealed writes and brokered mutations. The remaining work is architectural
cleanup: remove direct cached reads, move every mutation onto the queue as a
complete step, add generation checks only where work leaves the queue, and
demote file polling and offline CLI reads from normal behavior to recovery
compatibility.
