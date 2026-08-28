# Token reference

The canonical inventory of every credential in the relay ecosystem: what it is,
what it can do, and where it lives. See ADR-007 (`docs/decisions/007-project-token-brokering.md`)
for the project-token brokering model.

| Token | Where it's named | Purpose | Privilege / scope | Lifecycle & storage |
|---|---|---|---|---|
| **Project token** | env `RELAY_PROJECT_TOKEN` *(legacy: `RELAY_TOKEN`)* | The security boundary for MCP tool access — identifies the project for a tool call; relay injects the authenticated `project_id` into `_meta`. Injected into project shells / LLM CLIs / the `relay mcp` child. | **Scoped.** Permissions derived at auth time from the project's `allowed_mcp_ids` + `disabled_tools`. | Long-lived. Plaintext (`Token`) + SHA-256 (`TokenHash`) stored inline in the project in `settings.json` (0600). Rotatable via the `rotate_token` HTTP route / `rotate_project_token` IPC. |
| **Service token** | env `RELAY_SERVICE_TOKEN` *(legacy: `RELAY_MCP_TOKEN`)* | Authenticates a spawned service (e.g. relayLLM) to relay's **bridge** for broker/admin ops: `ResolvePtyEnv`, `RegisterManifest`, `ListProjects`/`GetProject`. | **Full, unfiltered bridge access** — bypasses all per-project tool filtering (router treats `Name=="service"` as god-mode). | Ephemeral, in-memory, minted per service spawn (`service_registry.go`). Never persisted. **Never injected into a child shell.** |
| **Frontend token** | env `RELAY_FRONTEND_TOKEN` | Authenticates frontend consumers (eve) to relay's front-door Unix socket. | Whatever the credential it migrates to holds — `read`+`configure`+`proxy` today, never `grant` or `execute`. Checked on every HTTP + WS before dispatch. Defense-in-depth atop the 0600 socket. No credentials at all fails **closed**. | Minted by relay per process (crypto/rand, 32-byte hex); handed to frontend consumers via env at spawn. Recorded as a control-plane credential named `legacy-frontend-token` on every start. |
| **Control-plane credential** | `settings.json` field `api_credentials`; minted by `relay credential mint` | Authenticates a caller to relay's control-plane HTTP API (the frontend socket and, if bound, `RELAY_API_LISTEN`). Replaces the single frontend bearer as the API's authenticator (ADR-015). | **Classed.** Carries an explicit set of `read` / `configure` / `grant` / `execute` / `proxy`; an absent set grants nothing. `execute` and `proxy` are socket-only. | Long-lived by default; `relay credential mint --ttl 12h` gives one an expiry. SHA-256 only in `settings.json` (0600) — the plaintext is printed once by `relay credential mint` and is not recoverable. Revoke with `relay credential revoke --id ID`. |
| **Enhanced-service internal bearer** | declared via `RegisterManifest` (per service) | Secures the internal socket between relay's dispatcher and an enhanced service (relayLLM, relayScheduler). Relay strips inbound `Authorization` and injects this token when proxying front-door traffic onward. | That service's internal endpoint only. Distinct from frontend creds. | Each service picks its own socket + token; told to relay at manifest registration. |
| **Admin secret** | `settings.json` field `admin_secret` | Gates admin-only bridge ops: `ReconcileExternalMcps`, `ReloadExternalMcp`, `ReloadService`. | Administrative control-plane. | Auto-generated on first run; constant-time compared via `ValidateAdmin` at the bridge layer. |
| **OAuth 2.1 tokens** | per HTTP MCP (`oauth.go`) | Authenticate relay to **upstream** HTTP MCP servers (PKCE, dynamic registration, auto-refresh). | The upstream provider, not relay's own boundary. | Access + refresh tokens stored per-MCP (`OAuthState` in `settings.json`). |
| **eve session token** | `eve_session` (browser localStorage) | Authenticates a human/browser user to **eve itself** — *not* a relay credential; listed to disambiguate. | eve's own app auth. | Independent of relay. |

Notes:

- `TokenHash` is not a separate credential — it's the SHA-256 at-rest/comparison
  form of the project token.
- The **project token** and **service token** are deliberately distinct: a
  project token is scoped to one project's tools; a service token is full bridge
  access. Relay never injects a service token into a spawned child — if a project
  token can't be resolved, the child gets no token at all (fail closed).
- Legacy env names `RELAY_TOKEN` / `RELAY_MCP_TOKEN` are accepted as transition
  fallbacks for one release, to be removed once relay + relayLLM have both shipped
  the rename.

## Control-plane credentials (ADR-015)

The HTTP API that Eve, relayScheduler and the settings view consume is
authenticated by a **control-plane credential**, not by a single shared bearer.
Two checks run, in this order, and they do different jobs:

1. `frontendCredentialAuth` resolves the request's `Authorization: Bearer` to a
   credential in `Settings.APICredentials`, constant-time, before any handler
   runs — so an unauthenticated WS upgrade never allocates a session. Absent,
   malformed and unknown bearers get the same 401 with the same body.
