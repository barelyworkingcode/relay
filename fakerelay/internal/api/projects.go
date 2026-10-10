package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

// extraKeys are project fields the model has no slot for. They are kept per
// project in memory and shown back in the project view.
var extraKeys = []string{"generate_skill", "disabled_tools", "session_folders", "allowed_tools", "access", "context", "allow_external", "mounts"}

var permissionModes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "bypassPermissions": true, "dontAsk": true, "auto": true}

var driveRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

type httpErr struct {
	status int
	msg    string
}

func (e *httpErr) Error() string { return e.msg }

var errChanged = &httpErr{http.StatusConflict, "project changed during approval"}

type projectBody struct {
	Name             *string            `json:"name"`
	Path             *string            `json:"path"`
	Kind             *string            `json:"kind"`
	HostID           *string            `json:"host_id"`
	Mode             *string            `json:"mode"`
	AllowedMCPIDs    *[]string          `json:"allowed_mcp_ids"`
	AllowedModels    *[]string          `json:"allowed_models"`
	AllowedTemplates *[]string          `json:"allowed_templates"`
	ChatTemplates    *[]json.RawMessage `json:"chat_templates"`
	PermissionPolicy json.RawMessage    `json:"permission_policy"`
	FilesReadOnly    *bool              `json:"files_read_only"`
}

func (a *api) projectRoutes(r server.Registrar) {
	r.Route(server.ClassRead, "GET /api/projects", a.listProjects)
	r.Route(server.ClassRead, "GET /api/projects/{id}", a.getProject)
	r.Route(server.ClassConfigure, "POST /api/projects", a.createProject)
	r.Route(server.ClassConfigure, "PUT /api/projects/{id}", a.updateProject)
	r.Route(server.ClassConfigure, "DELETE /api/projects/{id}", a.deleteProject)
	r.Route(server.ClassConfigure, "PUT /api/default_project/{mode}", a.setDefaultProject)
}

func emptyRaw(b json.RawMessage) bool {
	switch strings.TrimSpace(string(b)) {
	case "", "null", "false", `""`, "[]", "{}":
		return true
	}
	return false
}

func (a *api) view(m *state.Model, p world.Project) map[string]any {
	mode := p.Mode
	if mode == "" {
		mode = "both"
	}
	a.mu.Lock()
	ex := a.extra[p.ID]
	a.mu.Unlock()
	created := rfc3339(a.started)
	if raw, found := ex["created_at"]; found {
		_ = json.Unmarshal(raw, &created)
	}
	v := map[string]any{"id": p.ID, "name": p.Name, "path": p.Path, "mode": mode, "created_at": created,
		"allowed_mcp_ids": orEmpty(p.AllowedMCPIDs), "allowed_models": orEmpty(p.AllowedModels), "allowed_templates": orEmpty(p.AllowedTemplates)}
	if p.Kind != "" {
		v["kind"] = p.Kind
	}
	if p.HostID != "" {
		v["host_id"] = p.HostID
	}
	if len(p.ChatTemplates) > 0 {
		v["chat_templates"] = p.ChatTemplates
	}
	if !emptyRaw(p.PermissionPolicy) {
		v["permission_policy"] = p.PermissionPolicy
	}
	if p.FilesReadOnly {
		v["files_read_only"] = true
	}
	var def []string
	if d := m.DefaultProject; d != nil {
		if d.Home == p.ID {
			def = append(def, "home")
		}
		if d.Work == p.ID {
			def = append(def, "work")
		}
	}
	if def != nil {
		v["default_for"] = def
	}
	for _, k := range extraKeys {
		if raw, found := ex[k]; found && !emptyRaw(raw) {
			v[k] = raw
		}
	}
	return v
}

func (a *api) listProjects(w http.ResponseWriter, r *http.Request) {
	var out []map[string]any
	a.State.Read(func(m *state.Model) {
		out = make([]map[string]any, 0, len(m.Projects))
		for _, p := range m.Projects {
			out = append(out, a.view(m, p))
		}
	})
	a.Events.Begin(r.Context(), "project.list").Set("count", len(out)).End("ok", "", nil)
	ok(w, out)
}

