package main

// HTTP-door coverage for RegisterEvePasskeyRoutes (docs/eve-passkey-enrolment.md's
// second half): report shape and its returned pending set, the revocations
// GET, bad-JSON 400, and class reachability on both transports -- PUT is
// ClassConfigure and GET is ClassRead, both reachable everywhere
// (control.ClassReachableOn).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

func newEvePasskeyRoutesServer(t *testing.T, transport control.Transport) (*httptest.Server, config.SettingsStore, *EvePasskeyOps) {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	ops := &EvePasskeyOps{Store: store}
	mux := http.NewServeMux()
	RegisterEvePasskeyRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: transport}, ops)
	return httptest.NewServer(mux), store, ops
}

func TestEvePasskeyRoutes_PutReportsAndReturnsEmptyRevocations(t *testing.T) {
	srv, store, _ := newEvePasskeyRoutesServer(t, control.TransportSocket)
	defer srv.Close()

	resp, body := doJSON(t, "PUT", srv.URL+"/api/eve/passkeys", map[string]any{
		"passkeys": []map[string]any{
			{"id": "p1", "label": "iPhone", "created": "2026-09-07T10:00:00Z", "last_used": "2026-09-07T11:00:00Z"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var view evePasskeyRevocationsView
	mustUnmarshal(t, body, &view)
	if len(view.Revocations) != 0 {
		t.Fatalf("revocations = %v, want none for a fresh report", view.Revocations)
	}

	got := store.Get().EvePasskeys
	if len(got) != 1 || got[0].ID != "p1" || got[0].Label != "iPhone" {
		t.Fatalf("settings.json does not carry the reported passkey: %+v", got)
	}
}

// The PUT's 200 body is the still-pending set AFTER this report, so eve
// learns of a revocation it doesn't yet know about on the same round-trip.
func TestEvePasskeyRoutes_PutReturnsStillPendingRevocations(t *testing.T) {
	srv, store, _ := newEvePasskeyRoutesServer(t, control.TransportSocket)
	defer srv.Close()

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "p1"}, {ID: "p2"}}
		s.EvePasskeyRevocations = []config.EvePasskeyRevocation{{ID: "p1", Requested: "2026-09-07T00:00:00Z"}}
	}), "seed a pending revocation")

	resp, body := doJSON(t, "PUT", srv.URL+"/api/eve/passkeys", map[string]any{
		"passkeys": []map[string]any{{"id": "p1"}, {"id": "p2"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var view evePasskeyRevocationsView
	mustUnmarshal(t, body, &view)
	if len(view.Revocations) != 1 || view.Revocations[0] != "p1" {
		t.Fatalf("revocations = %v, want [p1] (still pending, eve has not applied it)", view.Revocations)
	}
}

func TestEvePasskeyRoutes_PutBadJSONGets400(t *testing.T) {
	srv, _, _ := newEvePasskeyRoutesServer(t, control.TransportSocket)
	defer srv.Close()

	resp, err := http.NewRequest(http.MethodPut, srv.URL+"/api/eve/passkeys", strings.NewReader("{not json"))
	assertNoErr(t, err, "NewRequest")
	got, err := http.DefaultClient.Do(resp)
	assertNoErr(t, err, "Do")
	defer got.Body.Close()
	if got.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got.StatusCode)
	}
}

func TestEvePasskeyRoutes_GetRevocations(t *testing.T) {
	srv, store, _ := newEvePasskeyRoutesServer(t, control.TransportSocket)
	defer srv.Close()

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "p1"}}
		s.EvePasskeyRevocations = []config.EvePasskeyRevocation{{ID: "p1", Requested: "2026-09-07T00:00:00Z"}}
	}), "seed a pending revocation")

	resp, body := doJSON(t, "GET", srv.URL+"/api/eve/passkeys/revocations", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var view evePasskeyRevocationsView
	mustUnmarshal(t, body, &view)
	if len(view.Revocations) != 1 || view.Revocations[0] != "p1" {
		t.Fatalf("revocations = %v, want [p1]", view.Revocations)
	}
}

// Both routes are read/configure, which control.ClassReachableOn admits on
// every transport (unlike an execute-class route, socket-only under
// ADR-015 decision 2).
func TestEvePasskeyRoutes_ReachableOnBothSocketAndTCPTransports(t *testing.T) {
	for _, transport := range []control.Transport{control.TransportSocket, control.TransportTCP} {
		t.Run(string(transport), func(t *testing.T) {
			srv, _, _ := newEvePasskeyRoutesServer(t, transport)
			defer srv.Close()

			resp, body := doJSON(t, "GET", srv.URL+"/api/eve/passkeys/revocations", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET status = %d on transport %s, body = %s", resp.StatusCode, transport, body)
			}
			resp2, body2 := doJSON(t, "PUT", srv.URL+"/api/eve/passkeys", map[string]any{"passkeys": []map[string]any{}})
			if resp2.StatusCode != http.StatusOK {
				t.Fatalf("PUT status = %d on transport %s, body = %s", resp2.StatusCode, transport, body2)
			}
		})
	}
}

func TestEvePasskeyRoutes_OnChangeFiresOnReport(t *testing.T) {
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	fired := 0
	ops := &EvePasskeyOps{Store: store, OnChange: func() { fired++ }}
	mux := http.NewServeMux()
	RegisterEvePasskeyRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, ops)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	doJSON(t, "PUT", srv.URL+"/api/eve/passkeys", map[string]any{"passkeys": []map[string]any{{"id": "a", "label": "iPhone"}}})
	if fired == 0 {
		t.Fatal("expected OnChange to fire on a successful report")
	}
}
