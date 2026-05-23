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

// EnhancedService is a single relay-enhanced service's runtime record.
// All fields land in one write — the service tells relay where to reach it
// AND what it exposes in a single RegisterManifest bridge call.
//
// The proxy field is built once at register time and reused for every
// dispatched HTTP request to this service — it owns a connection-pooling
// http.Transport, so per-request cost is one map lookup instead of a
// fresh socket dial.
type EnhancedService struct {
	ServiceID      string
	InternalSocket string
	InternalToken  string
	Manifest       bridge.Manifest
	RegisteredAt   time.Time
	proxy          *httputil.ReverseProxy
}

// dispatcherTargetURL is a placeholder URL parsed once; the actual address
// is set by the DialContext in newServiceProxy below.
var dispatcherTargetURL, _ = url.Parse("http://internal.relay.localsocket")

// newServiceProxy builds the reverse proxy used to forward HTTP requests
// to this service. Strips inbound Authorization (the frontend's token,
// already validated) and injects the service-declared internal token.
func newServiceProxy(serviceID, internalSocket, internalToken string) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(dispatcherTargetURL)
	rp.Transport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", internalSocket)
		},
	}
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

// EnhancedServiceRegistry holds the runtime state of every relay-enhanced
// service. One writer (the bridge handler at manifest registration), one
// reader (the front-door dispatcher), plus the lifecycle hook (Forget on
// bridge disconnect or process exit).
//
// Distinct from `ServiceRegistry` (service_registry.go) which manages
// process lifecycle. This registry only concerns the *protocol* side of
// enhanced services — what they expose, how to reach them.
type EnhancedServiceRegistry struct {
	mu       sync.RWMutex
	services map[string]*EnhancedService

	// onChange fires after any successful RegisterManifest/Forget. Used by
	// the front-door dispatcher to refresh its prefix table and by the
	// settings UI to push status updates.
	onChange func()
}

// NewEnhancedServiceRegistry returns an empty registry. onChange may be nil.
func NewEnhancedServiceRegistry(onChange func()) *EnhancedServiceRegistry {
	return &EnhancedServiceRegistry{
		services: make(map[string]*EnhancedService),
		onChange: onChange,
	}
}

// RegisterManifest stores a service's full record (internal socket + token +
// manifest) in one shot. Returns an error if any other already-registered
// service's manifest conflicts on a route. Re-registering the same
// serviceID is allowed and replaces the prior record — the service is the
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

// Forget drops a service from the registry. Called when the bridge
// connection to the service closes or when relay stops the service.
func (r *EnhancedServiceRegistry) Forget(serviceID string) {
	r.mu.Lock()
	_, existed := r.services[serviceID]
	delete(r.services, serviceID)
	r.mu.Unlock()
	if existed {
		r.fireOnChange()
	}
}

// Get returns the record for one service, or nil if unknown. Records are
// immutable once registered (re-registration replaces the pointer), so
// returning the raw pointer is safe and avoids per-call allocation.
func (r *EnhancedServiceRegistry) Get(serviceID string) *EnhancedService {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.services[serviceID]
}

// All returns every service record, sorted by serviceID for stable UI
// iteration. The slice is freshly allocated but the element pointers are
// shared with the registry (records are immutable).
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
// route matching the request path, or nil if none. Routes ending in "/"
// are treated as prefixes; routes not ending in "/" are exact matches.
// Hot path — called on every dispatched HTTP/WS request.
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

// checkRouteConflictsLocked walks every other service's routes looking for
// exact-string collisions with the new service's routes. Caller must hold
// r.mu.Lock().
//
// V1 conflict policy: any duplicate route string between two distinct
// serviceIDs is a conflict. Future work could allow finer-grained overlap
// (e.g. "/api/sessions/active" inside "/api/sessions/"), but for now we
// keep it simple and predictable.
func (r *EnhancedServiceRegistry) checkRouteConflictsLocked(serviceID string, routes []string) error {
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
