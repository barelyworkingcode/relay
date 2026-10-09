# Package layout: what lives where, and why

Relay is one command, `cmd/relay`, over a set of private packages under
`internal/`. This document records the reasoning that is not visible from the
directory listing: what belongs in a package, and what is deliberately left in the
command.

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

## Enforcement

No test enforces the layout; the reviewer checks it. Two properties are worth
the reviewer's attention because a rename or a move can break them silently:

- A gated mutator is called only from the allowlisted files, and
  `requireIssuanceAuditor` stays declared and called unqualified in
  `cmd/relay`, so a scan that matches by bare identifier still finds every
  gated core that audits its issuance.
- Settings reads outside `internal/config` go through the fresh-settings
  path, and `runTrayApp` calls `AcquireTrayOwnership` before
  `ResolveSealedStore` and `NewBridgeServer`.
