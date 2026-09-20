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
result. Ordinary reads use the latest committed snapshot. Revision numbers and
stale-result checks remain useful for recovery and UI freshness, but FIFO
ordering is the primary mechanism.

### Completed on this branch

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
- MCP add/remove and OAuth-state mutations now use the same queue. Discovery,
  browser flows, and bridge notifications remain outside the queue.
- Project create/update/remove/token-rotation mutations now use the same queue;
  project skill cleanup and session cleanup remain after the committed delete.
- Login bootstrap mint, passkey revoke, and browser-session sign-out now use the
  same queue, including their required issuance records.
- Enrolment create, sign, approve, update, revoke, refusal, and remote-config
  mutations now use the same queue. Their settings, issuance/audit work, and
  approval payload resolution complete before the caller returns.
- Remote-config change detection and its presence decision now run on that
  queue, so a queued remove cannot authorize against a stale pre-queue state.
- Eve enrolment-window open/consume and Eve passkey report/revoke/unrevoke
  mutations now use the same queue, including persistence and audit effects.
- Host create, update, remove, and probe reserve and commit persisted probe
  generations on the queue; SSH discovery remains outside it, and stale probe
  results cannot overwrite a newer connection shape or a removed host.
- Terminal template create, update, and remove now use the same queue; the
  Settings window (IPC) and the HTTP routes share one `TemplateOps`.

The remaining work is to apply the same boundary to the other configuration
domains, route normal CLI reads through the tray, and queue service-owned config
file edits with their restart. The branch is deliberately not calling this
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
boundary, a revision check, or a reconciliation loop that treats the current
configuration as the source of truth.

### 4. File polling is being used as a coordination mechanism

The tray's two-second `ReloadIfChanged` poll is doing three jobs:

- noticing an external edit;
- refreshing cached settings;
- triggering listener reconciliation and UI refresh.

That is workable while external edits are supported, but it is an awkward
control plane. It introduces an intentional delay and makes “configuration
changed” an observation rather than an event. If the tray owns configuration,
the normal path should be an immediate command result plus an event or revision
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
one short configuration transaction. If approval necessarily happens outside
that transaction, the approval must carry the revision it was based on and the
commit must refuse or re-check when the revision changed.

### 7. Persisted configuration and running processes can diverge

Service updates currently commit configuration and then restart the process.
That leaves an ordering window: an update can finish its external work after a
delete, or two updates can apply runtime changes in the opposite order from
their commits. The result can be a service running with a configuration Relay
no longer owns.

Runtime application needs its own ordered reconciliation worker. It should
consume committed revisions, coalesce obsolete work, and refuse to start a
service that the latest snapshot no longer names. Report “saved,” “applied,”
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
multiple steps. It must pause or exclusively own the configuration lane for the
whole recovery operation; a concurrent write must not use the old sealing key
mid-reset.

Also include configuration files that are not fields in `settings.json`. The
service configuration editor writes service-owned files separately, and host
probe results can be saved after network work against an older host record.
Those paths need an owner, revision or generation check, and a clear definition
of whether the file belongs to Relay or to the service itself. Atomic file
replacement prevents torn bytes; it does not prevent stale content winning.

## Recommended target design

Introduce a `ConfigManager` (name can change) owned by `App` and created once at
tray startup.

Its responsibilities are deliberately narrow:

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

The exact Go types are less important than the rules:

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

The persistence store should become an implementation detail of the manager.
Keep its good properties—sealed fields, reload-before-write during migration,
atomic rename, fsync, refusal to overwrite unreadable state—but stop injecting
it into every operation core in production.

## Why this is better than “put a bigger mutex around the store”

A bigger mutex would reduce some races but leave the important ambiguity:
callers could still hold old snapshots and act on them later. It also invites
long blocking work inside a lock, which creates UI stalls and deadlocks.

An actor-style configuration lane gives Relay one ordering point without
serializing unrelated work. It makes the behavior easy to explain:

```text
caller -> tray command -> validate current revision -> persist -> publish revision
                                      |
                                      v
                           async side-effect reconciliation
```

A database would provide stronger transactions, but it is not the right first
move here. Relay has one small sealed document, already has atomic persistence,
and needs macOS keychain ownership more than it needs a general database. SQLite
could be reconsidered if configuration becomes large, multi-user, or needs
historical transactions; it would not by itself fix stale in-memory decisions.

