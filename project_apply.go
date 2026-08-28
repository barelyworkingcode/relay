package main

import "encoding/json"

// projectCreateFields is the transport-agnostic body for creating a project.
// Both the HTTP POST route and the IPC create handler unmarshal into it so the
// create orchestration lives in exactly one place (applyProjectCreate).
type projectCreateFields struct {
	Name             string              `json:"name"`
	Path             string              `json:"path"`
	Kind             ProjectKind         `json:"kind,omitempty"`
	AllowedMcpIDs    []string            `json:"allowed_mcp_ids"`
	AllowedModels    []string            `json:"allowed_models"`
	ChatTemplates    []ChatTemplate      `json:"chat_templates"`
	ShellTemplates   []ShellTemplate     `json:"shell_templates"`
	PermissionPolicy *PermissionPolicy   `json:"permission_policy,omitempty"`
	GenerateSkill    bool                `json:"generate_skill,omitempty"`
	AllowCwdAuth     bool                `json:"allow_cwd_auth,omitempty"`
	DisabledTools    map[string][]string `json:"disabled_tools,omitempty"`
	SessionFolders   []string            `json:"session_folders,omitempty"`
	// AllowedTools, Access and Context are the permission set an operator can
	// type (ADR-011 decisions 2, 2b and 6): a confinement an operator cannot
	// express is a confinement that does not exist.
	AllowedTools map[string][]string        `json:"allowed_tools,omitempty"`
	Access       map[string]string          `json:"access,omitempty"`
	Context      map[string]json.RawMessage `json:"context,omitempty"`
	// AllowExternal is the second axis of the same permission set (ADR-011
	// decision 2c). Not a pointer here for the same reason none of the three
	// above is — a create carries the whole shape or none of it.
	AllowExternal map[string]bool `json:"allow_external,omitempty"`
}

// projectUpdateFields is the transport-agnostic patch body. Nil pointers mean
// "not in the request" (no change); set pointers fully replace the prior value.
// Shared by the HTTP PUT route and the IPC update handler.
type projectUpdateFields struct {
	Name             *string              `json:"name,omitempty"`
	Path             *string              `json:"path,omitempty"`
	Kind             *ProjectKind         `json:"kind,omitempty"`
	AllowedMcpIDs    *[]string            `json:"allowed_mcp_ids,omitempty"`
	AllowedModels    *[]string            `json:"allowed_models,omitempty"`
	ChatTemplates    *[]ChatTemplate      `json:"chat_templates,omitempty"`
	ShellTemplates   *[]ShellTemplate     `json:"shell_templates,omitempty"`
	PermissionPolicy *PermissionPolicy    `json:"permission_policy,omitempty"`
	GenerateSkill    *bool                `json:"generate_skill,omitempty"`
	AllowCwdAuth     *bool                `json:"allow_cwd_auth,omitempty"`
	DisabledTools    *map[string][]string `json:"disabled_tools,omitempty"`
	SessionFolders   *[]string            `json:"session_folders,omitempty"`
	// Pointers for the same nil-means-no-change reason as everything above:
	// an operator editing a project's name must not clear its scope, and a
	// cleared scope must be an empty object, not indistinguishable from an
	// absent one.
	AllowedTools  *map[string][]string        `json:"allowed_tools,omitempty"`
	Access        *map[string]string          `json:"access,omitempty"`
	Context       *map[string]json.RawMessage `json:"context,omitempty"`
	AllowExternal *map[string]bool            `json:"allow_external,omitempty"`
}

