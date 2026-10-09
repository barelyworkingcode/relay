package sessions

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

const scrollbackCap = 64 << 10

// terminal is an echo terminal: input comes back as output with each CR
// becoming CR LF.
type terminal struct {
	id, templateID, name, directory, origin string
	cols, rows                              int
	host                                    map[string]any
	scrollback                              []byte
	viewers                                 map[*conn]bool
}

func (t *terminal) row() map[string]any {
	return map[string]any{"id": t.id, "templateId": t.templateID, "name": t.name, "directory": t.directory, "state": "running"}
}

func (s *svc) listTerminals(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.terms))
	for id := range s.terms {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := []map[string]any{}
	for _, id := range ids {
		t := s.terms[id]
		row := t.row()
		if t.host != nil {
			row["host"] = map[string]any{"id": t.host["id"], "name": t.host["name"]}
		}
		if t.origin != "" {
			row["origin"] = t.origin
		}
		rows = append(rows, row)
	}
	s.mu.Unlock()
	s.Events.Begin(r.Context(), "terminal.list").Set("count", len(rows)).End("ok", "", nil)
	ok(w, map[string]any{"terminals": rows})
}

type terminalBody struct {
	TemplateID     string   `json:"templateId"`
	Name           string   `json:"name"`
	Directory      string   `json:"directory"`
	ProjectID      string   `json:"projectId"`
	Cols           int      `json:"cols"`
	Rows           int      `json:"rows"`
	PersistSession string   `json:"persist_session"`
	ExtraArgs      []string `json:"extraArgs"`
}

