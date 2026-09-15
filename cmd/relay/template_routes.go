package main

import (
	"fmt"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// RegisterTemplateRoutes serves relay's own terminal-template config.
// Read-only — template mutation has no route in this unit: the built-ins
// are seeded in code (internal/config/templates.go), and the only override
// points, Settings.TerminalTemplates and a project's ShellTemplates, have
// no editor yet. The JSON shape matches relayLLM's former
// GET /api/terminal/templates field-for-field, minus useRelayToken
// (dropped, not ported).
func RegisterTemplateRoutes(rr *control.RouteRegistrar, store config.SettingsStore) {
	rr.Handle(classFor("GET", "/api/terminal/templates"), "GET /api/terminal/templates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, config.EffectiveTerminalTemplates(store.Get()))
	})

	rr.Handle(classFor("GET", "/api/terminal/templates/{id}"), "GET /api/terminal/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		tmpl, ok := config.GetTerminalTemplate(store.Get(), r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "template not found"})
			return
		}
		writeJSON(w, http.StatusOK, tmpl)
	})
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
