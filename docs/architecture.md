# Relay architecture and design rationale

How relay is put together and why: the request flow, the file map, the project
and grant model, remote enrolment and the remote listener, the service manifest,
the session host, the credential model and the Settings UI.

This is the durable *why*. Decisions are cited as ADR-NNN throughout the code
and docs; those numbers are labels for the reasoning recorded here and in the
topic documents beside this one — there is no separate ADR directory. The
command line is documented in [`cli.md`](cli.md), testing in
[`testing.md`](testing.md), and what lives in which package (and the structural
tests that pin it) in [`package-layout.md`](package-layout.md).

## Architecture

Relay is the container: it owns projects, MCPs, services, and the user-facing
front door. Service-specific knowledge lives in services, not in relay. Each
enhanced service (relayLLM, relayScheduler, …) declares a manifest describing the
routes it serves; relay's front-door dispatcher routes inbound traffic
accordingly. Protocol: [`docs/service-manifest.md`](service-manifest.md).

Request flow: Browser → Eve → relay frontend socket → dispatcher → matching
enhanced service. When an LLM in that service calls a tool: service's MCP client
→ `relay mcp` subprocess (project token) → bridge socket → router (auth, filter,
`_meta` inject) → actual MCP server (fsMCP, macMCP, …) → result back up the chain.

### Key files

