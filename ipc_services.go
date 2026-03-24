package main

import (
	"encoding/json"
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

	ctx.Registry.Stop(msg.ID)
	if !ctx.withSettings(func(s *Settings) { s.RemoveService(msg.ID) }) {
		return
	}
	ctx.UpdateMenu()

	ctx.UI.EmitEvent("onServiceRemoved", msg.ID)
}

func ipcUpdateService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcServiceMsg](raw, "update_service")
	if !ok || msg.ID == "" {
		return
	}

	config := msg.toServiceConfig(msg.ID)

	if !ctx.withSettings(func(s *Settings) { s.UpdateService(config) }) {
		return
	}
	ctx.UpdateMenu()
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
