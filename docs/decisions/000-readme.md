# Architecture Decision Records

This directory captures load-bearing design decisions for relay — the *why*,
not the *what*. Read these when you're about to change something that
touches one of the patterns described here, or when a future agent asks
"why does it work this way?"

ADRs are numbered, immutable once accepted, and superseded by writing a new
ADR that references the old one. Do not edit accepted ADRs in place.

## Index

- [001 — Testing strategy](001-testing-strategy.md): three-tier model
  (hermetic default / `-tags=live` / `-race`), hermetic-first, **never touch
  the user's ConfigDir**, pre-commit gating.
- [002 — Production test seams](002-test-seams.md): which narrow seams
  (`SetConfigDirForTest`, `NewSettingsStoreAt`, etc.) live in production
  code, and the three criteria a new seam must meet to earn its keep.
- [003 — Fixture layout](003-fixture-layout.md): the dual-purpose
  `test/fixtures/relay-home/` tree (test source + demo harness) and content
  rules (no PII, no real tokens, no machine paths).
- [004 — Native project management lives in relay](004-project-mgmt-in-relay.md):
  why the native Projects tab and Eve's project dialog share one set of
  `Settings.*Project*` mutators instead of relay deferring to a service.
- [005 — TCC entitlements live on relay, MCPs inherit](005-tcc-permissions.md):
  relay holds the personal-information entitlements and fires the prompts;
  MCPs declare `--tcc-services` and inherit grants via responsible-parent
  attribution. Checklist for adding a new TCC service.
- [006 — Image generation via MCP + progress framework](006-image-gen-via-mcp-and-progress-framework.md):
  image gen ships as an MCP (relay-comfyui), not a relay carve-out, plus the
  generic MCP progress-notification framework.
- [007 — Relay is the sole broker of project tokens](007-project-token-brokering.md):
  Eve references projects by id only; relay resolves and injects the scoped
  project token just-in-time and never hands it to the frontend.
- [008 — Tool-call audit log at the router chokepoint](008-tool-call-audit-log.md):
  every call, denial, and auth failure is logged at `appRouter.CallTool`;
  attribution from relay + the kernel, redacted args, metadata-only results,
  fail-open but visibly so.
- [009 — Remote projects are capability grants, not directories](009-remote-projects.md):
  a `remote` project kind for agents running on a separate VM, with no
  filesystem path; validation refuses incoherent local-only features, and
  filesystem scope, sessions, and PTY launches are each refused twice —
  once by validation, once at the point of use.
- [010 — A remote caller is a certificate on a narrow listener](010-remote-client-transport-and-identity.md):
  the 2b half ADR-009 deferred. A separate `RemoteServer` whose dispatch table
  holds only `ListTools`/`CallTool`; relay is its own CA and an **enrolment
  record binds a certificate to its grants**, so there is no bearer token on the
  remote path at all; directory auth is unrepresentable rather than refused;
  audit is fail-closed and written *before* the call; and rate/volume budgets
  live on the enrolment, the unit of compromise.
- [011 — A client is an identity, an access profile, and a resource
  scope](011-resource-scope.md): the four allowlists, the `contextSchema`
  vocabulary (`scope` / `source` / `applies_to`), and why an absent resource
  scope is a refusal rather than a default.
- [012 — An external MCP child is supervised, and an over-long frame is one bad
  answer](012-external-mcp-supervision.md): stdio MCPs are restarted with a
  backoff and a cap on restart *intensity*; a respawn re-runs the handshake AND
  the context-schema discovery, and the connection is published only after both
  — in one critical section, so a callable MCP with no schema is
  unrepresentable; an over-long frame fails its own call and the stream
  resyncs; and a child's death, recovery, and abandonment become audit rows.
- [013 — Relay forwards a call's arguments, it does not re-serialise
  them](013-relay-forwards-arguments-verbatim.md): tool arguments travel as
  `json.RawMessage` from the wire to the MCP and into the audit log. Relay
  validates and authorises a call without decoding its payload, because a round
  trip through Go values substitutes U+FFFD for a lone surrogate, sorts keys,
  drops duplicates and reformats numbers — and because a log that paraphrases
  what a client sent is not ground truth.

- [014 — Every capability is an HTTP capability, and the view is just a
  client](014-every-capability-is-an-http-capability.md): the IPC dispatch
  table holds 27 commands and HTTP answers 9, so starting a service, revoking
  an enrolment and querying the audit log are reachable only from a mouse. One
  core per capability, envelopes that decode/call/encode and nothing else,
  commands that answer their caller, and events demoted to change
  notifications. Authorization is deferred and named as the risk; any listener
  beyond the 0600 socket is opt-in and absent by default.
- [015 — The control plane is classed by blast radius, and transport is part of
  the grant](015-control-plane-authorization.md): pays ADR-014's deferred
  authorization debt. Four capability classes (`read` / `configure` / `grant` /
  `execute`) where the line for `execute` is **who chose what runs**, not
  whether a process starts; `execute` routes are never registered on a TCP
  listener at all rather than gated behind a check, imitating ADR-010's
  two-entry dispatch table; a credential names its classes and an absent set
  grants nothing; and the settings view becomes the LEAST privileged client
  because a browser-held credential is the most exposed. `PeerPID` is
  explicitly refused as an authorization input.

## Format

Each ADR carries **Status** + **Date** in its header, then **Context**,
**Decision**, **Consequences**, and an optional **See also**. Mirrors the
convention in `../relayLLM/docs/decisions/`.
