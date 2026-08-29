package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

func (msg *ipcServiceMsg) fields() serviceFields {
	return serviceFields{
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

	created, err := ctx.Ops.Create(msg.fields())
	// Only errServiceProcess means the record landed; every other error means
	// nothing was persisted, and announcing a row for it would add a blank
	// service to the list.
	if err != nil && !errors.Is(err, errServiceProcess) {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
		return
	}
	if err != nil {
		ctx.UI.EmitEvent("onSettingsError", fmt.Sprintf("service added but %v", err))
	}

	ctx.UpdateMenu()
	ctx.UI.EmitEvent("onServiceAdded", marshalForUI(serviceConfigToNativeView(created)))
}

func ipcRemoveService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "remove_service")
	if !ok || msg.ID == "" {
		return
	}

	// Remove blocks on the stopped process's exit, so it runs off the UI thread.
	ctx.GoFunc(func() {
		err := ctx.Ops.Remove(msg.ID)
		ctx.Platform.DispatchToMain(func() {
			if err != nil {
				ctx.UI.EmitEvent("onSettingsError", err.Error())
				return
			}
			ctx.UI.EmitEvent("onServiceRemoved", msg.ID)
			ctx.refreshServiceUI()
		})
	})
}

func ipcUpdateService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcServiceMsg](raw, "update_service")
	if !ok || msg.ID == "" {
		return
	}
	fields := msg.fields()

	// Always off the UI thread: Update reloads a service that is running, and
	// whether it is running can change between any check here and Update's own.
	// Branching on it would put a blocking Stop on the main thread.
	ctx.GoFunc(func() {
		_, err := ctx.Ops.Update(msg.ID, fields)
		ctx.Platform.DispatchToMain(func() {
			switch {
			case err != nil && errors.Is(err, errServiceProcess):
				ctx.UI.EmitEvent("onSettingsError", fmt.Sprintf("service updated but %v", err))
			case err != nil:
				ctx.UI.EmitEvent("onSettingsError", err.Error())
				return
			}
			ctx.refreshServiceUI()
		})
	})
}

func ipcUpdateServiceAutostart(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcUpdateServiceAutostartMsg](raw, "update_service_autostart")
	if !ok || msg.ID == "" {
		return
	}
	if err := ctx.Ops.SetAutostart(msg.ID, msg.Autostart); err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
	}
}

// Synchronous on the IPC thread: spawning a child is fast, unlike stopping one.
func ipcStartService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "start_service")
	if !ok || msg.ID == "" {
		return
	}
	if err := ctx.Ops.Start(msg.ID); err != nil {
		ctx.UI.EmitEvent("onSettingsError", fmt.Sprintf("failed to start service: %v", err))
	}
	ctx.refreshServiceUI()
}

func ipcStopService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "stop_service")
	if !ok || msg.ID == "" {
		return
	}
	ctx.GoFunc(func() {
		err := ctx.Ops.Stop(msg.ID)
		ctx.Platform.DispatchToMain(func() {
			if err != nil {
				ctx.UI.EmitEvent("onSettingsError", err.Error())
				return
			}
			ctx.refreshServiceUI()
		})
	})
}
