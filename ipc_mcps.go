package main

import (
	"encoding/json"
	"errors"
)

// ---------------------------------------------------------------------------
// External MCP IPC handlers
// ---------------------------------------------------------------------------

// dispatchError emits a named error event to the settings UI on the main thread.
// Use from background goroutines where DispatchToMain is required.
func dispatchError(ctx *IPCContext, event string, args ...interface{}) {
	ctx.Platform.DispatchToMain(func() {
		ctx.UI.EmitEvent(event, args...)
	})
}

func ipcAddExternalMcp(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAddExternalMcpMsg](raw, "add_external_mcp")
	if !ok {
		return
	}

	id := slugify(msg.DisplayName)
	if id == "" {
		ctx.UI.EmitEvent("onExternalMcpError", "display name is required")
		return
	}

	if msg.Transport == "http" {
		if msg.URL == "" {
			ctx.UI.EmitEvent("onExternalMcpError", "URL is required for HTTP transport")
			return
		}
		if err := validateMcpURL(msg.URL); err != nil {
			ctx.UI.EmitEvent("onExternalMcpError", err.Error())
			return
		}
		ctx.UI.EmitEvent("onDiscoveryStarted")
		ctx.GoFunc(func() { addHTTPMcp(ctx, msg.DisplayName, id, msg.URL) })
		return
	}

	if msg.Command == "" {
		ctx.UI.EmitEvent("onExternalMcpError", "command is required for stdio transport")
		return
	}

	ctx.UI.EmitEvent("onDiscoveryStarted")

	ctx.GoFunc(func() {
		result, err := DiscoverExternalMcp(ctx.Ctx, msg.DisplayName, id, msg.Command, msg.Args, msg.Env)
		ctx.Platform.DispatchToMain(func() {
			if err != nil {
				ctx.UI.EmitEvent("onExternalMcpError", err.Error())
				return
			}

			if !ctx.withSettingsReconcile(func(s *Settings) { s.UpsertExternalMcp(*result) }) {
				return
			}

			ctx.UI.EmitEvent("onExternalMcpAdded", marshalForUI(result))
		})
	})
}

func ipcAuthenticateMcp(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "authenticate_mcp")
	if !ok || msg.ID == "" {
		return
	}
	ctx.GoFunc(func() { authenticateMcp(ctx, msg.ID) })
}

func ipcRemoveExternalMcp(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "remove_external_mcp")
	if !ok || msg.ID == "" {
		return
	}

	if !ctx.withSettingsReconcile(func(s *Settings) { s.RemoveExternalMcp(msg.ID) }) {
		return
	}

	ctx.UI.EmitEvent("onExternalMcpRemoved", msg.ID)
}

// ---------------------------------------------------------------------------
// HTTP MCP helpers
// ---------------------------------------------------------------------------

func addHTTPMcp(ctx *IPCContext, displayName, id, mcpURL string) {
	result, err := DiscoverHTTPMcp(ctx.Ctx, displayName, id, mcpURL, nil)

	if err != nil && !errors.Is(err, ErrAuthRequired) {
		dispatchError(ctx, "onExternalMcpError", err.Error())
		return
	}
	if result == nil {
		dispatchError(ctx, "onExternalMcpError", "discovery returned no configuration")
		return
	}

	needsAuth := errors.Is(err, ErrAuthRequired)

	ctx.Platform.DispatchToMain(func() {
		if !ctx.withSettingsReconcile(func(s *Settings) { s.UpsertExternalMcp(*result) }) {
			return
		}

		ctx.UI.EmitEvent("onExternalMcpAdded", marshalForUI(result))

		if needsAuth {
			ctx.UI.EmitEvent("onOAuthRequired", id)
		}
	})
}

func authenticateMcp(ctx *IPCContext, id string) {
	s := ctx.Store.Get()
	mcpCfg, _ := s.findMcpByID(id)
	if mcpCfg == nil {
		dispatchError(ctx, "onOAuthError", id, "MCP not found")
		return
	}
	if !mcpCfg.IsHTTP() {
		dispatchError(ctx, "onOAuthError", id, "only HTTP MCPs support OAuth")
		return
	}

	ctx.Platform.DispatchToMain(func() {
		ctx.UI.EmitEvent("onOAuthStarted", id)
	})

	oauth, err := startOAuthFlow(mcpCfg.URL, ctx.Platform.OpenURL)
	if err != nil {
		dispatchError(ctx, "onOAuthError", id, err.Error())
		return
	}

	ctx.Platform.DispatchToMain(func() {
		if !ctx.withSettingsNotify(
			func(s *Settings) { s.UpdateOAuthState(id, oauth) },
			func(secret string) error { return ctx.NotifyReloadMcp(id, secret) },
		) {
			return
		}

		ctx.UI.EmitEvent("onOAuthComplete", id)
	})
}
