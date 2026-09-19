package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/control"
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

	// Capabilities is what this service's launch identity may do
	// (docs/launch-identity.md). Every record relay writes spells the empty set
	// as []; a nil here exists only on a record read from a file written before
	// the field existed, and load migrates it from LegacyFrontendConsumer.
	Capabilities []ServiceCapability `json:"capabilities"`

	// LegacyFrontendConsumer is read only to migrate a record that predates
	// Capabilities, and is never written back.
	LegacyFrontendConsumer *bool `json:"frontend_consumer,omitempty"`

	// AllowedModels grants a service holding the models capability which
	// models it may call, read live on every model-endpoint request (docs/
	// model-endpoint.md). Absent or empty means NO models — the opposite of
	// a project's own AllowedModels default (spec-model-broker.md decision
	// 4): a project with no list means "any model", but a service is
	// deliberately opt-in, since a service (TTS, STT) is usually meant to
	// reach exactly one model rather than everything relayLLM serves.
	// ["*"] means every model, same spelling as a project's wildcard.
	AllowedModels []string `json:"allowed_models,omitempty"`
}

// ServiceCapability names one thing a service's launch identity may do.
type ServiceCapability string

const (
	// ServiceCapabilityFrontend is the frontend socket as
	// read+configure+proxy+execute (never grant); see
	// cmd/relay/api_credential.go's frontendConsumerClasses for what execute
	// does and doesn't gate on this socket.
	ServiceCapabilityFrontend ServiceCapability = "frontend"
	// ServiceCapabilityManifest is RegisterManifest under the service's own id.
	ServiceCapabilityManifest ServiceCapability = "manifest"
	// ServiceCapabilityModels grants model-endpoint calls (OpModelCall) and
	// the unfiltered model list (OpModelList), limited by AllowedModels.
	ServiceCapabilityModels ServiceCapability = "models"
	// ServiceCapabilityModelHost grants RegisterModelHost: registering this
	// service's router socket as the model endpoint's upstream. At most one
	// host may be live at a time (docs/model-endpoint.md).
	ServiceCapabilityModelHost ServiceCapability = "model_host"
	// ServiceCapabilitySessions grants SessionExited and the unfiltered model
	// list only (plan-broker-and-sessions.md §2 C1) — no model calls, no
	// project authority. Only the built-in RelaySessionsServiceID record may
	// ever hold it (validateCapabilities); a user-authored or registered
	// record naming it fails validation.
	ServiceCapabilitySessions ServiceCapability = "sessions"

	// retiredServiceCapabilityProjects named ResolvePtyEnv, ResolveProjectTemplate,
	// ListProjects, GetProject and tokenless service-scope ListTools/CallTool
	// (plan-broker-and-sessions.md §2 C1, "Deleted"). Those operations no
	// longer exist, so the name is never valid to write or register — a
	// record loaded holding it has the name silently dropped
	// (dropRetiredCapabilities), never refused, so an existing install keeps
	// starting across the upgrade.
	retiredServiceCapabilityProjects ServiceCapability = "projects"
)

// ServiceCapabilities is every capability relay knows.
var ServiceCapabilities = []ServiceCapability{
	ServiceCapabilityFrontend, ServiceCapabilityManifest,
	ServiceCapabilityModels, ServiceCapabilityModelHost, ServiceCapabilitySessions,
}

// RelaySessionsServiceID is the one service record allowed to hold
// ServiceCapabilitySessions (plan-broker-and-sessions.md §2 C1). This unit
// does not create that record — R-S9 does — it only enforces the rule that
// nothing else may ever be granted the capability.
const RelaySessionsServiceID = "relaysessions"

// RelaySessionsManifestRoutes are the routes relay-sessions registers in its
// own manifest (cmd/relaysessions): two path prefixes shared with relay's own
// routes (sessionHostSharedPrefixes in cmd/relay/enhanced_services.go, kept
// as its own literal rather than derived from this slice, deliberately —
// see that variable's own doc comment) plus two bare paths, /api/models and
// /ws, that relay never serves itself and so need no such exemption.
var RelaySessionsManifestRoutes = []string{"/api/terminals/", "/api/sessions/", "/api/models", "/ws"}

