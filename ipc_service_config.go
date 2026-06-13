package main

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"relaygo/bridge"
)

// ipcServiceConfigMsg drives the Service Inspector's config editor. The
// manifest is the authority: relay refuses get/save for any service that did
// not declare a Config. The file is opaque text on the wire — relay validates
// that it parses and is within the size cap, but never interprets its shape
// (the schema-driven form lives in the UI).
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

// ipcServiceConfig handles a config get/save for one enhanced service. Path
// resolution, reads, and writes go through resolveConfigPath (the single
// security gate) and run off the UI thread — a save also restarts the service,
// which blocks on process exit.
func ipcServiceConfig(ipc *IPCContext, raw json.RawMessage) {
	var msg ipcServiceConfigMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		dispatchEmit(ipc, "onServiceConfigResult", map[string]interface{}{
			"ok":    false,
			"error": fmt.Sprintf("decode config op: %v", err),
		})
		return
	}

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

	// allowedRoot = the service's configured working directory; the gate
	// confines reads/writes to it. Resolved synchronously (cheap, in-memory).
	allowedRoot := ""
	if svc, _ := ipc.Store.Get().findServiceByID(msg.ServiceID); svc != nil {
		allowedRoot = svc.WorkingDir
	}

	switch msg.Op {
	case configOpGet:
		ipc.GoFunc(func() {
			realPath, info, err := resolveConfigPath(decl, allowedRoot)
			if err != nil {
				emitConfigResult(ipc, msg, false, "", err.Error())
				return
			}
			data, err := readConfigFile(realPath, info)
			if err != nil {
				emitConfigResult(ipc, msg, false, "", err.Error())
				return
			}
			emitConfigResult(ipc, msg, true, string(data), "")
		})

	case configOpSave:
		ipc.GoFunc(func() {
			// Validate BEFORE resolving/writing so a malformed edit never
			// touches the file (load-bearing safety property).
			if err := validateConfigText([]byte(msg.Text), decl.Format); err != nil {
				emitConfigResult(ipc, msg, false, "", "config does not parse: "+err.Error())
				return
			}
			realPath, info, err := resolveConfigPath(decl, allowedRoot)
			if err != nil {
				emitConfigResult(ipc, msg, false, "", err.Error())
				return
			}
			// Write the ORIGINAL edited bytes (not a re-marshal) so comments and
			// key order survive on disk; preserve the file's existing mode (taken
			// from the same FileInfo we validated, avoiding a re-stat race).
			if err := writeConfigFile(realPath, []byte(msg.Text), info.Mode().Perm()); err != nil {
				emitConfigResult(ipc, msg, false, "", err.Error())
				return
			}
			emitConfigResult(ipc, msg, true, "", "")

			if decl.ApplyMode == bridge.ConfigApplyLive {
				// Service watches the file itself; nothing to restart.
				dispatchEmit(ipc, "onServiceConfigApplied", map[string]interface{}{
					"serviceId": msg.ServiceID, "mode": "saved",
				})
				return
			}
			restartServiceForConfig(ipc, msg.ServiceID)
		})

	default:
		emitConfigResult(ipc, msg, false, "", fmt.Sprintf("unsupported op %q (want get|save)", msg.Op))
	}
}

// restartServiceForConfig restarts a running service so it re-reads a freshly
// saved config file, mirroring the off-thread reload in ipcUpdateService. Must
// be called off-main (Reload→Stop blocks on process exit) — it already is, from
// ipcServiceConfig's goroutine.
func restartServiceForConfig(ipc *IPCContext, id string) {
	if !ipc.Registry.IsRunning(id) {
		// Not running — the new config loads on the next manual start.
		dispatchEmit(ipc, "onServiceConfigApplied", map[string]interface{}{
			"serviceId": id, "mode": "saved",
		})
		return
	}
	svc, _ := ipc.Store.Get().findServiceByID(id)
	if svc == nil {
		dispatchEmit(ipc, "onServiceConfigApplied", map[string]interface{}{
			"serviceId": id, "mode": "saved",
		})
		return
	}
	cfg := *svc
	if err := ipc.Registry.Reload(id, &cfg); err != nil {
		slog.Error("service restart after config save failed", "id", id, "error", err)
		dispatchEmit(ipc, "onServiceConfigApplied", map[string]interface{}{
			"serviceId": id, "mode": "error",
			"error": fmt.Sprintf("config saved but restart failed: %v", err),
		})
		return
	}
	// The reaper Forgets the old manifest on Stop; the restarted service
	// re-registers on boot. Re-poll so the inspector reflects the new state.
	if ipc.PushServiceStatusBatch != nil {
		ipc.PushServiceStatusBatch()
	}
	dispatchEmit(ipc, "onServiceConfigApplied", map[string]interface{}{
		"serviceId": id, "mode": "restarting",
	})
}

// emitConfigResult sends a get/save outcome back to the WebView. text carries
// the file contents on a successful get; it is empty otherwise.
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
