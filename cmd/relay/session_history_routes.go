package main

import (
	"net/http"

	"github.com/barelyworkingcode/relay/internal/control"
)

// RegisterSessionHistoryRoutes registers the history list. The class is execute:
// the list names every console transcript's folder and title, so it is absent
// from the TCP mux and from the Chief of Staff scope.
func RegisterSessionHistoryRoutes(rr *control.RouteRegistrar, ops *historyOps) {
	rr.Handle(control.ClassExecute, "GET /api/session-history", func(w http.ResponseWriter, r *http.Request) {
		if ops == nil || ops.index == nil {
			writeChiefOfStaffError(w, http.StatusServiceUnavailable, "history_unavailable", "session history is not available")
			return
		}
		writeJSON(w, http.StatusOK, ops.List(r.URL.Query().Get("projectId")))
	})
}
