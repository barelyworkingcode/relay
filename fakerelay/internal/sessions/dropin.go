package sessions

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
)

const dropInTemplate = "claude-code"

// dropIn takes a headless Claude session over for an echo terminal. The hold
// is set under the same lock as the checks, so two callers cannot both win.
func (s *svc) dropIn(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := s.Events.Begin(r.Context(), "session.drop_in").Set("session_id", id).Set("host", "console").Set("terminal_id", "")
	refuse := func(status int, code, msg string) {
		if status < 500 {
			ev.End("denied", code, errors.New(msg))
		} else {
			ev.End("error", code, errors.New(msg))
		}
		server.WriteJSON(w, status, map[string]string{"error": code, "message": msg})
	}
	var b struct{ Cols, Rows int }
	if body, err := readBody(w, r, 1<<20); err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &b)
	}
	if b.Cols <= 0 || b.Rows <= 0 {
		b.Cols, b.Rows = 120, 40
	}

	s.mu.Lock()
	x := s.sessions[id]
	var kind, projectID, dir, name string
	var host map[string]any
	if x != nil {
		kind, projectID, dir, host, name = x.kind, x.projectID, x.directory, x.host, x.askedName
	}
	s.mu.Unlock()
	hostName := "console"
	if host != nil {
		hostName, _ = host["name"].(string)
		ev.Set("host", hostName)
	}
	switch {
	case x == nil:
		refuse(http.StatusNotFound, "session_not_found", "no session "+id)
		return
	case kind != "claude":
		refuse(http.StatusConflict, "not_claude", "only Claude sessions can be taken over; this is a "+kind+" session")
		return
	}
	target, ref := s.authorize(projectID, "", "", "terminal")
	tpl := s.findTemplate(target, dropInTemplate)
	if ref == nil && tpl == nil {
		ref = &refusal{http.StatusForbidden, "template_unavailable", templateUnavailable(dropInTemplate)}
	}
	if ref != nil {
		refuse(ref.status, ref.code, ref.msg)
		return
	}
	target.dir = dir

	claudeID, ref := s.holdSession(r, id)
	if ref != nil {
		if ref.status == 0 {
			ev.End("error", "cancelled", r.Context().Err())
			return
		}
		refuse(ref.status, ref.code, ref.msg)
		return
	}
	if x.tracked() {
		s.setState(x, "running")
	}
	if name == "" {
		name = "session"
	}
	term := s.newTerminal(tpl, terminalBody{Name: name + " (drop-in)", Cols: b.Cols, Rows: b.Rows}, target, "")
	s.mu.Lock()
	term.dropIn = id
	s.mu.Unlock()
	ev.Set("terminal_id", term.id).End("ok", "", nil)

	tv := map[string]any{"terminalId": term.id, "templateId": tpl.ID, "name": term.name, "directory": term.directory, "host": nil}
	if target.host != nil {
		tv["host"] = map[string]any{"id": target.host["id"], "name": target.host["name"]}
	}
	resp := map[string]any{"sessionId": id, "claudeSessionId": claudeID, "terminal": tv}
	if host != nil {
		resp["host"] = hostName
	}
	server.WriteJSON(w, http.StatusCreated, resp)
}

// holdSession runs the state checks and, when they pass, holds the session
// and stops its agent. A turn in flight is waited out on the session's wake
// signal or the caller leaving, never on a timer; status 0 means the caller left.
func (s *svc) holdSession(r *http.Request, id string) (string, *refusal) {
	for {
		s.mu.Lock()
		x := s.sessions[id]
		var ref *refusal
		var wait chan struct{}
		claudeID := ""
		switch {
		case x == nil:
			ref = &refusal{http.StatusNotFound, "session_not_found", "no session " + id}
		case !x.headless:
			ref = &refusal{http.StatusConflict, "not_headless", "this session is not headless; continue it in eve"}
		case x.held:
			ref = &refusal{http.StatusConflict, "dropped_in", "a terminal already has this session; close it first"}
		case x.processing && x.tool != "":
			ref = &refusal{http.StatusConflict, "tool_running", "a tool is running (" + x.tool + "); wait for it to finish or stop the turn, then try again"}
		case x.processing:
			if x.changed == nil {
				x.changed = make(chan struct{})
			}
			wait = x.changed
		case x.claudeID == "":
			ref = &refusal{http.StatusConflict, "no_conversation", "the session has not run a turn yet; there is nothing to take over"}
		default:
			x.held, x.live, x.agent = true, false, nil
			claudeID = x.claudeID
		}
		s.mu.Unlock()
		if wait == nil {
			return claudeID, ref
		}
		select {
		case <-wait:
		case <-r.Context().Done():
			return "", &refusal{}
		}
	}
}
