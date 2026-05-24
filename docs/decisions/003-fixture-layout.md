# ADR-003: Fixture Layout & Content Guidelines

**Status:** Accepted
**Date:** 2026-05-24

## Context

Tests need realistic-looking config to exercise auth, project routing,
service dispatch, and skill resolution. We also want reproducible
screenshots and screencasts of a populated relay install — without
exposing real customer data, real tokens, or developer machine paths.

Solution: one fixture tree, used both ways. Tests copy it into
`t.TempDir()` (read-only source). The demo harness
(`scripts/demo.sh`) copies it into `/tmp/relay-demo-home/` and launches
relay with `--config-dir` pointed at the copy.

## Decision

### Layout

```
test/fixtures/
├── relay-home/                       # the dual-purpose root
│   ├── settings.json                 # populated install: 3 projects, 3 MCPs, 2 services
│   ├── projects/
│   │   ├── acme-website/             # web app — broad MCP access
│   │   ├── field-notes/              # knowledge vault — restricted MCPs
│   │   └── greenhouse-monitor/       # IoT — service-driven
│   └── samples/
│       ├── prompts/                  # canonical prompts for tool-loop tests
│       ├── tool-calls/               # canonical MCP JSON-RPC pairs
│       └── screencast-scripts/       # ordered prompts for reproducible demos
├── manifests/
│   └── relayllm.json                 # cross-repo contract: relayLLM's manifest
└── scenarios/                        # demo-only overlays
    ├── services-running/             # files dropped on top of relay-home to simulate state
    ├── permission-prompt-active/
    └── fresh-install/                # empty settings.json + no projects
```

### Content guidelines (mandatory)

These rules apply to anything under `test/fixtures/`. Code reviewers
should reject PRs that violate them.

- **No real names, emails, hostnames, customer references, or repo
  paths.** Use generic-but-recognizable domain names (acme-website,
  field-notes, greenhouse-monitor).
- **No real tokens.** All token values must be obvious test sentinels of
  the form `test-token-<service>-do-not-use`. The sandbox-safety guard
  flags any token that doesn't match this pattern.
- **No machine-specific paths.** Project `path` values in fixture
  `settings.json` use `${RELAY_HOME}/projects/<name>` placeholders that
  `mkSandboxRelayHome(t)` and `scripts/demo.sh` substitute at copy time.
- **Skills and CLAUDE.md files are written like real onboarding docs** —
  readable on a screen capture, no lorem ipsum. If you wouldn't show it
  to a stranger watching a demo, don't put it in fixtures.
- **No binary assets larger than 32 KiB** (icons, images, PDFs). The
  fixture tree should clone fast.

### Read-only convention

Tests never write to `test/fixtures/`. Always work against the tempdir
copy. The sandbox-safety guard fails any test run that modifies a file
under `test/fixtures/`.

### Adding a new project to the fixtures

1. Pick a generic name from a new domain (don't pile onto an existing
   one). Update this ADR's project list.
2. Add a `CLAUDE.md` written like an onboarding doc.
3. Register the project in `relay-home/settings.json` with a unique
   `id`, the `${RELAY_HOME}/projects/<name>` path, and a clearly-named
   test sentinel token.
4. If the new project is meant for a specific test scenario, add a
   matching overlay under `scenarios/` instead of mutating shared state.

### Adding a new scenario overlay

A scenario is a directory under `scenarios/` whose contents are
overlaid on top of `relay-home/` (via `cp -r`) by
`scripts/demo.sh --scenario <name>`. Scenarios are demo-only — they are
not consumed by tests. Tests should construct any necessary state from
the base fixture plus in-test mutations.

## Consequences

- **Good:** One fixture tree to maintain instead of two parallel
  test-data and demo-data trees that drift apart.
- **Good:** Screenshots are reproducible — the same fixture renders the
  same tray menu every time.
- **Good:** The "no real tokens" rule + automated check means a
  screencast can never leak a live secret.
- **Trade-off:** Fixtures grow over time. We commit to a 32 KiB per-file
  cap and an annual review (delete unused projects/scenarios).
- **Trade-off:** Tests are slightly slower than pure in-memory builders
  because of the recursive copy. Acceptable: copying ~3 small projects
  takes <5ms.

## See also

- ADR-001 — testing strategy (sandbox-safety rule)
- ADR-002 — `SetConfigDirForTest` seam used by `mkSandboxRelayHome`
- `support_test.go` — `mkSandboxRelayHome` implementation
- `scripts/demo.sh` — demo / screenshot harness
