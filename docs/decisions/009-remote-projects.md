# ADR-009: Remote Projects Are Capability Grants, Not Directories

**Status:** Accepted
**Date:** 2026-08-19

## Context

Step 2a of letting a semi-trusted LLM agent run on a separate VM and reach
relay for tool access, instead of running on the host where it would have the
world. Relay becomes a gated control plane for sensitive HOST resources —
mail, iMessage, calendar. The VM never touches the host filesystem, so whole
tool classes (`fs_*`, `bash*`) do not apply to a project scoped to such a
client. That is the intended outcome, not a limitation to work around.

The threat model is **exfiltration, not privilege escalation**: the VM cannot
escape to the host, so the risk worth designing against is an agent reading an
entire mailbox and shipping it somewhere, not an agent breaking out of its
sandbox.

This PR makes the project model coherent for that shape of client. It does
not make a remote project reachable — the listener, client certificates, and
enrollment flow are 2b. See Consequences.

## Decision

Add a second `ProjectKind`, `remote`, alongside the existing (now-implicit)
`local`. A remote project is a pure capability grant to a client on another
machine, with no filesystem root of its own.

1. **A remote project is a capability grant, not a folder.** `Kind`'s zero
   value reads as local (`types.go`), so every project already on disk —
   which predates this field — round-trips untouched. Nothing compares
   against `ProjectKindLocal` directly anywhere in the codebase; every check
   goes through `IsRemote()`. That is deliberate: an equality check against
   `"local"` invites a future `Kind != ProjectKindRemote` written the wrong
   way round, and an unset field must never be able to mean "remote" no
   matter how it's tested. `normalizeProjectKind` mirrors this on write —
   anything that isn't `ProjectKindRemote` collapses to `""` before it's
   stored, so a local project never persists the literal string `"local"`.