func (a *api) getProject(w http.ResponseWriter, r *http.Request) {
	var v map[string]any
	a.State.Read(func(m *state.Model) {
		if p, found := findProject(m, r.PathValue("id")); found {
			v = a.view(m, p)
		}
	})
	ev := a.Events.Begin(r.Context(), "project.get").Set("project_id", r.PathValue("id"))
	if v == nil {
		ev.End("error", "not_found", errors.New("project not found"))
		server.WriteError(w, http.StatusNotFound, "project not found")
		return
	}
	ev.End("ok", "", nil)
	ok(w, v)
}

// validateProject checks the project as it would be stored.
func validateProject(m *state.Model, p world.Project) error {
	bad := func(msg string) error { return &httpErr{http.StatusBadRequest, msg} }
	switch {
	case strings.TrimSpace(p.Name) == "":
		return bad("project name is required")
	case p.Path == "":
		return bad("project path is required")
	case p.Kind != "" && p.Kind != "remote":
		return bad("invalid project kind: " + p.Kind)
	case p.Kind == "remote" && p.HostID != "":
		return bad(`project cannot set both host_id and kind: "remote": an access profile does not run on a host`)
	case p.Mode != "" && p.Mode != "home" && p.Mode != "work" && p.Mode != "both":
		return bad("invalid mode: " + p.Mode)
	}
	if !strings.HasPrefix(p.Path, "/") && !(p.HostID != "" && driveRe.MatchString(p.Path)) {
		return bad(`project path must be an absolute path: "` + p.Path + `"`)
	}
	for _, seg := range strings.FieldsFunc(p.Path, func(c rune) bool { return c == '/' || c == '\\' }) {
		if seg == ".." {
			return bad(`project path must not contain '..': "` + p.Path + `"`)
		}
	}
	if p.HostID != "" {
		if _, found := findHost(m, p.HostID); !found {
			return bad(`unknown host_id "` + p.HostID + `"`)
		}
	}
	if !emptyRaw(p.PermissionPolicy) {
		var pol struct {
			DefaultMode string `json:"default_mode"`
		}
		if err := json.Unmarshal(p.PermissionPolicy, &pol); err != nil {
			return bad("invalid permission_policy: " + err.Error())
		}
		if pol.DefaultMode != "" && !permissionModes[pol.DefaultMode] {
			return bad("invalid default_mode: " + pol.DefaultMode)
		}
	}
	return nil
}

func applyBody(p *world.Project, b *projectBody, create bool) {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&p.Name, b.Name)
	set(&p.Path, b.Path)
	set(&p.Kind, b.Kind)
	set(&p.HostID, b.HostID)
	set(&p.Mode, b.Mode)
	if b.AllowedMCPIDs != nil {
		p.AllowedMCPIDs = *b.AllowedMCPIDs
	}
	if b.AllowedModels != nil {
		p.AllowedModels = *b.AllowedModels
	}
	if b.AllowedTemplates != nil {
		p.AllowedTemplates = *b.AllowedTemplates
	}
	if b.ChatTemplates != nil {
		p.ChatTemplates = *b.ChatTemplates
	}
	if len(b.PermissionPolicy) > 0 {
		p.PermissionPolicy = b.PermissionPolicy
	}
	if b.FilesReadOnly != nil && !create {
		p.FilesReadOnly = *b.FilesReadOnly
	}
}

func readProjectBody(w http.ResponseWriter, r *http.Request) (*projectBody, map[string]json.RawMessage, error) {
	raw, err := readBody(w, r, 1<<20)
	if err != nil {
		return nil, nil, err
	}
	var b projectBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, nil, err
	}
	var all map[string]json.RawMessage
	_ = json.Unmarshal(raw, &all)
	ex := map[string]json.RawMessage{}
	for _, k := range extraKeys {
		if v, found := all[k]; found {
			ex[k] = v
		}
	}
	return &b, ex, nil
}

func writeBodyErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errTooLarge) {
		server.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	invalidJSON(w, err)
}

