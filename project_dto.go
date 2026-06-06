package main

import "encoding/json"

// projectView is the frontend-facing projection of a Project: an explicit
// allow-list of fields safe to expose over relay's HTTP/frontend surface (eve,
// and through it the browser). The plaintext project token and its hash are the
// MCP-access security boundary and are deliberately excluded — the only place a
// token legitimately crosses HTTP is the explicit rotate_token response, which
// returns the new plaintext exactly once.
//
// This is an allow-list, not "Project minus the secrets", on purpose: a future
// secret-ish field added to Project stays hidden until someone consciously adds
// it here, so the frontend can never silently start leaking a new credential.
// (An embedding + json:"-" shadow does NOT work — the dropped outer field just
// uncovers the embedded one, re-leaking it.)
//
// IPC (ipc_projects.go / marshalForUI) intentionally does NOT use this view: the
// tray IS relay — the token authority — and legitimately shows and rotates the
// token in its native Projects tab. Only the eve-facing HTTP routes in
// project_routes.go project through this.
type projectView struct {
	ID               string                     `json:"id"`
	Name             string                     `json:"name"`
	Path             string                     `json:"path"`
	AllowedMcpIDs    []string                   `json:"allowed_mcp_ids"`
	AllowedModels    []string                   `json:"allowed_models"`
	ChatTemplates    []ChatTemplate             `json:"chat_templates,omitempty"`
	CreatedAt        string                     `json:"created_at"`
	DisabledTools    map[string][]string        `json:"disabled_tools,omitempty"`
	Context          map[string]json.RawMessage `json:"context,omitempty"`
	PermissionPolicy *PermissionPolicy          `json:"permission_policy,omitempty"`
	GenerateSkill    bool                       `json:"generate_skill,omitempty"`
}

func projectToView(p Project) projectView {
	return projectView{
		ID:               p.ID,
		Name:             p.Name,
		Path:             p.Path,
		AllowedMcpIDs:    p.AllowedMcpIDs,
		AllowedModels:    p.AllowedModels,
		ChatTemplates:    p.ChatTemplates,
		CreatedAt:        p.CreatedAt,
		DisabledTools:    p.DisabledTools,
		Context:          p.Context,
		PermissionPolicy: p.PermissionPolicy,
		GenerateSkill:    p.GenerateSkill,
	}
}

func projectsToView(ps []Project) []projectView {
	out := make([]projectView, 0, len(ps))
	for _, p := range ps {
		out = append(out, projectToView(p))
	}
	return out
}
