package main

import (
	"encoding/json"
	"errors"

	"relaygo/bridge"
)

// ---------------------------------------------------------------------------
// External MCP IPC handlers
// ---------------------------------------------------------------------------

func ipcAddExternalMcp(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAddExternalMcpMsg](raw, "add_external_mcp")
	if !ok {
		return
	}

	id := slugify(msg.DisplayName)
	if id == "" {
		return
	}

	if msg.Transport == "http" {
		if msg.URL == "" {
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

			if !ctx.withSettingsNotify(
				func(s *Settings) { s.AddExternalMcp(*result) },
				bridge.SendReconcile,
			) {
				return
			}

			mcpJSON, _ := json.Marshal(result)
			ctx.UI.EmitEvent("onExternalMcpAdded", json.RawMessage(mcpJSON))
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

	if !ctx.withSettingsNotify(
		func(s *Settings) { s.RemoveExternalMcp(msg.ID) },
		bridge.SendReconcile,
	) {
		return
	}

	ctx.UI.EmitEvent("onExternalMcpRemoved", msg.ID)
}

// ---------------------------------------------------------------------------
// HTTP MCP helpers (moved from router.go)
// ---------------------------------------------------------------------------

func addHTTPMcp(ctx *IPCContext, displayName, id, mcpURL string) {
	result, err := DiscoverHTTPMcp(ctx.Ctx, displayName, id, mcpURL, nil)

	if err != nil && !errors.Is(err, ErrAuthRequired) {
		ctx.Platform.DispatchToMain(func() {
			ctx.UI.EmitEvent("onExternalMcpError", err.Error())
		})
		return
	}

	needsAuth := errors.Is(err, ErrAuthRequired)

	ctx.Platform.DispatchToMain(func() {
		if !ctx.withSettingsNotify(
			func(s *Settings) { s.AddExternalMcp(*result) },
			bridge.SendReconcile,
		) {
			return
		}

		mcpJSON, _ := json.Marshal(result)
		ctx.UI.EmitEvent("onExternalMcpAdded", json.RawMessage(mcpJSON))

		if needsAuth {
			ctx.UI.EmitEvent("onOAuthRequired", id)
		}
	})
}

func authenticateMcp(ctx *IPCContext, id string) {
	s := ctx.Store.Get()
	mcpCfg, _ := s.findMcpByID(id)
	if mcpCfg == nil || !mcpCfg.IsHTTP() {
		return
	}

	ctx.Platform.DispatchToMain(func() {
		ctx.UI.EmitEvent("onOAuthStarted", id)
	})

	oauth, err := startOAuthFlow(mcpCfg.URL, ctx.Platform.OpenURL)
	if err != nil {
		ctx.Platform.DispatchToMain(func() {
			ctx.UI.EmitEvent("onOAuthError", id, err.Error())
		})
		return
	}

	ctx.Platform.DispatchToMain(func() {
		if !ctx.withSettingsNotify(
			func(s *Settings) { s.UpdateOAuthState(id, oauth) },
			func(secret string) error { return bridge.SendReloadMcp(id, secret) },
		) {
			return
		}

		ctx.UI.EmitEvent("onOAuthComplete", id)
	})
}
