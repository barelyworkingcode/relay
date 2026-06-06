# ADR-007: Relay Is the Sole Broker of Project Tokens

**Status:** Accepted
**Date:** 2026-06-06

## Context

A project token is the security boundary for MCP tool access: it maps to a
project and derives that project's allowed MCPs/tools at auth time. The intended
topology is `eve ↔ relay ↔ relayLLM`, with relay as the broker — eve and
relayLLM should reference a project by **id** and let relay turn an id into the
secret token. The pre-existing implementation leaked that boundary in both
directions:

1. **eve handled the raw token.** relay serialized the plaintext `Token` on every
   `GET /api/projects`; eve's server cached it and re-served it to the browser
   (which stored it verbatim, though never used it), and eve injected it as
   `mcpToken` into `POST /api/sessions` itself. The secret travelled over the
   wire to the browser and lived in eve's process memory.

2. **relayLLM stored the token on the session** (`Session.McpToken`, in-memory,
   `json:"-"`), which was lost on restart — sessions resumed after a relayLLM
   restart degraded.

3. **Shells mostly got no token, and could get the *wrong* token.** Only the
   `claude-code` terminal template injected a token; a plain `shell` got none, so
   `relay mcp` calls from inside it were unauthenticated. Worse, when a session's
   stored token was empty, providers **fell back to the service token**
   (`RELAY_MCP_TOKEN`) — a full-access, unfiltered bridge credential — so a child
   process could silently come up with god-mode access instead of project scope.

4. The managed-terminal path resolved the project by **directory string match**,
   letting a caller bind an arbitrary cwd to any project's token.

## Decision

Relay is the single source of truth for project tokens. The invariants:

1. **eve never handles a project token.** It references projects by **id** only.
   relay's frontend HTTP responses omit the token (see `projectView` in
   `project_dto.go`); the sole exception is the explicit `rotate_token` response.

2. **relayLLM never persists a project token and never receives it from eve.** It
   resolves the token **just-in-time** from relay's bridge by `projectId`
   (`resolveProjectToken`, `RelayManagedSpec.Resolve`), injects it into spawned
   children, and discards it. relayLLM is the spawning parent, so it necessarily
   holds the token transiently — the invariant is "never stored, never from eve,"
   not "never in memory." (Making it literally never touch the token would
   require relay to own PTY spawning — a far larger re-architecture that breaks
   the service/manifest separation; rejected.)

3. **The injected token is always project-scoped — never the service token.** If
   a project token can't be resolved, the child gets **no token** (fail closed).
   The service token's only job is authenticating relayLLM's own bridge calls.
   Crucially, relayLLM also **strips its own relay credentials from the inherited
   environment** of every child it spawns (`childBaseEnv` in relayLLM removes
   `RELAY_SERVICE_TOKEN`, the legacy `RELAY_MCP_TOKEN`, and `RELAY_FRONTEND_TOKEN`
   from `os.Environ()` before adding `RELAY_PROJECT_TOKEN`). Without this, a child
   shell would silently inherit relayLLM's full-access service token (and the
   unused front-door bearer) that relay injects into the relayLLM process —
   verified leaking into shells before the fix. The root cause — relay injected
   frontend creds into *every* spawned service (a holdover from when relayLLM
   hosted the frontend channel) even though only eve dials the front door — is
   also fixed: `ServiceConfig.FrontendConsumer` (`*bool`, default-inject for
   back-compat) gates the injection, and backends register with
   `service register --no-frontend-creds` (relayLLM's `build.sh` does). So
   relayLLM no longer receives the front-door bearer at all, and `childBaseEnv`
   is defense-in-depth for the service token it legitimately still holds.

4. **Resolution is by `projectId`, validated against the directory.** Relay's
   `ResolvePtyEnv` resolves the project by id and rejects a directory that isn't
   within the project's path (`dirWithinProject`), closing the confused-deputy
   hole. The legacy directory-match path remains for back-compat during
   migration.

5. **Project-scoped terminals get a token; ad-hoc terminals do not.** A terminal
   created with a `projectId` is resolved + injected with `RELAY_PROJECT_TOKEN`;
   one without gets nothing.

The two token env vars were renamed for clarity (the old `RELAY_TOKEN` /
`RELAY_MCP_TOKEN` pair invited confusion between scoped and full credentials):

- `RELAY_TOKEN` → `RELAY_PROJECT_TOKEN` (project-scoped, injected into children)
- `RELAY_MCP_TOKEN` → `RELAY_SERVICE_TOKEN` (full bridge access, relayLLM↔relay only)

Both legacy names are accepted as transition fallbacks for one release. See
`docs/tokens.md` for the full credential inventory.

## Consequences

- The "token lost on relayLLM restart" bug disappears for free — nothing is
  stored, so there is nothing to lose; the token is re-resolved on each spawn.
- A compromised or misconfigured child can no longer obtain a full-access token
  via the fallback path.
- relay + relayLLM must be deployed together for the env-var rename; the
  transition fallbacks (relay sets both service-token names; readers accept both
  project-token names) make the ordering forgiving. The one hard constraint: the
  frontend DTO token-strip must land with or after eve stops reading the token.

## Out of scope / follow-up

- **Project-token signing** (next refactor): scoped, verifiable tokens so relay
  can hand relayLLM a credential that can't be replayed for another project.
  This also closes the still-open findings that **any service token can read any
  project's plaintext via `ResolvePtyEnv`** and that **`RegisterManifest`
  identity isn't bound to the token** (see
  `memory/project_deferred_security_findings.md`). Today's resolver keeps the
  `requireServiceToken` gate + directory-containment check as interim mitigation.
