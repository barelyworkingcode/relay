package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Settings holds all persistent Relay configuration.
type Settings struct {
	Version      int             `json:"version"`
	ExternalMcps []ExternalMcp   `json:"external_mcps"`
	Services     []ServiceConfig `json:"services"`
	Projects     []Project       `json:"projects"`
	AdminSecret  string          `json:"admin_secret,omitempty"`

	// Enrolments bind client certificates to the remote grants they may use
	// (ADR-010 decision 2). omitempty, like Audit: an install that never
	// enrols a remote client keeps a settings.json byte-identical to the one
	// it had before this field existed, and no empty "enrolments": [] appears
	// on every unrelated project edit. Note what is NOT here — there is no
	// bearer token anywhere in this feature; the certificate is the identity.
	Enrolments []Enrolment `json:"enrolments,omitempty"`

	// Audit configures the tool-call log. Absent means defaults (enabled),
	// so an install that predates this feature starts logging without any
	// settings.json migration.
	Audit *AuditConfig `json:"audit,omitempty"`

	// Remote configures the mTLS listener remote clients reach relay through
	// (ADR-010 decision 9). Absent means NO LISTENER AT ALL — the opposite
	// default to Audit above, and deliberately so: a missing audit block should
	// keep an old install recording, while a missing remote block must never
	// open a network socket. omitempty keeps every install that has not enabled
	// one byte-identical to the one it had before this field existed.
	Remote *RemoteConfig `json:"remote,omitempty"`
}

// ---------------------------------------------------------------------------
// MCP CRUD — methods are small and cohesive with the Settings struct.
// ---------------------------------------------------------------------------

// AddExternalMcp adds an external MCP config. Does not save; use within store.With.
func (s *Settings) AddExternalMcp(mcp ExternalMcp) {
	s.ExternalMcps = append(s.ExternalMcps, mcp)
}

// UpdateExternalMcp replaces an external MCP config by ID.
// Does not save; use within store.With.
func (s *Settings) UpdateExternalMcp(cfg ExternalMcp) {
	_, idx := s.findMcpByID(cfg.ID)
	if idx < 0 {
		return
	}
	s.ExternalMcps[idx] = cfg
}

// RemoveExternalMcp removes an external MCP. Does not save; use within store.With.
func (s *Settings) RemoveExternalMcp(id string) {
	s.ExternalMcps = slices.DeleteFunc(s.ExternalMcps, func(m ExternalMcp) bool { return m.ID == id })
}

// UpsertExternalMcp adds or updates an external MCP config.
// Returns true if it updated an existing entry.
// Does not save; use within store.With.
func (s *Settings) UpsertExternalMcp(cfg ExternalMcp) bool {
	if _, idx := s.findMcpByID(cfg.ID); idx >= 0 {
		s.UpdateExternalMcp(cfg)
		return true
	}
	s.AddExternalMcp(cfg)
	return false
}

// ResolveMcpID returns the ID of an MCP found by exact id or display name lookup.
// Returns "" if not found.
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

// UpdateOAuthState updates the OAuth state for an HTTP MCP.
// Does not save; use within store.With.
func (s *Settings) UpdateOAuthState(mcpID string, oauth *OAuthState) {
	if mcp, _ := s.findMcpByID(mcpID); mcp != nil {
		mcp.OAuthState = oauth
	}
}

// AllExternalMcpIDs returns the IDs of all configured external MCPs.
func (s *Settings) AllExternalMcpIDs() []string {
	ids := make([]string, 0, len(s.ExternalMcps))
	for _, mcp := range s.ExternalMcps {
		ids = append(ids, mcp.ID)
	}
	return ids
}

// ---------------------------------------------------------------------------
// Service CRUD
// ---------------------------------------------------------------------------

// AddService adds a service config. Does not save; use within store.With.
func (s *Settings) AddService(config ServiceConfig) {
	s.Services = append(s.Services, config)
}

// RemoveService removes a service by ID. Does not save; use within store.With.
func (s *Settings) RemoveService(id string) {
	s.Services = slices.DeleteFunc(s.Services, func(svc ServiceConfig) bool { return svc.ID == id })
}

