package api

import (
	"net/http"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// historyRoutes serves relay's session-history list from the world's
// sessionHistory rows. The resume door lives with the terminals it opens.
func (a *api) historyRoutes(r server.Registrar) error {
	if err := world.ValidateHistory(a.World.SessionHistory); err != nil {
		return err
	}
	h := a.World.History()
	r.Route(server.ClassExecute, "GET /api/session-history", func(w http.ResponseWriter, req *http.Request) {
		if h.Down {
			cosError(w, http.StatusServiceUnavailable, "history_unavailable", "session history is not available")
			return
		}
		rows, revision := h.Rows(req.URL.Query().Get("projectId"))
		for i := range rows {
			rows[i].Refuse = ""
		}
		ok(w, map[string]any{"ready": true, "revision": revision, "rows": rows,
			"machines": []map[string]any{{"hostId": nil, "machine": "Console", "status": "answered", "lastScanAt": rfc3339(a.now())}}})
	})
	return nil
}
