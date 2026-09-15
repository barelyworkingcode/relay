# Model endpoint

Relay is the single authority between projects, tools and models
(`spec-model-broker.md`). This is the P1, additive shape of that broker
(`plan-broker-and-sessions.md` §2 C8, §3.2 R-M1b): nothing depends on it yet
— it is live and reachable, but no shipped client points at it until its own
migration unit lands. Code: `internal/modelbroker/` (pure decision logic:
extraction, normalisation, catalog filtering, error bodies, usage parsing —
imported and never duplicated here), `cmd/relay/model_endpoint.go` (the HTTP
surface), `cmd/relay/model_host_registry.go`, `cmd/relay/model_keys.go`,
`cmd/relay/router_model_host.go` (the bridge side of `RegisterModelHost`).

## Listeners

| Listener | Address | Default |
|---|---|---|
| `model.sock` | `bridge.ConfigDir()/model.sock`, mode 0600 | Always served, whether or not a model host has ever registered — "no host" is a 503 on each call, not an absent socket. |
| TCP | `settings.json`'s `model_endpoint.listen` | Absent block → **off**. A non-loopback address is refused (logged loudly) and never bound. A failed bind is logged loudly and retried on the next settings poll or reconcile call, the same convergence discipline `RemoteSupervisor` uses for the mTLS listener. |

`cmd/relay.SetModelListenOverrideForTest` is a package-level Go seam this
package's own tests use instead of the `model_endpoint` block — deliberately
not an environment variable: an env var is reachable from a production
process's own environment, which is exactly what "test-only" needs to rule
out. `ModelEndpointServer.Reconcile()` runs once at startup and on every
settings-poll tick (`trayapp.go`), so an out-of-process edit (a hand-edited
`settings.json`, a future CLI) takes effect without a restart.

`ListenSocket` unconditionally removes any file already at `model.sock`
before binding, the same as `bridge.NewBridgeServer` does for `relay.sock`
— see relay's own second-tray-app-instance warning (`CLAUDE.md`) for why a
second relay process racing this against the first is a state to avoid
running into in the first place, not something this unlink call is meant to
arbitrate.

## API surface

The explicit route allowlist, request/response shapes, the 64 MiB JSON body
cap and the 25 MB audio cap are `internal/modelbroker/routes.go` and
`extract.go` — this package does not restate them. Anything not on that
allowlist (`/api/*` passthrough, `/<name>/` passthroughs, `/models/load`,
`/models/unload`) 404s as "route not found" before authentication even has a
chance to matter, though authentication is still checked first (§ Auth
order) so a probe against an unbrokered path costs nothing extra to a caller
holding no credential at all.

## Auth order

Checked in this order, on every request:

