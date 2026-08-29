# Relay (Go)

macOS MCP orchestrator and project manager. Tray app with project-scoped auth, a
Unix-socket bridge, an external-MCP proxy (stdio + HTTP/OAuth), and background
service management.

## Modes

- `relay` — tray app (default). Hosts the bridge socket, manages services and projects, shows the settings UI.
- `relay mcp --token TOKEN` — stdio MCP server. Connects to the bridge; the token determines visible MCPs/tools.
- `relay mcp call --token TOKEN --list | --tool NAME [--args '<json>']` — one-shot list/invoke over the bridge (also spelled `relay mcpExec`). No long-lived session; handy for agents.
- `relay mcp register|unregister|list` — external MCP management.
- `relay service register|unregister|restart|list` — service self-registration. `restart` sends `ReloadService`; the tray does Stop → Start in place.
- `relay audit [--tail N] [--project ID] [--outcome denied] [--grep TEXT] [--json]` — tail the tool-call audit log. Reads the file directly, so it works with the tray stopped.
- `relay grant [--project ID] [--json]` — the operator-side "what did I actually grant?": every record's MCPs, mode, outbound grant, tools and the **real** scope values, with a scope reaching a filesystem root or a whole home directory called out. Reads settings.json directly, like `relay audit`. `disclose` governs the client's view and never this one (issue #41).
- `relay credential mint --name NAME --class CLASS [--class ...] [--ttl 12h] | list [--include-expired] | revoke --id ID` — control-plane API credentials (ADR-015, ADR-016). `--class` is one of `read`, `configure`, `grant`, `execute`, `proxy`; an unknown class or an empty set is refused. `--ttl` gives the credential an expiry; omitted means never. The plaintext token is printed once and only its SHA-256 is stored. Reserved: `legacy-frontend-token`, which the frontend-token migration owns.
- `relay enrol create --client-id ID --grant PROJECT_ID [--grant ...] | list | revoke --client-id ID` — remote-client enrolment. Signs a client certificate off relay's own CA and emits a bundle to copy to the client machine. Host-side operator act only: no self-service enrolment, no bootstrap token.
- `relay login enrol | list | revoke --id ID` — host-side anchor for interactive passkey login (ADR-016). `enrol` mints a single-use, two-minute registration code (only its SHA-256 is stored; the code is printed once and is never accepted in place of an assertion) and prints where to redeem it; `list` shows registered passkeys — name, abbreviated credential id, created, last-used counter — never the public key; `revoke` removes one (and does **not** end sessions it already signed in — those are `relay credential revoke`, or Settings → Passkeys). The code is also mintable from the tray's **Show Login Code...** item, which goes through the same `mintBootstrapCode`. Not a control-plane credential and not a fifth/sixth entry in `docs/tokens.md`'s inventory: it authorises registering a passkey, nothing else.

## Architecture

Relay is the container: it owns projects, MCPs, services, and the user-facing
front door. Service-specific knowledge lives in services, not in relay. Each
enhanced service (relayLLM, relayScheduler, …) declares a manifest describing the
routes it serves; relay's front-door dispatcher routes inbound traffic
accordingly. Protocol: [`docs/service-manifest.md`](docs/service-manifest.md).

Request flow: Browser → Eve → relay frontend socket → dispatcher → matching
enhanced service. When an LLM in that service calls a tool: service's MCP client
→ `relay mcp` subprocess (project token) → bridge socket → router (auth, filter,
`_meta` inject) → actual MCP server (fsMCP, macMCP, …) → result back up the chain.

### Key files

```
main.go                  Entry + command dispatch (relay / mcp / mcpExec / service)
trayapp.go               App lifecycle, menu, settings IPC, ToolRouter wiring
settings.go              Config, project CRUD, permission derivation
settings_store.go        Atomic settings.json read/write
types.go                 Project, StoredToken, ExternalMcp, ServiceConfig (Settings lives in settings.go)
tokens.go                hashToken, auth sentinel errors
project.go               Project + token creation
project_routes.go        HTTP project routes; shares Settings mutators with ipc_projects.go
project_dto.go           projectView DTO — strips the token from every response except rotate
context_schema.go        MCP contextSchema vocabulary (scope/source/applies_to), McpSurface, the tool-name matcher
router.go                Bridge auth (service vs project tokens), tool filtering, access mode, scope presence, _meta injection
audit.go                 Tool-call audit log: event model, async writer, ring, redaction, query
audit_call.go            Nil-safe per-call event builder used by the router instrumentation
audit_cmd.go             `relay audit` CLI
audit_issuance.go        credential_issued / credential_revoked: the record every mint and revoke writes,
                         the CLI's own append-only recorder, and the fail-closed rule for issuance
grant_cmd.go             `relay grant` CLI — the operator's view of a record's effective grant
scope_breadth.go         How much of the host one scope value reaches (root / home / bounded)
enrolment.go             Enrolment CRUD, grant validation, revocation + its live-connection hook
enrolment_ca.go          Relay's self-signed CA: lazy generation, client/server cert issuance, fingerprints
enrol_cmd.go             `relay enrol` CLI
capability.go            CapabilityClass, Transport, RouteRegistrar — the one door every control-plane route registers through (ADR-015)
api_credential.go        APICredential CRUD, the frontend-token migration, credentialAuthorizer
credential_cmd.go        `relay credential` CLI — mint/list/revoke control-plane credentials
login_ops.go             Bootstrap-code mint/consume, passkey + login-session views, LoginOps (the core the CLI, the tray item and the Passkeys tab share)
login_cmd.go             The `relay login` CLI (ADR-016 decision 2)
webauthn.go              WebAuthn verifier: registration + assertion, ES256 only, none attestation only
webauthn_cbor.go         CBOR decode via fxamacker/cbor, pinned to the CTAP2 canonical subset
webauthn_challenge.go    In-memory challenge table (single use, 60s) + the ceremony rate limiter
login_routes.go          The three unauthenticated /relay/login patterns and the door that serves them
login_document.go        The self-contained login page, served under a strict CSP
remote_server.go         Remote mTLS listener: two-entry dispatch table, cert→enrolment→grant, revocation hook
remote_reconcile.go      RemoteSupervisor: binds/moves/closes that listener as remote.* and audit.* change
external_mcp.go          stdio/HTTP MCP clients + runtime schema storage (McpConnection iface);
                         mcpSupervisor restarts a stdio child that dies (ADR-012)
wire_json.go             Verbatim JSON encoding for the outbound JSON-RPC frame (ADR-013)
http_mcp.go, oauth.go    HTTP transport + OAuth 2.1 (PKCE, dynamic registration, refresh)
mcp_cmd.go, exec_cmd.go, service_cmd.go   CLI subcommands
frontend_server.go       Front-door HTTP server; project routes local, rest falls through;
                         composes the public login mux in front of frontendCredentialAuth
frontend_dispatcher.go   Manifest-driven HTTP + WS dispatcher (longest-prefix match)
frontend_model_guard.go  Enforces a project's allowed_models before relayLLM sees the request
relay_llm_channel.go     Provisions the frontend socket + bearer token (filename legacy; contents are the generic FrontendChannel)
enhanced_services.go     In-memory registry of enhanced services; per-service reverse proxy
service_registry.go      Background process management + ephemeral service tokens
service_pidfile.go       Pidfiles under run/; enables orphan reclaim after a force-quit
service_status_client.go, service_status_poller.go   Generic per-service status polling + action dispatch
ipc_*.go                 Settings-UI IPC handlers (projects, services, mcps, service action/config, audit, enrolments, passkeys)
service_config_file.go   resolveConfigPath security gate for the manifest config editor
settings_html.go         Settings WKWebView HTML/JS
bridge/                  Unix-socket IPC (newline-delimited JSON); manifest.go holds Manifest/FieldDecl.
                         frameconn.go is the framing/scanner/deadline plumbing BOTH listeners share;
                         remote_request.go + remote_caller.go are the remote wire type and attested identity
mcp/                     MCP types + stdio server (proxies to the bridge)
```

## Projects

Projects are the primary infrastructure boundary in `settings.json`. Each binds:
path, allowed MCPs, allowed models, chat templates, scoped token, disabled tools,
and context.

- `allowed_mcp_ids: ["*"]` = all registered MCPs; explicit IDs to restrict.
- `allowed_models: ["*"]` = all models; explicit IDs to restrict.
- Permissions are derived at auth time from `allowed_mcp_ids` — not stored separately.
- `fs_bash` auto-disabled for filesystem MCPs; any `source: "project_path"` field
  in the MCP's context schema is auto-set to the project path.

Auth flow: `AuthenticateProject(plaintext)` → find project by token hash → derive
permissions from `allowed_mcp_ids` + registered MCPs → return a `StoredToken`
view with permissions + allowed_tools + access + disabled_tools + context.

### Four allowlists, each failing closed (ADR-011)

A grant answers four questions, and none of the four may widen another:

| question | field | notes |
|---|---|---|
| which MCP | `allowed_mcp_ids` | `["*"]` refused for a remote record |
| which tools | `allowed_tools` (MCP id → patterns) | anchored globs; `"*"` refused for a remote record; **absent means none for a remote record, all for a local project** |
| which operations | `access` (MCP id → `read`\|`write`) | admitted to `read` only on an explicit `annotations.readOnlyHint: true`; **absent means read for a remote record, write for a local project** |
| which resources | `context` → injected `_meta` | the MCP enforces it; relay cannot verify it — but relay must be able to **place** it |

The two asymmetric defaults are deliberate — the threat model differs, and a
local project written before these fields existed must keep working. The
resource scope is **not** given a default in either direction: a mode has a safe
wrong answer and a scope does not, so a `scope: "restrict"` field with no value
denies every call it governs, local projects included.

`disabled_tools` stays a **local-project** control and is refused on a remote
record, naming `allowed_tools` — an inert control reads on the screen as a
boundary and is not one. `checkToolAccess` still honours one that reaches it by
another route, because ignoring a denylist is the only direction that widens.

What each MCP may be narrowed by is read from its `contextSchema`:
[`docs/context-schema.md`](docs/context-schema.md).

**A scope relay cannot place is a scope relay cannot enforce.** Relay already
denies every call to an MCP whose `contextSchema` it cannot *read*; a value the
operator wrote that relay cannot *place* in that MCP's live schema is the same
condition from the other end and gets the same answer — denied, naming the
field and the MCP, with the MCP's tools withheld from every listing. It used to
drop the value and dispatch anyway, recording `scope=(none declared)`: relay
asserted a confinement in the profile, did not deliver it, and reported the
omission in the language of "there was nothing to apply". The audit record now
carries `scope_unplaced` and the two facts no longer share a string (issue
#42). v2 only — a v1 blob is injected verbatim, so nothing is dropped there.

**A count is not a measure of confinement** (`scope_breadth.go`, issue #41).
`disclose: "count"` renders `["/"]` and `["/Users/me/project"]` identically, so
a value whose entries resolve to a **filesystem root** is named as
`unrestricted (the whole filesystem)` at every `disclose` setting — the client
learns it the moment it lists `/`, so withholding it buys nothing. A **home
directory** is not named to the client (it is genuinely confined, and saying so
would disclose topology) but is loud on every operator surface. And the split
that made this a bug: **`disclose` governs what reaches the CLIENT and never
what relay shows the operator** — Settings → Projects, `relay grant`,
`relay audit --authority`, `_meta` and the audit JSONL all show the real value
unconditionally. The classification is a question about the *value*, never
about the field name; ADR-011 decision 3's no-registry rule is intact.

`allow_cwd_auth` (default false, per project) opts into a token-less fallback:
a caller with no token whose working directory is inside the project path
authenticates as that project via `AuthenticateProjectByPath`, with identical
scope. A present-but-invalid token never falls back. See
[`docs/tokens.md`](docs/tokens.md#directory-auth-allow_cwd_auth).

### Project kind: local vs. remote

`Project.Kind` (`ProjectKindLocal` / `ProjectKindRemote`) distinguishes a
host-directory project from a pure capability grant to a client on another
machine — e.g. an LLM agent running on a separate VM that reaches relay for
tool access instead of running on the host. Always test `proj.IsRemote()`,
never compare `Kind` against `ProjectKindLocal`: the zero value is local, so
every project written before this field existed round-trips unaffected, and
an equality check invites a future bug where an unset field reads as remote.

A remote project has no `Path` and cannot have anything that presumes a host
directory — `AllowCwdAuth`, `GenerateSkill`, `ShellTemplates`, and the
`allowed_mcp_ids: ["*"]` wildcard are all refused by `validateProjectShape`
(`project.go`), as is a non-empty `allowed_models` (an empty allowlist is the
only value `modelAllowedForProject` won't misread as "unrestricted"). A
MCP whose every tool needs the project path can't be granted to a remote
project either (`ValidateProjectGrants`) — and `SyncProjectToken`
independently refuses to derive any `source: "project_path"` field for one
regardless, since an MCP's schema is discovered at runtime and could gain
such a field after a grant was already validated. `appRouter.CallTool`
re-checks scope presence against the MCP's *live* schema, which is the only
one of the three defences that catches that upgrade. Sessions (`refuseRemoteSession`) and PTY
launches (`refuseRemotePty`) are refused at the point of use too, not just
at validation — see ADR-009 for why each of these is defended twice rather
than once.

See [ADR-009](docs/decisions/009-remote-projects.md) for the full reasoning.

### Remote client enrolment

An **enrolment** (`enrolment.go`, `settings.json` → `enrolments`) binds one
client certificate to the remote projects it may use. It is keyed by
*certificate, not by machine* — several agents on one VM each hold their own
enrolment, granted, audited, and revoked independently, and nothing may assume
one per machine. There is **no bearer token anywhere on this path**: a stolen
`settings.json` grants no remote access at all.

Relay is its own CA (`enrolment_ca.go`), generated lazily on first use and
persisted as `ca.key.sealed` (sealed, ADR-017) / `ca.crt` (clear, 0600) in the
config dir — not in `settings.json`, which is rewritten in full on every
mutation. The CA's private key is never written to disk as plaintext; only
the tray, holding the keychain key, can open it. Client certs are
long-lived because *revocation, not expiry, is the control*; revoking deletes
the record and fires `SetEnrolmentRevocationHook` so the listener can close
live connections.

Grants are validated at enrolment (`ValidateEnrolmentGrants` — every grant must
name a project with `IsRemote()` true) and at conversion
(`ValidateProjectEnrolments` — remote→local is refused while any enrolment
grants the project, naming the offenders), and a third time at call time by the
listener (`RemoteServer.resolveGrant` re-checks `IsRemote()` immediately before
dispatch, so a grant that went stale by any route relay did not anticipate
fails closed).

### The remote listener

`RemoteServer` (`remote_server.go`) is a **second listener beside**
`BridgeServer` — never a mode of it. Its dispatch table (`remoteHandlers`) has
exactly two entries, `ListTools` and `CallTool`: the other eight bridge request
types have no code path from a remote connection at all, so a new admin op is
unreachable from a VM until someone deliberately adds it to a list that is
visibly a security boundary.

Mutual TLS against relay's own CA (`tls.RequireAndVerifyClientCert`). The peer
certificate is fingerprinted and resolved to an enrolment **before any request
is read** — an unenrolled certificate is closed without processing, so it
cannot probe. The resolved identity goes into the context via
`bridge.WithRemoteCaller`, which is what puts every call on the fail-closed
audit path. The wire type (`bridge.RemoteRequest`) carries only
`type` / `name` / `arguments` / `project_id`; there is no token and no cwd, and
decoding is strict (`DisallowUnknownFields`) so a client sending `cwd` gets a
loud error rather than silent divergence. The project token is resolved
host-side from the granted project and never appears on the wire.

Every remote call is budgeted (`enrolment_budget.go`). Each enrolment carries a
rolling-window call-rate and result-volume cap, enforced in `appRouter.CallTool`
and refused with the `throttled` outcome — distinct from `denied` (a tool the
grant never included) and `tool_error` (a boundary inside the MCP) because it is
the only one of the three that says the grant was legitimate and the *pattern of
use* was not. Budgets live on the **enrolment, not the project**: the enrolment
is the unit of compromise, so it is the unit that bounds one. Rate is checked
before the MCP runs; volume is necessarily charged after a call returns, so the
guarantee is "at most one call's worth over the cap", not a hard ceiling. Local
callers are not budgeted at all — one context lookup and nothing else.

Auditing is a hard dependency of remote access: with `audit.enabled: false` the
listener refuses to start rather than serving unrecorded calls. The case for
letting a VM reach host mail rests on detection, so there is deliberately no
window in which a remote call runs without a record.

Config — absent block means **no listener at all**, and the default binds
loopback so misconfiguration cannot expose the control plane to a LAN:

```json
"remote": { "enabled": true, "listen": "127.0.0.1:9910" }
```

The listener **refuses to start when auditing is disabled**: a remote grant is
justified by the calls it records, so serving remote traffic unrecorded is not
a degraded mode. Local tooling is unaffected. It also sets read+write deadlines
(inactivity, not a cap on work) and keeps a connection table keyed by
fingerprint so `SetEnrolmentRevocationHook` closes a revoked client's *live*
connections.

**The listener follows settings; it is not frozen at startup.**
`RemoteSupervisor` (`remote_reconcile.go`) converges on every settings poll and
on every bridge-driven reconcile: it binds when the block is enabled, moves when
`listen` changes, and closes when the block is disabled *or auditing stops being
live* — so `audit.enabled: false` is a refusal at runtime and not only at
launch. Convergence can never open a listener the configuration does not
explicitly ask for (absent block and omitted `enabled` both resolve to
disabled). A rebind binds the new address **before** closing the old listener,
so a failed bind leaves the old one serving and says so loudly rather than
leaving nothing behind and no error; live connections on the old address are
then closed deliberately, because `listen` is the reachability control and a
narrowed bind that left old sessions running would not have narrowed anything.
The revocation hook is *owned* (`SetEnrolmentRevocationHookFor` /
`ClearEnrolmentRevocationHookFor`) so a replaced listener's teardown cannot
uninstall the live listener's hook.

**Every authorization read on this path goes through `freshSettings`, never
`store.Get()`.** `relay enrol create|revoke` runs in a CLI *process*, so a
cached settings view made a newly created enrolment look unenrolled until the
tray's next poll — indistinguishable from a genuine misconfiguration (issue
#21). `RemoteServer.currentSettings` stats `settings.json` and re-reads only
when it moved, which is what makes creation as immediate as revocation already
was, per request and per connection.

See [ADR-010](docs/decisions/010-remote-client-transport-and-identity.md).

## Service manifest (enhanced services)

Every spawned service gets `RELAY_BRIDGE_SOCKET` + `RELAY_SERVICE_ID`. Services
that implement the protocol dial the bridge with a `RegisterManifest` payload
declaring (a) the routes they serve, (b) their internal Unix socket + bearer
token, and (c) optional status endpoint, actions, and config editor. Generic
services ignore the env vars; relay never dispatches to them.

The dispatcher does longest-prefix match on registered routes and proxies to the
service's internal socket using its declared token; WS upgrades share the same
handler. The protocol is intentionally minimal — no version, no capability
declarations, no service-ID hardcoding anywhere in relay. Full spec:
[`docs/service-manifest.md`](docs/service-manifest.md).

## Security

The five-credential model (full inventory: [`docs/tokens.md`](docs/tokens.md);
the flow end to end, with worked examples:
[`docs/auth-flow.html`](docs/auth-flow.html); brokering rationale: ADR-007):

- **Project token** (`RELAY_PROJECT_TOKEN`) — the security boundary, scoped to a project's allowed MCPs/tools. Sealed at rest (ADR-017; `docs/sealed-config.md`) alongside a clear SHA-256 hash inline in the project. **Relay is the sole broker:** Eve references projects by id only (the DTO strips the token from every response except rotate); relayLLM resolves the token just-in-time from the bridge by `projectId`, injects it into spawned children, and never stores it or accepts it from Eve.
- **Service token** (`RELAY_SERVICE_TOKEN`) — ephemeral, in-memory, full bridge access; lets a service authenticate its own bridge calls. **Never injected into a spawned child** — if a project token can't be resolved, the child gets no token (fail closed).
- **Frontend token** (`RELAY_FRONTEND_TOKEN`) — frontend consumers dial `RELAY_FRONTEND_SOCKET` (0600), bearer-checked on every HTTP + WS before dispatch. It is no longer a credential of its own: relay records it as the `legacy-frontend-token` **control-plane credential** on every start, so it reaches exactly `read`+`configure`+`proxy`. Injected only into frontend consumers (`service register --no-frontend-creds` keeps it out of backends).
- **Control-plane credential** (`settings.json` → `api_credentials`) — the API's authenticator (ADR-015). Names an explicit set of `read` / `configure` / `grant` / `execute` / `proxy`; absent means **nothing**, never everything. `frontendCredentialAuth` resolves any bearer to one of these before a handler runs (no credentials at all fails closed), and `RouteRegistrar` then checks the route's class — the first asks "is this anyone?", the second "may they do this?". `execute` and `proxy` routes are absent from the TCP mux entirely, not refused on it. Mint with `relay credential mint --name N --class read [--class …]`; the plaintext is printed **once**. A consumer that needs `grant` — including `POST /api/projects/{id}/rotate_token` — or `execute` over HTTP must mint its own.
- **Enhanced internal bearer** — each service picks its own internal socket + token and declares both via the manifest; relay strips inbound `Authorization` and injects the service-declared token when proxying.

**The proxied surface has a class of its own, and it is socket-only** (ADR-016
decision 4). The `/` catch-all is the one mount whose blast radius relay
cannot see — what it reaches is whatever a manifest declares — so it is named
`proxy` rather than mislabelled as configuration. A `configure` credential no
longer reaches relayLLM's sessions, terminals or `/ws`, and the browser-facing
loopback bind reaches no proxied route at all: that is issue #50's guarantee
restored. Eve and relayScheduler dial the **socket** and the legacy migration
grants them `proxy`, so nothing relay injects is affected; a hand-minted
`configure` credential that relied on the catch-all must be re-minted. With no
catch-all on the TCP mux to absorb it, a near-miss like `POST /api/services`
there is a 405 from `http.ServeMux` rather than a proxied request — logged
(`slog.Warn` with method, path and transport, inside `frontendCredentialAuth`)
and deliberately not audited: an unregistered route is not an authorization
decision, and a `ControlDecision` would put an attacker-drivable write on the
listener ADR-015 decision 2 leaves empty.

**A service may not claim a route relay serves.** `RouteRegistrar` accumulates
relay's own route set as it registers it, and
`EnhancedServiceRegistry.checkRouteConflictsLocked` refuses any manifest route
whose path space overlaps one — a hand-maintained list would drift the first
time someone added a route. A wildcard pattern reserves its subtree; the `/`
catch-all is excluded, being the mount services are reached through rather
than a path relay serves. `/relay/` stays reserved separately, since the login
routes register outside `RouteRegistrar`. Rules and the operator-visible
error: [`docs/service-manifest.md`](docs/service-manifest.md).

**A credential may expire, and absent means never** (ADR-016 decision 3).
`--ttl` writes an RFC3339 `expires`; a record without one round-trips exactly
as it did before the field existed. An `expires` relay cannot parse reads as
**expired**, and an expired credential is refused *identically* to an unknown
one — a distinguishable answer would be an oracle for which credentials exist.
Expired records are reaped lazily, inside the same `store.With` as the next
mint, never by a timer. This is not a reversal of `enrolment_ca.go`'s
revocation-over-expiry choice: an enrolment is long-lived and revoked, a login
credential is short-lived by design and renewed by another ceremony. See
[`docs/tokens.md`](docs/tokens.md#expiry).

**External MCPs are supervised children** — one stdio connection per MCP id,
shared by every access profile that names it, and therefore restarted when it
dies rather than left down. `mcpSupervisor` (`external_mcp.go`) waits on the
reader goroutine, backs off exponentially, and caps restart *intensity*: a child
that stays up for `MCPRestartStableWindow` resets the counter, so an MCP that
dies occasionally is recovered forever while one that dies on every spawn is
abandoned after `MCPRestartMaxAttempts` and says so. A respawn is a **full**
start — `connectStdio` is the only path, and it re-runs the handshake and the
context-schema discovery before publishing, because a callable MCP whose schema
relay has not read yet fails *open*: `ParseContextSchema(nil, 0)` requires no
scope field and strips every stored context key. Tools and schema are installed
and the connection published in ONE critical section, so that state is
unrepresentable rather than merely unlikely. In-flight calls are failed, never
replayed — relay restores the capability, not the call. A frame longer than
`bridge.MaxMessageSize` fails only the call it answers and the stream resyncs to
the next newline. Every death, recovery, and abandonment is an audit row
(`relay audit --kind relay`). See ADR-012.

**Tool-call audit log** — every call, denial, and auth failure is recorded at
`appRouter.CallTool`, the single chokepoint every transport funnels through.
Attribution comes from relay's own auth resolution (project id) and the kernel
(peer pid off the bridge socket), never from the caller. Arguments are redacted
and capped; results are metadata-only unless explicitly opted in. For a local
caller the sink fails open and shows its drop count rather than stalling a tool
call; for a remote one it is fail-closed — an `intent` record is written and
flushed before the MCP runs, a `completion` record with the same `id` follows,
and a call whose intent cannot be recorded is refused (ADR-010 decision 5).
**Auditing is on by default**: an absent `audit` block resolves to enabled with
the rotation caps applied, so a fresh install records without being configured,
and only an explicit `"enabled": false` turns it off — which costs the remote
listener (ADR-010) and every `control_decision` (ADR-015). A new install writes
the block out explicitly so the file says what relay is doing.
**Every act that issues or revokes a credential is recorded too**
(`audit_issuance.go`): `credential_issued` / `credential_revoked`, naming what,
its identifier, the class set or grant, and which door it came from — CLI, the
Settings window, the tray menu, or HTTP. A `control_decision` says a caller was
allowed to reach `rotate_token`; it does not say a token was rotated, and most
issuance is a CLI process that reaches no route at all. **Issuance is
fail-closed** on ADR-010 decision 5's argument: the record is written and
synced before the secret reaches anyone, and an act that cannot be recorded is
refused — the plaintext withheld, an unrecorded enrolment revoked, an
unrecorded passkey removed. **Revocation is not**, because refusing to narrow a
grant when the log is broken is the worse failure; it is loud instead. A CLI
process appends with a recorder of its own and never rotates, so it cannot
rename the log out from under the tray's open descriptor.
Viewer: Settings → Tool Calls, or `relay audit` (`--kind remote` for anything a
VM did). Full reference: [`docs/audit-log.md`](docs/audit-log.md); rationale:
ADR-008, narrowed for remote callers by ADR-010, widened by ADR-012 with the
`mcp_down` / `mcp_up` records relay writes about itself.

**What relay modifies about a call it forwards: nothing** (ADR-012). Tool
arguments travel as `json.RawMessage` from the wire, through authorization, to
the MCP and into the audit log — relay validates that they are JSON and decides
whether the call is allowed, and never decodes them into Go values. It cannot:
a round trip through a Go `string` substitutes U+FFFD for a lone UTF-16
surrogate, which is legal JSON and which fsMCP refuses on purpose, so relay was
silently repairing the corruption its downstream was built to catch (issue #40).
The same round trip also sorted object keys, collapsed duplicate keys and
reformatted numbers. **Inspect a `RawMessage` for a decision; forward the
original bytes.** The audit log holds those same bytes — redaction of a
credential-like value is the only rewrite — because a log that paraphrases what
a client sent is not ground truth. The one thing relay changes on purpose is
insignificant whitespace: the stdio transport is newline-delimited, so a
caller's pretty-printed arguments must lose it or the frame breaks.

"Byte for byte" has **one measured exception** (ADR-013): Go's encoder re-spells
a raw U+2028/U+2029 inside a `json.RawMessage` as `\u2028`/`\u2029` even with
`SetEscapeHTML(false)`, and there is no seam short of hand-assembling the frame.
It decodes back to the same character, which is the line ADR-013 draws — relay
may not change what a document means, and does not claim to preserve how it was
spelled. Pinned by `TestCallTool_UnicodeLineSeparatorsAreReSpelledButNotChanged`.

That exception is **load-bearing for `_meta.args_sha256`**, so it is no longer
merely cosmetic. A client computing that hash must canonicalise through the same
encoder relay uses, not `json.Compact`: hashing the raw spelling while relay
forwards the re-spelled one makes the MCP hash bytes the client never hashed, and
fsMCP refuses a legitimate call with `integrity_failed` — U+2028 is ordinary in
JavaScript. relayRemote's `compactArgs` is the reference for getting this right.
Relay itself never recomputes the hash: it forwards the client's verbatim, since
a hash relay derived from arguments relay already holds would validate relay
against itself.

**TCC permissions** — relay holds the personal-information entitlements
(`Relay.entitlements`) and fires the prompts from its own process; MCPs declare
what they need with `--tcc-services foo,bar` and inherit relay's grants via TCC's
responsible-parent attribution at runtime. Rationale + checklist for adding a TCC
service: ADR-005.

## Settings UI

IPC: `ipc(json)` → `window.webkit.messageHandlers.ipc.postMessage`. Tabs:
Services, MCP Servers, Projects, Remote Clients, Passkeys, Service Inspector,
Tool Calls.

The Remote Clients tab (`ipc_enrolments.go`) lists every enrolment beside the
grants it reaches — by project *name*, with the certificate fingerprint in full
— and reads/writes the `remote` block. Creating an enrolment returns the bundle
**directory** only: the client private key inside it never crosses the IPC
boundary.

The Passkeys tab (`ipc_login.go`) is the Remote Clients tab's shape applied to
interactive login (ADR-016): registered passkeys with name, abbreviated
credential id, creation time and last sign count — **never** the public key,
which `passkeyView` has no field for — and beneath them the live browser
sessions those passkeys minted, each with its own Sign out. The two lists are
one screen because revoking a passkey stops the *next* login and does nothing
to a credential it already issued; a tab showing only the first would let
"revoked" read as "signed out" for up to twelve hours. Sign out goes through
`revokeAPICredentialIf` with a login-only gate inside the same `store.With` as
the delete, so the WebView can never revoke an operator's own long-lived
credential — that stays `relay credential revoke`.

The tray's **Show Login Code...** item is the second presentation ADR-016
decision 2 allows for the bootstrap anchor. It mints through the same gated
core method `relay login enrol` uses (`LoginOps.MintBootstrap`, ADR-017
decision 3 — a presence prompt either way) and shows the code in the Settings
window, because relay is `LSUIElement` and that window is the only surface
the tray has. A window that is not open yet gets the code seeded into its
first paint (`renderSettingsDocument`); one already open gets an emit — Cocoa
drops a script evaluated against a WebView that does not exist yet, and never
reloads a window that does. Minting replaces rather than accumulates, so the
panel says out loud that showing another code kills this one. The item is
never the *only* source: the menu is unreachable from the hermetic tier,
which is why `relay login enrol` stays as a second door — though both now
demand the same presence prompt and both refuse identically over SSH, so
neither reaches a fully headless install (`docs/tokens.md`).

The Projects tab is native and co-equal with Eve's project dialog — both hit the
same `Settings.*Project*` mutators (relay via `ipc_projects.go`, Eve via
`project_routes.go`), so HTTP and IPC paths are interchangeable. Cross-process
changes propagate live: an HTTP project mutation fires `onProjectsChanged`, which
re-renders an open Settings window. See ADR-004.

## Ecosystem

First-party services are reference implementations of the manifest protocol — no
privileged path in relay.

- `../relayLLM/` — LLM execution engine. Its manifest (see relayLLM's `manifest.go`) covers sessions, terminals, models, permission, status, generated assets, local-model (llama/mlx) management, and `/ws`.
- `../eve/` — browser frontend; dials relay's frontend socket.
- `../relayScheduler/` — task scheduler; registers `/api/tasks/*`, dispatched directly.
- `../relayTelegram/` — Telegram bot bridge.
- `../macMCP/` — Swift, macOS-native tools.
- `../fsMCP/` — TypeScript file system tools; uses `_meta.allowed_dirs` for scoping.

## Build

```bash
./build.sh              # build + install /Applications/Relay.app and launch it
./build.sh --test       # run the hermetic suite first; abort install on failure
./build.sh --release    # sign + notarize + emit /tmp/Relay.dmg (Developer ID required)
```

`--test` and `--release` may be combined; `--release` implies `--test`. Requires
a recent Go toolchain (see `go.mod`) and macOS.

## Testing

**Headline rule:** no test may read or mutate the real user config directory
(`~/Library/Application Support/relay/`). Tests route through
`mkSandboxRelayHome(t)` (in `support_test.go`), which redirects
`bridge.ConfigDir()` to a per-test temp dir under `/tmp` (via `mkShortTempDir`,
which sidesteps the 104-char Unix-socket path limit) populated from
`test/fixtures/relay-home/`. The `support_safety_test.go` guard fails the suite
if anything in the real ConfigDir changes during a run. The suite expects
relay stopped: a running instance legitimately rewrites `settings.json` there
on its own schedule and will trip this guard for a reason that has nothing to
do with the code under test.

### Three tiers

| Command | What runs | When |
|---|---|---|
| `go test ./...` | Hermetic suite — pure Go, no spawned binaries, no user files | Every commit (pre-commit hook) |
| `go test -tags=live ./...` | Spawns real binaries end-to-end: the `../relayLLM` binary, and a headless Google Chrome that runs the passkey login ceremony against the real `/relay/login` document (`webauthn_browser_live_test.go`) | After relay↔relayLLM boundary changes; after any change to the WebAuthn verifier, the login routes or the login page |
| `go test -race ./...` | Hermetic suite + race detector | Pre-push hook; before merging concurrency changes |

Install the hooks once per clone: `git config core.hooksPath .githooks`.

### Adding a test

1. Pick the tier (ADR-001). ~95% belong in the default hermetic tier.
2. Reading/writing settings, pidfiles, logs, or the bridge socket → call `mkSandboxRelayHome(t)` first.
3. Need a working router → `newTestRouter(t, settings, mgr)`.
4. Exercising a manifest-registering service → `NewFakeService(t, FakeServiceOptions{...})`. The relayLLM contract is covered by `integration_fake_relayllm_test.go`.
5. Need a real spawned subprocess → the `cmd/testservice` / `cmd/testmcp` binaries, built on demand via `buildTestServiceBinary(t)` / `buildTestMcpBinary(t)`, never an `exec.Command` mock.
6. Live-tier tests carry `//go:build live` and `t.Skip` gracefully when the
   real binary they need is absent — `../relayLLM` unbuilt, or Google Chrome
   not installed. A developer without one must see a skip, never a failure.
7. The WebAuthn verifier is covered twice on purpose (ADR-016 decision 8):
   `webauthn_test.go`'s software client owns every negative case in the
   hermetic tier, and `webauthn_browser_live_test.go` runs exactly one
   ceremony in a real Chrome — the only evidence that relay agrees with a
   user agent it did not also write. Neither covers real authenticator
   hardware or Safari; both gaps are named in `docs/testing-roadmap.md`.

### Not covered by the suite

- Cocoa tray UI (menu, dock) — exercise via `scripts/demo.sh`.
- Real `launchd` integration — `service_registry` is tested against `cmd/testservice`.
- Live OAuth round-trips — `oauth_test.go` covers PKCE/dynamic registration in isolation.
- Notarization / code-signing — exercised by `./build.sh --release`.

ADRs: see [`docs/decisions/`](docs/decisions/). Cross-repo test status:
[`docs/testing-roadmap.md`](docs/testing-roadmap.md).
