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

## Format

Each ADR carries **Status** + **Date** in its header, then **Context**,
**Decision**, **Consequences**, and an optional **See also**. Mirrors the
convention in `../relayLLM/docs/decisions/`.
