# ADR-006: Image Generation via MCP, and a Generic MCP Progress Framework

**Status:** Accepted
**Date:** 2026-06-04

## Context

Two problems converged on one symptom — *"I ask for an image and the model says it doesn't know how until I name `generate_image`."*

1. **Skills didn't route.** Agentskills.io skills (Claude Code, Pi.dev) are lazy-loaded: the agent only sees a skill's frontmatter `description` when deciding whether to activate it; the body loads after. Relay generated one `SKILL.md` per project with a generic, capability-free description, so no request ever matched it.

2. **Image generation was forked three ways**, duplicating ComfyUI logic:
   - relayLLM in-process builtin `generate_image` (`comfyui_client.go`) — used by Ollama/OpenAI/llama; streamed text progress.
   - relayComfy MCP `generate_image` — used by Claude via `relay mcp`; a literal copy of relayLLM's client; no progress.
   - relayLLM HTTP `/api/generate-image` + `pi_image_skill.go` — used by Pi via `curl`; no progress.

   This violated relay's charter ("specific service knowledge lives in services, not in relay"): ComfyUI knowledge had a proper home (relayComfy) yet also lived inside the LLM engine.

## Decision

Make **MCP + skills the single common-denominator interface** for tools across Claude and Pi.

1. **Per-category skills with capability-rich, deterministic descriptions.** Relay generates one `relay-<category>/SKILL.md` per tool bucket (server category, else owning MCP). The description is synthesized from the tools (names + harvested trigger phrases) so a request routes without the user naming a tool. Set reconcile prunes stale `relay-*` dirs; the legacy single `relay/` dir auto-migrates. Descriptions are double-quoted — they contain `": "`, which strict YAML parsers reject unquoted. (`skills.go`)

2. **A generic MCP `notifications/progress` passthrough.** Any external MCP can stream progress; relayComfy is the first emitter. The progress sink rides in `context` (`bridge.WithProgress` / `ProgressFromContext`), so no call-chain signature changes are needed. New non-terminal `RespProgress` bridge frame (`bridge/types.go`); the `relay mcp` stdio server handles tool calls on per-request goroutines (mutex-guarded stdout) and re-emits `notifications/progress` referencing the caller's `progressToken` (`mcp/server.go`). relayLLM's go-sdk client correlates by `progressToken` and an MCP-backed tool streams status identically to the old builtin.

3. **Retire relayLLM's in-process ComfyUI binding.** Native providers (Ollama/OpenAI/llama) reach `generate_image` through the relay-comfyui MCP (web-chat sessions set `useRelayTools=true`). The relay MCP is reached with a project-scoped `RELAY_PROJECT_TOKEN` that relay resolves just-in-time by `projectId`; if none resolves the session gets no relay MCP — it fails closed (`resolveRelayMCPServer`, `provider_chat_base.go`) and never substitutes the full-access service token. The `/api/generated/{filename}` serving route stays — relayComfy writes images there and Eve renders them. Two terminal-agent (Pi) hazards were closed in the process: `relay mcp call --args-file` removes shell-quoting issues (the agent writes the JSON to a file), and relayComfy's tool description now tells terminal agents to present `file_url` as a clickable link.

### Dropped (needs rearchitecture)

Image-to-image from a chat attachment. The old in-process builtin uploaded the user's attached image and ran an img2img workflow; the relay-comfyui MCP is text-to-image only and the MCP boundary can't carry the chat's file attachments. Re-introducing it needs a design that gets the input image to relayComfy (e.g. an image-input MCP arg referencing a served/uploaded file).

## Consequences

- One ComfyUI implementation (relayComfy). The LLM engine no longer knows about ComfyUI — builtin, `comfyui_client.go`, `/api/generate-image`, and the pi curl skill are gone. relayLLM retains only the `/api/generated/` static serving route, which requires relayComfy's `RELAY_LLM_DATA` to match relayLLM's data dir.
- The bridge wire format gains a non-terminal `Progress` frame. Clients skip unknown frame types, so a newer tray talking to an older `relay mcp` (or vice versa) degrades to "no progress" rather than breaking — but rebuild both together (`build.sh`).
- Progress for native providers requires `useRelayTools` (the web-chat path sets it). A native session created without relay tools has no image-gen once the builtin is gone — acceptable under "image-gen is an MCP tool."
- The `relay mcp` stdio server now serves tool calls concurrently; a long generation no longer blocks other calls on the same session.
