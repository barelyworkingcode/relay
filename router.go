package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"relaygo/bridge"
)

// ---------------------------------------------------------------------------
// ToolRouter implementation
// ---------------------------------------------------------------------------

type appRouter struct {
	app *App
}

func (r *appRouter) ListTools(token string) (json.RawMessage, error) {
	settings := LoadSettings()
	stored, err := settings.Authenticate(token)
	if err != nil {
		return nil, err
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
	stored, err := settings.Authenticate(token)
	if err != nil {
		return nil, err
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

		// Inject per-token context as _meta for this MCP.
		var meta json.RawMessage
		if stored.Context != nil {
			meta = stored.Context[extID]
		}

		return r.app.extMgr.CallTool(extID, name, args, meta)
	}

	return nil, fmt.Errorf("unknown tool: %s", name)
}

func (r *appRouter) ValidateAdmin(token string) error {
	s := LoadSettings()
	if len(token) == 0 || subtle.ConstantTimeCompare([]byte(token), []byte(s.AdminSecret)) != 1 {
		return fmt.Errorf("admin authentication failed")
	}
	return nil
}

func (r *appRouter) ReconcileExternalMcps() {
	settings := LoadSettings()
	r.app.extMgr.Reconcile(settings.ExternalMcps)
}

func (r *appRouter) ReloadService(id string) {
	settings := LoadSettings()
	for i := range settings.Services {
		if settings.Services[i].ID == id {
			if err := r.app.registry.Reload(id, &settings.Services[i]); err != nil {
				slog.Error("failed to reload service", "id", id, "error", err)
			}
			return
		}
	}
	slog.Warn("reload: no service found", "id", id)
}

func (r *appRouter) ReloadExternalMcp(id string) {
	settings := LoadSettings()
	for i := range settings.ExternalMcps {
		if settings.ExternalMcps[i].ID == id {
			if err := r.app.extMgr.Reload(id, &settings.ExternalMcps[i]); err != nil {
				slog.Error("failed to reload external MCP", "id", id, "error", err)
			}
			return
		}
	}
	slog.Warn("reload: no external MCP found", "id", id)
}

// ---------------------------------------------------------------------------
// HTTP MCP helpers
// ---------------------------------------------------------------------------

func (a *App) addHTTPMcp(displayName, id, mcpURL string) {
	result, err := DiscoverHTTPMcp(displayName, id, mcpURL, nil)

	if err != nil && !errors.Is(err, ErrAuthRequired) {
		a.platform.DispatchToMain(func() {
			escaped := escapeJSString(err.Error())
			a.evalSettings(fmt.Sprintf("onExternalMcpError('%s')", escaped))
		})
		return
	}

	needsAuth := errors.Is(err, ErrAuthRequired)

	a.platform.DispatchToMain(func() {
		var adminSecret string
		WithSettings(func(s *Settings) {
			s.AddExternalMcp(*result)
			adminSecret = s.AdminSecret
		})
		go func() { _ = bridge.SendReconcile(adminSecret) }()

		mcpJSON, _ := json.Marshal(result)
		a.evalSettings(fmt.Sprintf("onExternalMcpAdded(%s)", string(mcpJSON)))

		if needsAuth {
			a.evalSettings(fmt.Sprintf("onOAuthRequired('%s')", escapeJSString(id)))
		}
	})
}

func (a *App) authenticateMcp(id string) {
	s := LoadSettings()
	var mcpCfg *ExternalMcp
	for i := range s.ExternalMcps {
		if s.ExternalMcps[i].ID == id {
			mcpCfg = &s.ExternalMcps[i]
			break
		}
	}
	if mcpCfg == nil || !mcpCfg.IsHTTP() {
		return
	}

	a.platform.DispatchToMain(func() {
		a.evalSettings(fmt.Sprintf("onOAuthStarted('%s')", escapeJSString(id)))
	})

	oauth, err := startOAuthFlow(mcpCfg.URL, a.platform.OpenURL)
	if err != nil {
		a.platform.DispatchToMain(func() {
			escaped := escapeJSString(err.Error())
			a.evalSettings(fmt.Sprintf("onOAuthError('%s','%s')", escapeJSString(id), escaped))
		})
		return
	}

	a.platform.DispatchToMain(func() {
		var adminSecret string
		WithSettings(func(s *Settings) {
			s.UpdateOAuthState(id, oauth)
			adminSecret = s.AdminSecret
		})

		// Reload the MCP connection with the new tokens.
		go func() { _ = bridge.SendReloadMcp(id, adminSecret) }()

		a.evalSettings(fmt.Sprintf("onOAuthComplete('%s')", escapeJSString(id)))
	})
}
