package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"
)

var (
	ErrNoToken                     = errors.New("no token provided")
	ErrInvalidToken                = errors.New("invalid token")
	ErrHostProbeGenerationOverflow = errors.New("host probe generation exhausted")
)

type Settings struct {
	Version      int             `json:"version"`
	ExternalMcps []ExternalMcp   `json:"external_mcps"`
	Services     []ServiceConfig `json:"services"`
	Projects     []Project       `json:"projects"`
	// Hosts is every machine reached over ssh a project may live on
	// (docs/ssh-hosts.md). Not omitempty, like Projects: normalize() ensures
	// this is always a non-nil slice, so an install with no hosts still
	// serializes "hosts": [].
	Hosts []Host `json:"hosts"`
	// AdminSecret is sealed (§4.1): it is a plaintext bearer the bridge
	// accepts for a handful of admin ops, not a value relay only checks.
	AdminSecret Secret `json:"admin_secret,omitempty"`

	// SealedKeyID names the login-keychain key every sealed field below was
	// last sealed with. It is clear, not a Secret: an operator (and
	// relay itself, before it has resolved a Sealer) must be able to read
	// it straight off disk to tell a key mismatch from a missing key
	// (§5.5, §5.6). Absent means either a pre-sealing settings.json (some
	// legacy plaintext Secret, or none at all) or a fresh install that has
	// not chosen a key yet.
	SealedKeyID string `json:"sealed_key_id,omitempty"`

	// Enrolments bind client certificates to the remote grants they may use
	// (ADR-010 decision 2). omitempty, like Audit: an install that never
	// enrols a remote client keeps a settings.json byte-identical to the one
	// it had before this field existed. Note what is NOT here — there is no
	// bearer token anywhere in this feature; the certificate is the identity.
	Enrolments []Enrolment `json:"enrolments,omitempty"`

	// Audit configures the tool-call log. Absent means defaults (enabled),
	// so an install that predates this feature starts logging without any
	// settings.json migration.
	Audit *AuditConfig `json:"audit,omitempty"`

	// Remote configures the mTLS listener remote clients reach relay
	// through (ADR-010 decision 9). Absent means NO LISTENER AT ALL — the
	// opposite default to Audit above, and deliberately so: a missing audit
	// block should keep an old install recording, while a missing remote
	// block must never open a network socket.
	Remote *RemoteConfig `json:"remote,omitempty"`

	// ModelEndpoint configures the model endpoint's loopback TCP listener
	// (docs/model-endpoint.md). Absent means no TCP listener — model.sock is
	// unaffected either way. Same "absent means closed" default as Remote.
	ModelEndpoint *ModelEndpointConfig `json:"model_endpoint,omitempty"`

	// APICredentials are the bearer credentials the control-plane API
	// accepts, each naming its own capability classes (ADR-015 decision 3).
	// omitempty, like Enrolments and Audit: an install that never mints one
	// keeps a settings.json byte-identical to the one it had before this
	// field existed.
	APICredentials []APICredential `json:"api_credentials,omitempty"`

	// LoginBootstrap is the single-use anchor `relay login enrol` mints for
	// passkey registration (ADR-016 decision 2). omitempty: absent means no
	// registration is anchored, which is both the pre-feature state and the
	// state the moment after a code is consumed or expires.
	LoginBootstrap *LoginBootstrap `json:"login_bootstrap,omitempty"`

	// Passkeys holds every registered WebAuthn credential (ADR-016 decision
	// 3). omitempty, for the same reason as APICredentials: an install that
	// never registers one keeps settings.json byte-identical to before this
	// field existed.
	Passkeys []Passkey `json:"passkeys,omitempty"`

	// EveEnrolment is the single-use anchor an operator opens (tray or
	// `relay eve enrol`) so a second browser may register an eve passkey
	// (docs/eve-passkey-enrolment.md). omitempty: absent means closed, both
	// the pre-feature state and the state the moment after the slot is
	// consumed or expires.
	EveEnrolment *EveEnrolmentWindow `json:"eve_enrolment,omitempty"`

	// EvePasskeys mirrors eve's own credential list, reported by eve at
	// startup and after every change (docs/eve-passkey-enrolment.md decision
	// 8). omitempty: an eve that has never reported (or a relay that has
	// never run alongside one) keeps settings.json byte-identical to before
	// this field existed.
	EvePasskeys []EvePasskey `json:"eve_passkeys,omitempty"`

	// EvePasskeyRevocations is every eve passkey revocation relay has
	// recorded but eve has not yet acknowledged by omitting the id from a
	// later report (decisions 9 and 12). omitempty, for the same reason as
	// EvePasskeys.
	EvePasskeyRevocations []EvePasskeyRevocation `json:"eve_passkey_revocations,omitempty"`

	// TerminalTemplates is the complete set of terminal launch templates
	// (templates.go): there is no set computed in code, so what is listed here
	// is what a project can launch, and every entry, including the ones relay
	// seeds on first start, can be edited or removed. Each carries its own
	// sandbox folders. When it is empty relay writes one default template on
	// its next start (EnsureDefaultTerminalTemplates). omitempty, like
	// Enrolments and Passkeys.
	TerminalTemplates []TerminalTemplate `json:"terminal_templates,omitempty"`
}

