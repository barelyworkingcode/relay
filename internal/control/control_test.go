package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

type testCredentialKey struct{}

type testAuthorizer struct {
	err    error
	credID string
}

func (a testAuthorizer) Authorize(r *http.Request, _ CapabilityClass) error {
	*r = *r.WithContext(context.WithValue(r.Context(), testCredentialKey{}, a.credID))
	return a.err
}

func testCredentialID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(testCredentialKey{}).(string)
	return id, ok
}

type testAuditor struct{ decisions []ControlDecision }

func (a *testAuditor) RecordDecision(d ControlDecision) { a.decisions = append(a.decisions, d) }

type testReserver struct{ patterns []string }

func (r *testReserver) ReserveRelayRoute(pattern string) { r.patterns = append(r.patterns, pattern) }

type nilUnsafeAuditor struct{ tag string }

func (a *nilUnsafeAuditor) RecordDecision(ControlDecision) { _ = a.tag }

func TestClassReachableOn(t *testing.T) {
	cases := []struct {
		class     CapabilityClass
		transport Transport
		want      bool
	}{
		{ClassRead, TransportSocket, true}, {ClassRead, TransportTCP, true},
		{ClassConfigure, TransportSocket, true}, {ClassConfigure, TransportTCP, true},
		{ClassGrant, TransportSocket, true}, {ClassGrant, TransportTCP, true},
		{ClassExecute, TransportSocket, true}, {ClassExecute, TransportTCP, false},
		{ClassProxy, TransportSocket, true}, {ClassProxy, TransportTCP, false},
		{ClassProxy, Transport("bogus"), false}, {CapabilityClass("bogus"), TransportSocket, false},
		{CapabilityClass("bogus"), TransportTCP, false}, {CapabilityClass(""), TransportSocket, false},
		{ClassRead, Transport("bogus"), false},
	}
	for _, tc := range cases {
		t.Run(string(tc.class)+"/"+string(tc.transport), func(t *testing.T) {
			if got := ClassReachableOn(tc.class, tc.transport); got != tc.want {
				t.Errorf("ClassReachableOn(%q, %q) = %v, want %v", tc.class, tc.transport, got, tc.want)
			}
		})
	}
}

func TestAuthorizationStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{ErrNoCredential, http.StatusUnauthorized},
		{fmt.Errorf("resolve: %w", ErrNoCredential), http.StatusUnauthorized},
		{ErrClassNotGranted, http.StatusForbidden},
		{fmt.Errorf("policy: %w", ErrClassNotGranted), http.StatusForbidden},
		{errors.New("boom"), http.StatusForbidden},
	} {
		if got := AuthorizationStatus(tc.err); got != tc.want {
			t.Errorf("AuthorizationStatus(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestRouteRegistrarTransportAndReservation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport Transport
		class     CapabilityClass
		want      int
	}{
		{"execute tcp absent", TransportTCP, ClassExecute, http.StatusNotFound},
		{"proxy tcp absent", TransportTCP, ClassProxy, http.StatusNotFound},
		{"execute socket served", TransportSocket, ClassExecute, http.StatusCreated},
		{"proxy socket served", TransportSocket, ClassProxy, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			reserved := &testReserver{}
			rr := &RouteRegistrar{Mux: mux, Transport: tc.transport, Reserve: reserved}
			rr.Handle(tc.class, "POST /api/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
			res := httptest.NewRecorder()
			mux.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/x", nil))
			if res.Code != tc.want {
				t.Fatalf("status = %d, want %d", res.Code, tc.want)
			}
			if len(reserved.patterns) != 1 || reserved.patterns[0] != "POST /api/x" {
				t.Fatalf("reserved = %#v", reserved.patterns)
			}
		})
	}
}

func TestRouteRegistrarProxyCatchAllAbsentOnTCP(t *testing.T) {
	var ran bool
	mux := http.NewServeMux()
	rr := &RouteRegistrar{Mux: mux, Transport: TransportTCP}
	rr.Handle(ClassProxy, "/", func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})

	for _, path := range []string{"/", "/api/sessions", "/ws"} {
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, httptest.NewRequest(http.MethodPost, path, nil))
		if res.Code != http.StatusNotFound {
			t.Fatalf("POST %s: status = %d, want %d", path, res.Code, http.StatusNotFound)
		}
	}
	if ran {
		t.Fatal("socket-only proxy catch-all ran on TCP")
	}
}

func TestRouteRegistrarProxyCatchAllServedOnSocket(t *testing.T) {
	var ran bool
	mux := http.NewServeMux()
	rr := &RouteRegistrar{Mux: mux, Transport: TransportSocket}
	rr.Handle(ClassProxy, "/", func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})

	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/sessions", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if !ran {
		t.Fatal("proxy catch-all did not run on the socket")
	}
}

func TestRouteRegistrarAuthorizationAndAudit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		want    int
		allowed bool
	}{
		{"allowed", nil, http.StatusCreated, true},
		{"no credential", ErrNoCredential, http.StatusUnauthorized, false},
		{"class denied", ErrClassNotGranted, http.StatusForbidden, false},
		{"unknown denied", errors.New("boom"), http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auditor := &testAuditor{}
			mux := http.NewServeMux()
			rr := &RouteRegistrar{Mux: mux, Transport: TransportSocket, Authz: testAuthorizer{err: tc.err, credID: "cred-1"}, Auditor: auditor, CredentialID: testCredentialID}
			var ran bool
			rr.Handle(ClassConfigure, "POST /api/x", func(w http.ResponseWriter, _ *http.Request) {
				ran = true
				w.WriteHeader(http.StatusCreated)
			})
			res := httptest.NewRecorder()
			mux.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/x", nil))
			if res.Code != tc.want {
				t.Fatalf("status = %d, want %d", res.Code, tc.want)
			}
			if len(auditor.decisions) != 1 {
				t.Fatalf("decisions = %d, want 1", len(auditor.decisions))
			}
			d := auditor.decisions[0]
			if d.Allowed != tc.allowed || d.CredID != "cred-1" || d.Class != ClassConfigure || d.Transport != TransportSocket {
				t.Fatalf("decision = %+v", d)
			}
			if d.Method != http.MethodPost || d.Path != "/api/x" {
				t.Fatalf("decision request data = %+v", d)
			}
			if d.Allowed && d.Reason != "" {
				t.Fatalf("allowed decision reason = %q, want empty", d.Reason)
			}
			if !d.Allowed && d.Reason == "" {
				t.Fatal("refused decision has no reason")
			}
			if ran != tc.allowed {
				t.Fatalf("handler ran = %v, want %v", ran, tc.allowed)
			}
		})
	}
}

func TestRouteRegistrarNilAuthzAndAuditorAllows(t *testing.T) {
	mux := http.NewServeMux()
	rr := &RouteRegistrar{Mux: mux, Transport: TransportSocket}
	rr.Handle(ClassRead, "GET /api/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
}

func TestRouteRegistrarTypedNilAuditorStillCallsInterface(t *testing.T) {
	var auditor *nilUnsafeAuditor
	rr := &RouteRegistrar{Auditor: auditor}
	if rr.Auditor == nil {
		t.Fatal("typed nil auditor must remain non-nil when boxed in the interface")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("RecordDecision did not call the typed-nil auditor")
		}
	}()
	rr.recordDecision(httptest.NewRequest(http.MethodGet, "/api/x", nil), ClassRead, "", true, "")
}
