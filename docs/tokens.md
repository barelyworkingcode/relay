# Token reference

The canonical inventory of every credential in the relay ecosystem: what it is,
what it can do, and where it lives. See ADR-007 (`docs/decisions/007-project-token-brokering.md`)
for the project-token brokering model.

| Token | Where it's named | Purpose | Privilege / scope | Lifecycle & storage |
|---|---|---|---|---|
| **Project token** | env `RELAY_PROJECT_TOKEN` *(legacy: `RELAY_TOKEN`)* | The security boundary for MCP tool access — identifies the project for a tool call; relay injects the authenticated `project_id` into `_meta`. Injected into project shells / LLM CLIs / the `relay mcp` child. | **Scoped.** Permissions derived at auth time from the project's `allowed_mcp_ids` + `disabled_tools`. | Long-lived. Plaintext (`Token`) + SHA-256 (`TokenHash`) stored inline in the project in `settings.json` (0600). Rotatable via the `rotate_token` HTTP route / `rotate_project_token` IPC. |
| **Service token** | env `RELAY_SERVICE_TOKEN` *(legacy: `RELAY_MCP_TOKEN`)* | Authenticates a spawned service (e.g. relayLLM) to relay's **bridge** for broker/admin ops: `ResolvePtyEnv`, `RegisterManifest`, `ListProjects`/`GetProject`. | **Full, unfiltered bridge access** — bypasses all per-project tool filtering (router treats `Name=="service"` as god-mode). | Ephemeral, in-memory, minted per service spawn (`service_registry.go`). Never persisted. **Never injected into a child shell.** |
| **Frontend token** | env `RELAY_FRONTEND_TOKEN` | Authenticates frontend consumers (eve) to relay's front-door Unix socket. | Front-door access; bearer-checked on every HTTP + WS before dispatch. Defense-in-depth atop the 0600 socket. Empty configured token fails **closed**. | Minted by relay per process (crypto/rand, 32-byte hex); handed to frontend consumers via env at spawn. |
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

## Directory auth (`allow_cwd_auth`)

A project may opt into token-less bridge auth: with `allow_cwd_auth: true`, a
caller that presents **no** token but whose working directory is inside the
project's path is authenticated as that project. Default is off, per project.

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
