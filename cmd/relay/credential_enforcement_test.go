package main

// ADR-015 decision 3: credential and class enforcement. api_credential_test.go
// and capability_test.go already cover Grants' nil/empty/unknown-class
// matrix and controlStatus's three named branches individually — this file
// goes at what they leave shallow: the full error-variant matrix run
// through control.RouteRegistrar.Handle (including a wrapped control.ErrNoCredential) with a
// leak check on the response body, a multi-restart migration sequence
// against one persisted Settings with an unrelated credential present to
// prove it survives untouched, the migrated credential's grant/execute
// refusal proven through the real control.Authorizer + Handle stack rather than
// Grants alone, and an end-to-end audit-record leak check against a real
// minted credential.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// FrontendChannel.Ensure mints a fresh random token every process start and
// never persists it, so a realistic sequence is many restarts, each handing
// migrateFrontendTokenToCredential a DIFFERENT token, against the SAME
// persisted Settings. The marker-name keying must converge on one
// credential regardless, and an unrelated pre-existing credential must ride
// along untouched.
func TestCredentialEnforcement_Migration_ConvergesAcrossManyRestarts(t *testing.T) {
	s := &config.Settings{}

	other := config.APICredential{
		ID:      "other-tool-id",
		Name:    "other-tool",
		Hash:    config.HashToken("other-tool-token"),
		Classes: []control.CapabilityClass{control.ClassRead},
		Created: "2020-01-01T00:00:00Z",
	}
	addAPICredential(s, other)

	const restarts = 7
	var lastToken string
	for i := 0; i < restarts; i++ {
		lastToken = "boot-token-" + strconv.Itoa(i)
		migrateFrontendTokenToCredential(s, lastToken)
	}

	if len(s.APICredentials) != 2 {
		t.Fatalf("want 2 credentials (other + legacy) after %d restarts, got %d: %+v", restarts, len(s.APICredentials), s.APICredentials)
	}

	var legacy *config.APICredential
	for i := range s.APICredentials {
		if s.APICredentials[i].Name == legacyFrontendCredentialName {
			legacy = &s.APICredentials[i]
		}
	}
	if legacy == nil {
		t.Fatal("no credential named legacyFrontendCredentialName survived the restart sequence")
	}
	if legacy.Hash != config.HashToken(lastToken) {
		t.Fatal("legacy credential's hash does not match the LATEST restart's token")
	}
	if len(legacy.Classes) != 3 || !legacy.Grants(control.ClassRead) || !legacy.Grants(control.ClassConfigure) || !legacy.Grants(control.ClassProxy) {
		t.Fatalf("legacy credential's classes drifted across restarts: %+v", legacy.Classes)
	}
	if legacy.Grants(control.ClassGrant) || legacy.Grants(control.ClassExecute) {
		t.Fatalf("legacy credential picked up a class the migration must never carry: %+v", legacy.Classes)
	}

	for i := 0; i < restarts-1; i++ {
		stale := "boot-token-" + strconv.Itoa(i)
		if authenticateAPICredential(s, stale) != nil {
			t.Fatalf("a token from an earlier restart (%q) still authenticates", stale)
		}
	}
	if authenticateAPICredential(s, lastToken) == nil {
		t.Fatal("the latest restart's token does not authenticate")
	}

	found := findAPICredential(s, "other-tool-id")
	if found == nil {
		t.Fatal("migration removed the unrelated pre-existing credential")
	}
	if found.Hash != other.Hash || found.Name != other.Name || len(found.Classes) != 1 || found.Classes[0] != control.ClassRead {
		t.Fatalf("migration disturbed the unrelated pre-existing credential: %+v", found)
	}
}

func TestCredentialEnforcement_MigratedCredential_DeniedGrantAndExecute_ThroughRealStack(t *testing.T) {
	store := newCLISandboxStore(t)
	const frontendToken = "ce-migrated-frontend-token"
	assertNoErr(t, store.With(func(s *config.Settings) {
		if !migrateFrontendTokenToCredential(s, frontendToken) {
			t.Fatal("migration reported no change on first call")
		}
	}), "store.With migrate")

	authz := NewCredentialAuthorizer(store)

	for _, tc := range []struct {
		class control.CapabilityClass
		want  int
	}{
		{control.ClassRead, http.StatusOK},
		{control.ClassConfigure, http.StatusOK},
		{control.ClassGrant, http.StatusForbidden},
		{control.ClassExecute, http.StatusForbidden},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			mux := http.NewServeMux()
			rr := &control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket, Authz: authz}
			rr.Handle(tc.class, "POST /api/ce-y", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			status, _ := ceDoBearer(t, "POST", srv.URL+"/api/ce-y", frontendToken)
			if status != tc.want {
				t.Fatalf("class %s: status = %d, want %d", tc.class, status, tc.want)
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

// TestCredentialEnforcement_EveIsUnaffectedByTheProxySplit is the
// compatibility claim ADR-016 decision 4 rests on, made against a real
// enhanced service rather than against the dispatcher's 404. Eve holds
// RELAY_FRONTEND_TOKEN and dials the frontend SOCKET, so a socket-only
// control.ClassProxy must leave it reaching exactly what it reached before -- proven
// by the upstream service recording the request, not by the status alone.
//
// The TCP half is the other side of the same decision, and it is a change:
// the proxied surface is gone from the loopback bind for every credential,
// Eve's included.
func TestCredentialEnforcement_EveIsUnaffectedByTheProxySplit(t *testing.T) {
	store := newCLISandboxStore(t)
	const eveToken = "ce-eve-frontend-token"

	registry := NewEnhancedServiceRegistry(nil)
	fake := NewFakeService(t, FakeServiceOptions{ServiceID: "ce-svc", Manifest: newManifest("/api/a/")})
	assertNoErr(t, registry.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest()), "register manifest")

	ops := &ServiceOps{Store: store, Registry: &svcRecorder{}}
	extMgr := NewExternalMcpManager(nil)
	dir := mkShortTempDir(t, "ce-eve-")
	projOps := &ProjectOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		Endpoint{Socket: filepath.Join(dir, "frontend.sock"), Token: eveToken},
		registry, nil, nil, ops, &EnrolmentOps{Store: store}, &AuditOps{}, &McpOps{Store: store, Ctx: context.Background()}, projOps,
		NewCredentialAuthorizer(store), nil,
	)
	assertNoErr(t, err, "NewFrontendServer")
	go func() { _ = srv.Serve() }()
	assertNoErr(t, srv.ListenLoopback("127.0.0.1:0"), "ListenLoopback")
	go func() { _ = srv.ServeLoopback() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	sockClient := dialFrontendHTTP(srv.socketPath)
	req, err := http.NewRequest("POST", "http://unix/api/a/echo", strings.NewReader(`{"hello":"eve"}`))
	assertNoErr(t, err, "new socket request")
	req.Header.Set("Authorization", "Bearer "+eveToken)
	resp, err := sockClient.Do(req)
	assertNoErr(t, err, "POST over the frontend socket")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("RELAY_FRONTEND_TOKEN on a proxied route over the socket: status = %d, want 200", resp.StatusCode)
	}
	if got := fake.LastRequest(); got == nil || got.Path != "/api/a/echo" {
		t.Fatalf("the enhanced service never saw Eve's request: %+v", got)
	}

	before := len(fake.Requests())
	req, err = http.NewRequest("POST", "http://"+srv.tcpLn.Addr().String()+"/api/a/echo", strings.NewReader(`{"hello":"eve"}`))
	assertNoErr(t, err, "new tcp request")
	req.Header.Set("Authorization", "Bearer "+eveToken)
	resp, err = http.DefaultClient.Do(req)
	assertNoErr(t, err, "POST over loopback TCP")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a proxied route over loopback TCP: status = %d, want 404 (the mount is socket-only)", resp.StatusCode)
	}
	if after := len(fake.Requests()); after != before {
		t.Fatalf("the enhanced service saw %d requests over TCP; the proxied surface must be absent there", after-before)
	}
}