// UpdateService replaces a service config by ID. Does not save; use within store.With.
func (s *Settings) UpdateService(config ServiceConfig) {
	if _, idx := s.findServiceByID(config.ID); idx >= 0 {
		s.Services[idx] = config
	}
}

// UpsertService adds or updates a service config.
// Returns true if it updated an existing entry.
// Does not save; use within store.With.
func (s *Settings) UpsertService(cfg ServiceConfig) bool {
	if _, idx := s.findServiceByID(cfg.ID); idx >= 0 {
		s.Services[idx] = cfg
		return true
	}
	s.AddService(cfg)
	return false
}

// SetServiceAutostart updates the autostart flag for a service by ID.
// Does not save; use within store.With.
func (s *Settings) SetServiceAutostart(id string, autostart bool) {
	if svc, _ := s.findServiceByID(id); svc != nil {
		svc.Autostart = autostart
	}
}

// MergeServiceDefaults fills zero-value fields in cfg from the existing service
// with the same ID. Useful when CLI flags only specify fields being changed.
// Autostart is intentionally not merged: its zero value (false) is
// indistinguishable from "user explicitly set false", so the CLI flag always wins.
// Does not save; use within store.With.
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

// ResolveServiceID returns the ID of a service found by exact id or display name lookup.
// Returns "" if not found.
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

// ---------------------------------------------------------------------------
// Project CRUD
// ---------------------------------------------------------------------------

// AddProject adds a project and its associated token.
// Does not save; use within store.With.
func (s *Settings) AddProject(p Project) {
	s.Projects = append(s.Projects, p)
}

// RemoveProject removes a project by ID.
// Does not save; use within store.With.
func (s *Settings) RemoveProject(id string) {
	s.Projects = slices.DeleteFunc(s.Projects, func(p Project) bool { return p.ID == id })
}

// UpdateProjectMcps updates the allowed MCP IDs for a project and syncs
// the associated token's permissions and context.
// surfaces maps MCP IDs to their runtime schema + tool surface.
// Does not save; use within store.With.
func (s *Settings) UpdateProjectMcps(id string, mcpIDs []string, surfaces McpSurfaces) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.AllowedMcpIDs = mcpIDs
	s.SyncProjectToken(proj, surfaces)
}

// UpdateProjectModels updates the allowed models for a project.
// Does not save; use within store.With.
func (s *Settings) UpdateProjectModels(id string, models []string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.AllowedModels = models
}

// UpdateProjectName updates a project's name.
// Does not save; use within store.With.
func (s *Settings) UpdateProjectName(id string, name string) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.Name = name
}

// UpdateProjectPath updates a project's path and syncs token context.
// surfaces maps MCP IDs to their runtime schema + tool surface.
// Does not save; use within store.With.
func (s *Settings) UpdateProjectPath(id string, path string, surfaces McpSurfaces) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	// Belt-and-braces: the real refusal is validateProjectShape at the call
	// site (applyProjectUpdate validates the full candidate before any
	// mutation runs), but this mutator is exported on Settings and nothing
	// stops a future caller from invoking it directly without going through
	// that guard. A remote project has no filesystem scope, so silently
	// refuse rather than let one acquire a path no validation pass approved.
	// Clearing an already-remote project's path back to "" is a no-op and
	// stays allowed.
	if proj.IsRemote() && path != "" {
		return
	}
	proj.Path = path
	s.SyncProjectToken(proj, surfaces)
}

// UpdateProjectKind changes a project's kind. Does not save; use within
// store.With. Like the other single-field Update* mutators it leaves shape
// validation to the caller (applyProjectCreate / applyProjectUpdate, via
// validateProjectShape) — with one exception, below.
func (s *Settings) UpdateProjectKind(id string, kind ProjectKind) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	// See normalizeProjectKind: local always persists as "", never "local".
	kind = normalizeProjectKind(kind)
	// Belt-and-braces, exactly as UpdateProjectPath does it: the real refusal
	// is ValidateProjectEnrolments at the call site, but this mutator is
	// exported on Settings and nothing stops a future caller from invoking it
	// directly. A remote→local conversion under a live enrolment strands that
	// enrolment on a project whose whole shape it was never validated
	// against, and the failure mode is a silent widening of what a remote
	// client reaches — a host directory, its allowed_dirs context, cwd auth —
	// rather than a loud error. Refuse silently here so a bypass of the
	// validated path cannot produce it (ADR-010 decision 3).
	if !kind.IsRemote() && proj.IsRemote() && len(s.EnrolmentsGrantingProject(id)) > 0 {
		return
	}
	proj.Kind = kind
}

