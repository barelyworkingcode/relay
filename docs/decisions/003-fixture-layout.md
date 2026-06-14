# ADR-003: Fixture Layout & Content Guidelines

**Status:** Accepted
**Date:** 2026-05-24

## Context

Tests need realistic config to exercise auth, project routing, service
dispatch, and skill resolution. We also want reproducible screenshots and
screencasts of a populated relay install — without exposing real customer
data, real tokens, or developer machine paths.

One fixture tree serves both. Tests copy it into a per-test sandbox dir
(`mkSandboxRelayHome`, `support_test.go`). The demo harness
(`scripts/demo.sh`) copies it into `/tmp/relay-demo-home/` and launches
relay with `--config-dir` pointed at the copy. Neither path mutates the
checked-in tree.

## Decision

### Layout

```
test/fixtures/
├── relay-home/                       # the dual-purpose root (3 projects, 3 MCPs, 2 services)
│   ├── settings.json
│   ├── projects/
│   │   ├── acme-website/             # web app — broad MCP access (allowed_mcp_ids: *)
│   │   ├── field-notes/              # knowledge vault — restricted (fs only)
│   │   └── greenhouse-monitor/       # IoT — restricted (mac, search)
│   └── samples/                      # prompts, tool-call JSON pairs, screencast scripts
├── manifests/
│   └── relayllm.json                 # cross-repo contract: relayLLM's manifest (see NewFakeRelayLLMService)
└── scenarios/                        # demo-only overlays (services-running,
                                      # permission-prompt-active, fresh-install)
```

### Content guidelines (mandatory)

These rules apply to everything under `test/fixtures/`. They are enforced
by code review — there is no automated content guard. Reviewers should
reject PRs that violate them:

- **No real names, emails, hostnames, customer references, or repo paths.**
  Use generic-but-recognizable domains (acme-website, field-notes,
  greenhouse-monitor).
- **No real tokens.** Token values must be obvious test sentinels of the
  form `test-token-<service>-do-not-use`. Reject anything that could be
  mistaken for a live secret.
- **No machine-specific paths.** Project `path` values use
  `${RELAY_HOME}/projects/<name>` placeholders, substituted at copy time by
  `mkSandboxRelayHome(t)` (`support_test.go`) and `scripts/demo.sh`.
- **Skills and CLAUDE.md files read like real onboarding docs** — legible
  on a screen capture, no lorem ipsum. If you wouldn't show it to a
  stranger watching a demo, leave it out.
- **No binary assets over 32 KiB** (icons, images, PDFs). Guideline only,
  not automated — keep the tree fast to clone.

Tests never write to `test/fixtures/`: they always work against the
tempdir copy produced by `mkSandboxRelayHome(t)`. (The separate sandbox
guard in `support_safety_test.go` protects the *real* user ConfigDir, not
this tree — see ADR-001.)

### Adding a new project

1. Pick a generic name from a new domain (don't pile onto an existing one).
   Update the layout above.
2. Add a `CLAUDE.md` written like an onboarding doc.
3. Register it in `relay-home/settings.json` with a unique `id`, a
   `${RELAY_HOME}/projects/<name>` path, and a sentinel token.
4. If the project exists only for a specific scenario, add a matching
   overlay under `scenarios/` instead of mutating shared state.

### Adding a new scenario overlay

A scenario is a directory under `scenarios/` whose contents are overlaid on
top of `relay-home/` by `scripts/demo.sh --scenario <name>`. Scenarios are
demo-only — tests do not consume them and should construct any needed state
from the base fixture plus in-test mutations.

## Consequences

- **Good:** One fixture tree instead of parallel test-data and demo-data
  trees that drift apart.
- **Good:** Reproducible screenshots — the same fixture renders the same
  tray menu every time.
- **Good:** The sentinel-token rule means a screencast can't leak a live
  secret.
- **Trade-off:** Fixtures grow over time. We hold a 32 KiB per-file cap and
  an annual review (delete unused projects/scenarios).
- **Trade-off:** Tests are slightly slower than pure in-memory builders
  because of the recursive copy. Acceptable: ~3 small projects copy in
  <5ms.

## See also

- ADR-001 — testing strategy & the sandbox guard (`docs/decisions/001-testing-strategy.md`)
- ADR-002 — `SetConfigDirForTest` seam used by `mkSandboxRelayHome` (`docs/decisions/002-test-seams.md`)
- `support_test.go` — `mkSandboxRelayHome` implementation
- `scripts/demo.sh` — demo / screenshot harness