```
main.go                  Entry + command dispatch (relay / mcp / mcpExec / service)
trayapp.go               App lifecycle, menu, settings IPC, ToolRouter wiring
project_routes.go        HTTP project routes; shares Settings mutators with ipc_projects.go
project_dto.go           projectView DTO — strips the token from every response except rotate
host_routes.go           HTTP host routes (docs/ssh-hosts.md) — GET/POST/PUT/DELETE /api/hosts[/{id}], probe, disconnect
host_ops.go              HostOps: the ungated core host_routes.go and ipc_hosts.go share; runs sshhost.Probe and writes the host.probe audit event
router.go                Bridge auth (project tokens, launch identity by capability, directory auth), Hello, tool filtering, access mode, scope presence, _meta injection
audit_call.go            Nil-safe per-call event builder used by the router instrumentation; reads AuditRecorder
                         via RedactCallArgs/PreviewResult rather than its unexported config
audit_cmd.go             `relay audit` CLI
audit_start.go           Wires the audit engine to what it does not own: relay's log directory and log rotation
audit_issuance.go        The gate-facing half of issuance: IssuanceAuditor, requireIssuanceAuditor (ADR-017 §7.4,
                         and MUST stay an unqualified identifier in this package — gate_ast_scan_test.go matches
                         its call sites as a bare *ast.Ident), recordEnrolmentIssued/recordBootstrapIssued/etc.,
                         and the CLI's own append-only recorder
sandbox_cmd.go           `relay sandbox` CLI (docs/sandbox-command.md): raw-mode terminal attach to a launched session
sandbox_attach.go        Server half of `relay sandbox`: cwd -> project, the launch through sessionRouteDeps.launch, the /ws viewer pump
grant_cmd.go             `relay grant` CLI — the operator's view of a record's effective grant (rendered from `grant.view`)
admin_read_ops.go        The ungated admin_op reads (`*.list`, `grant.view`): the running tray is the only reader of
                         configuration for a CLI command; each answers with a purpose-built view, never Settings, so
                         no hash, sealed value, env value or key coordinate crosses the bridge
enrolment_ops.go         EnrolmentOps: the gated, audited core the CLI, HTTP and IPC doors share
enrol_cmd.go             `relay enrol` CLI
api_credential.go        APICredential CRUD, the frontend capability class set, legacy-frontend-token retirement, credentialAuthorizer
credential_cmd.go        `relay credential` CLI — mint/list/revoke control-plane credentials
login_ops.go             Bootstrap-code mint/consume, passkey + login-session views, LoginOps (the core the CLI, the tray item and the Passkeys tab share) — holds the presence.Gate, stays in main
login_cmd.go             The `relay login` CLI (ADR-016 decision 2)
login_routes.go          The three unauthenticated /relay/login patterns and the door that serves them
login_document.go        The self-contained login page, served under a strict CSP
webauthn_browser_live_test.go   Black-box HTTP+Chrome ceremony test via lrServer; never touches verifier internals, so it stayed in main rather than moving with login/
remote_server.go         Remote mTLS listener: two-entry dispatch table, cert→enrolment→grant, revocation hook
remote_reconcile.go      RemoteSupervisor: binds/moves/closes that listener as remote.* and audit.* change
mcp_ops.go               McpOps: the gated, audited core the CLI, HTTP and IPC MCP doors share
mcp_permissions.go       TCC permission probing; the darwin half reaches cocoa_darwin.go's cgo, so it
                         stays with the tray process rather than moving to mcpbroker/
mcp_cmd.go, exec_cmd.go, service_cmd.go   CLI subcommands
frontend_server.go       Front-door HTTP server; project routes local, rest falls through;
                         composes the public login mux in front of frontendCredentialAuth,
                         which admits a bearer or a socket peer's launch identity holding `frontend`
frontend_dispatcher.go   Manifest-driven HTTP + WS dispatcher (longest-prefix match)
frontend_model_guard.go  Enforces a project's allowed_models before relayLLM sees the request
relay_llm_channel.go     Provisions the frontend socket path (filename legacy; contents are the generic FrontendChannel)
enhanced_services.go     In-memory registry of enhanced services; per-service reverse proxy
log_rotate.go            RotatingWriter + serviceLogDir: shared by relay's own log, the audit log,
                         and every managed service's log via service.Registry.OpenLog
ipc_*.go                 Settings-UI IPC handlers (projects, services, mcps, service action/config, audit, enrolments, passkeys)
settings_html.go         Settings WKWebView HTML/JS
config/                  Settings (settings.go: Config, project CRUD, permission derivation),
                         SettingsStore/FileSettingsStore (store.go: atomic settings.json read/write),
                         the domain models — Project, StoredToken, ExternalMcp, ServiceConfig, Host,
                         APICredential, Enrolment, Passkey (models.go) — and config.HashToken plus the
                         CA file-naming constants (identity.go). Declares the gated mutators
                         gate_structural_test.go discovers every other package against. Depends on
                         bridge/sealed; the gate, the audit sink and every door stay in main.
control/                 CapabilityClass, Transport, RouteRegistrar — the one door every
                         control-plane route registers through (ADR-015)
bridge/                  Unix-socket IPC (newline-delimited JSON); manifest.go holds Manifest/FieldDecl.
                         frameconn.go is the framing/scanner/deadline plumbing BOTH listeners share;
                         remote_request.go + remote_caller.go are the remote wire type and attested identity
mcp/                     MCP types + stdio server (proxies to the bridge)
enrolment/               The enrolment domain: the record's CRUD/validation (enrolment.go), relay's
                         own CA (ca.go), CSR parsing (csr.go), the comparison code (sas.go) and the
                         per-enrolment budget ledger (budget.go). Depends on config/bridge/sealed;
                         the gate, the audit sink and every door stay in main.
project/                 The project domain — the unit a grant is scoped to: creation, token and
                         shape/permission validation (project.go, apply.go), the schema-driven
                         scope derivation and the updateProject* grant-shape mutators (scope.go,
                         context_schema.go), how broad one scope value is (scope_breadth.go), the
                         scope-value picker's policy (enumerate.go) and a remote's self-narrowing
                         rules (narrowing.go). McpSurfaces is the runtime MCP view it takes as a
                         parameter, so it never reaches the MCP manager. Depends on
                         config/enrolment; the presence gate, the router, the routes, the IPC
                         handlers and the DTO stay in main.
peertoken/               Reads a Unix-socket peer's kernel audit token (LOCAL_PEERTOKEN). A leaf
                         package, so presence/ (audit session) and bridge/ (launch identity) share
                         one reader.
service/                 Background service supervision: process lifecycle and the launch fd
                         (service_registry.go), restart-on-crash policy and state
                         (supervision.go, docs/service-manifest.md#restart-supervision) with an
                         injectable Clock for its backoff sleep (clock.go, real in production, a
                         FakeClock in tests), the launch-identity table Hello binds and every
                         identity lookup reads (launch_identity.go, docs/launch-identity.md),
                         pidfiles under run/ for orphan reclaim after
                         a force-quit (service_pidfile.go), generic per-service status polling and
                         action dispatch (service_status_client.go, poller.go), the manifest config
                         editor's ResolveConfigPath security gate (service_config_file.go), and the
                         exec.Cmd/env helpers (helpers.go, http.go) shared with external MCP
                         spawning. Depends on bridge/config; the frontend channel's lifecycle and log
                         rotation are main's, wired into Registry.FrontendEnv/OpenLog as callbacks so
                         the package never depends on either concrete type.
sshhost/                 SSH hosts (docs/ssh-hosts.md) — the one derivation of a Host into ssh
                         arguments: SSHArgv (the fixed-option argv prefix, ControlMaster shared
                         across relay/relayLLM/eve), RemoteCommand (decision 8's shell-agnostic
                         base64+eval remote command line, pinned byte-for-byte against the doc's
                         Fixtures by TestRemoteCommand_Fixtures), and Probe/Check/Disconnect, which
                         run over a `runner` exec seam (SetRunnerForTest) so no hermetic test in this
                         repo or a caller's ever shells out to a real ssh. ControlDir picks
                         `<relay data dir>/run/ssh` or a short `/tmp` fallback so ControlPath+%C never
                         overflows sun_path. Depends only on config/bridge.
audit/                   The tool-call audit log engine: the event/actor/config model, the async
                         writer, the in-memory ring, byte-level JSON redaction (ADR-012), AuditQuery,
                         and AuditOps (the read-only core behind both the HTTP and IPC audit doors).
                         Also the self-contained half of issuance recording (CredentialIssuance,
                         RecordIssuance, OpenCLIIssuanceRecorder) — the gate-facing half
                         (IssuanceAuditor, requireIssuanceAuditor, the recordEnrolment/Bootstrap/
                         ProjectToken/PasskeyIssued helpers) stays in main, because
                         requireIssuanceAuditor must stay an unqualified identifier for
                         gate_ast_scan_test.go's AST match to keep seeing its call sites. Depends on
                         config; log rotation is main's (log_rotate.go, shared with relay's own log
                         and every service's log) and reaches this package only via the OpenWriter
                         callback NewAuditRecorder/StartAuditRecorder take, the same pattern
                         service.Registry.OpenLog uses. The router instrumentation (audit_call.go)
                         and the CLI/HTTP/IPC surfaces (audit_cmd.go, audit_routes.go, ipc_audit.go)
                         stay in main, since they reach the router or unexported recorder state
                         directly.
mcpbroker/               The external-MCP client: the stdio and HTTP transports and their JSON-RPC
                         framing (external_mcp.go, http_mcp.go), the seatbelt launch decision
                         (mcp_sandbox.go), the OAuth 2.1 client relay authenticates to an upstream
                         HTTP MCP with (oauth.go — PKCE, dynamic registration, refresh; nothing to do
                         with relay's own login), the context/enumerate client (external_mcp_enumerate.go),
                         ADR-013's verbatim outbound encoder (wire_json.go) and the MCP/OAuth timeouts.
                         Manager owns one supervised connection per MCP id plus the runtime schema
                         table, and reports liveness through a SetHealthObserver callback so it needs
                         no audit dependency. Depends on bridge/config/jsonrpc/mcp/project/service.
                         Every door stays in main — McpOps (the gated core), mcp_routes.go, mcp_cmd.go,
                         ipc_mcps.go — as does router.go, which reaches this package only through the
                         ToolProvider/ToolManager interfaces, the ADR-012 audit translation
                         (audit_call.go) and mcp_permissions*.go, whose darwin half is cgo.
                         testseam.go is the only exported way into Manager's unexported connection and
                         schema tables (SetConnectionForTest / ConnectionForTest /
                         SetContextSchemaForTest); each panics outside a test binary, and cmd/relay's
                         router, audit and scoping tests are what need them.
login/                   The WebAuthn ceremony, pure: registration/assertion verification (webauthn.go,
                         ES256 only, none attestation only), CBOR decode pinned to the CTAP2 canonical
                         subset (webauthn_cbor.go), and the in-memory challenge table (webauthn_challenge.go,
                         single use, 60s). Depends only on crypto/*, encoding/*, errors, fmt, math/big and
                         internal/ceremonylimit — deliberately blind to the bootstrap code, the passkey
                         store and the presence gate, all of which stay in main (login_ops.go, login_routes.go).
                         loginfake/ is a separate, non-test package holding the software WebAuthn
                         authenticator cmd/relay's login_routes tests drive over real HTTP — split out
                         because a _test.go file's symbols cannot cross a package boundary, matching
                         internal/presence/presencetest's shape (never imported outside a test binary).
ceremonylimit/           A pure sync/time ceremony backoff (Limiter: Allow/RecordFailure/RecordSuccess),
                         with zero WebAuthn or enrolment knowledge. Shared by two unrelated ceremonies —
                         login/'s WebAuthnVerifier and cmd/relay's enrolment-request table
                         (enrolment_requests.go) — neither of which may import the other, so the backoff
                         lives in its own neutral package rather than in either.
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
[`docs/context-schema.md`](context-schema.md).

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

**A count is not a measure of confinement** (`internal/project/scope_breadth.go`,
issue #41).
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

`allow_cwd_auth`, the project field that used to opt into a token-less
working-directory-based fallback, is retired (plan-broker-and-sessions.md's
C3 decision) — a caller's asserted cwd is never authenticated, whether or
not it sends one. The retired mechanism is being replaced with kernel-
verified process ancestry (C3's membership check): a tokenless caller
authenticates only by *being* a real, provable descendant of a live
project session's root process, never by presenting or asserting anything.
A present-but-invalid token still never falls back to any tokenless path.
See [`docs/tokens.md`](tokens.md) and
[`plan-broker-and-sessions.md`](../../plan-broker-and-sessions.md) §2 C3.

### Project kind: local vs. remote

`Project.Kind` (`ProjectKindLocal` / `ProjectKindRemote`) distinguishes a
host-directory project from a pure capability grant to a client on another
machine — e.g. an LLM agent running on a separate VM that reaches relay for
tool access instead of running on the host. Always test `proj.IsRemote()`,
never compare `Kind` against `ProjectKindLocal`: the zero value is local, so
every project written before this field existed round-trips unaffected, and
an equality check invites a future bug where an unset field reads as remote.

A remote project has no `Path` and cannot have anything that presumes a host
directory — `GenerateSkill`, a non-empty `allowed_templates`, and the
`allowed_mcp_ids: ["*"]` wildcard are all refused by `project.ValidateShape`,
as is a non-empty `allowed_models` (an empty allowlist is the
only value `modelAllowedForProject` won't misread as "unrestricted"). A
MCP whose every tool needs the project path can't be granted to a remote
project either (`project.ValidateGrants`) — and the package's own token sync
independently refuses to derive any `source: "project_path"` field for one
regardless, since an MCP's schema is discovered at runtime and could gain
such a field after a grant was already validated. `appRouter.CallTool`
re-checks scope presence against the MCP's *live* schema, which is the only
one of the three defences that catches that upgrade. Chat sessions
(`refuseRemoteSession`) are refused at the point of use too, not just at
validation — see ADR-009 for why this is defended twice rather than once.

See ADR-009 (a remote project is a capability grant to a client on another machine, not a directory) for the full reasoning.

A project's directory can also live on another machine entirely, reached over
`ssh` rather than a VM reaching *in* — the mirror image of the remote-project
model above. That project is still `kind: local` in shape; it carries a
`HostID` naming a `Settings.Hosts` entry instead. `Kind == remote` and a
non-empty `HostID` are mutually exclusive — a host project has a real
directory, an access profile has none at all. See
[docs/ssh-hosts.md](ssh-hosts.md) for the full design, the wire
contract with relayLLM and eve, and why relay-brokered tools, mounts, cwd
auth and skill generation are all refused on a host project the same way
they are on a remote one, for related but distinct reasons.

### Remote client enrolment

An **enrolment** (`internal/enrolment`, `settings.json` → `enrolments`) binds one
client certificate to the remote projects it may use. It is keyed by
*certificate, not by machine* — several agents on one VM each hold their own
enrolment, granted, audited, and revoked independently, and nothing may assume
one per machine. There is **no bearer token anywhere on this path**: a stolen
`settings.json` grants no remote access at all.

**The client's private key is generated on the client, not on relay**
(ADR-018 decision 6 step 1). `relayremote enrol` generates the keypair and a
CSR; `relay enrol sign` (`enrolment.ParseClientCSR`, `RelayCA.SignClientCSR`) signs
the CSR's own public key and returns only certificates — the private key
never crosses to this host, and relay never writes one for a CSR enrolment
(`writeSignedCertBundle` refuses if `client.key` is already present in the
target directory). `relay enrol create` is the legacy path that still
generates the key on this host and emits it in the bundle; it stays
functionally unchanged for now (three doors — CLI, HTTP, the Remote Clients
tab — are not all CSR-ready yet) but is marked deprecated in its own CLI
output. `Enrolment.SPKISHA256` (CSR path only) refuses enrolling the same
private key twice under two client ids.

An unenrolled remote can also lodge its own CSR **over the network**,
instead of an operator carrying it by hand, through a third listener
(`enrolment_requests.go`, `enrolment_request_server.go`; ADR-018 decision
8). That listener's entire capability is two methods, `Lodge` and `Poll`,
over a bounded, in-memory, never-persisted table of at most 8 pending
requests — it holds no reference to a router, the CA, the sealer or
`settings.json`. **Lodging raises no prompt, ever**: no code on that path
touches `presence.Gate`, so an unauthenticated network peer can only make a
counter go up to its cap, never raise a dialog. **It may raise a
*notification*** (ADR-019 decision 4) — one coalesced, rate-limited (at most
one a minute, six an hour), dismissible tray banner carrying a count and
nothing a peer supplied, drawn by the tray's existing 2s poll *reading* the
lodge-generation counter, never by the lodge path *calling* anything. That
banner is discoverability, not a guarantee: it can be denied, suppressed by
Focus, or unavailable to a process with no bundle identifier, so the menu's
`Pending enrolment requests: N` line is the reliable surface and
`tray_notify.go`'s bounds are what make the banner safe rather than its
existence. The human approves from
`relay enrol requests`/`approve`/`refuse` or Settings → Remote Clients →
Pending requests, and *that* act reuses `enrolment.sign`'s existing gate and
digest unchanged — there is no `enrolment.approve` entry in
`presence.GatedOps`, deliberately, since a second op here would be exactly
the second door into issuance ADR-018 forbids. The digest binds the
*stored* CSR's public key, so a grant answered for one key is never
redeemable for another. The listener itself is plain TCP, not mTLS: nothing
on it is a secret in either direction (a self-signed CSR proves possession;
the certificates it returns are public), so TLS here would be decoration
that reads as a security property it cannot provide — the real control is
client-side and comes in two forms, one per client verb. `relayremote
request` pins the CA-fingerprint carried out of band (`--ca-fingerprint`, or
a watched `--tofu`), unchanged. `relayremote register` instead completes a
**commit–reveal comparison** (ADR-019 decision 3, `internal/enrolment/sas.go`): a
six-character code over relay's CA SPKI, the CSR's SPKI and a 16-byte nonce
from each side, the client's committed at lodge and opened on its first
poll. Both close the same gap — an attacker who lets a real CSR through to
relay and substitutes its own CA on the way back — and neither is optional:
a row that carries a commitment but was never opened, or whose open failed,
is refused by `EnrolmentOps.Approve` before the presence gate from every
door. `maxPendingEnrolmentRequests = 8` is now load-bearing for that bound
as well as for availability: it is the attacker's parallelism against 30
bits, so raising it degrades the margin linearly. See
[`docs/access-profiles.md`](access-profiles.md#approving-a-request-from-the-machine-itself)
and [`docs/install-remote-machine.md`](install-remote-machine.md).

Relay is its own CA (`internal/enrolment/ca.go`), generated lazily on first use and
persisted as `ca.key.sealed` (sealed, ADR-017) / `ca.crt` (clear, 0600) in the
config dir — not in `settings.json`, which is rewritten in full on every
mutation. The CA's private key is never written to disk as plaintext; only
the tray, holding the keychain key, can open it. Client certs are
long-lived because *revocation, not expiry, is the control*; revoking deletes
the record and fires `enrolment.SetRevocationHook` so the listener can close
live connections.

Grants are validated at enrolment (`enrolment.ValidateGrants` — every grant must
name a project with `IsRemote()` true) and at conversion
(`enrolment.ValidateProjectConversion` — remote→local is refused while any enrolment
grants the project, naming the offenders), and a third time at call time by the
listener (`RemoteServer.resolveGrant` re-checks `IsRemote()` immediately before
dispatch, so a grant that went stale by any route relay did not anticipate
fails closed).

### The remote listener

`RemoteServer` (`remote_server.go`) is a **second listener beside**
`BridgeServer` — never a mode of it. Its dispatch table (`remoteHandlers`) has
exactly two entries, `ListTools` and `CallTool`, and a second table
(`remoteConfigHandlers`, `DescribeGrant`/`NarrowGrant`), consulted only for a
`cli_admin` enrolment: the other eight bridge request types have no code path
from a remote connection at all, so a new admin op is unreachable from a VM
until someone deliberately adds it to a list that is visibly a security
boundary. The configuration table is itself filtered through the class–
transport matrix (`buildRemoteConfigHandlers`), so an `execute`- or
`proxy`-class entry added to it later is absent from the table rather than
refused inside it — see ADR-018.

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

Every remote call is budgeted (`internal/enrolment/budget.go`). Each enrolment carries a
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
"remote": { "enabled": true, "listen": "127.0.0.1:9910",
            "enrolment_requests": true, "enrolment_listen": "127.0.0.1:9911" }
```

