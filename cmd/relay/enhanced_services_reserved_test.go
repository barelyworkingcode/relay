package main

// Issue #50, second half: checkRouteConflictsLocked checked services against
// each other and never against relay's own patterns, so nothing but
// http.ServeMux's preference for the more specific pattern stopped a
// manifest claiming /api/projects. These tests pin the check, and pin that
// the set it checks against is derived from what control.RouteRegistrar was actually
// asked to register rather than written down beside it.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/control"
)

// esrRegistryFromRealRoutes returns the registry a real FrontendServer
// registered its whole route set through — the socket mux and the TCP mux
// both, exactly as production builds them.
func esrRegistryFromRealRoutes(t *testing.T) *EnhancedServiceRegistry {
	t.Helper()
	ts := teNewServer(t, newCLISandboxStore(t), nil)
	return ts.srv.routeDeps.enhanced
}

func TestReservedRoutes_ManifestCannotClaimARouteRelayServes(t *testing.T) {
	reg := esrRegistryFromRealRoutes(t)

	for _, route := range []string{"/api/projects", "/api/services", "/api/mcps", "/api/audit", "/api/enrolments", "/api/remote"} {
		err := reg.RegisterManifest("squatter", "/tmp/squatter.sock", "tok", newManifest(route))
		if err == nil {
			t.Fatalf("a manifest claiming %q was accepted", route)
		}
		if !strings.Contains(err.Error(), route) {
			t.Fatalf("the refusal for %q does not name the route: %v", route, err)
		}
		if !strings.Contains(err.Error(), "relay serves") {
			t.Fatalf("the refusal for %q does not say the route belongs to relay: %v", route, err)
		}
		if reg.Get("squatter") != nil {
			t.Fatalf("a refused manifest claiming %q was registered anyway", route)
		}
	}
}

// Every route relay actually registers must be refused, not a sample of
// them: the check is a claim about the whole surface.
func TestReservedRoutes_EveryRouteInRelaysTableIsRefused(t *testing.T) {
	reg := esrRegistryFromRealRoutes(t)
	ids := teFixtureIDs{projID: "p1", projDelID: "p2"}

	for _, r := range teRouteTable(ids) {
		if err := reg.RegisterManifest("squatter", "/tmp/squatter.sock", "tok", newManifest(r.path)); err == nil {
			t.Fatalf("a manifest claiming %q, which relay serves, was accepted", r.path)
		}
	}
}

// A prefix route that would swallow relay's routes is the collision string
// equality misses, and the one an attacker-shaped manifest would use.
func TestReservedRoutes_ASwallowingPrefixIsRefused(t *testing.T) {
	reg := esrRegistryFromRealRoutes(t)

	for _, route := range []string{"/api/", "/", "/api/projects/", "/api/services/svc1"} {
		if err := reg.RegisterManifest("squatter", "/tmp/squatter.sock", "tok", newManifest(route)); err == nil {
			t.Fatalf("a manifest claiming %q was accepted", route)
		}
	}
}

// The catch-all is registered at "/" on the socket and is deliberately not
// reserved: it is the mount that reaches services, not a path relay serves.
// Reserving it would refuse every enhanced service there is.
func TestReservedRoutes_LegitimateServicesStillRegister(t *testing.T) {
	reg := esrRegistryFromRealRoutes(t)

	if err := reg.RegisterManifest("relayScheduler", "/tmp/sched.sock", "tok", newManifest("/api/tasks/", "/api/tasks")); err != nil {
		t.Fatalf("relayScheduler's routes were refused: %v", err)
	}
}

// This is deliberate, not a regression: relayllm.json's committed manifest
// still claims the broad "/api/terminal/" prefix (relayLLM/internal/relay/manifest.go),
// which now overlaps GET /api/terminal/templates[/{id}] — R-S3 moved those
// two routes to relay itself (plan-broker-and-sessions.md §2 C1). Registering
// relayLLM's *current* manifest is therefore correctly refused until
// relayLLM's own retirement unit (L-S1) narrows "/api/terminal/" away from
// the routes relay now serves; this test pins that the refusal names the
// real overlapping route rather than silently swallowing it.
func TestReservedRoutes_RelayLLMManifestOverlapsTemplateRoutesUntilLS1(t *testing.T) {
	reg := esrRegistryFromRealRoutes(t)

	err := reg.RegisterManifest("relayLLM", "/tmp/llm.sock", "tok", loadManifestFixture(t, "relayllm.json"))
	if err == nil {
		t.Fatal("relayLLM's manifest was accepted despite claiming /api/terminal/, which relay now serves under it")
	}
	if !strings.Contains(err.Error(), "/api/terminal/") {
		t.Fatalf("refusal does not name the overlapping route: %v", err)
	}
}

// /relay/ is refused by a constant, not by the accumulated set: the login
// routes are registered outside control.RouteRegistrar and so appear in no set. The
// two checks share a loop, and this pins that the constant still answers
// first and in its own words.
func TestReservedRoutes_RelayPrefixIsStillRefusedInItsOwnWords(t *testing.T) {
	reg := esrRegistryFromRealRoutes(t)

	for _, route := range []string{"/relay/", "/relay", "/relay/login"} {
		err := reg.RegisterManifest("squatter", "/tmp/squatter.sock", "tok", newManifest(route))
		if err == nil {
			t.Fatalf("a manifest claiming %q was accepted", route)
		}
		if !strings.Contains(err.Error(), "reserved to relay") {
			t.Fatalf("the refusal for %q lost the reserved-prefix wording: %v", route, err)
		}
	}
}

