package main

import (
	"fmt"
	"sync"

	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
)

// modelHost is one registered upstream, together with the launch identity's
// process, so liveness is re-derived rather than tracked as a stored bool.
type modelHost struct {
	serviceID    string
	routerSocket string
	process      peertoken.Process
}

// ModelHostRegistry holds at most one live model-endpoint upstream
// (docs/model-endpoint.md, plan-broker-and-sessions.md C8). "Live" is
// answered fresh against service.Launches on every call: the launch table,
// not this registry, is the source of truth for whether a registration's
// process still holds its identity, so a crashed host is reflected here the
// instant its launch ends, with no separate teardown call needed.
type ModelHostRegistry struct {
	launches *service.Launches

	mu      sync.Mutex
	current *modelHost
}

// NewModelHostRegistry builds a registry that checks liveness against
// launches. A nil launches means every registration reads as never live,
// same fail-closed shape the rest of the launch-identity protocol uses.
func NewModelHostRegistry(launches *service.Launches) *ModelHostRegistry {
	return &ModelHostRegistry{launches: launches}
}

func (r *ModelHostRegistry) liveLocked(h *modelHost) bool {
	if h == nil || r.launches == nil {
		return false
	}
	id, ok := r.launches.Bound(h.serviceID)
	return ok && id.Process == h.process
}

// Register records serviceID/routerSocket/process as the model endpoint's
// upstream. Refused while a prior registration's launch is still live —
// "a new registration replaces the old only after the old launch has
// ended" (docs/model-endpoint.md) — whether the new attempt names the same
// service id or a different one: relayLLM registers exactly once, right
// after Hello, so a second registration from a still-live launch has no
// legitimate cause and is refused rather than silently accepted as an
// update.
func (r *ModelHostRegistry) Register(serviceID, routerSocket string, process peertoken.Process) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.liveLocked(r.current) {
		return fmt.Errorf("model host already registered by %q; it must end its launch before another may register", r.current.serviceID)
	}
	r.current = &modelHost{serviceID: serviceID, routerSocket: routerSocket, process: process}
	return nil
}

// Current returns the live host's coordinates, or ok=false when none is
// registered or the registered launch has ended — the model endpoint reads
// the latter as "no host" (503), not as a stale record to fall back on.
func (r *ModelHostRegistry) Current() (serviceID, routerSocket string, process peertoken.Process, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.liveLocked(r.current) {
		return "", "", peertoken.Process{}, false
	}
	return r.current.serviceID, r.current.routerSocket, r.current.process, true
}
