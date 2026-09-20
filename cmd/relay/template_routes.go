package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// RegisterTemplateRoutes serves relay's own terminal-template config
// (Settings.TerminalTemplates; nothing is computed in code). The JSON shape
// matches relayLLM's former GET /api/terminal/templates field-for-field,
// minus useRelayToken (dropped, not ported). Writes go through TemplateOps,
// the same core the Settings window uses.
func RegisterTemplateRoutes(rr *control.RouteRegistrar, store config.SettingsStore, ops *TemplateOps) {

	rr.Handle(classFor("GET", "/api/terminal/templates"), "GET /api/terminal/templates", func(w http.ResponseWriter, r *http.Request) {
		// A project is what permits a template (Project.AllowedTemplates), so
		// a list asked for with no project, or an unknown one, is empty. The
		// Settings window lists everything over IPC instead.
		settings := config.FreshSettings(store)
		proj, _ := config.FindProjectByID(settings, r.URL.Query().Get("project"))
		writeJSON(w, http.StatusOK, config.EffectiveTerminalTemplatesForProject(settings, proj))
	})

	rr.Handle(classFor("GET", "/api/terminal/templates/{id}"), "GET /api/terminal/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		tmpl, ok := config.GetTerminalTemplate(config.FreshSettings(store), r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "template not found"})
			return
		}
		writeJSON(w, http.StatusOK, tmpl)
	})

	save := func(create bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var t config.TerminalTemplate
			if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
				return
			}
			status := http.StatusCreated
			do := ops.Create
			if !create {
				t.ID, status, do = r.PathValue("id"), http.StatusOK, ops.Update
			}
			if err := do(r.Context(), t); err != nil {
				writeTemplateError(w, err)
				return
			}
			writeJSON(w, status, t)
		}
	}
	rr.Handle(classFor("POST", "/api/terminal/templates"), "POST /api/terminal/templates", save(true))
	rr.Handle(classFor("PUT", "/api/terminal/templates/{id}"), "PUT /api/terminal/templates/{id}", save(false))
	rr.Handle(classFor("DELETE", "/api/terminal/templates/{id}"), "DELETE /api/terminal/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := ops.Remove(r.Context(), r.PathValue("id")); err != nil {
			writeTemplateError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func writeTemplateError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, errTemplateInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, errTemplateNotFound):
		status = http.StatusNotFound
	case errors.Is(err, errTemplateExists):
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// classFor asks sessionRouteClasses (api_credential.go) rather than picking
// a class inline — that table is C1's single source of truth for a
// session-host route's class, built by R-S1 before this unit's routes
// existed specifically so a later unit would defer to it. A miss is a
// programming error this unit's own tests would catch, not a runtime state
// to degrade gracefully from.
func classFor(method, path string) control.CapabilityClass {
	class, ok := sessionRouteClass(method, path)
	if !ok {
		panic(fmt.Sprintf("template_routes.go: sessionRouteClasses has no entry for %q %q", method, path))
	}
	return class
}
