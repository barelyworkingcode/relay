package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/service"
)

// EnhancedService.proxy is built once at register time and reused for every
// dispatched request, so per-request cost is one map lookup, not a fresh dial.
type EnhancedService struct {
	ServiceID      string
	InternalSocket string
	InternalToken  string
	Manifest       bridge.Manifest
	RegisteredAt   time.Time
	proxy          *httputil.ReverseProxy
}

var dispatcherTargetURL, _ = url.Parse(service.InternalUnixHostURL)

// newServiceProxy strips inbound Authorization (the frontend's token,
// already validated) and injects the service-declared internal token.
func newServiceProxy(serviceID, internalSocket, internalToken string) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(dispatcherTargetURL)
	rp.Transport = service.NewUnixHTTPTransport(internalSocket)
	rp.FlushInterval = -1
	originalDirector := rp.Director
	rp.Director = func(req *http.Request) {
		originalDirector(req)
		req.Header.Del("Authorization")
		if internalToken != "" {
			req.Header.Set("Authorization", "Bearer "+internalToken)
		}
	}
	rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		slog.Warn("frontend dispatch: upstream error",
			"service", serviceID, "method", req.Method, "path", req.URL.Path, "error", err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
	return rp
}

// EnhancedServiceRegistry is distinct from service.Registry
// (internal/service/service_registry.go), which manages process lifecycle:
// this one covers only the protocol side -- what services expose, how to
// reach them.
type EnhancedServiceRegistry struct {
	mu       sync.RWMutex
	services map[string]*EnhancedService

	// relayRoutes is the path space relay itself serves, accumulated from
	// control.RouteRegistrar rather than written out here: a hand-maintained list
	// drifts the first time someone adds a route, and a security check that
	// has silently stopped covering half the surface is worse than none.
	relayRoutes map[string]struct{}

	// onChange fires after any successful RegisterManifest/Forget. Used by
	// the front-door dispatcher to refresh its prefix table and by the
	// settings UI to push status updates.
	onChange func()
}

func NewEnhancedServiceRegistry(onChange func()) *EnhancedServiceRegistry {
	return &EnhancedServiceRegistry{
		services: make(map[string]*EnhancedService),
		onChange: onChange,
	}
}

// RegisterManifest returns an error if any other already-registered
// service's manifest conflicts on a route. Re-registering the same
// serviceID is allowed and replaces the prior record -- the service is the
// source of truth for its own routes, address, and token.
func (r *EnhancedServiceRegistry) RegisterManifest(serviceID, internalSocket, internalToken string, m bridge.Manifest) error {
	if serviceID == "" {
		return fmt.Errorf("manifest registry: empty serviceID")
	}
	r.mu.Lock()
	if err := r.checkRouteConflictsLocked(serviceID, m.Routes); err != nil {
		r.mu.Unlock()
		return err
	}
	r.services[serviceID] = &EnhancedService{
		ServiceID:      serviceID,
		InternalSocket: internalSocket,
		InternalToken:  internalToken,
		Manifest:       m,
		RegisteredAt:   time.Now(),
		proxy:          newServiceProxy(serviceID, internalSocket, internalToken),
	}
	r.mu.Unlock()
	r.fireOnChange()
	return nil
}

func (r *EnhancedServiceRegistry) Forget(serviceID string) {
	r.mu.Lock()
	_, existed := r.services[serviceID]
	delete(r.services, serviceID)
	r.mu.Unlock()
	if existed {
		r.fireOnChange()
	}
}

// Get returns the raw pointer -- safe because records are immutable once
// registered (re-registration replaces the pointer rather than mutating it).
func (r *EnhancedServiceRegistry) Get(serviceID string) *EnhancedService {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.services[serviceID]
}

// ServeHTTP forwards to this service's own reverse proxy -- the same one
// LookupByPath's manifest-driven dispatch uses, exposed here for a caller
// (session_routes.go's bare GET /api/terminals and GET /api/sessions, SP6)
// that already knows which service it wants by id rather than by path.
func (es *EnhancedService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	es.proxy.ServeHTTP(w, r)
}

// All sorts by serviceID for stable UI iteration.
func (r *EnhancedServiceRegistry) All() []*EnhancedService {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*EnhancedService, 0, len(r.services))
	for _, rec := range r.services {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServiceID < out[j].ServiceID })
	return out
}

// LookupByPath returns the service whose manifest declares the longest
// route matching path. Routes ending in "/" are prefixes; routes not
// ending in "/" must match exactly. Hot path -- called on every dispatched
// HTTP/WS request.
func (r *EnhancedServiceRegistry) LookupByPath(path string) *EnhancedService {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var best *EnhancedService
	bestLen := -1
	for _, rec := range r.services {
		for _, route := range rec.Manifest.Routes {
			var matched bool
			if strings.HasSuffix(route, "/") {
				matched = strings.HasPrefix(path, route)
			} else {
				matched = path == route
			}
			if matched && len(route) > bestLen {
				best = rec
				bestLen = len(route)
			}
		}
	}
	return best
}

