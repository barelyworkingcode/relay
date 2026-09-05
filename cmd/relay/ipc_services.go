package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/barelyworkingcode/relay/internal/config"
)

// nativeServiceConfigWithCreds augments native_view.go's shared
// nativeServiceConfig with the field it does not carry: FrontendConsumer's
// tri-state spelled out as "explicit"/"implicit"/"off", the same three cases
// `relay service list`'s FRONT-DOOR column shows, rather than leaving the
// Settings window to re-derive them from FrontendConsumer's raw nil/true/false.
type nativeServiceConfigWithCreds struct {
	nativeServiceConfig
	FrontendCreds string `json:"frontend_creds"`
}

func serviceConfigToNativeViewWithCreds(c config.ServiceConfig) nativeServiceConfigWithCreds {
	return nativeServiceConfigWithCreds{
		nativeServiceConfig: serviceConfigToNativeView(c),
		FrontendCreds:       c.FrontendCredsState(),
	}
}

// fields always sets WorkingDir, Autostart and URL (never leaves them nil):
// the Settings window's form carries the service's complete state on every
// save, add or update, so every field it sends is an explicit value on the
// wire already — there is no "the operator left this blank" case for IPC to
// distinguish the way the CLI's absent flags need to. Only ServiceOps.Update
// needs the nil case at all, and only the CLI (register with a flag left off
// the command line) produces it.
func (msg *ipcServiceMsg) fields() serviceFields {
	return serviceFields{
		DisplayName: msg.DisplayName,
		Command:     msg.Command,
		Args:        msg.Args,
		Env:         msg.Env,
		WorkingDir:  &msg.WorkingDir,
		Autostart:   &msg.Autostart,
		URL:         &msg.URL,
	}
}

func ipcAddService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcServiceMsg](raw, "add_service")
	if !ok {
		return
	}
	fields := msg.fields()

	// Off the main thread: ServiceOps.Create is gated (service.register,
	// §6.4 of the ADR-017 implementation spec), and Gate.Require blocks on
	// LocalAuthentication's async completion handler, which needs the
	// Cocoa run loop pumped to be delivered — the same deadlock
	// showLoginCode's doc comment in trayapp.go describes. ipcUpdateService
	// just below already runs off-thread for an unrelated reason; this
	// keeps the two consistent.
	ctx.GoFunc(func() {
		created, err := ctx.Ops.Create(ctx.Ctx, fields, auditViaIPC, "")
		// Only errServiceProcess means the record landed; every other error
		// means nothing was persisted, and announcing a row for it would add
		// a blank service to the list.
		if err != nil && !errors.Is(err, errServiceProcess) {
			dispatchEmit(ctx, "onSettingsError", err.Error())
			return
		}
		ctx.Platform.DispatchToMain(func() {
			if err != nil {
				ctx.UI.EmitEvent("onSettingsError", fmt.Sprintf("service added but %v", err))
			}
			ctx.UpdateMenu()
			ctx.UI.EmitEvent("onServiceAdded", marshalForUI(serviceConfigToNativeViewWithCreds(created)))
		})
	})
}

func ipcRemoveService(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "remove_service")
	if !ok || msg.ID == "" {
		return
	}

	// Remove blocks on the stopped process's exit, so it runs off the UI thread.
	ctx.GoFunc(func() {
		err := ctx.Ops.Remove(msg.ID, auditViaIPC, "")
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
		_, err := ctx.Ops.Update(ctx.Ctx, msg.ID, fields, auditViaIPC, "")
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
