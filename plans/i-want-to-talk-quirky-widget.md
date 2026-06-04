# Skill generation overhaul + image-gen consolidation onto MCP

## Context

Two connected problems surfaced from one symptom: *"I ask Claude to generate an image and it
says it doesn't know how until I explicitly name `generate_image`."*

1. **Skills don't route.** Agentskills.io skills (Claude Code + Pi) are **lazy-loaded** — the agent
   only sees a skill's frontmatter `description` when deciding whether to activate it; the body
   (the tool list) loads *after* activation. Relay's generated `SKILL.md` uses a generic,
   capability-free description (`skills.go:128`: *"<project> project tools — invoke via the relay
   CLI. Use when the user asks to do something that matches one of the tools listed below"*). It
   names zero concrete capabilities, so a request like "generate an image" never matches and the
   skill never loads. The good keywords (the relayComfy MCP tool's own description literally says
   *"Use whenever the user asks for an image, illustration, picture, logo, or visual asset"* —
   `relayComfy/mcp/main.go:74`) are thrown away at the frontmatter level. **The description is the
   routing signal, and relay isn't generating one.**

2. **Image generation is forked three ways.** ComfyUI logic is duplicated across two repos feeding
   three consumer paths:
   - relayLLM builtin `generate_image` (`builtin_tools.go` + `comfyui_client.go`) — Ollama/OpenAI/
     llama.cpp, in-process, streams text progress.
   - relayComfy MCP `generate_image` (`relayComfy/mcp/`) — Claude + any MCP consumer via `relay mcp`;
     a literal *copy* of relayLLM's client (`relayComfy/mcp/client.go:18-22` says so); no progress.
   - relayLLM HTTP `/api/generate-image` + `pi_image_skill.go` — Pi via `curl`; no progress.

   This violates relay's own charter ("Relay is the container… specific service knowledge lives in
   services, not in relay") — ComfyUI knowledge has a proper home (relayComfy) and shouldn't also
   live inside the LLM engine.

**Intended outcome.** Make MCP/skill the single common-denominator interface for tools across
Claude and Pi: (1) relay generates per-category, capability-rich skills so requests route without
naming tools; (2) a **generic** MCP `notifications/progress` framework that any MCP can use to
report progress (relayComfy is the first consumer); (3) retire relayLLM's in-process ComfyUI
binding so every provider reaches image-gen through the one relayComfy MCP.

Decisions already made with the user: **full consolidation**, skill layout **per-category** with
`relay-<category>` naming, and progress via **a general MCP-progress framework** (not a ComfyUI
one-off).

---

## Phase 1 — Skill generation overhaul (relay only)

Standalone value: this alone fixes the routing symptom on the MCP path already in use. Lowest risk,
fully reversible. Do it first.

### 1a. New seam: bucketed tool listing
`router.go:141` `ListTools` flattens to `[]mcp.Tool` and **loses the owning MCP** — that wire
contract is consumed by `mcp/server.go` + `exec_cmd.go` and **must not change**. Add a parallel,
skill-only method instead.

- **`skills.go`** — extend the `SkillLister` interface (`skills.go:20`):
  ```go
  type SkillBucket struct {
      Key   string     // bucket key: server category, else owning MCP DisplayName
      Slug  string     // "mail", "comfyui" — filesystem/skill-safe
      Tools []mcp.Tool
  }
  type SkillLister interface {
      ListTools(ctx context.Context, token string) (json.RawMessage, error) // unchanged
      ListSkillBuckets(ctx context.Context, token string) ([]SkillBucket, error) // NEW
  }
  ```
- **`router.go`** — implement `(*appRouter).ListSkillBuckets` right after `ListTools`. Reuse the
  exact `resolveAuth` + per-MCP/per-tool `checkToolAccess` filter loop (so membership matches
  `ListTools`), but group into `map[key][]mcp.Tool`.
  **Bucket key = server-supplied `t.Category` if non-empty, else `ext.DisplayName`.** Do **not** use
  `toolCategory`'s name-prefix fallback for the bucket key — it produces noise like "Generate" from
  `generate_image`; uncategorized tools belong under their MCP (so relayComfy's tool → `relay-comfyui`,
  routed by its rich description, not its bucket name). macMCP supplies real categories, so its tools
  split into `relay-mail`, `relay-calendar`, etc.

### 1b. Per-bucket rendering + capability-rich descriptions
- **`skills.go`** — replace `renderSkillMd(proj, tools)` with `renderBucketSkillMd(proj, bucket)`:
  - Frontmatter: `name: relay-<slug>`, the synthesized `description` (below), and the existing
    `allowed-tools: Bash(<relayBin> mcp call *)`. Reuse `resolveRelayBin`, `yamlEscape`, `oneLine`.
  - Body: keep the generated-by banner, relay binary path, `RELAY_TOKEN` note, the Invocation block,
    and the bucket's tools (name + one-line description).
- **`synthesizeDescription(bucketKey string, tools []mcp.Tool) string`** (new, deterministic — no LLM,
  must be fast at PTY launch):
  1. Headline = bucketKey.
  2. For each tool (sorted by name): derive a short phrase. **Prefer the first clause of the tool's own
     `Description`** (cut at first `.`/`;`/newline via `oneLine`) when present and short (≤~60 chars) —
     these are often already trigger-rich (relayComfy's is). Else fall back to the name with `_`→spaces,
     dropping a leading token that duplicates the category (`mail_send` in Mail → "send").
  3. Dedup case-insensitively, cap ~6–8 phrases, append "and more" if truncated.
  4. Assemble: `"<Headline> tools via relay: <p1>, <p2>, …. Use when the user asks to <p1>, <p2>, or …."`
  5. `yamlEscape` + `oneLine`, hard-cap ~500 chars.
  - **Worked example (relayComfy `generate_image`):** `description: Comfyui tools via relay: generate an
    image from a text description; image, illustration, picture, logo, visual asset. Use when the user
    asks to generate an image, create a picture, or produce an illustration.` → "generate an image" now
    matches at routing time.
- **`skillSlug(key string) string`** (new): lowercase, non-`[a-z0-9]`→`-`, trim/collapse `-`, fallback
  `"tools"`, cap ~40. Merge tools whose keys slug-collide into one bucket.

### 1c. Set-based reconcile ("elegant find-and-replace")
The `relay-` prefix is the ownership marker.

- **`skills.go`** — `EmitSkill` → `EmitSkills(ctx, lister, proj, skillsRoot, mode) ([]string, error)`
  where `skillsRoot` is the **parent** `.claude/skills` dir:
  1. `RegenNever` → no writes, no prunes, return early.
  2. `ListSkillBuckets`; for each non-empty bucket render to `relay-<slug>/SKILL.md`.
  3. Idempotent write (byte-compare before write — preserves the fsnotify-quiet behavior at
     `skills.go:82`).
  4. **Prune**: remove any on-disk dir matching `isRelayManagedSkillDir(name)` (`name == "relay" ||
     strings.HasPrefix(name, "relay-")`) that isn't in the desired set. This auto-migrates the legacy
     single `relay/` dir away on first run. Empty tool list → all relay-managed dirs pruned.
  5. `RegenSkipIfExists` → if any relay-managed dir already exists, do nothing (whole-namespace skip).
- **`skills.go`** `RemoveSkill(skillsRoot)` — generalize from "base must == relay" to "remove every
  `isRelayManagedSkillDir` child"; never `RemoveAll` the root; still refuse non-relay entries.
- **`project_routes.go:29`** `projectSkillDir` → return `…/.claude/skills` (drop trailing `relay`).
- **Callers** switch to `EmitSkills`: `router.go:210` `regenProjectSkills`, `router.go:285`
  `ResolvePtyEnv`, `ipc_projects.go:268` `ipcRegenProjectSkill`, `project_routes.go` `reconcileProjectSkill`
  + the delete path. The IPC/HTTP `regen` responses return the skills-root path (keep field name `path`).
- **`ResolvePtyEnv` back-compat guard** (`router.go:285`): `PtyEnvRequest.SkillPath` is templated by an
  external relayLLM PTY template that still points at `…/skills/relay`. In `ResolvePtyEnv`, if
  `filepath.Base(SkillPath)` is `relay` or `relay-*`, use `filepath.Dir(SkillPath)` as the root. Makes
  relay tolerant of both the old and a future simplified template; update the `SkillPath` doc comment
  (`bridge/types.go:74`).

### 1d. Tests (`skills_test.go`, hermetic per ADR-001)
Rewrite the `TestRenderSkillMd_*`/`TestEmitSkill_*` set for buckets; extend the test `stubLister` with
`ListSkillBuckets`. New: per-bucket files written; description contains capability keywords (assert
`relay-comfyui` description contains "image"); stale `relay-*` pruned; legacy `relay` migrated; empty
tools prunes everything; idempotent re-emit; **no token leakage across all files** (walk the root);
`RegenNever`/`RegenSkipIfExists`; `skillSlug` + `synthesizeDescription` table tests;
`TestAppRouter_ListSkillBuckets` (via `newTestRouter(t)`) asserting category-else-DisplayName bucketing
and that `checkToolAccess` filtering is honored.

---

## Phase 2 — Generic MCP progress framework

Goal: any external MCP can report progress via the standard MCP `notifications/progress`; it surfaces
in relayLLM as a `ToolProgress` event identical to today's builtin progress. Tool-agnostic — ComfyUI is
just the first emitter. The chain is request/response-only today; progress must travel the reverse path:
`relayComfy → relay external client → router → bridge → relay mcp stdio → relayLLM go-sdk client → WS`.

**Correlation model:** the bridge opens a **fresh connection per call** (`bridge/client.go`), so the
connection *is* the correlation unit on the bridge hop — no token needed there. The two JSON-RPC hops
each manage their own `progressToken`: relayLLM↔`relay mcp` (token T1), and relay↔relayComfy (token T2).
The in-process callback chain bridges them.

### Per-hop changes

| Hop | File:area | Change |
|---|---|---|
| relayComfy (go-sdk **server**) | `relayComfy/mcp/main.go:76`, `client.go:166` | Read `req.Params.GetProgressToken()`; thread `progressToken any` + `*mcp.ServerSession` into `generate`→`pollHistory`; in the poll loop call `session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: tok, Message: "Generating… (Ns)", Progress: n})`. go-sdk v1.5.0 supports this directly; `relayComfy/mcp/go.mod` already requires the SDK. Standalone build preserved. |
| relay external client (**hand-rolled**) | `external_mcp.go:428` `CallTool`, `:537` `readLoop` | Add `progressCallback func(json.RawMessage) error` param. On send, generate T2 and inject `_meta.progressToken` (alongside existing `_meta` at `:451`); register callback in a `map[token]callback`. In `readLoop`, replace the `if resp.ID == nil { continue }` drop (`:560`) with: parse `notifications/progress`, look up the token, invoke the callback. |
| router | `router.go:166` `CallTool` | Add `progressCallback func(json.RawMessage) error`; pass through to `r.tools.CallTool` (`:189`). |
| bridge protocol | `bridge/types.go` | Add `RespProgress = "Progress"` constant + `Progress json.RawMessage` field on `BridgeResponse`. |
| bridge server | `bridge/server.go:94` `handleConn`, `:176` `handleCallTool` | Give `handleCallTool` a `progressWriter func(BridgeResponse) error`; build a `progressCallback` that writes `{Type:"Progress", Progress:…}` frames on the connection before the final `Result`. Guard the connection writer with a mutex; the final Result/Error stays terminal. |
| bridge client | `bridge/client.go` `CallTool` | Change from "write request → read one response" to "write request → read frames until terminal Result/Error", invoking an optional `onProgress` callback per `Progress` frame. |
| relay mcp stdio server (**hand-rolled**) | `mcp/server.go:14` loop, `:129` `handleToolsCall` | Read inbound T1 from the tools/call `_meta.progressToken`; call the bridge with an `onProgress` that writes a JSON-RPC **notification** (`method:"notifications/progress"`, `params.progressToken=T1`) to stdout, then write the final response. **Handle each request in a goroutine with a mutex-guarded stdout encoder** so a long image-gen doesn't block other tool calls on the same session (pre-existing serialization limit; fix it here). |
| relayLLM (go-sdk **client**) | `mcp_client.go:33-42` iface, `:155` `CallTool`, `:72` client init; `provider_chat_base.go:463` | Set `ClientOptions.ProgressNotificationHandler` at client creation (`:72`). Change `MCPClient.CallTool` to take `toolUseID` + send `CallToolParams.Meta{"progressToken": T1}`; maintain `map[T1]→(toolUseID,toolName)` so the global handler can route to `emitter.ToolProgress(toolUseID, toolName, msg)`. Update the dispatch at `provider_chat_base.go:463-464` to pass `tc.ID` (the MCP branch currently passes no emitter, unlike the builtin branch at `:462`). |

No change needed to `events.go` / `ws.go` — once an MCP progress becomes a `ToolProgress` event it
renders exactly like a builtin's progress today. Claude-CLI progress stays out of scope (Claude manages
its own MCP via `--mcp-config`).

### Tests
- relayComfy: handler emits ≥1 progress when a token is supplied, none when absent.
- relay: bridge `Progress` frames round-trip; `readLoop` routes `notifications/progress` to the callback;
  `relay mcp` stdio server emits a `notifications/progress` then the response; concurrent calls on one
  session interleave without corrupting stdout.
- relayLLM: a stubbed MCP server emitting progress drives `ToolProgress` with the right `toolUseID`.

---

## Phase 3 — Retire relayLLM's in-process ComfyUI binding

Now that the MCP path has progress parity, route **every** provider through the relayComfy MCP.

**Delete (relayLLM):** `comfyui_client.go` (whole file); the image-gen functions in `builtin_tools.go`
(`RegisterImageGenTool`, `buildImageGenSchema`, `handleGenerateImage`, helpers — keep the
`BuiltinToolRegistry` scaffolding only if another builtin exists; today `generate_image` is the only one,
so the registry may be removed entirely and `builtinTools` wiring dropped); `pi_image_skill.go` (whole
file); the `/api/generate-image` route (`api.go:456-477`); the ComfyUI init/registration block in
`main.go:200-241`.

**Keep (relayLLM):** `GET /api/generated/{filename}` static serving (`api.go:438-451`) and the
`{dataDir}/generated/` dir — relayComfy writes images there; Eve displays via this URL.

**Rewire (relayLLM):** drop `builtinTools` from the three `NewBaseChatProvider` calls (`session.go:362/374/389`)
and the field/setter; remove image-gen fields from the pi overlay (`pi_overlay.go`). Ollama/OpenAI/llama.cpp
reach `generate_image` through the relayComfy MCP via the existing `relay mcp` subprocess merge
(`provider_chat_base.go:301-313`) when `useRelayTools` + a project token are set.

**Pi:** image-gen becomes the generic relay path — the Phase-1 `relay-comfyui` SKILL.md teaches
`relay mcp call --tool generate_image`, replacing the `comfyui-image-gen` pi skill. Pi launched via
relay's PTY already gets `RELAY_TOKEN`; confirm the pi overlay includes the relay skills dir.

**relayComfy:** unchanged except the Phase-2 progress emission; remains the sole ComfyUI implementation.
The `client.go:18-22` "intentional copy" comment is now the *only* copy — update it to drop the
"copy of relayLLM" framing.

---

## Documentation

- **`docs/decisions/006-image-gen-via-mcp-and-progress-framework.md`** (new ADR): record the decision to
  converge image-gen on the relayComfy MCP, retire the relayLLM builtin/HTTP duplication, and add the
  generic MCP-progress passthrough. Capture the *why* (single common-denominator interface for Claude+Pi,
  kill duplicated ComfyUI logic, charter alignment) and the one tradeoff resolved (progress via the
  framework rather than the builtin emitter).
- Update `CLAUDE.md` (relay): skills now per-category `relay-<slug>`; bridge gains a `Progress` frame.
- Update `relayLLM/CLAUDE.md` + `relayComfy/CLAUDE.md`: image-gen is MCP-only; progress over MCP.

---

## Verification (end-to-end)

1. **Phase 1, unit:** `go test ./...` in relay (hermetic). Confirm a generated `relay-comfyui/SKILL.md`
   frontmatter `description` contains "image"/"picture".
2. **Phase 1, live routing:** create a project allowing the comfyui MCP, launch a PTY, run
   `relay mcp call --list` to confirm `generate_image` is present, then in a fresh Claude Code / Pi session
   ask *"generate an image of a red bicycle"* **without** naming the tool → the `relay-comfyui` skill
   should activate and the call should fire. This is the acceptance test for the original symptom.
3. **Phase 2, progress:** with relayComfy running, trigger an image gen from an Ollama/OpenAI session in
   relayLLM and confirm "Generating… (Ns)" `ToolProgress` events stream to the Eve WS (verify in-browser
   per the test-with-Chrome convention) — i.e. progress parity with the old builtin.
4. **Phase 2, generality:** add a throwaway progress emission to a second MCP (e.g. a `cmd/testservice`
   tool) and confirm its progress also surfaces — proves the framework isn't ComfyUI-specific.
5. **Phase 3, no regression:** `grep -ri comfy relayLLM/*.go` returns only the retained `/api/generated`
   serving + the `--comfyui-url` flag if kept; image gen still works for Claude, Ollama/OpenAI, and Pi,
   all through relayComfy; generated images still render inline in Eve.
6. **Race:** `go test -race ./...` in relay (Phase 2 adds concurrency on the bridge + stdio writer).

---

## Risks / open considerations

- **Bridge protocol bump.** The `Progress` frame changes the bridge wire format. The bridge client's
  read loop must treat unknown frame types as non-terminal and skip them, so a newer tray talking to an
  older `relay mcp` (or vice-versa) degrades to "no progress" rather than breaking. Verify both binaries
  are rebuilt together (`build.sh`).
- **stdio concurrency.** Relay's `mcp/server.go` currently serializes calls per session; Phase 2 moves to
  goroutine-per-request with a mutexed encoder. Get this right or progress for a slow call blocks fast ones.
- **External PTY template.** The `SkillPath` change relies on the back-compat guard; cleanest long-term is
  to also simplify relayLLM's PTY template to point at `…/.claude/skills`. Out of this repo — note it.
- **Phasing.** Phase 1 ships independently and resolves the user's immediate pain. Phase 3 depends on
  Phase 2 (progress parity) before deleting the builtin. Stop points exist between all three phases.
