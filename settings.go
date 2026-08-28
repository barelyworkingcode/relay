package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

type Settings struct {
	Version      int             `json:"version"`
	ExternalMcps []ExternalMcp   `json:"external_mcps"`
	Services     []ServiceConfig `json:"services"`
	Projects     []Project       `json:"projects"`
	AdminSecret  string          `json:"admin_secret,omitempty"`

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

func (s *Settings) UpdateProjectMcps(id string, mcpIDs []string, surfaces McpSurfaces) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.AllowedMcpIDs = mcpIDs
	s.SyncProjectToken(proj, surfaces)
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

func (s *Settings) UpdateProjectPath(id string, path string, surfaces McpSurfaces) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	// Belt-and-braces: the real refusal is validateProjectShape at the call
	// site, but this mutator is exported on Settings and nothing stops a
	// future caller from invoking it directly without that guard. A remote
	// project has no filesystem scope, so silently refuse rather than let
	// one acquire a path no validation pass approved. Clearing an
	// already-remote project's path back to "" is a no-op and stays allowed.
	if proj.IsRemote() && path != "" {
		return
	}
	proj.Path = path
	s.SyncProjectToken(proj, surfaces)
}

func (s *Settings) UpdateProjectKind(id string, kind ProjectKind) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	kind = normalizeProjectKind(kind)
	// Belt-and-braces, exactly as UpdateProjectPath does it: the real
	// refusal is ValidateProjectEnrolments at the call site, but this
	// mutator is exported and nothing stops a future caller from invoking
	// it directly. A remote→local conversion under a live enrolment strands
	// that enrolment on a project whose shape it was never validated
	// against — a silent widening of what a remote client reaches, rather
	// than a loud error. Refuse silently here so a bypass of the validated
	// path cannot produce it (ADR-010 decision 3).
	if !kind.IsRemote() && proj.IsRemote() && len(s.EnrolmentsGrantingProject(id)) > 0 {
		return
	}
	proj.Kind = kind
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
	proj.Token = plaintext
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
	if !isWildcard(proj.AllowedMcpIDs) && !slices.Contains(proj.AllowedMcpIDs, mcpID) {
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
		if !isWildcard(proj.AllowedMcpIDs) && !slices.Contains(proj.AllowedMcpIDs, mcpID) {
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
		if !isWildcard(proj.AllowedMcpIDs) && !slices.Contains(proj.AllowedMcpIDs, mcpID) {
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
		if !isWildcard(proj.AllowedMcpIDs) && !slices.Contains(proj.AllowedMcpIDs, mcpID) {
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

// UpdateProjectContext replaces a project's per-MCP context values — the
// resource scope an operator sets (ADR-011 decisions 4 and 6) — and then
// runs SyncProjectToken so every source: "project_path" field is
// re-derived on top of it (mergeContextField puts each back without
// touching the operator's). That ordering is what makes a wholesale
// replace safe: the operator cannot set a derived field directly
// (validateProjectPermissions refuses it), so a plain replace would
// otherwise silently delete one. surfaces is what that re-derivation
// needs; nil means no derivation.
func (s *Settings) UpdateProjectContext(id string, values map[string]json.RawMessage, surfaces McpSurfaces) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	cleaned := make(map[string]json.RawMessage, len(values))
	for mcpID, blob := range values {
		if !isWildcard(proj.AllowedMcpIDs) && !slices.Contains(proj.AllowedMcpIDs, mcpID) {
			continue
		}
		if len(contextValues(blob)) == 0 {
			continue
		}
		cleaned[mcpID] = blob
	}
	proj.Context = cleaned
	s.SyncProjectToken(proj, surfaces)
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

// SyncProjectToken derives the project's disabled tools and context from
// its current allowedMcpIDs and path, driven by the SCHEMA rather than by a
// field name relay knows: it writes the project's path into every field
// declaring source: "project_path" because the schema asked it to (ADR-011
// decision 5). surfaces maps MCP IDs to what relay knows at runtime; a nil
// map skips derivation for a caller with no live MCP manager wired.
//
// Deliberately NOT done here: pruning a stored field the live schema no
// longer declares. A merely-down MCP reports no schema at all, which is
// indistinguishable from "this schema now declares zero fields", so
// pruning on that signal would delete an operator's values because a
// process happened to be down at sync time — and it isn't needed for
// safety anyway, since a stale key is inert (filterKnownContextFields
// drops it at call time) and cannot be operator-set in the first place
// (validateProjectContextForMcp refuses unknown names at write time).
func (s *Settings) SyncProjectToken(proj *Project, surfaces McpSurfaces) {
	if proj.Context == nil {
		proj.Context = make(map[string]json.RawMessage)
	}
	if proj.DisabledTools == nil {
		proj.DisabledTools = make(map[string][]string)
	}
	mcpIDs := proj.AllowedMcpIDs
	if isWildcard(mcpIDs) {
		mcpIDs = s.AllExternalMcpIDs()
	}
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
	for id := range proj.Access {
		if !allowed[id] {
			delete(proj.Access, id)
		}
	}
	for id := range proj.AllowedTools {
		if !allowed[id] {
			delete(proj.AllowedTools, id)
		}
	}
	for id := range proj.AllowExternal {
		if !allowed[id] {
			delete(proj.AllowExternal, id)
		}
	}
	// Defence in depth: a remote project has no Path, and both ways to
	// handle that are unsafe (an empty-string root, or omitting the field
	// and letting the MCP fall back to its own possibly-unrestricted
	// default), so remote projects skip derivation entirely, unconditionally,
	// rather than only when ValidateProjectGrants happens to catch it —
	// this guard is what keeps a bypass of that check from silently
	// widening scope instead of failing loudly (ADR-011 decision 5).
	if proj.IsRemote() {
		return
	}
	for _, mcpID := range mcpIDs {
		surface := surfaces[mcpID]
		schema := ParseContextSchema(surface.Schema, surface.SchemaVersion)

		if schema.V2() {
			derived := 0
			for _, f := range schema.ProjectPathFields() {
				value, err := json.Marshal(projectPathValue(f, proj.Path))
				if err != nil {
					continue
				}
				proj.Context[mcpID] = mergeContextField(proj.Context[mcpID], f.Name, value)
				derived++
			}
			// DEFERRED (ADR-011): fs_bash auto-disable stays a hardcoded tool
			// name here, keyed off "this MCP scopes something to the project
			// path" rather than off a field name — the most domain-blind
			// form available without a schema change to carry it.
			if derived > 0 {
				s.disableToolByDefault(proj, mcpID, v1FsBashTool)
			}
			continue
		}

		// v1 compatibility: the last place in relay that knows a field name
		// directly; see v1AllowedDirsField.
		if schemaHasField(surface.Schema, v1AllowedDirsField) {
			ctx, _ := json.Marshal(map[string]interface{}{
				v1AllowedDirsField: []string{proj.Path},
			})
			proj.Context[mcpID] = ctx
			s.disableToolByDefault(proj, mcpID, v1FsBashTool)
		}
	}
}

// projectPathValue: a string-typed field gets the bare path; anything else
// (including array-typed) gets a one-element list, which is the only shape
// that can carry more than one root later.
func projectPathValue(f ContextField, path string) interface{} {
	if f.Type == "string" {
		return path
	}
	return []string{path}
}

// mergeContextField sets one field inside an MCP's context blob, leaving
// every other field alone — replacing the whole blob would destroy any
// field an operator can set beside a derived one (ADR-011 decision 6).
func mergeContextField(base json.RawMessage, name string, value json.RawMessage) json.RawMessage {
	m := contextValues(base)
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	m[name] = value
	out, err := json.Marshal(m)
	if err != nil {
		return base
	}
	return out
}

func (s *Settings) disableToolByDefault(proj *Project, mcpID, tool string) {
	if !slices.Contains(proj.DisabledTools[mcpID], tool) {
		proj.DisabledTools[mcpID] = append(proj.DisabledTools[mcpID], tool)
	}
}

// ValidateProjectGrants refuses a grant that would leave an MCP with no
// usable tools (ADR-011 decision 5): a remote-kind record has no Path, so
// a source: "project_path" field cannot be supplied, and by ADR-011
// decision 4 every tool that field governs then refuses. If the field's
// applies_to covers every tool the MCP exposes, the grant buys nothing and
// is refused; if it covers only some, the grant is permitted and precisely
// those tools lose out.
//
// This is a coherence check an operator sees at edit time, not the
// security boundary — that is SyncProjectToken (above) and CallTool's
// presence re-check — so where the MCP's tool list is unknown (relay has
// never connected to it), this permits, and the call-time check still
// denies. Local projects are exempt: a path-scoped MCP granted to a local
// project is the expected case.
func (s *Settings) ValidateProjectGrants(proj *Project, surfaces McpSurfaces) error {
	if !proj.IsRemote() {
		return nil
	}
	for _, mcpID := range proj.AllowedMcpIDs {
		surface := surfaces[mcpID]
		schema := ParseContextSchema(surface.Schema, surface.SchemaVersion)

		if schema.V2() {
			granted := grantedToolNames(proj, mcpID, surface.Tools)
			for _, f := range schema.ProjectPathFields() {
				if !f.GovernsAll(granted) {
					continue
				}
				return fmt.Errorf("remote project cannot be granted %q: its %q scope is derived from the project's path, it governs every tool this grant names, and a remote project has no path — the grant would leave no usable tools", mcpID, f.Name)
			}
			continue
		}

		// v1 compatibility: an MCP that declares allowed_dirs and no
		// version is refused outright, without consulting a tool list it
		// has no way to qualify.
		if schemaHasField(surface.Schema, v1AllowedDirsField) {
			return fmt.Errorf("remote project cannot be granted %q: it is a filesystem-scoped MCP (declares %s) and a remote project has no path to scope it to", mcpID, v1AllowedDirsField)
		}
	}
	return nil
}

// grantedToolNames narrows an MCP's live tool list to the ones this record
// may actually call (the same allowlist StoredToken.ToolAllowed applies at
// the chokepoint), because asking ValidateProjectGrants's question against
// the MCP's WHOLE surface is the wrong set: macMCP declares file_dirs
// governing mail_save_attachment alone, so the answer over 47 tools is
// always no, even for a grant of exactly that one tool that can call
// nothing.
//
// A grant naming NO tool of this MCP falls back to the whole surface — the
// fail-closed direction, not a shortcut, since an incomplete profile
// (decision 9b) is the ordinary order of work and must not read as
// vacuously safe.
func grantedToolNames(proj *Project, mcpID string, all []string) []string {
	tok := StoredToken{
		ProjectKind:  proj.Kind,
		AllowedTools: proj.AllowedTools,
	}
	out := make([]string, 0, len(all))
	for _, name := range all {
		if tok.ToolAllowed(mcpID, name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return all
	}
	return out
}

// schemaHasField is the V1 PATH ONLY — a v2 schema (contextSchemaVersion
// >= 2) never reaches here. It checks both the flat shape fsMCP ships and
// the JSON-Schema shape nested under "properties", because checking only
// one made the answer depend on how an MCP happened to spell the same
// declaration. Getting that wrong fails OPEN: a false negative silently
// permits a grant to an MCP that reads an absent allowlist as
// "unrestricted" (ADR-009).
func schemaHasField(schema json.RawMessage, field string) bool {
	if len(schema) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(schema, &fields); err != nil {
		return false
	}
	if _, ok := fields[field]; ok {
		return true
	}
	// Checked after the flat lookup so a schema that genuinely declares a
	// field called "properties" is still matched by the flat rule first.
	props, ok := fields["properties"]
	if !ok {
		return false
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(props, &nested); err != nil {
		return false
	}
	_, ok = nested[field]
	return ok
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

func isWildcard(ids []string) bool {
	return len(ids) == 1 && ids[0] == "*"
}

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
func (s *Settings) AuthenticateProjectByPath(dir string) *StoredToken {
	if dir == "" {
		return nil
	}
	var best *Project
	bestLen := -1
	for i := range s.Projects {
		p := &s.Projects[i]
		// Check the opt-in first: a project that hasn't enabled directory
		// auth must not even participate in the longest-match race, or it
		// could shadow an opted-in parent and turn a valid grant into a
		// denial.
		if !p.AllowCwdAuth || p.Path == "" {
			continue
		}
		if !dirWithinProject(dir, p.Path) {
			continue
		}
		if n := len(realpathBestEffort(p.Path)); n > bestLen {
			best, bestLen = p, n
		}
	}
	if best == nil {
		return nil
	}
	return s.storedTokenForProject(best, best.TokenHash)
}

// storedTokenForProject is shared by every authentication path so a
// project's scope cannot drift depending on how the caller was identified.
func (s *Settings) storedTokenForProject(proj *Project, hash string) *StoredToken {
	// Wildcard: nil permissions map — checkToolAccess treats a missing key
	// as allowed.
	if isWildcard(proj.AllowedMcpIDs) {
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
