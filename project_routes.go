package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
)

// ContextSchemasProvider supplies MCP context schemas required when
// (re)scoping a project's token. Implemented by *ExternalMcpManager.
type ContextSchemasProvider interface {
	AllContextSchemas() map[string]json.RawMessage
}

// MCPToolsProvider supplies the live tool list for a registered MCP. The
// project picker UI needs this to render the per-tool selector. Implemented
// by *ExternalMcpManager; nil-safe in route handlers.
type MCPToolsProvider interface {
	ToolInfos(id string) []ToolInfo
}

// ProjectsChangedFn is fired after any successful project mutation so the
// tray UI can refresh. nil = no fan-out.
type ProjectsChangedFn func()

// projectSkillDir is the skills root (under Project.Path) that relay manages.
// relay writes one "relay-<slug>" subdir per tool bucket here; Claude Code
// auto-discovers all of them from .claude/skills/, and Pi.Dev gets pointed at
// this root via --skill in its PTY template. User-authored skills can live
// alongside under the same root — relay only touches its own "relay-*" dirs.
func projectSkillDir(proj Project) string {
	if proj.Path == "" {
		return ""
	}
	return filepath.Join(proj.Path, ".claude", "skills")
}

// reconcileProjectSkill brings the on-disk skill state into sync with the
// project's GenerateSkill flag. Toggling on regenerates; deletion removes.
// Toggling off leaves stale files in place — the user removes them manually
// if desired. Best-effort: errors are logged, not returned.
func reconcileProjectSkill(ctx context.Context, lister SkillLister, proj Project) {
	if !proj.GenerateSkill {
		return
	}
	dir := projectSkillDir(proj)
	if dir == "" {
		slog.Warn("skill regen skipped: project has no path", "project", proj.Name)
		return
	}
	if _, err := EmitSkills(ctx, lister, proj, dir, RegenAlways); err != nil {
		slog.Warn("project skill regen failed", "project", proj.Name, "error", err)
	}
}

