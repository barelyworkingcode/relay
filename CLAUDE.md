# Relay (Go)

macOS MCP orchestrator and project manager: a tray app with project-scoped
auth, a Unix-socket bridge, an external-MCP proxy (stdio + HTTP/OAuth), a remote
mTLS listener and background service supervision. macOS only (cgo + Cocoa).

This file holds the working rules. The design and its reasoning live in `docs/`
— **read the document for an area before changing it**, and update it in the
same change when behaviour moves.

## Where things are written down

| Working on | Read first |
|---|---|
| Request flow, file map, grants, remote listener, credential model, Settings UI | [`docs/architecture.md`](docs/architecture.md) |
| Moving code between packages | [`docs/package-layout.md`](docs/package-layout.md) |
| Any CLI subcommand | [`docs/cli.md`](docs/cli.md) |
| Tokens, credentials, expiry, sealing | [`docs/tokens.md`](docs/tokens.md), [`docs/launch-identity.md`](docs/launch-identity.md), [`docs/sealed-config.md`](docs/sealed-config.md) |
| Presence prompts, gated ops | [`docs/presence-gate.md`](docs/presence-gate.md) |
| HTTP routes, credential class, request and response | [`docs/routes.md`](docs/routes.md) |
| Project files for eve (file routes, `/ws/files`, containment, read-only) | [`docs/project-files.md`](docs/project-files.md) |
| Remote projects, enrolment | [`docs/access-profiles.md`](docs/access-profiles.md), [`docs/install-remote-machine.md`](docs/install-remote-machine.md) |
| MCP scoping (`contextSchema`, `_meta`) | [`docs/context-schema.md`](docs/context-schema.md) |
| Audit log | [`docs/audit-log.md`](docs/audit-log.md) |
| Service logs: levels, line keys, trace ID | [`docs/logging-standard.md`](docs/logging-standard.md), [`docs/logging-schema.json`](docs/logging-schema.json) |
| Event lines, the event catalogue, `relay logs` | [`docs/events.md`](docs/events.md) |
| Enhanced services, manifests, restart supervision | [`docs/service-manifest.md`](docs/service-manifest.md) |
| Sessions (terminal, claude, pi, chat) | [`docs/session-host.md`](docs/session-host.md) |
| `relay sandbox`, the bridge takeover seam | [`docs/sandbox-command.md`](docs/sandbox-command.md) |
| SSH host projects | [`docs/ssh-hosts.md`](docs/ssh-hosts.md) |
| Model endpoint, model keys | [`docs/model-endpoint.md`](docs/model-endpoint.md) |
| Tests | [`docs/testing.md`](docs/testing.md), [`docs/testing-roadmap.md`](docs/testing-roadmap.md) |
| Devbox verify (layer 2, the running app) | [`cmd/devboxverify/README.md`](cmd/devboxverify/README.md) |
| Judging a security finding: who relay defends against, and what's out of scope | [`docs/THREAT-MODEL.md`](docs/THREAT-MODEL.md) |

Conventions the global rules name: the **threat model** is
[`docs/THREAT-MODEL.md`](docs/THREAT-MODEL.md), the **feature map** is
[`docs/FEATURES.md`](docs/FEATURES.md) (a PR that adds or changes a feature
updates it), and the **verify harness** is `cmd/devboxverify`. Planners and
reviewers judge security findings against the threat model: a finding names
the attacker, the asset and the promise broken, or it is out of scope.

ADR-NNN citations in code and docs are labels for reasoning recorded in those
documents; there is no ADR directory to look in.

## Layout

```
cmd/relay/           the tray app and CLI: gated cores (*Ops), doors (HTTP routes,
                     ipc_*.go, *_cmd.go), router.go, cgo/Cocoa
cmd/relaysessions/   the session host (relay-sessions), built into Contents/Helpers/
cmd/testmcp, cmd/testservice: probe binaries the devbox journeys build
internal/<domain>/   one capability per package: its types AND the operations on them
web/src/             Settings UI; bundled in-process by web/gen (no Node)
docs/                design, rationale, operator guides
```

Sibling repos (`../relayLLM`, `../eve`, `../relayScheduler`, `../relayTelegram`,
`../macMCP`, `../fsMCP`, `../relayRemote`) are reference clients of relay's
protocols. Relay has no privileged path for any of them and hardcodes no
service id.

## Build and test

