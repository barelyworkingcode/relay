package main

import (
	"encoding/json"
	"io"
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
	rr.Handle(control.ClassExecute, "POST /api/session-history/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		if ops == nil || ops.index == nil {
			writeChiefOfStaffError(w, http.StatusServiceUnavailable, "history_unavailable", "session history is not available")
			return
		}
		var body historyResumeBody
		data, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil || len(data) > 4096 || json.Unmarshal(data, &body) != nil {
			out := historyRefusal(http.StatusBadRequest, "invalid_body", "the body must be {\"cols\":1-1000,\"rows\":1-1000}")
			writeJSON(w, out.Status, out.Body)
			return
		}
		out := ops.Resume(r.Context(), resolveLaunchCaller(r, ops.store), r.PathValue("id"), body)
		writeJSON(w, out.Status, out.Body)
	})
}
