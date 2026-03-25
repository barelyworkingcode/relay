package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// ---------------------------------------------------------------------------
// Settings window
// ---------------------------------------------------------------------------

func (a *App) openSettingsWindow() {
	s := a.store.Get()
	html := renderSettingsHTML(s, a.registry.RunningIDs())
	a.platform.OpenSettings(html)
	a.settingsOpen = true
}

func (a *App) onSettingsClose() {
	a.settingsOpen = false
}

func (a *App) evalSettings(js string) {
	if a.settingsOpen {
		a.platform.EvalSettingsJS(js)
	}
}

// emitSettingsEvent sends a named event to the settings UI with JSON-marshaled arguments.
// Each arg is marshaled individually; json.RawMessage values are passed through as-is.
// This centralizes JS escaping and marshaling.
func (a *App) emitSettingsEvent(name string, args ...interface{}) {
	if !a.settingsOpen {
		return
	}
	if len(args) == 0 {
		a.platform.EvalSettingsJS(name + "()")
		return
	}
	var jsArgs []string
	for _, arg := range args {
		if raw, ok := arg.(json.RawMessage); ok {
			jsArgs = append(jsArgs, string(raw))
		} else {
			data, err := json.Marshal(arg)
			if err != nil {
				slog.Error("failed to marshal settings event arg, skipping event", "event", name, "argIndex", len(jsArgs), "error", err)
				return
			}
			jsArgs = append(jsArgs, string(data))
		}
	}
	a.platform.EvalSettingsJS(fmt.Sprintf("%s(%s)", name, strings.Join(jsArgs, ",")))
}

func (a *App) pushServiceStatus() {
	a.emitSettingsEvent("onServiceStatus", a.registry.RunningIDs())
}

// pushFullSettings sends the complete settings state to an open settings window.
// Called when external changes (CLI commands via bridge) modify settings.json.
func (a *App) pushFullSettings() {
	s := a.store.Get()
	a.emitSettingsEvent("onSettingsReloaded", map[string]interface{}{
		"tokens":        s.Tokens,
		"external_mcps": s.ExternalMcps,
		"services":      s.Services,
		"running_ids":   a.registry.RunningIDs(),
	})
}

// ---------------------------------------------------------------------------
// IPC decoupling types
// ---------------------------------------------------------------------------

// SettingsUI emits events to the settings window.
type SettingsUI interface {
	EmitEvent(name string, args ...interface{})
}

// EmitEvent implements SettingsUI for App.
func (a *App) EmitEvent(name string, args ...interface{}) {
	a.emitSettingsEvent(name, args...)
}

// IPCContext provides dependencies to IPC handlers, replacing *App coupling.
type IPCContext struct {
	Ctx              context.Context
	Store            SettingsStore
	UI               SettingsUI
	Platform         Platform
	Registry         ServiceManager
	UpdateMenu       func()
	GoFunc           func(fn func()) // tracked goroutine launcher
	NotifyReconcile  func(string) error
	NotifyReloadMcp  func(id, secret string) error
}

// withSettingsReconcile atomically mutates settings, then asynchronously sends
// a reconcile notification to the bridge. Returns true on success (save succeeded).
func (ctx *IPCContext) withSettingsReconcile(fn func(*Settings)) bool {
	return ctx.withSettingsNotify(fn, ctx.NotifyReconcile)
}

// withSettings atomically mutates settings and emits an error event on failure.
// Returns true on success.
func (ctx *IPCContext) withSettings(fn func(*Settings)) bool {
	if err := ctx.Store.With(fn); err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
		return false
	}
	return true
}

// withSettingsNotify atomically mutates settings, then dispatches a bridge
// notification asynchronously via GoFunc. This avoids blocking the main/UI
// thread during bridge round-trips that may trigger MCP process spawning or
// network I/O. Settings are persisted to disk before the notification is sent,
// so the notification handler reads the updated state.
func (ctx *IPCContext) withSettingsNotify(fn func(*Settings), notify func(string) error) bool {
	var secret string
	if err := ctx.Store.With(func(s *Settings) {
		fn(s)
		secret = s.AdminSecret
	}); err != nil {
		ctx.UI.EmitEvent("onSettingsError", err.Error())
		return false
	}
	ctx.GoFunc(func() {
		if err := notify(secret); err != nil {
			slog.Warn("bridge notification failed", "error", err)
		}
	})
	return true
}

// refreshServiceUI emits current service status and rebuilds the tray menu.
// Must be called on the main thread.
func (ctx *IPCContext) refreshServiceUI() {
	ctx.UI.EmitEvent("onServiceStatus", ctx.Registry.RunningIDs())
	ctx.UpdateMenu()
}

// ---------------------------------------------------------------------------
// IPC message types
// ---------------------------------------------------------------------------

type ipcMsg struct {
	Type string `json:"type"`
}

type ipcGenerateTokenMsg struct {
	Name string `json:"name"`
}

