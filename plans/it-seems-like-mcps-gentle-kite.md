# Relay Skills Bridge — universal PTY-driven design

## Context

The user asked whether MCPs are being replaced by Skills, and whether relay should expose its MCP infrastructure as a Skills bridge — `SKILL.md` files that tell an external agent how to invoke relay's tools via a CLI, secured by an env-var token. Target both **Pi.Dev** ([pi.dev](https://pi.dev) / `@mariozechner/pi-coding-agent`) and **Claude Code**, treated symmetrically via universal PTY-template fields rather than agent-specific code paths. Both adopt the [agentskills.io](https://agentskills.io) standard so the same SKILL.md works in both.

**The framing question.** Skills are *not* replacing MCPs — they're complementary. MCP is connectivity (live JSON-RPC server, ~55k tokens for a 5-server setup). Skills are procedural knowledge (lazy-loaded markdown, ~30-100 tokens until invoked). Pi.Dev and Claude Code both adopt the agentskills.io standard — the *same* `SKILL.md` runs in both.

A "skill that calls a CLI" is a particularly good fit for relay because:

1. **The bones already exist.** `relay mcpExec` (`exec_cmd.go`) is a working one-shot CLI invocation that reuses the same bridge socket + auth as the stdio MCP server. `RELAY_TOKEN` env-var fallback already works.
2. **Per-project token scoping is already enforced server-side.** `router.go`'s `AuthenticateProject` → permission derivation → tool filtering runs regardless of which transport (stdio MCP, mcpExec, future `relay mcp call`) invoked it. The skill bridge adds zero new trust surface.
3. **PTY infrastructure exists in relayLLM.** A `pty:` section in relayLLM's `config.json` defines templates (`claude-code`, `opencode`, `shell`). Eve fetches them via WebSocket `terminal_templates`. Per `plans/lets-work-on-this-elegant-spindle.md`, the source of truth is relayLLM's config, not eve. We can register a new `pi` template the same way.

## Recommended approach: universal PTY template fields

Generalize the mechanism. Any PTY template (Pi.Dev, Claude Code, anything that consumes agentskills.io skills) opts into the same three knobs in relayLLM's config. The relay-specific parts (token, project lookup) live behind one bridge call.

### Universal PTY template fields (new in relayLLM)

| Field | Type | Purpose |
|---|---|---|
| `useRelayToken` | bool | Inject `RELAY_TOKEN` env from the launching project's token at spawn time |
| `autoRegenSkills` | `"always"` \| `"skipIfExists"` \| `"never"` | When to regenerate the skill file before spawn. Default `"never"` |
| `skillPath` | string template | Where to write the skill, relative or absolute. Supports `${project.path}` substitution |
| `args` | string[] | May reference `${SKILL_PATH}`, `${RELAY_TOKEN}`, `${PROJECT_PATH}` |

### How each agent is configured

```json
"pidev": {
  "command": "/Users/jonathan/.bun/bin/pi",
  "icon": "terminal",
  "useRelayToken": true,
  "autoRegenSkills": "always",
  "skillPath": "${project.path}/.claude/skills/relay",
  "args": ["--skill", "${SKILL_PATH}"]
},
"claude-code": {
  "command": "claude",
  "icon": "claude-code",
  "useRelayToken": true,
  "autoRegenSkills": "always",
  "skillPath": "${project.path}/.claude/skills/relay"
}
```

Same path, same regen mechanism. The agent-specific knowledge (Pi needs `--skill <dir>`, Claude auto-discovers `.claude/skills/<name>/`) lives entirely in the template's `args` field — no agent-specific Go code in relay or relayLLM.

### Why `.claude/skills/relay/` is the universal path

- **Claude Code** auto-discovers `<project>/.claude/skills/<name>/SKILL.md`. Required path.
- **Pi.Dev** accepts `--skill <path>` (repeatable) and doesn't care what the parent dir is named — it loads the skill at the path it's told.
- One file, one regen, both agents. Slight semantic oddity (Pi loading from a `.claude/` dir) is purely cosmetic.

### What `autoRegenSkills` buys us

- **`always`** (Pi + Claude defaults): regen on every PTY launch. Security-trimmed to the project's *current* tool permissions. Revoke an MCP from a project → next launch's skill no longer mentions its tools. No manual sync.
- **`skipIfExists`**: regen only if the file is missing. Useful for stable, committable skills where the user prefers a frozen snapshot.
- **`never`** (default for unrelated templates): no skill management. The template behaves like any other PTY launch.