// UpdateProjectChatTemplates replaces a project's chat_templates list.
// Templates are scoped to the project and have no token/context impact, so
// no SyncProjectToken call is needed.
// Does not save; use within store.With.
func (s *Settings) UpdateProjectChatTemplates(id string, templates []ChatTemplate) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.ChatTemplates = templates
}

// UpdateProjectShellTemplates replaces a project's shell_templates list.
// Project-scoped terminal launch templates have no token/context impact, so no
// SyncProjectToken call is needed (same as chat templates).
// Does not save; use within store.With.
func (s *Settings) UpdateProjectShellTemplates(id string, templates []ShellTemplate) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.ShellTemplates = templates
}

// UpdateProjectSessionFolders replaces a project's ordered session-folder
// name list (Eve UI grouping metadata; relay never reads it). A nil/empty list
// clears it so the serialized form stays minimal. Trims and de-duplicates
// names case-sensitively, preserving first-seen order. Does not save; use
// within store.With.
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

// UpdateProjectPermissionPolicy replaces a project's permission policy.
// Pass nil to clear. Does not save; use within store.With.
func (s *Settings) UpdateProjectPermissionPolicy(id string, policy *PermissionPolicy) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.PermissionPolicy = policy
}

// RotateProjectToken generates fresh credentials for a project, replacing
// both Token and TokenHash. Returns the new plaintext, or "" and false if
// the project id is unknown.
//
// Rotation invalidates the old token at the very next AuthenticateProject
// call: any Eve/relayLLM/CLI session still holding the old plaintext will
// get an auth failure on its next request and must re-auth.
//
// Does not save; use within store.With.
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

// UpdateProjectDisabledTools replaces the per-MCP disabled-tools slice for a
// project. An empty (or nil) slice deletes the map key so the serialized form
// stays minimal.
//
// Refuses MCPs that are not currently in the project's AllowedMcpIDs (and the
// project is not wildcard) — disabling tools for an unallowed MCP would be a
// no-op at runtime but a future allow-MCP change would silently inherit a
// stale list. Returning early keeps the model honest.
//
// Does not save; use within store.With.
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

// UpdateProjectAllowedTools replaces a project's per-MCP tool allowlist
// (ADR-011 decision 2b). Entries naming an MCP the project is not granted are
// dropped rather than stored: a stale allowlist reads as a grant and is not
// one, and SyncProjectToken already prunes them on every resync.
// Does not save; use within store.With.
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

// UpdateProjectAccess replaces a project's per-MCP operation mode (ADR-011
// decision 2). Entries naming an MCP the project is not granted are dropped
// rather than stored, exactly as UpdateProjectAllowedTools drops them: a mode
// for an MCP this record cannot reach reads as an authority it does not have,
// and SyncProjectToken prunes them on every resync anyway.
//
// An unrecognised mode is stored as given rather than dropped or corrected.
// Dropping it would fall back to the DEFAULT, which for a local project is
// write — a mutator silently widening a grant on the strength of a typo. What
// StoredToken.AccessMode does with a value it does not recognise is read it as
// read-only, so keeping it is the fail-closed direction; the loud refusal is
// validateProjectPermissions, at the surfaces an operator actually types into.
//
// Does not save; use within store.With.
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

// UpdateProjectContext replaces a project's per-MCP context values — the
// resource scope an operator sets (ADR-011 decisions 4 and 6). Until this
// existed, Context was only ever DERIVED, by SyncProjectToken, and there was no
// operator path to it at all (ADR-011 finding 6).
//
// The map an operator supplies replaces what was there, and then
// SyncProjectToken runs so every source: "project_path" field is re-derived on
// top of it. That ordering is what makes a wholesale replace safe: the operator
// cannot set a derived field (validateProjectPermissions refuses it), so a
// replace would otherwise DELETE one — and a project whose write_dirs silently
// disappeared when its mail scope was edited would lose the two tools that
// field governs, fail-closed and unexplained. mergeContextField, which
// SyncProjectToken uses, puts each derived field back without touching the
// operator's.
//
// surfaces is what that re-derivation needs; passing nil means no derivation,
// which is the pre-ADR-011 contract for a caller with no live MCP manager.
//
// Does not save; use within store.With.
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

