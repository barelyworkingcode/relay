package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

type Permission string

const (
	PermOff Permission = "off"
	PermOn  Permission = "on"
)

type StoredToken struct {
	Name string
	// ProjectID is injected into _meta so an MCP can attribute a call to its
	// project without trusting LLM-supplied values.
	ProjectID string
	// ProjectKind is carried because the access-mode default is asymmetric
	// (see AccessMode) and checkToolAccess must know which side of that
	// asymmetry a token is on. Empty (the zero value) is local, exactly as on
	// Project.
	ProjectKind   ProjectKind
	Hash          string
	Permissions   map[string]Permission
	DisabledTools map[string][]string
	Context       map[string]json.RawMessage

	Access       map[string]string
	AllowedTools map[string][]string

	// AllowExternal is a separate axis from Access, not a third mode — see
	// ExternalAllowed.
	AllowExternal map[string]bool
}

// Write implies read; there is deliberately no third mode — nobody has
// named one, and an enum with a speculative member is a migration cost
// paid in advance.
const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// IsRemote goes through ProjectKind.IsRemote rather than comparing Kind
// itself — see the comment on Project.Kind for why the zero value must
// never be readable as remote.
func (t *StoredToken) IsRemote() bool {
	if t == nil {
		return false
	}
	return t.ProjectKind.IsRemote()
}

// ToolAllowed's default is asymmetric: an access profile with no list for
// an MCP holds no tools of it, a local project with no list holds all of
// them. An empty list is treated the same as an absent one — never as
// "everything".
func (t *StoredToken) ToolAllowed(mcpID, toolName string) bool {
	if t == nil {
		return false
	}
	patterns := t.AllowedTools[mcpID]
	if len(patterns) == 0 {
		return !t.IsRemote()
	}
	return toolAllowedByPatterns(patterns, toolName)
}

// AccessMode's default is asymmetric on purpose: a remote access profile
// with no entry defaults to read (an unconfigured grant must not mutate
// anything), a local project defaults to write (every project written
// before this field existed must keep working). Anything other than
// exactly "write" resolves to read, so a typo in a hand-edited
// settings.json narrows rather than widens.
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
	if t.IsRemote() {
		return AccessRead
	}
	return AccessWrite
}

// ExternalAllowed is orthogonal to AccessMode, not a value of it — a tool
// can be read-only and reach the network (web_fetch) or mutate and stay
// local (mail_create_draft). The default is asymmetric like AccessMode: a
// remote profile defaults to refused (relay is its only path off the
// host), a local project defaults to allowed (its agent already has the
// host's network). An explicit value always wins in both directions, which
// is why this is map[string]bool rather than a set — false has to be a
// value someone can write.
func (t *StoredToken) ExternalAllowed(mcpID string) bool {
	if t == nil {
		return false
	}
	if allowed, ok := t.AllowExternal[mcpID]; ok {
		return allowed
	}
	return !t.IsRemote()
}

type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
}

// ClientSecret, AccessToken and RefreshToken are the bearers relay presents
// upstream (or that mint one indefinitely); all three are sealed (§4.1).
// ClientID and TokenExpiry stay clear: relay only checks them, never hands
// them to anything.
type OAuthState struct {
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret Secret `json:"client_secret,omitempty"`
	AccessToken  Secret `json:"access_token,omitempty"`
	RefreshToken Secret `json:"refresh_token,omitempty"`
	TokenExpiry  string `json:"token_expiry,omitempty"`
}

type ExternalMcp struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args"`
	// Env values are sealed; the keys stay clear so an operator reading
	// settings.json can still see which variables this MCP receives (§4.3).
	Env             map[string]Secret `json:"env"`
	DiscoveredTools []ToolInfo        `json:"-"`
	ContextSchema   json.RawMessage   `json:"-"`
	// ContextSchemaVersion absent or < 2 means v1 (ADR-011 decision 3); it
	// travels with ContextSchema everywhere since the version says how the
	// schema may be read.
	ContextSchemaVersion int         `json:"-"`
	Transport            string      `json:"transport,omitempty"`
	URL                  string      `json:"url,omitempty"`
	OAuthState           *OAuthState `json:"oauth_state,omitempty"`

	// TccServices drives the Settings UI's "Reset Permissions" button: relay
	// runs tccutil reset against the MCP binary's bundle ID and fires its own
	// primer prompts so the MCP inherits the grant via TCC's
	// responsible-parent attribution. See mcp_permissions.go.
	TccServices []string `json:"tcc_services,omitempty"`

	// ResolvedRoot is set by prepareStdioLaunch at spawn time — a fact relay
	// derives from its own configuration, not something the MCP declares, so
	// it travels beside ContextSchema rather than inside it. Empty means
	// relay did not spawn this MCP with a --root argument.
	ResolvedRoot string `json:"-"`
}