// The set is accumulated from registration, so a pattern no production file
// mentions is reserved the moment control.RouteRegistrar sees it. A hand-maintained
// list cannot pass this.
func TestReservedRoutes_TrackRegistrationRatherThanAList(t *testing.T) {
	reg := NewEnhancedServiceRegistry(nil)
	const novel = "/api/a-route-no-manifest-check-was-written-for"

	if err := reg.RegisterManifest("early", "/tmp/early.sock", "tok", newManifest(novel)); err != nil {
		t.Fatalf("an unregistered path was refused before relay claimed it: %v", err)
	}
	reg.Forget("early")

	rr := &control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: http.NewServeMux(), Transport: control.TransportSocket, Reserve: reg}
	rr.Handle(control.ClassRead, "GET "+novel, func(w http.ResponseWriter, _ *http.Request) {})

	err := reg.RegisterManifest("late", "/tmp/late.sock", "tok", newManifest(novel))
	if err == nil {
		t.Fatalf("%q was not reserved after relay registered it", novel)
	}
	if !strings.Contains(err.Error(), novel) || !strings.Contains(err.Error(), "relay serves") {
		t.Fatalf("the refusal does not name the route as relay's: %v", err)
	}
}

// A route absent from a listener is still a route relay serves, so the
// reservation happens before control.RouteRegistrar's transport check — otherwise
// building only the TCP mux would leave every execute-class path claimable.
func TestReservedRoutes_AnExecuteRouteAbsentFromTCPIsStillReserved(t *testing.T) {
	reg := NewEnhancedServiceRegistry(nil)
	const novel = "/api/an-execute-route-tcp-never-carries"

	mux := http.NewServeMux()
	rr := &control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportTCP, Reserve: reg}
	rr.Handle(control.ClassExecute, "POST "+novel, func(w http.ResponseWriter, _ *http.Request) {})

	probe, err := http.NewRequest(http.MethodPost, "http://unix"+novel, nil)
	assertNoErr(t, err, "new request")
	if _, pattern := mux.Handler(probe); pattern != "" {
		t.Fatalf("an execute-class route reached the TCP mux under pattern %q", pattern)
	}
	if err := reg.RegisterManifest("squatter", "/tmp/squatter.sock", "tok", newManifest(novel)); err == nil {
		t.Fatalf("a manifest claimed %q, which relay serves on the socket", novel)
	}
}

// A registry nothing ever registered a relay route through refuses nothing
// new, so every existing caller that builds one standalone is unaffected.
func TestReservedRoutes_AnEmptySetRefusesNothing(t *testing.T) {
	reg := NewEnhancedServiceRegistry(nil)
	if err := reg.RegisterManifest("svc", "/tmp/svc.sock", "tok", newManifest("/api/", "/api/projects")); err != nil {
		t.Fatalf("an unpopulated reserved set refused a route: %v", err)
	}
}

func TestReservedRoutes_RelayRoutePathNarrowsAPattern(t *testing.T) {
	cases := map[string]string{
		"GET /api/projects":                    "/api/projects",
		"GET /api/projects/{id}":               "/api/projects/",
		"GET /api/mcps/{id}/tools":             "/api/mcps/",
		"POST /api/projects/{id}/rotate_token": "/api/projects/",
		"/":                                    "/",
	}
	for pattern, want := range cases {
		if got := relayRoutePath(pattern); got != want {
			t.Fatalf("relayRoutePath(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestReservedRoutes_OverlapIsNotStringEquality(t *testing.T) {
	overlapping := [][2]string{
		{"/api/projects", "/api/projects"},
		{"/api/projects", "/api/"},
		{"/api/projects/", "/api/projects/svc"},
		{"/api/projects/", "/api/"},
	}
	for _, pair := range overlapping {
		if !routesOverlap(pair[0], pair[1]) {
			t.Fatalf("routesOverlap(%q, %q) = false", pair[0], pair[1])
		}
	}
	disjoint := [][2]string{
		{"/api/projects", "/api/projects/"},
		{"/api/projects", "/api/tasks"},
		{"/api/projects/", "/api/tasks/"},
		{"/api/projects", "/api/projectsx"},
	}
	for _, pair := range disjoint {
		if routesOverlap(pair[0], pair[1]) {
			t.Fatalf("routesOverlap(%q, %q) = true", pair[0], pair[1])
		}
	}
}

// The refusal must be the same one whichever reserved route the map happened
// to yield first.
func TestReservedRoutes_RefusalIsDeterministic(t *testing.T) {
	reg := esrRegistryFromRealRoutes(t)
	deadline := time.Now().Add(2 * time.Second)
	first := ""
	for i := 0; time.Now().Before(deadline) && i < 200; i++ {
		err := reg.RegisterManifest("squatter", "/tmp/squatter.sock", "tok", newManifest("/api/"))
		if err == nil {
			t.Fatal("a manifest claiming /api/ was accepted")
		}
		if first == "" {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("the refusal varies between runs:\n%s\n%s", first, err.Error())
		}
	}
}
