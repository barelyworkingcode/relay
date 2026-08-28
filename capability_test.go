package main

// Tests for capability.go: ClassReachableOn's fail-closed matrix and
// RouteRegistrar.Handle's registration/authorization/audit wiring. Hermetic
// — no settings.json, no bridge socket, no spawned process. httptest.NewServer
// wraps an in-memory http.ServeMux, the same pattern service_routes_test.go
// and project_routes_test.go use for route-envelope coverage, and doJSON /
// assertNoErr (project_routes_test.go / support_test.go) are reused rather
// than duplicated.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAuthorizer is a minimal Authorizer double. When credID is set it
// writes it into the request context through the exact mechanism
// api_credential.go's real Authorizer uses (withAPICredentialID), so these
// tests exercise the real hand-off Handle relies on rather than a
// reimplementation of it.
type fakeAuthorizer struct {
	err    error
	credID string
}

func (f *fakeAuthorizer) Authorize(r *http.Request, _ CapabilityClass) error {
	if f.credID != "" {
		*r = *r.WithContext(withAPICredentialID(r.Context(), f.credID))
	}
	return f.err
}

type fakeAuditor struct {
	decisions []ControlDecision
}

func (f *fakeAuditor) RecordDecision(d ControlDecision) {
	f.decisions = append(f.decisions, d)
}

func TestClassReachableOn(t *testing.T) {
	cases := []struct {
		class     CapabilityClass
		transport Transport
		want      bool
	}{
		{ClassRead, TransportSocket, true},
		{ClassRead, TransportTCP, true},
		{ClassConfigure, TransportSocket, true},
		{ClassConfigure, TransportTCP, true},
		{ClassGrant, TransportSocket, true},
		{ClassGrant, TransportTCP, true},
		{ClassExecute, TransportSocket, true},
		{ClassExecute, TransportTCP, false},
		{CapabilityClass("bogus"), TransportSocket, false},
		{CapabilityClass("bogus"), TransportTCP, false},
		{CapabilityClass(""), TransportSocket, false},
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

func TestRouteRegistrar_Handle_ExecuteAbsentOnTCP(t *testing.T) {
	var ran bool
	mux := http.NewServeMux()
	rr := &RouteRegistrar{Mux: mux, Transport: TransportTCP}
	rr.Handle(ClassExecute, "POST /api/mcps", func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/mcps", "application/json", nil)
	assertNoErr(t, err, "POST")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("execute on TCP: status = %d, want 404 (no route registered)", resp.StatusCode)
	}
	if ran {
		t.Fatal("handler body ran; the route must never have been registered on this transport")
	}
}

func TestRouteRegistrar_Handle_ExecuteServedOnSocket(t *testing.T) {
	var ran bool
	mux := http.NewServeMux()
	rr := &RouteRegistrar{Mux: mux, Transport: TransportSocket}
	rr.Handle(ClassExecute, "POST /api/mcps", func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusCreated)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/mcps", "application/json", nil)
	assertNoErr(t, err, "POST")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("execute on socket: status = %d, want 201", resp.StatusCode)
	}
	if !ran {
		t.Fatal("handler body did not run for a route that must be reachable on this transport")
	}
}

func TestRouteRegistrar_Handle_AuthorizerRefusalMapsStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"no credential", errNoCredential, http.StatusUnauthorized},
		{"class not granted", errClassNotGranted, http.StatusForbidden},
		{"unrecognized error", errors.New("boom"), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ran bool
			mux := http.NewServeMux()
			rr := &RouteRegistrar{
				Mux:       mux,
				Transport: TransportSocket,
				Authz:     &fakeAuthorizer{err: tc.err},
			}
			rr.Handle(ClassRead, "GET /api/x", func(w http.ResponseWriter, _ *http.Request) {
				ran = true
				w.WriteHeader(http.StatusOK)
			})

			srv := httptest.NewServer(mux)
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/api/x")
			assertNoErr(t, err, "GET")
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if resp.StatusCode >= 500 {
				t.Fatalf("an authorization refusal must never produce a 5xx, got %d", resp.StatusCode)
			}
			if ran {
				t.Fatal("handler ran despite a refused authorization")
			}
		})
	}
}

