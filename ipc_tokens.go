package main

import (
	"encoding/json"
)

// ---------------------------------------------------------------------------
// Token & permission IPC handlers
// ---------------------------------------------------------------------------

func ipcGenerateToken(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcGenerateTokenMsg](raw, "generate_token")
	if !ok {
		return
	}

	var plaintext string
	var stored StoredToken
	if err := ctx.Store.With(func(s *Settings) {
		defaultPerms := make(map[string]Permission)
		for _, svcName := range s.AllExternalMcpIDs() {
			defaultPerms[svcName] = PermOff
		}
		plaintext, stored = GenerateToken(msg.Name, defaultPerms)
		s.Tokens = append(s.Tokens, stored)
	}); err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
		return
	}

	ctx.UI.EmitEvent("onTokenGenerated", map[string]interface{}{
		"plaintext": plaintext,
		"token":     stored,
	})
}

func ipcDeleteToken(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcTokenHash](raw, "delete_token")
	if !ok || msg.Hash == "" {
		return
	}
	if err := ctx.Store.With(func(s *Settings) { s.DeleteToken(msg.Hash) }); err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
		return
	}
	ctx.UI.EmitEvent("onTokenDeleted", msg.Hash)
}

func ipcRevokeAll(ctx *IPCContext, _ json.RawMessage) {
	if err := ctx.Store.With(func(s *Settings) { s.RevokeAll() }); err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
		return
	}
	ctx.UI.EmitEvent("onAllRevoked")
}

func ipcUpdatePermission(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcUpdatePermissionMsg](raw, "update_permission")
	if !ok {
		return
	}
	perm := PermOn
	if msg.Permission == "off" {
		perm = PermOff
	}
	if err := ctx.Store.With(func(s *Settings) { s.UpdatePermission(msg.Hash, msg.Service, perm) }); err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
	}
}

func ipcSetToolDisabled(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcSetToolDisabledMsg](raw, "set_tool_disabled")
	if !ok {
		return
	}
	if err := ctx.Store.With(func(s *Settings) { s.SetToolDisabled(msg.Hash, msg.McpID, msg.ToolName, msg.Disabled) }); err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
	}
}

func ipcSetAllToolsDisabled(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcSetAllToolsDisabledMsg](raw, "set_all_tools_disabled")
	if !ok {
		return
	}
	if err := ctx.Store.With(func(s *Settings) {
		var toolNames []string
		if mcp, _ := s.findMcpByID(msg.McpID); mcp != nil {
			for _, t := range mcp.DiscoveredTools {
				toolNames = append(toolNames, t.Name)
			}
		}
		s.SetAllToolsDisabled(msg.Hash, msg.McpID, toolNames, msg.Disabled)
	}); err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
	}
}

func ipcSetContext(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcSetContextMsg](raw, "set_context")
	if !ok {
		return
	}
	contextRaw, err := json.Marshal(msg.Context)
	if err != nil {
		return
	}
	if err := ctx.Store.With(func(s *Settings) { s.SetContext(msg.Hash, msg.McpID, json.RawMessage(contextRaw)) }); err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
	}
}
