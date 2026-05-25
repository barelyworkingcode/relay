package main

import (
	"encoding/json"
)

// ipcResetMcpPermissions runs the Reset Permissions flow for an MCP:
// clears existing TCC grants, fires Relay-side TCC primer prompts so the
// user can grant the needed services to Relay's own bundle (MCPs inherit
// via responsible-parent attribution), then spawns the MCP with
// --check-permissions to report final status in the UI summary.
//
// The actual work runs off the main thread because tccutil + the Relay
// TCC primers + the MCP spawn can take tens of seconds; main-thread
// blocking would freeze the WebView. Results emit back via
// onMcpPermissionsReset.
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