// RegisterProjectRoutes wires the project HTTP endpoints. Payloads are
// snake_case to match relay's on-disk format; Eve normalizes to camelCase
// on its side.
//
// Mutation routes (POST/PUT/DELETE) wrap the existing Settings mutators
// (CreateProjectWithToken, UpdateProject*, RemoveProject) inside store.With.
// Settings are persisted on save; cross-process state stays consistent
// because relay's bridge re-reads settings on every ListProjects/GetProject.
//
// skillLister resolves the live tool set for a project token; supplying nil
// disables out-of-band skill regen.
//
// tools enumerates the live MCP tool list for the project-picker UI; nil
// makes the GET /api/mcps/{id}/tools route return 503.
//
// onChange fires after any successful create/update/delete/rotate so the
// tray-window state can re-render. nil = no fan-out (tests use this).
func RegisterProjectRoutes(mux *http.ServeMux, store SettingsStore, mcps ContextSchemasProvider, tools MCPToolsProvider, skillLister SkillLister, onChange ProjectsChangedFn) {
	notify := func() {
		if onChange != nil {
			onChange()
		}
	}
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		projects := store.Get().Projects
		if projects == nil {
			projects = []Project{}
		}
		// projectView strips the plaintext token from the frontend response.
		writeJSON(w, http.StatusOK, projectsToView(projects))
	})

	mux.HandleFunc("GET /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		proj, _ := store.Get().findProjectByID(r.PathValue("id"))
		if proj == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusOK, projectToView(*proj))
	})

	mux.HandleFunc("POST /api/projects", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name             string              `json:"name"`
			Path             string              `json:"path"`
			AllowedMcpIDs    []string            `json:"allowed_mcp_ids"`
			AllowedModels    []string            `json:"allowed_models"`
			ChatTemplates    []ChatTemplate      `json:"chat_templates"`
			PermissionPolicy *PermissionPolicy   `json:"permission_policy,omitempty"`
			GenerateSkill    bool                `json:"generate_skill,omitempty"`
			DisabledTools    map[string][]string `json:"disabled_tools,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if err := validatePermissionPolicy(body.PermissionPolicy); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		var created Project
		var createErr error
		if err := store.With(func(s *Settings) {
			created, createErr = s.CreateProjectWithToken(
				body.Name, body.Path,
				body.AllowedMcpIDs, body.AllowedModels,
				body.ChatTemplates,
				mcps.AllContextSchemas(),
			)
			if createErr != nil {
				return
			}
			if body.PermissionPolicy != nil {
				s.UpdateProjectPermissionPolicy(created.ID, body.PermissionPolicy)
			}
			if body.GenerateSkill {
				s.SetProjectGenerateSkill(created.ID, true)
			}
			for mcpID, disabled := range body.DisabledTools {
				s.UpdateProjectDisabledTools(created.ID, mcpID, disabled)
			}
			if proj, _ := s.findProjectByID(created.ID); proj != nil {
				created = *proj
			}
		}); err != nil {
			slog.Error("create project: save failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save settings"})
			return
		}
		if createErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": createErr.Error()})
			return
		}
		if skillLister != nil {
			reconcileProjectSkill(r.Context(), skillLister, created)
		}
		notify()
		writeJSON(w, http.StatusCreated, projectToView(created))
	})

	mux.HandleFunc("PUT /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// Pointer fields distinguish "not in body" from "zero value" so callers
		// can patch a single field without clearing the others.
		var body struct {
			Name             *string              `json:"name,omitempty"`
			Path             *string              `json:"path,omitempty"`
			AllowedMcpIDs    *[]string            `json:"allowed_mcp_ids,omitempty"`
			AllowedModels    *[]string            `json:"allowed_models,omitempty"`
			ChatTemplates    *[]ChatTemplate      `json:"chat_templates,omitempty"`
			PermissionPolicy *PermissionPolicy    `json:"permission_policy,omitempty"`
			GenerateSkill    *bool                `json:"generate_skill,omitempty"`
			DisabledTools    *map[string][]string `json:"disabled_tools,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if body.PermissionPolicy != nil {
			if err := validatePermissionPolicy(body.PermissionPolicy); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		// Validate path up front (before any mutation) so an invalid path can't
		// leave a half-applied update behind.
		if body.Path != nil {
			if err := validateProjectPath(*body.Path); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}

		// Schemas are only needed when path or MCPs change; defer the fetch
		// (and its map allocation) so the common rename-only edit stays cheap.
		needsSchemas := body.Path != nil || body.AllowedMcpIDs != nil
		var updated Project
		var found bool
		if err := store.With(func(s *Settings) {
			if proj, _ := s.findProjectByID(id); proj == nil {
				return
			}
			var schemas map[string]json.RawMessage
			if needsSchemas {
				schemas = mcps.AllContextSchemas()
			}
			if body.Name != nil {
				s.UpdateProjectName(id, *body.Name)
			}
			if body.Path != nil {
				s.UpdateProjectPath(id, *body.Path, schemas)
			}
			if body.AllowedMcpIDs != nil {
				s.UpdateProjectMcps(id, *body.AllowedMcpIDs, schemas)
			}
			if body.AllowedModels != nil {
				s.UpdateProjectModels(id, *body.AllowedModels)
			}
			if body.ChatTemplates != nil {
				s.UpdateProjectChatTemplates(id, *body.ChatTemplates)
			}
			if body.PermissionPolicy != nil {
				policy := body.PermissionPolicy
				// Empty struct (no fields set) is treated as "clear policy".
				if policy.DefaultMode == "" && len(policy.AllowedTools) == 0 && len(policy.DeniedTools) == 0 {
					policy = nil
				}
				s.UpdateProjectPermissionPolicy(id, policy)
			}
			if body.GenerateSkill != nil {
				s.SetProjectGenerateSkill(id, *body.GenerateSkill)
			}
			if body.DisabledTools != nil {
				// Replace the entire disabled-tools map: any MCP key omitted is
				// reset to "all tools allowed". Mirrors the IPC update path.
				existing := map[string]bool{}
				if proj, _ := s.findProjectByID(id); proj != nil {
					for k := range proj.DisabledTools {
						existing[k] = true
					}
				}
				for mcpID := range existing {
					if _, kept := (*body.DisabledTools)[mcpID]; !kept {
						s.UpdateProjectDisabledTools(id, mcpID, nil)
					}
				}
				for mcpID, disabled := range *body.DisabledTools {
					s.UpdateProjectDisabledTools(id, mcpID, disabled)
				}
			}
			if proj, _ := s.findProjectByID(id); proj != nil {
				updated = *proj
				found = true
			}
		}); err != nil {
			slog.Error("update project: save failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save settings"})
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		if skillLister != nil {
			reconcileProjectSkill(r.Context(), skillLister, updated)
		}
		notify()
		writeJSON(w, http.StatusOK, projectToView(updated))
	})

	mux.HandleFunc("DELETE /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var existed bool
		var removed Project
		if err := store.With(func(s *Settings) {
			proj, _ := s.findProjectByID(id)
			if proj == nil {
				return
			}
			existed = true
			removed = *proj
			s.RemoveProject(id)
		}); err != nil {
			slog.Error("delete project: save failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save settings"})
			return
		}
		if !existed {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		if dir := projectSkillDir(removed); dir != "" {
			if err := RemoveSkill(dir); err != nil {
				slog.Warn("project skill remove failed", "project", removed.Name, "error", err)
			}
		}
		notify()
		w.WriteHeader(http.StatusNoContent)
	})

	// MCP listing for the Eve project dialog's "Allowed MCPs" picker.
	// Returns id + display_name only; OAuth state and credentials stay private.
	mux.HandleFunc("GET /api/mcps", func(w http.ResponseWriter, r *http.Request) {
		mcps := store.Get().ExternalMcps
		out := make([]map[string]string, 0, len(mcps))
		for _, m := range mcps {
			out = append(out, map[string]string{
				"id":           m.ID,
				"display_name": m.DisplayName,
			})
		}
		writeJSON(w, http.StatusOK, out)
	})

	// POST /api/projects/{id}/rotate_token — rotate the project's bearer
	// credential. Returns the new plaintext exactly once; clients must capture
	// it. Old token stops authenticating on the next request.
	mux.HandleFunc("POST /api/projects/{id}/rotate_token", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var newPlaintext string
		var ok bool
		if err := store.With(func(s *Settings) {
			newPlaintext, ok = s.RotateProjectToken(id)
		}); err != nil {
			slog.Error("rotate project token: save failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save settings"})
			return
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		notify()
		writeJSON(w, http.StatusOK, map[string]string{"token": newPlaintext})
	})

	// POST /api/projects/{id}/regen_skill — force a SKILL.md regen for one
	// project regardless of GenerateSkill (the toggle gates *automatic* regen;
	// this is the explicit "do it now" button).
	mux.HandleFunc("POST /api/projects/{id}/regen_skill", func(w http.ResponseWriter, r *http.Request) {
		if skillLister == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "skill regeneration not available in this mode"})
			return
		}
		id := r.PathValue("id")
		proj, _ := store.Get().findProjectByID(id)
		if proj == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		dir := projectSkillDir(*proj)
		if dir == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project has no path"})
			return
		}
		if _, err := EmitSkills(r.Context(), skillLister, *proj, dir, RegenAlways); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": dir})
	})

	// GET /api/mcps/{id}/tools — live tool list for the project picker.
	// 503 when no provider is wired (test contexts) or 404 when MCP is unknown
	// / not connected yet.
	mux.HandleFunc("GET /api/mcps/{id}/tools", func(w http.ResponseWriter, r *http.Request) {
		if tools == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "tool list not available"})
			return
		}
		infos := tools.ToolInfos(r.PathValue("id"))
		if infos == nil {
			// Distinguish unknown from empty-but-connected for the UI hint.
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "MCP not registered or not connected"})
			return
		}
		writeJSON(w, http.StatusOK, infos)
	})
}

var validPermissionModes = map[string]bool{
	"":                  true, // empty = inherit (default)
	"default":           true,
	"acceptEdits":       true,
	"plan":              true,
	"bypassPermissions": true,
}

// validatePermissionPolicy rejects unknown modes and oversized tool lists.
// Tool patterns are not parsed here — Claude CLI accepts a wide grammar
// (e.g. "Bash(ls *)") and we don't want to drift from upstream rules.
func validatePermissionPolicy(p *PermissionPolicy) error {
	if p == nil {
		return nil
	}
	if !validPermissionModes[p.DefaultMode] {
		return fmt.Errorf("invalid default_mode: %s", p.DefaultMode)
	}
	if len(p.AllowedTools) > 256 || len(p.DeniedTools) > 256 {
		return fmt.Errorf("tool list exceeds 256 entries")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("frontend: failed to encode response", "error", err)
	}
}
