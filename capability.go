package main

import (
	"errors"
	"net/http"
)

// CapabilityClass names a capability by blast radius (ADR-015), never by
// which tab it appears on.
type CapabilityClass string

const (
	ClassRead      CapabilityClass = "read"
	ClassConfigure CapabilityClass = "configure"
	ClassGrant     CapabilityClass = "grant"
	ClassExecute   CapabilityClass = "execute"
)

// Transport names which listener a request arrived on.
type Transport string

const (
	TransportSocket Transport = "socket"
	TransportTCP    Transport = "tcp"
)

// Authorizer decides whether the credential on a request may exercise
// class. A nil error means allowed.
type Authorizer interface {
	Authorize(r *http.Request, class CapabilityClass) error
}

// ControlAuditor records control-plane authorization outcomes. Every
// implementation must be nil-receiver safe.
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
}

// ClassReachableOn reports whether a class may be ROUTED on a transport.
// An unknown class is reachable on nothing: this is the fail-closed default
// that keeps a class added later, without an update here, absent rather
// than universal.
func ClassReachableOn(c CapabilityClass, t Transport) bool {
	switch c {
	case ClassExecute:
		return t == TransportSocket
	case ClassRead, ClassConfigure, ClassGrant:
		return t == TransportSocket || t == TransportTCP
	default:
		return false
	}
}

// RouteRegistrar is the single door every control-plane route registration
// goes through. Authz nil means allow (tests, and the socket during
// migration); Auditor is nil-safe.
type RouteRegistrar struct {
	Mux       *http.ServeMux
	Transport Transport
	Authz     Authorizer
	Auditor   ControlAuditor
}

// controlStatus maps an Authorize refusal to the HTTP status it produces.
// An authorization decision that errors is a refusal, never a 500 — an
// error this function doesn't recognize still maps to 403 rather than
// falling through to something a caller could mistake for a server fault.
func controlStatus(err error) int {
	switch {
	case errors.Is(err, errNoCredential):
		return http.StatusUnauthorized
	case errors.Is(err, errClassNotGranted):
		return http.StatusForbidden
	default:
		return http.StatusForbidden
	}
}

// Handle registers h for pattern under class, gated by rr.Transport.
//
// This is deliberate: when the class is not reachable on this transport,
// Handle registers nothing at all — not a wrapped handler that checks and
// refuses. A pattern this function never hands to rr.Mux has no code path
// on that listener for a later change to invert, wrap, or forget (ADR-015
// decision 2); a 403 from inside a handler would still be a route someone
// could reach.
func (rr *RouteRegistrar) Handle(class CapabilityClass, pattern string, h http.HandlerFunc) {
	if !ClassReachableOn(class, rr.Transport) {
		return
	}
	rr.Mux.HandleFunc(pattern, rr.authorize(class, h))
}

func (rr *RouteRegistrar) authorize(class CapabilityClass, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var err error
		if rr.Authz != nil {
			err = rr.Authz.Authorize(r, class)
		}
		// Read back regardless of outcome: api_credential.go's Authorizer
		// only has room to hand a resolved credential id back through r's
		// context (Authorize's signature is fixed by the contract), and a
		// future Authorizer may resolve an id on some refusals too (e.g. a
		// real credential that simply lacks class) — this stays correct
		// either way instead of assuming the id is only ever set on success.
		credID, _ := APICredentialIDFromContext(r.Context())

		if err != nil {
			rr.recordDecision(r, class, credID, false, err.Error())
			status := controlStatus(err)
			http.Error(w, http.StatusText(status), status)
			return
		}

		// Recorded before h runs: this is a record of the authorization
		// decision, not of the handler's outcome, so a panic or a slow
		// handler downstream can't suppress it. frontendRecover (outside
		// this mux) still catches the panic itself.
		rr.recordDecision(r, class, credID, true, "")
		h(w, r)
	}
}

func (rr *RouteRegistrar) recordDecision(r *http.Request, class CapabilityClass, credID string, allowed bool, reason string) {
	if rr.Auditor == nil {
		return
	}
	rr.Auditor.RecordDecision(ControlDecision{
		Method:    r.Method,
		Path:      r.URL.Path,
		Class:     class,
		Transport: rr.Transport,
		CredID:    credID,
		Allowed:   allowed,
		Reason:    reason,
	})
}
