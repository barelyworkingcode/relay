package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/files"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

func (a *api) hostRoutes(r server.Registrar) {
	r.Route(server.ClassRead, "GET /api/hosts", a.listHosts)
	r.Route(server.ClassRead, "GET /api/hosts/{id}", a.getHost)
	r.Route(server.ClassConfigure, "POST /api/hosts", a.createHost)
	r.Route(server.ClassConfigure, "PUT /api/hosts/{id}", a.updateHost)
	r.Route(server.ClassConfigure, "DELETE /api/hosts/{id}", a.deleteHost)
	r.Route(server.ClassConfigure, "POST /api/hosts/{id}/probe", a.probeHost)
	r.Route(server.ClassConfigure, "POST /api/hosts/{id}/disconnect", a.disconnectHost)
	r.Route(server.ClassRead, "GET /api/hosts/{id}/templates", a.listHostTemplates)
	r.Route(server.ClassConfigure, "POST /api/hosts/{id}/templates", a.createHostTemplate)
	r.Route(server.ClassConfigure, "PUT /api/hosts/{id}/templates/{tid}", a.updateHostTemplate)
	r.Route(server.ClassConfigure, "DELETE /api/hosts/{id}/templates/{tid}", a.deleteHostTemplate)
	r.Route(server.ClassRead, "GET /api/projects/{id}/persistent-sessions", a.listPersistent)
	r.Route(server.ClassConfigure, "DELETE /api/projects/{id}/persistent-sessions/{name}", a.killPersistent)
}

func (a *api) agentStatus(h world.Host) string {
	if st, found := files.HostStatuses()[h.ID]; found && st.Status != "" {
		return st.Status
	}
	return h.Agent
}

func (a *api) hostView(h world.Host) map[string]any {
	status := "unknown"
	switch {
	case a.agentStatus(h) == "connected":
		status = "connected"
	case h.Probe != nil && h.Probe.OK:
		status = "idle"
	case h.Probe != nil:
		status = "unreachable"
	}
	ssh := []string{"ssh", "-o", "BatchMode=yes"}
	if h.Port != 0 {
		ssh = append(ssh, "-p", strconv.Itoa(h.Port))
	}
	if h.IdentityFile != "" {
		ssh = append(ssh, "-i", h.IdentityFile)
	}
	tpl := h.TerminalTemplates
	if tpl == nil {
		tpl = []world.Template{}
	}
	a.mu.Lock()
	at, hasAt := a.probeAt[h.ID]
	created := a.extra["host:"+h.ID]["created_at"]
	a.mu.Unlock()
	if !hasAt {
		at = a.started
	}
	v := map[string]any{"id": h.ID, "name": h.Name, "target": h.Target, "created_at": rfc3339(a.started),
		"status": status, "ssh_argv": append(ssh, h.Target), "terminal_templates": tpl}
	if len(created) > 0 {
		v["created_at"] = strings.Trim(string(created), `"`)
	}
	if h.Port != 0 {
		v["port"] = h.Port
	}
	if h.IdentityFile != "" {
		v["identity_file"] = h.IdentityFile
	}
	if h.TmuxPath != "" {
		v["tmux_path"] = h.TmuxPath
	}
	if p := h.Probe; p != nil {
		probe := map[string]any{"at": rfc3339(at), "ok": p.OK}
		for k, s := range map[string]string{"os": p.OS, "arch": p.Arch, "home": p.Home, "shell": p.Shell,
			"node_path": p.NodePath, "claude_path": p.ClaudePath, "tmux_path": p.TmuxPath, "error": p.Error} {
			if s != "" {
				probe[k] = s
			}
		}
		v["probe"] = probe
	}
	return v
}

func (a *api) hostByID(w http.ResponseWriter, ev *events.Event, id string) (world.Host, bool) {
	var h world.Host
	var found bool
	a.State.Read(func(m *state.Model) {
		if p, f := findHost(m, id); f {
			h, found = *p, true
		}
	})
	if !found {
		ev.End("error", "not_found", errors.New("host not found"))
		server.WriteError(w, http.StatusNotFound, "host not found")
	}
	return h, found
}

func (a *api) listHosts(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	a.State.Read(func(m *state.Model) {
		for _, h := range m.Hosts {
			out = append(out, a.hostView(h))
		}
	})
	a.Events.Begin(r.Context(), "host.list").Set("count", len(out)).End("ok", "", nil)
	ok(w, out)
}

