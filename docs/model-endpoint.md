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

The explicit route allowlist and request/response shapes are
`internal/modelbroker/routes.go` and `extract.go` — this package does not
restate them; body size limits are their own section below. Anything not on
that allowlist (`/api/*` passthrough, `/<name>/` passthroughs, `/models/load`,
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

## Granting a service models, in Settings

Settings' service editor (`web/src/app.js`'s `renderServiceForm`) shows an
**Allowed Models** section once the `models` capability checkbox is on: a
list of model ids, add/remove rows, a lone `*` row meaning every model — the
same spelling `ServiceConfig.AllowedModels` and a project's own
`allowed_models` both use. The section states the empty-means-no-models rule
explicitly, since it is the opposite of a project's default. Leaving the
section untouched (the `models` capability off, or never opened at all) omits
`allowed_models` from the save entirely, the same "absent means leave it
alone" rule the capabilities and env editors already follow — a save that
never shows the grant can never clear it.

A model id round-trips through `ipc_services.go`'s `add_service`/
`update_service` messages and the `POST`/`PUT /api/services` HTTP routes as a
plain JSON string list. Server-side, an empty or whitespace-only id is
refused before anything persists, and every id is trimmed. **Adding** an id,
or switching the list to `*`, is new reach and goes through the presence gate
exactly like adding a capability does; removing ids, reordering them, or
resending the stored list unchanged narrows or changes nothing and is never
gated (`cmd/relay/service_ops.go`'s `serviceWidensAllowedModels`).

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

## Limits

R-M1b2 (`plan-broker-and-sessions.md`'s relay#116 re-review row) is required
hardening before the TCP listener above is ever turned on or any client
migrates to this endpoint — none of it changes the API surface, only what
relay is willing to spend memory on to serve it.

- **`JSONBodyCap` is 16 MiB** (`internal/modelbroker/extract.go`), down from
  64 MiB (which mirrored relayLLM's own `maxProxyBodyBytes`; relayLLM still
  enforces that independently, so a body relay now refuses at 16 MiB was
  never guaranteed to reach relayLLM's larger limit in the first place).
  Every route this cap applies to — `chat/completions`, `responses`,
  `messages`, `embeddings` — carries text, not binary payloads, so 16 MiB is
  generous headroom over any real conversation history. The reason for the
  cut is memory, not abuse: extracting and rewriting a JSON body's "model"
  field (`ExtractJSONModel`/`ExtractJSONModelFromBytes`, `RewriteJSONModel`)
  amplifies a body's size several-fold in live allocations while doing it —
  a near-cap body at the old 64 MiB ceiling measured at roughly 11-13x
  (`internal/modelbroker/memory_test.go`'s regression guard asserts a loose
  ceiling on this at the current cap), i.e. hundreds of MiB of GC pressure
  from a single caller's single request.
- **`AudioMultipartCap` stays 25 MiB**, unchanged, matching relayLLM's own
  `maxTranscriptionBytes`: `RewriteMultipartModel` re-streams every other
  part (in particular the audio file) through unread rather than decoding it
  into a Go value, so a multipart body's amplification is close to 2x, not
  10x+ — the JSON path is where the cut matters.
- **A shared `BodyBudget` (`internal/modelbroker/bodybudget.go`) bounds total
  in-flight body-processing bytes across every concurrent call, both
  listeners, at 64 MiB** (`maxInFlightBodyBytes`, `cmd/relay/model_endpoint.go`).
  This is the piece that actually bounds the *worst case*: a smaller
  per-request cap alone still lets memory scale linearly with however many
  callers connect at once (a same-uid process or a key holder opening many
  connections), which was the re-review's actual complaint (S6) — a handful
  of concurrent near-cap requests pushing relay into memory pressure. A
  request is admitted for a weight equal to its *route's cap*, never its
  actual or declared size: relay reads up to that cap regardless of what a
  caller claims up front (`readCapped` enforces it independent of
  `Content-Length`), so accounting by anything smaller would let a caller
  under-report size to buy extra concurrency the cap exists to rule out.
  With the ~11x measured amplification and a 64 MiB admission ceiling, the
  endpoint's documented worst case for body-processing memory is **~700
  MiB, independent of how many callers are connected** — down from
  unbounded (N concurrent near-cap callers previously scaled linearly with
  no ceiling at all). 64 MiB is comfortably above `AudioMultipartCap` so a
  single audio call is always admissible on its own. A blocked admission
  waits up to `bodyBudgetWaitTimeout` (5s) before the endpoint gives up and
  answers 429 (`rate_limited`) rather than queuing indefinitely. The budget
  is held only across the amplifying work — reading, extracting, rewriting —
  and released before the single, already-final rewritten body is handed to
  the reverse proxy, so it does not throttle overall proxy concurrency for
  calls that have already cleared that phase (including slow or streamed
  upstream responses).
- **A trailing value after the top-level JSON object is refused at
  extraction time** with the endpoint's normal shape-appropriate 400
  (`ErrTrailingData`), not left to surface later as `RewriteJSONModel`'s own
  `json.Unmarshal` error, which the endpoint used to turn into a bare 500.
- **The JSON extractor uses `json.Decoder.UseNumber()`**, so a number literal
  outside float64's safe range in a field the extractor merely skips past
  (never one it inspects) is not itself refused — `dec.Token()` would
  otherwise convert every number it walks to float64 and error on one out of
  range, refusing a body relayLLM's own untyped decode would accept.
  `RewriteJSONModel` already preserved every field's raw bytes exactly via
  `json.RawMessage` regardless of this setting; `UseNumber` is what lets
  extraction *reach* that point for such a body instead of refusing it
  first.

## Audit

`ModelCallAudit` (`model_endpoint.go`) is the single, clearly named hook
every finished call or listing is offered to — caller kind/name, auth kind,
model key label (never the key), requested and canonical model, the
upstream's target, byte counts, parsed token usage, status and outcome.
`AuditHook`'s zero value is still a no-op (a caller that constructs a
`*ModelEndpointServer` without wiring one, a test say, behaves exactly as
before), but in the running tray it is wired in `trayapp.go` to
`recordModelCall` (`cmd/relay/audit_model.go`, `plan-broker-and-sessions.md`
unit R-M1c), which turns one of these into a real `audit.AuditEvent` and
hands it to `internal/audit.AuditRecorder` on the ordinary fail-open path
(`Record`, never `RecordDurable`) — a model call is never delayed by, or
refused because of, a full or broken audit sink.

Two event kinds: `model_call` per finished call, `model_list` per
`GET /v1/models` listing — the latter gated by `log_lists`, exactly as
`list_tools`/`list_skills` are, since a caller lists far more often than it
calls. The full field list, the actor mapping (a project's own token vs. an
`rmk_` model key vs. a service's launch identity vs. an unresolved,
`unknown`-kind caller that still names what auth it attempted), the outcome
vocabulary, and worked CLI examples (`relay audit --event model_call`,
`--kind project_session`, ...) are documented once, in
[`docs/audit-log.md`](audit-log.md#the-model-endpoint) rather than restated
here. **Never recorded:** the model key or a project token (plaintext or
hash), or any request/response content — no prompt, message, instruction,
tool definition, audio, or completion text.

Outcome is one of: `ok`, `denied`, `not_found` (`modelbroker.ReasonDenied`/
`ReasonNotFound`, the audit-only distinction behind the identical 404 body —
see Scoping above), `unauthorized`, `remote_project`, `route_not_found`,
`host_unavailable`, `bad_request`, `body_too_large`, `trailing_data` (the
last three from `requestErrorBody`, one per way `ExtractJSONModel`/
`ExtractMultipartModel` can refuse a request body), `error`, `client_abort`,
`rate_limited` (new in R-M1b2, a `BodyBudget` admission that never got a
slot within its wait timeout — see Limits above). Two outcomes got more
precise in R-M1b2, both from the re-review of relay#116's proxy
recover/completion logic (`cmd/relay/model_endpoint.go`'s `proxy`):

- **`client_abort` is audited only for a recovered `http.ErrAbortHandler`.**
  `proxy`'s recover used to label every panic `client_abort`, which would
  hide a genuine bug in the proxy stack behind a client-fault outcome. Any
  other recovered value is audited `error` and still re-panics — a panic
  through a handler must still reach the server to do its job regardless of
  outcome; only re-labelling was ever wrong, not the propagation.
- **A completed call is audited `ok` even if the caller's context reports
  done by the time `proxy` checks it, provided the response body was copied
  to EOF first.** A client that disconnects the instant after receiving
  every byte of a complete response still cancels the request context —
  checking only `r.Context().Err()` after `rp.ServeHTTP` returns would
  misreport that fully successful call as `client_abort` with no usage
  recorded. `countingReader.eof`, set only once the upstream response body
  has been read to its own natural end, is what `proxy` consults first; the
  context is checked for the abort verdict only when the copy actually
  stopped short of that.

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
