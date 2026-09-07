package main

import (
	"encoding/json"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/control"
)

// RegisterEveEnrolmentRoutes wires the two HTTP endpoints
// docs/eve-passkey-enrolment.md's table specifies — eve's own door onto
// EveEnrolmentOps, unlike every other frontend route registered here.
// Reachable with the legacy frontend credential's existing read+configure
// grant, exactly as every other relay-served route is: opening the window
// stays reachable only from the tray or `relay eve enrol` (both host-only,
// gated doors), so this pair carries no path onto Open at all.
func RegisterEveEnrolmentRoutes(rr *control.RouteRegistrar, ops *EveEnrolmentOps) {
	rr.Handle(control.ClassRead, "GET /api/eve/passkey-enrolment", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ops.Status())
	})

	rr.Handle(control.ClassConfigure, "POST /api/eve/passkey-enrolment/consume", func(w http.ResponseWriter, r *http.Request) {
		var claim eveEnrolmentClaim
		if err := json.NewDecoder(r.Body).Decode(&claim); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		view, err := ops.Consume(r.Context(), claim)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
}