func (s *svc) createTerminal(w http.ResponseWriter, r *http.Request) {
	ev := s.Events.Begin(r.Context(), "session.launch").Set("kind", "terminal")
	deny := func(status int, code, msg string) {
		ev.End("denied", code, errors.New(msg))
		server.WriteError(w, status, msg)
	}
	var b terminalBody
	if err := decode(w, r, 1<<20, &b); err != nil {
		ev.End("error", "invalid", err)
		if errors.Is(err, errTooLarge) {
			server.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		server.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	ev.Set("project_id", b.ProjectID)
	t, ref := s.authorize(b.ProjectID, b.Directory, "", "terminal")
	if ref != nil {
		deny(ref.status, ref.code, ref.msg)
		return
	}
	var tpl *world.Template
	s.State.Read(func(m *state.Model) {
		list := m.Templates
		if t.project.HostID != "" {
			for _, h := range m.Hosts {
				if h.ID == t.project.HostID {
					list = h.TerminalTemplates
				}
			}
		}
		for i := range list {
			if list[i].ID == b.TemplateID && (t.project.HostID != "" || allowedTemplate(t.project.AllowedTemplates, b.TemplateID)) {
				x := list[i]
				tpl = &x
			}
		}
	})
	switch {
	case tpl == nil:
		deny(http.StatusForbidden, "template_unavailable", `terminal template "`+b.TemplateID+`" is not available for this project`)
		return
	case b.PersistSession != "" && !(t.host != nil && string(tpl.Rest["persist"]) == "true"):
		deny(http.StatusForbidden, "persist_unavailable", "persist_session applies only to a persist template of a host project")
		return
	case len(b.ExtraArgs) > 0 && t.host != nil:
		ev.End("error", "invalid", errors.New("extraArgs"))
		server.WriteError(w, http.StatusBadRequest, "extraArgs are not supported for host terminals")
		return
	}
	size := 0
	for _, a := range b.ExtraArgs {
		size += len(a)
	}
	if len(b.ExtraArgs) > 64 || size > 65536 {
		ev.End("error", "invalid", errors.New("extraArgs"))
		server.WriteError(w, http.StatusBadRequest, "extraArgs exceed the cap of 64 entries and 65536 bytes")
		return
	}
	x := s.newTerminal(tpl, b, t, "")
	ev.Set("session_id", x.id).End("ok", "", nil)
	resp := map[string]any{"terminalId": x.id, "templateId": x.templateID, "name": x.name, "directory": x.directory, "host": nil}
	if t.host != nil {
		resp["host"] = map[string]any{"id": t.host["id"], "name": t.host["name"]}
	}
	server.WriteJSON(w, http.StatusCreated, resp)
}

func allowedTemplate(list []string, id string) bool { return allowed(list, id) }

func (s *svc) newTerminal(tpl *world.Template, b terminalBody, t launchTarget, origin string) *terminal {
	x := &terminal{id: newUUID(), templateID: tpl.ID, name: b.Name, directory: t.dir, origin: origin, cols: b.Cols, rows: b.Rows,
		host: t.host, viewers: map[*conn]bool{}}
	if x.name == "" {
		x.name = tpl.Name
	}
	if x.cols <= 0 || x.rows <= 0 {
		x.cols, x.rows = 80, 24
	}
	s.mu.Lock()
	s.terms[x.id] = x
	s.mu.Unlock()
	return x
}

func (s *svc) deleteTerminal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := s.Events.Begin(r.Context(), "terminal.delete", events.Sessions()).Set("terminal_id", id)
	if !s.closeTerminal(id) {
		ev.End("error", "not_found", errors.New("terminal not found"))
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ev.End("ok", "", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *svc) closeTerminal(id string) bool {
	s.mu.Lock()
	_, found := s.terms[id]
	delete(s.terms, id)
	s.mu.Unlock()
	if found {
		s.broadcast(map[string]any{"type": "terminal_closed", "terminalId": id})
	}
	return found
}

func (s *svc) terminalCommand(c *conn, f inFrame) {
	switch f.Type {
	case "terminal_create":
		c.send(errFrame("terminal_create over WebSocket is retired; POST /api/terminals instead"))
		return
	case "terminal_templates":
		c.send(errFrame("terminal_templates over WebSocket is retired; GET /api/terminal/templates instead"))
		return
	case "terminal_list":
		s.mu.Lock()
		rows := []map[string]any{}
		for _, t := range s.terms {
			rows = append(rows, t.row())
		}
		s.mu.Unlock()
		sort.Slice(rows, func(i, j int) bool { return rows[i]["id"].(string) < rows[j]["id"].(string) })
		c.send(map[string]any{"type": "terminal_list", "terminals": rows})
		return
	}
	if f.TerminalID == "" {
		return
	}
	s.mu.Lock()
	t := s.terms[f.TerminalID]
	if t == nil {
		s.mu.Unlock()
		if f.Type != "leave_terminal" {
			c.send(errFrame("terminal not found: " + f.TerminalID))
		}
		return
	}
	switch f.Type {
	case "leave_terminal":
		delete(t.viewers, c)
		s.mu.Unlock()
	case "terminal_resize":
		t.cols, t.rows = f.Cols, f.Rows
		s.mu.Unlock()
	case "terminal_input":
		data, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			s.mu.Unlock()
			c.send(errFrame("invalid base64 data"))
			return
		}
		echo := bytes.ReplaceAll(data, []byte("\r"), []byte("\r\n"))
		t.scrollback = append(t.scrollback, echo...)
		if over := len(t.scrollback) - scrollbackCap; over > 0 {
			t.scrollback = t.scrollback[over:]
		}
		viewers := viewerList(t)
		s.mu.Unlock()
		out := map[string]any{"type": "terminal_output", "terminalId": t.id, "data": base64.StdEncoding.EncodeToString(echo)}
		for _, v := range viewers {
			v.send(out)
		}
	case "terminal_close":
		s.mu.Unlock()
		s.closeTerminal(f.TerminalID)
	default: // join_terminal, terminal_reconnect
		if f.Type == "terminal_reconnect" && f.Cols > 0 && f.Rows > 0 && (f.Cols != t.cols || f.Rows != t.rows) {
			t.cols, t.rows = f.Cols, f.Rows
		}
		t.viewers[c] = true
		frame := map[string]any{"type": "terminal_joined", "terminalId": t.id, "templateId": t.templateID, "name": t.name,
			"directory": t.directory, "state": "running", "cols": t.cols, "rows": t.rows,
			"scrollback": base64.StdEncoding.EncodeToString(t.scrollback), "host": nil}
		if t.host != nil {
			frame["host"] = map[string]any{"id": t.host["id"], "name": t.host["name"]}
		}
		s.mu.Unlock()
		c.send(frame)
	}
}

func viewerList(t *terminal) []*conn {
	out := make([]*conn, 0, len(t.viewers))
	for c := range t.viewers {
		out = append(out, c)
	}
	return out
}