func (m *ExternalMcp) IsHTTP() bool {
	return m.Transport == "http"
}

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

// ServiceConfig describes a background service managed by relay. A service
// that implements the manifest protocol detects RELAY_BRIDGE_SOCKET, binds
// its own listener, and sends RegisterManifest to relay; a generic service
// ignores the env var and relay never dispatches to it.
type ServiceConfig struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	// Env values are sealed; the keys stay clear (§4.3), same as ExternalMcp.Env.
	Env        map[string]Secret `json:"env"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Autostart  bool              `json:"autostart"`
	URL        string            `json:"url,omitempty"`

	// FrontendConsumer is tri-state: nil injects relay's front-door creds
	// (RELAY_FRONTEND_SOCKET/TOKEN) for backward compatibility, false
	// withholds them so they never land in a backend's env, true injects
	// explicitly. Set false via `service register --no-frontend-creds`.
	FrontendConsumer *bool `json:"frontend_consumer,omitempty"`
}

type ChatTemplate struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Model          string `json:"model"`
	Mode           string `json:"mode,omitempty"`
	Voice          string `json:"voice,omitempty"`
	SystemPrompt   string `json:"system_prompt,omitempty"`
	AppendClaudeMd bool   `json:"append_claude_md,omitempty"`
	UseRelayTools  bool   `json:"use_relay_tools,omitempty"`
}

// ShellTemplate is a project-scoped terminal launch template, unlike the
// global ones in relayLLM's settings.json `pty` map, so a project can carry
// private shells (e.g. an ssh into a specific host) not shared elsewhere.
// The global-only fields on relayLLM's TerminalTemplate (BuiltIn,
// UseRelayToken, EnvPassthrough, IdleTimeout) are deliberately omitted: a
// project-scoped template always gets RELAY_PROJECT_TOKEN via its
// projectID launch path, and relayLLM stamps remaining defaults
// server-side. Env is a plain map persisted in settings.json (0600) — do
// not store secrets here.
type ShellTemplate struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
	Icon        string            `json:"icon,omitempty"`
}

// PermissionPolicy is forwarded to relayLLM for both Claude CLI flags and to
// short-circuit permission requests in the hook. Tool patterns follow Claude
// CLI's grammar: "ToolName" matches any use, "ToolName:argPrefix" matches
// uses whose serialized input starts with argPrefix (e.g. "Bash:ls *").
type PermissionPolicy struct {
	DefaultMode  string   `json:"default_mode,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
	DeniedTools  []string `json:"denied_tools,omitempty"`
}

type ProjectKind string

