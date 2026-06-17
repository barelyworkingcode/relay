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
	Name string
	// ProjectID is the stable id of the project this token authenticates (empty
	// for service/external tokens). Injected into _meta so an MCP can attribute a
	// call to its project without trusting LLM-supplied values.
	ProjectID     string
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
	DiscoveredTools []ToolInfo        `json:"-"`                   // runtime-only; populated from live MCP connection
	ContextSchema   json.RawMessage   `json:"-"`                   // runtime-only; discovered during MCP handshake
	Transport       string            `json:"transport,omitempty"` // "stdio" (default) or "http"
	URL             string            `json:"url,omitempty"`       // MCP endpoint for HTTP transport
	OAuthState      *OAuthState       `json:"oauth_state,omitempty"`

	// TccServices lists the macOS TCC services this MCP needs (e.g.
	// ["calendar","contacts","reminders","microphone","appleevents"]).
	// Drives the Settings UI's "Reset Permissions" button: relay runs
	// tccutil reset for each service against the MCP binary's bundle ID,
	// fires Relay-side primer prompts (so the user grants Relay the
	// services and the MCP inherits via responsible-parent attribution),
	// then spawns the MCP with --check-permissions for a final status
	// summary. See mcp_permissions.go.
	TccServices []string `json:"tcc_services,omitempty"`
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
//
// Enhancement is automatic: every spawned service receives a
// RELAY_BRIDGE_SOCKET env var. A service that implements the manifest
// protocol (see plans/service-manifest-spec.md) detects the env var,
// binds its own listener, and sends RegisterManifest to relay; thereafter
// it receives front-door traffic dispatched by relay. A generic service
// just ignores the env var — relay never dispatches to it.
type ServiceConfig struct {
	ID          string            `json:"id"`
	DisplayName string            `json:"display_name"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	WorkingDir  string            `json:"working_dir,omitempty"`
	Autostart   bool              `json:"autostart"`
	URL         string            `json:"url,omitempty"`

	// FrontendConsumer controls whether relay injects its front-door creds
	// (RELAY_FRONTEND_SOCKET/TOKEN) into the spawned service. Only a frontend
	// consumer (eve) dials the front door; backends never do. Three-state:
	// nil = inject (backward-compatible default for existing registrations);
	// false = do not inject (backends opt out so the front-door bearer never
	// lands in their env, and thus never leaks into a spawned shell); true =
	// inject explicitly. Set false via `service register --no-frontend-creds`.
	FrontendConsumer *bool `json:"frontend_consumer,omitempty"`
}

// ChatTemplate defines a reusable session preset within a project.
type ChatTemplate struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Model          string `json:"model"`
	Mode           string `json:"mode,omitempty"` // "text" | "voice"
	Voice          string `json:"voice,omitempty"`
	SystemPrompt   string `json:"system_prompt,omitempty"`
	AppendClaudeMd bool   `json:"append_claude_md,omitempty"`
	UseRelayTools  bool   `json:"use_relay_tools,omitempty"`
}

// ShellTemplate defines a project-scoped terminal launch template. Unlike the
// global shell templates in relayLLM's settings.json `pty` map, these live on
// the project record so a project can carry private shells (e.g. an ssh into a
// specific host) that are not shared with other projects. relayLLM resolves one
// by (projectID, templateID) over the bridge at launch time when its global
// store misses, then spawns it through the same path as a global template.
//
// Fields mirror the launch-relevant subset of relayLLM's TerminalTemplate. The
// relay-managed/global-only notions (BuiltIn, UseRelayToken, EnvPassthrough,
// IdleTimeout) are deliberately omitted: a project-scoped template always gets
// RELAY_PROJECT_TOKEN via its projectID launch path, and relayLLM stamps any
// remaining defaults server-side when it hydrates the bridge response. Env is a
// plain map persisted in settings.json (0600) — do not store secrets here.
type ShellTemplate struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
	Icon        string            `json:"icon,omitempty"`
}

// PermissionPolicy is a per-project Claude permission policy. Forwarded to
// relayLLM in the session Settings; relayLLM uses it for both Claude CLI
// flags (--permission-mode, --allowedTools, --disallowedTools) and to
// short-circuit permission requests in the hook (no WebSocket roundtrip
// for matched rules).
//
// Tool patterns match Claude CLI's grammar: "ToolName" matches any use,
// "ToolName:argPrefix" matches uses whose serialized input starts with
// argPrefix (e.g. "Bash:ls *").
type PermissionPolicy struct {
	DefaultMode  string   `json:"default_mode,omitempty"`  // default|acceptEdits|plan|bypassPermissions
	AllowedTools []string `json:"allowed_tools,omitempty"` // patterns
	DeniedTools  []string `json:"denied_tools,omitempty"`  // patterns
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
	// ShellTemplates are project-scoped terminal launch templates (private
	// shells like ssh that aren't shared globally). Eve is the editor; relay
	// serves them to relayLLM JIT over the bridge at launch.
	ShellTemplates []ShellTemplate `json:"shell_templates,omitempty"`
	Token          string          `json:"token"` // plaintext (settings.json is 0600)
	TokenHash      string          `json:"token_hash"`
	CreatedAt      string          `json:"created_at"`

	// Per-project tool/context scoping (derived from allowed_mcp_ids at auth time).
	DisabledTools map[string][]string        `json:"disabled_tools,omitempty"`
	Context       map[string]json.RawMessage `json:"context,omitempty"`

	// Per-project Claude permission policy.
	PermissionPolicy *PermissionPolicy `json:"permission_policy,omitempty"`

	// SessionFolders is the ordered list of folder names a project's sessions
	// can be grouped under in Eve's UI (including empty folders awaiting their
	// first session). Pure organizational metadata — relay never reads it; the
	// session→folder membership lives on the session in relayLLM.
	SessionFolders []string `json:"session_folders,omitempty"`

	// GenerateSkill controls whether out-of-band hooks (project save,
	// MCP reconcile, project delete) maintain the relay-managed skills under
	// <Path>/.claude/skills/ — one "relay-<category>" dir per tool bucket,
	// reconciled to the project's current tool surface. The PTY-launch regen
	// path is controlled per-template, not per-project, so it runs independent
	// of this flag.
	GenerateSkill bool `json:"generate_skill,omitempty"`
}

// Validate checks that required fields are present.
func (c *ServiceConfig) Validate() error {
	if c.ID == "" {
		return fmt.Errorf("service ID is required")
	}
	if !isSafeID(c.ID) {
		return fmt.Errorf("service ID %q is invalid: use only letters, digits, '.', '_', '-' (no path separators)", c.ID)
	}
	if c.DisplayName == "" {
		return fmt.Errorf("service display name is required")
	}
	if c.Command == "" {
		return fmt.Errorf("service command is required")
	}
	return nil
}
