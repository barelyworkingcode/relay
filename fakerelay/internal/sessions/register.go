// Package sessions serves sessions, Chief of Staff sessions and messages,
// terminals, the model list and the /ws hub. Agents come from the fakes seam.
package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/fakes"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

type svc struct {
	server.Deps
	factory fakes.AgentFactory
	hosts   fakes.ModelHost
	started time.Time

	mu       sync.Mutex
	conns    map[*conn]struct{}
	sessions map[string]*session
	terms    map[string]*terminal
	perms    map[string]string // permission id -> session id
	logs     map[string]*termLog
}

// Register installs the doors and the /ws hub, and seeds the world's sessions.
func Register(r server.Registrar, d server.Deps) error {
	calls := fakes.NewCallLog(d.Dir)
	s := &svc{Deps: d, factory: fakes.NewAgentFactory(calls, filepath.Join(d.Dir, "home")), started: d.Clock.Now().UTC(),
		hosts: fakes.WorldModelHost{List: d.World.Models, Log: calls},
		conns: map[*conn]struct{}{}, sessions: map[string]*session{}, terms: map[string]*terminal{}, perms: map[string]string{}, logs: map[string]*termLog{}}
	s.seed()
	r.Route(server.ClassProxy, "GET /api/models", s.listModels)
	r.Route(server.ClassProxy, "GET /api/sessions", s.listSessions)
	r.Route(server.ClassExecute, "POST /api/sessions", s.createSession)
	r.Route(server.ClassProxy, "DELETE /api/sessions/{id}", s.deleteSession)
	r.Route(server.ClassExecute, "POST /api/sessions/{id}/resume", s.resumeSession)
	r.Route(server.ClassProxy, "GET /api/terminals", s.listTerminals)
	r.Route(server.ClassExecute, "POST /api/terminals", s.createTerminal)
	r.Route(server.ClassProxy, "DELETE /api/terminals/{id}", s.deleteTerminal)
	r.Route(server.ClassExecute, "POST /api/sessions/{id}/drop-in", s.dropIn)
	r.Route(server.ClassProxy, "GET /api/terminals/{id}/log", s.terminalLog)
	// The session host answers an unrouted path under these roots with the Go
	// mux's 404. The bare root is registered too, so the mux does not redirect
	// it to the subtree and every other method keeps the dispatch 404.
	for _, root := range []string{"/api/sessions", "/api/terminals"} {
		r.Route(server.ClassProxy, root, s.dispatchNotFound)
		r.Route(server.ClassProxy, root+"/", s.hostNotFound)
	}
	r.Route(server.ClassProxy, "GET /ws", s.serveWS)
	r.Route(server.ClassChiefOfStaff, "POST /api/chief-of-staff/messages", s.cosMessage)
	r.Route(server.ClassChiefOfStaff, "POST /api/chief-of-staff/sessions", s.cosStart)
	r.Control("GET /v1/state", s.serveState)
	return nil
}

func (s *svc) hostNotFound(w http.ResponseWriter, r *http.Request) {
	server.WriteText(w, http.StatusNotFound, "404 page not found")
}

func (s *svc) dispatchNotFound(w http.ResponseWriter, r *http.Request) {
	server.WriteText(w, http.StatusNotFound, "no service registered for this path")
}

func (s *svc) now() time.Time { return s.Clock.Now().UTC() }

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func ok(w http.ResponseWriter, v any) { server.WriteJSON(w, http.StatusOK, v) }

var errTooLarge = errors.New("request body too large")

func readBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		return nil, errTooLarge
	}
	return body, err
}

func decode(w http.ResponseWriter, r *http.Request, max int64, v any) error {
	body, err := readBody(w, r, max)
	if err != nil {
		return err
	}
	return json.NewDecoder(bytes.NewReader(body)).Decode(v)
}

// refusal is a launch refusal: the HTTP status, a stable code for events and
// the Chief of Staff routes, and the text.
type refusal struct {
	status    int
	code, msg string
}

// launchTarget is the project, directory and host a launch resolves to.
type launchTarget struct {
	project world.Project
	dir     string
	host    map[string]any
}

// authorize resolves a launch. model is checked only when it is not empty.
func (s *svc) authorize(projectID, directory, model, kind string) (launchTarget, *refusal) {
	var t launchTarget
	var found bool
	var host *world.Host
	s.State.Read(func(m *state.Model) {
		t.project, found = lookupProject(m, projectID)
		if found && t.project.HostID != "" {
			for i := range m.Hosts {
				if m.Hosts[i].ID == t.project.HostID {
					h := m.Hosts[i]
					host = &h
				}
			}
		}
	})
	switch {
	case projectID == "":
		return t, &refusal{http.StatusForbidden, "project_required", kind + " sessions require a project"}
	case !found || t.project.Kind == "remote":
		return t, &refusal{http.StatusForbidden, "project_unavailable", "project is not available for a session launch"}
	}
	t.dir = t.project.Path
	if directory != "" {
		t.dir = directory
		if !strings.HasPrefix(directory, "/") {
			t.dir = path.Join(t.project.Path, directory)
		}
		t.dir = path.Clean(t.dir)
		root := path.Clean(t.project.Path)
		if t.dir != root && !strings.HasPrefix(t.dir, strings.TrimSuffix(root, "/")+"/") {
			return t, &refusal{http.StatusForbidden, "directory_outside", "requested directory is outside the project"}
		}
	}
	if model != "" && !allowed(t.project.AllowedModels, model) {
		return t, &refusal{http.StatusForbidden, "model_not_allowed", "model is not allowed for this project"}
	}
	if host != nil {
		t.host = map[string]any{"id": host.ID, "name": host.Name, "ssh_argv": []string{"ssh", "-o", "BatchMode=yes", host.Target}}
	}
	return t, nil
}

// allowed: an empty list allows everything, like "*".
func allowed(list []string, v string) bool {
	if len(list) == 0 {
		return true
	}
	for _, x := range list {
		if x == "*" || x == v {
			return true
		}
	}
	return false
}

func lookupProject(m *state.Model, id string) (world.Project, bool) {
	for _, p := range m.Projects {
		if p.ID == id {
			return p, true
		}
	}
	return world.Project{}, false
}