// HasCapability reports whether the record grants want.
func (c *ServiceConfig) HasCapability(want ServiceCapability) bool {
	return slices.Contains(c.Capabilities, want)
}

// sanitizeIfBuiltin clears the one field a stored record must never
// control for the built-in RelaySessionsServiceID (SH §2.1): relay always
// resolves that record's Command (and the Args that go with it) fresh, at
// every start, from its own bundle path — never from settings.json.
// DisplayName, Capabilities and Autostart are left alone; §2.1 names
// Autostart as genuinely the operator's, and the others are inert without
// a Command internal/service trusts (validateCapabilities already refuses
// ServiceCapabilitySessions everywhere else). Runs on every load, so a
// hand-edited or stale settings.json can never smuggle a Command past
// internal/service's own synthesis.
func (c *ServiceConfig) sanitizeIfBuiltin() {
	if c.ID != RelaySessionsServiceID {
		return
	}
	c.Command = ""
	c.Args = nil
}

// migrateCapabilities converts a record that predates Capabilities, once, on
// load: frontend_consumer unset or true becomes [frontend], false becomes
// [manifest], the set each kind of service held before capabilities were
// named minus the now-retired projects capability (dropRetiredCapabilities
// handles a record that already spells it out explicitly). A record that
// already carries capabilities keeps them, and the legacy field is dropped
// either way so it is never written back.
func (c *ServiceConfig) migrateCapabilities() {
	if c.Capabilities == nil {
		if c.LegacyFrontendConsumer != nil && !*c.LegacyFrontendConsumer {
			c.Capabilities = []ServiceCapability{ServiceCapabilityManifest}
		} else {
			c.Capabilities = []ServiceCapability{ServiceCapabilityFrontend}
		}
	}
	c.LegacyFrontendConsumer = nil
	c.dropRetiredCapabilities()
}

// dropRetiredCapabilities silently removes a capability name relay no longer
// recognises as valid to hold from a record loaded off disk, logs the drop
// at info, and leaves the record to persist without it on the next write —
// never a hard error, since refusing to start an existing install over a
// capability whose operations were deleted out from under it would be a
// self-inflicted outage.
func (c *ServiceConfig) dropRetiredCapabilities() {
	before := len(c.Capabilities)
	c.Capabilities = slices.DeleteFunc(c.Capabilities, func(cap ServiceCapability) bool {
		return cap == retiredServiceCapabilityProjects
	})
	if len(c.Capabilities) != before {
		slog.Info("dropped retired service capability", "service", c.ID, "capability", retiredServiceCapabilityProjects)
	}
}

// validateCapabilities refuses a capability name relay does not know, and
// refuses ServiceCapabilitySessions on any record but the built-in
// RelaySessionsServiceID one. A record that fails it fails Validate, so
// relay will not start it.
func (c *ServiceConfig) validateCapabilities() error {
	for _, capability := range c.Capabilities {
		if !slices.Contains(ServiceCapabilities, capability) {
			return fmt.Errorf("service capability %q is unknown: use frontend, manifest, models, model_host or sessions", capability)
		}
		if capability == ServiceCapabilitySessions && c.ID != RelaySessionsServiceID {
			return fmt.Errorf("service capability %q may only be held by the built-in %q service", capability, RelaySessionsServiceID)
		}
	}
	return nil
}

// validateAllowedModels refuses an entry that is empty or whitespace-only —
// trimming here (rather than assuming a caller already did) is what makes
// this the one place a stray " " id from any door (CLI, HTTP, the Settings
// window) is caught, whether or not that door normalized its input first.
func (c *ServiceConfig) validateAllowedModels() error {
	for _, m := range c.AllowedModels {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("service allowed_models entry must not be empty or whitespace-only")
		}
	}
	return nil
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

