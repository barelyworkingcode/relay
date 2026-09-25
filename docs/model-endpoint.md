# Model endpoint

Relay is the single authority between projects, tools and models
(`spec-model-broker.md`). This is the P1, additive shape of that broker
(`plan-broker-and-sessions.md` §2 C8, §3.2 R-M1b), extended so one base URL
serves both a model relay manages and a provider's own model
([Client model routing](#client-model-routing)). Code: `internal/modelbroker/`
(pure decision logic: extraction, normalisation, catalog filtering, error
bodies, usage parsing, the passthrough path table and the `/v1/messages`
classification — imported and never duplicated here),
`cmd/relay/model_endpoint.go` (the HTTP surface), `cmd/relay/model_host_registry.go`,
`cmd/relay/model_keys.go`, `cmd/relay/router_model_host.go` (the bridge side
of `RegisterModelHost`).

## Listeners

| Listener | Address | Default |
|---|---|---|
| `model.sock` | `bridge.ConfigDir()/model.sock`, mode 0600 | Always served, whether or not a model host has ever registered — "no host" is a 503 on each call, not an absent socket. |
| TCP | `settings.json`'s `model_endpoint.listen` | Absent block → **off**. A non-loopback address is refused (logged loudly) and never bound. A failed bind is logged loudly and retried on the next committed change, recovery tick or reconcile call, the same convergence discipline `RemoteSupervisor` uses for the mTLS listener. |

`cmd/relay.SetModelListenOverrideForTest` is a package-level Go seam this
package's own tests use instead of the `model_endpoint` block — deliberately
not an environment variable: an env var is reachable from a production
process's own environment, which is exactly what "test-only" needs to rule
out. `ModelEndpointServer.Reconcile()` runs once at startup, after every
committed configuration change and on the tray's slow recovery tick
(`trayapp.go`). A hand edit of `settings.json` under a running tray is imported through the
config queue when valid, and reaches this reconcile as a commit event.

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
that allowlist and not one of the three fixed passthrough paths
([below](#client-model-routing)) — `/models/load`, `/models/unload`, any
other `/<name>/` — 404s as "route not found" after authentication (§ Auth
order), so a probe against an unbrokered path costs nothing extra to a caller
holding no credential at all.

## Auth order

This is the **local** branch's order ([Client model routing](#client-model-routing)
says when a request is on it). Checked in this order:

0. **`X-Relay-Key: X`.** Relay's own credential in its own header, for a client
   whose `Authorization` is its own provider login and cannot also carry relay's.
   When present it is the only relay credential considered: `X` is looked up
   exactly as step 1 below looks up a bearer, and `Authorization` and `x-api-key`
   are the client's — not consulted, never forwarded. A blank or repeated
   `X-Relay-Key` is a 401, never read as absent, and never falls through to a
   launch identity.
1. **`Authorization: Bearer X` or `x-api-key: X`** (when there is no
   `X-Relay-Key`; these are what pi's overlay and the chat provider send). If both headers are
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

On the grant side, an `allowed_models` entry `pi/<provider>/<id>` covers a
request for `<id>` whatever the provider segment says, `relay-router`
included. A pi session is created under that spelling but reaches the broker
with the bare id, so the grant that let the session start must also let its
requests through. Only the first segment is the provider: `pi/ep/m` covers
`m`, never the endpoint id `ep/m` (that needs `pi/<provider>/ep/m`). A
modelMap key is covered by `pi/<provider>/<key>` just as by the bare key, and
also by any spelling that covers its target.

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

## Choosing a project's models, in Settings

The Projects tab edits a project's `allowed_models` with a searchable
multi-select. It keeps the wildcard switch: when the switch is on, the grant
is `["*"]` and the list is hidden. The list is fed by the `list_models` IPC
(`cmd/relay/ipc_models.go`), a thin door over `ModelCatalogOps`
(`cmd/relay/model_catalog_ops.go`). That core is read-only and ungated, and it
has no HTTP door.

**Where the list comes from.** `ModelCatalogOps.List` reads relay-sessions'
`GET /api/models` through `sessionHostClient.ListModels`
([session-host.md](session-host.md)). That is the same list eve's session
picker shows: Claude aliases, pi models and broker models. It then reads this
endpoint's own catalog cache (`modelbroker.Cache.Snapshot`) to add detail the
host does not return. The order is deliberate. relay-sessions' fetch refreshes
the shared cache, so the second read matches the first.

- Only `chat`-provider rows are matched to cache rows, by exact id. A modelMap
  key (`owned_by: anthropic-map`) is grouped under `Model broker · aliases`
  and labelled `key → target`. A virtual model goes under
  `Model broker · virtual`, and any other row under `Model broker · <owned_by>`.
- Unmatched rows, and every row from another provider, keep the host's group
  and label.
- If relay-sessions cannot be reached, the view is `unavailable`
  (`session host unavailable`) with no models, and the cache is not read.
  If only the cache fails, the view is still `ok` with the host's grouping
  and the warning `model broker unavailable`.

**What the operator should know.**

- **Ids match exactly.** The picker saves the catalog's own ids, so a typo
  cannot hide a model. A saved id that is no longer listed stays selected,
  marked "not currently available", and is never dropped unless the operator
  unchecks it.
- **An empty list means every model**, the same as `["*"]` (see
  [Scoping](#scoping)). The picker says so when nothing is selected.
- **Alias targets are shown to the operator only.** The target reaches the
  Settings window and nowhere else. `/v1/models` callers still never see it:
  `modelbroker.Filter` strips `target`.
- **Non-chat models sit under a collapsed "Other" group.** The split is a
  heuristic (`modelKind`). It takes the id's last `/` segment, lowercases it,
  and splits it on anything that is not `a-z` or `0-9`. The model is "other"
  when a whole token is one of `tts asr stt speech whisper parakeet kokoro
  orpheus dia codec snac vocoder embed embedding embeddings rerank reranker`.
  An alias is "other" if either it or its target is. `audio` is deliberately
  not on the list, because chat model names carry it too. A wrong guess only
  moves a row between groups; it never changes what is saved.

## Model keys

Format `rmk_` + 64 lowercase hex, held in relay's memory only as a SHA-256
hash (`cmd/relay/model_keys.go`). `ModelKeyTable.Mint(projectID, label)`
returns the plaintext once; nothing else can recover it. `Revoke(label)`
removes every key minted under that label. `AuthorizeLaunch` mints one under
the label `session:<id>` for every `pi`/`chat` session and every `pty`
template with `model_key: true`, scoped to the launching project.

A key dies four ways:

- **`SessionExited`.** relay-sessions reports the session's end and
  `sessionAccount.end` revokes the key (also on project delete).
- **Its launch ends.** After a launch says Hello, `launchOnHost` binds the key
  to the launch (`ModelKeyTable.BindLaunch`), and `Lookup` re-derives whether
  that launch is still live on every call (`Launch.Live`) — the same "derive
  liveness, do not track a flag" rule `ModelHostRegistry.liveLocked` uses, with
  no sweeper. A root process that exits (the launch table's own watcher ends the
  launch), a replacing `Begin`, or relay-sessions' whole launch ending kills the
  key with nothing reporting it, which is what covers a crashed relay-sessions
  or a failed `SessionExited` report. A dead key is dropped from the table on
  the lookup that finds it.
- **A relay restart.** The table is in memory, so every live key is invalid
  after one. Accepted: relay already takes the sessions it hosts down with it.
- **An explicit `Revoke`/`RevokeKey`**, which a failed launch uses to undo
  exactly the key it minted.

Which keys are bound to a launch follows which launches say Hello: a `pty`
session (the shim), and a `claude`/`pi` session with a sandbox profile or a
launch identity, do; a `chat` session never does (its provider is an in-process
HTTP client with no shim) and an ad-hoc or SSH-hosted session has no launch at
all. Those keys keep `SessionExited`-only revocation. An unbound launch expires
after `ProjectSessionLaunchTTL`, so binding a key to one would kill it seconds
into the session.

**Delivery is the template's job.** Minting a key does not put it anywhere. A
`pty` template names where it goes with `${MODEL_KEY}` in an `env` value, and
only when the template has `model_key: true`; relay-sessions expands it at
spawn (`internal/sessions/terminal`). A template with no mapping gets no key in
its environment — nothing sets a default variable. `${MODEL_KEY}` in argv, or in
a template that did not opt in, is refused at validation, and `${RELAY_TOKEN}`
still is. For example, Claude Code:

```json
{ "id": "claude-code-relay", "name": "Claude Code (relay models)", "command": "claude",
  "model_key": true, "env_passthrough": [],
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:9911",
    "ANTHROPIC_CUSTOM_HEADERS": "X-Relay-Key: ${MODEL_KEY}"
  } }
```

Claude Code sends `ANTHROPIC_CUSTOM_HEADERS` (`Name: value`, newline-separated)
on its `/v1/messages` calls beside its own `Authorization`; Codex takes
`env_http_headers = { "X-Relay-Key" = "<env var name>" }` on a
`model_providers.<id>` entry, or `http_headers`, and sends its ChatGPT-login
token as `Authorization` beside it (verified against Claude Code 2.1.277 and
Codex 0.153.4 with a local recording server).

**The cost, accepted.** The key sits in the client's environment, readable by
any process of the same uid (`KERN_PROCARGS2`) — the exposure class
`RELAY_PROJECT_TOKEN` already has. It is bounded by the project's
`allowed_models` and the session's lifetime, and it reaches only this
endpoint. This reverses the stance `internal/config/templates.go` took when it
retired `${RELAY_TOKEN}`, for a much lower-value credential.

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

On the local branch, forwarding strips `Authorization`, `x-api-key` and every
`x-relay-*` header (`X-Relay-Key` included) before the request leaves relay
(`model_endpoint.go`'s `proxy`, mirroring `enhanced_services.go`'s
`newServiceProxy`). The passthrough branch strips only the `x-relay-*` ones. The response's
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

- **The `/models/load` and `/models/unload` routes** — deliberately absent from
  `internal/modelbroker.MatchRoute`'s allowlist, not merely unlisted by
  omission: they reach a control-plane action through what is meant to be a
  data plane.
- **A provider's own model, when the client holds its own credential.** It is
  no longer left to bypass this endpoint: it is forwarded by the passthrough
  branch ([below](#client-model-routing)), so one base URL serves both. Relay
  brokers nothing about it — no grant, no `allowed_models`, no scoping — and
  audits it only as a passthrough.
- **A `project_session` caller** (a session-host root or one of its
  descendants) reaches the local branch tokenlessly on `model.sock`
  (`resolveIdentity` in `model_endpoint.go`), as above.

## Client model routing

One endpoint, two branches, chosen per request (`plans/client-model-routing.md`).
The point: Claude Code, Codex and pi run with one base URL, a model relay
manages is served by the broker (scoped to the project, audited), and a
provider's own model reaches that provider with the client's own credential.

**Two credentials, two headers.** The client's own token stays in
`Authorization` (or `x-api-key`), untouched. Relay's per-session key travels in
`X-Relay-Key`. A subscription login sends one credential, its OAuth token, on
every request; it cannot also carry a relay key there.

| Request | Branch |
|---|---|
| a path under `/api/`, `/chatgpt/` or `/openai/` | **passthrough** |
| `POST /v1/messages` or `/v1/messages/count_tokens`, model is a `modelMap` key in relay's catalog | **local** |
| the same, model is a Claude id (`claude-…`) not in the catalog | **passthrough** (Anthropic) |
| the same, any other model, including a catalog model that is not a `modelMap` key | 404 |
| every other allowlisted route | **local** |

- **Local** is everything above this section: a valid relay credential, the
  project's `allowed_models`, an audit record, and `Authorization`,
  `x-api-key`, `X-Relay-Key` and every `x-relay-*` stripped before the request
  reaches the model host.
- **Passthrough** needs no relay authentication. The request is forwarded to
  the model host (relayLLM's router.sock, which forwards it to the provider),
  `Authorization` and `x-api-key` byte for byte, the body exactly as read, and
  `X-Relay-Key` and every `x-relay-*` stripped. WebSocket upgrades are carried
  (OMP's codex transport prefers one). A valid `X-Relay-Key` on a passthrough
  request attributes its audit record to the session's project and grants
  nothing.
- The **path table is fixed in relay** (`modelbroker.MatchPassthrough`), not
  read from relayLLM's `router.passthrough` config, so what can reach a cloud
  provider does not change under a config edit relay never saw. relayLLM
  mounts the routes it forwards to (`router.passthrough` entries `chatgpt` and
  `openai`, `router.anthropic` for `/api/` and `/v1/messages`); a route it does
  not have is its own 404. A path with a `.`, `..` or empty element is never a
  passthrough route.
- `/v1/messages` is classified by the model, so relay reads the body and
  resolves the model against a **fresh** catalog first
  (`Cache.Resolve`). The catalog check comes before the id's shape: a
  `modelMap` key that looks like a Claude id is relay's to serve. A catalog
  model that is *not* a `modelMap` key (a managed alias, an endpoint model)
  has no Anthropic-shaped way to be served, and relayLLM would forward it to
  Anthropic on that route, so relay answers 404 rather than let its prompt
  leave the machine. A mistyped local name (not Claude-shaped) is the same 404.
  Cost: a Claude model is never in the catalog, so every such request pays one
  extra `GET /v1/models` on router.sock.

**Rules that keep it safe.**

- A credential shaped like a relay key (`rmk_…`) that does not validate is a
  **401 on every branch** and is never forwarded — revoked keys, and every key
  after a relay restart, included. On a passthrough route an `rmk_`-shaped
  value in `Authorization` or `x-api-key` is a 401 **whether or not it
  validates**: those headers are forwarded, and a relay credential never is.
- A **catalog that cannot be read is a 503** on every path that needs it, and
  "not in the catalog" is never read as "unmanaged, send it to Anthropic". A
  passthrough path route needs no catalog and is unaffected. No host, or an
  unreachable one, is a 503 on both branches.
- A caller with **no relay credential** at all gets a 401, not a 404, for
  anything the broker would have to serve or refuse, so it cannot tell which
  model names relay manages by the shape of the error. (The one exception is
  inherent to routing by catalog: a Claude-shaped id that is a `modelMap` key
  is 401 without a credential where an unmapped one is forwarded.)
- Passthrough is not a relay-managed model, so `allowed_models` does not apply
  to it. A per-project "may use passthrough" rule is not built.
- Relay never forwards its own credentials, and forwards the client's only on
  passthrough routes.

**Audit.** A passthrough call is a `model_call` like any other, with
`model_target` `passthrough:<api|chatgpt|openai|anthropic>`, its status and
byte counts, and an anonymous actor unless it carried a valid `X-Relay-Key`. A
non-streamed passthrough response is not buffered for token usage; a streamed
one is scanned as usual.

**Listener.** Claude Code and Codex need a URL, so the TCP listener must be on
(`"model_endpoint": {"listen": "127.0.0.1:9911"}` in `settings.json`; loopback
only). Over TCP there is no peer identity, so `X-Relay-Key` (or the legacy
bearer) is the only relay authentication.
