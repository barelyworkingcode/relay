package sessions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

const (
	scrollbackCap = 64 << 10
	logCap        = 1 << 20
	logHead       = 64 << 10
)

var terminalIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// termLog keeps every byte a terminal sent. Past logCap it keeps the first
// logHead bytes and the newest ones.
type termLog struct{ head, tail []byte }

func (l *termLog) add(b []byte) {
	if room := logHead - len(l.head); room > 0 {
		n := min(room, len(b))
		l.head = append(l.head, b[:n]...)
		b = b[n:]
	}
	l.tail = append(l.tail, b...)
	if over := len(l.tail) - (logCap - logHead); over > 0 {
		l.tail = l.tail[over:]
	}
}

func (l *termLog) bytes() []byte { return append(append([]byte{}, l.head...), l.tail...) }

// terminal is an echo terminal: input comes back as output with each CR
// becoming CR LF. It ends as a real one does: EOT on an empty line, or an
// `exit N` line.
type terminal struct {
	id, templateID, name, directory, origin string
	cols, rows                              int
	host                                    map[string]any
	scrollback, line                        []byte
	log                                     *termLog
	viewers                                 map[*conn]bool
	stopped                                 bool
	exitCode                                int
	readOnly, outside                       bool
	dropIn                                  string // the session this terminal holds
	unsandboxed                             bool   // the drop-in of a session that runs in no project
}

func (t *terminal) row() map[string]any {
	v := map[string]any{"id": t.id, "templateId": t.templateID, "name": t.name, "directory": t.directory, "state": "running"}
	if t.readOnly {
		v["readOnly"], v["outside"] = true, t.outside
	}
	if t.unsandboxed {
		v["unsandboxed"] = true
	}
	if t.stopped {
		v["state"] = "stopped"
		if t.exitCode != 0 {
			v["exitCode"] = t.exitCode
		}
	}
	return v
}

// feed runs input through the echo. It stops at the byte that ends the
// terminal; later bytes are dropped.
func (t *terminal) feed(data []byte) (echo []byte, code int, exited bool) {
	for _, b := range data {
		switch b {
		case 0x04:
			if len(t.line) == 0 {
				return echo, 0, true
			}
		case '\r':
			echo = append(echo, '\r', '\n')
			c, isExit := exitLine(t.line)
			t.line = t.line[:0]
			if isExit {
				return echo, c, true
			}
		default:
			t.line = append(t.line, b)
			echo = append(echo, b)
		}
	}
	return echo, 0, false
}

// exitLine reads `exit` or `exit N` with N from 0 to 255.
func exitLine(line []byte) (int, bool) {
	f := strings.Fields(string(line))
	switch {
	case len(f) == 1 && f[0] == "exit":
		return 0, true
	case len(f) == 2 && f[0] == "exit":
		n, err := strconv.Atoi(f[1])
		if err == nil && n >= 0 && n <= 255 && strings.Trim(f[1], "0123456789") == "" {
			return n, true
		}
	}
	return 0, false
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
	args := b.ExtraArgs
	if args == nil {
		args = []string{}
	}
	ev.Set("project_id", b.ProjectID).Set("template_id", b.TemplateID).Set("extra_args", args)
	t, ref := s.authorize(b.ProjectID, b.Directory, "", "terminal")
	if ref != nil {
		deny(ref.status, ref.code, ref.msg)
		return
	}
	ev.Set("directory", t.dir)
	tpl := s.findTemplate(t, b.TemplateID)
	switch {
	case tpl == nil:
		deny(http.StatusForbidden, "template_unavailable", templateUnavailable(b.TemplateID))
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

func templateUnavailable(id string) string {
	return `terminal template "` + id + `" is not available for this project`
}

// findTemplate resolves a terminal template for a launch target: the host's
// for a host project, else a console template the project allows.
// consoleTemplate is a console template by id, with no project allow-list: the
// drop-in of an unsandboxed session has no project to allow it.
func (s *svc) consoleTemplate(id string) *world.Template {
	var tpl *world.Template
	s.State.Read(func(m *state.Model) {
		for i := range m.Templates {
			if m.Templates[i].ID == id {
				x := m.Templates[i]
				tpl = &x
			}
		}
	})
	return tpl
}

func (s *svc) findTemplate(t launchTarget, id string) *world.Template {
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
			if list[i].ID == id && (t.project.HostID != "" || allowedTemplate(t.project.AllowedTemplates, id)) {
				x := list[i]
				tpl = &x
			}
		}
	})
	return tpl
}

