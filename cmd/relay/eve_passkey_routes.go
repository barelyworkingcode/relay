package main

import (
	"encoding/json"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/control"
)

// RegisterEvePasskeyRoutes wires eve's two doors onto the mirror
// (docs/eve-passkey-enrolment.md's second table): eve reports its list and
// learns of new revocations in the same round-trip, and separately polls
// for pending ones. Registered alongside RegisterEveEnrolmentRoutes, and
// reachable the same way -- eve's legacy frontend credential already grants
// read and configure.
func RegisterEvePasskeyRoutes(rr *control.RouteRegistrar, ops *EvePasskeyOps) {
	rr.Handle(control.ClassConfigure, "PUT /api/eve/passkeys", func(w http.ResponseWriter, r *http.Request) {
		var req evePasskeysReportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if err := ops.Report(req.Passkeys); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, evePasskeyRevocationsView{Revocations: ops.Revocations()})
	})

	rr.Handle(control.ClassRead, "GET /api/eve/passkeys/revocations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, evePasskeyRevocationsView{Revocations: ops.Revocations()})
	})
}
