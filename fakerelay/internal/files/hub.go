package files

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

const (
	queueDepth   = 1024
	pingEvery    = 20 * time.Second
	idleLimit    = 60 * time.Second
	writeTimeout = 10 * time.Second
	agentNone    = "none"
)

// HostStatus is a host agent's state: none, connecting, connected or
// unreachable.
type HostStatus = server.HostStatus

// HostStatuses is the agent state of every host in the world.
func (h *hub) HostStatuses() map[string]server.HostStatus {
	out := map[string]server.HostStatus{}
	h.sync()
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, hs := range h.hosts {
		out[id] = *hs
	}
	return out
}

// SetHostStatus moves a host's agent to status and returns after the
// host_status frame has gone to every /ws/files connection. It reports false
// for an unknown host.
func (h *hub) SetHostStatus(id, status, errText string) bool {
	return h.setStatus(id, status, errText)
}

type hub struct {
	s         *service
	mu        sync.Mutex
	conns     map[*conn]bool
	hosts     map[string]*HostStatus
	connectMu sync.Mutex
}

func newHub(s *service) *hub {
	return &hub{s: s, conns: map[*conn]bool{}, hosts: map[string]*HostStatus{}}
}

type outFrame struct {
	data []byte
	done chan struct{}
}

type conn struct {
	ws      *websocket.Conn
	out     chan outFrame
	closed  chan struct{}
	once    sync.Once
	watches map[string]bool // guarded by hub.mu
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.closed)
		c.ws.Close()
	})
}

// send queues a frame. With wait it returns once the frame is on the wire, and
// reports whether it got there. A full queue closes the connection.
func (c *conn) send(v any, wait bool) bool {
	b, _ := json.Marshal(v)
	f := outFrame{data: b}
	if wait {
		f.done = make(chan struct{})
	}
	select {
	case c.out <- f:
	case <-c.closed:
		return false
	default:
		c.close()
		return false
	}
	if !wait {
		return true
	}
	select {
	case <-f.done:
		return true
	case <-c.closed:
		select {
		case <-f.done:
			return true
		default:
			return false
		}
	}
}

func (c *conn) writeLoop() {
	tick := time.NewTicker(pingEvery)
	defer tick.Stop()
	for {
		var err error
		select {
		case f := <-c.out:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err = c.ws.WriteMessage(websocket.TextMessage, f.data); err == nil && f.done != nil {
				close(f.done)
			}
		case <-tick.C:
			err = c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout))
		case <-c.closed:
			return
		}
		if err != nil {
			c.close()
			return
		}
	}
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func (h *hub) serveWS(w http.ResponseWriter, r *http.Request) {
	ev := h.s.d.Events.Begin(r.Context(), "file.ws.close")
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &conn{ws: ws, out: make(chan outFrame, queueDepth), closed: make(chan struct{}), watches: map[string]bool{}}
	go c.writeLoop()
	h.sync()
	h.mu.Lock()
	h.conns[c] = true
	frames := h.heldFrames()
	h.mu.Unlock()
	for _, f := range frames {
		c.send(f, false)
	}
	defer func() {
		h.mu.Lock()
		delete(h.conns, c)
		h.mu.Unlock()
		c.close()
		ev.End("ok", "", nil)
	}()
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(idleLimit)) })
	for {
		_ = ws.SetReadDeadline(time.Now().Add(idleLimit))
		_, msg, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var in struct {
			Type      string `json:"type"`
			ProjectID string `json:"project_id"`
		}
		if json.Unmarshal(msg, &in) != nil || in.ProjectID == "" {
			continue
		}
		switch in.Type {
		case "watch":
			h.watch(c, in.ProjectID)
		case "unwatch":
			h.mu.Lock()
			delete(c.watches, in.ProjectID)
			h.mu.Unlock()
		}
	}
}

func (h *hub) watch(c *conn, id string) {
	h.mu.Lock()
	delete(c.watches, id)
	h.mu.Unlock()
	fail := func(e *fileErr) {
		c.send(map[string]string{"type": "watch_error", "project_id": id, "code": watchCode(e), "error": e.Msg}, false)
	}
	t, e := h.s.target(id)
	if e == nil {
		e = h.ensure(t)
	}
	if e == nil {
		if _, err := statDir(t.root); err != nil {
			e = osErr(err)
		}
	}
	if e != nil {
		fail(e)
		return
	}
	h.mu.Lock()
	c.watches[id] = true
	h.mu.Unlock()
	c.send(map[string]string{"type": "watch_ok", "project_id": id}, false)
}

// watchCode narrows a file error to the codes a watch_error may carry.
func watchCode(e *fileErr) string {
	switch e.Code {
	case "ENOENT", "HOST_UNREACHABLE", "PROJECT_NOT_FOUND", "NOT_AVAILABLE":
		return e.Code
	}
	return "ERROR"
}

// projectChanged ends the watches on a project that was deleted or moved.
func (h *hub) projectChanged(old, cur *world.Project) {
	if old == nil || (cur != nil && cur.Path == old.Path && cur.HostID == old.HostID && cur.Kind == old.Kind) {
		return
	}
	for _, c := range h.watchers(old.ID, true) {
		c.send(map[string]string{"type": "watch_error", "project_id": old.ID, "code": "PROJECT_CHANGED", "error": "project changed"}, false)
	}
}

