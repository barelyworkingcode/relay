package main

import (
	"encoding/json"
	"fmt"
	"slices"
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
	ProjectID string
	// ProjectKind is the kind of the project this token authenticates. It is
	// carried because the access-mode default is ASYMMETRIC — see AccessMode —
	// and checkToolAccess therefore has to know which side of that asymmetry a
	// token is on. Empty (the zero value) is local, exactly as on Project.
	ProjectKind   ProjectKind
	Hash          string
	Permissions   map[string]Permission
	DisabledTools map[string][]string
	Context       map[string]json.RawMessage

	// Access is the per-MCP operation mode (ADR-011 decision 2), and
	// AllowedTools the per-MCP tool allowlist (decision 2b). Both are carried
	// onto the token the same way DisabledTools and Context are, so that every
	// authentication path yields the same scope.
	Access       map[string]string
	AllowedTools map[string][]string
}

// Access modes (ADR-011 decision 2). Relay applies this rule at its own
// chokepoint and records what it decided; the INPUT — whether a given tool is
// read-only — is the MCP's own readOnlyHint and is not something relay can
// check. Write implies read; there is deliberately no third mode. Nobody has named one, and an enum with a speculative member is
// a migration cost paid in advance.
const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// ToolAllowed reports whether this token's allowlist admits a tool of an MCP
// (ADR-011 decision 2b). The MCP-level grant is checked separately; this is
// the second of the four allowlists, and none of the four may widen another.
//
// The default is asymmetric for the same reason AccessMode's is, and in the
// same direction:
//
//   - An ACCESS PROFILE with no list for an MCP holds NO tools of it. That is
//     ADR-009 decision 4's reasoning one level down: a grant to another machine
//     must be an enumeration someone typed, and a tool registered later must
//     not silently join it. macMCP grew from 46 tools to 47 during ADR-010's
//     own testing.
//   - A LOCAL project with no list holds all of them, exactly as before, with
//     disabled_tools still subtracting.
//
// An empty list is the same as an absent one, deliberately: for a profile both
// mean nothing, and there is no reading under which an operator who saved an
// empty allowlist meant "everything".
func (t *StoredToken) ToolAllowed(mcpID, toolName string) bool {
	if t == nil {
		return false
	}
	patterns := t.AllowedTools[mcpID]
	if len(patterns) == 0 {
		return t.ProjectKind != ProjectKindRemote
	}
	return toolAllowedByPatterns(patterns, toolName)
}

