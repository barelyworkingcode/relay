package server

import (
	"bufio"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
)

type pidKey struct{}

// Route installs a door. The socket mux gets every class; the TCP mux only the
// classes that may cross it, so execute, proxy and chief_of_staff routes are
// absent there rather than refused.
func (s *Server) Route(class Class, pattern string, h http.HandlerFunc) {
	s.mu.Lock()
	s.patterns[pattern] = true
	if p := relayPath(pattern); p != "/" {
		s.relayPaths = append(s.relayPaths, p)
	}
	s.mu.Unlock()
	s.sockMux.Handle(pattern, s.guard(class, pattern, "socket", h))
	switch class {
	case ClassRead, ClassConfigure, ClassGrant:
		s.tcpMux.Handle(pattern, s.guard(class, pattern, "tcp", h))
	}
}

func (s *Server) Verb(name string, h VerbHandler) {
	s.mu.Lock()
	s.verbs[name] = h
	s.mu.Unlock()
}

func (s *Server) Control(pattern string, h http.HandlerFunc) {
	if !strings.Contains(pattern, "/v1/") {
		panic("server: control pattern " + pattern + " is not under /v1/")
	}
	s.ctlMux.HandleFunc(pattern, h)
}

// relayPath is the part of a pattern's path a manifest may not claim: it ends
// at the first wildcard, so a wildcard reserves its whole subtree.
func relayPath(pattern string) string {
	p := pattern[strings.LastIndex(pattern, " ")+1:]
	if i := strings.Index(p, "{"); i >= 0 {
		p = p[:i]
	}
	return p
}

func (s *Server) knownRoute(route string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.patterns[route]
}

// cosRoutes are the only routes the chief-of-staff scope reaches.
var cosRoutes = map[string]bool{
	"GET /api/sessions": true, "GET /ws": true,
	"POST /api/chief-of-staff/messages": true, "POST /api/chief-of-staff/sessions": true,
}

func (s *Server) guard(class Class, pattern, transport string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := events.WithTrace(r.Context(), r.Header.Get("X-Trace-Id"))
		caller, ok := s.authenticate(r, transport)
		if !ok {
			WriteText(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if authorize(&caller, class, pattern, r) != "" {
			WriteText(w, http.StatusForbidden, "Forbidden")
			return
		}
		r = r.WithContext(withCaller(ctx, caller))
		tw := &trackWriter{ResponseWriter: w, track: s.faults.track, route: pattern}
		if s.faults.apply(tw, r, pattern, true) {
			return
		}
		h(tw, r)
	})
}

func authorize(c *Caller, class Class, pattern string, r *http.Request) string {
	if vals := r.Header.Values("X-Relay-Scope"); len(vals) > 0 {
		switch {
		case len(vals) != 1 || vals[0] != scopeChiefOfStaff:
			return "unknown scope"
		case !c.has(ClassProxy):
			return "class not granted"
		case !cosRoutes[pattern]:
			return "outside chief-of-staff scope"
		}
		c.Scope = scopeChiefOfStaff
		return ""
	}
	if class == ClassChiefOfStaff || !c.has(class) {
		return "class not granted"
	}
	return ""
}

func (c Caller) has(class Class) bool {
	for _, x := range c.Classes {
		if x == class {
			return true
		}
	}
	return false
}

// authenticate resolves the caller. An Authorization header is always judged
// as a bearer; without one the frontend socket falls back to the peer's launch
// identity. Neither resolving is a 401.
func (s *Server) authenticate(r *http.Request, transport string) (Caller, bool) {
	if vals := r.Header.Values("Authorization"); len(vals) > 0 {
		tok, ok := strings.CutPrefix(vals[0], "Bearer ")
		if !ok || len(vals) != 1 {
			return Caller{}, false
		}
		for _, c := range s.world.Credentials {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(c.Token)) != 1 {
				continue
			}
			if c.Expires != "" && s.expired(c.Expires) {
				return Caller{}, false
			}
			cl := make([]Class, len(c.Classes))
			for i, x := range c.Classes {
				cl[i] = Class(x)
			}
			return Caller{Kind: "bearer", CredID: c.ID, Classes: cl, Transport: transport}, true
		}
		return Caller{}, false
	}
	if transport != "socket" {
		return Caller{}, false
	}
	pid, _ := r.Context().Value(pidKey{}).(int)
	id, ok := s.launch.IdentityByPID(pid)
	if !ok || !id.Has("frontend") {
		return Caller{}, false
	}
	return Caller{Kind: "identity", ServiceID: id.ServiceID, CredID: "launch:service:" + id.ServiceID,
		Classes: []Class{ClassRead, ClassConfigure, ClassProxy, ClassExecute}, Transport: transport}, true
}

// trackWriter records a hijacked connection under its route, for down faults.
type trackWriter struct {
	http.ResponseWriter
	track *connTracker
	route string
}

func (t *trackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	c, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return t.track.add(t.route, c), rw, nil
}

func (t *trackWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *trackWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }
