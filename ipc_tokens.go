package main

import (
	"encoding/json"
	"log/slog"
)

// ---------------------------------------------------------------------------
// Token & permission IPC handlers
// ---------------------------------------------------------------------------

func ipcGenerateToken(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcGenerateTokenMsg](raw, "generate_token")
	if !ok {
		return
	}

	// Generate the token before acquiring the settings lock.
	// Default permissions are computed inside withSettings to avoid a
	// TOCTOU race: MCPs could be added between a separate Get() and With().
	plaintext, stored, err := GenerateToken(msg.Name, nil)
	if err != nil {
		slog.Error("failed to generate token", "error", err)
		ctx.UI.EmitEvent("onSettingsError", "failed to generate token")
		return
	}

	if !ctx.withSettings(func(s *Settings) {
		// Set default permissions (all off) using the current MCP list
		// inside the write lock so no MCPs are missed.
		stored.Permissions = make(map[string]Permission)
		for _, id := range s.AllExternalMcpIDs() {
			stored.Permissions[id] = PermOff
		}
		s.Tokens = append(s.Tokens, stored)
	}) {
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
	if !ctx.withSettings(func(s *Settings) { s.DeleteToken(msg.Hash) }) {
		return
	}
	ctx.UI.EmitEvent("onTokenDeleted", msg.Hash)
}

func ipcRevokeAll(ctx *IPCContext, _ json.RawMessage) {
	if !ctx.withSettings(func(s *Settings) { s.RevokeAll() }) {
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
	if !ctx.withSettings(func(s *Settings) { s.UpdatePermission(msg.Hash, msg.Service, perm) }) {
		return
	}
}

func ipcSetToolDisabled(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcSetToolDisabledMsg](raw, "set_tool_disabled")
	if !ok {
		return
	}
	if !ctx.withSettings(func(s *Settings) { s.SetToolDisabled(msg.Hash, msg.McpID, msg.ToolName, msg.Disabled) }) {
		return
	}
}

func ipcSetAllToolsDisabled(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcSetAllToolsDisabledMsg](raw, "set_all_tools_disabled")
	if !ok {
		return
	}
	if !ctx.withSettings(func(s *Settings) {
		var toolNames []string
		if mcp, _ := s.findMcpByID(msg.McpID); mcp != nil {
			for _, t := range mcp.DiscoveredTools {
				toolNames = append(toolNames, t.Name)
			}
		}
		s.SetAllToolsDisabled(msg.Hash, msg.McpID, toolNames, msg.Disabled)
	}) {
		return
	}
}

func ipcSetContext(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcSetContextMsg](raw, "set_context")
	if !ok {
		return
	}
	contextRaw, err := json.Marshal(msg.Context)
	if err != nil {
		ctx.UI.EmitEvent("onSettingsError", "failed to save context: "+err.Error())
		return
	}
	if !ctx.withSettings(func(s *Settings) { s.SetContext(msg.Hash, msg.McpID, json.RawMessage(contextRaw)) }) {
		return
	}
}
