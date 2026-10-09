package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/projectfs"
)

// filesWSUpgrader is permissive for the reason the dispatcher's is: the
// frontend listener is a Unix socket and the credential check has already run.
var filesWSUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// fileWSQueue bounds the frames waiting for one connection. A consumer that
// falls this far behind is closed rather than fed stale events; it reconnects
// and re-sends its watches.
const fileWSQueue = 1024

const defaultWatchRecheck = 2 * time.Second

// watchEntry is the one underlying watcher for a project, shared by every
// connection that watches it.
type watchEntry struct {
	projectID string
	sig       string // path + host: what PROJECT_CHANGED compares

	ready chan struct{} // closed when Watch has returned
	err   error
	stop  func()

	mu     sync.RWMutex // guards subs; fan-out holds the read side
	subs   map[*filesConn]struct{}
	closed bool // set under hub.mu when the entry leaves the map
}

type watchHub struct {
	mu      sync.Mutex
	entries map[string]*watchEntry
	polling bool
}

func (o *FileOps) watchHub() *watchHub {
	o.hubOnce.Do(func() { o.hub = &watchHub{entries: map[string]*watchEntry{}} })
	return o.hub
}

func projectSig(p config.Project) string { return p.Path + "\x00" + p.HostID }

// join adds c to the project's shared watcher, starting it if c is first.
// It returns once the watcher is live; onJoined runs before any event can
// reach c, so a watch_ok queued there precedes every fs_event.
func (o *FileOps) joinWatch(c *filesConn, fs *fileSession, onJoined func()) (*watchEntry, error) {
	h := o.watchHub()
	sig := projectSig(fs.proj)
	for {
		h.mu.Lock()
		e := h.entries[fs.proj.ID]
		if e != nil && e.sig != sig {
			h.mu.Unlock()
			h.invalidate(e)
			continue
		}
		creator := e == nil
		if creator {
			e = &watchEntry{projectID: fs.proj.ID, sig: sig, ready: make(chan struct{}), subs: map[*filesConn]struct{}{}}
			h.entries[fs.proj.ID] = e
		}
		h.mu.Unlock()

		if creator {
			o.startWatch(h, e, fs)
		} else {
			select {
			case <-e.ready:
			case <-c.done:
				return nil, context.Canceled
			}
		}
		if e.err != nil {
			return nil, e.err
		}
		h.mu.Lock()
		if e.closed {
			h.mu.Unlock()
			continue // released or invalidated while joining
		}
		e.mu.Lock()
		e.subs[c] = struct{}{}
		onJoined()
		e.mu.Unlock()
		h.mu.Unlock()
		h.ensurePolling(o)
		return e, nil
	}
}

func (o *FileOps) startWatch(h *watchHub, e *watchEntry, fs *fileSession) {
	defer close(e.ready)
	b, err := fs.backend()
	if err == nil {
		ctx, cancel := context.WithCancel(context.Background())
		var stop func()
		stop, err = b.Watch(ctx, e.fanout)
		if err == nil {
			e.stop = func() { stop(); cancel() }
		} else {
			cancel()
		}
	}
	if err != nil {
		e.err = err
		h.mu.Lock()
		if h.entries[e.projectID] == e {
			delete(h.entries, e.projectID)
		}
		e.closed = true
		h.mu.Unlock()
	}
}

func (e *watchEntry) fanout(ev projectfs.Event) {
	frame := fileFrame{Type: "fs_event", ProjectID: e.projectID, Path: ev.Path, Kind: ev.Kind}
	e.mu.RLock()
	defer e.mu.RUnlock()
	for c := range e.subs {
		c.send(frame)
	}
}

// leave removes c; the last one out stops the watcher.
func (h *watchHub) leave(e *watchEntry, c *filesConn) {
	h.mu.Lock()
	e.mu.Lock()
	delete(e.subs, c)
	last := len(e.subs) == 0 && !e.closed
	e.mu.Unlock()
	if last {
		e.closed = true
		if h.entries[e.projectID] == e {
			delete(h.entries, e.projectID)
		}
	}
	h.mu.Unlock()
	if last && e.stop != nil {
		e.stop()
	}
}

