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
	if !ctx.withSettings(func(s *Settings) {
		defaultPerms := make(map[string]Permission)
		for _, svcName := range s.AllExternalMcpIDs() {
			defaultPerms[svcName] = PermOff
		}
		plaintext, stored = GenerateToken(msg.Name, defaultPerms)
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
	ctx.withSettings(func(s *Settings) { s.UpdatePermission(msg.Hash, msg.Service, perm) })
}

func ipcSetToolDisabled(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcSetToolDisabledMsg](raw, "set_tool_disabled")
	if !ok {
		return
	}
	ctx.withSettings(func(s *Settings) { s.SetToolDisabled(msg.Hash, msg.McpID, msg.ToolName, msg.Disabled) })
}

func ipcSetAllToolsDisabled(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcSetAllToolsDisabledMsg](raw, "set_all_tools_disabled")
	if !ok {
		return
	}
	ctx.withSettings(func(s *Settings) {
		var toolNames []string
		if mcp, _ := s.findMcpByID(msg.McpID); mcp != nil {
			for _, t := range mcp.DiscoveredTools {
				toolNames = append(toolNames, t.Name)
			}
		}
		s.SetAllToolsDisabled(msg.Hash, msg.McpID, toolNames, msg.Disabled)
	})
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
	ctx.withSettings(func(s *Settings) { s.SetContext(msg.Hash, msg.McpID, json.RawMessage(contextRaw)) })
}
