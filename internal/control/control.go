// Package control defines the transport and authorization boundary for relay's
// HTTP control plane.
package control

import (
	"context"
	"errors"
	"net/http"
)

// CapabilityClass names a capability by blast radius, never by which tab it
// appears on.
type CapabilityClass string

const (
	ClassRead      CapabilityClass = "read"
	ClassConfigure CapabilityClass = "configure"
	ClassGrant     CapabilityClass = "grant"
	ClassExecute   CapabilityClass = "execute"
	ClassProxy     CapabilityClass = "proxy"
	// ClassChiefOfStaff is held only inside the Chief of Staff request scope,
	// never by a credential, and is reachable on the socket alone.
	ClassChiefOfStaff CapabilityClass = "chief_of_staff"
)

// Transport names which listener a request arrived on.
type Transport string

const (
	TransportSocket Transport = "socket"
	TransportTCP    Transport = "tcp"
	// TransportBridge appears only in audit records and the doors document.
	// ClassReachableOn does not route on it: a class is never registered on
	// the bridge through a RouteRegistrar.
	TransportBridge Transport = "bridge"
)

// ErrNoCredential and ErrClassNotGranted are the authorization outcomes with
// distinct HTTP responses. Any other authorization error is fail-closed as
// forbidden.
var (
	ErrNoCredential    = errors.New("no credential")
	ErrClassNotGranted = errors.New("class not granted")
	ErrOutsideScope    = errors.New("outside chief-of-staff scope")
	ErrUnknownScope    = errors.New("unknown scope")
)

// Authorizer decides whether the credential on a request may exercise class.
type Authorizer interface {
	Authorize(r *http.Request, class CapabilityClass) error
}

// ControlAuditor records control-plane authorization outcomes.
type ControlAuditor interface {
	RecordDecision(d ControlDecision)
}

// ControlDecision is one authorization outcome on the control plane.
type ControlDecision struct {
	Method    string
	Path      string
	Class     CapabilityClass
	Transport Transport
	CredID    string
	Allowed   bool
	Reason    string

	ClientID    string
	Fingerprint string
}

// ClassReachableOn reports whether a class may be routed on a transport. An
// unknown class is reachable nowhere, the fail-closed default.
func ClassReachableOn(c CapabilityClass, t Transport) bool {
	switch c {
	case ClassExecute, ClassProxy, ClassChiefOfStaff:
		return t == TransportSocket
	case ClassRead, ClassConfigure, ClassGrant:
		return t == TransportSocket || t == TransportTCP
	default:
		return false
	}
}

// RouteReserver is told every pattern relay registers, so another router can
// refuse to shadow one.
type RouteReserver interface {
	ReserveRelayRoute(pattern string)
}

// CredentialID reads the already-resolved credential identity from request
// context. It keeps relay's context representation outside this package.
type CredentialID func(context.Context) (string, bool)

// RouteRegistrar is the single door for control-plane route registration.
// Nil Authz allows, and nil Auditor or Reserve disables its respective side
// effect.
type RouteRegistrar struct {
	Mux          *http.ServeMux
	Transport    Transport
	Authz        Authorizer
	Auditor      ControlAuditor
	Reserve      RouteReserver
	CredentialID CredentialID
	// Record is told every route that is actually registered, after the
	// reachability check, so the doors document lists what a mux serves and
	// nothing it refused to register.
	Record func(RouteInfo)
}

// RouteInfo describes one registered route. Gates names the presence
// operations its handler may require.
type RouteInfo struct {
	Pattern   string
	Class     CapabilityClass
	Transport Transport
	Gates     []string
}

// AuthorizationStatus maps an authorization refusal to its HTTP response.
// Authorization errors are refusals, never server faults.
func AuthorizationStatus(err error) int {
	if errors.Is(err, ErrNoCredential) {
		return http.StatusUnauthorized
	}
	return http.StatusForbidden
}

// Handle registers h for pattern under class, gated by Transport. A route
// unreachable on this transport is not registered at all.
func (rr *RouteRegistrar) Handle(class CapabilityClass, pattern string, h http.HandlerFunc) {
	rr.HandleGated(class, nil, pattern, h)
}

// HandleGated is Handle for a route whose handler may require a presence
// operation. gates is documentation for the doors document: the gate itself
// stays in the core the handler calls.
func (rr *RouteRegistrar) HandleGated(class CapabilityClass, gates []string, pattern string, h http.HandlerFunc) {
	if rr.Reserve != nil {
		rr.Reserve.ReserveRelayRoute(pattern)
	}
	if !ClassReachableOn(class, rr.Transport) {
		return
	}
	rr.Mux.HandleFunc(pattern, rr.authorize(class, h))
	if rr.Record != nil {
		rr.Record(RouteInfo{Pattern: pattern, Class: class, Transport: rr.Transport, Gates: gates})
	}
}

func (rr *RouteRegistrar) authorize(class CapabilityClass, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var err error
		if rr.Authz != nil {
			err = rr.Authz.Authorize(r, class)
		}
		credID := ""
		if rr.CredentialID != nil {
			credID, _ = rr.CredentialID(r.Context())
		}
		if err != nil {
			rr.recordDecision(r, class, credID, false, err.Error())
			status := AuthorizationStatus(err)
			http.Error(w, http.StatusText(status), status)
			return
		}
		rr.recordDecision(r, class, credID, true, "")
		h(w, r)
	}
}

func (rr *RouteRegistrar) recordDecision(r *http.Request, class CapabilityClass, credID string, allowed bool, reason string) {
	if rr.Auditor == nil {
		return
	}
	rr.Auditor.RecordDecision(ControlDecision{
		Method: r.Method, Path: r.URL.Path, Class: class, Transport: rr.Transport,
		CredID: credID, Allowed: allowed, Reason: reason,
	})
}
