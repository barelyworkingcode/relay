package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"relaygo/bridge"
)

// appInstance is the singleton tray app, set by runTrayApp and read by Cocoa callbacks.
var appInstance *App

// App is the main tray application state.
type App struct {
	settings     *Settings
	platform     Platform
	extMgr       *ExternalMcpManager
	registry     *ServiceRegistry
	bridgeServer *bridge.BridgeServer
	settingsOpen bool
	mu           sync.Mutex
}

// Menu item IDs.
const (
	menuIDSettings = 2
	menuIDExit     = 3
	menuIDSvcBase  = 100 // service items start here
)

// ensureBuiltinMcps registers bundled MCP servers (e.g. macmcp) if not already
// present, or migrates existing entries to use relative builtin paths.
func ensureBuiltinMcps(settings *Settings) {
	const builtinID = "macmcp"
	const builtinName = "macMCP"
	const builtinCmd = "macmcp"

	// Check if the binary actually exists next to us (skip when running via `go run`).
	if _, err := resolveBuiltinCommand(builtinCmd); err != nil {
		return
	}

	for i := range settings.ExternalMcps {
		if settings.ExternalMcps[i].ID == builtinID {
			// Already registered -- migrate to builtin if needed.
			if !settings.ExternalMcps[i].Builtin {
				settings.ExternalMcps[i].Builtin = true
				settings.ExternalMcps[i].Command = builtinCmd
				settings.Save()
			}
			return
		}
	}

	// Not registered -- add it.
	settings.AddExternalMcp(ExternalMcp{
		ID:              builtinID,
		DisplayName:     builtinName,
		Command:         builtinCmd,
		Builtin:         true,
		DiscoveredTools: []ToolInfo{},
	})
}

func runTrayApp() {
	fmt.Fprintf(os.Stderr, "[relay] starting tray app\n")

	platform := NewPlatform()

	// Initialize platform UI first so app delegate exists before tray setup.
	platform.Init()
	fmt.Fprintf(os.Stderr, "[relay] platform initialized\n")

	settings := LoadSettings()
	ensureBuiltinMcps(settings)
	fmt.Fprintf(os.Stderr, "[relay] settings loaded\n")

	// External MCP manager.
	extMgr := NewExternalMcpManager()

	app := &App{
		settings: settings,
		platform: platform,
		extMgr:   extMgr,
		registry: NewServiceRegistry(),
	}
	appInstance = app

	// Create and start bridge server.
	router := &appRouter{app: app}
	bs, err := bridge.NewBridgeServer(router)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[relay] failed to start bridge server: %v\n", err)
		os.Exit(1)
	}
	app.bridgeServer = bs
	go bs.Serve()
	fmt.Fprintf(os.Stderr, "[relay] bridge server started\n")

	// Start external MCPs.
	extMgr.StartAll(settings.ExternalMcps)

	// Start autostart services.
	app.registry.StartAllAutostart(settings.Services)

	// Set up tray icon.
	fmt.Fprintf(os.Stderr, "[relay] setting up tray icon\n")
	rgba, w, h := CreateIconRGBA()
	platform.SetupTray(rgba, w, h)
	fmt.Fprintf(os.Stderr, "[relay] tray icon set up\n")

	// Build and set initial menu.
	app.updateMenu()
	fmt.Fprintf(os.Stderr, "[relay] menu built\n")

	// Poll service status every 2s.
	go app.statusPoller()

	// Block on the platform run loop (must be on main thread).
	fmt.Fprintf(os.Stderr, "[relay] entering run loop\n")
	appRunning.Store(true)
	platform.Run()
}

// statusPoller periodically checks service status and updates the menu.
func (a *App) statusPoller() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if !appRunning.Load() {
			return
		}
		a.mu.Lock()
		s := LoadSettings()
		if len(s.Services) == 0 {
			a.mu.Unlock()
			continue
		}
		ids := make([]string, len(s.Services))
		for i, svc := range s.Services {
			ids[i] = svc.ID
		}
		statuses := a.registry.CheckAll(ids)
		a.mu.Unlock()

		_ = statuses
		a.platform.DispatchToMain(func() {
			a.updateMenu()
		})
	}
}

