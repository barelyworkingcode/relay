# ADR-007: Relay Is the Sole Broker of Project Tokens

**Status:** Accepted
**Date:** 2026-06-06

## Context

A project token is the security boundary for MCP tool access: it maps to a
project and derives that project's allowed MCPs/tools at auth time. The intended
topology is `eve ↔ relay ↔ relayLLM`, with relay as the broker — eve and
relayLLM reference a project by **id** and let relay turn an id into the secret
token. The pre-existing implementation leaked that boundary in four ways:

1. **eve handled the raw token.** relay serialized the plaintext token on every
   `GET /api/projects`; eve cached it, re-served it to the browser, and injected
   it as `mcpToken` into `POST /api/sessions`. The secret travelled to the
   browser and lived in eve's process memory.

2. **relayLLM persisted the token on the session** (`Session.McpToken`,
   in-memory, `json:"-"`), so it was lost on restart — resumed sessions degraded.

3. **Shells got no token, or the wrong one.** A plain `shell` got no token, so
   `relay mcp` calls from inside it were unauthenticated. Worse, when a session's
   stored token was empty, providers **fell back to the service token**
   (`RELAY_MCP_TOKEN`) — a full-access, unfiltered bridge credential — so a child
   could silently come up with god-mode access instead of project scope.

4. The managed-terminal path resolved the project by **directory string match**,
   letting a caller bind an arbitrary cwd to any project's token.

## Decision

Relay is the single source of truth for project tokens. The invariants:

1. **eve never handles a project token.** It references projects by **id** only.
   relay's frontend HTTP responses omit the token (`projectView` in
   `project_dto.go`); the sole exception is the explicit `rotate_token` response
   (`project_routes.go`), which returns the new plaintext exactly once.

2. **relayLLM never persists a project token and never receives it from eve.** It
   resolves the token **just-in-time** from relay's bridge by `projectId`
   (`ResolvePtyEnv`), injects it into spawned children, and discards it. relayLLM
   is the spawning parent, so it necessarily holds the token transiently — the
   invariant is "never stored, never from eve," not "never in memory." (Making it
   literally never touch the token would require relay to own PTY spawning — a
   far larger re-architecture that breaks the service/manifest separation;
   rejected.)

3. **The injected token is always project-scoped — never the service token.** If
   a project token can't be resolved, the child gets **no token** (fail closed).
   The service token's only job is authenticating relayLLM's own bridge calls.
   As defense-in-depth, relayLLM also strips its own relay credentials
   (`childBaseEnv`) from every child's inherited environment. The root cause is
   also fixed relay-side: relay no longer injects front-door creds into backends.
   `ServiceConfig.FrontendConsumer` (`*bool`, default-inject for back-compat)
   gates the injection, and backends register with
   `service register --no-frontend-creds` (relayLLM's `build.sh` does), so the
   front-door bearer never reaches relayLLM at all.

4. **Resolution is by `projectId`, validated against the directory.**
   `ResolvePtyEnv` resolves the project by id and rejects a directory that isn't
   within the project's path (`dirWithinProject`), closing the confused-deputy
   hole. The legacy directory-match path remains for back-compat during migration.

5. **Project-scoped terminals get a token; ad-hoc terminals do not.** A terminal
   created with a `projectId` is resolved + injected with `RELAY_PROJECT_TOKEN`;
   one without gets nothing.

Two token env vars were renamed for clarity — project-scoped vs. full-access —
with both legacy names accepted as transition fallbacks for one release. See
`docs/tokens.md` for the full credential inventory and the rename mapping.

## Consequences

- The "token lost on relayLLM restart" bug disappears for free — nothing is
  stored, so the token is re-resolved on each spawn.
- A compromised or misconfigured child can no longer obtain a full-access token
  via the fallback path.
- relay + relayLLM must be deployed together for the env-var rename; the
  transition fallbacks make the ordering forgiving. The one hard constraint: the
  frontend DTO token-strip must land with or after eve stops reading the token.

## Out of scope / follow-up

- **Project-token signing** (next refactor): scoped, verifiable tokens so relay
  can hand relayLLM a credential that can't be replayed for another project. This
  closes two still-open findings: **any service token can read any project's
  plaintext via `ResolvePtyEnv`** (`router.go`), and **`RegisterManifest`
  identity isn't bound to the token** (`router.go`). Today's resolver keeps the
  `requireServiceToken` gate + directory-containment check as interim mitigation.