func (a *api) createProject(w http.ResponseWriter, r *http.Request) {
	ev := a.Events.Begin(r.Context(), "project.create")
	b, ex, err := readProjectBody(w, r)
	if err != nil {
		ev.End("error", "invalid", err)
		writeBodyErr(w, err)
		return
	}
	p := world.Project{ID: "p_" + events.NewID()[:8]}
	applyBody(&p, b, true)
	var verr error
	a.State.Read(func(m *state.Model) { verr = validateProject(m, p) })
	if verr != nil {
		ev.End("error", "invalid", verr)
		server.WriteError(w, http.StatusBadRequest, verr.Error())
		return
	}
	ev.Set("project_id", "").Set("kind", "")
	if !a.gate(server.WithSubject(r.Context(), p.Name), w, ev, "project.grant") {
		return
	}
	ev.Set("project_id", p.ID).Set("kind", p.Kind)
	ex["created_at"], _ = json.Marshal(rfc3339(a.now()))
	var view map[string]any
	err = a.State.Write(func(m *state.Model) error {
		if err := validateProject(m, p); err != nil {
			return err
		}
		m.Projects = append(m.Projects, p)
		a.mu.Lock()
		a.extra[p.ID] = ex
		a.mu.Unlock()
		view = a.view(m, p)
		if p.HostID != "" {
			return os.MkdirAll(state.RootOf(m, p), 0o755)
		}
		return nil
	})
	if err != nil {
		ev.End("error", "invalid", err)
		server.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	ev.End("ok", "", nil)
	server.WriteJSON(w, http.StatusCreated, view)
}

// listAdds reports whether next holds an element prev lacks.
func listAdds(prev, next json.RawMessage) bool {
	var a, b []json.RawMessage
	_ = json.Unmarshal(prev, &a)
	if json.Unmarshal(next, &b) != nil {
		return !emptyRaw(next)
	}
	have := map[string]bool{}
	for _, x := range a {
		have[string(bytes.TrimSpace(x))] = true
	}
	for _, x := range b {
		if !have[string(bytes.TrimSpace(x))] {
			return true
		}
	}
	return false
}

// widens reports whether the patch grants more than the project holds now.
func widens(old world.Project, oldEx map[string]json.RawMessage, b *projectBody, ex map[string]json.RawMessage) bool {
	changed := func(n *string, o string) bool { return n != nil && *n != o }
	if changed(b.Kind, old.Kind) || changed(b.HostID, old.HostID) || changed(b.Path, old.Path) {
		return true
	}
	if b.AllowedMCPIDs != nil {
		prev, _ := json.Marshal(old.AllowedMCPIDs)
		next, _ := json.Marshal(*b.AllowedMCPIDs)
		if listAdds(prev, next) {
			return true
		}
	}
	for k, next := range ex {
		switch k {
		case "allowed_tools", "mounts":
			if listAdds(oldEx[k], next) {
				return true
			}
		case "allow_external":
			if string(bytes.TrimSpace(next)) == "true" && string(bytes.TrimSpace(oldEx[k])) != "true" {
				return true
			}
		case "access", "context":
			if !emptyRaw(next) && !bytes.Equal(bytes.TrimSpace(next), bytes.TrimSpace(oldEx[k])) {
				return true
			}
		}
	}
	return false
}

func (a *api) updateProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), "project.update").Set("project_id", id)
	b, ex, err := readProjectBody(w, r)
	if err != nil {
		ev.End("error", "invalid", err)
		writeBodyErr(w, err)
		return
	}
	view, err := a.patchProject(r, w, ev, id, b, ex)
	var he *httpErr
	switch {
	case err == nil && view != nil:
		ok(w, view)
	case errors.As(err, &he):
		server.WriteError(w, he.status, he.msg)
	}
}

