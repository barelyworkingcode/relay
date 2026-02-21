package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Permission levels for token-based access control.
type Permission string

const (
	PermOff Permission = "off"
	PermOn  Permission = "on"
)

// StoredToken is a hashed token with per-service permissions.
type StoredToken struct {
	Name          string                         `json:"name"`
	Hash          string                         `json:"hash"`
	Prefix        string                         `json:"prefix"`
	Suffix        string                         `json:"suffix"`
	CreatedAt     string                         `json:"created_at"`
	Permissions   map[string]Permission          `json:"permissions"`
	DisabledTools map[string][]string            `json:"disabled_tools,omitempty"`
	Context       map[string]json.RawMessage     `json:"context,omitempty"`
}

// ToolInfo describes a discovered tool from an external MCP server.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
}

// ExternalMcp describes a stdio-based MCP server managed by Relay.
type ExternalMcp struct {
	ID              string            `json:"id"`
	DisplayName     string            `json:"display_name"`
	Command         string            `json:"command"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env"`
	DiscoveredTools []ToolInfo        `json:"discovered_tools"`
	ContextSchema   json.RawMessage   `json:"context_schema,omitempty"`
}

// ServiceConfig describes a background service managed by Relay.
type ServiceConfig struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"display_name"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	WorkingDir  string            `json:"working_dir,omitempty"`
	Autostart   bool              `json:"autostart"`
	URL         string            `json:"url,omitempty"`
}

// Settings holds all persistent Relay configuration.
type Settings struct {
	Tokens       []StoredToken   `json:"tokens"`
	ExternalMcps []ExternalMcp   `json:"external_mcps"`
	Services     []ServiceConfig `json:"services"`

	mu sync.Mutex `json:"-"`
}

// settingsDir returns the platform config directory for relay.
func settingsDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir, _ = os.UserHomeDir()
	}
	return filepath.Join(dir, "relay")
}

// settingsPath returns the full path to settings.json.
func settingsPath() string {
	return filepath.Join(settingsDir(), "settings.json")
}

func defaultSettings() *Settings {
	return &Settings{
		Tokens:       []StoredToken{},
		ExternalMcps: []ExternalMcp{},
		Services:     []ServiceConfig{},
	}
}

// LoadSettings reads settings from disk or returns defaults.
func LoadSettings() *Settings {
	data, err := os.ReadFile(settingsPath())
	if err != nil {
		return defaultSettings()
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return defaultSettings()
	}
	// Ensure slices are non-nil for JSON serialization.
	if s.Tokens == nil {
		s.Tokens = []StoredToken{}
	}
	if s.ExternalMcps == nil {
		s.ExternalMcps = []ExternalMcp{}
	}
	if s.Services == nil {
		s.Services = []ServiceConfig{}
	}
	for i := range s.ExternalMcps {
		if s.ExternalMcps[i].DiscoveredTools == nil {
			s.ExternalMcps[i].DiscoveredTools = []ToolInfo{}
		}
	}
	return &s
}

// Save writes settings to disk as pretty-printed JSON.
func (s *Settings) Save() {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := settingsDir()
	_ = os.MkdirAll(dir, 0755)

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to serialize settings: %v\n", err)
		return
	}
	if err := os.WriteFile(settingsPath(), data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write settings: %v\n", err)
	}
}

func hashToken(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}

// GenerateToken creates a new random token. Returns the plaintext (shown once)
// and the StoredToken (persisted with hash only).
func GenerateToken(name string, defaultPermissions map[string]Permission) (string, StoredToken) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	plaintext := hex.EncodeToString(bytes[:])
	hash := hashToken(plaintext)

	token := StoredToken{
		Name:        name,
		Hash:        hash,
		Prefix:      hash[:6],
		Suffix:      hash[len(hash)-6:],
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Permissions: defaultPermissions,
	}
	return plaintext, token
}

// CheckAuth validates a bearer token against stored hashes.
// Returns nil on success, or an error describing the failure.
func (s *Settings) CheckAuth(token string) error {
	if len(s.Tokens) == 0 {
		return fmt.Errorf("no tokens configured")
	}
	if token == "" {
		return fmt.Errorf("no token provided")
	}
	hash := hashToken(token)
	for _, t := range s.Tokens {
		if t.Hash == hash {
			return nil
		}
	}
	return fmt.Errorf("invalid token")
}

