package main

// HTTP-door coverage for RegisterEveEnrolmentRoutes (docs/eve-passkey-enrolment.md):
// status/consume shapes, the 409 on a closed window, bad-JSON 400, and class
// reachability on both the socket and TCP transports — GET is ClassRead and
// POST .../consume is ClassConfigure, both reachable everywhere
// (control.ClassReachableOn), unlike an execute-class route.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

func newEveEnrolmentRoutesServer(t *testing.T, transport control.Transport) (*httptest.Server, config.SettingsStore, *EveEnrolmentOps) {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	ops := &EveEnrolmentOps{Store: store}
	mux := http.NewServeMux()
	RegisterEveEnrolmentRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: transport}, ops)
	return httptest.NewServer(mux), store, ops
}

func eveSeedOpenWindow(t *testing.T, store config.SettingsStore) string {
	t.Helper()
	expires := eveTestExpiresIn(t, time.Minute)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EveEnrolment = &config.EveEnrolmentWindow{Expires: expires}
	}), "seed an open window")
	return expires
}

func TestEveEnrolmentRoutes_StatusClosedByDefault(t *testing.T) {
	srv, _, _ := newEveEnrolmentRoutesServer(t, control.TransportSocket)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/eve/passkey-enrolment", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var view eveEnrolmentStatusView
	mustUnmarshal(t, body, &view)
	if view.Open {
		t.Fatalf("a fresh store reports an open window: %+v", view)
	}
}

func TestEveEnrolmentRoutes_StatusReflectsAnOpenWindow(t *testing.T) {
	srv, store, _ := newEveEnrolmentRoutesServer(t, control.TransportSocket)
	defer srv.Close()
	expires := eveSeedOpenWindow(t, store)

	resp, body := doJSON(t, "GET", srv.URL+"/api/eve/passkey-enrolment", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var view eveEnrolmentStatusView
	mustUnmarshal(t, body, &view)
	if !view.Open || view.Expires != expires {
		t.Fatalf("view = %+v, want open with expires %q", view, expires)
	}
}

func TestEveEnrolmentRoutes_ConsumeSucceedsThenSecondCallerGets409(t *testing.T) {
	srv, store, _ := newEveEnrolmentRoutesServer(t, control.TransportSocket)
	defer srv.Close()
	eveSeedOpenWindow(t, store)

	resp, body := doJSON(t, "POST", srv.URL+"/api/eve/passkey-enrolment/consume", map[string]any{"ip": "10.0.1.7", "label": "browser-a"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first consume status = %d, body = %s", resp.StatusCode, body)
	}
	var view eveEnrolmentConsumedView
	mustUnmarshal(t, body, &view)
	if view.Expires == "" {
		t.Fatalf("consumed view carries no expires: %+v", view)
	}
	if store.Get().EveEnrolment != nil {
		t.Fatalf("the window is still on disk after a successful consume")
	}

	resp2, body2 := doJSON(t, "POST", srv.URL+"/api/eve/passkey-enrolment/consume", map[string]any{"ip": "10.0.1.8", "label": "browser-b"})
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second consume status = %d, want 409, body = %s", resp2.StatusCode, body2)
	}
	var errBody map[string]string
	mustUnmarshal(t, body2, &errBody)
	if errBody["error"] == "" {
		t.Fatalf("409 body carries no error message: %s", body2)
	}
}

func TestEveEnrolmentRoutes_ConsumeWithNoWindowGets409(t *testing.T) {
	srv, _, _ := newEveEnrolmentRoutesServer(t, control.TransportSocket)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/eve/passkey-enrolment/consume", map[string]any{"ip": "10.0.1.9"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", resp.StatusCode, body)
	}
}

func TestEveEnrolmentRoutes_ConsumeBadJSONGets400(t *testing.T) {
	srv, _, _ := newEveEnrolmentRoutesServer(t, control.TransportSocket)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/eve/passkey-enrolment/consume", "application/json", strings.NewReader("{not json"))
	assertNoErr(t, err, "POST")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// Both routes are read/configure, which control.ClassReachableOn admits on
// every transport (unlike an execute-class route, socket-only under
// ADR-015 decision 2) — registering them against a TCP-transport registrar
// must still leave both patterns reachable.
func TestEveEnrolmentRoutes_ReachableOnBothSocketAndTCPTransports(t *testing.T) {
	for _, transport := range []control.Transport{control.TransportSocket, control.TransportTCP} {
		t.Run(string(transport), func(t *testing.T) {
			srv, store, _ := newEveEnrolmentRoutesServer(t, transport)
			defer srv.Close()
			eveSeedOpenWindow(t, store)

			resp, body := doJSON(t, "GET", srv.URL+"/api/eve/passkey-enrolment", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET status = %d on transport %s, body = %s", resp.StatusCode, transport, body)
			}
			resp2, body2 := doJSON(t, "POST", srv.URL+"/api/eve/passkey-enrolment/consume", map[string]any{"ip": "10.0.1.7"})
			if resp2.StatusCode != http.StatusOK {
				t.Fatalf("POST consume status = %d on transport %s, body = %s", resp2.StatusCode, transport, body2)
			}
		})
	}
}

func TestEveEnrolmentRoutes_OnChangeFiresOnConsume(t *testing.T) {
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	fired := 0
	ops := &EveEnrolmentOps{Store: store, OnChange: func() { fired++ }}
	mux := http.NewServeMux()
	RegisterEveEnrolmentRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, ops)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	eveSeedOpenWindow(t, store)

	doJSON(t, "POST", srv.URL+"/api/eve/passkey-enrolment/consume", map[string]any{"ip": "10.0.1.7"})
	if fired == 0 {
		t.Fatal("expected OnChange to fire on a successful consume")
	}
}

// eveTestExpiresIn formats an expiry d from now, RFC3339, the same shape
// EveEnrolmentOps itself writes.
func eveTestExpiresIn(t *testing.T, d time.Duration) string {
	t.Helper()
	return time.Now().UTC().Add(d).Format(time.RFC3339)
}