### Skill content

Frontmatter (agentskills.io standard, both agents accept):
```yaml
---
description: <project-name> tools — <one-liner of what the project does>
allowed-tools: Bash(relay mcp call *)
---
```

Body documents the tools resolved via `bridge.Client.ListTools()` with the project's token, plus the `relay mcp call --tool NAME --args '{...}'` invocation template. Tokens are NEVER written into the file — body documents: "`RELAY_TOKEN` is in the environment."

`<project.path>/.claude/skills/relay/` is relay-owned. Recommend `.gitignore` for `relay/` (one line), keeps the rest of `.claude/skills/` user-controlled and committable.

### Spawn flow (universal, applies to any template with `useRelayToken` or `autoRegenSkills`)

**Design principle:** Relay owns the project token. Tokens are NEVER baked into relayLLM's `config.json`. The template just declares intent (`useRelayToken: true`, `autoRegenSkills: "always"`); resolution happens via one bridge call at spawn time.

1. User clicks a PTY card in eve's shell launcher in the context of a project. Eve sends `{template_id, project}` to relayLLM (eve's launch payload already carries project context — the working directory is correct today, which is the proof point. Verification step added below.)
2. relayLLM looks up the template. If `useRelayToken: true` OR `autoRegenSkills != "never"`, call relay's bridge.
3. relayLLM calls relay's bridge socket: `{type: "resolve_pty_env", project, regen_skills: "<value>", skill_path: "<rendered template>"}` (NEW bridge request type — Implementation §9).
4. Relay's router:
   - Resolves project plaintext token from in-memory project store.
   - Computes `working_dir = Project.Path`.
   - If `regen_skills == "always"`, or if `regen_skills == "skipIfExists"` and the file is missing: emit fresh `SKILL.md` to `skill_path`, security-trimmed to the project's current tool set.
   - Returns `{RELAY_TOKEN, working_dir, skill_path}`.
5. relayLLM substitutes `${SKILL_PATH}`, `${RELAY_TOKEN}`, `${PROJECT_PATH}` into the template's `args`. Sets env (`RELAY_TOKEN` if `useRelayToken: true`, plus `env_passthrough` like `ANTHROPIC_API_KEY` from the user's shell). Sets `cmd.Dir = working_dir`.
6. relayLLM spawns the command. Pi sees `--skill <path>`; Claude auto-discovers from `<project.path>/.claude/skills/relay/`.
7. Any `relay mcp call` the agent runs hits the bridge with the correct project token → permissions enforced by `router.go`.

If relay is down, the project is gone, or skill regen fails, step 4 errors and relayLLM surfaces a clear message in the terminal pane instead of spawning with a missing token. Fail-closed.

### Out-of-band regen (safety net for non-PTY consumers)

The per-PTY-launch regen covers the headline path (user clicks a card → fresh skill). For agents launched outside relay's PTY system (e.g., a Claude Code session opened manually in a terminal), add a fallback:

- On project save (`SyncProjectToken` path): regen SKILL.md if `GenerateSkill: true` on the project.
- On MCP reconcile (`ReqReconcileExternalMcps`): regen SKILL.md for every project whose `allowed_mcp_ids` intersects the reconciled set.
- On project delete: remove the project's `.claude/skills/relay/` directory.

This is belt + suspenders. The PTY launch path is the canonical regen trigger; this fallback keeps the file fresh for hand-launched agents.

## Security model (deep)

This is the section the user specifically asked to deepen. The skill bridge does **not** introduce new auth — it reuses the project token. But the surface area expands (more apps, more env-var handoffs), so the model deserves spelling out.

### The auth boundary is the project token

- **What it is.** A plaintext UUID-style string stored only in memory (`Project.Token`) + SHA-256 hashed copy (`Project.TokenHash`) persisted to `settings.json`. See `tokens.go:hashToken()`.
- **What it grants.** Exactly the tools allowed by that project's `allowed_mcp_ids`, with `disabled_tools` removed and `_meta.allowed_dirs` set to project path. Permissions are *derived at every call* (`router.go:AuthenticateProject`) — never cached, never trusted from the client.
- **What it does NOT grant.** Cross-project access. Admin operations. Service-level operations (those use ephemeral service tokens, separate code path).
- **Rotation.** Tokens can be rotated via existing settings UI / `SyncProjectToken`. Active PTY/MCP processes keep using the old token until they restart and pick up the new env var. No invalidation broadcast — by design, simple and predictable.

