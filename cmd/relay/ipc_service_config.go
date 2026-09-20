package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/service"
)

// The manifest is the authority: relay refuses get/save for any service that
// did not declare a Config. The file is opaque text on the wire — relay
// validates that it parses and is within the size cap, but never interprets
// its shape (the schema-driven form lives in the UI).
type ipcServiceConfigMsg struct {
	ServiceID string `json:"serviceId"`
	Op        string `json:"op"`             // "get" | "save"
	Text      string `json:"text,omitempty"` // edited file text, for "save"
}

const MsgServiceConfig = "service_config"

const (
	configOpGet  = "get"
	configOpSave = "save"
)

// Reads resolve through service.ResolveConfigPath (the single security gate)
// and run off the UI thread; a save goes through ServiceOps.SaveConfigFile,
// which restarts the service and so blocks on process exit.
func ipcServiceConfig(ipc *IPCContext, raw json.RawMessage) {
	var msg ipcServiceConfigMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		dispatchEmit(ipc, "onServiceConfigResult", map[string]interface{}{
			"ok":    false,
			"error": fmt.Sprintf("decode config op: %v", err),
		})
		return
	}

	switch msg.Op {
	case configOpGet:
		if ipc.Enhanced == nil {
			emitConfigResult(ipc, msg, false, "", "no enhanced registry")
			return
		}
		rec := ipc.Enhanced.Get(msg.ServiceID)
		if rec == nil {
			emitConfigResult(ipc, msg, false, "", fmt.Sprintf("service %q not registered", msg.ServiceID))
			return
		}
		decl := rec.Manifest.Config
		if decl == nil {
			emitConfigResult(ipc, msg, false, "", fmt.Sprintf("service %q declares no config file", msg.ServiceID))
			return
		}
		allowedRoot := ""
		if svc, _ := config.FindServiceByID(ipc.Store.Get(), msg.ServiceID); svc != nil {
			allowedRoot = svc.WorkingDir
		}
		ipc.GoFunc(func() {
			realPath, info, err := service.ResolveConfigPath(decl, allowedRoot)
			if err != nil {
				emitConfigResult(ipc, msg, false, "", err.Error())
				return
			}
			data, err := service.ReadConfigFile(realPath, info)
			if err != nil {
				emitConfigResult(ipc, msg, false, "", err.Error())
				return
			}
			emitConfigResult(ipc, msg, true, string(data), "")
		})

	case configOpSave:
		if ipc.Ops == nil {
			emitConfigResult(ipc, msg, false, "", "service operations unavailable")
			return
		}
		ipc.GoFunc(func() { saveServiceConfig(ipc, msg) })

	default:
		emitConfigResult(ipc, msg, false, "", fmt.Sprintf("unsupported op %q (want get|save)", msg.Op))
	}
}

// The write and the restart are one queued ServiceOps step, so this only
// spells the outcome: a restart failure after a successful write reports the
// save as ok and the restart as the error.
func saveServiceConfig(ipc *IPCContext, msg ipcServiceConfigMsg) {
	res, err := ipc.Ops.SaveConfigFile(ipc.Ctx, msg.ServiceID, msg.Text)
	if err != nil && !errors.Is(err, errServiceProcess) {
		emitConfigResult(ipc, msg, false, "", err.Error())
		return
	}
	emitConfigResult(ipc, msg, true, "", "")
	if err != nil {
		dispatchEmit(ipc, "onServiceConfigApplied", map[string]interface{}{
			"serviceId": msg.ServiceID, "mode": "error", "error": err.Error(),
		})
		return
	}
	if !res.Restarted {
		dispatchEmit(ipc, "onServiceConfigApplied", map[string]interface{}{
			"serviceId": msg.ServiceID, "mode": "saved",
		})
		return
	}
	// The reaper Forgets the old manifest on Stop; the restarted service
	// re-registers on boot. Re-poll so the inspector reflects the new state.
	if ipc.PushServiceStatusBatch != nil {
		ipc.PushServiceStatusBatch()
	}
	dispatchEmit(ipc, "onServiceConfigApplied", map[string]interface{}{
		"serviceId": msg.ServiceID, "mode": "restarting",
	})
}

func emitConfigResult(ipc *IPCContext, msg ipcServiceConfigMsg, ok bool, text, errStr string) {
	if !ok {
		slog.Warn("service config op rejected", "service", msg.ServiceID, "op", msg.Op, "reason", errStr)
	}
	payload := map[string]interface{}{
		"serviceId": msg.ServiceID,
		"op":        msg.Op,
		"ok":        ok,
	}
	if text != "" {
		payload["text"] = text
	}
	if errStr != "" {
		payload["error"] = errStr
	}
	dispatchEmit(ipc, "onServiceConfigResult", payload)
}
