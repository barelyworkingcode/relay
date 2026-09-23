package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
	"github.com/barelyworkingcode/relay/internal/service"
)

// ---------------------------------------------------------------------------
// Settings window
// ---------------------------------------------------------------------------

func (a *App) openSettingsWindow() {
	s := config.DisplaySettings(a.store)
	// Consumed here so a code minted for THIS open never survives into the
	// next one: it is single use and two minutes old at best, and a stale one
	// reappearing would read as a code that still works.
	code := a.pendingLoginCode
	a.pendingLoginCode = nil
	// Consumed for the same reason the code above is: a page selected for
	// THIS open must not survive into the next one, or every subsequent
	// "Settings..." click reopens on a tab nobody asked for.
	page := a.pendingSettingsPage
	a.pendingSettingsPage = ""
	html := renderSettingsDocument(s, a.registry.RunningIDs(), a.buildToolCache(s), a.buildScopeFields(), code, page, a.buildOverviewSeed(s))
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
	a.emitSettingsEvent("onServiceStatus", serviceStatusEventPayload(a.registry))
}

// serviceStatusEventPayload is onServiceStatus's shared shape: running_ids
// and runtime carry whatever they always have, and supervision rides along
// the same way -- keyed by id, present only for a service relay is actively
// supervising the restart campaign of, so a Settings build that doesn't
// render it yet is unaffected.
func serviceStatusEventPayload(reg service.Manager) map[string]interface{} {
	return map[string]interface{}{
		"running_ids": reg.RunningIDs(),
		"runtime":     serviceRuntimeToNativeView(reg.Runtime()),
		"supervision": serviceSupervisionToNativeView(reg.SupervisionStatuses()),
	}
}

// pushFullSettings sends the complete settings state to an open settings window.
// Called when external changes (CLI commands via bridge) modify settings.json.
//
// mcp_tool_cache is included so the MCP Servers tab's tool counts (and the
// Projects picker) reflect the current live connection state after MCP
// adds/removes/auth — discovered_tools is runtime-only and never serialized
// on ExternalMcp itself.
func (a *App) pushFullSettings() {
	s := config.DisplaySettings(a.store)
	seed := a.buildOverviewSeed(s)
	a.emitSettingsEvent("onSettingsReloaded", map[string]interface{}{
		// Native-viewed, not the raw settings slices: config.Secret refuses to
		// marshal at all once it holds a real sealed value (Secret.MarshalJSON),
		// and even where an already-sealed record's envelope still marshals, the
		// JS side would receive that envelope object rather than a plaintext
		// string -- exactly the corruption a stale onSettingsReloaded broadcast
		// used to leave sitting in state.services right after a correctly
		// revealed onServiceAdded/onServiceUpdated had just rendered it right.
		"external_mcps":    externalMcpsToNativeView(s.ExternalMcps),
		"services":         serviceConfigsToNativeView(s.Services),
		"running_ids":      a.registry.RunningIDs(),
		"projects":         s.Projects,
		"mcp_tool_cache":   a.buildToolCache(s),
		"mcp_scope_fields": a.buildScopeFields(),
		// Enrolments and the remote block ride along so the Remote Clients tab
		// reflects an enrolment created or revoked by `relay enrol` while the
		// window is open. The audit state comes from the live recorder rather
		// than from settings, because a recorder that failed to start is the
		// same "remote is off" for the operator as one switched off on purpose.
		"enrolments": s.Enrolments,
		"remote":     remoteConfigViewOf(s, a.audit.Enabled()),
		// Passkeys and live login sessions ride along for the enrolments'
		// reason: a credential you cannot see is one you will not revoke, and
		// both change from outside this window — `relay login enrol|revoke`
		// in a terminal, and a browser completing the ceremony.
		"passkeys":       a.loginOps.Passkeys(),
		"login_sessions": a.loginOps.Sessions(),
		// Eve's mirror rides along for the same reason: a report or a
		// revoke made outside this window (eve's own poll, `relay eve
		// revoke`) must show up here without a manual reload.
		"eve_passkeys": a.evePasskeyOps.List(),
		// Overview tab. Rebuilt with the same helper openSettingsWindow's
		// first paint uses, so a reload mid-session and a fresh window never
		// disagree about MCP health, service runtime, or the seal/version
		// facts that don't change after boot.
		"mcp_health":      seed.MCPHealth,
		"service_runtime": seed.ServiceRuntime,
		"seal_status":     seed.SealStatus,
		"version":         seed.Version,
		"paths":           seed.Paths,
	})
}