func allowedTemplate(list []string, id string) bool { return allowed(list, id) }

func (s *svc) newTerminal(tpl *world.Template, b terminalBody, t launchTarget, origin string) *terminal {
	x := &terminal{id: newUUID(), templateID: tpl.ID, name: b.Name, directory: t.dir, origin: origin, cols: b.Cols, rows: b.Rows,
		host: t.host, viewers: map[*conn]bool{}, unsandboxed: t.unsandboxed}
	if x.name == "" {
		x.name = tpl.Name
	}
	if x.cols <= 0 || x.rows <= 0 {
		x.cols, x.rows = 80, 24
	}
	x.log = &termLog{}
	s.mu.Lock()
	s.terms[x.id] = x
	s.logs[x.id] = x.log
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

func (s *svc) terminalLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := s.Events.Begin(r.Context(), "terminal.log", events.Sessions()).Set("terminal_id", id)
	if !terminalIDRe.MatchString(id) {
		ev.End("error", "invalid", errors.New("malformed terminal id"))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	l := s.logs[id]
	var body []byte
	if l != nil {
		body = l.bytes()
	}
	s.mu.Unlock()
	if l == nil {
		ev.End("error", "not_found", errors.New("Not Found"))
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ev.End("ok", "", nil)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(body)
}

func (s *svc) closeTerminal(id string) bool {
	s.mu.Lock()
	t, found := s.terms[id]
	delete(s.terms, id)
	s.mu.Unlock()
	if found {
		s.broadcast(map[string]any{"type": "terminal_closed", "terminalId": id})
		s.releaseHold(t)
	}
	return found
}

// exitTerminal stops a terminal with its exit code. The event is written
// before the viewers hear of it.
func (s *svc) exitTerminal(t *terminal, code int) {
	s.mu.Lock()
	if t.stopped {
		s.mu.Unlock()
		return
	}
	t.stopped, t.exitCode = true, code
	viewers := viewerList(t)
	s.mu.Unlock()
	s.Events.Begin(context.Background(), "session.exited").Set("session_id", t.id).End("ok", "", nil)
	frame := map[string]any{"type": "terminal_exit", "terminalId": t.id, "exitCode": code}
	for _, v := range viewers {
		v.send(frame)
	}
	s.releaseHold(t)
}

// releaseHold hands a held session back when its drop-in terminal ends and
// resumes it, as relay does: the session is live and idle again with no
// message needed.
func (s *svc) releaseHold(t *terminal) {
	s.mu.Lock()
	sid := t.dropIn
	t.dropIn = ""
	x := s.sessions[sid]
	hostName := "console"
	if x != nil {
		x.held = false
		x.live, x.exit = true, nil
		if x.host != nil {
			if n, _ := x.host["name"].(string); n != "" {
				hostName = n
			}
		}
	}
	s.mu.Unlock()
	if x != nil {
		s.setState(x, "idle")
		s.Events.Begin(context.Background(), "session.hand_back").
			Set("session_id", sid).Set("terminal_id", t.id).Set("host", hostName).End("ok", "", nil)
	}
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
	if t.readOnly && (f.Type == "terminal_input" || f.Type == "terminal_resize") {
		s.mu.Unlock()
		c.send(map[string]any{"type": "error", "code": "terminal_read_only", "error": "terminal is read-only"})
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
		if t.stopped {
			s.mu.Unlock()
			return
		}
		echo, code, exited := t.feed(data)
		var viewers []*conn
		if len(echo) > 0 {
			t.scrollback = append(t.scrollback, echo...)
			if over := len(t.scrollback) - scrollbackCap; over > 0 {
				t.scrollback = t.scrollback[over:]
			}
			t.log.add(echo)
			viewers = viewerList(t)
		}
		s.mu.Unlock()
		if len(echo) > 0 {
			out := map[string]any{"type": "terminal_output", "terminalId": t.id, "data": base64.StdEncoding.EncodeToString(echo)}
			for _, v := range viewers {
				v.send(out)
			}
		}
		if exited {
			s.exitTerminal(t, code)
		}
	case "terminal_close":
		s.mu.Unlock()
		s.closeTerminal(f.TerminalID)
	default: // join_terminal, terminal_reconnect
		if !t.readOnly && f.Type == "terminal_reconnect" && f.Cols > 0 && f.Rows > 0 && (f.Cols != t.cols || f.Rows != t.rows) {
			t.cols, t.rows = f.Cols, f.Rows
		}
		if !t.stopped {
			t.viewers[c] = true
		}
		frame := map[string]any{"type": "terminal_joined", "terminalId": t.id, "templateId": t.templateID, "name": t.name,
			"directory": t.directory, "state": t.row()["state"], "cols": t.cols, "rows": t.rows,
			"scrollback": base64.StdEncoding.EncodeToString(t.scrollback), "host": nil}
		if t.readOnly {
			frame["readOnly"], frame["outside"] = true, t.outside
		}
		if t.host != nil {
			frame["host"] = map[string]any{"id": t.host["id"], "name": t.host["name"]}
		}
		stopped, code := t.stopped, t.exitCode
		s.mu.Unlock()
		c.send(frame)
		if stopped {
			c.send(map[string]any{"type": "terminal_exit", "terminalId": t.id, "exitCode": code})
		}
	}
}

func viewerList(t *terminal) []*conn {
	out := make([]*conn, 0, len(t.viewers))
	for c := range t.viewers {
		out = append(out, c)
	}
	return out
}

// createOutsideView models the production viewer door using the same terminal hub.
func (s *svc) createOutsideView(w http.ResponseWriter, r *http.Request) {
	var b struct {
		HostID      string `json:"hostId"`
		SessionName string `json:"sessionName"`
		Cols        int    `json:"cols"`
		Rows        int    `json:"rows"`
	}
	refuse := func(status int, code, msg string) {
		server.WriteJSON(w, status, map[string]string{"error": code, "message": msg})
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&b) != nil || decoder.Decode(new(any)) != io.EOF || b.SessionName == "" || strings.ContainsAny(b.SessionName, "|:") || strings.IndexFunc(b.SessionName, unicode.IsControl) >= 0 {
		refuse(400, "invalid_request", "an unambiguous session name is required")
		return
	}
	if b.Cols == 0 {
		b.Cols = 80
	}
	if b.Rows == 0 {
		b.Rows = 24
	}
	if b.Cols < 1 || b.Rows < 1 || b.Cols > 65535 || b.Rows > 65535 {
		refuse(400, "invalid_request", "terminal dimensions must be between 1 and 65535")
		return
	}
	var host *world.Host
	var directory string
	exists := false
	s.State.Read(func(m *state.Model) {
		for _, h := range m.Hosts {
			if h.ID == b.HostID && b.HostID != "" {
				copy := h
				host = &copy
			}
		}
		if host != nil {
			for _, p := range host.PersistentSessions {
				if p.Name == b.SessionName {
					exists = true
				}
			}
		}
		for _, o := range m.OutsideSessions {
			same := o.HostID == nil && b.HostID == "" || o.HostID != nil && *o.HostID == b.HostID
			if same && o.TmuxSession != nil && *o.TmuxSession == b.SessionName && o.Viewable && o.MachineStatus != "unreachable" {
				exists = true
				directory = o.Directory
			}
		}
	})
	if b.HostID != "" && (host == nil || !host.ShowAllTmuxSessions) {
		refuse(403, "outside_view_disabled", "outside viewing is disabled for this host")
		return
	}
	if host != nil {
		if status, ok := s.Hosts.HostStatuses()[host.ID]; ok && status.Status == "unreachable" {
			refuse(502, "host_unreachable", "host connection unavailable")
			return
		}
		path := host.TmuxPath
		if path == "" && host.Probe != nil {
			path = host.Probe.TmuxPath
		}
		if path == "" || host.TmuxViewSupported != nil && !*host.TmuxViewSupported {
			refuse(409, "tmux_view_unsupported", "tmux read-only viewing is unavailable")
			return
		}
	}
	if !exists {
		refuse(404, "tmux_session_not_found", "the exact tmux session was not found")
		return
	}
	var h map[string]any
	if host != nil {
		h = map[string]any{"id": host.ID, "name": host.Name}
	}
	t := &terminal{id: newUUID(), name: b.SessionName, directory: directory, cols: b.Cols, rows: b.Rows, host: h, readOnly: true, outside: true, viewers: map[*conn]bool{}, log: &termLog{}}
	s.mu.Lock()
	s.terms[t.id] = t
	s.logs[t.id] = t.log
	s.mu.Unlock()
	var hostView any
	if host != nil {
		hostView = map[string]any{"id": host.ID, "name": host.Name}
	}
	server.WriteJSON(w, 201, map[string]any{"terminalId": t.id, "templateId": "", "name": t.name, "directory": t.directory, "host": hostView, "readOnly": true, "outside": true})
}