// SetProjectGenerateSkill toggles the GenerateSkill flag. Extracted from the
// HTTP route so the IPC path can reuse the same mutation without duplicating
// the lookup. Does not save; use within store.With.
func (s *Settings) SetProjectGenerateSkill(id string, gen bool) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.GenerateSkill = gen
}

// SetProjectAllowCwdAuth toggles the AllowCwdAuth flag. Does not save; use
// within store.With.
func (s *Settings) SetProjectAllowCwdAuth(id string, allow bool) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return
	}
	proj.AllowCwdAuth = allow
}

// SyncProjectToken updates the project's disabled tools and context to match
// its current allowedMcpIDs and path. Permissions are derived at auth time
// from AllowedMcpIDs, so they're not stored.
//
// surfaces maps MCP IDs to what relay knows about them at runtime (from
// ExternalMcpManager). A nil map skips derivation entirely, which is the
// pre-ADR-011 contract for a caller with no live MCP manager wired.
//
// The derivation is driven by the SCHEMA, not by a field name relay knows:
// relay writes the project's path into every field declaring
// source: "project_path" because the schema asked it to (ADR-011 decision 5).
// The v1 branch below is the one exception and is scheduled for removal.
func (s *Settings) SyncProjectToken(proj *Project, surfaces McpSurfaces) {
	if proj.Context == nil {
		proj.Context = make(map[string]json.RawMessage)
	}
	if proj.DisabledTools == nil {
		proj.DisabledTools = make(map[string][]string)
	}
	// Resolve which MCP IDs to configure: all registered if wildcard.
	mcpIDs := proj.AllowedMcpIDs
	if isWildcard(mcpIDs) {
		mcpIDs = s.AllExternalMcpIDs()
	}
	// Clean stale entries for MCPs no longer in the allowed set.
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
	// Defence in depth: a remote project has no Path, and BOTH ways to handle
	// that are unsafe — writing a path-derived field as [""] hands a downstream
	// MCP an empty root to interpret (a Node MCP's path.resolve("") resolves to
	// ITS OWN cwd, which can be far more permissive than intended), and omitting
	// the field lets the MCP fall back to its own default, which may be
	// unrestricted. Relay can't see how a given MCP interprets either, so
	// remote projects skip this derivation entirely — never just when
	// validation happens to catch it. ValidateProjectGrants is supposed to
	// refuse granting an MCP whose whole tool surface needs a path to a remote
	// project before this ever runs; this guard is what keeps a bypass of that
	// check from turning into a silent scope widening instead of a loud one.
	//
	// Stated generically now (ADR-011 decision 5): NEVER derive a
	// source: "project_path" field for a remote-kind record, unconditionally,
	// before the loop. The reasoning is ADR-009's and is unchanged — the
	// failure mode is a silent widening, and schemas are discovered at runtime,
	// so an MCP can grow such a field after a grant was already validated.
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
			// name here. Moving it into the schema as a default_disabled_tools
			// declaration is the same ADR-006 violation as the old
			// allowed_dirs branch, but it is not resource scoping, so it is
			// explicitly out of ADR-011's scope. Keyed off "this MCP scopes
			// something to the project path" rather than off the field's name,
			// which is the most domain-blind form available without the schema
			// change.
			if derived > 0 {
				s.disableToolByDefault(proj, mcpID, v1FsBashTool)
			}
			continue
		}

		// v1 compatibility, for one release. This is the last place in relay
		// that knows a field name; see v1AllowedDirsField. An MCP that declares
		// no contextSchemaVersion is handled exactly as it was before ADR-011,
		// nested-schema tolerance (issue #17) included.
		if schemaHasField(surface.Schema, v1AllowedDirsField) {
			ctx, _ := json.Marshal(map[string]interface{}{
				v1AllowedDirsField: []string{proj.Path},
			})
			proj.Context[mcpID] = ctx
			s.disableToolByDefault(proj, mcpID, v1FsBashTool)
		}
	}
}

