// Package files serves the file plane: the project file routes, the host
// paste route, the /ws/files hub and the control endpoints that drive it. Disk
// is the only store; nothing is watched, so a change reaches a watcher as an
// fs_event that the mutating route or `ctl fs-event` sends.
package files

import (
	"net/http"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
)

// Register installs the file routes, /ws/files and the control endpoints.
func Register(r server.Registrar, d server.Deps) error {
	s := &service{d: d}
	s.hub = newHub(s)
	active.Store(s.hub)
	d.State.OnProjectChange(s.hub.projectChanged)

	const base = "POST /api/projects/{id}/files/"
	r.Route(server.ClassExecute, base+"list", s.handle("list", false, smallBody, s.list))
	r.Route(server.ClassExecute, base+"stat", s.handle("stat", false, smallBody, s.stat))
	r.Route(server.ClassExecute, base+"read", s.handle("read", false, smallBody, s.read))
	r.Route(server.ClassExecute, base+"write", s.handle("write", true, largeBody, s.write))
	r.Route(server.ClassExecute, base+"mkdir", s.handle("mkdir", true, smallBody, s.mkdir))
	r.Route(server.ClassExecute, base+"rename", s.handle("rename", true, smallBody, s.rename))
	r.Route(server.ClassExecute, base+"move", s.handle("move", true, smallBody, s.move))
	r.Route(server.ClassExecute, base+"delete", s.handle("delete", true, smallBody, s.remove))
	r.Route(server.ClassExecute, base+"search", s.handle("search", false, smallBody, s.search))
	r.Route(server.ClassExecute, base+"git", s.handle("git", false, smallBody, s.git))
	r.Route(server.ClassExecute, "GET /api/projects/{id}/files/stream", http.HandlerFunc(s.stream))
	r.Route(server.ClassExecute, "POST /api/hosts/{id}/pastetmp", http.HandlerFunc(s.pastetmp))
	r.Route(server.ClassExecute, "GET /ws/files", http.HandlerFunc(s.hub.serveWS))

	r.Control("PUT /v1/hosts/{id}/status", s.hub.controlHostStatus)
	r.Control("POST /v1/projects/{id}/fs-events", s.hub.controlFSEvent)
	return nil
}

type service struct {
	d   server.Deps
	hub *hub
}
