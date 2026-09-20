# Package layout: what lives where, and why

Relay is one command, `cmd/relay`, over a set of private packages under
`internal/`. This document records the reasoning that is not visible from the
directory listing: what belongs in a package, what is deliberately left in the
command, and which structural tests depend on that split holding.

## The rule

A package owns a capability: its types **and** the operations on them. If a
proposed package would hold the data while the command kept every operation on
it, the cut is in the wrong plane — that is an anemic package, and it moves
coupling rather than removing it. The symptom is a crop of functions in
`main` whose first parameter is a pointer to a type the package owns.

Dependencies point one way: `cmd/relay` imports `internal/...`, and packages
import `internal/config`, `internal/control`, `internal/sealed`. A package
under `internal/` must never import `cmd/relay`.

## What stays in cmd/relay, deliberately

**The gated cores.** `CredentialOps`, `EnrolmentOps`, `LoginOps`, `McpOps`,
`ProjectOps`, `ServiceOps`. Each is the same three steps — require the
presence gate, record the issuance, delegate to the domain — and each holds a
`presence.Gate` plus the shared issuance auditor. That is composition, not
domain. The behaviour they delegate to is what moved.

**The doors.** HTTP routes, IPC handlers and CLI command surfaces. A door
decodes a request, calls one operation, and encodes the answer.

**`router.go`.** Not extractable, and not a loose end. `appRouter` holds
direct fields of five of the six gated cores, so any package containing it
would have to import `cmd/relay`. Its coupling to the MCP layer is already
correct: it reaches tools only through the `ToolProvider`/`ToolManager`
interfaces, never a concrete manager.

**Native residue.** `mcp_permissions*.go` reaches AppKit through cgo to prime
TCC. That is a tray-process concern; an `internal/` package is the wrong home
for it.

**Log rotation.** It carries relay's own log and the audit log as well as
service logs, so it is general infrastructure rather than any one domain's.

## Structural tests that constrain this layout

These guards share a failure mode: each answers a question by matching source
text, so a rename or a move can leave one scanning nothing while still
reporting success. An empty scan reads exactly like compliance.

**Mutation containment** (`gate_structural_test.go`). Finds every call to a
gated mutator and requires it to be in an allowlisted file. It matches by
**identifier name**, so renaming a mutator silently removes it from the set
unless `gatedMutatorNames` and its pinned `want` copy are updated together.
Case is load-bearing: a package-level function in `main` is lowercase, a
method still on a `config` type keeps its exported name.

The directories it walks are **discovered** — `cmd/relay` plus every package
under `internal` except `config`, which declares the mutators. A
hand-maintained list is the wrong shape: code that moves out of it reports
zero violations forever.

**Issuance-auditor coverage** (`gate_ast_scan_test.go`,
`TestGate_EveryIssuanceAuditorCallSiteHasACase`). Matches
`requireIssuanceAuditor` as a bare `*ast.Ident`. A qualified call is an
`*ast.SelectorExpr` and would not match, so the scan would find nothing and
the guard would pass over an empty set. **`requireIssuanceAuditor` must stay
declared and called unqualified in `cmd/relay`.** A forwarding shim would be
the same vacuity with one more hop. This is why the audit extraction is
partial: the log engine moved, the gate-facing half did not.

**Test-only fakes** (`testfake_seam_test.go`). A package that fakes a security
boundary refuses to run outside a test binary; the guard proves the stronger
property, that nothing imports one, since Go links only what it imports. The
set is discovered from that refusal rather than listed, and the scan fails if
it finds fewer than two.

**Settings read boundary** (`cmd/relay/settings_read_boundary_test.go`).
Walks `cmd` and `internal` (except `internal/config`) for zero-argument
`Get`/`Reload`/`ReloadIfChanged` on a receiver whose name ends in `store`, and for
store construction (`config.NewSettingsStore*`, `config.ResolveSealedStore`, a
`FileSettingsStore` literal). Matching is by name, so a store held in a
differently named variable escapes it. It pins three known findings in
`runTrayApp` and `statusPoller` so an empty scan fails, and a synthetic-source
test proves each violation kind is reported. Writes (`With`, `WithDeclinable`)
are covered by the two guards above. The same file checks that `runTrayApp`
calls `AcquireTrayOwnership` before `ResolveSealedStore` and `NewBridgeServer`.

When changing any of these, prove the guard still bites: introduce the
violation it exists to catch, watch it fail by name, then revert. A passing
guard is not evidence that it is still looking.

## Reaching a state the exported API cannot express

Some tests need a state no constructor produces — a settings store serving
values never written to disk, a manager holding an injected connection. Add a
named constructor in the owning package guarded by
`if !testing.Testing() { panic(...) }`, as `config.NewSettingsStoreWithCache`
does. Never reach across a package boundary with `reflect` or `unsafe`: it
defeats the boundary and breaks silently when a field is renamed.