// invalidate tears the entry down and tells every subscriber its project
// changed under it.
func (h *watchHub) invalidate(e *watchEntry) {
	h.mu.Lock()
	if e.closed {
		h.mu.Unlock()
		return
	}
	e.closed = true
	if h.entries[e.projectID] == e {
		delete(h.entries, e.projectID)
	}
	h.mu.Unlock()
	<-e.ready
	if e.stop != nil {
		e.stop()
	}
	e.mu.Lock()
	subs := make([]*filesConn, 0, len(e.subs))
	for c := range e.subs {
		subs = append(subs, c)
	}
	e.subs = map[*filesConn]struct{}{}
	e.mu.Unlock()
	for _, c := range subs {
		c.projectChanged(e.projectID)
	}
}

// ensurePolling starts the settings comparison that raises PROJECT_CHANGED.
// It runs only while a watcher exists.
func (h *watchHub) ensurePolling(o *FileOps) {
	h.mu.Lock()
	if h.polling {
		h.mu.Unlock()
		return
	}
	h.polling = true
	h.mu.Unlock()
	go func() {
		every := o.WatchRecheck
		if every <= 0 {
			every = defaultWatchRecheck
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			if !h.recheck(o) {
				return
			}
		}
	}()
}

// recheck reports whether any watcher is left to look after.
func (h *watchHub) recheck(o *FileOps) bool {
	h.mu.Lock()
	entries := make([]*watchEntry, 0, len(h.entries))
	for _, e := range h.entries {
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		h.polling = false
		h.mu.Unlock()
		return false
	}
	h.mu.Unlock()
	s := config.FreshSettings(o.Store)
	for _, e := range entries {
		select {
		case <-e.ready:
		default:
			continue // still starting; the next tick looks again
		}
		p, _ := config.FindProjectByID(s, e.projectID)
		if p == nil || p.IsRemote() || p.Path == "" || projectSig(*p) != e.sig {
			h.invalidate(e)
		}
	}
	return true
}

