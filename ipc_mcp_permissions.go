package main

import (
	"encoding/json"
)

// Runs off the main thread: ResetPermissions blocks for tccutil, TCC
// prompts, and an MCP spawn.
func ipcResetMcpPermissions(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "reset_mcp_permissions")
	if !ok || msg.ID == "" {
		return
	}

	ctx.GoFunc(func() {
		result, err := ctx.McpOps.ResetPermissions(msg.ID)
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
