package sessions

import (
	"net/http"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

func historyError(w http.ResponseWriter, code, msg string) {
	server.WriteJSON(w, world.HistoryReasons[code], map[string]string{"error": msg, "code": code})
}

// resumeHistory opens a claude-code terminal for a history row and drops the
// row. The row's refuse code, resume value and resumeReason decide the answer
// a real relay would give.
func (s *svc) resumeHistory(w http.ResponseWriter, r *http.Request) {
	var body struct{ Cols, Rows int }
	if err := decode(w, r, 4096, &body); err != nil || body.Cols < 1 || body.Cols > 1000 || body.Rows < 1 || body.Rows > 1000 {
		historyError(w, "invalid_body", "cols and rows must be 1 to 1000")
		return
	}
	h := s.World.History()
	if h.Down {
		server.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "session history is not available", "code": "history_unavailable"})
		return
	}
	row, found := h.Find(r.PathValue("id"))
	if !found {
		historyError(w, "history_not_found", "history entry not found")
		return
	}
	code := row.Refuse
	switch {
	case row.Record == "launched" && row.Kind != "terminal":
		code = "use_session_resume"
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
	projectID := ""
	if row.ProjectID != nil {
		projectID = *row.ProjectID
	}
	// The row's folder is the world's truth: it is not held to the project's
	// path, which a spec may leave empty.
	t, ref := s.authorize(projectID, "", "", "terminal")
	t.dir = row.Directory
	if ref != nil {
		server.WriteJSON(w, ref.status, map[string]string{"error": ref.msg, "code": ref.code})
		return
	}
	tpl := s.findTemplate(t, "claude-code")
	if tpl == nil {
		server.WriteJSON(w, http.StatusForbidden, map[string]string{"error": templateUnavailable("claude-code"), "code": "template_unavailable"})
		return
	}
	x := s.newTerminal(tpl, terminalBody{Cols: body.Cols, Rows: body.Rows}, t, "")
	h.Remove(row.ID)
	server.WriteJSON(w, http.StatusCreated, map[string]any{"terminalId": x.id, "templateId": x.templateID, "name": x.name,
		"directory": x.directory, "host": nil, "supersedes": row.ID})
}