### How the token reaches each consumer

| Consumer | How token gets there | Trust assumption |
|---|---|---|
| stdio MCP (`relay mcp --token X`) | Flag passed by Claude Code's MCP config or eve's mcpManager | The launcher (eve/Claude) holds the token; it came from relay's settings UI or HTTP API |
| `relay mcp call ...` from a PTY-launched agent | `RELAY_TOKEN` env var injected by **relay** at PTY spawn time (via `ReqResolvePtyEnv`) | Token never leaves relay → relayLLM PTY env → child process tree. Not in any file. |
| `relay mcp call ...` from a hand-launched script | User exports `RELAY_TOKEN` from shell, ideally via `op read` (1Password) or `security find-generic-password` (keychain) | Token lives in the user's secret store, not in dotfiles. SKILL.md explicitly warns against literal tokens in `.envrc` etc. |
| Future: HTTP/RPC from an arbitrary app | Out of scope for v1. Would need an "issue scoped token" flow with refresh/revoke. | — |

### Why env-var-only (no flag, no prompt)

- **Not a flag.** `relay mcp call --token X` would put the literal token in `argv`, which lands in shell history, `ps` output, audit logs, and Claude Code's tool-call transcript. We *only* accept `RELAY_TOKEN` env. The CLI errors with a clear message if env is unset — no silent fallback.
- **Not a prompt.** A skill can't ask the user to type the token mid-session. Aside from UX cost, it would normalize copy-pasting tokens into prompts, which then end up in LLM transcripts and provider logs.
- **Env-var caveats acknowledged.** Env vars are visible in `/proc/<pid>/environ` to the same UID. That's an acceptable trust boundary for a single-user macOS workstation — the same UID can already read `settings.json` directly.

### Skill-side scoping

Generated SKILL.md uses:

```yaml
allowed-tools: Bash(relay mcp call *)
```

This pre-approves *only* the `relay mcp call` invocation pattern — not arbitrary `Bash`. Per the Skills docs ("Pre-approve tools for a skill"):

- For project-local skills (`.claude/skills/`, `.pi/skills/`), this requires explicit workspace trust acceptance — first launch into the project prompts the user to trust the workspace.
- Skills live at `<project.path>/.claude/skills/relay/`, so trust is scoped to the user's actual project directory — a familiar boundary.

### Token leakage threat model

| Threat | Mitigation |
|---|---|
| Token in committed dotfile | SKILL.md never contains the token; doc warns users not to put it in `.envrc` |
| Token in shell history | CLI rejects `--token` flag; env-var only |
| Token in tool-call transcript | Same — never in argv |
| Token in `ps` for another user | Single-user workstation assumption; multi-user is out of scope |
| Token in core dump / crash log | Out of scope; same risk as any secret-in-env program |
| Stolen `settings.json` | Already a full-compromise scenario today; skill bridge doesn't change this |
| Malicious third-party skill | Skills run in Claude/Pi's normal permission sandbox. `allowed-tools` is the *only* extra grant we add. User reviews on first workspace trust prompt. |

### Pi.Dev extension trust note

Pi's extension model lets TypeScript modules run arbitrary code in the host (`pi/packages/coding-agent/docs/extensions.md`). The Snyk ToxicSkills research (Feb 2026) found 13.4% of public skills had critical vulnerabilities. We:

- Ship **skills, not extensions** (markdown only, no executable code in the bundle).
- If we ever want an "extension" path (e.g. a Pi extension that exposes relay as native tools instead of via CLI), that's a separate proposal with its own threat model.

## Implementation

### File-by-file changes

**1. `main.go` + `mcp_cmd.go`** — add `relay mcp call` as an alias for the existing `relay mcpExec` dispatch. Keep `mcpExec` for back-compat. Add explicit "RELAY_TOKEN not set" error path.

**2. `exec_cmd.go`** — add `--schema` flag that dumps tool input schemas as JSON alongside `--list`. Required for skill generation. Reuses `bridge.Client.ListTools()`.