func (s *Settings) AddExternalMcp(mcp ExternalMcp) {
	s.ExternalMcps = append(s.ExternalMcps, mcp)
}

func (s *Settings) UpdateExternalMcp(cfg ExternalMcp) {
	_, idx := s.findMcpByID(cfg.ID)
	if idx < 0 {
		return
	}
	s.ExternalMcps[idx] = cfg
}

func (s *Settings) RemoveExternalMcp(id string) {
	s.ExternalMcps = slices.DeleteFunc(s.ExternalMcps, func(m ExternalMcp) bool { return m.ID == id })
}

func (s *Settings) UpsertExternalMcp(cfg ExternalMcp) bool {
	if _, idx := s.findMcpByID(cfg.ID); idx >= 0 {
		s.UpdateExternalMcp(cfg)
		return true
	}
	s.AddExternalMcp(cfg)
	return false
}

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

func (s *Settings) UpdateOAuthState(mcpID string, oauth *OAuthState) {
	if mcp, _ := s.findMcpByID(mcpID); mcp != nil {
		mcp.OAuthState = oauth
	}
}

func (s *Settings) AllExternalMcpIDs() []string {
	ids := make([]string, 0, len(s.ExternalMcps))
	for _, mcp := range s.ExternalMcps {
		ids = append(ids, mcp.ID)
	}
	return ids
}

func (s *Settings) AddService(config ServiceConfig) {
	s.Services = append(s.Services, config)
}

func (s *Settings) RemoveService(id string) {
	s.Services = slices.DeleteFunc(s.Services, func(svc ServiceConfig) bool { return svc.ID == id })
}

func (s *Settings) UpdateService(config ServiceConfig) {
	if _, idx := s.findServiceByID(config.ID); idx >= 0 {
		s.Services[idx] = config
	}
}

func (s *Settings) UpsertService(cfg ServiceConfig) bool {
	if _, idx := s.findServiceByID(cfg.ID); idx >= 0 {
		s.Services[idx] = cfg
		return true
	}
	s.AddService(cfg)
	return false
}

func (s *Settings) SetServiceAutostart(id string, autostart bool) {
	if svc, _ := s.findServiceByID(id); svc != nil {
		svc.Autostart = autostart
	}
}

// MergeServiceDefaults fills zero-value fields in cfg from the existing
// service with the same ID, for CLI flags that only specify fields being
// changed. Autostart is intentionally not merged: its zero value (false) is
// indistinguishable from "user explicitly set false", so the CLI flag
// always wins.
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
	if cfg.Capabilities == nil {
		cfg.Capabilities = existing.Capabilities
	}
}

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

func (s *Settings) AddProject(p Project) {
	s.Projects = append(s.Projects, p)
}

func (s *Settings) RemoveProject(id string) {
	s.Projects = slices.DeleteFunc(s.Projects, func(p Project) bool { return p.ID == id })
}

func (s *Settings) UpdateProjectModels(id string, models []string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.AllowedModels = models
}

func (s *Settings) UpdateProjectName(id string, name string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.Name = name
}

// UpdateProjectChatTemplates: no SyncProjectToken call, because templates
// have no token/context impact.
func (s *Settings) UpdateProjectChatTemplates(id string, templates []ChatTemplate) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.ChatTemplates = templates
}

// UpdateProjectAllowedTemplates: no SyncProjectToken call, for the same reason
// as UpdateProjectChatTemplates. A nil list is stored as empty: none.
func (s *Settings) UpdateProjectAllowedTemplates(id string, templates []string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	if templates == nil {
		templates = []string{}
	}
	proj.AllowedTemplates = templates
}