```bash
./build.sh              # build + install /Applications/Relay.app and launch it
./build.sh --test       # go vet first; abort install on failure
./build.sh --release    # sign + notarize + /tmp/Relay.dmg (implies --test)

go build ./... && go vet ./...        # while working
(cd e2e && go test -race ./features/)  # the feature tests; builds the test bundle itself
golangci-lint run ./...               # a ratchet, not a gate: add no new findings
```

What gates what (`.githooks/`, run by the machine's global hooks dispatcher; never set a repo-local `core.hooksPath`, which skips the push guard):

| Stage | Runs |
|---|---|
| commit | `gofmt`, `go build`, `go vet` — seconds |
| push | `go build ./...`, `go vet ./...` |
| PR and `main` (GitHub Actions) | `gofmt`, `go build`, `go vet`, `go vet -tags relaytest ./...`, the test-build absent/present check, and a step that fails on any `_test.go` outside `e2e/` |
| PR and `main` (GitHub Actions) | the `e2e` job: `gofmt` and `go vet` in `e2e/`, then `go test -race -count=1 ./features/` |

## House rules

### Security invariants

Each of these has been a bug or is one refactor away from being one. The
reasoning is in `docs/architecture.md`; do not relax one without reading it.

- **Every default fails closed.** Absent credential class = nothing. Absent
  `allowed_tools`/`access` on a remote record = none/read. A `scope: "restrict"`
  field with no value denies. An unparseable `expires` is expired. Never add a
  default that widens, and never let one allowlist widen another.
- **Test `proj.IsRemote()`**, never `Kind == ProjectKindLocal` — the zero value
  is local.
- **Tool arguments are `json.RawMessage` end to end.** Inspect for a decision,
  forward the original bytes; never decode into Go values and re-encode. Never
  recompute `_meta.args_sha256`.
- **Authorization reads on the remote path use `freshSettings`**, never
  `store.Get()`.
- **Dispatch tables are security boundaries.** `remoteHandlers`,
  `remoteConfigHandlers` and `presence.GatedOps` gain an entry only
  deliberately; there is one door into issuance (`enrolment.sign`) and no
  `enrolment.approve` op. The enrolment lodge path never touches
  `presence.Gate`. `maxPendingEnrolmentRequests = 8` is a security margin, not
  a tunable.
- **Issuance is recorded before the secret leaves**, and refused if it cannot
  be. Revocation is never refused for a broken log. Remote calls are
  intent-then-completion, fail-closed; no remote traffic with auditing off.
- **No relay credential in any child's environment.** Secrets go over fd 3;
  tokens never appear in a DTO except `rotate` and `reveal` (`relay project token`).
- **`disclose` governs what the client sees, never what the operator sees.**
- **Control-plane routes register through `RouteRegistrar`.** `execute` and
  `proxy` routes are absent from the TCP mux, not refused on it.
- **An external MCP is published only with its schema**: `connectStdio` is the
  only start path; tools, schema and connection install in one critical section.

### Structure

- `internal/` never imports `cmd/relay`. Gated cores, doors and `router.go` stay
  in `cmd/relay`; a package that would hold types while `main` keeps the
  operations is the wrong cut.
- HTTP and IPC doors share one core per domain (`*Ops`, the `Settings`
  mutators). Add behaviour to the core, never to one door.

### Tests

- Relay has no unit tests and CI rejects a `_test.go` outside `e2e/`. Until
  `e2e/` exists, a bug's failing repro is a devbox journey (`cmd/devboxverify`).
- Time-dependent code takes an injected clock; a test does not sleep to wait
  for one.
- Report a failure with its output. Do not skip, loosen or delete a test to
  get a green run; `--no-verify` is for the operator, not the agent.

### Comments and docs

- A comment says **this is subtle** or **this is deliberate** (name the
  constraint). Anything explaining *what* the code does is a naming failure.
- Never in a comment: change history, issue/PR numbers, what a previous
  version got wrong. Code is present tense; the *why* goes in `docs/`.
- Public-repo hygiene: no real hostnames, tokens or personal paths in docs,
  fixtures or test data.
- A PR that adds or changes a feature updates its row in
  [`docs/FEATURES.md`](docs/FEATURES.md) (ID, doors, proof) and the test the row
  names, in the same PR. A new door gets a row. Row IDs are never reused. A
  change to a promise in `docs/THREAT-MODEL.md` updates the promise table in
  the same PR. A PR that changes code under an area's `code` globs and moves
  no row says in the PR why nothing a user reaches moved. The reviewer checks
  it.

### This file

Rules and pointers only. If what you are adding explains how something works
or why it was decided, it belongs in `docs/` with a row in the table above.