// watchers returns the connections watching a project, dropping the watch
// when drop is set.
func (h *hub) watchers(id string, drop bool) []*conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*conn
	for c := range h.conns {
		if c.watches[id] {
			out = append(out, c)
			if drop {
				delete(c.watches, id)
			}
		}
	}
	return out
}

// emit sends fs_event frames to the watchers of a project and counts the
// connections that got every one. With wait it returns once they are written.
func (h *hub) emit(id string, evs []fsEvent, wait bool) int {
	if len(evs) == 0 {
		return 0
	}
	delivered := 0
	for _, c := range h.watchers(id, false) {
		ok := true
		for _, e := range evs {
			ok = c.send(map[string]string{"type": "fs_event", "project_id": id, "path": e.path, "kind": e.kind}, wait) && ok
		}
		if ok {
			delivered++
		}
	}
	return delivered
}

func (h *hub) controlFSEvent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
		server.WriteError(w, http.StatusBadRequest, "send {path, kind}")
		return
	}
	if body.Kind == "" {
		body.Kind = "change"
	}
	if body.Kind != "change" && body.Kind != "rename" {
		server.WriteError(w, http.StatusBadRequest, "kind must be change or rename")
		return
	}
	id := r.PathValue("id")
	if _, e := h.s.target(id); e != nil {
		server.WriteError(w, e.Status, e.Msg)
		return
	}
	n := h.emit(id, []fsEvent{{strings.TrimLeft(body.Path, "/"), body.Kind}}, true)
	server.WriteJSON(w, http.StatusOK, map[string]int{"delivered": n})
}

func (h *hub) controlHostStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		server.WriteError(w, http.StatusBadRequest, "send {status, error}")
		return
	}
	switch body.Status {
	case "connecting", "connected", "unreachable":
	default:
		server.WriteError(w, http.StatusBadRequest, "status must be connecting, connected or unreachable")
		return
	}
	if !h.setStatus(r.PathValue("id"), body.Status, body.Error) {
		server.WriteError(w, http.StatusNotFound, "host not found")
		return
	}
	server.WriteJSON(w, http.StatusOK, HostStatus{Status: body.Status, Error: body.Error})
}

// sync adds an entry for every host in the world that has none yet, from the
// host's initial agent state.
func (h *hub) sync() {
	var hosts []world.Host
	h.s.d.State.Read(func(m *state.Model) { hosts = append(hosts, m.Hosts...) })
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, host := range hosts {
		if h.hosts[host.ID] == nil {
			st := host.Agent
			if st == "" {
				st = agentNone
			}
			h.hosts[host.ID] = &HostStatus{Status: st}
		}
	}
}

func (h *hub) hostName(id string) (name string, ok bool) {
	h.s.d.State.Read(func(m *state.Model) {
		for _, host := range m.Hosts {
			if host.ID == id {
				name, ok = host.Name, true
			}
		}
	})
	return
}

func (h *hub) hostRoot(id string) (root string) {
	h.s.d.State.Read(func(m *state.Model) {
		for _, host := range m.Hosts {
			if host.ID == id {
				root = host.Root
			}
		}
	})
	return
}

// heldFrames is one host_status per host agent the hub holds. Call with mu
// held.
func (h *hub) heldFrames() []map[string]string {
	var out []map[string]string
	for id, hs := range h.hosts {
		if name, ok := h.hostName(id); ok && hs.Status != agentNone {
			out = append(out, statusFrame(id, name, *hs))
		}
	}
	return out
}

func statusFrame(id, name string, hs HostStatus) map[string]string {
	f := map[string]string{"type": "host_status", "host_id": id, "name": name, "status": hs.Status}
	if hs.Error != "" {
		f["error"] = hs.Error
	}
	return f
}

// setStatus records a host's agent state and returns once the host_status
// frame is on the wire of every connection.
func (h *hub) setStatus(id, status, errText string) bool {
	name, ok := h.hostName(id)
	if !ok {
		return false
	}
	h.sync()
	h.mu.Lock()
	hs := HostStatus{Status: status, Error: errText}
	h.hosts[id] = &hs
	conns := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	frame := statusFrame(id, name, hs)
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.send(frame, true)
		}()
	}
	wg.Wait()
	return true
}

// ensure makes a host project's agent connected. A host with no agent goes
// connecting then connected on first use; one that is connecting or
// unreachable refuses.
func (h *hub) ensure(t *target) *fileErr {
	if t.hostID == "" {
		return nil
	}
	if _, ok := h.hostName(t.hostID); !ok {
		return errHostDown
	}
	h.sync()
	h.connectMu.Lock()
	defer h.connectMu.Unlock()
	h.mu.Lock()
	st := h.hosts[t.hostID].Status
	h.mu.Unlock()
	switch st {
	case "connected":
		return nil
	case agentNone:
		h.setStatus(t.hostID, "connecting", "")
		h.setStatus(t.hostID, "connected", "")
		return nil
	}
	return errHostDown
}