const (
	ProjectKindLocal  ProjectKind = "local"
	ProjectKindRemote ProjectKind = "remote"
)

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
	Kind           ProjectKind     `json:"kind,omitempty"`
	AllowedMcpIDs  []string        `json:"allowed_mcp_ids"`
	AllowedModels  []string        `json:"allowed_models"`
	ChatTemplates  []ChatTemplate  `json:"chat_templates,omitempty"`
	ShellTemplates []ShellTemplate `json:"shell_templates,omitempty"`
	// Token is sealed (§4.1); TokenHash is the SHA-256 AuthenticateProject
	// actually resolves against and stays clear. The two are the same length
	// in hex and are NOT distinguishable by shape — never seal or clear one
	// by looking at the other, only by which field this is.
	Token     Secret `json:"token"`
	TokenHash string `json:"token_hash"`
	CreatedAt string `json:"created_at"`

	DisabledTools map[string][]string        `json:"disabled_tools,omitempty"`
	Context       map[string]json.RawMessage `json:"context,omitempty"`

	// AllowedTools is the per-MCP tool allowlist (ADR-011 decision 2b) — see
	// StoredToken.ToolAllowed for the asymmetric default.
	AllowedTools map[string][]string `json:"allowed_tools,omitempty"`

	// Access is the per-MCP operation mode (ADR-011 decision 2) — see
	// StoredToken.AccessMode for the asymmetric default and why it is
	// asymmetric.
	Access map[string]string `json:"access,omitempty"`

	// AllowExternal is the per-MCP outbound grant (ADR-011 decision 2c) —
	// see StoredToken.ExternalAllowed for the asymmetric default.
	AllowExternal map[string]bool `json:"allow_external,omitempty"`

	PermissionPolicy *PermissionPolicy `json:"permission_policy,omitempty"`

	// SessionFolders is pure organizational metadata for Eve's UI; relay
	// never reads it, and session→folder membership lives on the session in
	// relayLLM.
	SessionFolders []string `json:"session_folders,omitempty"`

	// GenerateSkill controls whether out-of-band hooks maintain the
	// relay-managed skill dirs under <Path>/.claude/skills/. The PTY-launch
	// regen path is controlled per-template, not per-project, so it runs
	// independent of this flag.
	GenerateSkill bool `json:"generate_skill,omitempty"`

	// AllowCwdAuth opts this project into token-less bridge auth for a
	// caller whose working directory is inside Path (see
	// AuthenticateProjectByPath). Default false.
	//
	// This trades an explicit grant for convenience. With a token, a process
	// holds this project's tool surface because something deliberately
	// handed it the credential; with this flag, any process running as the
	// user gets that surface by standing in the directory — a stray agent in
	// a subdirectory included. It is not a privilege escalation across users
	// (settings.json is 0600 and already holds every token in plaintext),
	// but it does erase the deliberate hand-off, so it stays opt-in.
	AllowCwdAuth bool `json:"allow_cwd_auth,omitempty"`
}

// EnrolmentBudget bounds what one enrolled client may draw per rolling
// window. The enrolment, not the project, carries the cap (ADR-010 decision
// 7) since it is the unit of compromise. There is deliberately no
// representation of "unlimited": a zero field means "unset", which
// normalizeEnrolmentBudget fills with the conservative default.
type EnrolmentBudget struct {
	WindowSeconds  int   `json:"window_seconds"`
	MaxCalls       int   `json:"max_calls"`
	MaxResultBytes int64 `json:"max_result_bytes"`
}

// Enrolment binds one client certificate to the grants it may use. It is
// the whole of a remote caller's authority: there is no bearer token on the
// remote path at all (ADR-010 decision 2), so a copy of settings.json —
// which holds every project token in plaintext — grants no remote access
// whatsoever.
//
// An enrolment is keyed by CERTIFICATE, NOT BY MACHINE. One host may hold
// many enrolments: several agents on one VM, each with its own certificate
// and its own grants, audited and revoked independently. Nothing may assume
// one enrolment per machine.
type Enrolment struct {
	// ClientID is also the certificate's Common Name and the bundle's
	// directory name, so it is restricted to the filesystem-safe charset
	// (isSafeID).
	ClientID string `json:"client_id"`
	// Fingerprint is the FULL SHA-256 of the client certificate's DER,
	// "sha256:" + 64 hex chars. Never truncated — see FingerprintDER.
	Fingerprint string `json:"fingerprint"`
	// ProjectIDs are the grants this certificate may select among by
	// sending a project id on the wire. Every id here must name a project
	// with IsRemote() true (ValidateEnrolmentGrants).
	ProjectIDs []string        `json:"project_ids"`
	Budget     EnrolmentBudget `json:"budget"`
	CreatedAt  string          `json:"created_at"`
	// SPKISHA256 is the hex SHA-256 of the certificate's SubjectPublicKeyInfo,
	// set only by the CSR path. Absent on every record written before this
	// field existed, and absent never collides.
	SPKISHA256 string `json:"spki_sha256,omitempty"`
	// CLIAdmin lets this certificate reach the remote listener's configuration
	// plane, scoped to the access profiles it already holds. Absent means off,
	// so every record written before this field existed round-trips unchanged.
	CLIAdmin bool `json:"cli_admin,omitempty"`
}

