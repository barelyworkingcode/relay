package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

var templateIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validTemplate(t world.Template) *httpErr {
	switch {
	case !templateIDRe.MatchString(t.ID):
		return &httpErr{http.StatusBadRequest, "template id must be letters, digits, '.', '_' or '-'"}
	case strings.TrimSpace(t.Name) == "":
		return &httpErr{http.StatusBadRequest, "template name is required"}
	}
	return nil
}

func (a *api) templateRoutes(r server.Registrar) {
	r.Route(server.ClassRead, "GET /api/terminal/templates", a.listTemplates)
	r.Route(server.ClassRead, "GET /api/terminal/templates/{id}", a.getTemplate)
	r.Route(server.ClassConfigure, "POST /api/terminal/templates", a.createTemplate)
	r.Route(server.ClassConfigure, "PUT /api/terminal/templates/{id}", a.updateTemplate)
	r.Route(server.ClassConfigure, "DELETE /api/terminal/templates/{id}", a.deleteTemplate)
}

// allows reports whether a project's allowed list names id; an empty list
// allows every template.
func allows(list []string, id string) bool {
	if len(list) == 0 {
		return true
	}
	for _, x := range list {
		if x == "*" || x == id {
			return true
		}
	}
	return false
}

func (a *api) listTemplates(w http.ResponseWriter, r *http.Request) {
	out := []world.Template{}
	a.State.Read(func(m *state.Model) {
		p, found := findProject(m, r.URL.Query().Get("project"))
		switch {
		case !found:
		case p.HostID != "":
			if h, f := findHost(m, p.HostID); f {
				out = append(out, sortedIDs(h.TerminalTemplates, func(t world.Template) string { return t.ID })...)
			}
		default:
			var all []world.Template
			for _, t := range m.Templates {
				if allows(p.AllowedTemplates, t.ID) {
					all = append(all, t)
				}
			}
			out = append(out, sortedIDs(all, func(t world.Template) string { return t.ID })...)
		}
	})
	a.Events.Begin(r.Context(), "template.list").Set("count", len(out)).End("ok", "", nil)
	ok(w, out)
}

func (a *api) getTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), "template.get").Set("template_id", id)
	var found *world.Template
	a.State.Read(func(m *state.Model) {
		for i := range m.Templates {
			if m.Templates[i].ID == id {
				t := m.Templates[i]
				found = &t
			}
		}
	})
	if found == nil {
		ev.End("error", "not_found", errors.New("template not found"))
		server.WriteError(w, http.StatusNotFound, "template not found")
		return
	}
	ev.End("ok", "", nil)
	ok(w, found)
}

// templateWrite runs fn on the console template list and answers.
func (a *api) templateWrite(w http.ResponseWriter, r *http.Request, key, id string, status int, herr *httpErr, fn func(m *state.Model) (*world.Template, *httpErr)) {
	ev := a.Events.Begin(r.Context(), key).Set("template_id", id)
	var out *world.Template
	if herr == nil {
		_ = a.State.Write(func(m *state.Model) error {
			out, herr = fn(m)
			if herr != nil {
				return herr
			}
			return nil
		})
	}
	if herr != nil {
		reason := map[int]string{http.StatusBadRequest: "invalid", http.StatusNotFound: "not_found", http.StatusConflict: "conflict", http.StatusRequestEntityTooLarge: "invalid"}[herr.status]
		ev.End("error", reason, herr)
		server.WriteError(w, herr.status, herr.msg)
		return
	}
	ev.End("ok", "", nil)
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	server.WriteJSON(w, status, out)
}

func (a *api) createTemplate(w http.ResponseWriter, r *http.Request) {
	t, herr := a.readTemplate(w, r, "")
	a.templateWrite(w, r, "template.create", t.ID, http.StatusCreated, herr, func(m *state.Model) (*world.Template, *httpErr) {
		for _, x := range m.Templates {
			if x.ID == t.ID {
				return nil, &httpErr{http.StatusConflict, `template "` + t.ID + `" already exists`}
			}
		}
		m.Templates = append(m.Templates, t)
		return &t, nil
	})
}

func (a *api) updateTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, herr := a.readTemplate(w, r, id)
	a.templateWrite(w, r, "template.update", id, http.StatusOK, herr, func(m *state.Model) (*world.Template, *httpErr) {
		for i := range m.Templates {
			if m.Templates[i].ID == id {
				m.Templates[i] = t
				return &t, nil
			}
		}
		return nil, &httpErr{http.StatusNotFound, `template "` + id + `" not found`}
	})
}

func (a *api) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.templateWrite(w, r, "template.remove", id, http.StatusNoContent, nil, func(m *state.Model) (*world.Template, *httpErr) {
		for i := range m.Templates {
			if m.Templates[i].ID == id {
				m.Templates = append(m.Templates[:i], m.Templates[i+1:]...)
				return nil, nil
			}
		}
		return nil, &httpErr{http.StatusNotFound, `template "` + id + `" not found`}
	})
}