// patchProject applies a patch, asking for presence first when it widens. A
// nil view with a nil error means the response is already written.
func (a *api) patchProject(r *http.Request, w http.ResponseWriter, ev *events.Event, id string, b *projectBody, ex map[string]json.RawMessage) (map[string]any, error) {
	var old world.Project
	var found bool
	a.State.Read(func(m *state.Model) { old, found = findProject(m, id) })
	if !found {
		err := &httpErr{http.StatusNotFound, "project not found"}
		ev.End("error", "not_found", err)
		return nil, err
	}
	a.mu.Lock()
	oldEx := a.extra[id]
	a.mu.Unlock()
	gated := widens(old, oldEx, b, ex)
	ev.Set("gated", gated)
	if gated && !a.gate(server.WithSubject(r.Context(), old.Name), w, ev, "project.grant") {
		return nil, nil
	}
	snapshot, _ := json.Marshal(old)
	var view map[string]any
	err := a.State.Write(func(m *state.Model) error {
		for i := range m.Projects {
			if m.Projects[i].ID != id {
				continue
			}
			if cur, _ := json.Marshal(m.Projects[i]); gated && !bytes.Equal(cur, snapshot) {
				return errChanged
			}
			next := m.Projects[i]
			applyBody(&next, b, false)
			if err := validateProject(m, next); err != nil {
				return err
			}
			m.Projects[i] = next
			a.mu.Lock()
			merged := map[string]json.RawMessage{}
			for k, v := range a.extra[id] {
				merged[k] = v
			}
			for k, v := range ex {
				merged[k] = v
			}
			a.extra[id] = merged
			a.mu.Unlock()
			view = a.view(m, next)
			return nil
		}
		return &httpErr{http.StatusNotFound, "project not found"}
	})
	if err != nil {
		reason := "invalid"
		var he *httpErr
		if errors.As(err, &he) {
			switch he.status {
			case http.StatusNotFound:
				reason = "not_found"
			case http.StatusConflict:
				reason = "conflict"
			}
		}
		ev.End("error", reason, err)
		return nil, err
	}
	ev.End("ok", "", nil)
	return view, nil
}

func (a *api) deleteProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := a.Events.Begin(r.Context(), "project.remove").Set("project_id", id)
	err := a.State.Write(func(m *state.Model) error {
		for i := range m.Projects {
			if m.Projects[i].ID == id {
				m.Projects = append(m.Projects[:i], m.Projects[i+1:]...)
				if d := m.DefaultProject; d != nil {
					if d.Home == id {
						d.Home = ""
					}
					if d.Work == id {
						d.Work = ""
					}
				}
				return nil
			}
		}
		return &httpErr{http.StatusNotFound, "project not found"}
	})
	if err != nil {
		ev.End("error", "not_found", err)
		server.WriteError(w, http.StatusNotFound, "project not found")
		return
	}
	a.mu.Lock()
	delete(a.extra, id)
	a.mu.Unlock()
	ev.End("ok", "", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) setDefaultProject(w http.ResponseWriter, r *http.Request) {
	mode := r.PathValue("mode")
	ev := a.Events.Begin(r.Context(), "project.default.set").Set("mode", mode)
	fail := func(msg string) {
		ev.End("error", "invalid", errors.New(msg))
		server.WriteError(w, http.StatusBadRequest, msg)
	}
	if mode != "home" && mode != "work" {
		fail("invalid mode: " + mode)
		return
	}
	var body struct {
		ProjectID *string `json:"project_id"`
	}
	if err := decode(w, r, 4096, &body, true); err != nil {
		ev.End("error", "invalid", err)
		writeBodyErr(w, err)
		return
	}
	if body.ProjectID == nil {
		fail(`project_id is required; send "" to clear the default`)
		return
	}
	ev.Set("project_id", *body.ProjectID)
	var out world.DefaultProject
	err := a.State.Write(func(m *state.Model) error {
		if *body.ProjectID != "" {
			if _, found := findProject(m, *body.ProjectID); !found {
				return &httpErr{http.StatusBadRequest, fmt.Sprintf("invalid default project: no project with id %q", *body.ProjectID)}
			}
		}
		if m.DefaultProject == nil {
			m.DefaultProject = &world.DefaultProject{}
		}
		if mode == "home" {
			m.DefaultProject.Home = *body.ProjectID
		} else {
			m.DefaultProject.Work = *body.ProjectID
		}
		out = *m.DefaultProject
		return nil
	})
	if err != nil {
		fail(err.Error())
		return
	}
	ev.End("ok", "", nil)
	ok(w, out)
}
