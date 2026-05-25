package main

import (
	"encoding/json"
)

// ipcResetMcpPermissions runs ResetMcpPermissions off the main thread (it
// blocks for tccutil + TCC prompts + MCP spawn). Results emit back via
// onMcpPermissionsReset. See mcp_permissions.go for the flow.
func ipcResetMcpPermissions(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "reset_mcp_permissions")
	if !ok || msg.ID == "" {
		return
	}

	s := ctx.Store.Get()
	mcp, _ := s.findMcpByID(msg.ID)
	if mcp == nil {
		ctx.UI.EmitEvent("onMcpPermissionsReset", msg.ID, map[string]interface{}{
			"ok":    false,
			"error": "MCP not found: " + msg.ID,
		})
		return
	}

	mcpCopy := *mcp
	ctx.GoFunc(func() {
		result, err := ResetMcpPermissions(mcpCopy)
		ctx.Platform.DispatchToMain(func() {
			if err != nil {
				ctx.UI.EmitEvent("onMcpPermissionsReset", msg.ID, map[string]interface{}{
					"ok":    false,
					"error": err.Error(),
				})
				return
			}
			ctx.UI.EmitEvent("onMcpPermissionsReset", msg.ID, map[string]interface{}{
				"ok":              true,
				"bundle_id":       result.BundleID,
				"reset_services":  result.ResetServices,
				"skipped_reasons": result.SkippedReasons,
				"spawn_output":    result.SpawnOutput,
			})
		})
	})
}