func TestRouteRegistrar_Handle_NilAuthzAndAuditorDoNotPanic(t *testing.T) {
	var ran bool
	mux := http.NewServeMux()
	rr := &RouteRegistrar{Mux: mux, Transport: TransportSocket} // Authz and Auditor both nil
	rr.Handle(ClassRead, "GET /api/x", func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/x")
	assertNoErr(t, err, "GET")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("nil Authz must allow; status = %d", resp.StatusCode)
	}
	if !ran {
		t.Fatal("handler did not run")
	}
}

func TestRouteRegistrar_Handle_NilAuditorSurvivesARefusal(t *testing.T) {
	mux := http.NewServeMux()
	rr := &RouteRegistrar{
		Mux:       mux,
		Transport: TransportSocket,
		Authz:     &fakeAuthorizer{err: errClassNotGranted},
		// Auditor left nil: RecordDecision must not be reached, let alone panic.
	}
	rr.Handle(ClassRead, "GET /api/x", func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler must not run on a refused request")
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/x")
	assertNoErr(t, err, "GET")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestRouteRegistrar_Handle_AuditsAllowedAndRefused(t *testing.T) {
	const secretToken = "super-secret-token-value"

	t.Run("allowed", func(t *testing.T) {
		aud := &fakeAuditor{}
		mux := http.NewServeMux()
		rr := &RouteRegistrar{
			Mux:       mux,
			Transport: TransportSocket,
			Authz:     &fakeAuthorizer{credID: "cred-1"},
			Auditor:   aud,
		}
		rr.Handle(ClassConfigure, "POST /api/y", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		srv := httptest.NewServer(mux)
		defer srv.Close()

		req, _ := http.NewRequest("POST", srv.URL+"/api/y", nil)
		req.Header.Set("Authorization", "Bearer "+secretToken)
		resp, err := http.DefaultClient.Do(req)
		assertNoErr(t, err, "POST")
		resp.Body.Close()

		if len(aud.decisions) != 1 {
			t.Fatalf("got %d decisions, want 1", len(aud.decisions))
		}
		d := aud.decisions[0]
		if !d.Allowed || d.Reason != "" {
			t.Fatalf("allowed decision = %+v", d)
		}
		if d.CredID != "cred-1" {
			t.Fatalf("CredID = %q, want cred-1", d.CredID)
		}
		if strings.Contains(d.CredID, secretToken) {
			t.Fatal("CredID must never contain the bearer token")
		}
	})

	t.Run("refused", func(t *testing.T) {
		aud := &fakeAuditor{}
		mux := http.NewServeMux()
		rr := &RouteRegistrar{
			Mux:       mux,
			Transport: TransportSocket,
			Authz:     &fakeAuthorizer{err: errClassNotGranted},
			Auditor:   aud,
		}
		rr.Handle(ClassConfigure, "POST /api/y", func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("handler must not run on a refused request")
		})

		srv := httptest.NewServer(mux)
		defer srv.Close()

		req, _ := http.NewRequest("POST", srv.URL+"/api/y", nil)
		req.Header.Set("Authorization", "Bearer "+secretToken)
		resp, err := http.DefaultClient.Do(req)
		assertNoErr(t, err, "POST")
		resp.Body.Close()

		if len(aud.decisions) != 1 {
			t.Fatalf("got %d decisions, want 1", len(aud.decisions))
		}
		d := aud.decisions[0]
		if d.Allowed || d.Reason == "" {
			t.Fatalf("refused decision = %+v", d)
		}
		if strings.Contains(d.CredID, secretToken) {
			t.Fatal("CredID must never contain the bearer token")
		}
	})
}
