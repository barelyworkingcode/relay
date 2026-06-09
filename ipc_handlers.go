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
	html := renderSettingsHTML(s, a.registry.RunningIDs(), a.buildToolCache(s))
	a.platform.OpenSettings(html)
	a.settingsOpen.Store(true)
	// First paint shouldn't wait the full 2s poll interval. pushServiceStatusBatch
	// handles its own main-thread hop for the WebView emit; the HTTP polling
	// stays off-main so it can't block the UI on a slow service.
	a.goFunc(a.pushServiceStatusBatch)
}

func (a *App) onSettingsClose() {
	a.settingsOpen.Store(false)
}

func (a *App) evalSettings(js string) {
	if a.settingsOpen.Load() {
		a.platform.EvalSettingsJS(js)
	}
}

// emitSettingsEvent sends a named event to the settings UI with JSON-marshaled arguments.
// Each arg is marshaled individually; json.RawMessage values are passed through as-is.
// This centralizes JS escaping and marshaling.
func (a *App) emitSettingsEvent(name string, args ...interface{}) {
	if !a.settingsOpen.Load() {
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
//
// mcp_tool_cache is included so the MCP Servers tab's tool counts (and the
// Projects picker) reflect the current live connection state after MCP
// adds/removes/auth — discovered_tools is runtime-only and never serialized
// on ExternalMcp itself.
func (a *App) pushFullSettings() {
	s := a.store.Get()
	a.emitSettingsEvent("onSettingsReloaded", map[string]interface{}{
		"external_mcps":  s.ExternalMcps,
		"services":       s.Services,
		"running_ids":    a.registry.RunningIDs(),
		"projects":       s.Projects,
		"mcp_tool_cache": a.buildToolCache(s),
	})
}

// buildToolCache snapshots the live per-MCP tool list for first-paint of
// the Projects tab. Missing MCPs (not registered or not connected yet) are
// represented as empty slices so the UI renders consistently — the picker
// handles empties with an "authenticate this MCP first" hint.
func (a *App) buildToolCache(s *Settings) map[string][]ToolInfo {
	out := make(map[string][]ToolInfo, len(s.ExternalMcps))
	for _, m := range s.ExternalMcps {
		infos := a.extMgr.ToolInfos(m.ID)
		if infos == nil {
			infos = []ToolInfo{}
		}
		out[m.ID] = infos
	}
	return out
}

// pushFullProjects sends only the projects slice. Used as the
// ProjectsChangedFn callback from the frontend HTTP server — when Eve, the
// scheduler, or the CLI mutates a project the in-tray UI re-renders.
// Cheaper than pushFullSettings when only projects changed.
func (a *App) pushFullProjects() {
	a.emitSettingsEvent("onProjectsReloaded", a.store.Get().Projects)
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
	Ctx                     context.Context
	Store                   SettingsStore
	UI                      SettingsUI
	Platform                Platform
	Registry                ServiceManager
	Enhanced                *EnhancedServiceRegistry
	UpdateMenu              func()
	PushServiceStatusBatch  func() // re-poll and emit after an action lands
	GoFunc                  func(fn func()) // tracked goroutine launcher
	NotifyReconcile         func(string) error
	NotifyReloadMcp         func(id, secret string) error
	// Tools is the live tool registry used by the Projects tab tri-state
	// picker. nil means "no tool data available" — handlers degrade by
	// emitting empty lists rather than panicking.
	Tools                   MCPToolsProvider
	// SkillLister is the same interface skills.go uses; threaded here so the
	// Regen Now button can run without re-importing *appRouter.
	SkillLister             SkillLister
}

// withSettingsReconcile atomically mutates settings, then asynchronously sends
// a reconcile notification to the bridge. Returns true on success (save succeeded).
func (ctx *IPCContext) withSettingsReconcile(fn func(*Settings)) bool {
	return ctx.withSettingsNotify(fn, ctx.NotifyReconcile)
}

// withSettings atomically mutates settings and emits an error event on failure.
// Returns true on success.
func (ctx *IPCContext) withSettings(fn func(*Settings)) bool {
	return ctx.withSettingsNotify(fn, nil)
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
	if notify != nil {
		ctx.GoFunc(func() {
			if err := notify(secret); err != nil {
				slog.Warn("bridge notification failed", "error", err)
			}
		})
	}
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
	MsgAddExternalMcp        = "add_external_mcp"
	MsgAuthenticateMcp       = "authenticate_mcp"
	MsgRemoveExternalMcp     = "remove_external_mcp"
	MsgResetMcpPermissions   = "reset_mcp_permissions"

	MsgAddService             = "add_service"
	MsgRemoveService          = "remove_service"
	MsgUpdateService          = "update_service"
	MsgUpdateServiceAutostart = "update_service_autostart"
	MsgStartService           = "start_service"
	MsgStopService            = "stop_service"

	// Projects (ipc_projects.go)
	MsgCreateProject              = "create_project"
	MsgUpdateProject              = "update_project"
	MsgRemoveProject              = "remove_project"
	MsgRotateProjectToken         = "rotate_project_token"
	MsgRegenProjectSkill          = "regen_project_skill"
	MsgUpdateProjectDisabledTools = "update_project_disabled_tools"
	MsgListMcpTools               = "list_mcp_tools"
)

// ---------------------------------------------------------------------------
// IPC dispatch
// ---------------------------------------------------------------------------

// ipcHandlers maps message types to handler functions.
var ipcHandlers = map[string]func(*IPCContext, json.RawMessage){
	// External MCPs (ipc_mcps.go, ipc_mcp_permissions.go)
	MsgAddExternalMcp:      ipcAddExternalMcp,
	MsgAuthenticateMcp:     ipcAuthenticateMcp,
	MsgRemoveExternalMcp:   ipcRemoveExternalMcp,
	MsgResetMcpPermissions: ipcResetMcpPermissions,

	// Services (ipc_services.go)
	MsgAddService:             ipcAddService,
	MsgRemoveService:          ipcRemoveService,
	MsgUpdateService:          ipcUpdateService,
	MsgUpdateServiceAutostart: ipcUpdateServiceAutostart,
	MsgStartService:           ipcStartService,
	MsgStopService:            ipcStopService,

	// Service Inspector (ipc_service_action.go, ipc_service_config.go)
	MsgServiceAction: ipcServiceAction,
	MsgServiceConfig: ipcServiceConfig,

	// Projects (ipc_projects.go)
	MsgCreateProject:              ipcCreateProject,
	MsgUpdateProject:              ipcUpdateProject,
	MsgRemoveProject:              ipcRemoveProject,
	MsgRotateProjectToken:         ipcRotateProjectToken,
	MsgRegenProjectSkill:          ipcRegenProjectSkill,
	MsgUpdateProjectDisabledTools: ipcUpdateProjectDisabledTools,
	MsgListMcpTools:               ipcListMcpTools,
}

// onSettingsIpc is called from the WKWebView IPC handler.
// The message body is a JSON string from the ipc() wrapper.
func (a *App) onSettingsIpc(body string) {
	raw := json.RawMessage(body)
	var msg ipcMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Warn("failed to unmarshal IPC message", "error", err)
		return
	}

	handler, ok := ipcHandlers[msg.Type]
	if !ok {
		slog.Warn("unknown IPC message type", "type", msg.Type)
		return
	}
	handler(a.ipcCtx, raw)
}

