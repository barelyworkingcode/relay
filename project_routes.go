package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
)

// McpSurfaceProvider supplies what relay knows at runtime about each MCP —
// context schema, its version, and the tool surface — required when
// (re)scoping a project's token and when validating its grants.
// Implemented by *ExternalMcpManager.
type McpSurfaceProvider interface {
	AllMcpSurfaces() McpSurfaces
}

// MCPToolsProvider supplies the live tool list for a registered MCP. The
// project picker UI needs this to render the per-tool selector. Implemented
// by *ExternalMcpManager; nil-safe in route handlers.
type MCPToolsProvider interface {
	ToolInfos(id string) []ToolInfo
}

// enumHTTPStatus maps an enumeration outcome onto an HTTP code.
//
// The body always carries the precise `status` — a client that must tell
// "does not implement" from "could not answer right now" reads that field, not
// the code. What the code is for is the client that reads only codes: a
// failure must never arrive as a 200 whose empty body could be mistaken for
// "there are none". So the two MCP failures get 5xx (the upstream refused, or
// could not answer), relay's own refusals get 4xx, and only a real answer is
// a 200.
func enumHTTPStatus(status string) int {
	switch status {
	case EnumStatusOK, EnumStatusUnsupported:
		// Unsupported is a 200 because it is a true, final answer ABOUT the
		// MCP — "this one does not enumerate" — not a failure to obtain one.
		// The caller's correct response is to render a text box, permanently.
		return http.StatusOK
	case EnumStatusUnknownMcp:
		return http.StatusNotFound
	case EnumStatusNotEnumerable:
		return http.StatusBadRequest
	case EnumStatusInvalidField:
		// The MCP refused the request relay built. Relay is the one at fault,
		// and a 502 says the failure is on this side of the operator.
		return http.StatusBadGateway
	default: // EnumStatusUnavailable
		return http.StatusServiceUnavailable
	}
}

// enumerateRequest is the POST body for the enumeration route.
//
// POST rather than GET, for a read: `values` is a map from field name to that
// field's already-chosen value, whose shape is whatever the MCP declared, and
// there is no query-string encoding of that which does not quietly assume
// array-of-string. A GET would work today and break on the first MCP that
// declares something else, which is precisely the domain knowledge ADR-011
// decision 3 keeps out of relay.
type enumerateRequest struct {
	Field  string                     `json:"field"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
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
// enum asks a connected MCP to list a scope field's real values; nil makes
// POST /api/mcps/{id}/enumerate answer "unavailable" rather than 404, because
// the field still exists and the editor's fallback is still text entry.
//
// skillLister resolves the live tool set for a project token; supplying nil
// disables out-of-band skill regen.
//
// tools enumerates the live MCP tool list for the project-picker UI; nil
// makes the GET /api/mcps/{id}/tools route return 503.
//
// onChange fires after any successful create/update/delete/rotate so the
// tray-window state can re-render. nil = no fan-out (tests use this).
func RegisterProjectRoutes(mux *http.ServeMux, store SettingsStore, mcps McpSurfaceProvider, tools MCPToolsProvider, enum ContextEnumerator, skillLister SkillLister, onChange ProjectsChangedFn) {
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
		var body projectCreateFields
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
			created, createErr = applyProjectCreate(s, body, mcps.AllMcpSurfaces())
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
		var body projectUpdateFields
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
		// Shape/grant validation (including path) now happens inside
		// applyProjectUpdate against the fully-merged candidate. Whether an
		// empty path is valid depends on Kind (required for local, mandatory
		// for remote), so a standalone path-only pre-check can no longer judge
		// it correctly — the merged candidate is the only place that knows.
		var updated Project
		var found bool
		var updateErr error
		if err := store.With(func(s *Settings) {
			updated, found, updateErr = applyProjectUpdate(s, id, body, mcps.AllMcpSurfaces)
		}); err != nil {
			slog.Error("update project: save failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save settings"})
			return
		}
		if updateErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": updateErr.Error()})
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
		var genErr error
		if err := store.With(func(s *Settings) {
			newPlaintext, ok, genErr = s.RotateProjectToken(id)
		}); err != nil {
			slog.Error("rotate project token: save failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save settings"})
			return
		}
		if genErr != nil {
			slog.Error("rotate project token: token generation failed", "error", genErr)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate token"})
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

	// GET /api/mcps/{id}/scope_fields — the scope: "restrict" fields an MCP
	// declares, so an editor can render one input per field with the MCP's own
	// description as help text (ADR-011 decision 6). It is the same projection
	// the tray's Settings UI is seeded with; ADR-004's two co-equal editors
	// cannot stay co-equal if only one of them can see what may be narrowed.
	//
	// An MCP relay has never connected to has no entry, and that is a 404
	// rather than an empty list: "this MCP scopes nothing" and "relay cannot
	// tell you what this MCP scopes" are different answers, and only one of
	// them means an editor may safely offer no fields.
	mux.HandleFunc("GET /api/mcps/{id}/scope_fields", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		surfaces := mcps.AllMcpSurfaces()
		if _, ok := surfaces[id]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "MCP not registered or not connected"})
			return
		}
		writeJSON(w, http.StatusOK, surfaces.Schema(id).ScopeFieldViews())
	})

	// POST /api/mcps/{id}/enumerate — ask the MCP for one scope field's real
	// values (ADR-011 decision 6), so the editor offers a picker instead of a
	// free-text box whose easiest failure is a confinement that does not
	// confine. Body: {"field": "...", "values": {"<dependency>": <chosen>}}.
	//
	// It sits on the same mux as every other project route, which is the
	// guard: the frontend socket is 0600 and every request through it is
	// bearer-checked by frontendBearerAuth. Enumeration is disclosure — the
	// list of every mail account on this machine — so it belongs behind the
	// same admin boundary and nowhere near the remote listener, whose dispatch
	// table is ListTools and CallTool and gains nothing here.
	mux.HandleFunc("POST /api/mcps/{id}/enumerate", func(w http.ResponseWriter, r *http.Request) {
		var body enumerateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		res := enumerateScopeField(r.Context(), mcps.AllMcpSurfaces(), enum, r.PathValue("id"), body.Field, body.Values)
		writeJSON(w, enumHTTPStatus(res.Status), res)
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
