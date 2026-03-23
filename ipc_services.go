package main

import (
	"encoding/json"
	"log/slog"
)

// ---------------------------------------------------------------------------
// Service IPC handlers
// ---------------------------------------------------------------------------

// serviceConfigFromMsg converts an IPC service message to a ServiceConfig.
func serviceConfigFromMsg(msg *ipcServiceMsg, id string) ServiceConfig {
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

func ipcAddService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcServiceMsg](raw, "add_service")
	if !ok {
		return
	}

	id := slugify(msg.DisplayName)
	if id == "" || msg.Command == "" {
		return
	}

	config := serviceConfigFromMsg(msg, id)

	ctx.Store.With(func(s *Settings) { s.AddService(config) })

	if msg.Autostart {
		if err := ctx.Registry.Start(&config); err != nil {
			slog.Error("service autostart failed", "error", err)
		}
	}

	ctx.UpdateMenu()

	configJSON, _ := json.Marshal(config)
	ctx.UI.EmitEvent("onServiceAdded", json.RawMessage(configJSON))
}

func ipcRemoveService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "remove_service")
	if !ok || msg.ID == "" {
		return
	}

	ctx.Registry.Stop(msg.ID)
	ctx.Store.With(func(s *Settings) { s.RemoveService(msg.ID) })
	ctx.UpdateMenu()

	ctx.UI.EmitEvent("onServiceRemoved", msg.ID)
}

func ipcUpdateService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcServiceMsg](raw, "update_service")
	if !ok || msg.ID == "" {
		return
	}

	config := serviceConfigFromMsg(msg, msg.ID)

	ctx.Store.With(func(s *Settings) { s.UpdateService(config) })
	ctx.UpdateMenu()
}

func ipcUpdateServiceAutostart(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcUpdateServiceAutostartMsg](raw, "update_service_autostart")
	if !ok {
		return
	}
	ctx.Store.With(func(s *Settings) {
		if svc, _ := s.findServiceByID(msg.ID); svc != nil {
			svc.Autostart = msg.Autostart
		}
	})
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
	ctx.UI.EmitEvent("onServiceStatus", ctx.Registry.RunningIDs())
	ctx.UpdateMenu()
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
			ctx.UI.EmitEvent("onServiceStatus", ctx.Registry.RunningIDs())
			ctx.UpdateMenu()
		})
	})
}