2. `RouteRegistrar` checks the resolved credential against the **class** the
   route was registered under, and returns 403 if it does not hold it.

The first answers "is this anyone?"; the second answers "may they do this?".
They were briefly conflated, with the outer check admitting exactly one token,
which made every credential except that one unusable — the class model existed
and could not be reached.

### The five classes and what each reaches

| class | what it means | routes |
|---|---|---|
| `read` | discloses configuration or history | `GET /api/projects`, `/api/projects/{id}`, `/api/services`, `/api/services/{id}`, `/api/enrolments`, `/api/enrolments/{id}`, `/api/remote`, `/api/mcps`, `/api/mcps/{id}/tools`, `/api/mcps/{id}/scope_fields`, `/api/audit`, `/api/audit/log`; `POST /api/mcps/{id}/enumerate` (a POST that discloses and changes nothing) |
| `configure` | changes relay's own state | `POST`/`PUT`/`DELETE /api/projects…`, `POST /api/projects/{id}/regen_skill`, `DELETE /api/services/{id}`, `POST /api/services/{id}/start`\|`stop`, `PUT /api/services/{id}/autostart`, `DELETE /api/mcps/{id}`, `POST /api/audit/export` |
| `grant` | issues or revokes a credential another party holds | `POST /api/enrolments`, `DELETE /api/enrolments/{id}`, **`POST /api/projects/{id}/rotate_token`** |
| `execute` | the *caller* supplies what runs or what is exposed | `POST /api/mcps`, `POST /api/services`, `PUT /api/services/{id}`, `PUT /api/remote` |
| `proxy` | reaches a surface relay has **not** classified | the `/` catch-all — every route an enhanced service registers via its manifest, `/ws` included |

An absent or empty class set grants **nothing** — never "everything", never
"read". A credential minted by a tool that predates the class model is inert.

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
- Eve and relayScheduler are unaffected: they dial the frontend **socket**,
  and the legacy-token migration grants `proxy` alongside `read`+`configure`.

`proxy` is a class rather than `execute` because `execute` would also hand the
legacy token `POST /api/mcps` and `PUT /api/services/{id}` — the two things
the migration exists to withhold. Classing per route is the real answer and
needs the manifest to describe blast radius, which is a protocol change across
repositories and is still deferred.

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