// updateMenu rebuilds the tray menu JSON and pushes it to the platform.
func (a *App) updateMenu() {
	type menuItem struct {
		Title   string `json:"title"`
		ID      int    `json:"id"`
		Enabled bool   `json:"enabled"`
	}

	var items []menuItem

	// Service items.
	s := LoadSettings()
	for i, svc := range s.Services {
		running := a.registry.IsRunning(svc.ID)
		dot := "\U0001F534" // red
		if running {
			dot = "\U0001F7E2" // green
		}
		items = append(items, menuItem{
			Title:   fmt.Sprintf("%s %s", dot, svc.DisplayName),
			ID:      menuIDSvcBase + i,
			Enabled: true,
		})
	}

	if len(s.Services) > 0 {
		items = append(items, menuItem{Title: "-", ID: 0})
	}

	items = append(items,
		menuItem{Title: "Settings...", ID: menuIDSettings, Enabled: true},
		menuItem{Title: "-", ID: 0},
		menuItem{Title: "Exit", ID: menuIDExit, Enabled: true},
	)

	data, err := json.Marshal(items)
	if err != nil {
		return
	}
	a.platform.UpdateMenu(string(data))
}

// onMenuClick is called from the platform menu action on the main thread.
func (a *App) onMenuClick(itemID int) {
	switch {
	case itemID == menuIDSettings:
		a.openSettingsWindow()

	case itemID == menuIDExit:
		a.cleanup()
		os.Exit(0)

	case itemID >= menuIDSvcBase:
		a.toggleService(itemID - menuIDSvcBase)
	}
}

func (a *App) toggleService(index int) {
	s := LoadSettings()
	if index < 0 || index >= len(s.Services) {
		return
	}
	config := &s.Services[index]

	// If the service has a URL, lazy-start and open the URL.
	if config.URL != "" {
		if !a.registry.IsRunning(config.ID) {
			if err := a.registry.Start(config); err != nil {
				fmt.Fprintf(os.Stderr, "service start: %v\n", err)
			}
		}
		a.platform.OpenURL(config.URL)
		a.updateMenu()
		return
	}

	if a.registry.IsRunning(config.ID) {
		a.registry.Stop(config.ID)
	} else {
		if err := a.registry.Start(config); err != nil {
			fmt.Fprintf(os.Stderr, "service toggle: %v\n", err)
		}
	}
	a.updateMenu()
}

func (a *App) cleanup() {
	appRunning.Store(false)
	a.registry.StopAll()
	a.extMgr.StopAll()
	if a.bridgeServer != nil {
		a.bridgeServer.Close()
	}
}

// ---------------------------------------------------------------------------
// Settings window
// ---------------------------------------------------------------------------