**3. New file: `skills.go`** — internal package (NOT a CLI subcommand — the regen happens inside `ReqResolvePtyEnv`). Exposes:
- `EmitSkill(project Project, path string) error` — renders SKILL.md content from the project's allowed-tool set (via `bridge.Client.ListTools()` using the project's plaintext token) and writes it to `path/SKILL.md`. Creates parent dirs as needed.
- `RemoveSkill(project Project, path string) error` — for project delete cleanup.
- Token resolution: read project plaintext from in-memory store; never written to file.
- Optional thin CLI wrapper `relay skills regen --project NAME --path PATH` for manual testing only.

**4. `types.go`** — add to `Project`:
- `GenerateSkill bool` — toggle in Settings UI. Controls whether the out-of-band regen hooks fire for this project. The PTY-launch regen path triggers regardless (it's per-template, not per-project).

**5. `settings.go`** — wire out-of-band regen in `SyncProjectToken` and on project delete. Reuses existing project store accessors. Calls `skills.EmitSkill` / `skills.RemoveSkill`.

**6. `external_mcp.go` / `router.go`** — in `ReqReconcileExternalMcps` handler, after reconcile, walk projects with `GenerateSkill: true` and regen those whose tool set changed.

**7. New: relayLLM PTY template registration (one-shot, two templates)** — relay registers `pidev` and `claude-code` templates with relayLLM at first run (`POST /api/terminal/templates`), each with the universal fields (`useRelayToken: true`, `autoRegenSkills: "always"`, `skillPath: "${project.path}/.claude/skills/relay"`, args). Idempotent — re-registers on relay startup if missing. No per-project templates.

**8. `settings_html.go`** — per-project "Generate skills for Pi.Dev / Claude Code" checkboxes in the project edit form.

**9. `bridge/types.go` + `router.go`** — add a new bridge request type `ReqResolvePtyEnv`:
- Request: `{type: "resolve_pty_env", project, regen_skills, skill_path}`. `regen_skills` is `"always" | "skipIfExists" | "never"`. `skill_path` is the template-rendered absolute path (relayLLM does the `${project.path}` substitution before sending).
- Response: `{RELAY_TOKEN, working_dir, skill_path}` or error.
- Auth: requires a service-token caller (relayLLM is a registered service, has one). Reject project-token callers.
- Side effect: if `regen_skills` requires it, emit fresh SKILL.md to `skill_path` (creating parent dirs as needed), security-trimmed to the project's current tool set. Regen is part of the bridge call so it can't be skipped by a misbehaving spawn.
- Implemented in `router.go` alongside existing `ReqListProjects`/`ReqGetProject` handlers.

**10. Cross-repo (relayLLM)** — extend relayLLM's PTY template schema with the universal fields:
- `useRelayToken: bool`
- `autoRegenSkills: "always" | "skipIfExists" | "never"` (default `"never"`)
- `skillPath: string` (with `${project.path}` substitution)
- `args` with `${SKILL_PATH}` / `${RELAY_TOKEN}` / `${PROJECT_PATH}` substitution
- `env_passthrough: []string`

In the spawn path, if `useRelayToken` or `autoRegenSkills != "never"`, render `skillPath`, call `bridge.ResolvePtyEnv(project, regen_skills, skill_path)` before `cmd.Start()`, substitute returned values into args/env/cwd. If the call fails, surface the error and abort the spawn. This change lives in relayLLM, not relay — out of scope for the relay PR but needs a paired PR.

**11. Cross-repo (eve, verification only)** — confirm eve's shell-launch payload includes a project identifier when launching a terminal from a project-scoped context. The user asserts it does (working directory is correct today). If not, eve also needs a small change to include `project` in the launch message; this is a third paired PR.

### Key files to reuse

| Purpose | File | Function |
|---|---|---|
| Tool listing + schemas | `bridge/client.go` | `Client.ListTools()` |
| Tool invocation | `bridge/client.go` | `Client.CallTool(name, args)` |
| Token-aware client | `bridge/client.go` | `NewClient(token)` |
| Existing one-shot exec | `exec_cmd.go` | `runMcpExec()` |
| Project store + sync | `settings.go` | existing accessors + `SyncProjectToken` |
| MCP reconcile hook | `router.go` | `ReqReconcileExternalMcps` handler |
| Env merge for spawn | `helpers.go` | `mergeEnv()` |

## Verification

1. **Build**: `./build.sh`.
2. **CLI smoke test**:
   ```bash
   RELAY_TOKEN=<project-token> relay mcp call --list
   RELAY_TOKEN=<project-token> relay mcp call --list --schema
   RELAY_TOKEN=<project-token> relay mcp call --tool <name> --args '{}'
   unset RELAY_TOKEN && relay mcp call --list   # expect clear "RELAY_TOKEN not set" error
   ```
3. **Skill emission via bridge call (manual)**:
   ```bash
   # Use the optional CLI wrapper or hit the bridge directly
   relay skills regen --project <name> --path ~/Documents/EVE/TBO/.claude/skills/relay
   ls ~/Documents/EVE/TBO/.claude/skills/relay/SKILL.md
   grep '<token-plaintext>' ~/Documents/EVE/TBO/.claude/skills/relay/SKILL.md   # must return zero hits
   ```
4. **Pi.Dev end-to-end** (the headline test):
   - Install Pi: `npm install -g @mariozechner/pi-coding-agent` + set `ANTHROPIC_API_KEY` in keychain/shell.
   - Open eve in a project context (e.g. `TBO` with `Path: ~/Documents/EVE/TBO`), hit shell launcher → expect `Pi.dev` card → click.
   - Verify `~/Documents/EVE/TBO/.claude/skills/relay/SKILL.md` was just written (mtime within seconds of click).
   - In Pi: type `/skills` → expect `relay` listed. Ask "what tools does relay expose?" → confirm Pi invokes `relay mcp call --list`. Ask a question that should trigger a specific tool → confirm correct invocation.
   - **Security trim test**: in relay settings, remove an MCP from the project. Close Pi. Relaunch. Confirm the removed tool is no longer in `.claude/skills/relay/SKILL.md` and Pi can no longer invoke it (router rejects on auth).
5. **Claude Code end-to-end**: same flow with the `claude-code` template. Confirm Claude finds the same SKILL.md via its auto-discovery path. Confirm parity with Pi for tool invocation.
6. **`autoRegenSkills: "skipIfExists"` mode**: change one template to this mode, hand-edit the generated SKILL.md, relaunch — confirm the file is NOT overwritten.
7. **Token isolation**: launch two PTYs (Pi + Claude, or two Pis) for two different projects. In each, run `relay mcp call --list`. Confirm each sees only its own project's tools (proves token-per-PTY isolation).
8. **Workspace trust**: first launch into a project where `.claude/skills/relay/` is new should prompt for trust before `allowed-tools` pre-approval takes effect.

## Resolved design decisions

- **Universal PTY template fields** (`useRelayToken`, `autoRegenSkills`, `skillPath`) — same mechanism for Pi.Dev, Claude Code, and any future agentskills.io consumer. No agent-specific code paths in relay or relayLLM.
- **Single skill path per project**: `<project.path>/.claude/skills/relay/` (a directory containing SKILL.md). Claude auto-discovers; Pi gets `--skill <that-dir>` substituted into args.
- **`autoRegenSkills: "always"`** for both Pi and Claude in v1. Security-trimming on every launch — revoke an MCP, next launch's skill drops the tools. `"skipIfExists"` available for users who want stable committable skills. Default `"never"` for unrelated templates.
- **Tokens never in `config.json`.** Resolved dynamically at spawn via `ReqResolvePtyEnv` bridge call. Token rotation takes effect on next spawn.
- **Recommended `.gitignore`**: `.claude/skills/relay/` (one line) — relay-owned subdir, regenerates. Rest of `.claude/skills/` stays user-controlled and committable.
- **Pi's `--skill` flag** takes a path (per Pi README "Load skill (repeatable)"). Treated as a directory containing SKILL.md (agentskills.io standard). Exact semantics — file vs dir — verified during implementation; either form works since we control the path layout.

## Out of scope (deliberately deferred)

- Pi Extensions (TypeScript code). Skills-only for v1; extensions are a separate threat-model conversation.
- HTTP/RPC token issuance for arbitrary external apps. Today: PTY-launched + manual shell export only.
- Per-tool skills. Would balloon context cost; not worth it.
- Plugin packaging for Claude Code. Skills + `allowed-tools` is enough.
- Token rotation broadcast / forced PTY restart. Old-token-until-restart is the contract.
