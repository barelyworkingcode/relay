package main

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"relaygo/bridge"
)

// ---------------------------------------------------------------------------
// Settings window
// ---------------------------------------------------------------------------

func (a *App) openSettingsWindow() {
	s := LoadSettings()
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

func (a *App) pushServiceStatus() {
	if !a.settingsOpen {
		return
	}
	data, err := json.Marshal(a.registry.RunningIDs())
	if err != nil {
		slog.Error("failed to marshal service status", "error", err)
		return
	}
	a.evalSettings(fmt.Sprintf("onServiceStatus(%s)", string(data)))
}

// ---------------------------------------------------------------------------
// IPC message types
// ---------------------------------------------------------------------------

type ipcMsg struct {
	Type string `json:"type"`
}

type ipcGenerateToken struct {
	Name string `json:"name"`
}

type ipcTokenHash struct {
	Hash string `json:"hash"`
}

type ipcUpdatePermission struct {
	Hash       string `json:"hash"`
	Service    string `json:"service"`
	Permission string `json:"permission"`
}

type ipcSetToolDisabled struct {
	Hash     string `json:"hash"`
	McpID    string `json:"mcp_id"`
	ToolName string `json:"tool_name"`
	Disabled bool   `json:"disabled"`
}

type ipcSetAllToolsDisabled struct {
	Hash     string `json:"hash"`
	McpID    string `json:"mcp_id"`
	Disabled bool   `json:"disabled"`
}

type ipcSetContext struct {
	Hash    string      `json:"hash"`
	McpID   string      `json:"mcp_id"`
	Context interface{} `json:"context"`
}

type ipcAddExternalMcp struct {
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

type ipcCopyToClipboard struct {
	Text string `json:"text"`
}

type ipcServiceMsg struct {
	DisplayName string            `json:"display_name"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	WorkingDir  string            `json:"working_dir"`
	Autostart   bool              `json:"autostart"`
	URL         string            `json:"url"`
}

type ipcUpdateServiceMsg struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"display_name"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	WorkingDir  string            `json:"working_dir"`
	Autostart   bool              `json:"autostart"`
	URL         string            `json:"url"`
}

type ipcUpdateServiceAutostart struct {
	ID        string `json:"id"`
	Autostart bool   `json:"autostart"`
}

// ---------------------------------------------------------------------------
// IPC dispatch
// ---------------------------------------------------------------------------

// ipcHandlers maps message types to handler functions.
var ipcHandlers map[string]func(*App, json.RawMessage)

func init() {
	ipcHandlers = map[string]func(*App, json.RawMessage){
		"generate_token":          (*App).ipcGenerateToken,
		"delete_token":            (*App).ipcDeleteToken,
		"revoke_all":              (*App).ipcRevokeAll,
		"update_permission":       (*App).ipcUpdatePermission,
		"set_tool_disabled":       (*App).ipcSetToolDisabled,
		"set_all_tools_disabled":  (*App).ipcSetAllToolsDisabled,
		"set_context":             (*App).ipcSetContext,
		"add_external_mcp":        (*App).ipcAddExternalMcp,
		"authenticate_mcp":        (*App).ipcAuthenticateMcp,
		"remove_external_mcp":     (*App).ipcRemoveExternalMcp,
		"copy_to_clipboard":       (*App).ipcCopyToClipboard,
		"add_service":             (*App).ipcAddService,
		"remove_service":          (*App).ipcRemoveService,
		"update_service":          (*App).ipcUpdateService,
		"update_service_autostart": (*App).ipcUpdateServiceAutostart,
		"start_service":           (*App).ipcStartService,
		"stop_service":            (*App).ipcStopService,
	}
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
	handler(a, raw)
}

// ---------------------------------------------------------------------------
// IPC handlers
// ---------------------------------------------------------------------------

func (a *App) ipcGenerateToken(raw json.RawMessage) {
	var msg ipcGenerateToken
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	var plaintext string
	var stored StoredToken
	WithSettings(func(s *Settings) {
		defaultPerms := make(map[string]Permission)
		for _, svcName := range s.AllExternalMcpIDs() {
			defaultPerms[svcName] = PermOff
		}
		plaintext, stored = GenerateToken(msg.Name, defaultPerms)
		s.Tokens = append(s.Tokens, stored)
	})

	response := map[string]interface{}{
		"plaintext": plaintext,
		"token":     stored,
	}
	responseJSON, err := json.Marshal(response)
	if err != nil {
		slog.Error("failed to marshal token response", "error", err)
		return
	}
	a.evalSettings(fmt.Sprintf("onTokenGenerated(%s)", string(responseJSON)))
}

func (a *App) ipcDeleteToken(raw json.RawMessage) {
	var msg ipcTokenHash
	if err := json.Unmarshal(raw, &msg); err != nil || msg.Hash == "" {
		return
	}
	WithSettings(func(s *Settings) { s.DeleteToken(msg.Hash) })
	a.evalSettings(fmt.Sprintf("onTokenDeleted('%s')", escapeJSString(msg.Hash)))
}

func (a *App) ipcRevokeAll(_ json.RawMessage) {
	WithSettings(func(s *Settings) { s.RevokeAll() })
	a.evalSettings("onAllRevoked()")
}

func (a *App) ipcUpdatePermission(raw json.RawMessage) {
	var msg ipcUpdatePermission
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	perm := PermOn
	if msg.Permission == "off" {
		perm = PermOff
	}
	WithSettings(func(s *Settings) { s.UpdatePermission(msg.Hash, msg.Service, perm) })
}

func (a *App) ipcSetToolDisabled(raw json.RawMessage) {
	var msg ipcSetToolDisabled
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	WithSettings(func(s *Settings) { s.SetToolDisabled(msg.Hash, msg.McpID, msg.ToolName, msg.Disabled) })
}

func (a *App) ipcSetAllToolsDisabled(raw json.RawMessage) {
	var msg ipcSetAllToolsDisabled
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	WithSettings(func(s *Settings) {
		var toolNames []string
		for _, mcp := range s.ExternalMcps {
			if mcp.ID == msg.McpID {
				for _, t := range mcp.DiscoveredTools {
					toolNames = append(toolNames, t.Name)
				}
				break
			}
		}
		s.SetAllToolsDisabled(msg.Hash, msg.McpID, toolNames, msg.Disabled)
	})
}

func (a *App) ipcSetContext(raw json.RawMessage) {
	var msg ipcSetContext
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	contextRaw, err := json.Marshal(msg.Context)
	if err != nil {
		slog.Error("failed to marshal context", "error", err)
		return
	}
	WithSettings(func(s *Settings) { s.SetContext(msg.Hash, msg.McpID, json.RawMessage(contextRaw)) })
}

func (a *App) ipcAddExternalMcp(raw json.RawMessage) {
	var msg ipcAddExternalMcp
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	id := slugify(msg.DisplayName)
	if id == "" {
		return
	}

	if msg.Transport == "http" {
		if msg.URL == "" {
			return
		}
		if err := validateMcpURL(msg.URL); err != nil {
			a.evalSettings(fmt.Sprintf("onExternalMcpError('%s')", escapeJSString(err.Error())))
			return
		}
		a.evalSettings("onDiscoveryStarted()")
		go a.addHTTPMcp(msg.DisplayName, id, msg.URL)
		return
	}

	if msg.Command == "" {
		return
	}

	a.evalSettings("onDiscoveryStarted()")

	go func() {
		result, err := DiscoverExternalMcp(msg.DisplayName, id, msg.Command, msg.Args, msg.Env)
		a.platform.DispatchToMain(func() {
			if err != nil {
				escaped := escapeJSString(err.Error())
				a.evalSettings(fmt.Sprintf("onExternalMcpError('%s')", escaped))
				return
			}

			var adminSecret string
			WithSettings(func(s *Settings) {
				s.AddExternalMcp(*result)
				adminSecret = s.AdminSecret
			})

			go func() { _ = bridge.SendReconcile(adminSecret) }()

			mcpJSON, err := json.Marshal(result)
			if err != nil {
				slog.Error("failed to marshal external MCP", "error", err)
				return
			}
			a.evalSettings(fmt.Sprintf("onExternalMcpAdded(%s)", string(mcpJSON)))
		})
	}()
}

func (a *App) ipcAuthenticateMcp(raw json.RawMessage) {
	var msg ipcIDMsg
	if err := json.Unmarshal(raw, &msg); err != nil || msg.ID == "" {
		return
	}
	go a.authenticateMcp(msg.ID)
}

func (a *App) ipcRemoveExternalMcp(raw json.RawMessage) {
	var msg ipcIDMsg
	if err := json.Unmarshal(raw, &msg); err != nil || msg.ID == "" {
		return
	}

	var adminSecret string
	WithSettings(func(s *Settings) {
		s.RemoveExternalMcp(msg.ID)
		adminSecret = s.AdminSecret
	})

	go func() { _ = bridge.SendReconcile(adminSecret) }()

	a.evalSettings(fmt.Sprintf("onExternalMcpRemoved('%s')", escapeJSString(msg.ID)))
}

func (a *App) ipcCopyToClipboard(raw json.RawMessage) {
	var msg ipcCopyToClipboard
	if err := json.Unmarshal(raw, &msg); err != nil || msg.Text == "" {
		return
	}
	a.platform.CopyToClipboard(msg.Text)
}

func (a *App) ipcAddService(raw json.RawMessage) {
	var msg ipcServiceMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}

	id := slugify(msg.DisplayName)
	if id == "" || msg.Command == "" {
		return
	}

	config := ServiceConfig{
		ID:          id,
		DisplayName: msg.DisplayName,
		Command:     msg.Command,
		Args:        msg.Args,
		Env:         msg.Env,
		WorkingDir:  msg.WorkingDir,
		Autostart:   msg.Autostart,
		URL:         msg.URL,
	}

	WithSettings(func(s *Settings) { s.AddService(config) })

	if msg.Autostart {
		if err := a.registry.Start(&config); err != nil {
			slog.Error("service autostart failed", "error", err)
		}
	}

	a.updateMenu()

	configJSON, err := json.Marshal(config)
	if err != nil {
		slog.Error("failed to marshal service config", "error", err)
		return
	}
	a.evalSettings(fmt.Sprintf("onServiceAdded(%s)", string(configJSON)))
}

func (a *App) ipcRemoveService(raw json.RawMessage) {
	var msg ipcIDMsg
	if err := json.Unmarshal(raw, &msg); err != nil || msg.ID == "" {
		return
	}

	a.registry.Stop(msg.ID)
	WithSettings(func(s *Settings) { s.RemoveService(msg.ID) })
	a.updateMenu()

	a.evalSettings(fmt.Sprintf("onServiceRemoved('%s')", escapeJSString(msg.ID)))
}

func (a *App) ipcUpdateService(raw json.RawMessage) {
	var msg ipcUpdateServiceMsg
	if err := json.Unmarshal(raw, &msg); err != nil || msg.ID == "" {
		return
	}

	config := ServiceConfig{
		ID:          msg.ID,
		DisplayName: msg.DisplayName,
		Command:     msg.Command,
		Args:        msg.Args,
		Env:         msg.Env,
		WorkingDir:  msg.WorkingDir,
		Autostart:   msg.Autostart,
		URL:         msg.URL,
	}

	WithSettings(func(s *Settings) { s.UpdateService(config) })
	a.updateMenu()
}

func (a *App) ipcUpdateServiceAutostart(raw json.RawMessage) {
	var msg ipcUpdateServiceAutostart
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	WithSettings(func(s *Settings) {
		for i := range s.Services {
			if s.Services[i].ID == msg.ID {
				s.Services[i].Autostart = msg.Autostart
				break
			}
		}
	})
}

func (a *App) ipcStartService(raw json.RawMessage) {
	var msg ipcIDMsg
	if err := json.Unmarshal(raw, &msg); err != nil || msg.ID == "" {
		return
	}
	s := LoadSettings()
	for i := range s.Services {
		if s.Services[i].ID == msg.ID {
			if err := a.registry.Start(&s.Services[i]); err != nil {
				slog.Error("service start failed", "error", err)
			}
			break
		}
	}
	a.pushServiceStatus()
	a.updateMenu()
}

func (a *App) ipcStopService(raw json.RawMessage) {
	var msg ipcIDMsg
	if err := json.Unmarshal(raw, &msg); err != nil || msg.ID == "" {
		return
	}
	go func() {
		a.registry.Stop(msg.ID)
		a.platform.DispatchToMain(func() {
			a.pushServiceStatus()
			a.updateMenu()
		})
	}()
}
