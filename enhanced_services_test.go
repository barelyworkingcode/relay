package main

import (
	"sync/atomic"
	"testing"

	"relaygo/bridge"
)

// Tests for the in-memory registry of relay-enhanced services. Covers:
//   - Register CRUD + replacement semantics
//   - Route-conflict detection
//   - Longest-prefix-match correctness
//   - Forget lifecycle
//   - onChange callback fan-out

func newManifest(routes ...string) bridge.Manifest {
	return bridge.Manifest{Routes: routes}
}

func TestEnhancedServiceRegistry_RegisterAndGet(t *testing.T) {
	r := NewEnhancedServiceRegistry(nil)
	if err := r.RegisterManifest("svc-a", "/tmp/a.sock", "token-a", newManifest("/api/a")); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	got := r.Get("svc-a")
	if got == nil {
		t.Fatal("Get returned nil for registered service")
	}
	if got.ServiceID != "svc-a" || got.InternalSocket != "/tmp/a.sock" || got.InternalToken != "token-a" {
		t.Fatalf("Get returned wrong record: %+v", got)
	}
	if got.proxy == nil {
		t.Fatal("ReverseProxy should be built at register time")
	}
}

func TestEnhancedServiceRegistry_RejectsEmptyServiceID(t *testing.T) {
	r := NewEnhancedServiceRegistry(nil)
	err := r.RegisterManifest("", "/tmp/a.sock", "token-a", newManifest("/api/a"))
	if err == nil {
		t.Fatal("expected error on empty serviceID")
	}
}

func TestEnhancedServiceRegistry_ReregisterReplaces(t *testing.T) {
	r := NewEnhancedServiceRegistry(nil)
	_ = r.RegisterManifest("svc-a", "/tmp/a-v1.sock", "tok-v1", newManifest("/api/a"))
	if err := r.RegisterManifest("svc-a", "/tmp/a-v2.sock", "tok-v2", newManifest("/api/a", "/api/a2")); err != nil {
		t.Fatalf("re-register should succeed: %v", err)
	}
	got := r.Get("svc-a")
	if got.InternalSocket != "/tmp/a-v2.sock" || got.InternalToken != "tok-v2" {
		t.Fatalf("re-register did not replace: %+v", got)
	}
	if len(got.Manifest.Routes) != 2 {
		t.Fatalf("expected 2 routes after re-register, got %d", len(got.Manifest.Routes))
	}
}

func TestEnhancedServiceRegistry_RouteConflict(t *testing.T) {
	r := NewEnhancedServiceRegistry(nil)
	_ = r.RegisterManifest("svc-a", "/tmp/a.sock", "tok-a", newManifest("/api/shared"))
	err := r.RegisterManifest("svc-b", "/tmp/b.sock", "tok-b", newManifest("/api/shared"))
	if err == nil {
		t.Fatal("expected conflict error on duplicate route across services")
	}
	// And svc-b should NOT be registered after the conflict.
	if r.Get("svc-b") != nil {
		t.Fatal("conflicting registration must not be persisted")
	}
}

func TestEnhancedServiceRegistry_RouteConflict_AllowsSameServiceReregister(t *testing.T) {
	r := NewEnhancedServiceRegistry(nil)
	_ = r.RegisterManifest("svc-a", "/tmp/a.sock", "tok-a", newManifest("/api/shared"))
	// Re-registering svc-a with the same route must NOT conflict with itself.
	if err := r.RegisterManifest("svc-a", "/tmp/a.sock", "tok-a-rotated", newManifest("/api/shared")); err != nil {
		t.Fatalf("re-register of same service should not conflict: %v", err)
	}
}

func TestEnhancedServiceRegistry_Forget(t *testing.T) {
	r := NewEnhancedServiceRegistry(nil)
	_ = r.RegisterManifest("svc-a", "/tmp/a.sock", "tok-a", newManifest("/api/a"))
	r.Forget("svc-a")
	if r.Get("svc-a") != nil {
		t.Fatal("Forget did not remove service")
	}
}

func TestEnhancedServiceRegistry_LookupByPath_LongestPrefix(t *testing.T) {
	r := NewEnhancedServiceRegistry(nil)
	// Two services with overlapping prefixes — longest must win.
	_ = r.RegisterManifest("api", "/tmp/api.sock", "t1", newManifest("/api/"))
	_ = r.RegisterManifest("sessions", "/tmp/sess.sock", "t2", newManifest("/api/sessions/"))

	cases := []struct {
		path     string
		wantSvc  string
	}{
		{"/api/foo", "api"},                          // matches /api/ only
		{"/api/sessions/123", "sessions"},            // matches both, /api/sessions/ longer
		{"/api/sessions/", "sessions"},               // exact-prefix match on sessions
		{"/api/", "api"},                             // matches /api/ exactly
		{"/unknown", ""},                             // nothing claims it
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			got := r.LookupByPath(c.path)
			gotSvc := ""
			if got != nil {
				gotSvc = got.ServiceID
			}
			if gotSvc != c.wantSvc {
				t.Fatalf("LookupByPath(%q) = %q; want %q", c.path, gotSvc, c.wantSvc)
			}
		})
	}
}

