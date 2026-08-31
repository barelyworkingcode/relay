package main

import "encoding/json"

// projectView is the projection of a Project safe to expose to a caller that
// is not the tray itself: relay's HTTP/frontend surface (eve, and through it
// the browser) and the bridge's ListProjects/GetProject, answered to any
// service-token holder. The plaintext project token and its hash are
// deliberately excluded — the only place a token legitimately crosses either
// surface is the rotate_token response, which returns the new plaintext
// exactly once, and ResolvePtyEnv, the bridge's sole plaintext-token egress.
//
// This is an allow-list, not "Project minus the secrets", on purpose: a future
// secret-ish field added to Project stays hidden until someone consciously adds
// it here. (An embedding + json:"-" shadow does NOT work — the dropped outer
// field just uncovers the embedded one, re-leaking it.)
//
// IPC (ipc_projects.go / marshalForUI) intentionally does NOT use this view:
// the tray IS relay — the token authority — and legitimately shows and rotates
// the token in its native Projects tab. Only the eve-facing HTTP routes in
// project_routes.go project through this.
type projectView struct {
	ID               string                     `json:"id"`
	Name             string                     `json:"name"`
	Path             string                     `json:"path"`
	Kind             ProjectKind                `json:"kind,omitempty"`
	AllowedMcpIDs    []string                   `json:"allowed_mcp_ids"`
	AllowedModels    []string                   `json:"allowed_models"`
	ChatTemplates    []ChatTemplate             `json:"chat_templates,omitempty"`
	ShellTemplates   []ShellTemplate            `json:"shell_templates,omitempty"`
	CreatedAt        string                     `json:"created_at"`
	DisabledTools    map[string][]string        `json:"disabled_tools,omitempty"`
	Context          map[string]json.RawMessage `json:"context,omitempty"`
	AllowedTools     map[string][]string        `json:"allowed_tools,omitempty"`
	Access           map[string]string          `json:"access,omitempty"`
	AllowExternal    map[string]bool            `json:"allow_external,omitempty"`
	PermissionPolicy *PermissionPolicy          `json:"permission_policy,omitempty"`
	GenerateSkill    bool                       `json:"generate_skill,omitempty"`
	AllowCwdAuth     bool                       `json:"allow_cwd_auth,omitempty"`
	SessionFolders   []string                   `json:"session_folders,omitempty"`
}

func projectToView(p Project) projectView {
	return projectView{
		ID:               p.ID,
		Name:             p.Name,
		Path:             p.Path,
		Kind:             p.Kind,
		AllowedMcpIDs:    p.AllowedMcpIDs,
		AllowedModels:    p.AllowedModels,
		ChatTemplates:    p.ChatTemplates,
		ShellTemplates:   p.ShellTemplates,
		CreatedAt:        p.CreatedAt,
		DisabledTools:    p.DisabledTools,
		Context:          p.Context,
		AllowedTools:     p.AllowedTools,
		Access:           p.Access,
		AllowExternal:    p.AllowExternal,
		PermissionPolicy: p.PermissionPolicy,
		GenerateSkill:    p.GenerateSkill,
		AllowCwdAuth:     p.AllowCwdAuth,
		SessionFolders:   p.SessionFolders,
	}
}

func projectsToView(ps []Project) []projectView {
	out := make([]projectView, 0, len(ps))
	for _, p := range ps {
		out = append(out, projectToView(p))
	}
	return out
}