// ValidateToken returns the matching StoredToken if the plaintext is valid.
func (s *Settings) ValidateToken(plaintext string) *StoredToken {
	hash := hashToken(plaintext)
	for i := range s.Tokens {
		if s.Tokens[i].Hash == hash {
			return &s.Tokens[i]
		}
	}
	return nil
}

// GetPermission returns the permission level for a token+service pair.
// Defaults to PermOn if not explicitly set. Legacy "read"/"full" values are treated as PermOn.
func (s *Settings) GetPermission(tokenHash, serviceName string) Permission {
	for _, t := range s.Tokens {
		if t.Hash == tokenHash {
			if p, ok := t.Permissions[serviceName]; ok {
				if p == PermOff {
					return PermOff
				}
				return PermOn
			}
			return PermOn
		}
	}
	return PermOn
}

// DeleteToken removes a token by its hash.
func (s *Settings) DeleteToken(hash string) {
	filtered := s.Tokens[:0]
	for _, t := range s.Tokens {
		if t.Hash != hash {
			filtered = append(filtered, t)
		}
	}
	s.Tokens = filtered
	s.Save()
}

// RevokeAll removes all tokens.
func (s *Settings) RevokeAll() {
	s.Tokens = []StoredToken{}
	s.Save()
}

// UpdatePermission sets a specific permission and persists.
func (s *Settings) UpdatePermission(hash, service string, perm Permission) {
	for i := range s.Tokens {
		if s.Tokens[i].Hash == hash {
			if s.Tokens[i].Permissions == nil {
				s.Tokens[i].Permissions = make(map[string]Permission)
			}
			s.Tokens[i].Permissions[service] = perm
			break
		}
	}
	s.Save()
}

// AddExternalMcp adds an external MCP config and grants full permission to all existing tokens.
func (s *Settings) AddExternalMcp(mcp ExternalMcp) {
	s.ExternalMcps = append(s.ExternalMcps, mcp)
	for i := range s.Tokens {
		if s.Tokens[i].Permissions == nil {
			s.Tokens[i].Permissions = make(map[string]Permission)
		}
		if _, exists := s.Tokens[i].Permissions[mcp.ID]; !exists {
			s.Tokens[i].Permissions[mcp.ID] = PermOn
		}
	}
	s.Save()
}

// UpdateExternalMcp replaces an external MCP config by ID and persists.
// Preserves DiscoveredTools from the existing entry if the new one has none.
func (s *Settings) UpdateExternalMcp(cfg ExternalMcp) {
	for i := range s.ExternalMcps {
		if s.ExternalMcps[i].ID == cfg.ID {
			if len(cfg.DiscoveredTools) == 0 {
				cfg.DiscoveredTools = s.ExternalMcps[i].DiscoveredTools
			}
			s.ExternalMcps[i] = cfg
			break
		}
	}
	s.Save()
}

// RemoveExternalMcp removes an external MCP and cleans up token permissions.
func (s *Settings) RemoveExternalMcp(id string) {
	filtered := s.ExternalMcps[:0]
	for _, m := range s.ExternalMcps {
		if m.ID != id {
			filtered = append(filtered, m)
		}
	}
	s.ExternalMcps = filtered
	for i := range s.Tokens {
		delete(s.Tokens[i].Permissions, id)
		delete(s.Tokens[i].DisabledTools, id)
	}
	s.Save()
}

// AddService adds a service config and persists.
func (s *Settings) AddService(config ServiceConfig) {
	s.Services = append(s.Services, config)
	s.Save()
}

// RemoveService removes a service by ID and persists.
func (s *Settings) RemoveService(id string) {
	filtered := s.Services[:0]
	for _, svc := range s.Services {
		if svc.ID != id {
			filtered = append(filtered, svc)
		}
	}
	s.Services = filtered
	s.Save()
}

