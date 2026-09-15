package main

// plan-broker-and-sessions.md §2 C1, "Route classes (socket-only)": these
// routes do not exist yet (R-S3 and R-S4b register the actual handlers), so
// this asserts the class-to-route mapping sessionRouteClasses pins, and that
// a frontend launch identity — which reaches every OTHER class the frontend
// socket serves (control.ClassRead, ClassConfigure, ClassProxy;
// TestFrontendCapability_HoldsExactlyReadConfigureAndProxy) — never reaches
// an execute-class route, hermetically, with no live route to hit.

import (
	"net/http/httptest"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/service"
)

func TestSessionRouteClasses_MatchesTheC1Table(t *testing.T) {
	want := map[string]control.CapabilityClass{
		"POST /api/terminals":              control.ClassExecute,
		"POST /api/sessions":               control.ClassExecute,
		"POST /api/sessions/{id}/resume":   control.ClassExecute,
		"GET /api/terminal/templates":      control.ClassRead,
		"GET /api/terminal/templates/{id}": control.ClassRead,
		"GET /api/terminals":               control.ClassProxy,
		"GET /api/sessions":                control.ClassProxy,
	}
	if len(sessionRouteClasses) != len(want) {
		t.Fatalf("sessionRouteClasses has %d entries, want %d: %v", len(sessionRouteClasses), len(want), sessionRouteClasses)
	}
	for route, class := range want {
		if got, ok := sessionRouteClass(splitRouteKey(route)); !ok || got != class {
			t.Errorf("%s: class = %v, ok = %v, want %v", route, got, ok, class)
		}
	}
	if _, ok := sessionRouteClass("DELETE", "/api/terminals"); ok {
		t.Error("a method/path pair C1 never names was found in the table")
	}
}

// splitRouteKey turns "METHOD /path" back into the two arguments
// sessionRouteClass takes, so the table above can be written the same way
// the plan document writes it.
func splitRouteKey(route string) (method, path string) {
	for i := 0; i < len(route); i++ {
		if route[i] == ' ' {
			return route[:i], route[i+1:]
		}
	}
	return route, ""
}

// TestSessionRouteClasses_FrontendIdentityNeverReachesAnExecuteClassRoute is
// the required hermetic proof that a frontend launch identity — the identity
// eve and relayScheduler hold on the frontend socket — cannot reach any
// route this table marks execute-class, while it does reach the read- and
// proxy-class ones, exactly mirroring frontendConsumerClasses (control.ClassRead,
// ClassConfigure, ClassProxy; never ClassGrant or ClassExecute). No live
// route exists yet to call this against — R-S3/R-S4b's registration is what
// makes that live — so this asks control.Authorizer.Authorize directly, the
// same chokepoint every frontend-socket route (present or future) must pass
// through.
func TestSessionRouteClasses_FrontendIdentityNeverReachesAnExecuteClassRoute(t *testing.T) {
	authz := NewCredentialAuthorizer(newCLISandboxStore(t))
	id := service.Identity{Kind: service.IdentityKindService, Name: "eve-like", Capabilities: []config.ServiceCapability{config.ServiceCapabilityFrontend}}

	for route, class := range sessionRouteClasses {
		r := httptest.NewRequest("GET", "/x", nil)
		r = r.WithContext(withFrontendIdentity(r.Context(), id))
		err := authz.Authorize(r, class)
		wantGranted := class != control.ClassExecute
		if granted := err == nil; granted != wantGranted {
			t.Errorf("%s (class %s): granted = %v, want %v (err=%v)", route, class, granted, wantGranted, err)
		}
	}
}