func (a *api) getHost(w http.ResponseWriter, r *http.Request) {
	ev := a.Events.Begin(r.Context(), "host.get").Set("host_id", r.PathValue("id"))
	if h, found := a.hostByID(w, ev, r.PathValue("id")); found {
		ev.End("ok", "", nil)
		ok(w, a.hostView(h))
	}
}

type hostBody struct {
	Name         *string `json:"name"`
	Target       *string `json:"target"`
	Port         *int    `json:"port"`
	IdentityFile *string `json:"identity_file"`
	TmuxPath     *string `json:"tmux_path"`
}

func (a *api) createHost(w http.ResponseWriter, r *http.Request) {
	ev := a.Events.Begin(r.Context(), "host.create")
	var b hostBody
	if err := decode(w, r, 1<<20, &b, false); err != nil {
		ev.End("error", "invalid", err)
		writeBodyErr(w, err)
		return
	}
	switch {
	case b.Name == nil || strings.TrimSpace(*b.Name) == "":
		ev.End("error", "invalid", errors.New("host name is required"))
		server.WriteError(w, http.StatusBadRequest, "host name is required")
		return
	case b.Target == nil || strings.TrimSpace(*b.Target) == "":
		ev.End("error", "invalid", errors.New("host target is required"))
		server.WriteError(w, http.StatusBadRequest, "host target is required")
		return
	}
	h := world.Host{ID: "h_" + events.NewID()[:8], Name: *b.Name, Target: *b.Target, Agent: "none"}
	h.Root = filepath.Join(a.Dir, "hosts", h.ID)
	applyHostBody(&h, &b)
	h.Probe = defaultProbe(h)
	ev.Set("host_id", h.ID)
	if err := os.MkdirAll(h.Root, 0o755); err != nil {
		ev.End("error", "internal", err)
		server.WriteError(w, http.StatusInternalServerError, "host folder could not be created")
		return
	}
	_ = a.State.Write(func(m *state.Model) error { m.Hosts = append(m.Hosts, h); return nil })
	a.mu.Lock()
	a.probeAt[h.ID] = a.now()
	a.extra["host:"+h.ID] = map[string]json.RawMessage{"created_at": json.RawMessage(strconv.Quote(rfc3339(a.now())))}
	a.mu.Unlock()
	ev.End("ok", "", nil)
	server.WriteJSON(w, http.StatusCreated, a.hostView(h))
}

func defaultProbe(h world.Host) *world.Probe {
	p := &world.Probe{OK: true, OS: "Linux", Arch: "x86_64", Home: "/home/fake", Shell: "/bin/bash", TmuxPath: h.TmuxPath}
	return p
}

func applyHostBody(h *world.Host, b *hostBody) {
	if b.Name != nil {
		h.Name = *b.Name
	}
	if b.Target != nil {
		h.Target = *b.Target
	}
	if b.Port != nil {
		h.Port = *b.Port
	}
	if b.IdentityFile != nil {
		h.IdentityFile = *b.IdentityFile
	}
	if b.TmuxPath != nil {
		h.TmuxPath = *b.TmuxPath
	}
}

func (a *api) updateHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), "host.update").Set("host_id", id)
	var b hostBody
	if err := decode(w, r, 1<<20, &b, false); err != nil {
		ev.End("error", "invalid", err)
		writeBodyErr(w, err)
		return
	}
	if (b.Name != nil && strings.TrimSpace(*b.Name) == "") || (b.Target != nil && strings.TrimSpace(*b.Target) == "") {
		ev.End("error", "invalid", errors.New("name and target must not be empty"))
		server.WriteError(w, http.StatusBadRequest, "host name and target must not be empty")
		return
	}
	var out world.Host
	found := false
	_ = a.State.Write(func(m *state.Model) error {
		if h, f := findHost(m, id); f {
			applyHostBody(h, &b)
			out, found = *h, true
		}
		return nil
	})
	if !found {
		ev.End("error", "not_found", errors.New("host not found"))
		server.WriteError(w, http.StatusNotFound, "host not found")
		return
	}
	ev.End("ok", "", nil)
	ok(w, a.hostView(out))
}