## Phased migration

### Phase 0 — close the immediate correctness gaps

Before a larger API migration, fix the races with the highest consequence:

- Re-check approval requirements against the current revision at commit.
- Add deterministic ordering for service lifecycle application; a delete must
  prevent an older delayed update from restarting the service.
- Enforce one tray owner before store initialization and bridge socket setup.
- Make sealed-store reset exclusive across settings, CA files, and keychain
  changes.
- Add generation checks to service-owned configuration and delayed host-probe
  writes.

These are correctness fixes, not a reason to serialize network or subprocess
work on the configuration lane.

### Phase 1 — make the contract explicit

Write and enforce these invariants:

- The tray is the only normal configuration owner.
- Production code never constructs `FileSettingsStore` except at tray startup
  and in the explicitly documented offline recovery path.
- Production code never calls `SettingsStore.Get` or `With` outside the config
  package and manager adapter.
- `settings.json` is not a supported live second writer.
- Every committed configuration change has one revision and one notification.

Add a structural test that fails on new production uses of the old store API.
Keep the existing tests, but stop treating a green `-race` run as proof of
correct configuration ordering; race freedom and freshness are different
properties.

### Phase 2 — introduce snapshots and revisions

Wrap the existing `FileSettingsStore` in a tray-owned manager. At first, the
manager may still use the current mutex-backed implementation internally.

- Load once at startup.
- Assign revision 1 to the committed startup state.
- Replace direct reads in lifecycle and authorization paths with
  `Snapshot`/purpose-specific query methods.
- Add a revision to listener and service reconciliation logs.
- Keep the two-second file poll temporarily, but convert an observed external
  change into a manager reload and a published revision.

This phase gives visibility without changing all mutation paths at once.

### Phase 3 — move all writes behind commands

Replace `Store.With` calls in operation cores with named manager commands, for
example `UpdateService`, `RemoveMCP`, `SetRemoteConfig`, and
`RotateProjectToken`.

Each command must resolve the target record inside the serialized command,
which preserves the current TOCTOU lessons in `docs/tokens.md`. The command
returns the committed record or a precise refusal. Do not let command handlers
return success until persistence has succeeded.

The bridge `admin_op`, IPC handlers, HTTP routes, and tray menu should all call
the same command methods. CLI mutations are already close to this shape; make
the rule universal rather than operation-specific.

### Phase 4 — remove stale reads from normal paths

Audit every production `Get()` call. Classify each one:

- display-only: use a snapshot;
- authorization or routing: use a current manager query;
- mutation precondition: move inside the serialized command;
- lifecycle convergence: consume the committed revision and reconcile;
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

Once all supported changes go through the manager, make the manager's commit
event the trigger for UI refresh, remote listener reconciliation, model endpoint
reconciliation, and service configuration refresh.

Retain a slow health/recovery poll for crashed children, external keychain/file
repair, and missed events. It should reconcile from the manager's current
revision; it should not be a second configuration reader.

### Phase 7 — lock down and test the ordering contract

Add deterministic tests for:

- two concurrent commands: both changes survive and revisions are ordered;
- a command that waits on external work: a newer revision causes stale work to
  be discarded or re-run;
- a listener rebind racing a configuration change;
- service removal racing start/restart;
- a service delete racing delayed update application;
- two service updates whose external work completes in reverse order;
- an approval decision racing a narrowing or widening update;
- a second tray startup racing socket setup;
- sealed-store reset racing a write;
- a failed persistence operation: no event and no revision advance;
- a tray restart: the first snapshot exactly matches the last committed file;
- direct-file edits: either rejected as unsupported or imported through one
  explicit path, never silently merged.

Run the focused race tests and `go test -race ./...` after each concurrency
phase. Also test freshness with deterministic barriers; a race detector cannot
prove that a logically stale snapshot was not used.

## Success criteria

The migration is complete when these statements are all true:

- There is one tray-owned configuration manager in a running Relay.
- The only production persistence code is behind that manager.
- Normal reads come from manager snapshots or manager queries.
- Normal mutations are ordered commands, not arbitrary callbacks.
- Every committed change has a revision and a post-commit event.
- Listener and service side effects are revision-aware and converge from current
  state.
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
cleanup: remove direct cached reads, move the store behind a manager API, make
commands revision-aware, and demote file polling and offline CLI reads from
normal behavior to recovery compatibility.
