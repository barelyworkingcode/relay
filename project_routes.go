package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ContextSchemasProvider supplies MCP context schemas required when
// (re)scoping a project's token. Implemented by *ExternalMcpManager.
type ContextSchemasProvider interface {
	AllContextSchemas() map[string]json.RawMessage
}

// RegisterProjectRoutes wires the project HTTP endpoints. Payloads are
// snake_case to match relay's on-disk format; Eve normalizes to camelCase
// on its side.
//
// Mutation routes (POST/PUT/DELETE) wrap the existing Settings mutators
// (CreateProjectWithToken, UpdateProject*, RemoveProject) inside store.With.
// Settings are persisted on save; cross-process state stays consistent
// because relay's bridge re-reads settings on every ListProjects/GetProject.
func RegisterProjectRoutes(mux *http.ServeMux, store SettingsStore, mcps ContextSchemasProvider) {
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		projects := store.Get().Projects
		if projects == nil {
			projects = []Project{}
		}
		writeJSON(w, http.StatusOK, projects)
	})

	mux.HandleFunc("GET /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		proj, _ := store.Get().findProjectByID(r.PathValue("id"))
		if proj == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusOK, proj)
	})

	mux.HandleFunc("POST /api/projects", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name          string         `json:"name"`
			Path          string         `json:"path"`
			AllowedMcpIDs []string       `json:"allowed_mcp_ids"`
			AllowedModels []string       `json:"allowed_models"`
			ChatTemplates []ChatTemplate `json:"chat_templates"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
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
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if createErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": createErr.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, created)
	})

	mux.HandleFunc("PUT /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// Pointer fields distinguish "not in body" from "zero value" so callers
		// can patch a single field without clearing the others.
		var body struct {
			Name          *string         `json:"name,omitempty"`
			Path          *string         `json:"path,omitempty"`
			AllowedMcpIDs *[]string       `json:"allowed_mcp_ids,omitempty"`
			AllowedModels *[]string       `json:"allowed_models,omitempty"`
			ChatTemplates *[]ChatTemplate `json:"chat_templates,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
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
			if proj, _ := s.findProjectByID(id); proj != nil {
				updated = *proj
				found = true
			}
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})

	mux.HandleFunc("DELETE /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var existed bool
		if err := store.With(func(s *Settings) {
			if proj, _ := s.findProjectByID(id); proj == nil {
				return
			}
			existed = true
			s.RemoveProject(id)
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if !existed {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
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
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("frontend: failed to encode response", "error", err)
	}
}
