package main

import (
	"encoding/json"
	"slices"
)

// Settings holds all persistent Relay configuration.
type Settings struct {
	Version      int             `json:"version"`
	Tokens       []StoredToken   `json:"tokens"`
	ExternalMcps []ExternalMcp   `json:"external_mcps"`
	Services     []ServiceConfig `json:"services"`
	AdminSecret  string          `json:"admin_secret,omitempty"`
}

// ---------------------------------------------------------------------------
// MCP CRUD — methods are small and cohesive with the Settings struct.
// ---------------------------------------------------------------------------

// AddExternalMcp adds an external MCP config. New MCPs default to PermOff for
// existing tokens (least privilege). Does not save; use within store.With.
func (s *Settings) AddExternalMcp(mcp ExternalMcp) {
	s.ExternalMcps = append(s.ExternalMcps, mcp)
	for i := range s.Tokens {
		if s.Tokens[i].Permissions == nil {
			s.Tokens[i].Permissions = make(map[string]Permission)
		}
		if _, exists := s.Tokens[i].Permissions[mcp.ID]; !exists {
			s.Tokens[i].Permissions[mcp.ID] = PermOff
		}
	}
}

// UpdateExternalMcp replaces an external MCP config by ID.
// Preserves DiscoveredTools from the existing entry if the new one has none.
// Does not save; use within store.With.
func (s *Settings) UpdateExternalMcp(cfg ExternalMcp) {
	existing, idx := s.findMcpByID(cfg.ID)
	if existing == nil {
		return
	}
	if len(cfg.DiscoveredTools) == 0 {
		cfg.DiscoveredTools = existing.DiscoveredTools
	}
	s.ExternalMcps[idx] = cfg
}

// RemoveExternalMcp removes an external MCP and cleans up token permissions,
// disabled tools, and per-MCP context. Does not save; use within store.With.
func (s *Settings) RemoveExternalMcp(id string) {
	s.ExternalMcps = slices.DeleteFunc(s.ExternalMcps, func(m ExternalMcp) bool { return m.ID == id })
	for i := range s.Tokens {
		delete(s.Tokens[i].Permissions, id)
		delete(s.Tokens[i].DisabledTools, id)
		delete(s.Tokens[i].Context, id)
	}
}

// UpsertExternalMcp adds or updates an external MCP config.
// Returns true if it updated an existing entry.
// Does not save; use within store.With.
func (s *Settings) UpsertExternalMcp(cfg ExternalMcp) bool {
	if _, idx := s.findMcpByID(cfg.ID); idx >= 0 {
		s.UpdateExternalMcp(cfg)
		return true
	}
	s.AddExternalMcp(cfg)
	return false
}

// ResolveMcpID returns the ID of an MCP found by exact id or display name lookup.
// Returns "" if not found.
func (s *Settings) ResolveMcpID(id, name string) string {
	if id != "" {
		if _, idx := s.findMcpByID(id); idx >= 0 {
			return id
		}
		return ""
	}
	for _, m := range s.ExternalMcps {
		if m.DisplayName == name {
			return m.ID
		}
	}
	return ""
}

// UpdateOAuthState updates the OAuth state for an HTTP MCP.
// Does not save; use within store.With.
func (s *Settings) UpdateOAuthState(mcpID string, oauth *OAuthState) {
	if mcp, _ := s.findMcpByID(mcpID); mcp != nil {
		mcp.OAuthState = oauth
	}
}

// UpdateDiscoveredTools updates the persisted tool list for an external MCP.
// Does not save; use within store.With.
func (s *Settings) UpdateDiscoveredTools(mcpID string, tools []ToolInfo) {
	if mcp, _ := s.findMcpByID(mcpID); mcp != nil {
		mcp.DiscoveredTools = tools
	}
}

// UpdateContextSchema updates the persisted context schema for an external MCP.
// Does not save; use within store.With.
func (s *Settings) UpdateContextSchema(mcpID string, schema json.RawMessage) {
	if mcp, _ := s.findMcpByID(mcpID); mcp != nil {
		mcp.ContextSchema = schema
	}
}