func (a *api) deleteHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), "host.remove").Set("host_id", id)
	var users []string
	found := false
	_ = a.State.Write(func(m *state.Model) error {
		for _, p := range m.Projects {
			if p.HostID == id {
				users = append(users, p.Name)
			}
		}
		if len(users) > 0 {
			return errors.New("host is used")
		}
		for i := range m.Hosts {
			if m.Hosts[i].ID == id {
				m.Hosts = append(m.Hosts[:i], m.Hosts[i+1:]...)
				found = true
			}
		}
		return nil
	})
	switch {
	case len(users) > 0:
		ev.End("error", "conflict", errors.New("host is used by projects"))
		server.WriteJSON(w, http.StatusConflict, map[string]any{"error": "host is used by one or more projects", "projects": users})
	case !found:
		ev.End("error", "not_found", errors.New("host not found"))
		server.WriteError(w, http.StatusNotFound, "host not found")
	default:
		ev.End("ok", "", nil)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *api) probeHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), "host.probe").Set("host_id", id)
	var out world.Host
	found := false
	_ = a.State.Write(func(m *state.Model) error {
		if h, f := findHost(m, id); f {
			if h.Probe == nil {
				h.Probe = defaultProbe(*h)
			}
			out, found = *h, true
		}
		return nil
	})
	if !found {
		ev.End("error", "not_found", errors.New("host not found"))
		server.WriteError(w, http.StatusNotFound, "host not found")
		return
	}
	a.mu.Lock()
	a.probeAt[id] = a.now()
	a.mu.Unlock()
	ev.End("ok", "", nil)
	ok(w, a.hostView(out))
}

func (a *api) disconnectHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), "host.disconnect").Set("host_id", id)
	h, found := a.hostByID(w, ev, id)
	if !found {
		return
	}
	files.SetHostStatus(id, "unreachable", "disconnected")
	h.Agent = "unreachable"
	ev.End("ok", "", nil)
	ok(w, a.hostView(h))
}

func (a *api) listHostTemplates(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), "host_template.list").Set("host_id", id)
	h, found := a.hostByID(w, ev, id)
	if !found {
		return
	}
	out := sortedIDs(h.TerminalTemplates, func(t world.Template) string { return t.ID })
	if out == nil {
		out = []world.Template{}
	}
	ev.Set("count", len(out)).End("ok", "", nil)
	ok(w, out)
}

// hostTemplateWrite runs create, update or delete on a host's template list.
// fn returns the HTTP error to answer, or nil.
func (a *api) hostTemplateWrite(w http.ResponseWriter, r *http.Request, key string, tid string, status int, fn func(h *world.Host) (*world.Template, *httpErr)) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), key).Set("host_id", id)
	var out *world.Template
	var herr *httpErr
	_ = a.State.Write(func(m *state.Model) error {
		h, f := findHost(m, id)
		if !f {
			herr = &httpErr{http.StatusNotFound, "host not found"}
			return herr
		}
		out, herr = fn(h)
		if herr != nil {
			return herr
		}
		return nil
	})
	if herr != nil {
		reason := map[int]string{http.StatusBadRequest: "invalid", http.StatusNotFound: "not_found", http.StatusConflict: "conflict"}[herr.status]
		ev.End("error", reason, herr)
		server.WriteError(w, herr.status, herr.msg)
		return
	}
	if out != nil {
		tid = out.ID
	}
	ev.Set("template_id", tid).End("ok", "", nil)
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	server.WriteJSON(w, status, out)
}