The CLI runs in a separate process from the tray and writes through
`store.With`; `Authorize` reads through `freshSettings`, so a credential minted
here authenticates on the API's very next request, with no restart and no poll
interval — the same guarantee `relay enrol create` gets (issue #21).

That guarantee has a write half, and it is the half that is easy to lose. See
[The settings file has more than one writer](#the-settings-file-has-more-than-one-writer)
below: a credential's plaintext is printed once and is unrecoverable, so a
minted credential that a later write erases leaves the operator holding a token
that 401s with nothing on any surface saying why.

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

**This is not a reversal of the enrolment trade.** `enrolment_ca.go` chose
revocation over expiry for client certificates deliberately, because a short
certificate lifetime needs an authenticated renewal path, and any credential
replayable to obtain a fresh certificate reintroduces a bearer secret at the
one point where the result is a *new identity*. That argument does not
transfer here and the two conclusions do not conflict: an enrolment is
long-lived by design and revoked, while a login credential is short-lived by
design and its renewal path is *another ceremony* — an unforgeable
user-presence act rather than a replayable secret. Expiry is affordable
exactly where revocation was the only option.

`legacy-frontend-token` is reserved: the migration below rewrites that record's
hash on every relay start, so an operator-minted credential under that name
would be silently clobbered. Both `mint` and `revoke` refuse it.

### The legacy frontend token

`RELAY_FRONTEND_TOKEN` still works, unchanged, for every consumer relay injects
it into. It works *as a credential*: on every start relay records it as
`legacy-frontend-token` holding exactly `read`+`configure`+`proxy`. There is no
second authentication path for it. An install whose record predates `proxy` is
upgraded in place on the next start — same id, same created date, wider class
set — because the migration owns that record's class set outright.

That is a **narrowing**, and it has a consequence worth stating plainly: a
consumer that needs `grant` or `execute` over HTTP must now mint its own
credential naming that class. In particular, **project token rotation
(`POST /api/projects/{id}/rotate_token`) is `grant`-class** — it issues a
credential another party holds — so an existing consumer that rotates project
tokens needs a credential of its own:

    relay credential mint --name my-rotator --class grant

The same applies to `POST /api/enrolments` and `DELETE /api/enrolments/{id}`,
and to the four `execute` routes on the socket.

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

**A second presentation of the code, not a second anchor.** The tray's
**Show Login Code...** item mints through the same `mintBootstrapCode` inside
the same `store.With` `relay login enrol` uses, and shows the result in the
Settings window — relay is `LSUIElement`, so that window is the only surface
the tray has. ADR-016 decision 2 accepts it as a presentation and refuses it as
*the* source: the tray menu is unreachable over SSH and from the hermetic tier,
and a capability reachable only from a mouse is a capability half-built. The
"replaces rather than accumulates" rule is unchanged and is stated on screen —
opening the item twice leaves exactly one code working, the second.

## The settings file has more than one writer

Every credential in this document except the ephemeral ones lives in one file,
`settings.json`, and **relay is not one process**. The tray holds it open for
the life of the app; `relay credential mint`, `relay enrol create`,
`relay service register` and `relay mcp register` each write it from a process
that exits seconds later. Two rules follow, and they are separate rules
answering opposite directions of the same fact.

**Reads go through `freshSettings`, never `store.Get()`.** `Get()` answers from
an in-memory cache that only a mutation made by this process and the tray's 2 s
poll refresh, which is fine for a menu and wrong for an authorization decision.
`freshSettings` resolves through `ReloadIfChanged`, which stats the file and
re-reads only when it moved — so a record another process just wrote is
authoritative on the very next request. Cost is one stat per decision.

**Writes reload before they apply.** `FileSettingsStore.With` re-reads the file
under the same lock before running its callback, using the same
stat-then-read-only-if-moved machinery, so a mutation is always derived from
what is on disk rather than from a cache that could be a whole poll interval
old. Without that, a tray whose cache predated a CLI's write would serialize
its own stale view back over the top and silently destroy the record — worst
for a credential, whose plaintext was printed once and cannot be reissued.
Within a single process the mutex alone would be enough; it is the second
process that makes the reload necessary. A reload that could not read the file
does not fall back to defaults and save those: the write is refused, per
[An unreadable file is unknown, not empty](#an-unreadable-file-is-unknown-not-empty).

Two consequences worth stating rather than discovering:

- **The callback sees fresher state than its caller did.** The reload happens
  inside `With`, so anything a caller read beforehand may already be stale by
  the time the callback runs. This is why every ops core (`service_ops.go`,
  `enrolment_ops.go`, `mcp_ops.go`) resolves the record it is about to change
  *inside* the callback and reports "not found" from a flag set there —
  resolving outside and mutating inside is a TOCTOU window on a file two
  processes write.
- **It is still last-writer-wins.** The reload closes the window between the
  tray's cached view and the file; it does not make read-modify-write atomic
  against another process writing in the gap between the reload and the save.
  Nothing in relay takes a lock across processes on this file. The remaining
  window is the duration of one callback plus one `atomicWriteFile`, against a
  writer that must land inside it — a different order of magnitude from a 2 s
  poll interval, and not zero. A cross-process lock is the fix if that ever
  matters; the durability half is already handled (`atomicWriteFile` fsyncs the
  temp file, renames, and fsyncs the directory, so a crash cannot leave a
  half-written or zero-length settings.json behind).

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
one save path, and the 23 existing call sites are unaffected.

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
honour it with. The four ops cores that still write on a not-found
(`ServiceOps.Update`/`Remove`/`SetAutostart`, `McpOps.Remove`) are a
smaller version of the same thing and are not yet converted.

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

## Directory auth (`allow_cwd_auth`)

A project may opt into token-less bridge auth: with `allow_cwd_auth: true`, a
caller that presents **no** token but whose working directory is inside the
project's path is authenticated as that project. Default is off, per project.

- **Remote projects can't enable it.** `allow_cwd_auth` compares a caller's
  cwd against the project's `Path`, and a remote project — a capability
  grant to a client on another machine, see ADR-009 — has no `Path`: a
  remote caller's cwd is a path on a *different* machine, with nothing on
  the host to compare it against. `validateProjectShape` (`project.go`)
  refuses the combination outright rather than let it silently mean
  nothing.

- **Scope is unchanged.** The caller gets exactly the project's token scope —
  same derived permissions, same `disabled_tools`, same `_meta` context, same
  `project_id`. Directory auth changes how a caller is *identified*, never what
  the project may reach.
- **Only the absence of a token triggers it.** A token that is present but
  invalid is still a hard failure; the fallback never rescues a bad credential.
  Relay ignores an inbound `cwd` whenever a token is set, so a directory can't
  re-scope an authenticated call.
- **Service ops stay out of reach.** Directory auth yields a project-scoped
  token, and `requireServiceToken` resolves against a bare context, so
  `ResolvePtyEnv` / `RegisterManifest` / project reads can never be satisfied
  this way.
- **Nested projects** resolve to the most specific opted-in project containing
  the directory. A nested project that has *not* opted in does not shadow an
  opted-in parent.
- **The cwd is asserted, not attested** — the client sends its own
  `os.Getwd()`. That is deliberate: `settings.json` is 0600 and already holds
  every project token in plaintext, so anything that could lie about its cwd can
  already read the token it would be forging. This is not a boundary against
  another user; it is a convenience for the local user.
- **What it costs.** The deliberate hand-off. With a token, a process holds a
  project's tools because something gave it the credential; with this flag, any
  process running as the user gets them by standing in the directory. Grants are
  logged (`cwd auth granted`) because that log is the only audit trail left.

Enable per project in Settings → Projects → **Directory Auth**, or via
`allow_cwd_auth` on the create/update project APIs (HTTP and IPC).