// AllExternalMcpIDs returns the IDs of all configured external MCPs.
func (s *Settings) AllExternalMcpIDs() []string {
	ids := make([]string, 0, len(s.ExternalMcps))
	for _, mcp := range s.ExternalMcps {
		ids = append(ids, mcp.ID)
	}
	return ids
}

// ---------------------------------------------------------------------------
// Service CRUD
// ---------------------------------------------------------------------------

// AddService adds a service config. Does not save; use within store.With.
func (s *Settings) AddService(config ServiceConfig) {
	s.Services = append(s.Services, config)
}

// RemoveService removes a service by ID. Does not save; use within store.With.
func (s *Settings) RemoveService(id string) {
	s.Services = slices.DeleteFunc(s.Services, func(svc ServiceConfig) bool { return svc.ID == id })
}

// UpdateService replaces a service config by ID. Does not save; use within store.With.
func (s *Settings) UpdateService(config ServiceConfig) {
	if _, idx := s.findServiceByID(config.ID); idx >= 0 {
		s.Services[idx] = config
	}
}

// UpsertService adds or updates a service config.
// Returns true if it updated an existing entry.
// Does not save; use within store.With.
func (s *Settings) UpsertService(cfg ServiceConfig) bool {
	if _, idx := s.findServiceByID(cfg.ID); idx >= 0 {
		s.Services[idx] = cfg
		return true
	}
	s.AddService(cfg)
	return false
}

// SetServiceAutostart updates the autostart flag for a service by ID.
// Does not save; use within store.With.
func (s *Settings) SetServiceAutostart(id string, autostart bool) {
	if svc, _ := s.findServiceByID(id); svc != nil {
		svc.Autostart = autostart
	}
}

// MergeServiceDefaults fills zero-value fields in cfg from the existing service
// with the same ID. Useful when CLI flags only specify fields being changed.
// Autostart is intentionally not merged: its zero value (false) is
// indistinguishable from "user explicitly set false", so the CLI flag always wins.
// Does not save; use within store.With.
func (s *Settings) MergeServiceDefaults(cfg *ServiceConfig) {
	existing, _ := s.findServiceByID(cfg.ID)
	if existing == nil {
		return
	}
	if cfg.DisplayName == "" {
		cfg.DisplayName = existing.DisplayName
	}
	if cfg.Command == "" {
		cfg.Command = existing.Command
	}
	if cfg.Env == nil {
		cfg.Env = existing.Env
	}
	if cfg.Args == nil {
		cfg.Args = existing.Args
	}
	if cfg.WorkingDir == "" {
		cfg.WorkingDir = existing.WorkingDir
	}
	if cfg.URL == "" {
		cfg.URL = existing.URL
	}
}

// ResolveServiceID returns the ID of a service found by exact id or display name lookup.
// Returns "" if not found.
func (s *Settings) ResolveServiceID(id, name string) string {
	if id != "" {
		if _, idx := s.findServiceByID(id); idx >= 0 {
			return id
		}
		return ""
	}
	for _, svc := range s.Services {
		if svc.DisplayName == name {
			return svc.ID
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Lookup helpers — eliminate repeated linear scans
// ---------------------------------------------------------------------------

// findTokenByHash returns the token with the given hash and its index, or nil, -1.
func (s *Settings) findTokenByHash(hash string) (*StoredToken, int) {
	for i := range s.Tokens {
		if s.Tokens[i].Hash == hash {
			return &s.Tokens[i], i
		}
	}
	return nil, -1
}

// findMcpByID returns the MCP with the given ID and its index, or nil, -1.
func (s *Settings) findMcpByID(id string) (*ExternalMcp, int) {
	for i := range s.ExternalMcps {
		if s.ExternalMcps[i].ID == id {
			return &s.ExternalMcps[i], i
		}
	}
	return nil, -1
}

// findServiceByID returns the service with the given ID and its index, or nil, -1.
func (s *Settings) findServiceByID(id string) (*ServiceConfig, int) {
	for i := range s.Services {
		if s.Services[i].ID == id {
			return &s.Services[i], i
		}
	}
	return nil, -1
}
