# Token reference

The canonical inventory of every credential in the relay ecosystem: what it is,
what it can do, and where it lives. See ADR-007 (`docs/decisions/007-project-token-brokering.md`)
for the project-token brokering model.

| Token | Env var | Purpose | Privilege / scope | Lifecycle & storage |
|---|---|---|---|---|
| **Project token** | `RELAY_PROJECT_TOKEN` *(was `RELAY_TOKEN`)* | The security boundary for MCP tool access — identifies the project for a tool call; relay injects the authenticated `project_id` into `_meta`. Injected into project shells / LLM CLIs / the `relay mcp` child. | **Scoped.** Permissions derived at auth time from the project's `allowed_mcp_ids` + `disabled_tools`. | Long-lived. Plaintext + SHA-256 `TokenHash` stored inline in the project in `settings.json` (0600). Rotatable via `rotate_token`. |
| **Service token** | `RELAY_SERVICE_TOKEN` *(was `RELAY_MCP_TOKEN`)* | Authenticates a spawned service (e.g. relayLLM) to relay's **bridge** for broker/admin ops: `ResolvePtyEnv`, `RegisterManifest`, `ListProjects`/`GetProject`. | **Full, unfiltered bridge access** — bypasses all per-project tool filtering (router treats `Name=="service"` as god-mode). | Ephemeral, in-memory, minted per service spawn (`service_registry.go`). Never persisted. **Never injected into a child shell.** |
| **Frontend token** | `RELAY_FRONTEND_TOKEN` | Authenticates frontend consumers (eve) to relay's front-door Unix socket. | Front-door access; bearer-checked on every HTTP + WS before dispatch. Defense-in-depth atop the 0600 socket. Empty configured token fails **closed**. | Minted by relay per process (crypto/rand); handed to eve via env at spawn. |
| **Enhanced-service internal bearer** | declared via `RegisterManifest` (per service) | Secures the internal socket between relay's dispatcher and an enhanced service (relayLLM, relayScheduler). Relay strips inbound `Authorization` and injects this token when proxying front-door traffic onward. | That service's internal endpoint only. Distinct from frontend creds. | Each service picks its own socket + token; told to relay at manifest registration. |
| **Admin secret** | `AdminSecret` (`ValidateAdmin`) | Gates admin-only bridge ops: `ReconcileExternalMcps`, `ReloadExternalMcp`, `ReloadService`. | Administrative control-plane. | Constant-time compared at the bridge layer. |
| **OAuth 2.1 tokens** | per HTTP MCP (`oauth.go`) | Authenticate relay to **upstream** HTTP MCP servers (PKCE, dynamic registration, auto-refresh). | The upstream provider, not relay's own boundary. | Access + refresh tokens stored per-MCP. |
| **eve session token** | `eve_session` (browser localStorage) | Authenticates a human/browser user to **eve itself** — *not* a relay credential; listed to disambiguate. | eve's own app auth. | Independent of relay. |

Notes:

- `TokenHash` is not a separate credential — it's the SHA-256 at-rest/comparison
  form of the project token.
- The **project token** and **service token** are deliberately distinct: a
  project token is scoped to one project's tools; a service token is full bridge
  access. Relay never injects a service token into a spawned child — if a project
  token can't be resolved, the child gets no token at all (fail closed).
- Legacy env names `RELAY_TOKEN` / `RELAY_MCP_TOKEN` are accepted as transition
  fallbacks for one release and will be removed once relay + relayLLM have both
  shipped the rename.