func (a *App) openSettingsWindow() {
	s := LoadSettings()
	html := renderSettingsHTML(s)
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

// onSettingsIpc is called from the WKWebView IPC handler.
// The message body is a JSON string from the ipc() wrapper.
func (a *App) onSettingsIpc(body string) {
	var msg map[string]interface{}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		return
	}

	msgType, _ := msg["type"].(string)

	switch msgType {
	case "generate_token":
		name, _ := msg["name"].(string)
		s := LoadSettings()
		defaultPerms := make(map[string]Permission)
		for _, svcName := range s.AllServiceNames() {
			defaultPerms[svcName] = PermOn
		}
		plaintext, stored := GenerateToken(name, defaultPerms)
		s.Tokens = append(s.Tokens, stored)
		s.Save()

		response := map[string]interface{}{
			"plaintext": plaintext,
			"token":     stored,
		}
		responseJSON, _ := json.Marshal(response)
		a.evalSettings(fmt.Sprintf("onTokenGenerated(%s)", string(responseJSON)))

	case "delete_token":
		hash, _ := msg["hash"].(string)
		s := LoadSettings()
		s.DeleteToken(hash)
		a.evalSettings(fmt.Sprintf("onTokenDeleted('%s')", escapeJSString(hash)))

	case "revoke_all":
		s := LoadSettings()
		s.RevokeAll()
		a.evalSettings("onAllRevoked()")

	case "update_permission":
		hash, _ := msg["hash"].(string)
		service, _ := msg["service"].(string)
		permStr, _ := msg["permission"].(string)
		perm := PermOn
		if permStr == "off" {
			perm = PermOff
		}
		s := LoadSettings()
		s.UpdatePermission(hash, service, perm)

	case "set_tool_disabled":
		hash, _ := msg["hash"].(string)
		mcpID, _ := msg["mcp_id"].(string)
		toolName, _ := msg["tool_name"].(string)
		disabled, _ := msg["disabled"].(bool)
		s := LoadSettings()
		s.SetToolDisabled(hash, mcpID, toolName, disabled)

	case "set_all_tools_disabled":
		hash, _ := msg["hash"].(string)
		mcpID, _ := msg["mcp_id"].(string)
		disabled, _ := msg["disabled"].(bool)
		s := LoadSettings()
		// Collect tool names from discovered tools for this MCP.
		var toolNames []string
		for _, mcp := range s.ExternalMcps {
			if mcp.ID == mcpID {
				for _, t := range mcp.DiscoveredTools {
					toolNames = append(toolNames, t.Name)
				}
				break
			}
		}
		s.SetAllToolsDisabled(hash, mcpID, toolNames, disabled)

	case "add_external_mcp":
		displayName, _ := msg["display_name"].(string)
		command, _ := msg["command"].(string)
		args := jsonStringArray(msg["args"])
		env := jsonStringMap(msg["env"])

		id := slugify(displayName)
		if id == "" || command == "" {
			return
		}

		a.evalSettings("onDiscoveryStarted()")

		go func() {
			result, err := DiscoverExternalMcp(displayName, id, command, args, env)
			a.platform.DispatchToMain(func() {
				if err != nil {
					escaped := escapeJSString(err.Error())
					a.evalSettings(fmt.Sprintf("onExternalMcpError('%s')", escaped))
					return
				}

				s := LoadSettings()
				s.AddExternalMcp(*result)

				// Notify bridge to reconcile.
				go func() { _ = bridge.SendReconcile() }()

				mcpJSON, _ := json.Marshal(result)
				a.evalSettings(fmt.Sprintf("onExternalMcpAdded(%s)", string(mcpJSON)))
			})
		}()

	case "remove_external_mcp":
		id, _ := msg["id"].(string)
		if id == "" {
			return
		}

		s := LoadSettings()
		for _, m := range s.ExternalMcps {
			if m.ID == id && m.Builtin {
				return
			}
		}
		s.RemoveExternalMcp(id)

		go func() { _ = bridge.SendReconcile() }()

		escaped := escapeJSString(id)
		a.evalSettings(fmt.Sprintf("onExternalMcpRemoved('%s')", escaped))

	case "copy_to_clipboard":
		if text, ok := msg["text"].(string); ok {
			a.platform.CopyToClipboard(text)
		}

	case "add_service":
		displayName, _ := msg["display_name"].(string)
		command, _ := msg["command"].(string)
		args := jsonStringArray(msg["args"])
		env := jsonStringMap(msg["env"])
		workingDir, _ := msg["working_dir"].(string)
		autostart, _ := msg["autostart"].(bool)
		url, _ := msg["url"].(string)

		id := slugify(displayName)
		if id == "" || command == "" {
			return
		}

		config := ServiceConfig{
			ID:          id,
			DisplayName: displayName,
			Command:     command,
			Args:        args,
			Env:         env,
			WorkingDir:  workingDir,
			Autostart:   autostart,
			URL:         url,
		}

		s := LoadSettings()
		s.AddService(config)

		if autostart {
			if err := a.registry.Start(&config); err != nil {
				fmt.Fprintf(os.Stderr, "service autostart: %v\n", err)
			}
		}

		a.updateMenu()

		configJSON, _ := json.Marshal(config)
		a.evalSettings(fmt.Sprintf("onServiceAdded(%s)", string(configJSON)))

	case "remove_service":
		id, _ := msg["id"].(string)
		if id == "" {
			return
		}

		a.registry.Stop(id)
		s := LoadSettings()
		s.RemoveService(id)
		a.updateMenu()

		escaped := escapeJSString(id)
		a.evalSettings(fmt.Sprintf("onServiceRemoved('%s')", escaped))

	case "update_service":
		id, _ := msg["id"].(string)
		if id == "" {
			return
		}
		displayName, _ := msg["display_name"].(string)
		command, _ := msg["command"].(string)
		args := jsonStringArray(msg["args"])
		env := jsonStringMap(msg["env"])
		workingDir, _ := msg["working_dir"].(string)
		autostart, _ := msg["autostart"].(bool)
		url, _ := msg["url"].(string)

		config := ServiceConfig{
			ID:          id,
			DisplayName: displayName,
			Command:     command,
			Args:        args,
			Env:         env,
			WorkingDir:  workingDir,
			Autostart:   autostart,
			URL:         url,
		}

		s := LoadSettings()
		s.UpdateService(config)
		a.updateMenu()

	case "update_service_autostart":
		id, _ := msg["id"].(string)
		autostart, _ := msg["autostart"].(bool)
		s := LoadSettings()
		for i := range s.Services {
			if s.Services[i].ID == id {
				s.Services[i].Autostart = autostart
				break
			}
		}
		s.Save()
	}
}

