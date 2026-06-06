package main

import (
	"encoding/json"
	"log/slog"
)

// ---------------------------------------------------------------------------
// Project IPC handlers — relay's native Projects tab. Mirrors project_routes.go
// (the HTTP surface eve uses) but emits events instead of returning HTTP bodies
// so the in-tray WebView can stay reactive. The HTTP and IPC paths share the
// same Settings mutators, so a project created over IPC is identical to one
// created over HTTP and vice versa.
// ---------------------------------------------------------------------------

// ipcCreateProjectMsg mirrors the POST /api/projects body. Pointer-free —
// the create form always sends every field.
type ipcCreateProjectMsg struct {
	Name             string            `json:"name"`
	Path             string            `json:"path"`
	AllowedMcpIDs    []string          `json:"allowed_mcp_ids"`
	AllowedModels    []string          `json:"allowed_models"`
	ChatTemplates    []ChatTemplate    `json:"chat_templates"`
	PermissionPolicy *PermissionPolicy `json:"permission_policy,omitempty"`
	GenerateSkill    bool              `json:"generate_skill,omitempty"`
	DisabledTools    map[string][]string `json:"disabled_tools,omitempty"`
}

// ipcUpdateProjectMsg mirrors the PUT /api/projects/{id} body. Pointer fields
// distinguish "not in body" (no change) from explicit empty values.
type ipcUpdateProjectMsg struct {
	ID               string               `json:"id"`
	Name             *string              `json:"name,omitempty"`
	Path             *string              `json:"path,omitempty"`
	AllowedMcpIDs    *[]string            `json:"allowed_mcp_ids,omitempty"`
	AllowedModels    *[]string            `json:"allowed_models,omitempty"`
	ChatTemplates    *[]ChatTemplate      `json:"chat_templates,omitempty"`
	PermissionPolicy *PermissionPolicy    `json:"permission_policy,omitempty"`
	GenerateSkill    *bool                `json:"generate_skill,omitempty"`
	DisabledTools    *map[string][]string `json:"disabled_tools,omitempty"`
}

type ipcProjectDisabledToolsMsg struct {
	ID       string   `json:"id"`
	McpID    string   `json:"mcp_id"`
	Disabled []string `json:"disabled"`
}

type ipcListMcpToolsMsg struct {
	McpID string `json:"mcp_id"`
}

// ipcCreateProject creates a new project, applies optional skill/policy, and
// emits onProjectAdded with the full row (including the freshly-generated
// plaintext token — the user copies it from the form's Token field).
func ipcCreateProject(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcCreateProjectMsg](raw, "create_project")
	if !ok {
		return
	}

	var created Project
	var createErr error
	okSettings := ctx.withSettings(func(s *Settings) {
		schemas := mcpContextSchemasFrom(ctx)
		created, createErr = s.CreateProjectWithToken(
			msg.Name, msg.Path,
			msg.AllowedMcpIDs, msg.AllowedModels,
			msg.ChatTemplates,
			schemas,
		)
		if createErr != nil {
			return
		}
		if msg.PermissionPolicy != nil {
			if err := validatePermissionPolicy(msg.PermissionPolicy); err != nil {
				createErr = err
				// Roll back the partially-created project so the failed
				// creation doesn't leave a malformed entry behind.
				s.RemoveProject(created.ID)
				return
			}
			s.UpdateProjectPermissionPolicy(created.ID, msg.PermissionPolicy)
		}
		if msg.GenerateSkill {
			s.SetProjectGenerateSkill(created.ID, true)
		}
		for mcpID, disabled := range msg.DisabledTools {
			s.UpdateProjectDisabledTools(created.ID, mcpID, disabled)
		}
		if proj, _ := s.findProjectByID(created.ID); proj != nil {
			created = *proj
		}
	})
	if !okSettings {
		return
	}
	if createErr != nil {
		ctx.UI.EmitEvent("onProjectError", createErr.Error())
		return
	}

	// Skill regen runs off the UI thread — same pattern as the HTTP route.
	if ctx.SkillLister != nil && created.GenerateSkill {
		ctx.GoFunc(func() {
			reconcileProjectSkill(ctx.Ctx, ctx.SkillLister, created)
		})
	}

	ctx.UI.EmitEvent("onProjectAdded", marshalForUI(created))
}

