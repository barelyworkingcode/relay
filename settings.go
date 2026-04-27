package main

import (
	"encoding/json"
	"slices"
)

// Settings holds all persistent Relay configuration.
type Settings struct {
	Version      int             `json:"version"`
	ExternalMcps []ExternalMcp   `json:"external_mcps"`
	Services     []ServiceConfig `json:"services"`
	Projects     []Project       `json:"projects"`
	AdminSecret  string          `json:"admin_secret,omitempty"`
}

// ---------------------------------------------------------------------------
// MCP CRUD — methods are small and cohesive with the Settings struct.
// ---------------------------------------------------------------------------

// AddExternalMcp adds an external MCP config. Does not save; use within store.With.
func (s *Settings) AddExternalMcp(mcp ExternalMcp) {
	s.ExternalMcps = append(s.ExternalMcps, mcp)
}

// UpdateExternalMcp replaces an external MCP config by ID.
// Does not save; use within store.With.
func (s *Settings) UpdateExternalMcp(cfg ExternalMcp) {
	_, idx := s.findMcpByID(cfg.ID)
	if idx < 0 {
		return
	}
	s.ExternalMcps[idx] = cfg
}

// RemoveExternalMcp removes an external MCP. Does not save; use within store.With.
func (s *Settings) RemoveExternalMcp(id string) {
	s.ExternalMcps = slices.DeleteFunc(s.ExternalMcps, func(m ExternalMcp) bool { return m.ID == id })
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
// Project CRUD
// ---------------------------------------------------------------------------

// AddProject adds a project and its associated token.
// Does not save; use within store.With.
func (s *Settings) AddProject(p Project) {
	s.Projects = append(s.Projects, p)
}

// RemoveProject removes a project by ID.
// Does not save; use within store.With.
func (s *Settings) RemoveProject(id string) {
	s.Projects = slices.DeleteFunc(s.Projects, func(p Project) bool { return p.ID == id })
}

// UpdateProjectMcps updates the allowed MCP IDs for a project and syncs
// the associated token's permissions and context.
// schemas maps MCP IDs to their runtime context schemas.
// Does not save; use within store.With.
func (s *Settings) UpdateProjectMcps(id string, mcpIDs []string, schemas map[string]json.RawMessage) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.AllowedMcpIDs = mcpIDs
	s.SyncProjectToken(proj, schemas)
}

// UpdateProjectModels updates the allowed models for a project.
// Does not save; use within store.With.
func (s *Settings) UpdateProjectModels(id string, models []string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.AllowedModels = models
}

// UpdateProjectName updates a project's name.
// Does not save; use within store.With.
func (s *Settings) UpdateProjectName(id string, name string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.Name = name
}

// UpdateProjectPath updates a project's path and syncs token context.
// schemas maps MCP IDs to their runtime context schemas.
// Does not save; use within store.With.
func (s *Settings) UpdateProjectPath(id string, path string, schemas map[string]json.RawMessage) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.Path = path
	s.SyncProjectToken(proj, schemas)
}

