package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/bridge"
	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

type fault struct {
	world.Fault
	left int // applications remaining; meaningful only when Times > 0
	rel  chan struct{}
	// holds counts requests waiting on a slow fault; a spent slow fault stays
	// findable by id until they end.
	holds int
	spent bool
}

type faultSet struct {
	mu    sync.Mutex
	seq   int
	list  []*fault
	known func(route string) bool
	log   *events.Log
	track *connTracker
}

func newFaultSet(log *events.Log, known func(string) bool) *faultSet {
	return &faultSet{log: log, known: known, track: &connTracker{conns: map[string]map[net.Conn]bool{}}}
}

// Add validates and arms a fault and returns its id. A down fault also closes
// the route's open connections.
func (s *faultSet) Add(spec world.Fault) (string, error) {
	if err := world.ValidateFault("fault", spec); err != nil {
		return "", err
	}
	if !s.routeOK(spec.Route) {
		return "", fmt.Errorf("fault.route: unknown route %q", spec.Route)
	}
	s.mu.Lock()
	s.seq++
	f := &fault{Fault: spec, left: spec.Times, rel: make(chan struct{})}
	if f.ID == "" {
		f.ID = fmt.Sprintf("f%d", s.seq)
	}
	for _, o := range s.list {
		if o.ID == f.ID {
			s.mu.Unlock()
			return "", fmt.Errorf("fault.id: %q is already in use", f.ID)
		}
	}
	s.list = append(s.list, f)
	s.mu.Unlock()
	if f.Mode == "down" {
		s.track.closeRoute(f.Route)
	}
	return f.ID, nil
}

func (s *faultSet) routeOK(route string) bool {
	switch {
	case route == "*":
		return true
	case strings.HasPrefix(route, "BRIDGE "):
		return strings.TrimSpace(route[7:]) != ""
	case strings.HasPrefix(route, "PROXY "):
		return strings.HasPrefix(route[6:], "/")
	}
	return s.known(route)
}

// Clear removes one fault, or all when id is "", releasing what they hold.
func (s *faultSet) Clear(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []*fault
	found := false
	for _, f := range s.list {
		if id == "" || f.ID == id {
			found = true
			close(f.rel)
			f.rel = make(chan struct{})
			f.spent = true
			if f.holds > 0 {
				kept = append(kept, f)
			}
			continue
		}
		kept = append(kept, f)
	}
	s.list = kept
	return found
}

// Release lets every request held by the fault go and re-arms the hold.
func (s *faultSet) Release(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.list {
		if f.ID == id {
			close(f.rel)
			f.rel = make(chan struct{})
			return true
		}
	}
	return false
}

// take returns the armed fault for route and spends one application. A fault
// on the exact route beats the "*" fault; wildcard says whether "*" applies.
func (s *faultSet) take(route string, wildcard bool) (*fault, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pass := 0; pass < 2; pass++ {
		for _, f := range s.list {
			if f.spent {
				continue
			}
			if (pass == 0 && f.Route == route) || (pass == 1 && wildcard && f.Route == "*") {
				rel := f.rel
				if f.Mode == "slow" {
					f.holds++
				}
				if f.Times > 0 {
					f.left--
					if f.left == 0 {
						f.spent = true
						s.dropIfIdle(f)
					}
				}
				return f, rel
			}
		}
	}
	return nil, nil
}

// dropIfIdle removes a spent fault once no request waits on it. The caller
// holds s.mu.
func (s *faultSet) dropIfIdle(f *fault) {
	if !f.spent || f.holds > 0 {
		return
	}
	for i, o := range s.list {
		if o == f {
			s.list = append(s.list[:i:i], s.list[i+1:]...)
			return
		}
	}
}

func (s *faultSet) emit(ctx context.Context, f *fault, action string) {
	s.log.Begin(ctx, "fakerelay.fault").Set("fault_id", f.ID).Set("route", f.Route).Set("mode", f.Mode).Set("action", action).End("ok", "", nil)
}

// wait holds until release, the delay or ctx ends, and reports whether the
// request may go on.
func (s *faultSet) wait(ctx context.Context, f *fault, rel chan struct{}) bool {
	defer func() {
		s.mu.Lock()
		f.holds--
		s.dropIfIdle(f)
		s.mu.Unlock()
	}()
	s.emit(ctx, f, "held")
	var timer <-chan time.Time
	if f.DelayMS > 0 {
		t := time.NewTimer(time.Duration(f.DelayMS) * time.Millisecond)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-rel:
	case <-timer:
	case <-ctx.Done():
		return false
	}
	s.emit(ctx, f, "released")
	return true
}

// apply runs the fault for an HTTP route. It reports whether the request is
// finished.
func (s *faultSet) apply(w http.ResponseWriter, r *http.Request, route string, wildcard bool) bool {
	f, rel := s.take(route, wildcard)
	if f == nil {
		return false
	}
	ctx := r.Context()
	switch f.Mode {
	case "down":
		s.emit(ctx, f, "applied")
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			conn.Close()
		}
		return true
	case "slow":
		return !s.wait(ctx, f, rel)
	}
	s.emit(ctx, f, "applied")
	status, body := f.Status, string(f.Body)
	if n, ok := world.FaultNames[f.Name]; ok {
		status, body = n.Status, n.Body
	}
	if strings.HasPrefix(body, "{") {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
	return true
}

// bridgeFault runs the fault for a bridge frame type.
func (s *faultSet) bridgeFault(typ string) bridge.FaultResult {
	f, rel := s.take("BRIDGE "+typ, false)
	if f == nil {
		return bridge.FaultResult{}
	}
	ctx := context.Background()
	switch f.Mode {
	case "down":
		s.emit(ctx, f, "applied")
		return bridge.FaultResult{Down: true}
	case "slow":
		s.wait(ctx, f, rel)
		return bridge.FaultResult{}
	}
	s.emit(ctx, f, "applied")
	msg := f.Name
	if msg == "" {
		msg = string(f.Body)
	}
	return bridge.FaultResult{Refuse: true, Message: msg}
}

// connTracker remembers hijacked connections by route so a down fault can close
// a WebSocket that is already open.
type connTracker struct {
	mu    sync.Mutex
	conns map[string]map[net.Conn]bool
}

func (t *connTracker) add(route string, c net.Conn) net.Conn {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conns[route] == nil {
		t.conns[route] = map[net.Conn]bool{}
	}
	t.conns[route][c] = true
	return &trackedConn{Conn: c, t: t, route: route}
}

func (t *connTracker) closeRoute(route string) {
	t.mu.Lock()
	var victims []net.Conn
	for r, set := range t.conns {
		if route == "*" || r == route {
			for c := range set {
				victims = append(victims, c)
			}
		}
	}
	t.mu.Unlock()
	for _, c := range victims {
		c.Close()
	}
}

type trackedConn struct {
	net.Conn
	t     *connTracker
	route string
}

func (c *trackedConn) Close() error {
	c.t.mu.Lock()
	delete(c.t.conns[c.route], c.Conn)
	c.t.mu.Unlock()
	return c.Conn.Close()
}
