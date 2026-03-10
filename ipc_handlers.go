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
		var plaintext string
		var stored StoredToken
		WithSettings(func(s *Settings) {
			defaultPerms := make(map[string]Permission)
			for _, svcName := range s.AllExternalMcpIDs() {
				defaultPerms[svcName] = PermOn
			}
			plaintext, stored = GenerateToken(name, defaultPerms)
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

	case "delete_token":
		hash, _ := msg["hash"].(string)
		WithSettings(func(s *Settings) { s.DeleteToken(hash) })
		a.evalSettings(fmt.Sprintf("onTokenDeleted('%s')", escapeJSString(hash)))

	case "revoke_all":
		WithSettings(func(s *Settings) { s.RevokeAll() })
		a.evalSettings("onAllRevoked()")

	case "update_permission":
		hash, _ := msg["hash"].(string)
		service, _ := msg["service"].(string)
		permStr, _ := msg["permission"].(string)
		perm := PermOn
		if permStr == "off" {
			perm = PermOff
		}
		WithSettings(func(s *Settings) { s.UpdatePermission(hash, service, perm) })

	case "set_tool_disabled":
		hash, _ := msg["hash"].(string)
		mcpID, _ := msg["mcp_id"].(string)
		toolName, _ := msg["tool_name"].(string)
		disabled, _ := msg["disabled"].(bool)
		WithSettings(func(s *Settings) { s.SetToolDisabled(hash, mcpID, toolName, disabled) })

	case "set_all_tools_disabled":
		hash, _ := msg["hash"].(string)
		mcpID, _ := msg["mcp_id"].(string)
		disabled, _ := msg["disabled"].(bool)
		WithSettings(func(s *Settings) {
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
		})

	case "set_context":
		hash, _ := msg["hash"].(string)
		mcpID, _ := msg["mcp_id"].(string)
		contextRaw, err := json.Marshal(msg["context"])
		if err != nil {
			slog.Error("failed to marshal context", "error", err)
			return
		}
		WithSettings(func(s *Settings) { s.SetContext(hash, mcpID, json.RawMessage(contextRaw)) })

	case "add_external_mcp":
		displayName, _ := msg["display_name"].(string)
		transport, _ := msg["transport"].(string)

		id := slugify(displayName)
		if id == "" {
			return
		}

		if transport == "http" {
			mcpURL, _ := msg["url"].(string)
			if mcpURL == "" {
				return
			}
			if err := validateMcpURL(mcpURL); err != nil {
				a.evalSettings(fmt.Sprintf("onExternalMcpError('%s')", escapeJSString(err.Error())))
				return
			}
			a.evalSettings("onDiscoveryStarted()")
			go a.addHTTPMcp(displayName, id, mcpURL)
			return
		}

		command, _ := msg["command"].(string)
		args := jsonStringArray(msg["args"])
		env := jsonStringMap(msg["env"])

		if command == "" {
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

				var adminSecret string
				WithSettings(func(s *Settings) {
					s.AddExternalMcp(*result)
					adminSecret = s.AdminSecret
				})

				// Notify bridge to reconcile.
				go func() { _ = bridge.SendReconcile(adminSecret) }()

				mcpJSON, err := json.Marshal(result)
				if err != nil {
					slog.Error("failed to marshal external MCP", "error", err)
					return
				}
				a.evalSettings(fmt.Sprintf("onExternalMcpAdded(%s)", string(mcpJSON)))
			})
		}()

	case "authenticate_mcp":
		id, _ := msg["id"].(string)
		if id == "" {
			return
		}
		go a.authenticateMcp(id)

	case "remove_external_mcp":
		id, _ := msg["id"].(string)
		if id == "" {
			return
		}

		var adminSecret string
		WithSettings(func(s *Settings) {
			s.RemoveExternalMcp(id)
			adminSecret = s.AdminSecret
		})

		go func() { _ = bridge.SendReconcile(adminSecret) }()

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

		WithSettings(func(s *Settings) { s.AddService(config) })

		if autostart {
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

	case "remove_service":
		id, _ := msg["id"].(string)
		if id == "" {
			return
		}

		a.registry.Stop(id)
		WithSettings(func(s *Settings) { s.RemoveService(id) })
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

		WithSettings(func(s *Settings) { s.UpdateService(config) })
		a.updateMenu()

	case "update_service_autostart":
		id, _ := msg["id"].(string)
		autostart, _ := msg["autostart"].(bool)
		WithSettings(func(s *Settings) {
			for i := range s.Services {
				if s.Services[i].ID == id {
					s.Services[i].Autostart = autostart
					break
				}
			}
		})

	case "start_service":
		id, _ := msg["id"].(string)
		s := LoadSettings()
		for i := range s.Services {
			if s.Services[i].ID == id {
				if err := a.registry.Start(&s.Services[i]); err != nil {
					slog.Error("service start failed", "error", err)
				}
				break
			}
		}
		a.pushServiceStatus()
		a.updateMenu()

	case "stop_service":
		id, _ := msg["id"].(string)
		go func() {
			a.registry.Stop(id)
			a.platform.DispatchToMain(func() {
				a.pushServiceStatus()
				a.updateMenu()
			})
		}()
	}
}
