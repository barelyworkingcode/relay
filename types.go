package main

import (
	"encoding/json"
	"fmt"
)

// Permission levels for token-based access control.
type Permission string

const (
	PermOff Permission = "off"
	PermOn  Permission = "on"
)

// StoredToken represents resolved auth credentials with per-MCP permissions.
// Used by the router for service tokens and project token views.
type StoredToken struct {
	Name          string
	Hash          string
	Permissions   map[string]Permission
	DisabledTools map[string][]string
	Context       map[string]json.RawMessage
}

// ToolInfo describes a discovered tool from an external MCP server.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
}

// OAuthState stores OAuth 2.1 credentials for HTTP MCP servers.
type OAuthState struct {
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenExpiry  string `json:"token_expiry,omitempty"`
}

// ExternalMcp describes an MCP server managed by Relay.
type ExternalMcp struct {
	ID              string            `json:"id"`
	DisplayName     string            `json:"display_name"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env"`
	DiscoveredTools []ToolInfo        `json:"-"` // runtime-only; populated from live MCP connection
	ContextSchema   json.RawMessage   `json:"-"` // runtime-only; discovered during MCP handshake
	Transport       string            `json:"transport,omitempty"`    // "stdio" (default) or "http"
	URL             string            `json:"url,omitempty"`          // MCP endpoint for HTTP transport
	OAuthState      *OAuthState       `json:"oauth_state,omitempty"`
}

// IsHTTP returns true if this MCP uses the HTTP Streamable transport.
func (m *ExternalMcp) IsHTTP() bool {
	return m.Transport == "http"
}

// Validate checks that required fields are present for the configured transport.
func (m *ExternalMcp) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("MCP ID is required")
	}
	if m.DisplayName == "" {
		return fmt.Errorf("MCP display name is required")
	}
	if m.IsHTTP() {
		if m.URL == "" {
			return fmt.Errorf("URL is required for HTTP transport")
		}
	} else {
		if m.Command == "" {
			return fmt.Errorf("command is required for stdio transport")
		}
	}
	return nil
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

// ChatTemplate defines a reusable session preset within a project.
type ChatTemplate struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Model        string `json:"model"`
	Mode         string `json:"mode,omitempty"`            // "text" | "voice"
	Voice        string `json:"voice,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
}

// Project defines an infrastructure boundary: a directory, a set of MCPs,
// allowed models, chat templates, and a scoped auth token.
type Project struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Path          string         `json:"path"`
	AllowedMcpIDs []string       `json:"allowed_mcp_ids"`
	AllowedModels []string       `json:"allowed_models"`
	ChatTemplates []ChatTemplate `json:"chat_templates,omitempty"`
	Token         string         `json:"token"`      // plaintext (settings.json is 0600)
	TokenHash     string         `json:"token_hash"`
	CreatedAt     string         `json:"created_at"`

	// Per-project tool/context scoping (derived from allowed_mcp_ids at auth time).
	DisabledTools map[string][]string        `json:"disabled_tools,omitempty"`
	Context       map[string]json.RawMessage `json:"context,omitempty"`
}

// Validate checks that required fields are present.
func (c *ServiceConfig) Validate() error {
	if c.ID == "" {
		return fmt.Errorf("service ID is required")
	}
	if c.DisplayName == "" {
		return fmt.Errorf("service display name is required")
	}
	if c.Command == "" {
		return fmt.Errorf("service command is required")
	}
	return nil
}