1. **`Authorization: Bearer X` or `x-api-key: X`.** If both headers are
   present and name different values, refused (401) before either is looked
   up against anything — a caller cannot use a valid credential in one
   header to smuggle a second, different assertion past the other.
   - `X` has the `rmk_` shape → looked up in the in-memory model-key table.
     Not found (never minted, or revoked) → 401.
   - Otherwise `X` is hashed and matched against a project's `token_hash`
     (the same hash relay's project-token authentication already uses) → 401
     if it matches no project.
2. **No header, on `model.sock`.** The connection's peer audit token is
   looked up against relay's launch-identity table
   (`docs/launch-identity.md`). The bound identity must hold `models`
   (`service.OpModelCall`) for a call, or `models` for `GET /v1/models` too
   (`service.OpModelList` — a future `sessions` capability's wider,
   unfiltered list, per `plan-broker-and-sessions.md` §2 C1, is not part of
   this unit). No identity, or one that doesn't hold it → 401.
3. **No header, on TCP.** Always 401. There is no mapping from a TCP
   connection to a peer identity — a URL-only client (pi's overlay, Claude
   Code's `ANTHROPIC_BASE_URL`) authenticates by bearer, never by presence.

## Scoping

| Caller | Grant |
|---|---|
| Remote project (`kind: remote`) | Refused with 403, whatever its `allowed_models` says. |
| Project (bearer = its token, or an `rmk_` key minted for it) | `allowed_models`, **read live** from settings on every request. Empty or `["*"]` means every model — translated to the literal wildcard entry before calling `modelbroker.Allowed`, since that function's own empty-grant default is "nothing" (see next row). |
| Service identity holding `models` | `ServiceConfig.AllowedModels`, read live by service id. **Empty means no models** — the opposite of a project's default, deliberate (`spec-model-broker.md` decision 4): a service like TTS or STT is normally meant to reach exactly one model, not everything relayLLM serves. `["*"]` means every model, same spelling as a project's wildcard. |

A model that resolves to nothing in the current catalog (`ReasonNotFound`)
and one that resolves but isn't in the caller's grant (`ReasonDenied`) return
the byte-identical 404 — a project must not be able to enumerate models
outside its grant by the shape of the error. The distinction is carried only
to the audit hook (see below), never onto the wire.

A caller's spelling is canonicalised before the grant check and before
forwarding (`internal/modelbroker/normalise.go`'s `canonicalize`, this
package's `RewriteJSONModel`/`RewriteMultipartModel`): relayLLM's router
dispatches only on the bare catalog id, so a `llama/<alias>`,
`mlx/<alias>` or `pi/relay-router/<id>` spelling that was allowed under one
of those aliases is rewritten to the bare id before the request reaches
`router.sock` — forwarding the caller's original spelling unchanged would
have the router itself 400 it as an unknown model.

## Model keys

Format `rmk_` + 64 lowercase hex, held in relay's memory only as a SHA-256
hash (`cmd/relay/model_keys.go`). `ModelKeyTable.Mint(projectID, label)`
returns the plaintext once; nothing else can recover it. `Revoke(label)`
removes every key minted under that label. **Nothing mints a key yet except
tests** — the bridge op that will (`MintModelKey`, restricted to a caller
holding `projects` or, later, a `project_session` acting for its own
project) is a later unit (`plan-broker-and-sessions.md` F5); this table
exists now purely so the auth resolver has a real table to check an `rmk_`
bearer against.

## Upstream (`RegisterModelHost`)

A service holding `model_host` registers its own internal router socket as
the model endpoint's one upstream, tokenless, exactly the same shape
`RegisterManifest` uses for the manifest capability:

```json
{"type": "RegisterModelHost", "arguments": {"service_id": "relayllm", "router_socket": "/path/to/router.sock"}}
```

- **Own id only.** `service_id` must equal the calling identity's own launch
  name, refused otherwise (`router_model_host.go`, mirroring
  `RegisterManifest`'s rule).
- **`router_socket` must be an absolute path**, refused at `Validate()`
  otherwise — it is dialed straight from relay's own working directory
  (`dialVerifiedUnix`), and a relative path would resolve against relay's,
  never the registering service's.
- **At most one live host.** A second registration — from the same service
  id or a different one — is refused while the current registration's
  launch is still live (`ModelHostRegistry`, `model_host_registry.go`).
  "Live" is answered fresh against `service.Launches` on every check, never
  cached as a flag, so a crashed host's registration stops being live the
  instant its launch ends, with no separate teardown call needed. A
  replacement is accepted only once the prior launch has ended.
- **Verified on every dial, not just at registration.** When relay actually
  connects to `router_socket` — to fetch `/v1/models` for the catalog cache,
  or to forward a call — it reads the *server* peer's kernel audit token
  (`LOCAL_PEERTOKEN`) off that connection and refuses (503) unless it equals
  the process that registered the host. This is the mirror image of
  relayLLM's own check on its Hello dial to relay's bridge socket (spike
  SP1, `plan-broker-and-sessions.md` §2 C9): the kernel, not either side's
  own assertion, is what ties a socket path to an identity.
- **No host, or an unreachable/mismatched one → 503** `model host
  unavailable` on every route that needs the catalog or a forward, including
  `GET /v1/models`.

Forwarding strips `Authorization`, `x-api-key` and every `x-relay-*` header
before the request leaves relay (`model_endpoint.go`'s `proxy`, mirroring
`enhanced_services.go`'s `newServiceProxy`). The response's
`X-Relay-Model-Target` header — relayLLM's own account of which managed
alias, endpoint or resolved virtual candidate served the call — is read for
the audit hook and removed before the response reaches the caller; it is
relayLLM's internal dispatch detail, never the caller's business (the same
rule `internal/modelbroker.Filter` applies to a `/v1/models` listing's
`target` field). Streaming uses `httputil.ReverseProxy` with
`FlushInterval: -1`, so the first chunk of an SSE response reaches the
caller without waiting for the upstream call to finish; a client that
disconnects mid-stream cancels the outbound request to the upstream via the
shared request context, the same propagation `net/http` gives any reverse
proxy.

## Audit

`ModelCallAudit` (`model_endpoint.go`) is the single, clearly named hook
every finished call or listing is offered to — caller kind/name, auth kind,
model key label (never the key), requested and canonical model, the
upstream's target, byte counts, parsed token usage, status and outcome.
Nothing is written to the audit file by this unit: the event constants and
the wiring into `internal.audit.AuditRecorder` are `plan-broker-and-sessions.md`
unit R-M1c. `AuditHook`'s zero value is a no-op, so wiring a real recorder
in later is a pure addition at the call site (`trayapp.go`), not a change to
this file.

## What is not brokered

- **A caller that holds its own upstream credential directly** (Claude
  Code's Anthropic subscription, a user's own API key) never touches this
  endpoint at all — that is `spec-model-broker.md` §4's decision, unchanged
  here.
- **The `/api/*` and `/<name>/` passthrough routes**, and `/models/load` /
  `/models/unload` — deliberately absent from
  `internal/modelbroker.MatchRoute`'s allowlist, not merely unlisted by
  omission (brokering the first would forward a client's own upstream
  credential; the others reach a control-plane action through what is meant
  to be a data plane).
- **A `project_session` caller** (a session-host root or one of its
  descendants) — that identity kind does not exist in this repo yet
  (`plan-broker-and-sessions.md` §2 C1/C2 is a later unit); the seam is
  `resolveIdentity` in `model_endpoint.go`, which today only resolves a
  `service` kind.