The listener **refuses to start when auditing is disabled**: a remote grant is
justified by the calls it records, so serving remote traffic unrecorded is not
a degraded mode. Local tooling is unaffected. It also sets read+write deadlines
(inactivity, not a cap on work) and keeps a connection table keyed by
fingerprint so `enrolment.SetRevocationHook` closes a revoked client's *live*
connections.

`enrolment_requests` and `enrolment_listen` configure the third listener
(`EnrolmentRequestServer`, `enrolment_request_server.go`) on the same block:
`enrolment_requests` absent or `false` opens no enrolment socket at all —
opening that network door is a thing the operator says, not a thing relay
infers — and `enrolment_requests: true` with `enabled: false` is refused at
resolve time, naming why, since the request channel is a companion to the
tool-plane listener rather than a substitute for turning it on.
`enrolment_listen` defaults to `127.0.0.1:9910`'s neighbour, `127.0.0.1:9911`
— loopback, same reasoning as `listen`. That listener carries its own,
tighter bounds: at most 8 pending requests, a 15-minute TTL to be approved
and another 15 minutes to be collected after approval, 16 concurrent
connections, a 64-frames-then-redial cap per connection, and a 64 KiB frame
limit (`enrolment_requests.go`, `enrolment_request_server.go`) — sized for a
human walking to the Mac, an order of magnitude tighter than the login
challenge table's 64-entry, 60-second shape, because nothing on this
listener is machine-paced.