2. **Validation refuses incoherent combinations rather than degrading.**
   `validateProjectShape` (`project.go`) is the single point that decides
   whether a candidate project's shape is coherent, called from both the
   create and update paths. For a remote project it refuses, in turn: a
   non-empty `Path`; `AllowCwdAuth` (a remote caller's cwd is a path on a
   *different* machine — there is nothing to compare it against); non-empty
   `ShellTemplates` (they launch a terminal on the project's host directory,
   which doesn't exist); and `GenerateSkill`. The `generate_skill` case is
   the clearest illustration of the principle: `regenProjectSkills`
   (`router.go`) already silently skips any project whose skill directory
   resolves empty, so leaving the flag settable on a remote project wouldn't
   break anything — it would just make the flag an inert toggle that lies
   about what it does. Refusing it at the door is more honest than a control
   that quietly no-ops.

3. **`allowed_dirs` is defended twice.** `SyncProjectToken` (`settings.go`)
   writes `allowed_dirs: [proj.Path]` into an MCP's context whenever that
   MCP's runtime schema declares the field. For a pathless project, both
   obvious behaviors are unsafe: writing `allowed_dirs: [""]` hands a
   downstream MCP an empty root to interpret (a Node MCP's
   `path.resolve("")` resolves to *its own* cwd, which can be far more
   permissive than intended), and omitting the field entirely lets the MCP
   fall back to whatever default it has, possibly unrestricted. Relay cannot
   see how a given MCP resolves either case. So there are two independent
   defences: `ValidateProjectGrants` refuses to *grant* a remote project any
   MCP whose schema declares `allowed_dirs` in the first place, naming the
   offending MCP; and `SyncProjectToken` separately skips the derivation for
   any remote project, unconditionally, before it ever loops over
   `mcpIDs`. Belt-and-braces is justified here specifically because the
   failure mode is a *silent* widening of filesystem scope rather than a
   loud error, and because MCP context schemas are discovered at runtime —
   an MCP could add an `allowed_dirs` field to its schema *after* a grant
   was already validated against an earlier version of that schema. The
   second defence, living inside the function that actually writes context,
   is what makes that later addition survivable instead of a silent hole.

4. **`allowed_mcp_ids: ["*"]` is refused for remote projects.**
   `validateProjectShape` rejects the wildcard outright. On a local project
   the wildcard is a convenience meaning "every MCP relay currently knows
   about." On a remote grant it would mean that registering a new MCP on the
   host silently widens what another machine can reach — with no action
   taken against the project itself and no diff to review. A remote grant
   must be an enumeration someone typed by hand; an empty list is fine
   (enroll now, widen deliberately later is the expected resting state).

5. **Sessions are refused on remote projects, and this could not live in the
   model allowlist.** `validateProjectShape` requires a remote project's
   `AllowedModels` to be empty. But `modelAllowedForProject`
   (`frontend_model_guard.go`) reads an *empty* allowlist as "unrestricted" —
   so, left to the allowlist alone, the most restrictive configuration
   (no path, no filesystem access, no models listed) would have produced the
   most permissive outcome (every model permitted). `refuseRemoteSession`
   is therefore a separate check in `newSessionModelGuard`, run before the
   allowlist check, that rejects any session-create request naming a remote
   project outright. `frontend_model_guard_test.go` carries the paired test:
   `TestModelAllowedForProject_WouldPermitRemoteProject` asserts the
   allowlist alone *would* permit it, so if that assertion ever starts
   failing, `refuseRemoteSession` may have become redundant and the pair
   should be revisited — the test exists to keep the guard from silently
   rotting into dead code.

6. **PTY launches are refused, and this closed a live hole.**
   `dirWithinProject("", "")` (`router.go`) returns `true`: the empty-dir
   branch ("no directory to validate") is checked before the
   empty-project-path branch and short-circuits it. Without an explicit
   guard, `ResolvePtyEnv` would succeed for a pathless remote project and
   return its plaintext token with `WorkingDir: ""` — and Go's `exec.Cmd`
   treats an empty `Dir` as the *parent process's* working directory, so a
   host shell would come up holding a remote project's credential, rooted
   wherever relay happens to be running. `refuseRemotePty` closes this at
   both of `ResolvePtyEnv`'s resolution branches: the authoritative
   `ProjectID` path and the legacy name/directory-match path
   (`TestResolvePtyEnv_RefusesRemoteProject` and
   `TestResolvePtyEnv_RefusesRemoteProjectViaLegacyPath`). The same
   reasoning applies to `ResolveProjectTemplate`, which refuses a remote
   project explicitly rather than relying on `ShellTemplates` being empty —
   a directly-constructed `Project` (a migration, a hand-edited
   `settings.json`) could still carry templates from a former life as a
   local project (`TestResolveProjectTemplate_RefusesRemoteProject`).

## Consequences

- **Nothing can reach a remote project yet.** This PR makes the project
  model coherent for a remote client; it does not add the listener, client
  certificates, or the enrollment flow that would let one actually connect.
  Those are 2b.
- **Converting a local project to remote is possible but constrained.** It
  is refused while the project still holds a filesystem-scoped grant,
  because the stale `allowed_dirs` context on that project would otherwise
  be inherited by the converted remote project — exactly the scope leak
  `ValidateProjectGrants` exists to prevent on creation. Dropping the
  filesystem-scoped MCP grant in the same update request makes the
  conversion legal. See
  `TestProjectConvertLocalToRemote_CannotInheritFilesystemScope`
  (`project_convert_test.go`).
- **Per-tool tri-state permissions (`allow`/`prompt`/`deny`) are deliberately
  out of scope here**, deferred to their own PR rather than folded into this
  one. The `disabled_tools` ↔ `tool_policy` dual-write that a tri-state model
  implies is the riskiest part of that design and deserves review on its
  own, separate from the remote-project shape.

## See also

- ADR-007 — the project-token brokering model this builds on
  (`docs/decisions/007-project-token-brokering.md`)
- `docs/tokens.md` — directory auth (`allow_cwd_auth`) and why remote
  projects can't enable it
- `project.go` — `validateProjectShape`, `CreateProjectWithTokenKind`
- `settings.go` — `ValidateProjectGrants`, `SyncProjectToken`,
  `UpdateProjectKind`
- `router.go` — `refuseRemotePty`, `dirWithinProject`,
  `ResolveProjectTemplate`
- `frontend_model_guard.go` — `refuseRemoteSession`,
  `modelAllowedForProject`