// projectPathValue renders the project's path in the shape the field declared.
// An array-typed field gets a one-element list, a string-typed one gets the
// bare path. Anything else gets the list, which is what every path-scoped MCP
// relay has met declares and the only shape that can carry more than one root
// later.
func projectPathValue(f ContextField, path string) interface{} {
	if f.Type == "string" {
		return path
	}
	return []string{path}
}

// mergeContextField sets one field inside an MCP's context blob, leaving every
// other field alone. The old code replaced the whole blob, which was harmless
// while relay derived exactly one field and destructive as soon as an operator
// can set others beside it (ADR-011 decision 6).
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

// disableToolByDefault adds a tool to a project's disabled list once.
func (s *Settings) disableToolByDefault(proj *Project, mcpID, tool string) {
	if !slices.Contains(proj.DisabledTools[mcpID], tool) {
		proj.DisabledTools[mcpID] = append(proj.DisabledTools[mcpID], tool)
	}
}

// ValidateProjectGrants refuses a grant that would leave an MCP with no usable
// tools (ADR-011 decision 5).
//
// This replaces the old rule, which asked the wrong question: it refused a
// remote project any MCP declaring the literal field "allowed_dirs", encoding
// "filesystem" where it meant "derived from the project's path". The general
// question is asked here instead.
//
// A remote-kind record has no Path, so a source: "project_path" field cannot be
// supplied for one. By ADR-011 decision 4 every tool that field governs then
// refuses. If the field's applies_to covers EVERY tool the MCP exposes, the
// grant buys nothing and is refused, naming the MCP and why. If it covers only
// some — macMCP, where only mail_save_attachment and mail_get_source are
// governed — the grant is permitted and precisely those tools lose out. That
// is ADR-011 finding 1's fix, arriving as a consequence of the model rather
// than as a special case.
//
// Note what this is and is not. It is a coherence check an operator sees at
// edit time, not the security boundary: the boundary is SyncProjectToken
// refusing to derive the value (above) and CallTool refusing the call
// (appRouter.CallTool's presence re-check). So where the information needed to
// answer is missing — relay has never connected to the MCP, so it does not know
// what tools it exposes — this permits, and the call-time check still denies.
// Refusing on missing information would turn an MCP that is merely not running
// into an un-grantable one.
//
// Local projects are exempt: a path-scoped MCP granted to a local project is
// exactly the expected case, and SyncProjectToken fills in its real Path.
func (s *Settings) ValidateProjectGrants(proj *Project, surfaces McpSurfaces) error {
	if !proj.IsRemote() {
		return nil
	}
	for _, mcpID := range proj.AllowedMcpIDs {
		surface := surfaces[mcpID]
		schema := ParseContextSchema(surface.Schema, surface.SchemaVersion)

		if schema.V2() {
			for _, f := range schema.ProjectPathFields() {
				if !f.GovernsAll(surface.Tools) {
					continue
				}
				return fmt.Errorf("remote project cannot be granted %q: its %q scope is derived from the project's path, it governs every tool this MCP exposes, and a remote project has no path — the grant would leave no usable tools", mcpID, f.Name)
			}
			continue
		}

		// v1 compatibility, for one release: an MCP that declares
		// allowed_dirs and no version is refused exactly as before, without
		// consulting a tool list it has no way to qualify.
		if schemaHasField(surface.Schema, v1AllowedDirsField) {
			return fmt.Errorf("remote project cannot be granted %q: it is a filesystem-scoped MCP (declares %s) and a remote project has no path to scope it to", mcpID, v1AllowedDirsField)
		}
	}
	return nil
}

// schemaHasField reports whether a context schema declares a given field, in
// either shape an MCP might reasonably use.
//
// THIS IS THE V1 PATH ONLY. A v2 schema (contextSchemaVersion >= 2) is parsed
// by ParseContextSchema and never reaches here; this exists because fsMCP as
// shipped declares its context flat — {"allowed_dirs": {...}} — with no
// version, while the ordinary JSON-Schema shape nests the same declaration
// under "properties", and relay's own test fixtures use that form. Checking
// only the flat shape made the answer depend on how an MCP happened to spell an
// identical declaration.
//
// Getting that wrong fails OPEN, which is why both shapes are checked here
// rather than a shape being mandated elsewhere. A false negative does not
// merely skip a warning — the grant is allowed, the second defence then
// correctly declines to derive a path-scoped field for a remote project, the
// MCP receives no value at all, and an MCP that reads an absent allowlist as
// "unrestricted" (fsMCP does) hands a client on another machine the whole host
// filesystem. ADR-009 names that exact sequence as the thing this check exists
// to prevent.
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
	// JSON-Schema form: the declarations live under "properties". Checked after
	// the flat lookup so a schema that genuinely declares a field called
	// "properties" is still matched by the flat rule first.
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