**The listener follows settings; it is not frozen at startup.**
`RemoteSupervisor` (`remote_reconcile.go`) converges **both** listeners after every committed
configuration change, on every bridge-driven reconcile and on a slow recovery tick: it binds each when
its half of the block is enabled, moves either when its own `listen` changes,
and closes either when its half is disabled *or auditing stops being live* —
so `audit.enabled: false` is a refusal at runtime and not only at launch, for
the enrolment-request listener exactly as for the tool-plane one. Convergence
can never open a listener the configuration does not explicitly ask for
(absent block and omitted `enabled` both resolve to disabled). A rebind binds
the new address **before** closing the old listener, so a failed bind leaves
the old one serving and says so loudly rather than leaving nothing behind and
no error; live connections on the old address are then closed deliberately,
because `listen` is the reachability control and a narrowed bind that left
old sessions running would not have narrowed anything. The revocation hook is
*owned* (`enrolment.SetRevocationHookFor` / `enrolment.ClearRevocationHookFor`)
so a replaced listener's teardown cannot uninstall the live listener's hook.

**Every authorization read on this path goes through `freshSettings`, never
`store.Get()`.** `relay enrol create|revoke` runs in a CLI *process*, so a
cached settings view made a newly created enrolment look unenrolled until the
tray's next poll — indistinguishable from a genuine misconfiguration (issue
#21). `RemoteServer.currentSettings` stats `settings.json` and re-reads only
when it moved, which is what makes creation as immediate as revocation already
was, per request and per connection.