// applyProjectCreate creates a project and applies its optional policy, skill
// flag, and disabled-tools map inside a single settings mutation. Call within
// store.With / withSettings. The caller validates the permission policy
// *before* invoking, so a bad policy never creates a project that has to be
// rolled back. Returns the fully-resolved project (re-read after the
// sub-mutations).
func applyProjectCreate(s *Settings, f projectCreateFields, surfaces McpSurfaces) (Project, error) {
	// GenerateSkill, AllowCwdAuth, ShellTemplates, PermissionPolicy,
	// ChatTemplates and the permission-set fields are not parameters of
	// CreateProjectWithTokenKind — they are applied by sub-mutations below,
	// after the project already exists. They still have to be on this
	// candidate so their remote refusals (and every check in
	// validateProjectPermissions) are reachable from a create, not just an
	// edit: validating the full requested shape here, before anything is
	// created, means a request that fails on e.g. remote+generate_skill
	// never leaves a half-built project to roll back.
	candidate := Project{
		Kind:             f.Kind,
		Path:             f.Path,
		AllowedMcpIDs:    f.AllowedMcpIDs,
		AllowedModels:    f.AllowedModels,
		ShellTemplates:   f.ShellTemplates,
		GenerateSkill:    f.GenerateSkill,
		AllowCwdAuth:     f.AllowCwdAuth,
		DisabledTools:    f.DisabledTools,
		AllowedTools:     f.AllowedTools,
		Access:           f.Access,
		Context:          f.Context,
		AllowExternal:    f.AllowExternal,
		PermissionPolicy: f.PermissionPolicy,
		ChatTemplates:    f.ChatTemplates,
	}
	if err := validateProjectShape(&candidate); err != nil {
		return Project{}, err
	}
	if err := validateProjectPermissions(&candidate, surfaces); err != nil {
		return Project{}, err
	}
	if err := s.ValidateProjectGrants(&candidate, surfaces); err != nil {
		return Project{}, err
	}

	created, err := s.CreateProjectWithTokenKind(
		f.Kind, f.Name, f.Path,
		f.AllowedMcpIDs, f.AllowedModels,
		f.ChatTemplates,
		surfaces,
	)
	if err != nil {
		return Project{}, err
	}
	if !permissionPolicyIsEmpty(f.PermissionPolicy) {
		s.UpdateProjectPermissionPolicy(created.ID, f.PermissionPolicy)
	}
	if f.GenerateSkill {
		s.SetProjectGenerateSkill(created.ID, true)
	}
	if f.AllowCwdAuth {
		s.SetProjectAllowCwdAuth(created.ID, true)
	}
	if len(f.SessionFolders) > 0 {
		s.UpdateProjectSessionFolders(created.ID, f.SessionFolders)
	}
	if len(f.AllowedTools) > 0 {
		s.UpdateProjectAllowedTools(created.ID, f.AllowedTools)
	}
	if len(f.Access) > 0 {
		s.UpdateProjectAccess(created.ID, f.Access)
	}
	if len(f.AllowExternal) > 0 {
		s.UpdateProjectAllowExternal(created.ID, f.AllowExternal)
	}
	// Last, because it re-runs SyncProjectToken, which prunes by the MCP set
	// the record ends up with.
	if len(f.Context) > 0 {
		s.UpdateProjectContext(created.ID, f.Context, surfaces)
	}
	if len(f.ShellTemplates) > 0 {
		s.UpdateProjectShellTemplates(created.ID, f.ShellTemplates)
	}
	for mcpID, disabled := range f.DisabledTools {
		s.UpdateProjectDisabledTools(created.ID, mcpID, disabled)
	}
	if proj, _ := s.findProjectByID(created.ID); proj != nil {
		created = *proj
	}
	return created, nil
}