// SyncProjectToken updates the project's disabled tools and context to match
// its current allowedMcpIDs and path. Permissions are derived at auth time
// from AllowedMcpIDs, so they're not stored.
// schemas maps MCP IDs to their runtime context schemas (from ExternalMcpManager).
// If schemas is nil, filesystem auto-detection is skipped.
func (s *Settings) SyncProjectToken(proj *Project, schemas map[string]json.RawMessage) {
	if proj.Context == nil {
		proj.Context = make(map[string]json.RawMessage)
	}
	if proj.DisabledTools == nil {
		proj.DisabledTools = make(map[string][]string)
	}
	// Resolve which MCP IDs to configure: all registered if wildcard.
	mcpIDs := proj.AllowedMcpIDs
	if isWildcard(mcpIDs) {
		mcpIDs = s.AllExternalMcpIDs()
	}
	// Clean stale entries for MCPs no longer in the allowed set.
	allowed := make(map[string]bool, len(mcpIDs))
	for _, id := range mcpIDs {
		allowed[id] = true
	}
	for id := range proj.Context {
		if !allowed[id] {
			delete(proj.Context, id)
		}
	}
	for id := range proj.DisabledTools {
		if !allowed[id] {
			delete(proj.DisabledTools, id)
		}
	}
	for _, mcpID := range mcpIDs {
		if schemaHasField(schemas[mcpID], "allowed_dirs") {
			ctx, _ := json.Marshal(map[string]interface{}{
				"allowed_dirs": []string{proj.Path},
			})
			proj.Context[mcpID] = ctx
			// Disable fs_bash by default for filesystem MCPs.
			if !slices.Contains(proj.DisabledTools[mcpID], "fs_bash") {
				proj.DisabledTools[mcpID] = append(proj.DisabledTools[mcpID], "fs_bash")
			}
		}
	}
}

// schemaHasField checks if a context schema declares a given field.
func schemaHasField(schema json.RawMessage, field string) bool {
	if len(schema) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil {
		return false
	}
	_, ok := fields[field]
	return ok
}

// ---------------------------------------------------------------------------
// Lookup helpers — eliminate repeated linear scans
// ---------------------------------------------------------------------------

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

// findProjectByID returns the project with the given ID and its index, or nil, -1.
func (s *Settings) findProjectByID(id string) (*Project, int) {
	for i := range s.Projects {
		if s.Projects[i].ID == id {
			return &s.Projects[i], i
		}
	}
	return nil, -1
}

// findProjectByTokenHash returns the project whose token matches the given hash.
func (s *Settings) findProjectByTokenHash(hash string) *Project {
	for i := range s.Projects {
		if s.Projects[i].TokenHash == hash {
			return &s.Projects[i]
		}
	}
	return nil
}

// isWildcard returns true if the list contains a single "*" entry,
// meaning "allow all".
func isWildcard(ids []string) bool {
	return len(ids) == 1 && ids[0] == "*"
}

// AuthenticateProject validates a bearer token against project token hashes.
// Returns a synthetic StoredToken with permissions derived from AllowedMcpIDs.
func (s *Settings) AuthenticateProject(plaintext string) (*StoredToken, error) {
	if plaintext == "" {
		return nil, ErrNoToken
	}
	stored := s.AuthenticateProjectByHash(hashToken(plaintext))
	if stored == nil {
		return nil, ErrInvalidToken
	}
	return stored, nil
}

// AuthenticateProjectByHash finds a project by pre-computed token hash and
// returns a synthetic StoredToken with derived permissions. Returns nil if
// no project matches. Used by resolveAuth to avoid double-hashing.
func (s *Settings) AuthenticateProjectByHash(hash string) *StoredToken {
	proj := s.findProjectByTokenHash(hash)
	if proj == nil {
		return nil
	}
	// Wildcard: nil permissions map — checkToolAccess treats missing keys as allowed.
	if isWildcard(proj.AllowedMcpIDs) {
		return &StoredToken{
			Name:          "project:" + proj.Name,
			Hash:          hash,
			DisabledTools: proj.DisabledTools,
			Context:       proj.Context,
		}
	}
	// Explicit list: only store PermOff entries (deny-set).
	perms := make(map[string]Permission)
	allowed := make(map[string]bool, len(proj.AllowedMcpIDs))
	for _, id := range proj.AllowedMcpIDs {
		allowed[id] = true
	}
	for _, mcp := range s.ExternalMcps {
		if !allowed[mcp.ID] {
			perms[mcp.ID] = PermOff
		}
	}
	return &StoredToken{
		Name:          "project:" + proj.Name,
		Hash:          hash,
		Permissions:   perms,
		DisabledTools: proj.DisabledTools,
		Context:       proj.Context,
	}
}