// ---------------------------------------------------------------------------
// ToolRouter implementation
// ---------------------------------------------------------------------------

type appRouter struct {
	app *App
}

func (r *appRouter) ListTools(token string) (json.RawMessage, error) {
	settings := LoadSettings()
	if err := settings.CheckAuth(token); err != nil {
		return nil, err
	}
	stored := settings.ValidateToken(token)
	if stored == nil {
		return nil, fmt.Errorf("invalid token")
	}

	var tools []json.RawMessage

	// External MCP tools.
	for _, ext := range settings.ExternalMcps {
		perm := settings.GetPermission(stored.Hash, ext.ID)
		if perm == PermOff {
			continue
		}
		for _, t := range r.app.extMgr.Tools(ext.ID) {
			if settings.IsToolDisabled(stored.Hash, ext.ID, t.Name) {
				continue
			}
			data, _ := json.Marshal(t)
			tools = append(tools, data)
		}
	}

	// Build a single JSON array.
	result := []byte{'['}
	for i, t := range tools {
		if i > 0 {
			result = append(result, ',')
		}
		result = append(result, t...)
	}
	result = append(result, ']')
	return result, nil
}

func (r *appRouter) CallTool(name string, args json.RawMessage, token string) (json.RawMessage, error) {
	settings := LoadSettings()
	if err := settings.CheckAuth(token); err != nil {
		return nil, err
	}
	stored := settings.ValidateToken(token)
	if stored == nil {
		return nil, fmt.Errorf("invalid token")
	}

	// Check external MCPs.
	extID, extMcp := r.app.extMgr.FindToolOwner(name)
	if extMcp != nil {
		perm := settings.GetPermission(stored.Hash, extID)
		if perm == PermOff {
			return nil, fmt.Errorf("access denied: service '%s' is disabled for this token", extID)
		}

		if settings.IsToolDisabled(stored.Hash, extID, name) {
			return nil, fmt.Errorf("access denied: tool '%s' is disabled for this token", name)
		}

		result, err := r.app.extMgr.CallTool(extID, name, args)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return data, nil
	}

	return nil, fmt.Errorf("unknown tool: %s", name)
}

func (r *appRouter) ReconcileExternalMcps() {
	settings := LoadSettings()
	r.app.extMgr.Reconcile(settings.ExternalMcps)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func escapeJSString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func slugify(name string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	parts := strings.Split(b.String(), "-")
	var nonEmpty []string
	for _, p := range parts {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "-")
}

func jsonStringArray(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func jsonStringMap(v interface{}) map[string]string {
	obj, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	result := make(map[string]string, len(obj))
	for k, val := range obj {
		if s, ok := val.(string); ok {
			result[k] = s
		}
	}
	return result
}
