# Plan: Gate chat-input icons on per-model capability flags

## Context

In Eve's chat input, the paperclip (attach files) and hexagon (plan/permission mode) icons are always shown. When the active session uses a non-Claude model, clicking the hexagon yields a runtime error: `permission mode toggle only supported for Claude provider` (from `relayLLM/ws.go:408`). That check exists because permission mode is wired through Claude CLI flags (`--permission-mode`) and has no equivalent for the OpenAI / Ollama / Llama paths.

The right fix is to advertise capabilities up front: relayLLM tells Eve, per model, whether the underlying provider supports permission mode and attachments. Eve hides icons the active model doesn't support, so the user never reaches the dead-end error.

Today the model catalog (`GET /api/models`) returns only `{label, value, group, provider}` — no capability metadata. Eve already has a `provider`-string sniff in `eve/public/core/ui-utils.js:55-57` (`isClaudeModel`), but that's an indirect proxy and only covers Claude.

## Approach

Two-side change, kept narrow:

1. **relayLLM** — add two boolean fields to the model catalog response and a small per-provider capability lookup. The WS permission-mode handler reuses the same lookup so the gate logic lives in one place.
2. **Eve** — on session activation and on model-dropdown change, look up the selected model's capabilities and toggle the two icons via `hidden`.

Capabilities are static per provider type (not per individual model). Treatment by provider:

| Provider | `supportsPermissions` | `supportsAttachments` |
|---|---|---|
| `claude` | true | true |
| `openai` | false | true |
| `ollama` | false | true |
| `llama`  | false | false |

(Per-model variation — e.g. only vision-capable OpenAI models support images — is out of scope for this change; revisit if it bites.)

## Changes

### relayLLM

**New file: `relayLLM/provider_capabilities.go`**

```go
package main

// ProviderCapabilities reports static feature support per provider type.
// Surfaced in the model catalog so clients can gate UI features without
// guessing from the provider string.
type ProviderCapabilities struct {
    SupportsPermissions bool `json:"supportsPermissions"`
    SupportsAttachments bool `json:"supportsAttachments"`
}

// CapabilitiesForProvider returns capabilities for a provider type string
// as used by Session.ProviderType ("claude", "openai", "ollama", "llama").
func CapabilitiesForProvider(providerType string) ProviderCapabilities {
    switch providerType {
    case "claude":
        return ProviderCapabilities{SupportsPermissions: true, SupportsAttachments: true}
    case "openai", "ollama":
        return ProviderCapabilities{SupportsAttachments: true}
    default:
        return ProviderCapabilities{}
    }
}
```

**`relayLLM/api.go` (lines 131–136):** extend `ModelInfo` and populate the new fields when building the catalog.

```go
type ModelInfo struct {
    Label               string `json:"label"`
    Value               string `json:"value"`
    Group               string `json:"group"`
    Provider            string `json:"provider"`
    SupportsPermissions bool   `json:"supportsPermissions"`
    SupportsAttachments bool   `json:"supportsAttachments"`
}
```

Then, in the handler at `api.go:139`, after collecting all `models`, post-process to stamp capabilities:

```go
for i := range models {
    caps := CapabilitiesForProvider(models[i].Provider)
    models[i].SupportsPermissions = caps.SupportsPermissions
    models[i].SupportsAttachments = caps.SupportsAttachments
}
```

This keeps the change additive and avoids touching the four model-source paths (hardcoded Claude list, `fetchOllamaModels`, `FetchOpenAIModels`, `LlamaServerManager.ListModels`).

**`relayLLM/ws.go:406-408`:** replace the type assertion with the capability lookup so the gate stays consistent with what the catalog advertises.

```go
if !CapabilitiesForProvider(session.ProviderType).SupportsPermissions {
    sendWSError(wc, "permission mode toggle not supported for this provider")
    return
}
```

(The downstream code still needs the `*ClaudeProvider` to call `SetPermissionMode`, so the type assertion below it stays — this just makes the user-facing gate match the advertised capability.)

### Eve

**`eve/public/app.js`** — add a helper and call it on session/model changes:

```js
// Toggle chat-input icons based on the active model's advertised capabilities.
_updateChatInputCapabilities(modelValue) {
  const model = this.models.find(m => m.value === modelValue);
  const caps = {
    permissions: !!model?.supportsPermissions,
    attachments: !!model?.supportsAttachments,
  };
  if (this.elements.attachBtn)   this.elements.attachBtn.hidden   = !caps.attachments;
  if (this.elements.planModeBtn) this.elements.planModeBtn.hidden = !caps.permissions;
}
```

Call sites:

- `loadModels()` (after `setModels`, around `app.js:728`) — re-evaluate against whatever model is currently selected so the icons settle on first paint.
- `sessionModelSelect` change listener (`app.js:434`) — call `_updateChatInputCapabilities(this.elements.sessionModelSelect.value)`.
- Wherever a session becomes active (the place that drives `this.currentSessionId`) — call with `session.model`. Concretely, the handler that switches the visible session needs one extra line; locate via `grep -n "currentSessionId =" public/app.js` and add the call alongside the existing UI updates.

**No HTML change.** `hidden` on a `<button>` removes it from the layout, which matches the user's "should only appear" wording. No tooltip / disabled state — if it's not supported, it's not there.

## Critical files

- `relayLLM/provider_capabilities.go` (new)
- `relayLLM/api.go` — `ModelInfo` struct and `/api/models` handler
- `relayLLM/ws.go` — `handleSetPermissionMode` gate
- `eve/public/app.js` — capability helper + three call sites
- `eve/public/core/ui-utils.js` — leave `isClaudeModel` alone for now; it has other callers and removing it is out of scope

## Verification

1. Build relayLLM: `cd ../relayLLM && go build ./...` and run unit tests: `go test ./...` (no new tests required for a switch-table; existing `TestDeriveProviderType_*` still pass).
2. Run relayLLM + Eve locally.
3. `curl http://localhost:<port>/api/models` — confirm each entry now has `supportsPermissions` and `supportsAttachments` and Claude is the only provider with `supportsPermissions: true`.
4. In Eve (via Chrome MCP tools per memory `feedback_test_with_chrome.md`):
   - Open a Claude session → both paperclip and hexagon visible.
   - Switch the model dropdown to an OpenAI model → hexagon disappears, paperclip stays.
   - Switch to a Llama model (if configured) → both disappear.
   - Switch back to Claude → both reappear.
   - With an OpenAI session active, confirm the dead-end error is unreachable (the icon isn't there to click).
5. Sanity-check `read_console_messages` for any layout / undefined errors after the toggles.