// HostProbe is the last probe result for a Host: absent until the first
// probe runs, then replaced wholesale by every later one. OK false means
// Error names why; OK true means every other field was discovered live.
type HostProbe struct {
	At            string `json:"at"`
	OK            bool   `json:"ok"`
	OS            string `json:"os,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Home          string `json:"home,omitempty"`
	Shell         string `json:"shell,omitempty"`
	NodePath      string `json:"node_path,omitempty"`
	NodeVersion   string `json:"node_version,omitempty"`
	ClaudePath    string `json:"claude_path,omitempty"`
	ClaudeVersion string `json:"claude_version,omitempty"`
	Error         string `json:"error,omitempty"`
}

// Host is a machine reached over ssh that a project's directory can live on
// (docs/ssh-hosts.md). Relay execs /usr/bin/ssh with this record turned into
// an argv prefix (internal/sshhost.SSHArgv) — it never opens a raw TCP
// connection or handles a private key itself.
type Host struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Target is user@host, host, or an ssh_config alias — whatever
	// /usr/bin/ssh's destination argument accepts.
	Target string `json:"target"`
	// Port is 0 to mean "ssh default / ssh_config", never literally 22 by
	// default: a 0 asks ssh to decide, an explicit 22 would override an
	// ssh_config Port an operator already set for this alias.
	Port int `json:"port,omitempty"`
	// IdentityFile is passed as -i when set; empty defers to the operator's
	// own ssh-agent/config, which is the ordinary case.
	IdentityFile string `json:"identity_file,omitempty"`
	CreatedAt    string `json:"created_at"`
	// Probe is the last probe result; nil until the first one runs.
	Probe *HostProbe `json:"probe,omitempty"`
}

type ProjectKind string

const (
	ProjectKindLocal  ProjectKind = "local"
	ProjectKindRemote ProjectKind = "remote"
)

// MountGrant is one relay-granted host directory exposed to a remote client
// as a real filesystem mount, over the mount plane (relay-9p/1), never
// through fsMCP. It exists only on a kind: remote project.
type MountGrant struct {
	// ID is a surface name the client names in its MountAttach preamble,
	// unique within the project. Must satisfy enrolment.SafeID (it is not
	// joined into a filesystem path the way an enrolment's client id is, but
	// the charset restriction is reused so a mount id is always safe to log,
	// to put in an audit mcp_id field, and to put in a CLI flag value with
	// no quoting question).
	ID string `json:"id"`
	// Path is an absolute host directory. It is an operator value, not
	// caller input: checked once at validation (must exist, must not be a
	// symlink), and re-opened with os.OpenRoot at attach time by a later
	// piece — a path that changed shape between validation and attach fails
	// then, not here.
	Path string `json:"path"`
	// Access is "read" or "write"; absent means read. Mirrors
	// StoredToken.AccessMode's asymmetric-default pattern: an unset or
	// misspelled value must never be readable as write.
	Access string `json:"access,omitempty"`
}

// AccessMode reads Access the same way StoredToken.AccessMode reads a
// project's per-MCP access map: "write" only on the exact string "write",
// anything else (absent, a typo, "Write", "rw") reads as read. A typo must
// narrow, never widen.
func (m MountGrant) AccessMode() string {
	if m.Access == AccessWrite {
		return AccessWrite
	}
	return AccessRead
}

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
	Kind ProjectKind `json:"kind,omitempty"`
	// HostID names a Host this project's Path lives on instead of the
	// console (docs/ssh-hosts.md). Empty means the console, same
	// zero-value-is-safe discipline as Kind: every project written before
	// this field existed round-trips as a console project. Mutually
	// exclusive with Kind == ProjectKindRemote — the two solve different
	// problems (a host project still IS kind: local in shape) and must never
	// be read together.
	HostID         string          `json:"host_id,omitempty"`
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

	// Mounts is the mount-plane grant: each entry exposes one host directory to
	// a remote client as a real POSIX filesystem mount, kernel-contained,
	// instead of through fsMCP's curated tool surface. Refused on a
	// kind: local project (project.ValidateMounts) — a local project already
	// reaches its directory through shells and fsMCP.
	Mounts []MountGrant `json:"mounts,omitempty"`
}

// EnrolmentBudget bounds what one enrolled client may draw per rolling
// window. The enrolment, not the project, carries the cap (ADR-010 decision
// 7) since it is the unit of compromise. There is deliberately no
// representation of "unlimited": a zero field means "unset", which
// enrolment.NormalizeBudget fills with the conservative default.
type EnrolmentBudget struct {
	WindowSeconds  int   `json:"window_seconds"`
	MaxCalls       int   `json:"max_calls"`
	MaxResultBytes int64 `json:"max_result_bytes"`
	// MountMaxOps, MountMaxReadBytes and MountMaxWriteBytes bound the mount
	// plane on the same rolling window as the tool-plane fields above,
	// counted separately: a mount cannot starve tool calls and the reverse.
	// Zero never means unlimited — see enrolment.NormalizeBudget.
	MountMaxOps        int   `json:"mount_max_ops,omitempty"`
	MountMaxReadBytes  int64 `json:"mount_max_read_bytes,omitempty"`
	MountMaxWriteBytes int64 `json:"mount_max_write_bytes,omitempty"`
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
	// (enrolment.SafeID).
	ClientID string `json:"client_id"`
	// Fingerprint is the FULL SHA-256 of the client certificate's DER,
	// "sha256:" + 64 hex chars. Never truncated — see FingerprintDER.
	Fingerprint string `json:"fingerprint"`
	// ProjectIDs are the grants this certificate may select among by
	// sending a project id on the wire. Every id here must name a project
	// with IsRemote() true (enrolment.ValidateGrants).
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
	ID      string                    `json:"id"`
	Name    string                    `json:"name,omitempty"`
	Hash    string                    `json:"hash"`
	Classes []control.CapabilityClass `json:"classes"`
	Created string                    `json:"created,omitempty"`
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
// An unknown string in Classes simply never equals a real control.CapabilityClass
// constant, so it grants nothing without needing to be rejected up front.
func (c APICredential) Grants(class control.CapabilityClass) bool {
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

// EveEnrolmentWindow is the anchor an operator opens so one new browser may
// register an eve passkey (docs/eve-passkey-enrolment.md), the same shape of
// lifetime as LoginBootstrap: an RFC 3339 expiry, at most one at a time, and
// opening a new one replaces whatever was there. Unlike LoginBootstrap it
// carries no secret to hash — the window is single-use because eve consumes
// it atomically through relay, not because anything here is unguessable.
type EveEnrolmentWindow struct {
	Expires string `json:"expires"`
}

// EvePasskey is relay's mirror of one credential eve reports about itself
// (docs/eve-passkey-enrolment.md decision 8). Display metadata only — never
// a public key or a counter, which is eve's alone to keep — refreshed
// wholesale on every report rather than merged field by field, so the
// mirror can never drift into a shape eve never actually sent.
type EvePasskey struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Created  string `json:"created"`
	LastUsed string `json:"last_used"`
	// Reported is when relay last heard about this credential, so a mirror
	// eve has not refreshed in a while (eve down for a week) is visibly
	// stale in the Passkeys tab rather than silently trusted.
	Reported string `json:"reported"`
}

// EvePasskeyRevocation is a revoke relay has recorded but eve has not yet
// applied (decision 9). It is dropped the moment a report from eve no
// longer lists the id (decision 12) — the report IS the acknowledgement,
// so there is no separate ack to forget or replay.
type EvePasskeyRevocation struct {
	ID        string `json:"id"`
	Requested string `json:"requested"`
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

// IsHosted reports whether this project's directory lives on a Host rather
// than the console. Test this, never `HostID != ""` inline, for the same
// reason IsRemote exists as a method: one place decides what the field means.
func (p *Project) IsHosted() bool {
	return p.HostID != ""
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

// NormalizeProjectKind preserves the empty representation of a local
// project in settings.json.
func NormalizeProjectKind(kind ProjectKind) ProjectKind {
	return normalizeProjectKind(kind)
}

func (c *ServiceConfig) Validate() error {
	if c.ID == "" {
		return fmt.Errorf("service ID is required")
	}
	if !IsSafeID(c.ID) {
		return fmt.Errorf("service ID %q is invalid: use only letters, digits, '.', '_', '-' (no path separators)", c.ID)
	}
	if c.DisplayName == "" {
		return fmt.Errorf("service display name is required")
	}
	if c.Command == "" {
		return fmt.Errorf("service command is required")
	}
	// relay injects its own RELAY_* variables into every spawned service
	// (bridge socket, launch fd, frontend socket) after the operator's
	// env is applied; an operator-supplied key in that namespace would only
	// ever collide with one of them, never mean anything on its own.
	for k := range c.Env {
		if strings.HasPrefix(k, "RELAY_") {
			return fmt.Errorf("service env key %q is reserved: the RELAY_ prefix is relay's own", k)
		}
	}
	if err := c.validateCapabilities(); err != nil {
		return err
	}
	return c.validateAllowedModels()
}

// IsSafeID guards persisted IDs that are used as filenames by application
// adapters.
func IsSafeID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func toolAllowedByPatterns(patterns []string, toolName string) bool {
	for _, pattern := range patterns {
		if pattern == "" || overBroadToolPattern(pattern) {
			continue
		}
		if ok, err := path.Match(pattern, toolName); err == nil && ok {
			return true
		}
	}
	return false
}

func overBroadToolPattern(pattern string) bool {
	if toolPatternLiteral(pattern) == "" {
		return true
	}
	for _, probe := range []string{
		"zqx_abcdefghijklmnopqrstuvwxyz_0123456789",
		"ZQX-ABCDEFGHIJKLMNOPQRSTUVWXYZ.0123456789",
		"z",
	} {
		if ok, err := path.Match(pattern, probe); err == nil && ok {
			return true
		}
	}
	return false
}

func toolPatternLiteral(pattern string) string {
	var lit strings.Builder
	for i := 0; i < len(pattern); {
		switch c := pattern[i]; c {
		case '*', '?':
			i++
		case '\\':
			i++
			if i < len(pattern) {
				lit.WriteByte(pattern[i])
				i++
			}
		case '[':
			i++
			if i < len(pattern) && pattern[i] == '^' {
				i++
			}
			for first := true; i < len(pattern); first = false {
				if pattern[i] == ']' && !first {
					i++
					break
				}
				if pattern[i] == '\\' {
					i++
				}
				i++
			}
		default:
			lit.WriteByte(c)
			i++
		}
	}
	return lit.String()
}

// AuditConfig is the persisted configuration of the audit recorder.
type AuditConfig struct {
	Enabled               *bool    `json:"enabled,omitempty"`
	LogArgs               *bool    `json:"log_args,omitempty"`
	LogLists              *bool    `json:"log_lists,omitempty"`
	MaxArgBytes           int      `json:"max_arg_bytes,omitempty"`
	MaxResultPreviewBytes int      `json:"max_result_preview_bytes,omitempty"`
	RingSize              int      `json:"ring_size,omitempty"`
	MaxFileBytes          int64    `json:"max_file_bytes,omitempty"`
	Generations           int      `json:"generations,omitempty"`
	RedactKeys            []string `json:"redact_keys,omitempty"`
}

// RemoteConfig is the persisted configuration of remote listeners.
type RemoteConfig struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Listen  string `json:"listen,omitempty"`

	EnrolmentRequests *bool  `json:"enrolment_requests,omitempty"`
	EnrolmentListen   string `json:"enrolment_listen,omitempty"`
}

// ModelEndpointConfig configures the model endpoint's loopback TCP listener
// (docs/model-endpoint.md, plan-broker-and-sessions.md decision b). Absent
// means no TCP listener at all — model.sock is always served regardless —
// the same "absent means closed" default RemoteConfig uses, and for the
// same reason: opening a network door is something the operator's settings
// say, never something a fresh install infers. A test-only Go seam
// (cmd/relay's SetModelListenOverrideForTest) can override this value;
// deliberately not an environment variable, which a production process's
// own environment could also reach.
type ModelEndpointConfig struct {
	Listen string `json:"listen,omitempty"`
}