**Reads have two names, and production code uses only those.**
`config.FreshSettings(store)` is for any decision: authorization, routing, a
mutation's precondition, a lifecycle action. `config.DisplaySettings(store)` is
the cached view for rendering (menus, lists, Settings-window payloads) and may
lag the last committed change by nothing in the tray, whose store never re-reads
the file (see below). Nothing else in production calls `Get`, `Reload`
or `ReloadIfChanged` on a store, and only `runTrayApp` constructs one;
`TestProductionSettingsReadsAndConstructionStayBehindTheBoundary` holds that
line. Its allowlist is the complete exception set: `runTrayApp` (boot snapshot
and audit recorder, taken before anything serves; the single store
construction).

**The tray owns `settings.json` while it runs.** The store never re-reads the
file on its own; a filesystem watcher submits a hand edit as one queued import
that is validated and applied only if valid (an invalid file keeps the current
state). Committed changes, including an imported edit, reach the UI and both
listeners through the queue's post-commit event (`App.onConfigCommitted`); the
2 s poll is a health poll and a 30 s tick re-runs listener reconciliation as
recovery, neither reading the file. Rationale and the legacy callbacks
that remain: [`config-ownership-and-races.md`](config-ownership-and-races.md).

**One tray per configuration directory.** `config.AcquireTrayOwnership` takes a
non-blocking `flock` on `tray.lock` in the directory before the sealed store
opens or the bridge socket is created, and the tray drops it last in `cleanup`.
A second tray exits with `ErrOwnedByAnotherTray` without touching the first. It
is an flock on an open descriptor so a crash frees it with the process; the
file is never unlinked, because a second tray could then lock a fresh inode
while the first still holds the old one.