func TestEnhancedServiceRegistry_LookupByPath_ExactVsPrefix(t *testing.T) {
	// Routes WITHOUT a trailing slash are exact-match; routes WITH a
	// trailing slash are prefix-match. The dispatcher relies on this
	// distinction — verify it.
	r := NewEnhancedServiceRegistry(nil)
	_ = r.RegisterManifest("exact", "/tmp/e.sock", "te", newManifest("/api/exact"))
	_ = r.RegisterManifest("prefix", "/tmp/p.sock", "tp", newManifest("/api/prefix/"))

	if got := r.LookupByPath("/api/exact"); got == nil || got.ServiceID != "exact" {
		t.Fatalf("exact match failed; got=%v", got)
	}
	if got := r.LookupByPath("/api/exact/sub"); got != nil {
		t.Fatalf("exact route must not match sub-paths; got=%v", got.ServiceID)
	}
	if got := r.LookupByPath("/api/prefix/anything"); got == nil || got.ServiceID != "prefix" {
		t.Fatalf("prefix match failed; got=%v", got)
	}
	if got := r.LookupByPath("/api/prefix"); got != nil {
		// "/api/prefix/" requires the trailing slash to match
		t.Fatalf("prefix route should require the trailing slash; got=%v", got.ServiceID)
	}
}

func TestEnhancedServiceRegistry_All_SortedByServiceID(t *testing.T) {
	r := NewEnhancedServiceRegistry(nil)
	_ = r.RegisterManifest("c", "/tmp/c.sock", "t", newManifest("/c"))
	_ = r.RegisterManifest("a", "/tmp/a.sock", "t", newManifest("/a"))
	_ = r.RegisterManifest("b", "/tmp/b.sock", "t", newManifest("/b"))
	all := r.All()
	if len(all) != 3 {
		t.Fatalf("expected 3 services, got %d", len(all))
	}
	for i, want := range []string{"a", "b", "c"} {
		if all[i].ServiceID != want {
			t.Fatalf("All()[%d] = %q; want %q", i, all[i].ServiceID, want)
		}
	}
}

func TestEnhancedServiceRegistry_OnChange_FiresOnRegister(t *testing.T) {
	var calls atomic.Int32
	r := NewEnhancedServiceRegistry(func() { calls.Add(1) })

	_ = r.RegisterManifest("svc-a", "/tmp/a.sock", "t", newManifest("/a"))
	if got := calls.Load(); got != 1 {
		t.Fatalf("onChange should fire once per register; got %d", got)
	}
	_ = r.RegisterManifest("svc-b", "/tmp/b.sock", "t", newManifest("/b"))
	if got := calls.Load(); got != 2 {
		t.Fatalf("onChange should fire once per register; got %d", got)
	}
}

func TestEnhancedServiceRegistry_OnChange_FiresOnForget_OnlyIfExisted(t *testing.T) {
	var calls atomic.Int32
	r := NewEnhancedServiceRegistry(func() { calls.Add(1) })
	_ = r.RegisterManifest("svc-a", "/tmp/a.sock", "t", newManifest("/a"))
	calls.Store(0) // ignore register fire

	r.Forget("svc-a")
	if got := calls.Load(); got != 1 {
		t.Fatalf("onChange should fire on Forget of existing service; got %d", got)
	}
	r.Forget("svc-a") // already gone
	if got := calls.Load(); got != 1 {
		t.Fatalf("onChange must NOT fire on Forget of unknown service; got %d", got)
	}
}

func TestEnhancedServiceRegistry_OnChange_FiresOnConflictFailure(t *testing.T) {
	// Verifies that a failed register does NOT call onChange (regression
	// guard: a UI rebuild on a no-op register is wasted work).
	var calls atomic.Int32
	r := NewEnhancedServiceRegistry(func() { calls.Add(1) })
	_ = r.RegisterManifest("svc-a", "/tmp/a.sock", "t", newManifest("/api/shared"))
	calls.Store(0)
	_ = r.RegisterManifest("svc-b", "/tmp/b.sock", "t", newManifest("/api/shared"))
	if got := calls.Load(); got != 0 {
		t.Fatalf("onChange must NOT fire on failed register; got %d", got)
	}
}