// UpdateProjectSessionFolders trims and de-duplicates names
// case-sensitively, preserving first-seen order. A nil/empty list clears
// the field so the serialized form stays minimal.
func (s *Settings) UpdateProjectSessionFolders(id string, folders []string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	if len(folders) == 0 {
		proj.SessionFolders = nil
		return
	}
	cleaned := make([]string, 0, len(folders))
	seen := make(map[string]bool, len(folders))
	for _, f := range folders {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		cleaned = append(cleaned, f)
	}
	if len(cleaned) == 0 {
		proj.SessionFolders = nil
		return
	}
	proj.SessionFolders = cleaned
}

// UpdateProjectPermissionPolicy: pass nil to clear.
func (s *Settings) UpdateProjectPermissionPolicy(id string, policy *PermissionPolicy) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.PermissionPolicy = policy
}

// RotateProjectToken invalidates the old token at the very next
// AuthenticateProject call: any Eve/relayLLM/CLI session still holding the
// old plaintext gets an auth failure on its next request and must re-auth.
func (s *Settings) RotateProjectToken(id string) (plaintext string, found bool, err error) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return "", false, nil
	}
	plaintext, hash, err := generateProjectToken()
	if err != nil {
		return "", true, err
	}
	proj.Token = NewSecret(plaintext)
	proj.TokenHash = hash
	return plaintext, true, nil
}

// UpdateProjectDisabledTools refuses an MCP not currently in the project's
// AllowedMcpIDs (unless wildcard): disabling tools for an unallowed MCP
// would be a no-op at runtime but a future allow-MCP change would silently
// inherit a stale list.
func (s *Settings) UpdateProjectDisabledTools(id, mcpID string, disabled []string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	if !IsWildcard(proj.AllowedMcpIDs) && !slices.Contains(proj.AllowedMcpIDs, mcpID) {
		return
	}
	if proj.DisabledTools == nil {
		proj.DisabledTools = make(map[string][]string)
	}
	if len(disabled) == 0 {
		delete(proj.DisabledTools, mcpID)
		return
	}
	cleaned := make([]string, 0, len(disabled))
	seen := make(map[string]bool, len(disabled))
	for _, t := range disabled {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		cleaned = append(cleaned, t)
	}
	proj.DisabledTools[mcpID] = cleaned
}

// UpdateProjectAllowedTools (ADR-011 decision 2b) drops entries naming an
// MCP the project is not granted rather than storing them: a stale
// allowlist reads as a grant and is not one, and SyncProjectToken already
// prunes them on every resync.
func (s *Settings) UpdateProjectAllowedTools(id string, allowed map[string][]string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	if len(allowed) == 0 {
		proj.AllowedTools = nil
		return
	}
	cleaned := make(map[string][]string, len(allowed))
	for mcpID, patterns := range allowed {
		if !IsWildcard(proj.AllowedMcpIDs) && !slices.Contains(proj.AllowedMcpIDs, mcpID) {
			continue
		}
		list := make([]string, 0, len(patterns))
		seen := make(map[string]bool, len(patterns))
		for _, p := range patterns {
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			list = append(list, p)
		}
		if len(list) > 0 {
			cleaned[mcpID] = list
		}
	}
	if len(cleaned) == 0 {
		proj.AllowedTools = nil
		return
	}
	proj.AllowedTools = cleaned
}

// UpdateProjectAccess (ADR-011 decision 2) drops entries naming an MCP the
// project is not granted, exactly as UpdateProjectAllowedTools does.
//
// An unrecognised mode is stored as given rather than dropped or corrected.
// Dropping it would fall back to the DEFAULT, which for a local project is
// write — a mutator silently widening a grant on the strength of a typo.
// StoredToken.AccessMode reads an unrecognised value as read-only, so
// keeping it is the fail-closed direction; the loud refusal is
// validateProjectPermissions, at the surface an operator actually types into.
func (s *Settings) UpdateProjectAccess(id string, access map[string]string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	if len(access) == 0 {
		proj.Access = nil
		return
	}
	cleaned := make(map[string]string, len(access))
	for mcpID, mode := range access {
		if !IsWildcard(proj.AllowedMcpIDs) && !slices.Contains(proj.AllowedMcpIDs, mcpID) {
			continue
		}
		cleaned[mcpID] = mode
	}
	if len(cleaned) == 0 {
		proj.Access = nil
		return
	}
	proj.Access = cleaned
}

