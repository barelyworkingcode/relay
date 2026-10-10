package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/fakerelay/internal/bridge"
	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
)

type service struct {
	id, sock, token string
	routes          []string
	gen             uint64
	proxy           *httputil.ReverseProxy
}

// dispatcher is the manifest route table.
type dispatcher struct {
	mu   sync.Mutex
	svcs map[string]*service
	gen  uint64
}

func newDispatcher() *dispatcher { return &dispatcher{svcs: map[string]*service{}} }

func collides(route, served string) bool {
	rp, sp := strings.HasSuffix(route, "/"), strings.HasSuffix(served, "/")
	switch {
	case rp && sp:
		return strings.HasPrefix(served, route) || strings.HasPrefix(route, served)
	case rp:
		return strings.HasPrefix(served, route)
	case sp:
		return strings.HasPrefix(route, served)
	}
	return route == served
}

// register installs a manifest; a later one for the same id replaces it.
func (d *dispatcher) register(id string, m bridge.Manifest, sock, token string, served []string) error {
	for _, r := range m.Routes {
		for _, p := range served {
			if collides(r, p) {
				return fmt.Errorf("manifest registry: route %q collides with %q, which relay serves", r, p)
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range m.Routes {
		for oid, o := range d.svcs {
			for _, or := range o.routes {
				if oid != id && or == r {
					return fmt.Errorf("manifest registry: route %q already claimed by service %q", r, oid)
				}
			}
		}
	}
	d.gen++
	svc := &service{id: id, sock: sock, token: token, routes: m.Routes, gen: d.gen}
	svc.proxy = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme, req.URL.Host = "http", "service"
			req.Header.Set("Authorization", "Bearer "+token)
			if t := events.TraceFrom(req.Context()); t != "" {
				req.Header.Set("X-Trace-Id", t)
			}
		},
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, "unix", sock)
		}},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			WriteText(w, http.StatusBadGateway, "bad gateway")
		},
	}
	d.svcs[id] = svc
	return nil
}

func (d *dispatcher) forget(id string, gen uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.svcs[id]; s != nil && (gen == 0 || s.gen == gen) {
		delete(d.svcs, id)
	}
}

// match finds the service for a path: an exact route first, then the longest
// prefix route.
func (d *dispatcher) match(path string) (*service, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var best *service
	bestRoute := ""
	for _, s := range d.svcs {
		for _, r := range s.routes {
			if r == path {
				return s, r
			}
			if strings.HasSuffix(r, "/") && strings.HasPrefix(path, r) && len(r) > len(bestRoute) {
				best, bestRoute = s, r
			}
		}
	}
	return best, bestRoute
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	svc, route := s.disp.match(r.URL.Path)
	if svc == nil {
		WriteText(w, http.StatusNotFound, "no service registered for this path")
		return
	}
	if s.faults.apply(w, r, "PROXY "+route, false) {
		return
	}
	if websocket.IsWebSocketUpgrade(r) {
		bridgeWS(w, r, svc)
		return
	}
	svc.proxy.ServeHTTP(w, r)
}

var wsUp = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// bridgeWS relays frames between the client and a WebSocket dialed on the
// service socket. A dial failure closes the client with 1011.
func bridgeWS(w http.ResponseWriter, r *http.Request, svc *service) {
	client, err := wsUp.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	hdr := http.Header{"Authorization": {"Bearer " + svc.token}}
	if t := events.TraceFrom(r.Context()); t != "" {
		hdr.Set("X-Trace-Id", t)
	}
	d := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var nd net.Dialer
		return nd.DialContext(ctx, "unix", svc.sock)
	}}
	up, _, err := d.DialContext(r.Context(), "ws://service"+r.URL.RequestURI(), hdr)
	if err != nil {
		_ = client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "upstream unreachable"), wsDeadline())
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	pump := func(from, to *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			mt, b, err := from.ReadMessage()
			if err != nil {
				if ce, ok := err.(*websocket.CloseError); ok {
					_ = to.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(ce.Code, ce.Text), wsDeadline())
				}
				return
			}
			if to.WriteMessage(mt, b) != nil {
				return
			}
		}
	}
	go pump(client, up)
	go pump(up, client)
	<-done
}
