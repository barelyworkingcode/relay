package main

// ADR-015 decision 3: credential and class enforcement. api_credential_test.go
// and capability_test.go already cover Grants' nil/empty/unknown-class
// matrix and controlStatus's three named branches individually — this file
// goes at what they leave shallow: the full error-variant matrix run
// through control.RouteRegistrar.Handle (including a wrapped control.ErrNoCredential) with a
// leak check on the response body, and an end-to-end audit-record leak check against a real
// minted credential.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

type ceFakeAuthorizer struct {
	err    error
	credID string
}

func (f *ceFakeAuthorizer) Authorize(r *http.Request, _ control.CapabilityClass) error {
	if f.credID != "" {
		*r = *r.WithContext(withAPICredentialID(r.Context(), f.credID))
	}
	return f.err
}

type ceRecordingAuditor struct {
	decisions []control.ControlDecision
}

func (a *ceRecordingAuditor) RecordDecision(d control.ControlDecision) {
	a.decisions = append(a.decisions, d)
}

func ceDoBearer(t *testing.T, method, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	assertNoErr(t, err, "ceDoBearer: NewRequest")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	assertNoErr(t, err, "ceDoBearer: Do")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	assertNoErr(t, err, "ceDoBearer: ReadAll")
	return resp.StatusCode, string(body)
}

func ceAssertNoLeak(t *testing.T, haystack, plaintext, hash string) {
	t.Helper()
	if strings.Contains(haystack, plaintext) {
		t.Fatalf("plaintext token leaked: %q", haystack)
	}
	if hash != "" && strings.Contains(haystack, hash) {
		t.Fatalf("credential hash leaked: %q", haystack)
	}
}

func TestCredentialEnforcement_Handle_ErrorVariantMatrix_StatusAndNoLeak(t *testing.T) {
	const secret = "ce-matrix-secret-token-value"
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil allowed", nil, http.StatusOK},
		{"control.ErrNoCredential", control.ErrNoCredential, http.StatusUnauthorized},
		{"control.ErrClassNotGranted", control.ErrClassNotGranted, http.StatusForbidden},
		{"wrapped control.ErrNoCredential", fmt.Errorf("resolve bearer: %w", control.ErrNoCredential), http.StatusUnauthorized},
		{"wrapped control.ErrClassNotGranted", fmt.Errorf("policy: %w", control.ErrClassNotGranted), http.StatusForbidden},
		{"unrelated error", errors.New("settings store is on fire"), http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			rr := &control.RouteRegistrar{CredentialID: APICredentialIDFromContext,
				Mux:       mux,
				Transport: control.TransportSocket,
				Authz:     &ceFakeAuthorizer{err: tc.err, credID: "cred-under-test"},
			}
			rr.Handle(control.ClassConfigure, "POST /api/ce-x", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			status, body := ceDoBearer(t, "POST", srv.URL+"/api/ce-x", secret)
			if status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}
			if status >= 500 {
				t.Fatalf("an authorization outcome must never produce a 5xx, got %d", status)
			}
			if strings.Contains(body, secret) {
				t.Fatalf("response body leaked the bearer token: %q", body)
			}
		})
	}
}