// ---------------------------------------------------------------------------
// Lookup helpers — eliminate repeated linear scans
// ---------------------------------------------------------------------------

// findMcpByID returns the MCP with the given ID and its index, or nil, -1.
func (s *Settings) findMcpByID(id string) (*ExternalMcp, int) {
	for i := range s.ExternalMcps {
		if s.ExternalMcps[i].ID == id {
			return &s.ExternalMcps[i], i
		}
	}
	return nil, -1
}

// findServiceByID returns the service with the given ID and its index, or nil, -1.
func (s *Settings) findServiceByID(id string) (*ServiceConfig, int) {
	for i := range s.Services {
		if s.Services[i].ID == id {
			return &s.Services[i], i
		}
	}
	return nil, -1
}

// findProjectByID returns the project with the given ID and its index, or nil, -1.
func (s *Settings) findProjectByID(id string) (*Project, int) {
	for i := range s.Projects {
		if s.Projects[i].ID == id {
			return &s.Projects[i], i
		}
	}
	return nil, -1
}

// findProjectByTokenHash returns the project whose token matches the given hash.
// Uses a constant-time compare for consistency with the admin/frontend token
// checks — both sides are SHA-256 hashes, but matching the hardened path keeps
// the auth-comparison policy uniform.
func (s *Settings) findProjectByTokenHash(hash string) *Project {
	want := []byte(hash)
	for i := range s.Projects {
		if subtle.ConstantTimeCompare([]byte(s.Projects[i].TokenHash), want) == 1 {
			return &s.Projects[i]
		}
	}
	return nil
}

// isWildcard returns true if the list contains a single "*" entry,
// meaning "allow all".
func isWildcard(ids []string) bool {
	return len(ids) == 1 && ids[0] == "*"
}

// AuthenticateProject validates a bearer token against project token hashes.
// Returns a synthetic StoredToken with permissions derived from AllowedMcpIDs.
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

// AuthenticateProjectByHash finds a project by pre-computed token hash and
// returns a synthetic StoredToken with derived permissions. Returns nil if
// no project matches. Used by resolveAuth to avoid double-hashing.
func (s *Settings) AuthenticateProjectByHash(hash string) *StoredToken {
	proj := s.findProjectByTokenHash(hash)
	if proj == nil {
		return nil
	}
	return s.storedTokenForProject(proj, hash)
}

// AuthenticateProjectByPath resolves a caller's working directory to a project
// that has opted into directory auth (AllowCwdAuth) and returns the same
// synthetic StoredToken the token path would produce. Returns nil when dir is
// empty, matches nothing, or matches only projects that have NOT opted in —
// every failure mode is "no access", never "all access".
//
// The scope granted is identical to the project's token: opting in changes how
// a caller is *identified*, never what the project is allowed to reach.
//
// Nested projects resolve to the most specific match (longest project path
// containing dir), so a project nested inside another wins for its own subtree.
func (s *Settings) AuthenticateProjectByPath(dir string) *StoredToken {
	if dir == "" {
		return nil
	}
	var best *Project
	bestLen := -1
	for i := range s.Projects {
		p := &s.Projects[i]
		// Check the opt-in first: a project that hasn't enabled directory auth
		// must not even participate in the longest-match race, or it could
		// shadow an opted-in parent and turn a valid grant into a denial.
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

// storedTokenForProject builds the synthetic StoredToken view of a project:
// permissions derived from AllowedMcpIDs, plus the project's disabled tools and
// per-MCP context. Shared by every authentication path so a project's scope
// cannot drift depending on how the caller was identified.
func (s *Settings) storedTokenForProject(proj *Project, hash string) *StoredToken {
	// Wildcard: nil permissions map — checkToolAccess treats missing keys as allowed.
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
	}
}
