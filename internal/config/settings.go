package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
)

var (
	ErrNoToken      = errors.New("no token provided")
	ErrInvalidToken = errors.New("invalid token")
)

type Settings struct {
	Version      int             `json:"version"`
	ExternalMcps []ExternalMcp   `json:"external_mcps"`
	Services     []ServiceConfig `json:"services"`
	Projects     []Project       `json:"projects"`
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

	// APICredentials replace the single frontend bearer with credentials
	// that each name their own capability classes (ADR-015 decision 3).
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
	if cfg.FrontendConsumer == nil {
		cfg.FrontendConsumer = existing.FrontendConsumer
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

// UpdateProjectShellTemplates: no SyncProjectToken call, for the same reason
// as UpdateProjectChatTemplates.
func (s *Settings) UpdateProjectShellTemplates(id string, templates []ShellTemplate) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.ShellTemplates = templates
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

func (s *Settings) SetProjectGenerateSkill(id string, gen bool) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.GenerateSkill = gen
}

func (s *Settings) SetProjectAllowCwdAuth(id string, allow bool) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.AllowCwdAuth = allow
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
// the admin/frontend token checks — both sides are SHA-256 hashes, but
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

// AuthenticateProjectByPath returns nil when dir is empty, matches nothing,
// or matches only projects that have NOT opted into AllowCwdAuth — every
// failure mode is "no access", never "all access". The scope granted is
// identical to the project's token: opting in changes how a caller is
// *identified*, never what the project is allowed to reach.
//
// Nested projects resolve to the most specific match (longest project path
// containing dir), so a project nested inside another wins for its own
// subtree.
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