// AccessMode resolves the operation mode in force for one MCP.
//
// THE DEFAULT IS ASYMMETRIC ON PURPOSE, and it is the point of the decision
// rather than an oversight to be tidied away later:
//
//   - An ACCESS PROFILE (a remote-kind record) with no entry defaults to
//     "read". A grant to a client on another machine that nobody has said
//     anything about must not be able to send mail, move messages or write
//     files. This is the same asymmetry ADR-009 and ADR-010 apply everywhere
//     else on this path — the threat model differs — and it means ADR-011
//     landing turns the live "Hermes Mail" profile read-only until an operator
//     says otherwise. That is the safe direction and it is loud in the UI.
//   - A LOCAL project with no entry defaults to "write". Every local project
//     that exists today was written before this field did, and silently
//     revoking every mutating tool from all of them is not a security
//     improvement, it is an outage. A local project is the operator's own
//     machine acting as itself.
//
// Anything that is not exactly "write" resolves to "read": a hand-edited
// settings.json carrying "readwrite", "rw", or a typo must narrow rather than
// widen, and there is no third mode for it to mean.
func (t *StoredToken) AccessMode(mcpID string) string {
	if t == nil {
		return AccessRead
	}
	if mode, ok := t.Access[mcpID]; ok {
		if mode == AccessWrite {
			return AccessWrite
		}
		return AccessRead
	}
	if t.ProjectKind == ProjectKindRemote {
		return AccessRead
	}
	return AccessWrite
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
	// ContextSchemaVersion is the declared contextSchemaVersion from the same
	// serverInfo. Absent or < 2 means v1 (ADR-011 decision 3). Runtime-only,
	// and it travels with ContextSchema everywhere — the version is what says
	// how the schema may be read.
	ContextSchemaVersion int         `json:"-"`
	Transport            string      `json:"transport,omitempty"` // "stdio" (default) or "http"
	URL                  string      `json:"url,omitempty"`       // MCP endpoint for HTTP transport
	OAuthState           *OAuthState `json:"oauth_state,omitempty"`

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

// ProjectKind distinguishes a project bound to a host directory from one that
// is purely a capability grant to a remote client.
type ProjectKind string

const (
	ProjectKindLocal  ProjectKind = "local"
	ProjectKindRemote ProjectKind = "remote"
)

// Project defines an infrastructure boundary: a directory, a set of MCPs,
// allowed models, chat templates, and a scoped auth token.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	// Kind distinguishes a host-directory project (default) from a remote
	// capability grant. omitempty so every project already on disk — which
	// predates this field — round-trips byte-identical, and the zero value
	// ("") deserializes into ProjectKindLocal territory. That zero value must
	// NEVER be readable as remote: an old settings.json, a hand-edited entry
	// missing the key, or a struct literal in a test all produce "", and
	// every one of those must behave exactly as a local project always has.
	// Enforce this by testing IsRemote() everywhere, never
	// `Kind == ProjectKindLocal` — the latter is false for "" too, but an
	// equality check invites someone to later write `Kind != ProjectKindRemote`
	// wrongly, or to compare against the wrong constant. IsRemote() is the one
	// place that decision is made.
	Kind          ProjectKind    `json:"kind,omitempty"`
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

	// AllowedTools is the per-MCP tool allowlist: MCP id -> patterns
	// (ADR-011 decision 2b). The MCP is the wrong unit of grant — macMCP is
	// one MCP and thirteen domains — and finding 9 measured what that costs:
	// a profile called "Hermes Mail" was authorized for capture_screenshot,
	// capture_audio, shortcuts_run, web_fetch, contacts_* and messages_send,
	// every one of them through a grant naming one mailbox.
	//
	// The access mode does not close that, because contacts_*, calendars_*,
	// messages_get_chat and web_fetch are all legitimately readOnlyHint: true
	// and survive a read grant. An allowlist is the only thing that does.
	//
	// Patterns are matched by matchToolPattern, the same anchored matcher a
	// context field's applies_to uses. Absent or empty means NO TOOLS for a
	// remote-kind record and ALL TOOLS for a local project — the same
	// asymmetry, and the same reason, as Access.
	AllowedTools map[string][]string `json:"allowed_tools,omitempty"`

	// Access is the per-MCP operation mode: MCP id -> AccessRead | AccessWrite
	// (ADR-011 decision 2). It is the layer of authority relay can decide by
	// itself, at its own chokepoint, from a declaration the MCP already
	// publishes — unlike Context, which relay injects and cannot verify.
	//
	// A missing entry does NOT mean "unset, so allow": see StoredToken.AccessMode
	// for the asymmetric default and why it is asymmetric. omitempty so every
	// project already on disk round-trips byte-identical.
	//
	// This is what makes ADR-011 finding 9 safe. disabled_tools is a denylist,
	// so it fails open on upgrade: granting an MCP grants every tool it has
	// minus whatever was enumerated AT GRANT TIME, and a tool added tomorrow
	// silently joins every existing grant. A mode derived from the MCP's own
	// readOnlyHint works on tools that did not exist when the profile was
	// written.
	Access map[string]string `json:"access,omitempty"`

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

	// AllowCwdAuth opts this project into token-less bridge auth for callers
	// whose working directory is inside Path (see AuthenticateProjectByPath).
	// Default false: without it, a tokenless caller gets nothing.
	//
	// This trades an explicit grant for convenience. With a token, a process
	// holds this project's tool surface because something deliberately handed
	// it the credential; with this flag, ANY process running as the user gets
	// that surface by standing in the directory — a stray agent in a
	// subdirectory included. It is not a privilege escalation across users
	// (settings.json is 0600 and already holds every token in plaintext), but
	// it does erase the deliberate hand-off, so it stays opt-in per project.
	AllowCwdAuth bool `json:"allow_cwd_auth,omitempty"`
}

