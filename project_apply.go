package main

import "encoding/json"

// projectCreateFields is the transport-agnostic body for creating a project.
// Both the HTTP POST route and the IPC create handler unmarshal into it so the
// create orchestration lives in exactly one place (applyProjectCreate).
type projectCreateFields struct {
	Name             string              `json:"name"`
	Path             string              `json:"path"`
	AllowedMcpIDs    []string            `json:"allowed_mcp_ids"`
	AllowedModels    []string            `json:"allowed_models"`
	ChatTemplates    []ChatTemplate      `json:"chat_templates"`
	ShellTemplates   []ShellTemplate     `json:"shell_templates"`
	PermissionPolicy *PermissionPolicy   `json:"permission_policy,omitempty"`
	GenerateSkill    bool                `json:"generate_skill,omitempty"`
	DisabledTools    map[string][]string `json:"disabled_tools,omitempty"`
	SessionFolders   []string            `json:"session_folders,omitempty"`
}

// projectUpdateFields is the transport-agnostic patch body. Nil pointers mean
// "not in the request" (no change); set pointers fully replace the prior value.
// Shared by the HTTP PUT route and the IPC update handler.
type projectUpdateFields struct {
	Name             *string              `json:"name,omitempty"`
	Path             *string              `json:"path,omitempty"`
	AllowedMcpIDs    *[]string            `json:"allowed_mcp_ids,omitempty"`
	AllowedModels    *[]string            `json:"allowed_models,omitempty"`
	ChatTemplates    *[]ChatTemplate      `json:"chat_templates,omitempty"`
	ShellTemplates   *[]ShellTemplate     `json:"shell_templates,omitempty"`
	PermissionPolicy *PermissionPolicy    `json:"permission_policy,omitempty"`
	GenerateSkill    *bool                `json:"generate_skill,omitempty"`
	DisabledTools    *map[string][]string `json:"disabled_tools,omitempty"`
	SessionFolders   *[]string            `json:"session_folders,omitempty"`
}

// applyProjectCreate creates a project and applies its optional policy, skill
// flag, and disabled-tools map inside a single settings mutation. Call within
// store.With / withSettings. The caller is responsible for validating the
// permission policy *before* invoking (so a bad policy never creates a project
// that has to be rolled back) and for fetching schemas the same way it always
// has. Returns the fully-resolved project (re-read after the sub-mutations).
func applyProjectCreate(s *Settings, f projectCreateFields, schemas map[string]json.RawMessage) (Project, error) {
	created, err := s.CreateProjectWithToken(
		f.Name, f.Path,
		f.AllowedMcpIDs, f.AllowedModels,
		f.ChatTemplates,
		schemas,
	)
	if err != nil {
		return Project{}, err
	}
	if f.PermissionPolicy != nil {
		s.UpdateProjectPermissionPolicy(created.ID, f.PermissionPolicy)
	}
	if f.GenerateSkill {
		s.SetProjectGenerateSkill(created.ID, true)
	}
	if len(f.SessionFolders) > 0 {
		s.UpdateProjectSessionFolders(created.ID, f.SessionFolders)
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

// applyProjectUpdate patches the project with id from the set fields of f inside
// a single settings mutation. Call within store.With / withSettings. Returns
// (updated, false) if no project has that id. The caller validates the path and
// permission policy up front; schemas is a lazy fetch invoked only when a path
// or MCP change actually needs it (the common rename stays allocation-free).
func applyProjectUpdate(s *Settings, id string, f projectUpdateFields, schemas func() map[string]json.RawMessage) (Project, bool) {
	if proj, _ := s.findProjectByID(id); proj == nil {
		return Project{}, false
	}

	var sc map[string]json.RawMessage
	if f.Path != nil || f.AllowedMcpIDs != nil {
		sc = schemas()
	}
	if f.Name != nil {
		s.UpdateProjectName(id, *f.Name)
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
		policy := f.PermissionPolicy
		// An empty struct (no fields set) clears the policy.
		if policy.DefaultMode == "" && len(policy.AllowedTools) == 0 && len(policy.DeniedTools) == 0 {
			policy = nil
		}
		s.UpdateProjectPermissionPolicy(id, policy)
	}
	if f.GenerateSkill != nil {
		s.SetProjectGenerateSkill(id, *f.GenerateSkill)
	}
	if f.SessionFolders != nil {
		s.UpdateProjectSessionFolders(id, *f.SessionFolders)
	}
	if f.DisabledTools != nil {
		// Replace the entire disabled-tools map: any MCP key omitted from the
		// request is reset to "all tools allowed". Walk both the existing and new
		// keys so removals propagate.
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
		return *proj, true
	}
	return Project{}, false
}