// buildToolCache snapshots the live per-MCP tool list for first-paint of
// the Projects tab. Missing MCPs (not registered or not connected yet) are
// represented as empty slices so the UI renders consistently — the picker
// handles empties with an "authenticate this MCP first" hint.
func (a *App) buildToolCache(s *config.Settings) map[string][]config.ToolInfo {
	out := make(map[string][]config.ToolInfo, len(s.ExternalMcps))
	for _, m := range s.ExternalMcps {
		infos := a.extMgr.ToolInfos(m.ID)
		if infos == nil {
			infos = []config.ToolInfo{}
		}
		out[m.ID] = infos
	}
	return out
}

// buildScopeFields snapshots what each connected MCP declares as narrowable
// (ADR-011 decision 6). Keyed by MCP id; an MCP that declares no restrict
// fields gets an empty slice, and one relay has never connected to gets no key
// at all — which is what lets the editor say "relay cannot see what this MCP
// scopes" instead of "this MCP scopes nothing".
func (a *App) buildScopeFields() map[string][]project.ScopeFieldView {
	return a.extMgr.AllMcpSurfaces().ScopeFields()
}

// pushFullProjects sends only the projects slice. Used as the
// ProjectsChangedFn callback from the frontend HTTP server — when Eve, the
// scheduler, or the CLI mutates a project the in-tray UI re-renders.
// Cheaper than pushFullSettings when only projects changed.
func (a *App) pushFullProjects() {
	a.emitSettingsEvent("onProjectsReloaded", config.DisplaySettings(a.store).Projects)
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
	Ctx                    context.Context
	Store                  config.SettingsStore
	UI                     SettingsUI
	Platform               Platform
	Registry               service.Manager
	Enhanced               *EnhancedServiceRegistry
	UpdateMenu             func()
	PushServiceStatusBatch func()          // re-poll and emit after an action lands
	GoFunc                 func(fn func()) // tracked goroutine launcher
	NotifyReconcile        func(string) error
	NotifyReloadMcp        func(id, secret string) error
	// Tools is the live tool registry used by the Projects tab tri-state
	// picker. nil means "no tool data available" — handlers degrade by
	// emitting empty lists rather than panicking.
	Tools MCPToolsProvider
	// Enumerate asks a connected MCP for a scope field's real values
	// (ADR-011 decision 6), so the Projects tab can offer a picker instead of
	// a box. nil means every enumeration answers "unavailable", which is the
	// state the editor already has to render correctly — the text box is the
	// fallback, so a missing provider costs nothing but the picker.
	Enumerate project.ContextEnumerator
	// SkillLister is the same interface skills.go uses; threaded here so the
	// Regen Now button can run without re-importing *appRouter.
	SkillLister SkillLister
	// Audit backs the Tool Calls tab. Nil when auditing is off; every method
	// on the recorder is nil-safe, so handlers don't guard on it.
	Audit *audit.AuditRecorder
	// Ops is the shared core behind both the Services tab and
	// RegisterServiceRoutes (ADR-014) — the IPC handlers in ipc_services.go
	// are thin adapters over it.
	Ops *ServiceOps
	// EnrolmentOps is Ops's counterpart for the Remote Clients tab and
	// RegisterEnrolmentRoutes (ADR-014) — ipc_enrolments.go's handlers are
	// thin adapters over it too.
	EnrolmentOps *EnrolmentOps
	// AuditOps is Ops's counterpart for the Tool Calls tab and
	// RegisterAuditRoutes (ADR-014) — ipc_audit.go's handlers are thin
	// adapters over it too.
	AuditOps *audit.AuditOps
	// LoginOps is Ops's counterpart for the Passkeys tab (ADR-016) —
	// ipc_login.go's handlers are thin adapters over it. Unlike the others
	// it has no HTTP door at all: registering and revoking a passkey is a
	// host-side act, and giving it a route would be the self-service
	// enrolment ADR-010 decision 8 and ADR-016 decision 2 both refuse.
	LoginOps *LoginOps
	// EvePasskeyOps is LoginOps' counterpart for the Passkeys tab's eve
	// section (docs/eve-passkey-enrolment.md) -- ipc_eve_passkeys.go's
	// handler is a thin adapter over it. Like LoginOps it has no route on
	// this server's own HTTP door beyond eve's own PUT/GET pair, which is
	// registered separately (RegisterEvePasskeyRoutes).
	EvePasskeyOps *EvePasskeyOps
	// McpOps is Ops's counterpart for the MCPs tab and RegisterMcpRoutes
	// (ADR-014) — ipc_mcps.go's and ipc_mcp_permissions.go's handlers are
	// thin adapters over it too. Unlike Ops and EnrolmentOps, two of its
	// four IPC commands (authenticate, reset-permissions) have no HTTP
	// counterpart; see McpOps for why.
	McpOps *McpOps
	// ProjectOps is Ops's counterpart for the Projects tab (ADR-014) —
	// ipc_projects.go's handlers are thin adapters over it too, so a
	// project created from curl and one created from the tray share the
	// presence gate and the audit record (ADR-017 decisions 2 and 3).
	ProjectOps *ProjectOps
	// HostOps is Ops's counterpart for the Hosts tab and RegisterHostRoutes
	// (docs/ssh-hosts.md) — ipc_hosts.go's handlers are thin adapters over
	// it too, so a host created from curl and one created from the tray
	// share the same probe and the same audit record.
	HostOps     *HostOps
	TemplateOps *TemplateOps
	// HostTemplateOps backs the Hosts tab's per-host template list
	// (ipc_host_templates.go) and shares the tray's command queue.
	HostTemplateOps *HostTemplateOps
	// ConfigDir and LogsDir back the Overview tab's "Reveal" actions
	// (ipc_overview.go). ConfigDir is a plain string because it is fixed at
	// boot; LogsDir is a func because resolving it can fail (directory
	// creation) the way ConfigDir never does.
	ConfigDir string
	LogsDir   func() (string, error)
}

