package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"relaygo/bridge"
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

// internalUnixHostURL is a placeholder host: DialContext ignores it (we
// always dial a Unix socket) but net/url and net/http both need *something*
// parseable.
const internalUnixHostURL = "http://internal.relay.localsocket"

var dispatcherTargetURL, _ = url.Parse(internalUnixHostURL)

// newUnixHTTPTransport pins DialContext to one Unix socket. IdleConnTimeout
// keeps a per-tick transport (the status poller builds one per service per
// tick) from leaking its idle conn until GC. ResponseHeaderTimeout is
// deliberately unset: the reverse proxy forwards long-poll routes (e.g.
// permission prompts) that legitimately withhold headers for minutes.
func newUnixHTTPTransport(socket string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		IdleConnTimeout: 90 * time.Second,
	}
}

// newServiceProxy strips inbound Authorization (the frontend's token,
// already validated) and injects the service-declared internal token.
func newServiceProxy(serviceID, internalSocket, internalToken string) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(dispatcherTargetURL)
	rp.Transport = newUnixHTTPTransport(internalSocket)
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

// EnhancedServiceRegistry is distinct from ServiceRegistry
// (service_registry.go), which manages process lifecycle: this one covers
// only the protocol side -- what services expose, how to reach them.
type EnhancedServiceRegistry struct {
	mu       sync.RWMutex
	services map[string]*EnhancedService

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
			matched := false
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

// checkRouteConflictsLocked flags any duplicate route string between two
// distinct serviceIDs, and refuses relay's own reserved prefix outright.
// Caller must hold r.mu.Lock().
//
// The reserved check runs against relay's patterns, not against another
// service's: /relay/ carries the unauthenticated login ceremony, and a
// manifest that could claim it would put a service in front of the one door
// relay serves with no credential (ADR-016 decision 4). This closes only the
// /relay/ half of issue #50 — the general problem, that a service can claim
// any other path relay serves, is still open.
func (r *EnhancedServiceRegistry) checkRouteConflictsLocked(serviceID string, routes []string) error {
	for _, route := range routes {
		if route == strings.TrimSuffix(relayReservedPrefix, "/") || strings.HasPrefix(route, relayReservedPrefix) {
			return fmt.Errorf("manifest registry: route %q is reserved to relay (%s)", route, relayReservedPrefix)
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