type ipcTokenHash struct {
	Hash string `json:"hash"`
}

type ipcUpdatePermissionMsg struct {
	Hash       string `json:"hash"`
	Service    string `json:"service"`
	Permission string `json:"permission"`
}

type ipcSetToolDisabledMsg struct {
	Hash     string `json:"hash"`
	McpID    string `json:"mcp_id"`
	ToolName string `json:"tool_name"`
	Disabled bool   `json:"disabled"`
}

type ipcSetAllToolsDisabledMsg struct {
	Hash     string `json:"hash"`
	McpID    string `json:"mcp_id"`
	Disabled bool   `json:"disabled"`
}

type ipcSetContextMsg struct {
	Hash    string      `json:"hash"`
	McpID   string      `json:"mcp_id"`
	Context interface{} `json:"context"`
}

type ipcAddExternalMcpMsg struct {
	DisplayName string            `json:"display_name"`
	Transport   string            `json:"transport"`
	URL         string            `json:"url"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
}

type ipcIDMsg struct {
	ID string `json:"id"`
}

type ipcCopyToClipboardMsg struct {
	Text string `json:"text"`
}

// ipcServiceMsg is the shared message format for add and update service operations.
// For add: ID is empty (derived from DisplayName). For update: ID is required.
type ipcServiceMsg struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"display_name"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	WorkingDir  string            `json:"working_dir,omitempty"`
	Autostart   bool              `json:"autostart"`
	URL         string            `json:"url,omitempty"`
}

type ipcUpdateServiceAutostartMsg struct {
	ID        string `json:"id"`
	Autostart bool   `json:"autostart"`
}

// ---------------------------------------------------------------------------
// IPC message type constants — single source of truth for the JS/Go contract.
// ---------------------------------------------------------------------------

const (
	MsgGenerateToken       = "generate_token"
	MsgDeleteToken         = "delete_token"
	MsgRevokeAll           = "revoke_all"
	MsgUpdatePermission    = "update_permission"
	MsgSetToolDisabled     = "set_tool_disabled"
	MsgSetAllToolsDisabled = "set_all_tools_disabled"
	MsgSetContext          = "set_context"

	MsgAddExternalMcp    = "add_external_mcp"
	MsgAuthenticateMcp   = "authenticate_mcp"
	MsgRemoveExternalMcp = "remove_external_mcp"

	MsgAddService             = "add_service"
	MsgRemoveService          = "remove_service"
	MsgUpdateService          = "update_service"
	MsgUpdateServiceAutostart = "update_service_autostart"
	MsgStartService           = "start_service"
	MsgStopService            = "stop_service"

	MsgCopyToClipboard = "copy_to_clipboard"
)

// ---------------------------------------------------------------------------
// IPC dispatch
// ---------------------------------------------------------------------------

// ipcHandlers maps message types to handler functions.
var ipcHandlers = map[string]func(*IPCContext, json.RawMessage){
	// Tokens & permissions (ipc_tokens.go)
	MsgGenerateToken:       ipcGenerateToken,
	MsgDeleteToken:         ipcDeleteToken,
	MsgRevokeAll:           ipcRevokeAll,
	MsgUpdatePermission:    ipcUpdatePermission,
	MsgSetToolDisabled:     ipcSetToolDisabled,
	MsgSetAllToolsDisabled: ipcSetAllToolsDisabled,
	MsgSetContext:          ipcSetContext,

	// External MCPs (ipc_mcps.go)
	MsgAddExternalMcp:    ipcAddExternalMcp,
	MsgAuthenticateMcp:   ipcAuthenticateMcp,
	MsgRemoveExternalMcp: ipcRemoveExternalMcp,

	// Services (ipc_services.go)
	MsgAddService:             ipcAddService,
	MsgRemoveService:          ipcRemoveService,
	MsgUpdateService:          ipcUpdateService,
	MsgUpdateServiceAutostart: ipcUpdateServiceAutostart,
	MsgStartService:           ipcStartService,
	MsgStopService:            ipcStopService,

	// Utility
	MsgCopyToClipboard: ipcCopyToClipboard,
}

// onSettingsIpc is called from the WKWebView IPC handler.
// The message body is a JSON string from the ipc() wrapper.
func (a *App) onSettingsIpc(body string) {
	raw := json.RawMessage(body)
	var msg ipcMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	handler, ok := ipcHandlers[msg.Type]
	if !ok {
		slog.Warn("unknown IPC message type", "type", msg.Type)
		return
	}
	handler(a.ipcCtx, raw)
}

// ---------------------------------------------------------------------------
// Utility handlers
// ---------------------------------------------------------------------------

func ipcCopyToClipboard(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcCopyToClipboardMsg](raw, "copy_to_clipboard")
	if !ok || msg.Text == "" {
		return
	}
	ctx.Platform.CopyToClipboard(msg.Text)
}