// GrantsProject is checked before resolving a request's project id —
// holding a certificate says who is calling, never what they may reach.
func (e *Enrolment) GrantsProject(projectID string) bool {
	return slices.Contains(e.ProjectIDs, projectID)
}

// APICredential binds a bearer token to the control-plane capability
// classes it may exercise (ADR-015 decision 3). Only Hash is stored — the
// plaintext is returned once, by Mint, and never again.
type APICredential struct {
	ID      string            `json:"id"`
	Name    string            `json:"name,omitempty"`
	Hash    string            `json:"hash"`
	Classes []CapabilityClass `json:"classes"`
	Created string            `json:"created,omitempty"`
	// Expires is RFC3339 and ABSENT MEANS NEVER, so every record written
	// before this field existed round-trips unchanged — the same zero-value
	// discipline Project.Kind follows (ADR-016 decision 3).
	Expires string `json:"expires,omitempty"`
}

// Expired reports whether c may no longer authenticate at now. The instant
// named by Expires is already past it, matching how a deadline reads
// everywhere else.
//
// This is deliberate: an Expires that will not parse reads as EXPIRED, not
// as absent. Absent is a value relay writes on purpose and means never;
// unparseable is a value relay cannot evaluate, and the only safe answer to
// a lifetime it cannot read is that the lifetime is over.
func (c APICredential) Expired(now time.Time) bool {
	if c.Expires == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, c.Expires)
	if err != nil {
		return true
	}
	return !now.Before(at)
}

// Grants reports whether the credential holds class. A nil or empty
// Classes grants nothing — never "everything" — so a credential minted by
// a tool that predates the class model is inert rather than omnipotent.
// An unknown string in Classes simply never equals a real CapabilityClass
// constant, so it grants nothing without needing to be rejected up front.
func (c APICredential) Grants(class CapabilityClass) bool {
	return slices.Contains(c.Classes, class)
}

// LoginBootstrap anchors passkey registration to a process already running
// as the owning user (ADR-016 decision 2). Only the code's SHA-256 is
// stored, never the code itself, matching every other credential in this
// file; there is at most one at a time, so minting a new one replaces
// whatever was there.
type LoginBootstrap struct {
	Hash    string `json:"hash"`
	Expires string `json:"expires"`
}

// Passkey is one registered WebAuthn credential (ADR-016 decisions 2 and 7).
// Only public material is stored: X and Y are the COSE ES256 public key's
// coordinates, never a private key, which never leaves the authenticator.
type Passkey struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	X    []byte `json:"x"`
	Y    []byte `json:"y"`
	// SignCount is the authenticator's signature counter as of the last
	// accepted assertion (or registration, initially).
	SignCount uint32 `json:"sign_count"`
	// CounterSupported is fixed at registration: a registration reporting
	// counter 0 means this authenticator does not implement counters at
	// all, which is the ordinary case for a synced passkey. Recording that
	// fact per credential, rather than re-deriving it per assertion, is
	// what keeps a later legitimate assertion of 0 from being mistaken for
	// a cloned-authenticator replay (ADR-016 decision 7, point 10).
	CounterSupported bool   `json:"counter_supported"`
	UserHandle       string `json:"user_handle,omitempty"`
	Created          string `json:"created,omitempty"`
}

// IsRemote is the one place this comparison is written — see the comment on
// Project.Kind for why the zero value must always read as local. Every
// holder of a Kind (a Project, a StoredToken) asks here rather than
// comparing against a constant itself.
func (k ProjectKind) IsRemote() bool {
	return k == ProjectKindRemote
}

func (p *Project) IsRemote() bool {
	return p.Kind.IsRemote()
}

// normalizeProjectKind collapses anything that isn't ProjectKindRemote to
// the zero value, so a local project's stored Kind is always "" — never the
// literal "local" — regardless of whether a caller passed ProjectKindLocal
// explicitly or left it unset. This keeps every local project, old or new,
// serializing identically, and keeps settings.json minimal for the
// overwhelmingly common case.
func normalizeProjectKind(kind ProjectKind) ProjectKind {
	if kind.IsRemote() {
		return ProjectKindRemote
	}
	return ""
}

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