// fileFrame is every frame relay sends on /ws/files.
type fileFrame struct {
	Type      string `json:"type"`
	ProjectID string `json:"project_id,omitempty"`
	HostID    string `json:"host_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status,omitempty"`
	Path      string `json:"path,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Code      string `json:"code,omitempty"`
	Error     string `json:"error,omitempty"`
}

type filesConn struct {
	o     *FileOps
	ws    *websocket.Conn
	actor audit.AuditActor
	out   chan []byte
	done  chan struct{}
	once  sync.Once

	mu       sync.Mutex
	watches  map[string]*watchEntry
	tail     map[string]chan struct{} // last queued frame per project
	released bool
}

func (c *filesConn) close() {
	c.once.Do(func() {
		close(c.done)
		_ = c.ws.Close()
	})
}

// send queues a frame. A full queue closes the connection.
func (c *filesConn) send(f fileFrame) {
	raw, err := json.Marshal(f)
	if err != nil {
		return
	}
	select {
	case c.out <- raw:
	case <-c.done:
	default:
		slog.Warn("files: websocket consumer too slow, closing")
		c.close()
	}
}

// enqueue runs fn after every earlier frame for the same project has
// finished, so a watch followed by an unwatch cannot be reordered, while a
// slow host does not hold up other projects.
func (c *filesConn) enqueue(id string, fn func()) {
	c.mu.Lock()
	prev := c.tail[id]
	next := make(chan struct{})
	c.tail[id] = next
	c.mu.Unlock()
	go func() {
		defer close(next)
		if prev != nil {
			<-prev
		}
		fn()
	}()
}

func (c *filesConn) projectChanged(id string) {
	c.mu.Lock()
	delete(c.watches, id)
	c.mu.Unlock()
	c.send(fileFrame{Type: "watch_error", ProjectID: id, Code: projectfs.CodeProjectChanged, Error: "project changed"})
}

func (c *filesConn) watch(id string) {
	c.mu.Lock()
	_, have := c.watches[id]
	c.mu.Unlock()
	if have {
		c.send(fileFrame{Type: "watch_ok", ProjectID: id})
		return
	}
	fs, err := c.o.open(c.actor, id)
	var e *watchEntry
	if err == nil {
		e, err = c.o.joinWatch(c, fs, func() {
			c.mu.Lock()
			c.watches[id] = nil // placeholder replaced just below
			c.mu.Unlock()
			c.send(fileFrame{Type: "watch_ok", ProjectID: id})
		})
	}
	if err != nil {
		code, msg := projectfs.CodeOf(err), "watch failed"
		if fe, ok := err.(*projectfs.Error); ok {
			msg = fe.Msg
		}
		switch code {
		case projectfs.CodeENOENT, projectfs.CodeUnsupported, projectfs.CodeHostUnreachable,
			projectfs.CodeProjectNotFound, projectfs.CodeNotAvailable:
		default:
			code = projectfs.CodeError
		}
		c.send(fileFrame{Type: "watch_error", ProjectID: id, Code: code, Error: msg})
		return
	}
	c.mu.Lock()
	// An invalidation between the join and here already sent its
	// watch_error and cleared the placeholder; do not resurrect it. A
	// connection that closed meanwhile has released everything and must not
	// keep a subscription.
	_, still := c.watches[id]
	if still && !c.released {
		c.watches[id] = e
	}
	leaveNow := still && c.released
	c.mu.Unlock()
	if leaveNow {
		c.o.watchHub().leave(e, c)
	}
}

func (c *filesConn) unwatch(id string) {
	c.mu.Lock()
	e := c.watches[id]
	delete(c.watches, id)
	c.mu.Unlock()
	if e != nil {
		c.o.watchHub().leave(e, c)
	}
}

func (c *filesConn) releaseAll() {
	c.mu.Lock()
	held := c.watches
	c.released = true
	c.watches = map[string]*watchEntry{}
	c.mu.Unlock()
	for _, e := range held {
		if e != nil {
			c.o.watchHub().leave(e, c)
		}
	}
}

func hostStatusFrame(s FileHostStatus) fileFrame {
	return fileFrame{Type: "host_status", HostID: s.HostID, Name: s.Name, Status: s.Status, Error: s.Error}
}

// serveFilesWS serves GET /ws/files: eve's one connection for watch events
// and host status.
func (o *FileOps) serveFilesWS(w http.ResponseWriter, r *http.Request) {
	ws, err := filesWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &filesConn{
		o: o, ws: ws, actor: fileActor(o, r),
		out: make(chan []byte, fileWSQueue), done: make(chan struct{}),
		watches: map[string]*watchEntry{}, tail: map[string]chan struct{}{},
	}
	defer func() {
		c.close()
		c.releaseAll()
	}()

	if o.Hosts != nil {
		cancel := o.Hosts.Subscribe(func(s FileHostStatus) { c.send(hostStatusFrame(s)) })
		defer cancel()
		for _, s := range o.Hosts.Statuses() {
			c.send(hostStatusFrame(s))
		}
	}

	go c.writeLoop()

	_ = ws.SetReadDeadline(time.Now().Add(wsPongWait()))
	ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(wsPongWait())) })
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(wsPongWait()))
		var in struct {
			Type      string `json:"type"`
			ProjectID string `json:"project_id"`
		}
		if json.Unmarshal(raw, &in) != nil || in.ProjectID == "" {
			continue // unknown or malformed frames are ignored
		}
		switch in.Type {
		case "watch":
			c.enqueue(in.ProjectID, func() { c.watch(in.ProjectID) })
		case "unwatch":
			c.enqueue(in.ProjectID, func() { c.unwatch(in.ProjectID) })
		}
	}
}

func (c *filesConn) writeLoop() {
	ping := time.NewTicker(wsPingPeriod())
	defer ping.Stop()
	for {
		select {
		case raw := <-c.out:
			_ = c.ws.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.ws.WriteMessage(websocket.TextMessage, raw); err != nil {
				c.close()
				return
			}
		case <-ping.C:
			_ = c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait))
		case <-c.done:
			return
		}
	}
}
