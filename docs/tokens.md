# Token reference

The canonical inventory of every credential in the relay ecosystem: what it is,
what it can do, and where it lives. See ADR-007 (`docs/decisions/007-project-token-brokering.md`)
for the project-token brokering model.

| Token | Where it's named | Purpose | Privilege / scope | Lifecycle & storage |
|---|---|---|---|---|
| **Project token** | env `RELAY_PROJECT_TOKEN` *(legacy: `RELAY_TOKEN`)* | The security boundary for MCP tool access — identifies the project for a tool call; relay injects the authenticated `project_id` into `_meta`. Injected into project shells / LLM CLIs / the `relay mcp` child. | **Scoped.** Permissions derived at auth time from the project's `allowed_mcp_ids` + `disabled_tools`. | Long-lived. Plaintext (`Token`) + SHA-256 (`TokenHash`) stored inline in the project in `settings.json` (0600). Rotatable via the `rotate_token` HTTP route / `rotate_project_token` IPC. |
| **Service token** | env `RELAY_SERVICE_TOKEN` *(legacy: `RELAY_MCP_TOKEN`)* | Authenticates a spawned service (e.g. relayLLM) to relay's **bridge** for broker/admin ops: `ResolvePtyEnv`, `RegisterManifest`, `ListProjects`/`GetProject`. | **Full, unfiltered bridge access** — bypasses all per-project tool filtering (router treats `Name=="service"` as god-mode). | Ephemeral, in-memory, minted per service spawn (`service_registry.go`). Never persisted. **Never injected into a child shell.** |
| **Frontend token** | env `RELAY_FRONTEND_TOKEN` | Authenticates frontend consumers (eve) to relay's front-door Unix socket. | Whatever the credential it migrates to holds — `read`+`configure` today, never `grant` or `execute`. Checked on every HTTP + WS before dispatch. Defense-in-depth atop the 0600 socket. No credentials at all fails **closed**. | Minted by relay per process (crypto/rand, 32-byte hex); handed to frontend consumers via env at spawn. Recorded as a control-plane credential named `legacy-frontend-token` on every start. |
| **Control-plane credential** | `settings.json` field `api_credentials`; minted by `relay credential mint` | Authenticates a caller to relay's control-plane HTTP API (the frontend socket and, if bound, `RELAY_API_LISTEN`). Replaces the single frontend bearer as the API's authenticator (ADR-015). | **Classed.** Carries an explicit set of `read` / `configure` / `grant` / `execute`; an absent set grants nothing. `execute` is socket-only. | Long-lived. SHA-256 only in `settings.json` (0600) — the plaintext is printed once by `relay credential mint` and is not recoverable. Revoke with `relay credential revoke --id ID`. |
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

### The four classes and what each reaches

| class | what it means | routes |
|---|---|---|
| `read` | discloses configuration or history | `GET /api/projects`, `/api/projects/{id}`, `/api/services`, `/api/services/{id}`, `/api/enrolments`, `/api/enrolments/{id}`, `/api/remote`, `/api/mcps`, `/api/mcps/{id}/tools`, `/api/mcps/{id}/scope_fields`, `/api/audit`, `/api/audit/log`; `POST /api/mcps/{id}/enumerate` (a POST that discloses and changes nothing) |
| `configure` | changes relay's own state | `POST`/`PUT`/`DELETE /api/projects…`, `POST /api/projects/{id}/regen_skill`, `DELETE /api/services/{id}`, `POST /api/services/{id}/start`\|`stop`, `PUT /api/services/{id}/autostart`, `DELETE /api/mcps/{id}`, `POST /api/audit/export`, **and the `/` catch-all that proxies to enhanced services** |
| `grant` | issues or revokes a credential another party holds | `POST /api/enrolments`, `DELETE /api/enrolments/{id}`, **`POST /api/projects/{id}/rotate_token`** |
| `execute` | the *caller* supplies what runs or what is exposed | `POST /api/mcps`, `POST /api/services`, `PUT /api/services/{id}`, `PUT /api/remote` |

An absent or empty class set grants **nothing** — never "everything", never
"read". A credential minted by a tool that predates the class model is inert.

**`execute` is socket-only.** Those four routes are not registered on the
loopback TCP mux at all, so a caller there gets a genuine no-route 404 however
its credential is classed (ADR-015 decision 2). The class still exists on the
credential so the socket path can tell a consumer that needs it from one that
does not.

**The proxied surface is `configure`.** The `/` catch-all — every route an
enhanced service registers via its manifest, `/ws` included — is registered
under `configure`, so a `read`-only credential cannot reach relayLLM sessions
or terminals through it. Whether a proxied route that starts a terminal is
really `execute` is open as issue #50.

### Minting

    relay credential mint --name eve-view --class read --class configure
    relay credential list
    relay credential revoke --id 5d6dad87-4a31-40f6-88f8-9193adcba554

`--class` is repeatable, like `relay enrol create --grant`. An unrecognized
class is a hard error naming all four, and an empty class set is refused at the
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

`legacy-frontend-token` is reserved: the migration below rewrites that record's
hash on every relay start, so an operator-minted credential under that name
would be silently clobbered. Both `mint` and `revoke` refuse it.

### The legacy frontend token

`RELAY_FRONTEND_TOKEN` still works, unchanged, for every consumer relay injects
it into. It works *as a credential*: on every start relay records it as
`legacy-frontend-token` holding exactly `read`+`configure`. There is no second
authentication path for it.

That is a **narrowing**, and it has a consequence worth stating plainly: a
consumer that needs `grant` or `execute` over HTTP must now mint its own
credential naming that class. In particular, **project token rotation
(`POST /api/projects/{id}/rotate_token`) is `grant`-class** — it issues a
credential another party holds — so an existing consumer that rotates project
tokens needs a credential of its own:

    relay credential mint --name my-rotator --class grant

The same applies to `POST /api/enrolments` and `DELETE /api/enrolments/{id}`,
and to the four `execute` routes on the socket.

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