// applyProjectUpdate patches the project with id from the set fields of f
// inside a single settings mutation. Call within store.With / withSettings.
// Returns (_, false, nil) if no project has that id, and (_, true, err) if
// the patch would produce an invalid shape or grant list — in that case
// NOTHING is mutated. surfaces is a lazy fetch invoked only when a path/MCP
// change or a remote-shaped result actually needs it, so the common rename
// stays allocation-free.
func applyProjectUpdate(s *Settings, id string, f projectUpdateFields, surfaces func() McpSurfaces) (Project, bool, error) {
	proj, _ := s.findProjectByID(id)
	if proj == nil {
		return Project{}, false, nil
	}

	// Validate the FINAL shape the patch would produce, not the touched
	// fields in isolation: a request that only flips AllowCwdAuth on an
	// already-remote project must still be refused, and one that only clears
	// Path on an already-remote project (a legal no-op) must still succeed.
	candidate := *proj
	if f.Kind != nil {
		candidate.Kind = *f.Kind
	}
	if f.Path != nil {
		candidate.Path = *f.Path
	}
	if f.AllowedMcpIDs != nil {
		candidate.AllowedMcpIDs = *f.AllowedMcpIDs
	}
	if f.AllowedModels != nil {
		candidate.AllowedModels = *f.AllowedModels
	}
	if f.ShellTemplates != nil {
		candidate.ShellTemplates = *f.ShellTemplates
	}
	if f.GenerateSkill != nil {
		candidate.GenerateSkill = *f.GenerateSkill
	}
	if f.AllowCwdAuth != nil {
		candidate.AllowCwdAuth = *f.AllowCwdAuth
	}
	if f.DisabledTools != nil {
		candidate.DisabledTools = *f.DisabledTools
	}
	if f.ChatTemplates != nil {
		candidate.ChatTemplates = *f.ChatTemplates
	}
	if f.PermissionPolicy != nil {
		// Normalised the same way the mutation below normalises it, so the
		// shape that is VALIDATED is the shape that would be STORED —
		// otherwise emptying a policy to convert a project to a profile
		// would be refused on the strength of a value being cleared.
		candidate.PermissionPolicy = f.PermissionPolicy
		if permissionPolicyIsEmpty(f.PermissionPolicy) {
			candidate.PermissionPolicy = nil
		}
	}
	if f.AllowedTools != nil {
		candidate.AllowedTools = *f.AllowedTools
	}
	if f.Access != nil {
		candidate.Access = *f.Access
	}
	if f.AllowExternal != nil {
		candidate.AllowExternal = *f.AllowExternal
	}
	if f.Context != nil {
		candidate.Context = *f.Context
	}
	if err := validateProjectShape(&candidate); err != nil {
		return Project{}, true, err
	}
	// A project that stops being remote strands every enrolment granting it,
	// so the conversion is refused while any exists (ADR-010 decision 3).
	// Checked only when the result is local AND the project either was
	// remote or had its kind named in the request — an ordinary edit to a
	// long-standing local project has no conversion to refuse, and running
	// the check there would turn a pre-existing bad grant into a wall in
	// front of the very edit that might fix it.
	if !candidate.IsRemote() && (proj.IsRemote() || f.Kind != nil) {
		if err := s.ValidateProjectEnrolments(&candidate); err != nil {
			return Project{}, true, err
		}
	}

	// surfaces() is a lazy fetch (real callers wire it to a live MCP-manager
	// call); fetch it once below and reuse it rather than fetching it twice.
	// Grants only need re-checking when Kind flips to remote or the MCP set
	// changes on an already/still-remote project — a bare rename of an
	// already-valid remote project doesn't need to pay for it.
	needGrantsCheck := candidate.IsRemote() && (f.AllowedMcpIDs != nil || f.Kind != nil)
	// The permission set is re-validated only when the request names part of
	// it — a rename cannot invalidate a scope value, and making every edit
	// pay for a live MCP surface fetch is what this laziness avoids.
	needPermissionsCheck := f.Access != nil || f.AllowedTools != nil || f.Context != nil || f.AllowExternal != nil
	var sc McpSurfaces
	if f.Path != nil || f.AllowedMcpIDs != nil || needGrantsCheck || needPermissionsCheck {
		sc = surfaces()
	}
	if needPermissionsCheck {
		if err := validateProjectPermissions(&candidate, sc); err != nil {
			return Project{}, true, err
		}
	}
	if needGrantsCheck {
		if err := s.ValidateProjectGrants(&candidate, sc); err != nil {
			return Project{}, true, err
		}
	}

	if f.Name != nil {
		s.UpdateProjectName(id, *f.Name)
	}
	if f.Kind != nil {
		s.UpdateProjectKind(id, *f.Kind)
	}
	if f.Path != nil {
		s.UpdateProjectPath(id, *f.Path, sc)
	}
	if f.AllowedMcpIDs != nil {
		s.UpdateProjectMcps(id, *f.AllowedMcpIDs, sc)
	}
	if f.AllowedModels != nil {
		s.UpdateProjectModels(id, *f.AllowedModels)
	}
	if f.ChatTemplates != nil {
		s.UpdateProjectChatTemplates(id, *f.ChatTemplates)
	}
	if f.ShellTemplates != nil {
		s.UpdateProjectShellTemplates(id, *f.ShellTemplates)
	}
	if f.PermissionPolicy != nil {
		// Same reading the candidate above was validated under: an empty
		// struct clears the policy.
		policy := f.PermissionPolicy
		if permissionPolicyIsEmpty(policy) {
			policy = nil
		}
		s.UpdateProjectPermissionPolicy(id, policy)
	}
	if f.GenerateSkill != nil {
		s.SetProjectGenerateSkill(id, *f.GenerateSkill)
	}
	if f.AllowCwdAuth != nil {
		s.SetProjectAllowCwdAuth(id, *f.AllowCwdAuth)
	}
	if f.SessionFolders != nil {
		s.UpdateProjectSessionFolders(id, *f.SessionFolders)
	}
	if f.AllowedTools != nil {
		s.UpdateProjectAllowedTools(id, *f.AllowedTools)
	}
	if f.Access != nil {
		s.UpdateProjectAccess(id, *f.Access)
	}
	if f.AllowExternal != nil {
		s.UpdateProjectAllowExternal(id, *f.AllowExternal)
	}
	if f.Context != nil {
		s.UpdateProjectContext(id, *f.Context, sc)
	}
	if f.DisabledTools != nil {
		// Full replace: any MCP key omitted from the request resets to "all
		// tools allowed", so both the existing and new keys are walked to
		// let removals propagate.
		existing := map[string]bool{}
		if proj, _ := s.findProjectByID(id); proj != nil {
			for k := range proj.DisabledTools {
				existing[k] = true
			}
		}
		for mcpID := range existing {
			if _, kept := (*f.DisabledTools)[mcpID]; !kept {
				s.UpdateProjectDisabledTools(id, mcpID, nil)
			}
		}
		for mcpID, disabled := range *f.DisabledTools {
			s.UpdateProjectDisabledTools(id, mcpID, disabled)
		}
	}

	if proj, _ := s.findProjectByID(id); proj != nil {
		return *proj, true, nil
	}
	return Project{}, false, nil
}
