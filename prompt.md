# Shipping the configuration-ownership work

Continue in `/Users/admin/source/barelyworkingcode/relay`.

Read `docs/config-ownership-and-races.md` before changing anything. Its
status summary and current checkpoint are authoritative. This prompt is the
shipping gate for the work already implemented; it is not a request to design
another configuration architecture.

## Current state

The implementation for tasks 1–6 is complete under the documented policies:

- one tray owner per configuration directory;
- tray-owned normal reads and queued mutations;
- an enforceable production read boundary;
- one post-commit event per persisted command;
- event-driven normal convergence with a slow recovery poll;
- explicit `settings.json` import semantics.

The event rule is deliberate: failure before persistence publishes no event;
an operation that persists and then reports a later side-effect error still
publishes because committed state changed. A missed filesystem notification is
an acknowledged recovery limitation, not an invitation to add normal polling.

Task 7 is partial. The remaining work is deterministic test coverage and
final verification so the current implementation can ship with evidence.

## Goal

Ship the current configuration-ownership implementation without broadening its
architecture.

## In scope

1. Inspect the current tests and status document, then mark the shipping task
   `In progress` in `docs/config-ownership-and-races.md` before editing.
2. Add only the smallest deterministic tests needed for the documented gaps:
   listener rebind ordering; service deletion versus delayed start/update;
   reverse-order external work; reset versus write; restart snapshot
   consistency; direct-file behavior; and concurrent HTTP/IPC/tray mutations.
   Reuse existing barriers, fixtures, and queue seams. Add MCP/enrolment
   cross-door coverage only if the existing harness supports it without a new
   test framework or production abstraction.
3. If a new test proves a real implementation defect, make the smallest
   production fix, add its regression test, and update the status immediately.
   Do not change behavior merely to make a test easier to write.
4. Run focused tests for each slice, the relevant `cmd/relay` and
   `internal/config` package tests, and the repository-wide tests with explicit
   Go timeouts. Record every command and result in the status document.
5. Review the final diff for accidental architecture, weakened assertions,
   stale status claims, or changes to `.playwright-cli/`.

## Out of scope

- no `ConfigManager`, global revision system, new polling coordinator, or new
  configuration layer;
- no product/UI/network redesign;
- no changing the accepted event or external-writer policies;
- no weakening, skipping, quarantining, or rewriting tests to obtain green;
- no destructive Git commands, merge, push, or pull request;
- no unrelated cleanup.

## Shipping acceptance

The work may be marked `Complete` only when all of these are true:

- every new test is deterministic and fails for the regression it covers;
- focused tests pass;
- `go test -timeout 5m ./...` passes;
- `go test -race -timeout 10m ./...` passes, or the environment makes that
  impossible and the status records the exact blocking package/test without
  claiming shipment;
- the status table, task breakdown, current checkpoint, and test evidence in
  `docs/config-ownership-and-races.md` agree;
- no task is `Outstanding`, `Partial`, or `Blocked` unless shipping is
  explicitly blocked and the blocker is recorded;
- the final diff contains only the bounded test/fix/documentation changes.

If a full-suite command hangs or times out, identify the package/test from the
output, stop safely, and record it. Do not infer success from the absence of a
failure line.

## Status protocol

For each coherent slice:

1. mark it `In progress` with scope, files/components, and acceptance checks;
2. implement the smallest slice;
3. run and record focused checks;
4. mark it `Complete` only after the checks pass, otherwise `Partial` or
   `Blocked` with the exact remaining work;
5. keep the status table and task breakdown synchronized;
6. finish with changed files, tests run, failures, and the next follow-up.

Preserve existing user work and the untracked `.playwright-cli/` directory.
Use `apply_patch` for edits and keep comments only for subtle or deliberate
constraints. Work on a `pm/<short-slug>` branch and commit completed slices.

## Final report

Report whether the current implementation is shippable, the exact test
commands and results, any environmental blockers, changed files, and whether
the status document is `Complete` or still `Partial`.
