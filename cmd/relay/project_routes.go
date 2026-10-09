package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/project"
)

// projectOpsHTTPStatus maps a ProjectOps refusal onto the status a caller
// should see: a presence or audit-dependency refusal is 403, an internal
// settings-write failure is 500, and everything else (a validation error
// from project.ApplyCreate/project.ApplyUpdate) is 400 — the status this
// route always gave a createErr/updateErr before ProjectOps existed.
func projectOpsHTTPStatus(err error) int {
	switch {
	case errors.Is(err, presence.ErrGrantInvalid), errors.Is(err, presence.ErrRefused),
		errors.Is(err, presence.ErrNoSession), errors.Is(err, presence.ErrUnavailable),
		errors.Is(err, errPresenceGateNotWired), errors.Is(err, errIssuanceAuditingRequired):
		return http.StatusForbidden
	case errors.Is(err, errProjectChangedDuringApproval):
		return http.StatusConflict
	case errors.Is(err, errProjectSaveFailed), errors.Is(err, errProjectTokenUnrecorded):
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

func writeProjectGateError(w http.ResponseWriter, err error) {
	writeJSON(w, projectOpsHTTPStatus(err), map[string]string{"error": err.Error()})
}

// McpSurfaceProvider supplies what relay knows at runtime about each MCP —
// context schema, its version, and the tool surface — required when
// (re)scoping a project's token and when validating its grants.
// Implemented by *mcpbroker.Manager.
type McpSurfaceProvider interface {
	AllMcpSurfaces() project.McpSurfaces
}

// MCPToolsProvider supplies the live tool list for a registered MCP. The
// project picker UI needs this to render the per-tool selector. Implemented
// by *mcpbroker.Manager; nil-safe in route handlers.
type MCPToolsProvider interface {
	ToolInfos(id string) []config.ToolInfo
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
	case project.EnumStatusOK, project.EnumStatusUnsupported:
		// Unsupported is a 200 because it is a true, final answer ABOUT the
		// MCP — "this one does not enumerate" — not a failure to obtain one.
		// The caller's correct response is to render a text box, permanently.
		return http.StatusOK
	case project.EnumStatusUnknownMcp:
		return http.StatusNotFound
	case project.EnumStatusNotEnumerable:
		return http.StatusBadRequest
	case project.EnumStatusInvalidField:
		// The MCP refused the request relay built. Relay is the one at fault,
		// and a 502 says the failure is on this side of the operator.
		return http.StatusBadGateway
	default: // project.EnumStatusUnavailable
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

// setDefaultProjectRequest's ProjectID is a pointer because "" is a valid
// request (clear the default) and a missing key is not.
type setDefaultProjectRequest struct {
	ProjectID *string `json:"project_id"`
}

const errDefaultProjectIDRequired = `project_id is required; send "" to clear the default`

// setChiefOfStaffRequest takes pointers so a missing key is not a zero value.
// DailyModelCalls is a float64 so a fractional number reaches the range check
// as daily_model_calls_invalid rather than a decode error.
type setChiefOfStaffRequest struct {
	ProjectID       *string  `json:"projectId"`
	Model           *string  `json:"model"`
	DailyModelCalls *float64 `json:"dailyModelCalls"`
}

const maxChiefOfStaffConfigBodyBytes = 4 << 10

func writeChiefOfStaffConfigError(w http.ResponseWriter, err error) {
	var cosErr *config.ChiefOfStaffError
	if errors.As(err, &cosErr) {
		writeChiefOfStaffError(w, http.StatusBadRequest, cosErr.Code, cosErr.Message)
		return
	}
	writeChiefOfStaffError(w, http.StatusInternalServerError, "save_failed", "failed to save settings")
}

// ProjectsChangedFn is fired after any successful project mutation so the
// tray UI can refresh. nil = no fan-out.
type ProjectsChangedFn func()

// projectSkillDir is the skills root (under Project.Path) that relay manages.
// relay writes one "relay-<slug>" subdir per tool bucket here; Claude Code
// auto-discovers all of them from .claude/skills/, and Pi.Dev gets pointed at
// this root via --skill in its PTY template. User-authored skills can live
// alongside under the same root — relay only touches its own "relay-*" dirs.
func projectSkillDir(proj config.Project) string {
	if proj.Path == "" {
		return ""
	}
	return filepath.Join(proj.Path, ".claude", "skills")
}

// reconcileProjectSkill brings the on-disk skill state into sync with the
// project's GenerateSkill flag. Toggling on regenerates; deletion removes.
// Toggling off leaves stale files in place — the user removes them manually
// if desired. Best-effort: errors are logged, not returned.
func reconcileProjectSkill(ctx context.Context, lister SkillLister, proj config.Project) {
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
// onChange is unused: every mutation goes through ops, and the tray refreshes
// from the config queue's post-commit event. The parameter stays for callers
// and doc symmetry with the IPC surface.
func RegisterProjectRoutes(rr *control.RouteRegistrar, store config.SettingsStore, ops *ProjectOps, mcps McpSurfaceProvider, tools MCPToolsProvider, enum project.ContextEnumerator, skillLister SkillLister, onChange ProjectsChangedFn) { //nolint:unparam // deliberate: kept for doc/call-site symmetry with the IPC surface, see comment above
	rr.Handle(control.ClassRead, "GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		ev := logging.BeginEvent(r.Context(), "project.list")
		s := config.DisplaySettings(store)
		projects := s.Projects
		if projects == nil {
			projects = []config.Project{}
		}
		ev.Set("count", len(projects)).End(logging.OutcomeOK, "", nil)
		// projectView strips the plaintext token from the frontend response.
		writeJSON(w, http.StatusOK, projectsToView(s, projects))
	})

	rr.Handle(control.ClassRead, "GET /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		ev := logging.BeginEvent(r.Context(), "project.get").Set("project_id", r.PathValue("id"))
		s := config.DisplaySettings(store)
		proj, _ := config.FindProjectByID(s, r.PathValue("id"))
		if proj == nil {
			endEventHTTP(ev, http.StatusNotFound, "project not found")
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		ev.End(logging.OutcomeOK, "", nil)
		writeJSON(w, http.StatusOK, projectToView(s, *proj))
	})

	rr.HandleGated(control.ClassConfigure, []string{"project.grant"}, "POST /api/projects", func(w http.ResponseWriter, r *http.Request) {
		var body project.CreateFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if err := validatePermissionPolicy(body.PermissionPolicy); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		created, createErr := ops.Create(r.Context(), body, mcps.AllMcpSurfaces(), auditViaHTTP, credIDOf(r))
		if createErr != nil {
			writeProjectGateError(w, createErr)
			return
		}
		if skillLister != nil {
			reconcileProjectSkill(r.Context(), skillLister, created)
		}
		writeJSON(w, http.StatusCreated, projectToView(config.DisplaySettings(store), created))
	})

	rr.HandleGated(control.ClassConfigure, []string{"project.grant"}, "PUT /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// Pointer fields distinguish "not in body" from "zero value" so callers
		// can patch a single field without clearing the others.
		var body project.UpdateFields
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
		// project.ApplyUpdate against the fully-merged candidate. Whether an
		// empty path is valid depends on Kind (required for local, mandatory
		// for remote), so a standalone path-only pre-check can no longer judge
		// it correctly — the merged candidate is the only place that knows.
		updated, found, updateErr := ops.Update(r.Context(), id, body, mcps.AllMcpSurfaces, auditViaHTTP, credIDOf(r))
		if updateErr != nil {
			writeProjectGateError(w, updateErr)
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		if skillLister != nil {
			reconcileProjectSkill(r.Context(), skillLister, updated)
		}
		writeJSON(w, http.StatusOK, projectToView(config.DisplaySettings(store), updated))
	})

	rr.Handle(control.ClassConfigure, "DELETE /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		found, err := ops.Remove(r.Context(), id)
		if err != nil {
			slog.Error("delete project: save failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save settings"})
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// PUT /api/default_project/{mode} — set or clear ("") the default
	// project for home or work. configure, not grant: a default is a label
	// and reaches nothing the project's own grant does not already reach.
	rr.Handle(control.ClassConfigure, "PUT /api/default_project/{mode}", func(w http.ResponseWriter, r *http.Request) {
		var body setDefaultProjectRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if body.ProjectID == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errDefaultProjectIDRequired})
			return
		}
		defaults, err := ops.SetDefaultProject(r.Context(), config.ProjectMode(r.PathValue("mode")), *body.ProjectID)
		if err != nil {
			writeProjectGateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, defaultProjectViewOf(defaults))
	})

	// The Chief of Staff setting. The scope header never reaches these doors:
	// Authorize refuses read and configure inside the chief-of-staff scope, so
	// the Chief of Staff cannot repoint itself.
	rr.Handle(control.ClassRead, "GET /api/chief-of-staff/config", func(w http.ResponseWriter, r *http.Request) {
		logging.BeginEvent(r.Context(), "chief_of_staff.config.get").End(logging.OutcomeOK, "", nil)
		writeJSON(w, http.StatusOK, chiefOfStaffViewOf(config.FreshSettings(store)))
	})

	rr.Handle(control.ClassConfigure, "PUT /api/chief-of-staff/config", func(w http.ResponseWriter, r *http.Request) {
		var body setChiefOfStaffRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxChiefOfStaffConfigBodyBytes)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeChiefOfStaffError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is larger than 4 KiB")
				return
			}
			writeChiefOfStaffError(w, http.StatusBadRequest, "invalid_body", `body must be JSON {"projectId","model","dailyModelCalls"}`)
			return
		}
		if body.ProjectID == nil || body.Model == nil || body.DailyModelCalls == nil {
			writeChiefOfStaffError(w, http.StatusBadRequest, "invalid_body", "projectId, model and dailyModelCalls are all required")
			return
		}
		calls := -1 // a fraction is refused as daily_model_calls_invalid, after the earlier checks
		if *body.DailyModelCalls == math.Trunc(*body.DailyModelCalls) && math.Abs(*body.DailyModelCalls) <= math.MaxInt32 {
			calls = int(*body.DailyModelCalls)
		}
		stored, err := ops.SetChiefOfStaff(r.Context(), config.ChiefOfStaffConfig{
			ProjectID: *body.ProjectID, Model: *body.Model, DailyModelCalls: calls,
		})
		if err != nil {
			writeChiefOfStaffConfigError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, chiefOfStaffViewFromConfig(stored))
	})

	rr.Handle(control.ClassConfigure, "DELETE /api/chief-of-staff/config", func(w http.ResponseWriter, r *http.Request) {
		if err := ops.ClearChiefOfStaff(r.Context()); err != nil {
			writeChiefOfStaffConfigError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, chiefOfStaffView{})
	})

	// MCP listing for the Eve project dialog's "Allowed MCPs" picker.
	// Returns id + display_name only; OAuth state and credentials stay private.
	rr.Handle(control.ClassRead, "GET /api/mcps", func(w http.ResponseWriter, r *http.Request) {
		ev := logging.BeginEvent(r.Context(), "mcp.list")
		mcps := config.DisplaySettings(store).ExternalMcps
		ev.Set("count", len(mcps)).End(logging.OutcomeOK, "", nil)
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
	//
	// grant: this issues a credential another party holds (ADR-015 decision
	// 1), the same reasoning as enrolment create.
	rr.HandleGated(control.ClassGrant, []string{"project.rotate_token"}, "POST /api/projects/{id}/rotate_token", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// ProjectOps.RotateToken records the rotation itself (so it can
		// attach the presence_id the gate minted) and withholds the new
		// plaintext when that record cannot be written — the old token is
		// already dead either way, so refusing here still means no project
		// token reaches a holder unrecorded.
		newPlaintext, ok, err := ops.RotateToken(r.Context(), id, auditViaHTTP, credIDOf(r))
		if err != nil {
			writeProjectGateError(w, err)
			return
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"token": newPlaintext})
	})

	// POST /api/projects/{id}/regen_skill — force a SKILL.md regen for one
	// project regardless of GenerateSkill (the toggle gates *automatic* regen;
	// this is the explicit "do it now" button).
	rr.Handle(control.ClassConfigure, "POST /api/projects/{id}/regen_skill", func(w http.ResponseWriter, r *http.Request) {
		if skillLister == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "skill regeneration not available in this mode"})
			return
		}
		id := r.PathValue("id")
		dir, found, err := ops.RegenSkill(r.Context(), skillLister, id)
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "project not found"})
			return
		}
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errProjectHosted) || errors.Is(err, errProjectHasNoPath) {
				status = http.StatusBadRequest
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
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
	rr.Handle(control.ClassRead, "GET /api/mcps/{id}/scope_fields", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ev := logging.BeginEvent(r.Context(), "mcp.scope_fields.get").Set("mcp_id", id)
		surfaces := mcps.AllMcpSurfaces()
		if _, ok := surfaces[id]; !ok {
			endEventHTTP(ev, http.StatusNotFound, "MCP not registered or not connected")
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "MCP not registered or not connected"})
			return
		}
		ev.End(logging.OutcomeOK, "", nil)
		writeJSON(w, http.StatusOK, surfaces.Schema(id).ScopeFieldViews())
	})

	// POST /api/mcps/{id}/enumerate — ask the MCP for one scope field's real
	// values (ADR-011 decision 6), so the editor offers a picker instead of a
	// free-text box whose easiest failure is a confinement that does not
	// confine. Body: {"field": "...", "values": {"<dependency>": <chosen>}}.
	//
	// It sits on the same mux as every other project route, which is the
	// guard: the frontend socket is 0600 and every request through it is
	// resolved to a credential by frontendCredentialAuth. Enumeration is
	// disclosure — the list of every mail account on this machine — so it
	// belongs behind the same admin boundary and nowhere near the remote
	// listener, whose dispatch table is ListTools and CallTool and gains
	// nothing here.
	// read, not configure: it discloses real values and changes nothing
	// (ADR-015 decision 1), even though the HTTP verb is POST.
	rr.Handle(control.ClassRead, "POST /api/mcps/{id}/enumerate", func(w http.ResponseWriter, r *http.Request) {
		var body enumerateRequest
		ev := logging.BeginEvent(r.Context(), "mcp.scope_field.enumerate").Set("mcp_id", r.PathValue("id"))
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			endEventHTTP(ev, http.StatusBadRequest, "invalid JSON: "+err.Error())
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		res := project.EnumerateScopeField(r.Context(), mcps.AllMcpSurfaces(), enum, r.PathValue("id"), body.Field, body.Values)
		endEventHTTP(ev.Set("field", body.Field), enumHTTPStatus(res.Status), "enumeration "+res.Status)
		writeJSON(w, enumHTTPStatus(res.Status), res)
	})

	// GET /api/mcps/{id}/tools — live tool list for the project picker.
	// 503 when no provider is wired (test contexts) or 404 when MCP is unknown
	// / not connected yet.
	rr.Handle(control.ClassRead, "GET /api/mcps/{id}/tools", func(w http.ResponseWriter, r *http.Request) {
		ev := logging.BeginEvent(r.Context(), "mcp.tools.list").Set("mcp_id", r.PathValue("id"))
		if tools == nil {
			endEventHTTP(ev, http.StatusServiceUnavailable, "tool list not available")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "tool list not available"})
			return
		}
		infos := tools.ToolInfos(r.PathValue("id"))
		if infos == nil {
			// Distinguish unknown from empty-but-connected for the UI hint.
			endEventHTTP(ev, http.StatusNotFound, "MCP not registered or not connected")
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "MCP not registered or not connected"})
			return
		}
		ev.Set("count", len(infos)).End(logging.OutcomeOK, "", nil)
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
func validatePermissionPolicy(p *config.PermissionPolicy) error {
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
