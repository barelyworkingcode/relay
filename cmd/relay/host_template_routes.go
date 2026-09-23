package main

import (
	"encoding/json"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// RegisterHostTemplateRoutes serves a host's own terminal templates
// (docs/ssh-hosts.md). The shapes and status codes match the console
// template routes (template_routes.go), plus 404 for an unknown host. Writes
// go through HostTemplateOps, the same core the Hosts tab uses.
func RegisterHostTemplateRoutes(rr *control.RouteRegistrar, ops *HostTemplateOps) {
	rr.Handle(control.ClassRead, "GET /api/hosts/{id}/templates", func(w http.ResponseWriter, r *http.Request) {
		ts, err := ops.List(r.PathValue("id"))
		if err != nil {
			writeTemplateError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ts)
	})

	// configure: the same class as the console template writes and the host
	// record itself.
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
				t.ID, status, do = r.PathValue("tid"), http.StatusOK, ops.Update
			}
			if err := do(r.Context(), r.PathValue("id"), t); err != nil {
				writeTemplateError(w, err)
				return
			}
			writeJSON(w, status, t)
		}
	}
	rr.Handle(control.ClassConfigure, "POST /api/hosts/{id}/templates", save(true))
	rr.Handle(control.ClassConfigure, "PUT /api/hosts/{id}/templates/{tid}", save(false))
	rr.Handle(control.ClassConfigure, "DELETE /api/hosts/{id}/templates/{tid}", func(w http.ResponseWriter, r *http.Request) {
		if err := ops.Remove(r.Context(), r.PathValue("id"), r.PathValue("tid")); err != nil {
			writeTemplateError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
