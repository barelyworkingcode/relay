# Architecture Decision Records

This directory captures load-bearing design decisions for relay — the *why*,
not the *what*. Read these when you're about to change something that
touches one of the patterns described here, or when a future agent asks
"why does it work this way?"

ADRs are numbered, immutable once accepted, and superseded by writing a new
ADR that references the old one. Do not edit accepted ADRs in place.

## Index

- [001 — Testing strategy](001-testing-strategy.md): three-tier model
  (default / `-tags=live`), hermetic-first, **never touch the user's
  ConfigDir**, pre-commit gating.
- [002 — Production test seams](002-test-seams.md): which narrow seams
  (ConfigDir override, etc.) live in production code, and the criteria a
  new seam has to meet to earn its keep.
- [003 — Fixture layout](003-fixture-layout.md): what's in
  `test/fixtures/`, the dual-purpose test-and-demo rule, and content
  guidelines (no PII, no real tokens, no machine paths).

## Format

Each ADR has: **Status**, **Date**, **Context**, **Decision**,
**Consequences**, and optional **See also**. Mirrors the convention used
in `../relayLLM/docs/decisions/`.
