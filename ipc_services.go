package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// toServiceConfig converts an IPC service message to a ServiceConfig.
// The id parameter overrides msg.ID (used for add where ID is derived from name).
func (msg *ipcServiceMsg) toServiceConfig(id string) ServiceConfig {
	return ServiceConfig{
		ID:          id,
		DisplayName: msg.DisplayName,
		Command:     msg.Command,
		Args:        msg.Args,
		Env:         msg.Env,
		WorkingDir:  msg.WorkingDir,
		Autostart:   msg.Autostart,
		URL:         msg.URL,
	}
}

// ---------------------------------------------------------------------------
// Service IPC handlers
// ---------------------------------------------------------------------------

func ipcAddService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcServiceMsg](raw, "add_service")
	if !ok {
		return
	}

	id := slugify(msg.DisplayName)
	if id == "" {
		ctx.UI.EmitEvent("onSettingsError", "display name is required")
		return
	}
	if msg.Command == "" {
		ctx.UI.EmitEvent("onSettingsError", "command is required")
		return
	}

	config := msg.toServiceConfig(id)

	if !ctx.withSettings(func(s *Settings) { s.UpsertService(config) }) {
		return
	}

	if config.Autostart {
		if err := ctx.Registry.Start(&config); err != nil {
			slog.Error("service autostart failed", "error", err)
			ctx.UI.EmitEvent("onSettingsError", fmt.Sprintf("service added but autostart failed: %v", err))
		}
	}

	ctx.UpdateMenu()

	ctx.UI.EmitEvent("onServiceAdded", marshalForUI(config))
}

func ipcRemoveService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "remove_service")
	if !ok || msg.ID == "" {
		return
	}

	// Remove from settings immediately so the UI updates without delay.
	if !ctx.withSettings(func(s *Settings) { s.RemoveService(msg.ID) }) {
		return
	}
	ctx.UI.EmitEvent("onServiceRemoved", msg.ID)

	// Stop the process asynchronously — Stop() blocks waiting for process
	// exit and must not run on the main/UI thread.
	ctx.GoFunc(func() {
		ctx.Registry.Stop(msg.ID)
		ctx.Platform.DispatchToMain(func() {
			ctx.refreshServiceUI()
		})
	})
}

func ipcUpdateService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcServiceMsg](raw, "update_service")
	if !ok || msg.ID == "" {
		return
	}
	if msg.Command == "" {
		ctx.UI.EmitEvent("onSettingsError", "command is required")
		return
	}

	config := msg.toServiceConfig(msg.ID)
	wasRunning := ctx.Registry.IsRunning(msg.ID)

	if !ctx.withSettings(func(s *Settings) { s.UpdateService(config) }) {
		return
	}

	// Restart the service if it was running so it picks up the new config.
	// Reload calls Stop() which blocks waiting for process exit, so run
	// it off the main/UI thread.
	if wasRunning {
		ctx.GoFunc(func() {
			if err := ctx.Registry.Reload(msg.ID, &config); err != nil {
				slog.Error("service restart after update failed", "id", msg.ID, "error", err)
				ctx.Platform.DispatchToMain(func() {
					ctx.UI.EmitEvent("onSettingsError", fmt.Sprintf("service updated but restart failed: %v", err))
				})
			}
			ctx.Platform.DispatchToMain(func() {
				ctx.refreshServiceUI()
			})
		})
	} else {
		ctx.refreshServiceUI()
	}
}

func ipcUpdateServiceAutostart(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcUpdateServiceAutostartMsg](raw, "update_service_autostart")
	if !ok {
		return
	}
	if !ctx.withSettings(func(s *Settings) {
		s.SetServiceAutostart(msg.ID, msg.Autostart)
	}) {
		return
	}
}

// ipcStartService starts a service synchronously on the IPC thread.
// Start is fast (spawns a child process) so blocking is acceptable.
func ipcStartService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "start_service")
	if !ok || msg.ID == "" {
		return
	}
	s := ctx.Store.Get()
	if svc, _ := s.findServiceByID(msg.ID); svc != nil {
		if err := ctx.Registry.Start(svc); err != nil {
			slog.Error("service start failed", "error", err)
			ctx.UI.EmitEvent("onSettingsError", fmt.Sprintf("failed to start service: %v", err))
		}
	}
	ctx.refreshServiceUI()
}

// ipcStopService stops a service asynchronously because process teardown may
// block (SIGTERM + wait). UI updates dispatch back to main thread when done.
func ipcStopService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "stop_service")
	if !ok || msg.ID == "" {
		return
	}
	ctx.GoFunc(func() {
		ctx.Registry.Stop(msg.ID)
		ctx.Platform.DispatchToMain(func() {
			ctx.refreshServiceUI()
		})
	})
}