func (a *api) readTemplate(w http.ResponseWriter, r *http.Request, pathID string) (world.Template, *httpErr) {
	var t world.Template
	if err := decode(w, r, 1<<20, &t, false); err != nil {
		if errors.Is(err, errTooLarge) {
			return t, &httpErr{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return t, &httpErr{http.StatusBadRequest, "invalid JSON: " + err.Error()}
	}
	if pathID != "" {
		t.ID = pathID
	}
	return t, validTemplate(t)
}

func (a *api) createHostTemplate(w http.ResponseWriter, r *http.Request) {
	t, herr := a.readTemplate(w, r, "")
	a.hostTemplateWrite(w, r, "host_template.create", t.ID, http.StatusCreated, func(h *world.Host) (*world.Template, *httpErr) {
		if herr != nil {
			return nil, herr
		}
		for _, x := range h.TerminalTemplates {
			if x.ID == t.ID {
				return nil, &httpErr{http.StatusConflict, `template "` + t.ID + `" already exists`}
			}
		}
		h.TerminalTemplates = append(h.TerminalTemplates, t)
		return &t, nil
	})
}

func (a *api) updateHostTemplate(w http.ResponseWriter, r *http.Request) {
	tid := r.PathValue("tid")
	t, herr := a.readTemplate(w, r, tid)
	a.hostTemplateWrite(w, r, "host_template.update", tid, http.StatusOK, func(h *world.Host) (*world.Template, *httpErr) {
		for i := range h.TerminalTemplates {
			if h.TerminalTemplates[i].ID == tid {
				if herr != nil {
					return nil, herr
				}
				h.TerminalTemplates[i] = t
				return &t, nil
			}
		}
		return nil, &httpErr{http.StatusNotFound, `template "` + tid + `" not found`}
	})
}

func (a *api) deleteHostTemplate(w http.ResponseWriter, r *http.Request) {
	tid := r.PathValue("tid")
	a.hostTemplateWrite(w, r, "host_template.remove", tid, http.StatusNoContent, func(h *world.Host) (*world.Template, *httpErr) {
		for i := range h.TerminalTemplates {
			if h.TerminalTemplates[i].ID == tid {
				h.TerminalTemplates = append(h.TerminalTemplates[:i], h.TerminalTemplates[i+1:]...)
				return nil, nil
			}
		}
		return nil, &httpErr{http.StatusNotFound, `template "` + tid + `" not found`}
	})
}

var persistName = regexp.MustCompile(`^relay-[A-Za-z0-9_]+-[A-Za-z0-9._-]+-[0-9]+$`)

// persistentHost resolves the host project behind a persistent-sessions route.
func (a *api) persistentHost(w http.ResponseWriter, r *http.Request, ev *events.Event) (world.Host, bool) {
	var h world.Host
	code, msg := 0, ""
	a.State.Read(func(m *state.Model) {
		p, found := findProject(m, r.PathValue("id"))
		if !found || p.HostID == "" {
			code, msg = http.StatusNotFound, "project not found"
			return
		}
		hp, _ := findHost(m, p.HostID)
		if hp == nil {
			code, msg = http.StatusNotFound, "project not found"
			return
		}
		h = *hp
		switch {
		case h.TmuxPath == "" && (h.Probe == nil || h.Probe.TmuxPath == ""):
			code, msg = http.StatusConflict, "host has no tmux"
		case a.agentStatus(h) == "unreachable" || (h.Probe != nil && !h.Probe.OK):
			code, msg = http.StatusBadGateway, "host unreachable"
		}
	})
	if code != 0 {
		reason := map[int]string{http.StatusNotFound: "not_found", http.StatusConflict: "conflict", http.StatusBadGateway: "upstream"}[code]
		ev.End("error", reason, errors.New(msg))
		server.WriteError(w, code, msg)
		return h, false
	}
	return h, true
}

func (a *api) listPersistent(w http.ResponseWriter, r *http.Request) {
	ev := a.Events.Begin(r.Context(), "session.persistent.list").Set("project_id", r.PathValue("id"))
	h, found := a.persistentHost(w, r, ev)
	if !found {
		return
	}
	rows := make([]map[string]any, 0, len(h.PersistentSessions))
	list := append([]world.PersistentSession(nil), h.PersistentSessions...)
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	for _, s := range list {
		tid, n := "", 0
		if rest, f := strings.CutPrefix(s.Name, "relay-"); f {
			if i := strings.LastIndex(rest, "-"); i > 0 {
				n, _ = strconv.Atoi(rest[i+1:])
				rest = rest[:i]
			}
			if i := strings.Index(rest, "-"); i >= 0 {
				tid = rest[i+1:]
			}
		}
		rows = append(rows, map[string]any{"name": s.Name, "template_id": tid, "n": n, "created": s.Created,
			"attached": s.Attached, "attached_here": false})
	}
	ev.End("ok", "", nil)
	ok(w, rows)
}

func (a *api) killPersistent(w http.ResponseWriter, r *http.Request) {
	ev := a.Events.Begin(r.Context(), "session.persistent.kill").Set("project_id", r.PathValue("id"))
	name := r.PathValue("name")
	if !persistName.MatchString(name) {
		ev.End("error", "invalid", errors.New("invalid persistent session name"))
		server.WriteError(w, http.StatusBadRequest, "invalid persistent session name")
		return
	}
	h, found := a.persistentHost(w, r, ev)
	if !found {
		return
	}
	killed := false
	_ = a.State.Write(func(m *state.Model) error {
		hp, _ := findHost(m, h.ID)
		for i, s := range hp.PersistentSessions {
			if s.Name == name {
				hp.PersistentSessions = append(hp.PersistentSessions[:i], hp.PersistentSessions[i+1:]...)
				killed = true
				break
			}
		}
		return nil
	})
	if !killed {
		ev.End("error", "not_found", errors.New("persistent session not found"))
		server.WriteError(w, http.StatusNotFound, "persistent session not found")
		return
	}
	ev.End("ok", "", nil)
	w.WriteHeader(http.StatusNoContent)
}
