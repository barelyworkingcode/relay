package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"

	"relaygo/mcp"
)

// ---------------------------------------------------------------------------
// Interfaces for dependency injection
// ---------------------------------------------------------------------------

// ToolProvider abstracts access to external MCP connections.
type ToolProvider interface {
	Tools(id string) []mcp.Tool
	FindToolOwner(name string) (string, *ExternalMcp)
	CallTool(ctx context.Context, id, name string, args, meta json.RawMessage) (json.RawMessage, error)
	Reconcile(mcps []ExternalMcp)
	Reload(id string, cfg *ExternalMcp) error
}

// ServiceReloader abstracts service restart operations.
type ServiceReloader interface {
	Reload(id string, cfg *ServiceConfig) error
}

// checkToolAccess verifies that the given token has permission to access
// the specified MCP and (optionally) tool. Pass empty toolName to check
// only the MCP-level permission.
func checkToolAccess(s *Settings, tokenHash, mcpID, toolName string) error {
	if s.GetPermission(tokenHash, mcpID) == PermOff {
		return fmt.Errorf("access denied: service '%s' is disabled for this token", mcpID)
	}
	if toolName != "" && s.IsToolDisabled(tokenHash, mcpID, toolName) {
		return fmt.Errorf("access denied: tool '%s' is disabled for this token", toolName)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ToolRouter implementation
// ---------------------------------------------------------------------------

type appRouter struct {
	tools    ToolProvider
	services ServiceReloader
	onChange func()
}

// resolveAuth loads settings and authenticates the given token.
func (r *appRouter) resolveAuth(token string) (*StoredToken, *Settings, error) {
	s := GetSettings()
	stored, err := s.Authenticate(token)
	if err != nil {
		return nil, nil, err
	}
	return stored, s, nil
}

func (r *appRouter) ListTools(_ context.Context, token string) (json.RawMessage, error) {
	stored, settings, err := r.resolveAuth(token)
	if err != nil {
		return nil, err
	}

	tools := make([]mcp.Tool, 0)

	// External MCP tools.
	for _, ext := range settings.ExternalMcps {
		if checkToolAccess(settings, stored.Hash, ext.ID, "") != nil {
			continue
		}
		for _, t := range r.tools.Tools(ext.ID) {
			if checkToolAccess(settings, stored.Hash, ext.ID, t.Name) != nil {
				continue
			}
			tools = append(tools, t)
		}
	}

	return json.Marshal(tools)
}

func (r *appRouter) CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error) {
	stored, settings, err := r.resolveAuth(token)
	if err != nil {
		return nil, err
	}

	// Check external MCPs.
	extID, extMcp := r.tools.FindToolOwner(name)
	if extMcp != nil {
		if err := checkToolAccess(settings, stored.Hash, extID, name); err != nil {
			return nil, err
		}

		// Inject per-token context as _meta for this MCP.
		var meta json.RawMessage
		if stored.Context != nil {
			meta = stored.Context[extID]
		}

		return r.tools.CallTool(ctx, extID, name, args, meta)
	}

	return nil, fmt.Errorf("unknown tool: %s", name)
}

func (r *appRouter) ValidateAdmin(token string) error {
	s := GetSettings()
	if len(token) == 0 || subtle.ConstantTimeCompare([]byte(token), []byte(s.AdminSecret)) != 1 {
		return fmt.Errorf("admin authentication failed")
	}
	return nil
}

func (r *appRouter) ReconcileExternalMcps() {
	settings := ReloadSettings()
	r.tools.Reconcile(settings.ExternalMcps)
	r.onChange()
}

func (r *appRouter) ReloadService(id string) {
	settings := ReloadSettings()
	svc, _ := settings.findServiceByID(id)
	if svc == nil {
		slog.Warn("reload: no service found", "id", id)
		return
	}
	if err := r.services.Reload(id, svc); err != nil {
		slog.Error("failed to reload service", "id", id, "error", err)
	}
	r.onChange()
}

func (r *appRouter) ReloadExternalMcp(id string) {
	settings := ReloadSettings()
	mcpCfg, _ := settings.findMcpByID(id)
	if mcpCfg == nil {
		slog.Warn("reload: no external MCP found", "id", id)
		return
	}
	if err := r.tools.Reload(id, mcpCfg); err != nil {
		slog.Error("failed to reload external MCP", "id", id, "error", err)
	}
	r.onChange()
}