See ADR-010 (the remote listener's mTLS transport and certificate-based client identity model).

## Service manifest (enhanced services)

Every spawned service gets `RELAY_BRIDGE_SOCKET` + `RELAY_SERVICE_ID` +
`RELAY_LAUNCH_FD=3`, and no credential. A service reads its single-use launch
secret from fd 3 and sends `Hello`, which binds the launch to its kernel audit
token; after that it authenticates by that token alone
([`docs/launch-identity.md`](launch-identity.md)). Services
that implement the protocol dial the bridge with a `RegisterManifest` payload
declaring (a) the routes they serve, (b) their internal Unix socket + bearer
token, and (c) optional status endpoint, actions, and config editor. Generic
services ignore the env vars; relay never dispatches to them.

The dispatcher does longest-prefix match on registered routes and proxies to the
service's internal socket using its declared token; WS upgrades share the same
handler. The protocol is intentionally minimal — no version, no capability
declarations, no service-ID hardcoding anywhere in relay. Full spec:
[`docs/service-manifest.md`](service-manifest.md).

Every service relay started this session (autostart or an explicit `Start`)
is supervised: an exit relay did not request is restarted through a fresh
`Start` (new secret, new Hello) with exponential backoff, and marked failed
with its last exit code after `ServiceRestartMaxAttempts` consecutive
failures — never a service the operator stopped. Restart supervision:
[`docs/service-manifest.md#restart-supervision`](service-manifest.md#restart-supervision).

## Session host

`relay-sessions` (`cmd/relaysessions`, built into `Contents/Helpers/` beside
the app) hosts every terminal, Claude Code, pi and chat session: a built-in
service record (`internal/service/builtin_sessions.go`, capabilities
`manifest` + `sessions`) relay launches and supervises like any other,
talking to relay over a peer-verified internal API (`POST /launch`, `POST
/terminate`) rather than the manifest dispatcher's proxied surface. Sessions
launch under `relay-sessions exec`, a shim whose own pid is the session's
root — true for a `pty` launch only today. A `claude`/`pi` launch and a
`chat` session's own provider process run with no shim today, a known gap
(see [`docs/session-host.md`](session-host.md#what-is-not-built-yet), gap
1). A `chat` session's optional relay-MCP tool child is the one exception:
`buildChatMCPManager` spawns it through the shim as its own `project_session`
root, once the session has opted into `useRelayTools` and the launch isn't a
host session. Where the shim is in play, its pid is what relay's launch identity
binds to, and what C3's process-ancestry membership walk
(`internal/membership`) treats as the root a caller must descend from to be
admitted tokenlessly. A `project_session`-kind launch
identity ([`docs/launch-identity.md`](launch-identity.md#project_session-the-session-host-identity-kind))
is the session-host root's own credential; it and C3 membership together are
what replaced the retired `allow_cwd_auth` directory-auth fallback.

```
internal/sessions/
  hostapi/     the internal API server: /launch, /terminate, /permission, and
               (mounted alongside them, guarded by the same peer+bearer check)
               the eve-facing HTTP/WS surface below
  shim/        `relay-sessions exec` (C6)
  hook/        `relay-sessions hook`, Claude Code's PreToolUse client
  terminal/    pty session lifecycle
  session/     claude/pi/chat session lifecycle, resume-on-user-action (SH-6)
  provider/    the Claude Code and pi CLI process adapters
  sandbox/     C7 SBPL profile rendering
  api/         eve-facing HTTP/WS handlers, mounted by hostapi.New/ListenInternal
  ledger/      the claude/pi/chat session record relay itself persists
```

Full design, the internal API's mutual peer verification (including the
single-trust-domain property that mount runs under — any frontend-capable
caller reaches every session on the host, not just their own), the shim's
exact behavior, the host data directory layout, and the honest list of what
is built but not yet wired together (unsandboxed claude/pi launches, the
unemitted `session_bound` audit event): [`docs/session-host.md`](session-host.md).

## Security

The credential model (full inventory: [`docs/tokens.md`](tokens.md);
the flow end to end, with worked examples:
[`docs/auth-flow.html`](auth-flow.html); brokering rationale: ADR-007):

- **Project token** (`RELAY_PROJECT_TOKEN`) — the security boundary, scoped to a project's allowed MCPs/tools. Sealed at rest (ADR-017; `docs/sealed-config.md`) alongside a clear SHA-256 hash inline in the project. **Relay is the sole broker:** Eve references projects by id only (the DTO strips the token from every response except rotate); relayLLM resolves the token just-in-time from the bridge by `projectId`, injects it into spawned children, and never stores it or accepts it from Eve.
- **Launch identity** (not a bearer; [`docs/launch-identity.md`](launch-identity.md)) — **no relay credential is in any service's environment**, because any same-user process can read another's startup environment. Relay passes a single-use 64-hex secret on fd 3; the service's bridge `Hello` binds that launch to its peer audit token (pid + pidversion, `LOCAL_PEERTOKEN`); later tokenless requests from that exact process authenticate by it, until the registry sees the launch end. The record carries a `kind`: `service`, whose authority is its own fixed `capabilities` set, and `project_session` ([`docs/session-host.md`](session-host.md)) — the session host's own root process, whose authority is its one named project's live grant instead of a capability list. What a `service` identity may do is decided by one function (`service.Allowed`): `frontend` is the frontend socket as `read`+`configure`+`proxy`+`execute` (never `grant`) with no `Authorization` header (and the only way to be told `RELAY_FRONTEND_SOCKET`), `manifest` is `RegisterManifest` under its own id, `models`/`model_host` are model-endpoint calls and `RegisterModelHost`, `sessions` (built-in `relaysessions` record only) is `SessionExited` and the unfiltered model list; the empty set reaches only `Hello`. The retired `projects` capability (`ResolvePtyEnv`/`ResolveProjectTemplate`/`ListProjects`/`GetProject`, tokenless cross-MCP `ListTools`/`CallTool`) no longer exists — a stored record naming it has the name silently dropped on load, never refused. An unknown capability name fails validation and relay will not start the record; a record written before the field existed is migrated once on load (`frontend_consumer` unset/true → `[frontend]`, false → `[manifest]`). Relay scrubs `RELAY_SERVICE_TOKEN`, `RELAY_MCP_TOKEN` and `RELAY_FRONTEND_TOKEN` from every service environment, and deletes any `legacy-frontend-token` credential on start. If a project token can't be resolved, a spawned child gets no token (fail closed).
- **Control-plane credential** (`settings.json` → `api_credentials`) — the API's authenticator (ADR-015). Names an explicit set of `read` / `configure` / `grant` / `execute` / `proxy`; absent means **nothing**, never everything. `frontendCredentialAuth` resolves any bearer to one of these before a handler runs (no credentials at all fails closed), and `RouteRegistrar` then checks the route's class — the first asks "is this anyone?", the second "may they do this?". `execute` and `proxy` routes are absent from the TCP mux entirely, not refused on it. Mint with `relay credential mint --name N --class read [--class …]`; the plaintext is printed **once**. A consumer that needs `grant` — including `POST /api/projects/{id}/rotate_token` — or `execute` over HTTP must mint its own.
- **Model key** (`rmk_…`, per session) — minted at launch for `pi`/`chat` and any `pty` template with `model_key: true`, presented to the model endpoint in `X-Relay-Key` (or as a bearer), scoped to the project's `allowed_models`. A template delivers it into the client's env only through `${MODEL_KEY}` in its own `env`. Dies on `SessionExited`, when its launch ends (a session whose launch says Hello), or on a relay restart. The same endpoint forwards a provider's own request (Anthropic, ChatGPT, OpenAI) with the client's own credential untouched and never a relay one: [`docs/model-endpoint.md`](model-endpoint.md#client-model-routing).
- **Enhanced internal bearer** — each service picks its own internal socket + token and declares both via the manifest; relay strips inbound `Authorization` and injects the service-declared token when proxying.

**The proxied surface has a class of its own, and it is socket-only** (ADR-016
decision 4). The `/` catch-all is the one mount whose blast radius relay
cannot see — what it reaches is whatever a manifest declares — so it is named
`proxy` rather than mislabelled as configuration. A `configure` credential no
longer reaches relayLLM's sessions, terminals or `/ws`, and the browser-facing
loopback bind reaches no proxied route at all: that is issue #50's guarantee
restored. Eve dials the **socket** and its `frontend` capability holds
`proxy`; a hand-minted
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
error: [`docs/service-manifest.md`](service-manifest.md).

**A credential may expire, and absent means never** (ADR-016 decision 3).
`--ttl` writes an RFC3339 `expires`; a record without one round-trips exactly
as it did before the field existed. An `expires` relay cannot parse reads as
**expired**, and an expired credential is refused *identically* to an unknown
one — a distinguishable answer would be an oracle for which credentials exist.
Expired records are reaped lazily, inside the same `store.With` as the next
mint, never by a timer. This is not a reversal of `internal/enrolment/ca.go`'s
revocation-over-expiry choice: an enrolment is long-lived and revoked, a login
credential is short-lived by design and renewed by another ceremony. See
[`docs/tokens.md`](tokens.md#expiry).

**External MCPs are supervised children** — one stdio connection per MCP id,
shared by every access profile that names it, and therefore restarted when it
dies rather than left down. `mcpSupervisor` (`internal/mcpbroker/external_mcp.go`) waits on the
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
VM did). Full reference: [`docs/audit-log.md`](audit-log.md); rationale:
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
Overview, Services, MCP Servers, Projects, Hosts, Remote Clients, Passkeys,
Service Inspector, Tool Calls.

The Overview tab (`web/src/app.js`'s `renderOverview()`; no dedicated Go IPC
file) is the landing page (`initialPage` defaults to it): one tile per other
tab's headline state, a "Needs attention" list aggregated client-side from
state every other tab already has (scope gaps, MCP health, unreachable hosts,
autostart-but-not-running services, audit disabled/dropped, sealed-store
degradation, pending enrolment requests), the last 6 audit rows, and a footer
with the version and two "Reveal" actions (`reveal_config_dir`,
`reveal_logs_dir`, both in `ipc_overview.go`). MCP health (`onMcpHealth`,
whole-map push) and service runtime (folded into the existing
`onServiceStatus` push) are the two pieces of data no other tab seeds on its
own; both ride along in `onSettingsReloaded` too.

The Services tab also sets the tray menu's shape. Services appear in the menu
in the order of the stored `services` list, and a record with `hide_from_menu`
set (omitted when false) is left out of the menu and nowhere else: it stays
registered, keeps running and still autostarts. The built-in session host row
follows the same rules. Both settings are cosmetic, so reordering
(`move_service`, `PUT /api/services/{id}/position`) and hiding
(`update_service_menu_hidden`, `PUT /api/services/{id}/menu`) sit in the
`configure` class with no presence prompt, no `presence.GatedOps` entry and no
issuance record, and neither starts, stops or restarts a process.

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
