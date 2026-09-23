# Token reference

The canonical inventory of every credential in the relay ecosystem: what it is,
what it can do, and where it lives. See ADR-007 (`docs/decisions/007-project-token-brokering.md`)
for the project-token brokering model.

| Token | Where it's named | Purpose | Privilege / scope | Lifecycle & storage |
|---|---|---|---|---|
| **Project token** | env `RELAY_PROJECT_TOKEN` | The security boundary for MCP tool access — identifies the project for a tool call; relay injects the authenticated `project_id` into `_meta`. Injected into project shells / LLM CLIs / the `relay mcp` child. On the bridge it may call `ListTools`, `CallTool` and `DescribeProject` — the last returns its own project's record and resolved grant (path, allowed models, per-MCP access, launch root and tools), never a token, hash or another project. | **Scoped.** Permissions derived at auth time from the project's `allowed_mcp_ids` + `disabled_tools`. | Long-lived. Sealed on disk (AES-256-GCM, keyed from the login keychain) alongside a clear SHA-256 (`TokenHash`) — see [`docs/sealed-config.md`](sealed-config.md). Rotatable via the `rotate_token` HTTP route / `rotate_project_token` IPC, both of which now go through the running service and a presence prompt (ADR-017 decision 3): rotation issues the security boundary itself. |
| **Launch identity** *(not a bearer)* | a single-use 64-hex launch secret on inherited **fd 3** (`RELAY_LAUNCH_FD=3`), presented once in a bridge `Hello` | Recognises a process relay launched. `Hello` binds the launch to the kernel audit token (pid + pidversion) of the process that presented the secret; every later request from that exact process authenticates by its peer audit token, with no token on the wire. Protocol: [`docs/launch-identity.md`](launch-identity.md). | For a `service`-kind identity, by the service record's `capabilities`, a set: `frontend` is the frontend socket as `read`+`configure`+`proxy`+`execute`; `manifest` is `RegisterManifest` under its own id; `models`/`model_host` are model-endpoint calls and `RegisterModelHost`; `sessions` (built-in `relaysessions` record only) is `SessionExited` and the unfiltered model list. The empty set reaches only `Hello`. A `project_session`-kind identity instead reaches a fixed operation set scoped to its one project's own live grant, never a capability set. | The secret lives only in the pipe and, as a SHA-256, in relay's memory; it is spent by the first successful `Hello`. The identity lives until the registry sees that launch end. Nothing is persisted, and nothing is in any environment or argv. |
| **Control-plane credential** | `settings.json` field `api_credentials`; minted by `relay credential mint` | Authenticates a caller to relay's control-plane HTTP API (the frontend socket and, if bound, `RELAY_API_LISTEN`). The API's bearer authenticator (ADR-015); a service relay launched may instead reach the frontend socket by its launch identity. | **Classed.** Carries an explicit set of `read` / `configure` / `grant` / `execute` / `proxy`; an absent set grants nothing. `execute` and `proxy` are socket-only. | Long-lived by default; `relay credential mint --ttl 12h` gives one an expiry. SHA-256 only in `settings.json` (0600) — the plaintext is printed once by `relay credential mint` and is not recoverable. Revoke with `relay credential revoke --id ID`. |
| **Enhanced-service internal bearer** | declared via `RegisterManifest` (per service) | Secures the internal socket between relay's dispatcher and an enhanced service (relayLLM, relayScheduler). Relay strips inbound `Authorization` and injects this token when proxying front-door traffic onward. | That service's internal endpoint only. Distinct from frontend creds. | Each service picks its own socket + token; told to relay at manifest registration. |
| **Admin secret** | `settings.json` field `admin_secret` | Gates admin-only bridge ops: `ReconcileExternalMcps`, `ReloadExternalMcp`, `ReloadService`. | Administrative control-plane. | Auto-generated on first run; constant-time compared via `ValidateAdmin` at the bridge layer. Sealed on disk; only the tray, which holds the keychain key, ever reads it back (see [`docs/sealed-config.md`](sealed-config.md)). |
| **OAuth 2.1 tokens** | per HTTP MCP (`internal/mcpbroker/oauth.go`) | Authenticate relay to **upstream** HTTP MCP servers (PKCE, dynamic registration, auto-refresh). | The upstream provider, not relay's own boundary. | Access token, refresh token and client secret stored per-MCP (`OAuthState` in `settings.json`), sealed on disk; `client_id` and `token_expiry` stay clear. |
| **eve session token** | `eve_session` (browser localStorage) | Authenticates a human/browser user to **eve itself** — *not* a relay credential; listed to disambiguate. | eve's own app auth. | Independent of relay. |
| **Model key** *(not a bearer relay issues generally — a per-session credential)* | `rmk_` + 64 lowercase hex, presented in `X-Relay-Key` (or as a bearer, on `model.sock`) | Lets a spawned session call the model endpoint without holding the project's own token: `AuthorizeLaunch` (`cmd/relay/session_launch.go`) mints one for every `pi`/`chat` session launch and any `pty` template opting in (`ModelKey: true`), scoped to the launching project. A `pty` template delivers it into the client's env only where its own `env` names `${MODEL_KEY}` (typically as `X-Relay-Key: ${MODEL_KEY}` in `ANTHROPIC_CUSTOM_HEADERS`); a template with no mapping gets none. | **Scoped to one project**, same `allowed_models` a project token gets — see [`docs/model-endpoint.md`](model-endpoint.md#model-keys). | Held only as a SHA-256 in relay's memory (`ModelKeyTable`, `cmd/relay/model_keys.go`); the plaintext is returned once, at mint, in the `LaunchSpec` relay hands relay-sessions. Revoked when the session ends (`sessionAccount.end`) or its project is deleted, and — for a session whose launch says Hello — dead the moment that launch ends, whether or not anything reports it ([`docs/launch-identity.md`](launch-identity.md#a-model-key-is-bound-to-its-launch)). Nothing persists it to disk; a relay restart invalidates every live key. It sits in the client's environment, readable by same-uid processes, the exposure `RELAY_PROJECT_TOKEN` already has. |

Notes:

- `TokenHash` is not a separate credential — it's the SHA-256 at-rest/comparison
  form of the project token.
- The **project token** and a **launch identity holding `projects`** are
  deliberately distinct: a project token is scoped to one project's tools; the
  `projects` capability is every project and cannot be handed on, since it is a property of one
  process rather than a value. If a project token can't be resolved, a spawned
  child gets no token at all (fail closed).
- **No relay credential is in any service's environment.** Relay removes
  `RELAY_SERVICE_TOKEN`, `RELAY_MCP_TOKEN` and `RELAY_FRONTEND_TOKEN` from every
  environment it passes to a service. `RELAY_PROJECT_TOKEN` is still injected
  into project shells and agent CLIs; the legacy `RELAY_TOKEN` alias is
  retired here (`relay mcp`/`relay mcp call` read only `RELAY_PROJECT_TOKEN`
  now) — relayLLM's own spawned sessions still dual-write `RELAY_TOKEN`
  pending its own retirement at the P3 cutover.
- **Every plaintext this table lists as sealed lives only in relay's own
  memory and in an AES-256-GCM envelope on disk, keyed from the login
  keychain.** A verifier — `token_hash`, a credential's `hash`, a passkey's
  public key coordinates, a certificate fingerprint — is never sealed: relay
  authenticates against those directly and never needs the plaintext to do
  it. The reasoning, the field list, and what a sealed file still leaks on
  purpose (every name, path, command and scope value) are in
  [`docs/sealed-config.md`](sealed-config.md).

## Control-plane credentials (ADR-015)

The HTTP API that Eve, relayScheduler and the settings view consume is
authenticated by a **control-plane credential** or, on the frontend socket, by
a **launch identity holding the `frontend` capability** — never by a single shared bearer.
Two checks run, in this order, and they do different jobs:

1. `frontendCredentialAuth` resolves the caller before any handler runs — so an
   unauthenticated WS upgrade never allocates a session. A request with no
   `Authorization` header whose socket peer holds the `frontend` capability is
   that identity ([`docs/launch-identity.md`](launch-identity.md)).
   Any other request must carry an `Authorization: Bearer` that resolves,
   constant-time, to a credential in `Settings.APICredentials`. Absent,
   malformed and unknown bearers, and a headerless caller with no identity, get
   the same 401 with the same body.
2. `RouteRegistrar` checks the resolved credential or identity against the
   **class** the route was registered under, and returns 403 if it does not
   hold it.

The first answers "is this anyone?"; the second answers "may they do this?".
They were briefly conflated, with the outer check admitting exactly one token,
which made every credential except that one unusable — the class model existed
and could not be reached.

### The five classes and what each reaches

| class | what it means | routes |
|---|---|---|
| `read` | discloses configuration or history | `GET /api/projects`, `/api/projects/{id}`, `/api/services`, `/api/services/{id}`, `/api/enrolments`, `/api/enrolments/{id}`, `/api/remote`, `/api/mcps`, `/api/mcps/{id}/tools`, `/api/mcps/{id}/scope_fields`, `/api/audit`, `/api/audit/log`; `POST /api/mcps/{id}/enumerate` (a POST that discloses and changes nothing) |
| `configure` | changes relay's own state | `POST`/`PUT`/`DELETE /api/projects…`, `POST /api/projects/{id}/regen_skill`, `DELETE /api/services/{id}`, `POST /api/services/{id}/start`\|`stop`, `PUT /api/services/{id}/autostart`\|`position`\|`menu`, `DELETE /api/mcps/{id}`, `POST /api/audit/export` |
| `grant` | issues or revokes a credential another party holds | `POST /api/enrolments`, `DELETE /api/enrolments/{id}`, **`POST /api/projects/{id}/rotate_token`** |
| `execute` | the *caller* supplies what runs or what is exposed | `POST /api/mcps`, `POST /api/services`, `PUT /api/services/{id}`, `PUT /api/remote` |
| `proxy` | reaches a surface relay has **not** classified | the `/` catch-all — every route an enhanced service registers via its manifest, `/ws` included |

An absent or empty class set grants **nothing** — never "everything", never
"read". A credential minted by a tool that predates the class model is inert.

**Every `execute` route that can make relay run a new command is
presence-gated**, at the operation core each one shares with its CLI/IPC
doors, not only here: `mcp.register`, `service.register` (covers both
`POST /api/services` and the command-setting fields of
`PUT /api/services/{id}`) and `remote.configure` in
[`docs/presence-gate.md`](presence-gate.md). The class check above answers
"does this caller's credential reach `execute` at all"; the gate separately
answers "did a human standing at this machine approve this exact call" —
holding the class is necessary but never sufficient for those paths.

This is **not** true of every mutation `execute` reaches, though. Two
narrowing/rename paths have no prompt at all: `PUT /api/services/{id}`
renaming a service's `DisplayName` or dropping its own capabilities/
`allowed_models` (`serviceUpdateNeedsGate` never inspects the name and treats
narrowing as safe by default), and `PUT /api/remote` with `{"remove": true}`
or turning a listener off (only turning one *on* is gated). Since
plan-broker-and-sessions.md's F1 decision, `execute` is held by default by
eve's frontend launch identity (not only an operator-minted credential, which
is what these two conditional gates were designed against) — a
frontend-capable service can now rename a sibling service or wipe the remote
config with no human prompt. That's integrity/availability exposure, not
privilege escalation (every command-setting path stays gated), and is an open
question for the user rather than something silently tightened or accepted —
see `STATUS-relay-security.md`.

