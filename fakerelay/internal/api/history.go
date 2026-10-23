package api

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// historyState is the world's session-history rows, which a resume removes.
type historyState struct {
	mu       sync.Mutex
	rows     []world.HistoryRow
	revision uint64
	terminal int
	down     bool
}

// historyRoutes serves relay's session-history list and its resume door from
// the world's sessionHistory rows. A resume answers 201 with a terminal body
// and drops the row; it opens no terminal.
func (a *api) historyRoutes(r server.Registrar) error {
	if err := world.ValidateHistory(a.World.SessionHistory); err != nil {
		return err
	}
	h := &historyState{rows: append([]world.HistoryRow{}, a.World.SessionHistory...), revision: 1, down: a.World.SessionHistoryUnavailable}
	r.Route(server.ClassExecute, "GET /api/session-history", h.list)
	r.Route(server.ClassExecute, "POST /api/session-history/{id}/resume", h.resume)
	return nil
}

func (h *historyState) list(w http.ResponseWriter, r *http.Request) {
	if h.down {
		cosError(w, http.StatusServiceUnavailable, "history_unavailable", "session history is not available")
		return
	}
	project := r.URL.Query().Get("projectId")
	h.mu.Lock()
	defer h.mu.Unlock()
	rows := []world.HistoryRow{}
	for _, row := range h.rows {
		if project == "" || (row.ProjectID != nil && *row.ProjectID == project) {
			row.Refuse = ""
			rows = append(rows, row)
		}
	}
	ok(w, map[string]any{"ready": true, "revision": h.revision, "rows": rows,
		"machines": []map[string]any{{"hostId": nil, "machine": "Console", "status": "answered", "lastScanAt": nil}}})
}

func historyError(w http.ResponseWriter, code, msg string) {
	server.WriteJSON(w, world.HistoryReasons[code], map[string]string{"error": msg, "code": code})
}

func (h *historyState) resume(w http.ResponseWriter, r *http.Request) {
	if h.down {
		cosError(w, http.StatusServiceUnavailable, "history_unavailable", "session history is not available")
		return
	}
	var body struct{ Cols, Rows int }
	if err := decode(w, r, 4096, &body, false); err != nil || body.Cols < 1 || body.Cols > 1000 || body.Rows < 1 || body.Rows > 1000 {
		historyError(w, "invalid_body", "cols and rows must be 1 to 1000")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	at := -1
	for i, row := range h.rows {
		if row.ID == r.PathValue("id") {
			at = i
		}
	}
	if at < 0 {
		historyError(w, "history_not_found", "history entry not found")
		return
	}
	row := h.rows[at]
	code := row.Refuse
	switch {
	case code != "":
	case row.Resume == "session" || (row.Resume == "none" && row.ResumeReason == "kind"):
		code = "use_session_resume"
	case row.Resume == "none" && world.HistoryReasons[row.ResumeReason] != 0:
		code = row.ResumeReason
	case row.Resume == "none":
		code = "unavailable"
	}
	if code != "" {
		historyError(w, code, "the entry cannot be resumed: "+code)
		return
	}
	h.rows = append(h.rows[:at:at], h.rows[at+1:]...)
	h.revision++
	h.terminal++
	server.WriteJSON(w, http.StatusCreated, map[string]any{"terminalId": fmt.Sprintf("history-terminal-%d", h.terminal),
		"templateId": "claude-code", "name": "Claude", "directory": row.Directory, "host": nil, "supersedes": row.ID})
}
