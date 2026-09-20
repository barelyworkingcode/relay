package main

import (
	"encoding/json"

	"github.com/barelyworkingcode/relay/internal/config"
)

// The Overview tab's three "Reveal" actions. Each opens Finder on a
// directory rather than a specific file — the same choice ipcRevealAuditLog
// makes and for the same reason: relay has no TCC-free "select this exact
// file" API, and a directory is what Platform.OpenURL(fileURL(...)) can
// actually do. IPC-only like ipcRevealAuditLog: a remote caller popping a
// Finder window on someone's desktop is not a sensible HTTP capability.

func ipcRevealConfigDir(ctx *IPCContext, _ json.RawMessage) {
	if ctx.ConfigDir == "" {
		return
	}
	ctx.Platform.OpenURL(fileURL(ctx.ConfigDir))
}

func ipcRevealLogsDir(ctx *IPCContext, _ json.RawMessage) {
	dir, err := ctx.LogsDir()
	if err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
		return
	}
	ctx.Platform.OpenURL(fileURL(dir))
}

// ipcRevealServiceLog reveals the SAME logs directory ipcRevealLogsDir does
// — every managed service's merged stdout+stderr lands there as
// "<id>.log" — but only for an id that names a configured service, so the
// Services tab's per-card "Logs" link can't be used to fish for the
// directory's existence under an id that was never real.
func ipcRevealServiceLog(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, MsgRevealServiceLog)
	if !ok || msg.ID == "" {
		return
	}
	found := false
	for _, svc := range config.DisplaySettings(ctx.Store).Services {
		if svc.ID == msg.ID {
			found = true
			break
		}
	}
	if !found {
		return
	}
	dir, err := ctx.LogsDir()
	if err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
		return
	}
	ctx.Platform.OpenURL(fileURL(dir))
}