**`execute` is socket-only.** Those four routes are not registered on the
loopback TCP mux at all, so a caller there gets the mux's own refusal however
its credential is classed (ADR-015 decision 2). The class still exists on the
credential so the socket path can tell a consumer that needs it from one that
does not.

**The proxied surface is `proxy`, and `proxy` is socket-only too** (ADR-016
decision 4). The `/` catch-all is the one mount whose blast radius relay
cannot see: what it reaches is whatever a manifest declares, which is why it
is named as unclassified rather than called configuration. Three consequences
follow.

- A route that starts a terminal is not reachable from the loopback TCP bind,
  which is the browser-facing door. That is issue #50's guarantee restored.
- `configure` stops silently meaning "and also every route relayLLM
  registers". A hand-minted `configure` credential that reached relayLLM
  through the catch-all is strictly less able than it was and must be
  re-minted naming `proxy`.
- Eve reaches the proxied surface: it dials the frontend **socket**, and a
  `frontend` capability holds `proxy` alongside `read`+`configure`.

`proxy` is a class distinct from `execute` so that `configure`-only credentials
and services never gain the proxied surface by accident; the `frontend`
capability holds `proxy` alongside `read`+`configure`+`execute` today
(plan-broker-and-sessions.md's F1 decision), so this is no longer a boundary
kept between `frontend` and `execute` the way it once was. Classing per route
remains the finer-grained answer and needs the manifest to describe blast
radius, which is a protocol change across repositories and is still deferred.

**A near-miss on TCP is now a 405.** With no catch-all on the loopback mux to
absorb it, `POST /api/services` there is `http.ServeMux` refusing a method it
has no pattern for, rather than a proxied request. No handler runs either way;
the status is the only thing that moved.

### Minting

    relay credential mint --name eve-view --class read --class configure
    relay credential list
    relay credential revoke --id 5d6dad87-4a31-40f6-88f8-9193adcba554

`--class` is repeatable, like `relay enrol create --grant`. An unrecognized
class is a hard error naming all five, and an empty class set is refused at the
CLI: `Grants` treating an absent set as nothing is the correct *runtime*
default, but minting one is an operator mistake worth catching at the point of
entry rather than discovering as a credential that silently reaches nothing.

The plaintext is printed **once** and is not recoverable — only its SHA-256 is
stored. Lose it and the remedy is to revoke and mint again. `relay credential
list` never prints the hash and never prints the plaintext.

`relay credential mint` runs in a separate process from the tray, but no
longer writes `settings.json` itself: it asks the running tray to mint over
the bridge socket (`admin_op`, ADR-017 decision 2), which demands a
LocalAuthentication presence check — one login-password prompt per mint —
before the tray commits anything (see
[`docs/presence-gate.md`](presence-gate.md)). With relay stopped, the command
refuses by its own name rather than falling back to a direct write. Once it
does commit, `Authorize` reads through `freshSettings`, so a credential minted
this way authenticates on the API's very next request, with no restart and no
poll interval — the same guarantee `relay enrol create` gets (issue #21).

That guarantee used to have a write half that was easy to lose, back when the
CLI held its own write path into `settings.json`. See
[settings.json has one writer and several goroutines](#settingsjson-has-one-writer-and-several-goroutines)
below: the tray is now the only process that ever writes the file, so the
remaining way to lose a freshly minted credential's plaintext is a hand-edit
landing in the same instant — a credential's plaintext is printed once and is
unrecoverable regardless, so the remedy either way is to revoke and mint
again.

### Expiry

A credential minted with no `--ttl` never expires, and **an absent `expires`
field means exactly that** — every record written before the field existed
round-trips unchanged, the same zero-value discipline `Project.Kind` follows.

    relay credential mint --name browser-login --class read --ttl 12h
    relay credential list                    # hides expired records
    relay credential list --include-expired  # shows what the next mint will reap

Four rules, and each exists for a reason worth stating.

- **An `expires` relay cannot parse reads as expired.** Absent is a value
  relay writes on purpose; unparseable is a lifetime relay cannot evaluate,
  and the only safe answer to that is that the lifetime is over. A corrupt or
  hand-edited timestamp must not be the way to mint an immortal credential.
- **An expired credential is refused *identically* to an unknown one.** Same
  401, same body, same nil from `AuthenticateAPICredential`. A distinguishable
  answer is an oracle for which credentials exist.
- **Reaping is lazy — on the next mint, inside the same `store.With`, never on
  a timer.** A background goroutine rewriting `settings.json` on a schedule is
  a writer nothing asked for, against a file that already has more writers
  than it wants (see below). Reaping is housekeeping, not enforcement: a
  credential stops authenticating the moment it expires, swept or not.
- **`list` hides expired records by default.** One credential per interactive
  login (ADR-016 decision 3) means `api_credentials` stops being a short
  human-curated list; `--include-expired` is how an operator sees what is
  about to go.

**This is not a reversal of the enrolment trade.** `internal/enrolment/ca.go` chose
revocation over expiry for client certificates deliberately, because a short
certificate lifetime needs an authenticated renewal path, and any credential
replayable to obtain a fresh certificate reintroduces a bearer secret at the
one point where the result is a *new identity*. That argument does not
transfer here and the two conclusions do not conflict: an enrolment is
long-lived by design and revoked, while a login credential is short-lived by
design and its renewal path is *another ceremony* — an unforgeable
user-presence act rather than a replayable secret. Expiry is affordable
exactly where revocation was the only option.

`legacy-frontend-token` is reserved: relay deletes every credential under that
name on start, so an operator-minted credential under it would be deleted too.
Both `mint` and `revoke` refuse it.

### The frontend capability

A service holding the `frontend` capability authenticates to the frontend
socket by its launch identity, sending no `Authorization` header, and holds
exactly `read`+`configure`+`proxy`+`execute` (never `grant`) — the `execute`
grant is per plan-broker-and-sessions.md's F1 decision, so eve can reach the
session-host launch routes; see "This is **not** true of every mutation
`execute` reaches" above for what that does and doesn't expose. There is no
frontend bearer and no per-start frontend token. A record named
`legacy-frontend-token` — the hash of a bearer relay once placed in
consumers' environments, where any same-user process could read it — is
deleted on start, and settings.json is not written when there is none.

`ServiceConfig.Capabilities` decides it, alongside `manifest`, `models`,
`model_host`, and `sessions` (built-in `relaysessions` record only)
([`docs/launch-identity.md`](launch-identity.md#identity-kinds-and-capabilities)).
Only a service holding `frontend` is told `RELAY_FRONTEND_SOCKET`.
`relay service register --capability frontend` grants it; `service list`'s
`CAPABILITIES` column and the Settings window's service card show the set.

That class set is a **narrowing** all the same: it excludes `grant` always,
and a consumer that needs `grant` over HTTP must be handed its own credential
naming that class. In particular, **project token rotation
(`POST /api/projects/{id}/rotate_token`) is `grant`-class** — it issues a
credential another party holds — so an existing consumer that rotates project
tokens needs a credential of its own:

    relay credential mint --name my-rotator --class grant

The same applies to `POST /api/enrolments` and `DELETE /api/enrolments/{id}`.

### The login credential

An interactive login at `http://localhost:PORT/relay/login` mints an ordinary
control-plane credential — not a sixth kind of token. There stays exactly one
thing the API authenticates and one place an operator revokes (ADR-016
decision 3).

| | |
|---|---|
| **Classes** | `read` + `configure`, and nothing else. Never `grant`: a view that can rotate a project token is a view whose compromise issues credentials. Never `execute`, which is unroutable on TCP anyway — stating it on the credential means the refusal survives someone later serving the view over the socket. Never `proxy`, which is what makes `configure` mean what it says: the browser view cannot reach a session, a terminal or `/ws`. |
| **Lifetime** | Twelve hours, written as `expires`. A first value, expected to be wrong; what matters is that expiry exists and that `relay audit --event control_decision` can show whether it is being hit. |
| **Storage** | SHA-256 in `settings.json`, like every other credential. Expired records are reaped inside the same `store.With` as the next login. |
| **Naming** | One record per login, named for the ceremony that produced it (`login <abbreviated credential id> <timestamp>`), so `ControlDecision.CredID` attributes a browser session rather than a role. That prefix is `loginCredentialPrefix`, and it is what the sign-out gate reads. |
| **Signing out** | Revoking that one record. Two doors, one operation: `relay credential revoke --id ID`, or **Settings → Passkeys → Signed-in Browsers → Sign out**. The Settings door refuses anything that is not a login credential, inside the same `store.With` as the delete — a WebView must not be able to revoke a credential an operator minted for a script, and `legacy-frontend-token` is refused there as it is everywhere. |

**It is returned once, in the response body of
`POST /relay/login/verify`, and the page holds it in memory and nowhere
else.** Not `localStorage`, which every script the page loads can read and
which survives a restart. Not a cookie: a cookie is ambient authority, sent
by the browser on any request any page causes, which would hand a CSRF
surface to the one credential a page can reach — and relay refuses ambient
authority consistently everywhere else. The token lives in a closure and is
sent as `Authorization: Bearer`, so **a reload runs the ceremony again**. On
a machine with no platform authenticator that makes every reload a physical
act, and that cost is the one this model chooses over persistence.

**The class set is a ceiling, not a measurement.** It is narrowed by running
the view against it and taking the distinct (method, path, class) tuples it
actually reached from the audit log — never widened by one.

**The routes that mint it are the only unauthenticated surface relay serves**,
and they exist only when `RELAY_API_LISTEN` is bound. A WebAuthn ceremony is
verified against the origin of the listener relay actually bound
(`http://localhost:PORT`, derived at bind time, never from a header or a
setting), so with no TCP listener there is no origin, and the routes are
registered nowhere rather than registered against a placeholder. They live in
a separate mux consulted before `frontendCredentialAuth` holding exactly
three patterns — `GET /relay/login`, `POST /relay/login/challenge`,
`POST /relay/login/verify` — rather than as an exemption inside the
authenticated mux, which would be the second-gate defect ADR-015 already had
to fix once. `/relay/` is reserved: a service manifest claiming it is refused
at registration.

### What the unauthenticated surface records, and what it still costs

`relay audit` is ground truth for anything relay gates, so every outcome of a
ceremony is a `control_decision` record on `POST /relay/login/verify`: a login
that minted a credential (allowed, `cred_id` naming the record it minted, which
is what makes a browser session attributable), a registration that landed
(allowed, `cred_id` naming the abbreviated passkey id), and each refusal —
a bad bootstrap code, an unknown credential id, a rejected assertion, the
cloned-authenticator counter signal. `class` is empty on all of them, because
these routes are not registered through `RouteRegistrar` and there is no class
that means "none".

Three things are deliberately kept out of those records.

- **Anything secret.** No token, no stored hash, no bootstrap code. The reason
  a refusal records the *sentinel* rather than the error's full text is the
  code: a decode failure quotes the body it choked on, and the body of a
  registration is where a caller's guess at the anchor lives. The counter
  refusal is the one exception, and its detail is relay's own — a stored
  credential id and two stored counters — which ADR-016 decision 7 point 10
  requires by name. It is capped like `path` and `method` are.
- **Refusals from before a ceremony was attempted** — a malformed body, an
  unknown ceremony name. They are reachable at line rate by anything that can
  open a socket and say nothing about a login.
- **A refusal the ceremony limiter itself produced.** It is the one refusal an
  unauthenticated caller can provoke as fast as it can send, and recording it
  would be the audit-log amplification the control-plane caps exist to prevent.
  The failures that caused the throttle are each recorded.

**What the ceremony limiter counts.** Failed verifications are counted and
delayed (ADR-016 decision 7 point 12), and the count is cleared only by a
ceremony that completed *including the parts the verifier cannot see*. The
verifier is pure and knows nothing about the bootstrap code, so a registration
it accepts and the route then refuses is charged as a failure by the route, and
`VerifyRegistration` no longer clears anything on its own. Without that split,
a registration carrying no code — which costs an unauthenticated caller only a
key of its own — was a failure to the route and a *success* to the limiter, so
the attacker chose whether the throttle applied to their own failed assertions.
A caller that forgets to confirm leaves the count standing: the signal fails
closed.

**A challenge flood is a denial of service relay bounds but cannot prevent.**
`POST /relay/login/challenge` allocates an entry in a fixed table (64, 60 s,
single use) and is unauthenticated by construction; on a loopback bind every
request arrives from the same address, so there is no identity to reserve a
slot against. A flood that keeps issuing as entries expire therefore keeps the
owner refused for as long as it runs — not for the one TTL a refusal costs
otherwise. Evicting instead of refusing does not fix it: it converts a visible
refusal into a ceremony that fails later, and the owner's entry is displaced
within milliseconds anyway. What is bounded is what it costs relay — a fixed
table, no disk, no goroutine, and the table cannot be grown past its bound —
and the condition is no longer silent: a full table warns once per TTL, which
is the difference between an operator seeing "login is broken" and seeing that
something is hammering the login route. The CLI is the recovery path, as it is
for every other login failure.

## The login bootstrap code is not a credential

`relay login enrol` prints a code, but it is **not** a sixth entry in this
document's inventory and must never be listed alongside the five
control-plane classes above. It authorises exactly one thing: registering a
passkey. It is never accepted in place of an assertion, so it cannot become
a password, and it never authenticates a request to relay's API on its own
(ADR-016 decision 2).

    relay login enrol
    relay login list
    relay login revoke --id ID

- **What it authorises.** Nothing beyond `POST /relay/login/verify` in
  registration mode. It does not read, configure, grant, execute or proxy
  anything, and it is refused the instant it has done its one job.
- **Lifetime.** Two minutes, single use. `relay login enrol` replaces any
  existing code rather than accumulating one — there is at most one anchor
  live at a time. Only its SHA-256 is stored, in `Settings.LoginBootstrap`;
  the plaintext is printed once and is not recoverable.
- **Refusal is uniform.** An absent record, an expired one, and a wrong
  guess are refused identically, for the same oracle reason `expires`
  handling is on `APICredential`: a distinguishable answer would tell an
  unprivileged caller whether registration is currently anchored at all.
- **A passkey is revoked with `relay login revoke --id ID`, never by
  deleting `settings.json`.** Deleting the file loses every project and
  credential along with it, and — because the file's absence is what would
  otherwise re-arm self-registration under trust-on-first-use — this is
  exactly the reason ADR-016 decision 2 refuses TOFU as the anchor in the
  first place. `relay login list` shows every registered passkey's name,
  abbreviated credential id, creation time and last-used signature counter,
  never its public key; **Settings → Passkeys** shows the same fields and the
  same omission.
- **Revoking a passkey does not sign anybody out.** The passkey and the
  credentials it has minted are separate records with separate lifetimes: the
  revoke stops the *next* login, and a browser that signed in beforehand keeps
  working until its credential expires — up to twelve hours. Ending that is a
  credential revoke, above. Both operator surfaces say so at the point of the
  act rather than leaving it to be discovered.

**A second browser's passkey is a different anchor, not this one.** Eve's
`auth.json` holds its own passkeys, entirely outside this inventory; relay's
part is only the five-minute `eve_enrolment` window that lets a second
browser register one. See
[`docs/eve-passkey-enrolment.md`](eve-passkey-enrolment.md) for the design
and `relay eve enrol` in [`docs/cli.md`](cli.md#relay-eve) for the door.

**A second presentation of the code, not a second anchor.** The tray's
**Show Login Code...** item and `relay login enrol` both reach the same gated
core method, `LoginOps.MintBootstrap`, and show the result in the Settings
window — relay is `LSUIElement`, so that window is the only surface the tray
has. ADR-016 decision 2 accepts the menu item as a presentation and refuses it
as *the* source: a capability reachable only from a mouse is a capability
half-built, which is why `relay login enrol` stays as the second door. The
"replaces rather than accumulates" rule is unchanged and is stated on screen —
opening the item twice leaves exactly one code working, the second.

**Both doors now demand presence, and both refuse the same way over SSH.**
Minting a bootstrap code is itself a gated operation (ADR-017 decision 3):
`LoginOps.MintBootstrap` requires a login-password presence check before it
mints, whichever door reached it. A session that cannot display that prompt —
an SSH connection, in particular — refuses immediately rather than queuing,
for `relay login enrol` exactly as it does for the tray's own menu item. That
sharpens the reason the CLI form was kept in ADR-016 decision 2 ("the tray
menu is unreachable over SSH") into a cost the ADR did not anticipate: the
CLI form no longer reaches where the menu does not, either. A fully headless
install therefore has no working bootstrap path at all — no way to register a
passkey and no way to mint a credential to work around it — and there is no
mitigation for this, by decision (ADR-017 implementation spec §9.8, §3.2).
`relay login revoke` is gated too (it destroys a login identity) and refuses
the same way over SSH; only the read half, `relay login list`, is unaffected.

## The enrolment request channel is not a credential

`relayremote request` prints a request id, but it is **not** a sixth entry
in this document's inventory and must never be listed alongside the five
control-plane classes above or the certificate/enrolment pair
[`docs/access-profiles.md`](access-profiles.md) describes. It authorises
nothing at all — a request id names a row in an in-memory table, never an
identity, and nobody redeems it for anything by presenting it.

- **What it authorises.** Nothing. Lodging a request is an unauthenticated
  act by design (`docs/decisions/018-configuration-is-a-capability-of-an-identity.md`
  decision 8) — a network peer can add a row to a bounded table and reach
  precisely that far. The id that comes back names the row so the same
  caller can poll it; it does not open any door a stranger could not already
  reach by lodging a fresh request.
- **It cannot be presented in place of a certificate.** The remote listener
  and the tool-plane listener take a certificate at the TLS handshake, never
  a request id on the wire, and the enrolment-request listener itself holds
  no method that accepts one as authority for anything — it can only be
  polled. A request id proves nothing about the machine that holds it, which
  is the opposite property a credential needs.
- **Its loss costs nothing.** There is no plaintext to protect and nothing
  to revoke: the id is not a secret before it is lodged, during the wait, or
  after collection. Losing it means re-running `relayremote request` and
  lodging a fresh one — the previous row simply expires at its own TTL.
- **Nor is the six-character comparison code** `relayremote register` prints
  (ADR-019 decision 3). It is not presented to anything and authorises
  nothing: both ends *derive* it independently from material they already
  hold, and comparing the two is an act performed by a human between two
  screens. Relay deliberately never echoes it to the client — if it did, a
  man-in-the-middle could forward relay's value and the comparison would be a
  comparison of one number with itself. It is short-lived, worthless once the
  request is approved or expires, and knowing it lets an attacker do nothing
  it could not already do: the guess it would have to win is against a value
  neither side could choose after committing.

**What the channel records, and what it deliberately does not.** `relay
audit` is ground truth for anything relay gates, and lodging is the one act
here an unauthenticated caller drives at line rate — recording it would be
exactly the audit-log amplification [above](#what-the-unauthenticated-surface-records-and-what-it-still-costs)
already exists to avoid, so **lodging is never audited.** It is *counted*
instead: a full table warns at most once per TTL, the same discipline the
login challenge table uses for its own flood. That count is also what the
tray's `Pending enrolment requests: N` line and its (coalesced, rate-limited)
notification are derived from — a read on a timer, never a call from the
lodge path, which is what keeps ADR-018 §8 P1's structural proof intact while
the banner exists at all. **An operator's explicit
refusal from the pending list IS audited**, as a genuine `ControlDecision`
with `Allowed: false` — a human declining a stranger's request is an
authorization decision no attacker can drive, unlike lodging it in the first
place. **Approval is already audited**, by the same `credential_issued`
record every `enrolment.sign` produces — approving a network request and
signing a CSR handed to relay directly write the identical record, because
they are the identical act underneath (see [`docs/presence-gate.md`](presence-gate.md)).
**Expiry is logged, not audited** — a `slog.Info` line with a count, because
a request nobody answered in time is not a decision anybody made.

## settings.json has one writer, and several goroutines

Every credential in this document except the ephemeral ones lives in one
file, `settings.json`, and the tray is now its only writer. Minting a
credential, creating or updating an enrolment, registering an MCP or a
service, and rotating a project token all used to run as a separate CLI
process that opened `settings.json` and wrote it directly; they now ask the
running tray to do it over the bridge socket instead (`admin_op`, ADR-017
decision 2 — see [`docs/sealed-config.md`](sealed-config.md) for why: the
tray is the only process holding the keychain key that unseals the file's
sealed fields, so it has to be the only process writing them). A CLI process
that still exits seconds later now reports what the tray decided rather than
deciding anything itself.

That retires the *inter-process* race this section used to describe, not
concurrency itself. Within the one process that now holds the file, HTTP
handlers, IPC handlers, bridge handlers, the OAuth refresh callback and the
status poller all mutate settings from their own goroutines, and single-process
execution does not serialize them for free. Everything below is the
discipline that window still needs — narrowed from "another process" to
"another goroutine," with a hand-edit while the tray is running as the one
remaining way a second *process* can still touch the file at all.

**Reads go through `freshSettings`, never `store.Get()`.** `Get()` answers
from an in-memory cache that only a mutation made by this process and the
tray's 2 s poll refresh, which is fine for a menu and wrong for an
authorization decision. `freshSettings` resolves through `ReloadIfChanged`,
which stats the file and re-reads only when it moved — so a hand-edit, or a
write this same process just committed on another goroutine, is authoritative
on the very next request. Cost is one stat per decision.

**Writes reload before they apply.** `FileSettingsStore.With` re-reads the
file under the same lock before running its callback, using the same
stat-then-read-only-if-moved machinery, so a mutation is always derived from
what is on disk rather than from a cache that could be a whole poll interval
old. Without that, a request whose cached view predated another goroutine's
very recent write — or a hand-edit — would serialize its own stale view back
over the top and silently destroy the record, worst for a credential whose
plaintext was printed once and cannot be reissued. A reload that could not
read the file does not fall back to defaults and save those: the write is
refused, per
[An unreadable file is unknown, not empty](#an-unreadable-file-is-unknown-not-empty).

Two consequences worth stating rather than discovering:

- **The callback sees fresher state than its caller did.** The reload happens
  inside `With`, so anything a caller read beforehand may already be stale by
  the time the callback runs. This is why every ops core (`service_ops.go`,
  `enrolment_ops.go`, `mcp_ops.go`) resolves the record it is about to change
  *inside* the callback and reports "not found" from a flag set there —
  resolving outside and mutating inside is a TOCTOU window that used to run
  between two processes and now runs between two goroutines in one, which is
  a smaller window and not a closed one: an HTTP handler, an IPC handler and
  the status poller can still all reach the same `With` concurrently.
- **A hand-edit under a running tray is imported, not merged.** The tray's
  store never re-reads the file on its own; a watcher submits the edit as one
  queued, validated import, so it is ordered with every other mutation (a save
  imports a pending edit first, then applies on top). See the external-writer policy in
  [`config-ownership-and-races.md`](config-ownership-and-races.md).
- **Historically it was last-writer-wins, narrowed to a hand-edit.** The reload closes
  the window between a caller's cached view and the file; it does not make
  read-modify-write atomic against a write that lands in the gap between the
  reload and the save. The mutex around `With` already serializes every
  in-process caller against every other, so the only writer left that can
  land in that gap is a human editing `settings.json` by hand while the tray
  is running. A cross-process advisory lock — the fix this section used to
  point to for the CLI-versus-tray case — is no longer needed for anything
  relay does on its own; it would only ever help against a hand-edit racing a
  save, a window narrow enough that nobody has asked for one. The durability
  half is unrelated and unchanged: `atomicWriteFile` fsyncs the temp file,
  renames, and fsyncs the directory, so a crash cannot leave a half-written or
  zero-length settings.json behind.

Two rules follow from that, and both are about writes that should never have
happened at all.

**A callback may decline the write, and a refusal must.**
`FileSettingsStore.With` saves whatever its callback leaves behind, *including
nothing*, so a callback that decides its change must not happen still rewrites
settings.json. `WithDeclinable` is the same method with a callback that returns
an error: a non-nil return declines the write outright — nothing is saved, the
file is left byte for byte as it was, and the error comes back unchanged so
`errors.Is` still reaches the callback's own sentinel. `With` is now that
method with a callback that can only succeed, so both go through one lock and
one save path, and the remaining `With` call sites are unaffected.

`withDeclinable(store, fn)` reaches that behaviour through the
`DeclinableSettingsStore` interface, and falls back for a store that cannot
decline — the fallback still writes, and rolls the refused mutation back so a
refusal cannot persist half of what it refused. Only a store implementing the
interface can promise the file was not touched at all.

This is not tidiness. `POST /relay/login/verify` is unauthenticated by design
(ADR-016 decision 5), and a registration refused for want of a bootstrap code
used to run the save anyway — which handed anything able to reach
`RELAY_API_LISTEN`, with no code, no credential and no passkey, a trigger on
relay's settings writer at whatever rate it cared to send. Every one of those
writes is also a chance to lose a concurrent writer's change, and the change
most expensive to lose is a freshly minted credential whose plaintext was
printed once. The interface stays as it was: declining is a second, narrow
interface (`DeclinableSettingsStore`), because widening `SettingsStore` would
oblige every implementation to grow a method most of them have no file to
honour it with.

**Every ops core that resolves a record inside the write now declines when it
finds nothing.** `ServiceOps.Update`/`Remove`/`SetAutostart`/`SetMenuHidden`/`Move`, `McpOps.Remove`,
`McpOps.StartOAuth`, `revokePasskey`, `createEnrolment`, `updateEnrolment`
and `revokeEnrolment` each used to run the save
anyway — a full rewrite of `settings.json` to record that nothing had changed,
reachable from every HTTP and IPC door by naming an id that does not exist.
None of them is the unauthenticated surface the login routes are, so the cost is
the lost-write one rather than the flood one; it is the same cost.

Two contracts survive that conversion unchanged, and both are why the refusal is
a *sentinel* rather than a bare error:

- **"Committed, side effect failed" stays distinguishable from "nothing
  persisted."** `errServiceProcess` and `errEnrolmentBundle` are raised *after*
  the write returned nil, and still hand back the record that landed.
- **A genuine save failure still reads as a save failure.** The decline and the
  refusal over an unreadable file both come back as the single error
  `withDeclinable` returns, so each caller separates them with `errors.Is`
  against its own not-found sentinel and wraps everything else as `save …`. HTTP
  status and IPC shape are chosen from that sentinel, so neither moved.

**`McpOps.StartOAuth` is the widest of these windows** (issue #52). It resolves
the MCP, then runs OAuth discovery, opens a browser and blocks on a callback
listener, and only then persists — so a removal landing in the middle was
reported as a persisted OAuth state that never landed. The record is resolved
again inside the write. `ServiceOps.Start` is the other half of #52 and is
**not** closed: it acts on the process registry rather than on settings, so
there is no `store.With` to move the check into, and closing it needs
synchronisation spanning two resources plus care around `Stop`, which
deliberately stops a service settings no longer names.

**The staging file has a unique name, which stops tearing and nothing else.**
`atomicWriteFile` used to stage through a fixed `<path>.tmp` opened `O_TRUNC`,
so two writers opened the *same* file: one truncated the other's half-written
bytes, and either could rename the mixture over the target. That is how a
settings.json ending `}}` gets onto disk, at which point every read fails
closed, `With` refuses to save and `EnsureInitialized` refuses to repair — the
control plane is out of service until a human intervenes. Staging through
`os.CreateTemp` in the target's own directory means no two writers ever share a
staging file. It does **not** make a cross-process read-modify-write atomic:
that remains last-writer-wins, exactly as stated above. The cost is that a
process killed between create and rename leaves a uniquely named `*.tmp` behind
rather than reusing one slot; nothing reads those files, and no sweeper was
added because a timer rewriting this directory is the writer nobody asked for.

## No settings file means no credentials

Deleting `settings.json` is how an operator locks the control plane out, so it
must behave like one: **a settings file that existed and no longer does
resolves to *absent settings*, never to the last-loaded cache.** Every
credential in the deleted file stops authenticating on the next request, and
every enrolment in it stops being enrolled — a cache that outlived the file
would keep the whole set alive in memory with nothing on disk left to say so,
and no operator surface would show it.

This is one case of the general rule, which holds **for reads** in every
degraded state of the file: a settings.json that is corrupt, truncated,
unreadable or missing resolves to empty settings, and an empty credential set
fails **closed** (`frontendCredentialAuth`). Serving open would silently expose
every proxied service.

A write may not draw the same conclusion, and that is the whole of
[An unreadable file is unknown, not empty](#an-unreadable-file-is-unknown-not-empty)
below.

**Deleting `settings.json` alone still resolves this the same way it always
did** — relay comes up with nothing, the sealing key already in the login
keychain survives untouched, and a fresh, empty settings.json is sealed under
that same key on next start. That is a full loss of everything the deleted
file named, which is the point of using it as a lock-out.

It is a **different** case from a settings.json that names a `sealed_key_id`
the keychain no longer holds, or holds under a different id — deleting the
file does not repair that, because the mismatch was never about the file.
Recovering from *that* state is the sealed-store's own break-glass path: the
tray's **Reset Sealed Store…** item, behind a presence prompt, deletes
`settings.json` **and** the keychain item **and** the CA files together and
starts over. There is deliberately no CLI equivalent and no offline recovery
code — see [`docs/sealed-config.md`](sealed-config.md#break-glass-and-why-there-is-no-offline-recovery-code).

Two boundaries on that:

- **"Not created yet" is not "deleted".** A fresh install has no
  `settings.json` until `EnsureInitialized` writes one, and a process can
  legitimately hold settings that nothing has persisted. The store tracks
  whether it has ever seen the file, and only a file it *had* seen invalidates
  the cache when it goes missing. A file that was never there leaves the cache
  alone, so a first start comes up and creates its settings rather than losing
  them.
- **The modtime is the only signal.** The store notices a change by stat'ing
  the file, so a state change that does not move the modtime is invisible to
  it. The one reachable case is `chmod 000` on an otherwise untouched
  settings.json: the file is now unreadable, but nothing tells the store to
  look again, so it keeps answering from the copy it already parsed until
  something else moves the modtime. Every state that involves a *write* or a
  deletion moves it. The signal is only needed to *enter* the degraded state:
  once a read has failed, the store stops trusting the modtime and re-reads on
  every look, so the `chmod` back that repairs it needs no timestamp of its own.

## An unreadable file is unknown, not empty

Missing and unreadable are indistinguishable to a *reader* — both resolve to
empty settings, both fail closed — and they are opposite states to a *writer*.

- A file that is **absent** has known contents: nothing. Relay may write one,
  and `EnsureInitialized` exists to do exactly that on a fresh install.
- A file that **exists and could not be read or parsed** has unknown contents.
  Every project, token hash, API credential, enrolment and OAuth refresh token
  on the host may still be in it. Resolving that to empty settings is right for
  a read and catastrophic for a write: saving a mutation on top of the emptiness
  rewrites settings.json as *defaults plus that one change*, destroying
  everything else — and reports success.

Three rules follow.

**`FileSettingsStore.With` refuses.** The callback never runs, nothing is
written, the file is left byte for byte as it was. The error matches
`errSettingsUnreadable` under `errors.Is` and names the path and the underlying
read or parse failure, so a caller can tell it from a save that was attempted
and failed — the two want different operator responses (repair the file vs.
free the disk).

**`EnsureInitialized` refuses too, and the tray exits.** Creating settings is
its whole job, and a file that exists and cannot be read is not the case it
exists for; writing one over it is a total wipe repeated at every launch, with
no operator surface saying so. Starting anyway — read-only, on the empty
settings the read produced — was the alternative, and it is worse: it shows the
operator a relay that appears to have lost every project and credential, which
invites them to rebuild it by hand and make the loss real, while every write
silently refuses. A refusal at launch naming the file leaves the only
recoverable copy on disk and says what to do with it.

**The refusal does not latch.** An `EACCES` or an `EIO` can be a moment rather
than a verdict, and the two acts that make a file readable again — a `chmod`
back, or a hand-edit that finally parses — need not move the modtime the store
watches. So a store that has seen a read fail re-reads on every look until one
succeeds, and resumes normally the moment it does. There is no retry loop and
no timer: the cost is one extra read per look, paid only while degraded.

The boundary is the same one the deletion rule draws: an **absent** file is
still written. A fresh install starts, `EnsureInitialized` creates
settings.json, and a store holding settings that nothing has persisted yet
still saves them.

**A stat that fails closes reads too, not only writes.** Setting `readErr` is
what refuses the next write; on its own it left the cache and the return
untouched, so an authorization decision resolving through `freshSettings` kept
answering from a copy older than the failure — stale rather than closed, and the
only degraded state of the file that behaved that way. The store now re-reads
there and lets `load()` decide `readErr` itself, which resolves to the file
rather than to the stat's guess in the case where the two disagree. The boundary
is the deletion rule's: a file this store has **never** seen is left alone,
because a first start holds settings nothing has written out yet. Reaching this
at all needs the config *directory* to become unusable, where `atomicWriteFile`
fails anyway; what it buys is that the file's degraded states now all give the
same answer.

**A record is normalized before it is written, not only after it is read.**
`normalize()` fills a nil slice or map with an empty one so every record spells
"empty" the same way. It ran on `load()` and not on what a `With` callback
produced, so a record appended by a mutation reached disk with `"args": null`
beside records spelling the same emptiness `[]`. A round trip through `load()`
repairs it, so nothing in-process ever noticed — but `relay audit`,
`relay grant` and a hand-edit read the **file**, and a file whose records
disagree about how emptiness is spelled invites the reader to conclude the two
spellings mean different things. `normalize()` is idempotent and only ever
replaces nil with empty, so nothing it touches can be a value an operator
chose.

## Directory auth (`allow_cwd_auth`) — retired

The `allow_cwd_auth` project field and its token-less, caller-asserted-cwd
bridge-auth fallback described in earlier revisions of this document are
retired (plan-broker-and-sessions.md's C3 decision): the field no longer
exists, a caller's asserted working directory is never authenticated, and
there is nothing to enable in Settings. The reasoning that retired it: the
cwd was always caller-asserted, never kernel-attested, so anything able to
lie about its cwd could reach a project's scope without ever holding that
project's credential.

Its replacement is C3's membership check: a tokenless caller authenticates
only by *being* a real, kernel-verified descendant of a live project
session's root process (ancestry walked via `proc_pidinfo`, matched on pid
and exact process start time) — never by presenting or asserting anything
of its own. This is live in `cmd/relay/router.go`'s `resolveAuth` (its third
and last step, tried only once a token is absent and the peer holds no
launch identity of its own) and covered here as the retirement it caused;
the session-host system that gives a tokenless caller something to be a
descendant *of* — the `project_session` launch identity kind, the shim that
roots a session, and the internal API that authorizes launching one — is
[`docs/session-host.md`](session-host.md). See
[`plan-broker-and-sessions.md`](../plan-broker-and-sessions.md) §2 C3 and
[`docs/launch-identity.md`](launch-identity.md#project_session-the-session-host-identity-kind)
for the mechanism.

## What determines a caller's authority now

Summarizing the state this document's history led to. This is **not** one
ordered list with a fallthrough: a caller acting as a project's grant is
authorized by `resolveAuth` (`cmd/relay/router.go`), and a caller acting as
a fixed service-kind capability is authorized by the entirely separate
`requireServiceIdentity`/`identityAllowed` path (`cmd/relay/router.go`,
[`docs/session-host.md`](session-host.md#the-sessionexited-bridge-notification)
has a worked example) — the second never consults `resolveAuth` at all.
Within `resolveAuth`, in the order it actually checks them — every one of
these is real and reachable today, not aspirational:

1. **A project token** (`RELAY_PROJECT_TOKEN`), if presented, resolves to
   its project's grant. Present-but-invalid is a hard refusal; it never
   falls through to a later step.
2. **A bound `project_session`-kind launch identity** on the peer's own
   kernel-attested connection (the session-host root process itself) acting
   under its named project's own live grant. `resolveAuth` refuses outright
   a bound identity of any *other* kind here — it checks `id.Kind !=
   service.IdentityKindProjectSession` and returns unauthorized rather than
   trying step 3 — so a `service`-kind identity never falls through to C3
   membership; it is authorized on the separate path named above instead
   ([`docs/launch-identity.md`](launch-identity.md#identity-kinds-and-capabilities)).
3. **C3 membership**: no token, no bound identity of the peer's own, but the
   peer is a kernel-verified process-tree descendant of a live
   `project_session`'s root — the mechanism directly above.
4. Otherwise, unauthorized.

`RELAY_TOKEN` (the pre-migration alias) and `allow_cwd_auth` are both fully
retired from this list: nothing in this repo reads either one to authenticate
a caller any more (`RELAY_TOKEN` is stripped, never read, everywhere under
`internal/sessions/`; `resolveMcpToken` in `cmd/relay/exec_cmd.go` never
reads it either). `RELAY_PROJECT_TOKEN` remains exactly where it always was:
injected into a project shell, an agent CLI, or the `relay mcp` child that
isn't reached through the session-host path at all. A per-launch **model
key** (`rmk_…`, the inventory table above,
[`docs/model-endpoint.md`](model-endpoint.md#model-keys)) is a fourth,
narrower alternative again: not for reaching the bridge's tool surface at
all, but for a spawned session to call the model endpoint without holding
its project's own token.