// refreshServiceUI emits current service status and rebuilds the tray menu.
// Must be called on the main thread.
func (ctx *IPCContext) refreshServiceUI() {
	ctx.UI.EmitEvent("onServiceStatus", serviceStatusEventPayload(ctx.Registry))
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
	TccServices []string          `json:"tcc_services"`
}

type ipcIDMsg struct {
	ID string `json:"id"`
}

// ipcServiceMsg is the shared message format for add and update service
// operations. For add: ID is empty (derived from DisplayName). For update:
// ID is required. Env's value type is *string, matching serviceFields.Env
// -- see that field's own comment for what null versus an omitted key means.
type ipcServiceMsg struct {
	ID           string                      `json:"id"`
	DisplayName  string                      `json:"display_name"`
	Command      string                      `json:"command"`
	Args         []string                    `json:"args"`
	Env          map[string]*string          `json:"env"`
	WorkingDir   string                      `json:"working_dir,omitempty"`
	Autostart    bool                        `json:"autostart"`
	URL          string                      `json:"url,omitempty"`
	Capabilities *[]config.ServiceCapability `json:"capabilities,omitempty"`
	// AllowedModels is a pointer for the same reason Capabilities is: the
	// Settings window omits this key entirely when the Allowed Models
	// section was never shown (the models capability off), and an absent
	// key must leave the stored grant alone rather than clear it.
	AllowedModels *[]string `json:"allowed_models,omitempty"`
}

type ipcUpdateServiceAutostartMsg struct {
	ID        string `json:"id"`
	Autostart bool   `json:"autostart"`
}

// ---------------------------------------------------------------------------
// IPC message type constants — single source of truth for the JS/Go contract.
// ---------------------------------------------------------------------------