// ipcUpdateProject patches an existing project. Empty body fields are
// "no change" (pointer semantics); set fields fully replace prior values.
func ipcUpdateProject(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcUpdateProjectMsg](raw, "update_project")
	if !ok || msg.ID == "" {
		return
	}
	if msg.PermissionPolicy != nil {
		if err := validatePermissionPolicy(msg.PermissionPolicy); err != nil {
			ctx.UI.EmitEvent("onProjectError", err.Error())
			return
		}
	}
	// Validate path up front (before any mutation) so an invalid path can't
	// leave a half-applied update behind — mirrors the HTTP PUT route.
	if msg.Path != nil {
		if err := validateProjectPath(*msg.Path); err != nil {
			ctx.UI.EmitEvent("onProjectError", err.Error())
			return
		}
	}

	needsSchemas := msg.Path != nil || msg.AllowedMcpIDs != nil
	var updated Project
	var found bool
	okSettings := ctx.withSettings(func(s *Settings) {
		if proj, _ := s.findProjectByID(msg.ID); proj == nil {
			return
		}
		var schemas map[string]json.RawMessage
		if needsSchemas {
			schemas = mcpContextSchemasFrom(ctx)
		}
		if msg.Name != nil {
			s.UpdateProjectName(msg.ID, *msg.Name)
		}
		if msg.Path != nil {
			s.UpdateProjectPath(msg.ID, *msg.Path, schemas)
		}
		if msg.AllowedMcpIDs != nil {
			s.UpdateProjectMcps(msg.ID, *msg.AllowedMcpIDs, schemas)
		}
		if msg.AllowedModels != nil {
			s.UpdateProjectModels(msg.ID, *msg.AllowedModels)
		}
		if msg.ChatTemplates != nil {
			s.UpdateProjectChatTemplates(msg.ID, *msg.ChatTemplates)
		}
		if msg.PermissionPolicy != nil {
			policy := msg.PermissionPolicy
			// Empty struct (no fields set) clears the policy — same rule as the HTTP route.
			if policy.DefaultMode == "" && len(policy.AllowedTools) == 0 && len(policy.DeniedTools) == 0 {
				policy = nil
			}
			s.UpdateProjectPermissionPolicy(msg.ID, policy)
		}
		if msg.GenerateSkill != nil {
			s.SetProjectGenerateSkill(msg.ID, *msg.GenerateSkill)
		}
		if msg.DisabledTools != nil {
			// Replace the entire disabled-tools map: any MCP key omitted is
			// reset to "all tools allowed". Walk both the new and existing
			// keys so removals propagate.
			existing := map[string]bool{}
			if proj, _ := s.findProjectByID(msg.ID); proj != nil {
				for k := range proj.DisabledTools {
					existing[k] = true
				}
			}
			for mcpID := range existing {
				if _, kept := (*msg.DisabledTools)[mcpID]; !kept {
					s.UpdateProjectDisabledTools(msg.ID, mcpID, nil)
				}
			}
			for mcpID, disabled := range *msg.DisabledTools {
				s.UpdateProjectDisabledTools(msg.ID, mcpID, disabled)
			}
		}
		if proj, _ := s.findProjectByID(msg.ID); proj != nil {
			updated = *proj
			found = true
		}
	})
	if !okSettings {
		return
	}
	if !found {
		ctx.UI.EmitEvent("onProjectError", "project not found")
		return
	}

	if ctx.SkillLister != nil && updated.GenerateSkill {
		ctx.GoFunc(func() {
			reconcileProjectSkill(ctx.Ctx, ctx.SkillLister, updated)
		})
	}

	ctx.UI.EmitEvent("onProjectUpdated", marshalForUI(updated))
}

// ipcRemoveProject deletes a project and its skill file. Idempotent.
func ipcRemoveProject(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "remove_project")
	if !ok || msg.ID == "" {
		return
	}

	var removed Project
	var existed bool
	okSettings := ctx.withSettings(func(s *Settings) {
		proj, _ := s.findProjectByID(msg.ID)
		if proj == nil {
			return
		}
		existed = true
		removed = *proj
		s.RemoveProject(msg.ID)
	})
	if !okSettings {
		return
	}
	if !existed {
		ctx.UI.EmitEvent("onProjectError", "project not found")
		return
	}

	if dir := projectSkillDir(removed); dir != "" {
		if err := RemoveSkill(dir); err != nil {
			slog.Warn("project skill remove failed", "project", removed.Name, "error", err)
		}
	}

	ctx.UI.EmitEvent("onProjectRemoved", msg.ID)
}

