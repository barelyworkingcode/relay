# ADR-006: Image Generation via MCP, and a Generic MCP Progress Framework

**Status:** Accepted (Phases 1–2 complete; Phase 3 native path complete, pi path pending)
**Date:** 2026-06-04

## Context

Two problems converged on one symptom — *"I ask for an image and the model says
it doesn't know how until I name `generate_image`."*

1. **Skills didn't route.** Agentskills.io skills (Claude Code, Pi.dev) are
   lazy-loaded: the agent only sees a skill's frontmatter `description` when
   deciding whether to activate it; the body loads after. Relay generated one
   `SKILL.md` per project with a generic, capability-free description, so no
   request ever matched it.

2. **Image generation was forked three ways**, duplicating ComfyUI logic:
   - relayLLM in-process builtin `generate_image` (`comfyui_client.go`) — used
     by Ollama/OpenAI/llama; streamed text progress.
   - relayComfy MCP `generate_image` (`relayComfy/mcp/`) — used by Claude via
     `relay mcp`; a literal copy of relayLLM's client; no progress.
   - relayLLM HTTP `/api/generate-image` + `pi_image_skill.go` — used by Pi via
     `curl`; no progress.

   This violated relay's charter ("specific service knowledge lives in services,
   not in relay"): ComfyUI knowledge had a proper home (relayComfy) yet also
   lived inside the LLM engine.

## Decision

Make **MCP + skills the single common-denominator interface** for tools across
Claude and Pi:

1. **Per-category skills with capability-rich, deterministic descriptions.**
   Relay generates one `relay-<category>/SKILL.md` per tool bucket (server
   category, else owning MCP). The description is synthesized from the tools
   (names + harvested trigger phrases) so a request routes without the user
   naming a tool. Set reconcile prunes stale `relay-*` dirs; the legacy single
   `relay/` dir auto-migrates. Descriptions are double-quoted (they contain
   `": "`, which strict YAML parsers reject unquoted).

2. **A generic MCP `notifications/progress` passthrough.** Any external MCP can
   stream progress; relayComfy is the first emitter. The progress sink rides in
   `context` (`bridge.WithProgress` / `ProgressFromContext`), so the
   `ToolRouter` / `router.CallTool` / `manager.CallTool` / `SendRequest`
   signatures are unchanged. New `RespProgress` bridge frame; the relay mcp
   stdio server handles tool calls on per-request goroutines (mutex-guarded
   stdout) and re-emits `notifications/progress` referencing the caller's
   token. relayLLM's go-sdk client sets a `ProgressNotificationHandler` and
   correlates by `progressToken` to `EventEmitter.ToolProgress` — so an
   MCP-backed tool streams status identically to the old builtin.

3. **Retire relayLLM's in-process ComfyUI binding.** Native providers
   (Ollama/OpenAI/llama) reach `generate_image` through the relay-comfyui MCP
   (web-chat sessions set `useRelayTools=true`; relayLLM falls back to the
   injected `RELAY_MCP_TOKEN` service token). The `/api/generated/{filename}`
   serving route stays — relayComfy writes images there and Eve renders them.

## Status of implementation

- **Done + verified live:** per-category skills (routing confirmed across
  Claude Haiku, pi, llama); the generic progress framework end-to-end (llama
  shows `Generating image… (Ns)` sourced from relayComfy through every hop);
  native providers consolidated onto the MCP.
- **Pending:** retiring pi's curl path (`/api/generate-image` +
  `pi_image_skill.go`) and removing the now-unused builtin registration. Pi.dev
  via the relay-comfyui skill currently has two gaps to resolve first: bash
  arg-quoting on `relay mcp call` (parentheses in the prompt), and presenting
  the result as a clickable `file://` URI rather than attempting inline render.

## Consequences

- One ComfyUI implementation (relayComfy); the LLM engine no longer knows about
  ComfyUI once the pi path is retired.
- The bridge wire format gains a non-terminal `Progress` frame. Clients skip
  unknown frame types, so a newer tray talking to an older `relay mcp` (or vice
  versa) degrades to "no progress" rather than breaking — but rebuild both
  together (`build.sh`).
- Progress for native providers requires `useRelayTools` (the web-chat path
  sets it). A native session created without relay tools has no image-gen once
  the builtin is gone — acceptable under "image-gen is an MCP tool."
- The relay mcp stdio server now serves tool calls concurrently; a long
  generation no longer blocks other calls on the same session.

See `plans/i-want-to-talk-quirky-widget.md` for the full plan and per-hop seams.
