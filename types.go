package main

import "encoding/json"

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
	DiscoveredTools []ToolInfo        `json:"discovered_tools"`
	ContextSchema   json.RawMessage   `json:"context_schema,omitempty"`
	Transport       string            `json:"transport,omitempty"`    // "stdio" (default) or "http"
	URL             string            `json:"url,omitempty"`          // MCP endpoint for HTTP transport
	OAuthState      *OAuthState       `json:"oauth_state,omitempty"`
}

// IsHTTP returns true if this MCP uses the HTTP Streamable transport.
func (m *ExternalMcp) IsHTTP() bool {
	return m.Transport == "http"
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
