# Testing Roadmap

Cross-repo status of bringing each sibling repo up to the bar in
ADR-001 (the three-tier testing strategy). relay has no unit tests and is
proven by devbox journeys ([`testing.md`](testing.md)).

Headline rule everywhere: **no test may touch the user's real config
directory** (full rationale in ADR-001). Each repo enforces it differently;
record the approach here when you add coverage to a new one.

## Status by repo

| Repo | Default tier | Live tier | Pre-commit gate | Sandbox guard |
|---|---|---|---|---|
| **relay** | none (devbox journeys; see [`testing.md`](testing.md)) | no | build+vet at commit, build+vet at push, build+vet in CI | none |
| **relayLLM** | yes | yes (`-tags=live` + `-tags=llm`) | yes (`.githooks/pre-commit`) | partial |
| **eve** | yes (Jest) + e2e (Playwright) | no | no | no |
| **relayScheduler** | partial (`client_test.go`) | no | no | no |
| **macMCP** | none | no | no | no |
| **fsMCP** | none | no | no | no |
| **relayTelegram** | none | no | no | no |

relayLLM's tier model is documented in its
`docs/decisions/002-three-tier-testing.md`.

## Recommended next pickup order

1. **fsMCP** — no harness yet, smallest TypeScript surface, exercised on
   every relay tool call. Highest leverage per hour. Needs:
   - Vitest or Jest setup.
   - Sandbox helpers that isolate `_meta.allowed_dirs` from the host FS.
   - Cross-repo contract test asserting its tool schema matches relay's
     auto-disable-on-fs expectation (relay keys off the `allowed_dirs`
     field in the MCP schema — see `internal/project.V1AllowedDirsField`).
2. **eve** — already has Jest unit + Playwright e2e; finish the standard:
   wire a pre-commit gate and add a sandbox guard.
3. **relayScheduler** — extend beyond `client_test.go` to a full tier +
   pre-commit gate.
4. **macMCP** — Swift, slowest to set up (XCTest + sandboxed FileManager).
   Defer until the others are done.

## Lessons learned (relay overhaul)

Pitfalls so the next repo's overhaul moves faster.

### macOS Unix-socket length cap (104 chars)
Temp-dir paths on macOS look like
`/var/folders/k6/.../T/TestName1234567890/001/` — easily 80+ chars. Add a
socket name (`relay.sock`) and you blow past 104 with
`bind: invalid argument`. Allocate socket-holding dirs under `/tmp`.

### A live tray app rewrites the real config dir
A running relay rewrites `settings.json` in the real config dir on its own
schedule, so a before/after comparison of that dir cannot tell the live app
from a leaking test. An isolated `HOME` that points at a tripwire under a
`/tmp` root avoids the ambiguity. Relay has no such guard now.

### The router and the registry must share one launch table
The registry begins launches in `service.Launches` and the router binds and
looks them up there. Giving each its own `service.NewLaunches()` silently
breaks identity auth — every `Hello` is refused because the router's table
never saw the launch. Production wires `reg.Launches = router.launches`.

### A sandboxed `HOME` breaks a spawned browser, silently
A Chrome spawned with `HOME` pointed at a temp dir starts, attaches to the
DevTools Protocol and answers every command, but **every navigation hangs
before it commits** — which reads as the server under test not answering, and
is not. Restore the account's real `HOME` for the browser only; Chrome's own
state stays in `--user-data-dir`.

### Do not mock subprocess spawn
Use the real `cmd/testservice/main.go` binary so the journeys exercise the
production spawn path (env injection, pidfile, log routing, reaper, token
cleanup).

### Cross-repo contract via committed JSON fixture
What relayLLM registers is a cross-repo contract. No committed fixture
carries it now; relay keeps no copy, so relayLLM's own side has nothing in this
repo to assert its generated manifest against, and drift is caught only by
the journeys that register it.

## Named gaps in the passkey login evidence

ADR-016 decision 8 states these rather than leaving them to be assumed away.
A real ceremony in headless Chrome against the real `/relay/login` document
would not buy the following, and no test runs one now.

### No real hardware authenticator
A Chrome-driven ceremony uses Chrome's virtual CTAP2 authenticator. This host is an
Apple VM with no Touch ID, no Secure Enclave and no USB passthrough, so no
genuine authenticator — platform or roaming — has ever produced an assertion
relay has seen. A virtual authenticator configured with
`isUserVerified: true` always sets the UV bit, so such a run proves relay
*checks* that bit and never that real hardware would have set it. Closing this
needs a machine with a real authenticator; no amount of software raises it.

### No Safari coverage
Safari is the only other browser on this host and the one the owner would
actually use. Its consent UI, its passkey storage in iCloud Keychain and its
own view of `localhost` as a secure context are exercised by nothing. A
Chrome-green run is evidence about WebAuthn, not about Safari. Safari has no
DevTools-Protocol equivalent for injecting a virtual authenticator, so this
gap cannot be closed by the same mechanism and would need a driven real
authenticator — which is the first gap again.

## Not yet adopted

Tracked but unbuilt: byte-equality goldens for the bridge wire format and a
goroutine-leak check. None exist today; add if a drift or leak incident
warrants it.