// UpdateService replaces a service config by ID and persists.
func (s *Settings) UpdateService(config ServiceConfig) {
	for i := range s.Services {
		if s.Services[i].ID == config.ID {
			s.Services[i] = config
			break
		}
	}
	s.Save()
}

// IsToolDisabled returns true if a specific tool is disabled for the given token and MCP.
func (s *Settings) IsToolDisabled(tokenHash, mcpID, toolName string) bool {
	for _, t := range s.Tokens {
		if t.Hash == tokenHash {
			if t.DisabledTools == nil {
				return false
			}
			for _, name := range t.DisabledTools[mcpID] {
				if name == toolName {
					return true
				}
			}
			return false
		}
	}
	return false
}

// SetToolDisabled enables or disables a specific tool for a token+MCP pair.
func (s *Settings) SetToolDisabled(hash, mcpID, toolName string, disabled bool) {
	for i := range s.Tokens {
		if s.Tokens[i].Hash != hash {
			continue
		}
		if s.Tokens[i].DisabledTools == nil {
			s.Tokens[i].DisabledTools = make(map[string][]string)
		}
		list := s.Tokens[i].DisabledTools[mcpID]
		if disabled {
			for _, n := range list {
				if n == toolName {
					s.Save()
					return
				}
			}
			s.Tokens[i].DisabledTools[mcpID] = append(list, toolName)
		} else {
			filtered := list[:0]
			for _, n := range list {
				if n != toolName {
					filtered = append(filtered, n)
				}
			}
			if len(filtered) == 0 {
				delete(s.Tokens[i].DisabledTools, mcpID)
			} else {
				s.Tokens[i].DisabledTools[mcpID] = filtered
			}
		}
		break
	}
	s.Save()
}

// SetAllToolsDisabled sets all tools for a token+MCP pair to disabled or enabled.
func (s *Settings) SetAllToolsDisabled(hash, mcpID string, toolNames []string, disabled bool) {
	for i := range s.Tokens {
		if s.Tokens[i].Hash != hash {
			continue
		}
		if s.Tokens[i].DisabledTools == nil {
			s.Tokens[i].DisabledTools = make(map[string][]string)
		}
		if disabled {
			names := make([]string, len(toolNames))
			copy(names, toolNames)
			s.Tokens[i].DisabledTools[mcpID] = names
		} else {
			delete(s.Tokens[i].DisabledTools, mcpID)
		}
		break
	}
	s.Save()
}

// SetContext sets per-MCP context for a token. Context is passed as _meta to
// the external MCP on tool calls, enabling per-token restrictions like allowed_dirs.
func (s *Settings) SetContext(hash, mcpID string, ctx json.RawMessage) {
	for i := range s.Tokens {
		if s.Tokens[i].Hash != hash {
			continue
		}
		if s.Tokens[i].Context == nil {
			s.Tokens[i].Context = make(map[string]json.RawMessage)
		}
		if ctx == nil || len(ctx) == 0 || string(ctx) == "null" {
			delete(s.Tokens[i].Context, mcpID)
		} else {
			s.Tokens[i].Context[mcpID] = ctx
		}
		break
	}
	s.Save()
}

// UpdateDiscoveredTools updates the persisted tool list for an external MCP.
func (s *Settings) UpdateDiscoveredTools(mcpID string, tools []ToolInfo) {
	for i := range s.ExternalMcps {
		if s.ExternalMcps[i].ID == mcpID {
			s.ExternalMcps[i].DiscoveredTools = tools
			break
		}
	}
	s.Save()
}

// UpdateContextSchema updates the persisted context schema for an external MCP.
func (s *Settings) UpdateContextSchema(mcpID string, schema json.RawMessage) {
	for i := range s.ExternalMcps {
		if s.ExternalMcps[i].ID == mcpID {
			s.ExternalMcps[i].ContextSchema = schema
			break
		}
	}
	s.Save()
}

// AllServiceNames returns all external MCP service names.
func (s *Settings) AllServiceNames() []string {
	names := make([]string, 0, len(s.ExternalMcps))
	for _, mcp := range s.ExternalMcps {
		names = append(names, mcp.ID)
	}
	return names
}