// ipcRotateProjectToken issues a new plaintext token. The old token stops
// authenticating on the next call to AuthenticateProject, so any active
// session (Eve, relayLLM, CLI) must re-auth.
func ipcRotateProjectToken(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "rotate_project_token")
	if !ok || msg.ID == "" {
		return
	}

	var newPlaintext string
	var found bool
	okSettings := ctx.withSettings(func(s *Settings) {
		newPlaintext, found = s.RotateProjectToken(msg.ID)
	})
	if !okSettings {
		return
	}
	if !found {
		ctx.UI.EmitEvent("onProjectError", "project not found")
		return
	}

	// Emit the new plaintext ONCE — the UI shows a "copy now" banner.
	// Re-fetches of the project carry the same plaintext (it lives inline in
	// the project struct) but the banner makes the rotation visible.
	ctx.UI.EmitEvent("onProjectTokenRotated", msg.ID, newPlaintext)
}

// ipcRegenProjectSkill forces a SKILL.md regeneration regardless of the
// GenerateSkill flag. The flag gates *automatic* regen on save/MCP-change;
// this is the explicit user-initiated path.
func ipcRegenProjectSkill(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "regen_project_skill")
	if !ok || msg.ID == "" {
		return
	}
	if ctx.SkillLister == nil {
		ctx.UI.EmitEvent("onProjectSkillRegen", msg.ID, false, "skill regeneration not available")
		return
	}
	proj, _ := ctx.Store.Get().findProjectByID(msg.ID)
	if proj == nil {
		ctx.UI.EmitEvent("onProjectSkillRegen", msg.ID, false, "project not found")
		return
	}
	dir := projectSkillDir(*proj)
	if dir == "" {
		ctx.UI.EmitEvent("onProjectSkillRegen", msg.ID, false, "project has no path")
		return
	}
	projCopy := *proj
	ctx.GoFunc(func() {
		if _, err := EmitSkills(ctx.Ctx, ctx.SkillLister, projCopy, dir, RegenAlways); err != nil {
			dispatchEmit(ctx, "onProjectSkillRegen", msg.ID, false, err.Error())
			return
		}
		dispatchEmit(ctx, "onProjectSkillRegen", msg.ID, true, dir)
	})
}

// ipcUpdateProjectDisabledTools is the fine-grained handler the form can
// call when the user toggles individual tools — avoids resending the entire
// project body on every checkbox click. Kept in addition to update_project
// (which patches the full map) because it makes the per-row UX cheap.
func ipcUpdateProjectDisabledTools(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcProjectDisabledToolsMsg](raw, "update_project_disabled_tools")
	if !ok || msg.ID == "" || msg.McpID == "" {
		return
	}

	var updated Project
	var found bool
	okSettings := ctx.withSettings(func(s *Settings) {
		if proj, _ := s.findProjectByID(msg.ID); proj == nil {
			return
		}
		s.UpdateProjectDisabledTools(msg.ID, msg.McpID, msg.Disabled)
		if proj, _ := s.findProjectByID(msg.ID); proj != nil {
			updated = *proj
			found = true
		}
	})
	if !okSettings {
		return
	}
	if !found {
		ctx.UI.EmitEvent("onProjectError", "project not found")
		return
	}
	ctx.UI.EmitEvent("onProjectUpdated", marshalForUI(updated))
}

// ipcListMcpTools returns the live tool list for one MCP so the picker can
// render checkboxes. Emits an empty list rather than an error when the MCP
// is registered-but-not-connected (e.g. HTTP MCP awaiting OAuth) so the UI
// can show its own "authenticate first" hint without a noisy error.
func ipcListMcpTools(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcListMcpToolsMsg](raw, "list_mcp_tools")
	if !ok || msg.McpID == "" {
		return
	}
	var infos []ToolInfo
	if ctx.Tools != nil {
		infos = ctx.Tools.ToolInfos(msg.McpID)
	}
	if infos == nil {
		infos = []ToolInfo{}
	}
	ctx.UI.EmitEvent("onMcpToolsListed", msg.McpID, marshalForUI(infos))
}

// mcpContextSchemasFrom returns the MCP context-schema map from the IPC
// context's tool provider when it also implements ContextSchemasProvider
// (the production *ExternalMcpManager satisfies both). In tests where Tools
// is a narrow stub, returns nil and SyncProjectToken falls back to its
// "no filesystem auto-detect" path.
func mcpContextSchemasFrom(ctx *IPCContext) map[string]json.RawMessage {
	if p, ok := ctx.Tools.(ContextSchemasProvider); ok {
		return p.AllContextSchemas()
	}
	return nil
}