// UpdateProjectAllowExternal (ADR-011 decision 2c) drops entries naming an
// MCP the project is not granted, exactly as UpdateProjectAccess.
//
// BOTH VALUES ARE STORED, including false. False is not "the same as
// absent" even though it looks like it for a profile: the default is
// asymmetric (StoredToken.ExternalAllowed), so for a LOCAL project absent
// means allowed and an explicit false is the only way to say the opposite —
// a mutator that discarded the false would make that unsayable. An empty
// map still clears the whole field, returning every MCP to its default.
func (s *Settings) UpdateProjectAllowExternal(id string, allow map[string]bool) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	if len(allow) == 0 {
		proj.AllowExternal = nil
		return
	}
	cleaned := make(map[string]bool, len(allow))
	for mcpID, allowed := range allow {
		if !IsWildcard(proj.AllowedMcpIDs) && !slices.Contains(proj.AllowedMcpIDs, mcpID) {
			continue
		}
		cleaned[mcpID] = allowed
	}
	if len(cleaned) == 0 {
		proj.AllowExternal = nil
		return
	}
	proj.AllowExternal = cleaned
}

// UpdateProjectMounts replaces id's mount-plane grant list wholesale — the
// same plain-replace shape as UpdateProjectAllowedTemplates, since a mount
// carries no per-MCP cross-check the way AllowExternal's does; ValidateMounts
// (called from ValidateShape, before this mutator ever runs) is what refuses
// a bad id, an overlapping path, or a kind:local project's non-empty list.
func (s *Settings) UpdateProjectMounts(id string, mounts []MountGrant) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.Mounts = mounts
}

func (s *Settings) SetProjectGenerateSkill(id string, gen bool) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.GenerateSkill = gen
}

// SetProjectHostID moves a project between the console and a host. "" moves
// it back to the console — validated by project.ValidateHostRef/ValidateShape
// at the call site, not here, matching every other field this package trusts
// its caller to have already checked (SetProjectGenerateSkill, UpdateProjectMounts).
func (s *Settings) SetProjectHostID(id string, hostID string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.HostID = hostID
}

func (s *Settings) findHostByID(id string) (*Host, int) {
	for i := range s.Hosts {
		if s.Hosts[i].ID == id {
			return &s.Hosts[i], i
		}
	}
	return nil, -1
}

func (s *Settings) findHostByName(name string) (*Host, int) {
	for i := range s.Hosts {
		if strings.EqualFold(s.Hosts[i].Name, name) {
			return &s.Hosts[i], i
		}
	}
	return nil, -1
}

// FindHostByID returns the mutable persisted host record and its index.
func FindHostByID(s *Settings, id string) (*Host, int) {
	return s.findHostByID(id)
}

// FindHostByName looks up a host case-insensitively, matching how
// ValidateHost enforces uniqueness.
func FindHostByName(s *Settings, name string) (*Host, int) {
	return s.findHostByName(name)
}