// ReserveRelayRoute records one pattern relay registers, so no manifest can
// claim a path relay already answers. Patterns arrive as http.ServeMux
// spells them ("GET /api/projects/{id}"); relayRoutePath narrows one to the
// path space it occupies.
//
// This is deliberate: the catch-all is dropped rather than reserved. "/" is
// not a path relay serves — it is the mount that reaches services — so
// reserving it would refuse every manifest route there is.
func (r *EnhancedServiceRegistry) ReserveRelayRoute(pattern string) {
	if r == nil {
		return
	}
	path := relayRoutePath(pattern)
	if path == "" || path == "/" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.relayRoutes == nil {
		r.relayRoutes = make(map[string]struct{})
	}
	r.relayRoutes[path] = struct{}{}
}

// relayRoutePath reduces an http.ServeMux pattern to the path space it
// occupies: the method is dropped, and a wildcard segment turns everything
// from it onward into a prefix.
//
// This is deliberate: truncating at the wildcard reserves the whole subtree,
// which is wider than the pattern matches. A route relay reaches by wildcard
// makes that subtree relay's namespace, and a service placing a route inside
// it survives only by http.ServeMux preferring the more specific pattern —
// the ordering accident this check exists so nothing has to rely on.
func relayRoutePath(pattern string) string {
	if i := strings.LastIndex(pattern, " "); i >= 0 {
		pattern = pattern[i+1:]
	}
	if i := strings.Index(pattern, "{"); i >= 0 {
		pattern = pattern[:i]
	}
	return pattern
}

// routesOverlap reports whether two routes can ever answer the same request.
// A route ending in "/" is a prefix and everything else is exact, which is
// the rule LookupByPath dispatches by.
//
// This is deliberate: overlap, not string equality. Equality would let a
// manifest claim "/api/" and swallow every relay route beneath it, and would
// let one claim a path relay reaches by wildcard, which relay would then win
// by specificity — a dead route rather than a refused one.
func routesOverlap(a, b string) bool {
	aPrefix := strings.HasSuffix(a, "/")
	bPrefix := strings.HasSuffix(b, "/")
	switch {
	case aPrefix && bPrefix:
		return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
	case aPrefix:
		return strings.HasPrefix(b, a)
	case bPrefix:
		return strings.HasPrefix(a, b)
	default:
		return a == b
	}
}

// collidingRelayRouteLocked returns the relay route that overlaps route, or
// "". Sorted so a route overlapping two of them always names the same one.
// Caller must hold r.mu.
func (r *EnhancedServiceRegistry) collidingRelayRouteLocked(route string) string {
	reserved := make([]string, 0, len(r.relayRoutes))
	for path := range r.relayRoutes {
		reserved = append(reserved, path)
	}
	sort.Strings(reserved)
	for _, path := range reserved {
		if routesOverlap(path, route) {
			return path
		}
	}
	return ""
}

// sessionHostSharedPrefixes are the two path prefixes C5 splits between
// relay's own routes (create, resume, the bare list proxies) and
// relay-sessions' own manifest (every other per-session operation
// underneath them, e.g. GET /api/sessions/{id}). Relay's own patterns are
// always registered directly on the mux, never through the manifest
// dispatcher's "/" catch-all, so http.ServeMux resolves a request matching
// one of them before relay-sessions' manifest is ever consulted — the two
// claims cannot actually collide at request time, only in this
// defense-in-depth check, which is why only the one service the split
// names may claim them.
var sessionHostSharedPrefixes = func() map[string]struct{} {
	m := make(map[string]struct{}, len(config.RelaySessionsManifestRoutes))
	for _, route := range config.RelaySessionsManifestRoutes {
		m[route] = struct{}{}
	}
	return m
}()

// checkRouteConflictsLocked flags any duplicate route string between two
// distinct serviceIDs, and refuses relay's own routes outright.
// Caller must hold r.mu.Lock().
//
// Two reserved checks, because they rest on different evidence. /relay/ is a
// constant: it carries the unauthenticated login ceremony, which is
// registered outside control.RouteRegistrar (ADR-016 decision 5) and so is in no
// accumulated set, and a manifest claiming it would put a service in front
// of the one door relay serves with no credential. Everything else comes
// from what control.RouteRegistrar was actually asked to register.
func (r *EnhancedServiceRegistry) checkRouteConflictsLocked(serviceID string, routes []string) error {
	for _, route := range routes {
		if route == strings.TrimSuffix(relayReservedPrefix, "/") || strings.HasPrefix(route, relayReservedPrefix) {
			return fmt.Errorf("manifest registry: route %q is reserved to relay (%s)", route, relayReservedPrefix)
		}
		if claimed := r.collidingRelayRouteLocked(route); claimed != "" {
			if _, shared := sessionHostSharedPrefixes[claimed]; shared && serviceID == config.RelaySessionsServiceID {
				continue
			}
			return fmt.Errorf("manifest registry: route %q collides with %q, which relay serves", route, claimed)
		}
	}
	for otherID, other := range r.services {
		if otherID == serviceID {
			continue
		}
		for _, otherRoute := range other.Manifest.Routes {
			for _, newRoute := range routes {
				if otherRoute == newRoute {
					return fmt.Errorf("manifest registry: route %q already claimed by service %q", newRoute, otherID)
				}
			}
		}
	}
	return nil
}

func (r *EnhancedServiceRegistry) fireOnChange() {
	if r.onChange != nil {
		r.onChange()
	}
}
