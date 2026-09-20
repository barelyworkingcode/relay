package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/control"
)

func hostHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errHostNotFound):
		return http.StatusNotFound
	case errors.Is(err, errHostInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeHostError(w http.ResponseWriter, err error) {
	writeJSON(w, hostHTTPStatus(err), map[string]string{"error": err.Error()})
}

// RegisterHostRoutes wires the host HTTP endpoints exactly as
// docs/ssh-hosts.md's table specifies. A host rename or a fresh probe result
// changes what eve's project dialog and host chip show; the tray refreshes
// from the config queue's post-commit event.
func RegisterHostRoutes(rr *control.RouteRegistrar, ops *HostOps) {
	rr.Handle(control.ClassRead, "GET /api/hosts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, hostsToView(ops.List()))
	})

	rr.Handle(control.ClassRead, "GET /api/hosts/{id}", func(w http.ResponseWriter, r *http.Request) {
		h, err := ops.Get(r.PathValue("id"))
		if err != nil {
			writeHostError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, hostToView(h))
	})

	// configure: creating a host is a caller-chosen ssh destination, the
	// same class as every other operator-authored infrastructure record
	// (a service, an external MCP) rather than a tool-permission grant.
	rr.Handle(control.ClassConfigure, "POST /api/hosts", func(w http.ResponseWriter, r *http.Request) {
		var body hostFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		created, err := ops.Create(r.Context(), body)
		if err != nil {
			writeHostError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, hostToView(created))
	})

	rr.Handle(control.ClassConfigure, "PUT /api/hosts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body hostPatchFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		updated, found, err := ops.Update(r.Context(), id, body)
		if err != nil {
			writeHostError(w, err)
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "host not found"})
			return
		}
		writeJSON(w, http.StatusOK, hostToView(updated))
	})

	rr.Handle(control.ClassConfigure, "DELETE /api/hosts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		found, refs, err := ops.Remove(r.Context(), id)
		if err != nil {
			writeHostError(w, err)
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "host not found"})
			return
		}
		if len(refs) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":    "host is used by one or more projects",
				"projects": refs,
			})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// execute: a probe and a disconnect each run a real ssh process, the
	// same class every other "relay runs something now" route uses.
	rr.Handle(control.ClassConfigure, "POST /api/hosts/{id}/probe", func(w http.ResponseWriter, r *http.Request) {
		updated, found, err := ops.Probe(r.Context(), r.PathValue("id"))
		if err != nil {
			writeHostError(w, err)
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "host not found"})
			return
		}
		writeJSON(w, http.StatusOK, hostToView(updated))
	})

	rr.Handle(control.ClassConfigure, "POST /api/hosts/{id}/disconnect", func(w http.ResponseWriter, r *http.Request) {
		updated, found, err := ops.Disconnect(r.PathValue("id"))
		if err != nil {
			writeHostError(w, err)
			return
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "host not found"})
			return
		}
		writeJSON(w, http.StatusOK, hostToView(updated))
	})
}