// AddHost validates and appends a new host, assigning its ID and CreatedAt
// if the caller left them empty. Call within store.With.
func (s *Settings) AddHost(h Host) (Host, error) {
	if h.ID == "" {
		h.ID = "h_" + newHostSuffix()
	}
	if h.CreatedAt == "" {
		h.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	h.ProbeGeneration = 1
	if err := ValidateHost(&h, s.Hosts, ""); err != nil {
		return Host{}, err
	}
	s.Hosts = append(s.Hosts, h)
	return h, nil
}

// HostPatch is the set-fields patch for UpdateHost; a nil pointer means "not
// in the request", matching UpdateFields' discipline for projects.
type HostPatch struct {
	Name         *string
	Target       *string
	Port         *int
	IdentityFile *string
	TmuxPath     *string
}

// UpdateHost patches name/target/port/identity_file, validated against the
// FINAL shape the patch would produce (candidate), never against the touched
// fields alone — matching ApplyUpdate's rule for projects. A target, port or
// identity_file change is the caller's cue to re-probe; UpdateHost itself
// never runs one.
func (s *Settings) UpdateHost(id string, patch HostPatch) (Host, bool, error) {
	h, idx := s.findHostByID(id)
	if h == nil {
		return Host{}, false, nil
	}
	candidate := *h
	if patch.Name != nil {
		candidate.Name = *patch.Name
	}
	if patch.Target != nil {
		candidate.Target = *patch.Target
	}
	if patch.Port != nil {
		candidate.Port = *patch.Port
	}
	if patch.IdentityFile != nil {
		candidate.IdentityFile = *patch.IdentityFile
	}
	if patch.TmuxPath != nil {
		candidate.TmuxPath = *patch.TmuxPath
	}
	if err := ValidateHost(&candidate, s.Hosts, id); err != nil {
		return Host{}, true, err
	}
	s.Hosts[idx] = candidate
	return candidate, true, nil
}

// UpdateHostAndReserveProbe applies a host patch and reserves a generation
// when its connection fields changed. A rename leaves the generation alone.
func (s *Settings) UpdateHostAndReserveProbe(id string, patch HostPatch) (Host, bool, bool, error) {
	h, idx := s.findHostByID(id)
	if h == nil {
		return Host{}, false, false, nil
	}
	candidate := *h
	if patch.Name != nil {
		candidate.Name = *patch.Name
	}
	if patch.Target != nil {
		candidate.Target = *patch.Target
	}
	if patch.Port != nil {
		candidate.Port = *patch.Port
	}
	if patch.IdentityFile != nil {
		candidate.IdentityFile = *patch.IdentityFile
	}
	if patch.TmuxPath != nil {
		candidate.TmuxPath = *patch.TmuxPath
	}
	if err := ValidateHost(&candidate, s.Hosts, id); err != nil {
		return Host{}, true, false, err
	}
	connectionChanged := candidate.Target != h.Target || candidate.Port != h.Port || candidate.IdentityFile != h.IdentityFile
	if connectionChanged {
		if candidate.ProbeGeneration == ^uint64(0) {
			return Host{}, true, false, ErrHostProbeGenerationOverflow
		}
		candidate.ProbeGeneration++
	}
	s.Hosts[idx] = candidate
	return candidate, true, connectionChanged, nil
}

// ReserveHostProbe gives an explicit probe a new generation before network
// work starts. A later result can persist only if this generation still owns
// the record.
func (s *Settings) ReserveHostProbe(id string) (Host, bool, error) {
	h, idx := s.findHostByID(id)
	if h == nil {
		return Host{}, false, nil
	}
	if h.ProbeGeneration == ^uint64(0) {
		return Host{}, true, ErrHostProbeGenerationOverflow
	}
	h.ProbeGeneration++
	s.Hosts[idx] = *h
	return *h, true, nil
}

// RemoveHost refuses to remove a host any project still references, naming
// every referencing project so the operator knows what to repoint first.
// found is false when no host has this id at all; refs is non-empty exactly
// when found is true but the removal was refused.
func (s *Settings) RemoveHost(id string) (found bool, refs []string) {
	h, _ := s.findHostByID(id)
	if h == nil {
		return false, nil
	}
	for _, p := range s.Projects {
		if p.HostID == id {
			refs = append(refs, p.Name)
		}
	}
	if len(refs) > 0 {
		return true, refs
	}
	s.Hosts = slices.DeleteFunc(s.Hosts, func(x Host) bool { return x.ID == id })
	return true, nil
}

// SetHostProbe replaces id's last probe result wholesale — a probe result is
// a point-in-time snapshot, never merged with the last one.
func (s *Settings) SetHostProbe(id string, probe HostProbe) bool {
	h, _ := s.findHostByID(id)
	if h == nil {
		return false
	}
	h.Probe = &probe
	return true
}

// SetHostProbeIfGeneration persists a probe only when the host still has the
// connection generation it was run against.
func (s *Settings) SetHostProbeIfGeneration(id string, generation uint64, probe HostProbe) (Host, bool) {
	h, _ := s.findHostByID(id)
	if h == nil || h.ProbeGeneration != generation {
		return Host{}, false
	}
	h.Probe = &probe
	return *h, true
}

// SetHostTemplates replaces hostID's terminal templates wholesale with a copy
// of ts. It does not validate: callers run ValidateHostTemplate first, and
// TemplatesForProject drops anything invalid at resolution anyway.
func (s *Settings) SetHostTemplates(hostID string, ts []TerminalTemplate) bool {
	h, _ := s.findHostByID(hostID)
	if h == nil {
		return false
	}
	h.TerminalTemplates = cloneTerminalTemplates(ts)
	return true
}

func (s *Settings) findMcpByID(id string) (*ExternalMcp, int) {
	for i := range s.ExternalMcps {
		if s.ExternalMcps[i].ID == id {
			return &s.ExternalMcps[i], i
		}
	}
	return nil, -1
}

func (s *Settings) findServiceByID(id string) (*ServiceConfig, int) {
	for i := range s.Services {
		if s.Services[i].ID == id {
			return &s.Services[i], i
		}
	}
	return nil, -1
}

func (s *Settings) findProjectByID(id string) (*Project, int) {
	for i := range s.Projects {
		if s.Projects[i].ID == id {
			return &s.Projects[i], i
		}
	}
	return nil, -1
}

// FindProjectByID returns the mutable persisted project record and its index.
// Domain code uses this instead of attaching methods to config.Settings.
func FindProjectByID(s *Settings, id string) (*Project, int) {
	return s.findProjectByID(id)
}

// FindServiceByID returns the mutable persisted service record and its index.
func FindServiceByID(s *Settings, id string) (*ServiceConfig, int) {
	return s.findServiceByID(id)
}

// FindExternalMcpByID returns the mutable persisted MCP record and its index.
func FindExternalMcpByID(s *Settings, id string) (*ExternalMcp, int) {
	return s.findMcpByID(id)
}

// findProjectByTokenHash uses a constant-time compare for consistency with
// the admin secret and API credential checks — both sides are SHA-256 hashes, but
// matching the hardened path keeps the auth-comparison policy uniform.
func (s *Settings) findProjectByTokenHash(hash string) *Project {
	want := []byte(hash)
	for i := range s.Projects {
		if subtle.ConstantTimeCompare([]byte(s.Projects[i].TokenHash), want) == 1 {
			return &s.Projects[i]
		}
	}
	return nil
}

func IsWildcard(ids []string) bool {
	return len(ids) == 1 && ids[0] == "*"
}

func (s *Settings) AuthenticateProject(plaintext string) (*StoredToken, error) {
	if plaintext == "" {
		return nil, ErrNoToken
	}
	stored := s.AuthenticateProjectByHash(HashToken(plaintext))
	if stored == nil {
		return nil, ErrInvalidToken
	}
	return stored, nil
}

// AuthenticateProjectByHash lets resolveAuth pass a pre-computed hash
// rather than double-hashing.
func (s *Settings) AuthenticateProjectByHash(hash string) *StoredToken {
	proj := s.findProjectByTokenHash(hash)
	if proj == nil {
		return nil
	}
	return s.storedTokenForProject(proj, hash)
}

// storedTokenForProject is shared by every authentication path so a
// project's scope cannot drift depending on how the caller was identified.
func (s *Settings) storedTokenForProject(proj *Project, hash string) *StoredToken {
	// Wildcard: nil permissions map — checkToolAccess treats a missing key
	// as allowed.
	if IsWildcard(proj.AllowedMcpIDs) {
		return &StoredToken{
			Name:          "project:" + proj.Name,
			ProjectID:     proj.ID,
			ProjectKind:   proj.Kind,
			Hash:          hash,
			DisabledTools: proj.DisabledTools,
			Context:       proj.Context,
			Access:        proj.Access,
			AllowedTools:  proj.AllowedTools,
			AllowExternal: proj.AllowExternal,
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
		ProjectID:     proj.ID,
		ProjectKind:   proj.Kind,
		Hash:          hash,
		Permissions:   perms,
		DisabledTools: proj.DisabledTools,
		Context:       proj.Context,
		Access:        proj.Access,
		AllowedTools:  proj.AllowedTools,
		AllowExternal: proj.AllowExternal,
	}
}

// newHostSuffix is a short random identifier, not a secret — a host id is
// logged and shown in the UI freely, so 4 bytes (enough to avoid an
// accidental collision, not to resist a guess) is the right size. Ignoring
// crypto/rand.Read's error leaves b as its zero value in the practically
// unreachable case it fails, which only risks a duplicate id -- ValidateHost
// does not key uniqueness on it, only on Name.
func newHostSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func generateProjectToken() (string, string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	plaintext := hex.EncodeToString(b[:])
	return plaintext, HashToken(plaintext), nil
}

// StoredTokenForProject constructs the authenticated view shared by token
// and directory authentication paths.
func StoredTokenForProject(s *Settings, proj *Project, hash string) *StoredToken {
	return s.storedTokenForProject(proj, hash)
}