// TestCredentialEnforcement_ProxyClassGatesTheProxiedSurface is what ADR-016
// decision 4 buys: a configure credential stops silently meaning "and also
// every route relayLLM registers". Run through the real composed stack
// (accNewServer, api_credential_cli_test.go) rather than a synthetic mux,
// because the claim is about the class registerFrontendRoutes actually
// mounts "/" under, not about Grants.
//
// The dispatcher's own 404 body is the evidence the proxy credential got
// through: http.ServeMux answers a missing route with the same status and
// the same text/plain, so only the body tells the catch-all's answer from
// the mux's.
func TestCredentialEnforcement_ProxyClassGatesTheProxiedSurface(t *testing.T) {
	store := newCLISandboxStore(t)
	srv := accNewServer(t, store, accLegacyToken)

	configureOnly := accMint(t, store, "ce-configurer", "read", "configure")
	proxyOnly := accMint(t, store, "ce-proxier", "proxy")

	const dispatcherAnswer = "no service registered for this path"
	for _, path := range []string{"/api/sessions", "/api/terminals/1/input", "/ws"} {
		t.Run(path, func(t *testing.T) {
			resp, body := srv.socket(t, "POST", path, configureOnly, map[string]any{})
			accAssertForbidden(t, resp, body, "configure-only credential on the proxied catch-all")
			if strings.Contains(string(body), dispatcherAnswer) {
				t.Fatalf("configure-only credential reached the dispatcher on %s: body=%s", path, body)
			}

			resp, body = srv.socket(t, "POST", path, proxyOnly, map[string]any{})
			accAssertReached(t, resp, body, "proxy credential on the proxied catch-all")
			if !strings.Contains(string(body), dispatcherAnswer) {
				t.Fatalf("proxy credential on %s: status = %d body=%s; want the dispatcher's own answer", path, resp.StatusCode, body)
			}
		})
	}

	// The control in both directions: the configure credential is not a
	// blanket refusal, and the proxy credential is not a superset.
	resp, body := srv.socket(t, "POST", "/api/projects", configureOnly, map[string]any{"name": "ce-proj", "path": t.TempDir()})
	accAssertReached(t, resp, body, "configure credential on a configure-class route")
	resp, body = srv.socket(t, "GET", "/api/services", proxyOnly, nil)
	accAssertForbidden(t, resp, body, "proxy credential on a read-class route")
}

func TestCredentialEnforcement_ControlDecision_NeverLeaksCredentialTokenOrHash(t *testing.T) {
	store := newCLISandboxStore(t)
	const plaintext = "ce-distinctive-plaintext-do-not-leak-798234"

	var cred config.APICredential
	assertNoErr(t, store.With(func(s *config.Settings) {
		cred = config.APICredential{
			ID:      "ce-leak-cred",
			Name:    "ce-leak-check",
			Hash:    config.HashToken(plaintext),
			Classes: []control.CapabilityClass{control.ClassRead},
			Created: "2026-01-01T00:00:00Z",
		}
		addAPICredential(s, cred)
	}), "store.With add credential")

	authz := NewCredentialAuthorizer(store)
	aud := &ceRecordingAuditor{}
	mux := http.NewServeMux()
	rr := &control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket, Authz: authz, Auditor: aud}
	rr.Handle(control.ClassRead, "GET /api/ce-leak", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	rr.Handle(control.ClassGrant, "GET /api/ce-leak-denied", func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler must not run: this credential does not hold control.ClassGrant")
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	status, body := ceDoBearer(t, "GET", srv.URL+"/api/ce-leak", plaintext)
	if status != http.StatusOK {
		t.Fatalf("allowed request status = %d, want 200", status)
	}
	ceAssertNoLeak(t, body, plaintext, cred.Hash)

	status, body = ceDoBearer(t, "GET", srv.URL+"/api/ce-leak-denied", plaintext)
	if status != http.StatusForbidden {
		t.Fatalf("refused request status = %d, want 403", status)
	}
	ceAssertNoLeak(t, body, plaintext, cred.Hash)

	if len(aud.decisions) != 2 {
		t.Fatalf("want 2 recorded decisions, got %d", len(aud.decisions))
	}
	for _, d := range aud.decisions {
		dump := fmt.Sprintf("%+v", d)
		ceAssertNoLeak(t, dump, plaintext, cred.Hash)
	}

	allowed := aud.decisions[0]
	if !allowed.Allowed || allowed.CredID != cred.ID {
		t.Fatalf("allowed decision = %+v, want Allowed=true CredID=%q", allowed, cred.ID)
	}
	refused := aud.decisions[1]
	if refused.Allowed || refused.CredID != cred.ID {
		t.Fatalf("refused decision = %+v, want Allowed=false CredID=%q", refused, cred.ID)
	}
}
