package sessions

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

type conn struct {
	ws     *websocket.Conn
	wmu    sync.Mutex
	scoped bool
}

func (c *conn) send(v any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = c.ws.WriteJSON(v)
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// broadcast reaches every connection, joined or not.
func (s *svc) broadcast(v any) {
	s.mu.Lock()
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.send(v)
	}
}

func (s *svc) toViewers(x *session, v any) {
	s.mu.Lock()
	cs := make([]*conn, 0, len(x.viewers))
	for c := range x.viewers {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.send(v)
	}
}

type inFrame struct {
	Type         string `json:"type"`
	SessionID    string `json:"sessionId"`
	TerminalID   string `json:"terminalId"`
	Text         string `json:"text"`
	Name         string `json:"name"`
	Folder       string `json:"folder"`
	Mode         string `json:"mode"`
	PermissionID string `json:"permissionId"`
	Approved     bool   `json:"approved"`
	TraceID      string `json:"trace_id"`
	Data         string `json:"data"`
	Cols         int    `json:"cols"`
	Rows         int    `json:"rows"`
}

func errFrame(msg string) map[string]any { return map[string]any{"type": "error", "message": msg} }

func (s *svc) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &conn{ws: ws, scoped: server.CallerFrom(r.Context()).Scope != ""}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		for _, x := range s.sessions {
			delete(x.viewers, c)
		}
		for _, t := range s.terms {
			delete(t.viewers, c)
		}
		s.mu.Unlock()
		ws.Close()
		s.Events.Begin(r.Context(), "session.ws.close", events.Sessions()).End("ok", "", nil)
	}()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if c.scoped {
			c.wmu.Lock()
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "chief-of-staff scope is read-only"), time.Now().Add(time.Second))
			c.wmu.Unlock()
			return
		}
		var f inFrame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		s.dispatch(c, f)
	}
}

func (s *svc) dispatch(c *conn, f inFrame) {
	switch f.Type {
	case "join_session", "leave_session", "permission_response":
		if f.Type == "permission_response" {
			s.permissionResponse(c, f)
			return
		}
		if f.SessionID == "" {
			return
		}
		if f.Type == "join_session" {
			s.joinSession(c, f.SessionID)
			return
		}
		s.mu.Lock()
		if x := s.sessions[f.SessionID]; x != nil {
			delete(x.viewers, c)
		}
		s.mu.Unlock()
	case "send_message", "stop_generation", "end_session", "delete_session", "rename_session", "clear_session", "set_session_folder", "set_permission_mode":
		if f.SessionID == "" {
			c.send(errFrame("sessionId required"))
			return
		}
		s.sessionCommand(c, f)
	case "join_terminal", "terminal_reconnect", "leave_terminal", "terminal_input", "terminal_resize", "terminal_close", "terminal_list", "terminal_create", "terminal_templates":
		s.terminalCommand(c, f)
	}
}

func (s *svc) joinSession(c *conn, id string) {
	s.mu.Lock()
	x := s.sessions[id]
	if x == nil {
		s.mu.Unlock()
		c.send(errFrame("session not found: " + id))
		return
	}
	x.viewers[c] = true
	hist := append([]map[string]any{}, x.history...)
	stats := map[string]any{}
	for k, v := range x.stats {
		stats[k] = v
	}
	frame := map[string]any{"type": "session_joined", "sessionId": x.id, "projectId": x.projectID, "directory": x.directory,
		"model": x.model, "name": x.name, "folder": x.folder, "history": hist, "stats": stats, "headless": x.headless,
		"protocolVersion": "2", "host": nil, "live": x.live}
	if x.host != nil {
		frame["host"] = x.host
	}
	s.mu.Unlock()
	c.send(frame)
}

func (s *svc) permissionResponse(c *conn, f inFrame) {
	s.mu.Lock()
	sid, found := s.perms[f.PermissionID]
	x := s.sessions[sid]
	if !found || x == nil {
		s.mu.Unlock()
		return
	}
	if !x.viewers[c] {
		s.mu.Unlock()
		c.send(errFrame("permission response refused: this connection has not joined session " + sid))
		return
	}
	delete(s.perms, f.PermissionID)
	agent := x.agent
	s.mu.Unlock()
	s.setState(x, "running")
	if agent != nil {
		agent.Answer(f.PermissionID, f.Approved)
	}
}

func (s *svc) sessionCommand(c *conn, f inFrame) {
	if f.Type == "send_message" {
		x, ref := s.beginTurn(f.SessionID, f.Text, "")
		switch {
		case ref == nil:
			go s.runTurn(x, f.Text, "", f.TraceID)
		case ref.code == "resume_required":
			c.send(map[string]any{"type": "error", "sessionId": f.SessionID, "code": ref.code, "message": ref.msg})
		default:
			c.send(errFrame(ref.msg))
		}
		return
	}
	if f.Type == "delete_session" {
		s.removeSession(events.WithTrace(context.Background(), ""), f.SessionID)
		return
	}
	s.mu.Lock()
	x := s.sessions[f.SessionID]
	if x == nil {
		s.mu.Unlock()
		c.send(errFrame("session: not found"))
		return
	}
	var reply map[string]any
	var more []map[string]any
	switch f.Type {
	case "stop_generation":
		if x.cancel != nil && x.processing {
			x.cancel()
		}
	case "end_session":
		x.live, x.agent = false, nil
		if x.cancel != nil {
			x.cancel()
		}
	case "rename_session":
		x.name = f.Name
		reply = map[string]any{"type": "session_renamed", "sessionId": x.id, "name": f.Name}
	case "clear_session":
		x.history, x.stats = nil, newStats()
		reply = map[string]any{"type": "clear_messages", "sessionId": x.id}
		more = []map[string]any{{"type": "stats_update", "sessionId": x.id, "stats": newStats()}, {"type": "system_message", "sessionId": x.id, "text": "Conversation cleared"}}
	case "set_session_folder":
		x.folder = f.Folder
		reply = map[string]any{"type": "session_folder_changed", "sessionId": x.id, "folder": f.Folder}
	case "set_permission_mode":
		if x.kind == "claude" {
			x.permMode = f.Mode
			reply = map[string]any{"type": "mode_changed", "sessionId": x.id, "mode": f.Mode}
		}
	}
	s.mu.Unlock()
	if f.Type == "end_session" {
		s.setState(x, "ended")
	}
	if reply != nil {
		s.toViewers(x, reply)
	}
	for _, m := range more {
		s.toViewers(x, m)
	}
}