// EnrolmentBudget bounds what one enrolled client may draw per rolling
// window. The enrolment is the unit of compromise, so it is the unit that
// carries the cap (ADR-010 decision 7): if one agent's key is stolen, the
// attacker holds that key and nothing else, and this is what bounds the
// damage.
//
// Rate and volume are budgeted together because they fail differently — a
// call cap alone does not stop a slow drain, and a mailbox exfiltrated over
// six hours is exfiltrated. Enforcement lives in appRouter.CallTool; this
// type only carries the numbers.
//
// There is deliberately no representation of "unlimited": a zero field means
// "unset", which normalizeEnrolmentBudget fills with the conservative default
// rather than reading as no limit. A budget that can be switched off by
// omitting a key from a hand-edited settings.json is a budget that will be off
// on the one host where it mattered.
type EnrolmentBudget struct {
	WindowSeconds  int   `json:"window_seconds"`
	MaxCalls       int   `json:"max_calls"`
	MaxResultBytes int64 `json:"max_result_bytes"`
}

// Enrolment binds one client certificate to the grants it may use. It is the
// whole of a remote caller's authority: there is no bearer token on the
// remote path at all (ADR-010 decision 2), so a copy of settings.json — which
// holds every project token in plaintext — grants no remote access whatsoever.
//
// An enrolment is keyed by CERTIFICATE, NOT BY MACHINE. One host may hold many
// enrolments, and that is the expected shape: several agents on one VM, each
// with its own certificate and its own grants, audited and revoked
// independently. Nothing may assume one enrolment per machine — relay cannot
// even tell that two enrolments are co-located, and the separation between
// them is exactly as strong as the filesystem isolation on the client side.
//
// Device and capability revocation stay independent: editing ProjectIDs
// changes what a client may reach without touching its certificate; deleting
// the enrolment cuts the client without disturbing any project.
type Enrolment struct {
	// ClientID is the human-readable, unique name for this enrolment. It is
	// also the certificate's Common Name and the bundle's directory name, so
	// it is restricted to the filesystem-safe charset (isSafeID).
	ClientID string `json:"client_id"`
	// Fingerprint is the FULL SHA-256 of the client certificate's DER,
	// "sha256:" + 64 hex chars. Never truncated — see FingerprintDER.
	Fingerprint string `json:"fingerprint"`
	// ProjectIDs are the grants this certificate may select among by sending
	// a project id on the wire. A project id is not a secret; relay honours it
	// only if this enrolment actually holds the grant. Every id here must name
	// a project with IsRemote() true (ValidateEnrolmentGrants).
	ProjectIDs []string        `json:"project_ids"`
	Budget     EnrolmentBudget `json:"budget"`
	CreatedAt  string          `json:"created_at"`
}

// GrantsProject reports whether this enrolment holds a grant for projectID.
// The remote listener calls this before resolving a request's project id —
// holding a certificate says who is calling, never what they may reach.
func (e *Enrolment) GrantsProject(projectID string) bool {
	return slices.Contains(e.ProjectIDs, projectID)
}

// IsRemote reports whether this project is a remote capability grant rather
// than a host-directory project. This is the ONLY place that decision should
// be made — see the comment on Kind for why the zero value must always read
// as local.
func (p *Project) IsRemote() bool {
	return p.Kind == ProjectKindRemote
}

// normalizeProjectKind collapses anything that isn't ProjectKindRemote to the
// zero value, so a local project's stored Kind is always "" — never the
// literal "local" string — regardless of whether a caller passed
// ProjectKindLocal explicitly or left it unset. Without this, a freshly
// created local project would carry "kind":"local" while an old one (or one
// loaded from a pre-remote settings.json) carries no key at all: two
// spellings of the same state. Collapsing to one keeps every local project,
// old or new, serializing identically, and keeps settings.json minimal for
// the overwhelmingly common case. Mirrors IsRemote()'s pattern of testing
// for remote rather than for local.
func normalizeProjectKind(kind ProjectKind) ProjectKind {
	if kind == ProjectKindRemote {
		return ProjectKindRemote
	}
	return ""
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