const (
	MsgAddExternalMcp      = "add_external_mcp"
	MsgAuthenticateMcp     = "authenticate_mcp"
	MsgRemoveExternalMcp   = "remove_external_mcp"
	MsgResetMcpPermissions = "reset_mcp_permissions"

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
	MsgEnumerateScopeField        = "enumerate_scope_field"

	// Tool Calls / audit log (ipc_audit.go)
	MsgQueryAudit     = "query_audit"
	MsgExportAudit    = "export_audit"
	MsgRevealAuditLog = "reveal_audit_log"

	// Hosts (ipc_hosts.go)
	MsgListHosts      = "list_hosts"
	MsgCreateHost     = "create_host"
	MsgUpdateHost     = "update_host"
	MsgRemoveHost     = "remove_host"
	MsgProbeHost      = "probe_host"
	MsgDisconnectHost = "disconnect_host"

	// Overview (ipc_overview.go)
	MsgRevealConfigDir  = "reveal_config_dir"
	MsgRevealLogsDir    = "reveal_logs_dir"
	MsgRevealServiceLog = "reveal_service_log"

	// Templates (ipc_templates.go)
	MsgListTemplates  = "list_templates"
	MsgCreateTemplate = "create_template"
	MsgUpdateTemplate = "update_template"
	MsgRemoveTemplate = "remove_template"

	// Host templates (ipc_host_templates.go)
	MsgListHostTemplates  = "list_host_templates"
	MsgCreateHostTemplate = "create_host_template"
	MsgUpdateHostTemplate = "update_host_template"
	MsgRemoveHostTemplate = "remove_host_template"
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
	MsgEnumerateScopeField:        ipcEnumerateScopeField,

	// Tool Calls (ipc_audit.go)
	MsgQueryAudit:     ipcQueryAudit,
	MsgExportAudit:    ipcExportAudit,
	MsgRevealAuditLog: ipcRevealAuditLog,

	// Remote Clients (ipc_enrolments.go)
	MsgCreateEnrolment:         ipcCreateEnrolment,
	MsgRevokeEnrolment:         ipcRevokeEnrolment,
	MsgUpdateRemoteConfig:      ipcUpdateRemoteConfig,
	MsgListEnrolmentRequests:   ipcListEnrolmentRequests,
	MsgApproveEnrolmentRequest: ipcApproveEnrolmentRequest,
	MsgRefuseEnrolmentRequest:  ipcRefuseEnrolmentRequest,

	// Passkeys (ipc_login.go)
	MsgListPasskeys:     ipcListPasskeys,
	MsgRevokePasskey:    ipcRevokePasskey,
	MsgSignOutLogin:     ipcSignOutLogin,
	MsgRevokeEvePasskey: ipcRevokeEvePasskey,

	// Hosts (ipc_hosts.go)
	MsgListHosts:      ipcListHosts,
	MsgCreateHost:     ipcCreateHost,
	MsgUpdateHost:     ipcUpdateHost,
	MsgRemoveHost:     ipcRemoveHost,
	MsgProbeHost:      ipcProbeHost,
	MsgDisconnectHost: ipcDisconnectHost,

	// Overview (ipc_overview.go)
	MsgRevealConfigDir:  ipcRevealConfigDir,
	MsgRevealLogsDir:    ipcRevealLogsDir,
	MsgRevealServiceLog: ipcRevealServiceLog,

	// Templates (ipc_templates.go)
	MsgListTemplates:  ipcListTemplates,
	MsgCreateTemplate: ipcCreateTemplate,
	MsgUpdateTemplate: ipcUpdateTemplate,
	MsgRemoveTemplate: ipcRemoveTemplate,

	// Host templates (ipc_host_templates.go)
	MsgListHostTemplates:  ipcListHostTemplates,
	MsgCreateHostTemplate: ipcCreateHostTemplate,
	MsgUpdateHostTemplate: ipcUpdateHostTemplate,
	MsgRemoveHostTemplate: ipcRemoveHostTemplate,
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
