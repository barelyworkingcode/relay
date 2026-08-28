package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for FrontendServer. Covers:
//   - bearer-token enforcement (401 on missing/wrong, 200 on right)
//   - Unix socket created with 0600 permissions
//   - Empty token = dev mode (no auth)
//   - Unknown route falls through to dispatcher → 404

func newTestFrontendServer(t *testing.T, token string) (*FrontendServer, string) {
	t.Helper()
	dir := mkShortTempDir(t, "fe-")
	sock := filepath.Join(dir, "frontend.sock")

	store := NewSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	enhanced := NewEnhancedServiceRegistry(nil)

	extMgr := NewExternalMcpManager(nil)
	srv, err := NewFrontendServer(
		store,
		extMgr,
		extMgr,
		extMgr,
		Endpoint{Socket: sock, Token: token},
		enhanced,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewFrontendServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	_ = dialUnixWithTimeout(t, sock, 2*time.Second).Close()
	return srv, sock
}

// dialFrontendHTTP constructs an http.Client that dials the Unix socket.
func dialFrontendHTTP(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
}

func TestFrontendServer_SocketHas0600Perms(t *testing.T) {
	_, sock := newTestFrontendServer(t, "some-token")
	fi, err := os.Stat(sock)
	assertNoErr(t, err, "stat socket")
	mode := fi.Mode().Perm()
	if mode != 0o600 {
		t.Fatalf("socket perms = %o; want 0600", mode)
	}
}

func TestFrontendServer_BearerAuth_RejectsMissing(t *testing.T) {
	_, sock := newTestFrontendServer(t, "good-token")
	client := dialFrontendHTTP(sock)

	resp, err := client.Get("http://unix/api/unclaimed")
	assertNoErr(t, err, "GET")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without Authorization; got %d", resp.StatusCode)
	}
}

func TestFrontendServer_BearerAuth_RejectsWrong(t *testing.T) {
	_, sock := newTestFrontendServer(t, "good-token")
	client := dialFrontendHTTP(sock)

	req, _ := http.NewRequest("GET", "http://unix/api/unclaimed", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := client.Do(req)
	assertNoErr(t, err, "GET")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong Authorization; got %d", resp.StatusCode)
	}
}

func TestFrontendServer_BearerAuth_AcceptsCorrect_Returns404ForUnknownRoute(t *testing.T) {
	_, sock := newTestFrontendServer(t, "good-token")
	client := dialFrontendHTTP(sock)

	req, _ := http.NewRequest("GET", "http://unix/api/unclaimed", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	resp, err := client.Do(req)
	assertNoErr(t, err, "GET")
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 from dispatcher for unclaimed route; got %d body=%s", resp.StatusCode, body)
	}
}

func TestFrontendServer_EmptyToken_FailsClosed(t *testing.T) {
	// An empty configured token is a misconfiguration (the frontend channel
	// always mints one). It must fail CLOSED — reject every request — rather
	// than silently disable auth and expose all proxied services.
	_, sock := newTestFrontendServer(t, "")
	client := dialFrontendHTTP(sock)

	resp, err := client.Get("http://unix/api/anything")
	assertNoErr(t, err, "GET")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty token must fail closed (401); got %d", resp.StatusCode)
	}
}

func TestFrontendServer_BearerAuth_RejectsWrongScheme(t *testing.T) {
	_, sock := newTestFrontendServer(t, "good-token")
	client := dialFrontendHTTP(sock)

	req, _ := http.NewRequest("GET", "http://unix/api/x", nil)
	req.Header.Set("Authorization", "Basic good-token") // wrong scheme
	resp, err := client.Do(req)
	assertNoErr(t, err, "GET")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on Basic scheme; got %d", resp.StatusCode)
	}
}

func TestListenLoopback_RefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:9980", ":9980", "192.168.64.1:9980"} {
		t.Run(addr, func(t *testing.T) {
			s := &FrontendServer{server: &http.Server{}}
			err := s.ListenLoopback(addr)
			if !errors.Is(err, ErrNonLoopbackAPIListen) {
				t.Fatalf("%q must be refused as non-loopback, got %v", addr, err)
			}
			if s.tcpLn != nil {
				t.Fatal("a refused address must not leave a listener bound")
			}
		})
	}
}

// TestListenLoopback_ServesReadAndConfigureButNotExecute pins ADR-015
// decision 2: the TCP mux is built fresh per transport and only carries
// read/configure/grant routes, never execute. It deliberately replaces a
// pre-ADR-015 test that asserted the TCP listener served the socket's exact
// handler — that property is gone on purpose.
func TestListenLoopback_ServesReadAndConfigureButNotExecute(t *testing.T) {
	dir := mkShortTempDir(t, "fe-tcp-")
	store := NewSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	ops := &ServiceOps{Store: store, Registry: &svcRecorder{}}
	if _, err := ops.Create(serviceFields{DisplayName: "Worker", Command: "/bin/sleep", Args: []string{"1"}}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	extMgr := NewExternalMcpManager(nil)

	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		Endpoint{Socket: filepath.Join(dir, "frontend.sock"), Token: "tok"},
		NewEnhancedServiceRegistry(nil), nil, nil, ops, nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("NewFrontendServer: %v", err)
	}
	if err := srv.ListenLoopback("127.0.0.1:0"); err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	go func() { _ = srv.Serve() }()
	go func() { _ = srv.ServeLoopback() }()
	defer srv.Shutdown(context.Background())

	base := "http://" + srv.tcpLn.Addr().String()

	// read, no bearer: the TCP door enforces the same auth as the socket.
	resp, err := http.Get(base + "/api/services")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a read route without a bearer must be 401, got %d", resp.StatusCode)
	}

	// read, with bearer: the route is registered and answers for real.
	req, _ := http.NewRequest("GET", base+"/api/services", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an authenticated read route must reach its handler, got %d", resp.StatusCode)
	}

	// configure, no bearer: same enforcement as read.
	autostartBody := func() *bytes.Buffer { return bytes.NewBufferString(`{"autostart":true}`) }
	req, _ = http.NewRequest("PUT", base+"/api/services/worker/autostart", autostartBody())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a configure route without a bearer must be 401, got %d", resp.StatusCode)
	}

	// configure, with bearer: reachable, and mutates real state.
	req, _ = http.NewRequest("PUT", base+"/api/services/worker/autostart", autostartBody())
	req.Header.Set("Authorization", "Bearer tok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT with token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an authenticated configure route must reach its handler, got %d", resp.StatusCode)
	}
	if svc, _ := store.Get().findServiceByID("worker"); svc == nil || !svc.Autostart {
		t.Fatal("the configure route must have actually run on TCP")
	}

	// execute: POST /api/services is never registered on TCP at all
	// (ADR-015 decision 2) — a correct bearer cannot reach it either. The
	// mux answers 405 rather than 404 because GET /api/services IS
	// registered on that same path and nothing else claims the method: the
	// "/" catch-all is socket-only (ADR-016 decision 4) and absorbs nothing
	// here. Either way no handler ran, which the Allow header and the
	// unchanged store below both attest.
	req, _ = http.NewRequest("POST", base+"/api/services", bytes.NewBufferString(`{"display_name":"phantom","command":"/bin/true"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("an execute route must be absent from the TCP mux, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Allow") == "" {
		t.Fatal("the 405 carries no Allow header, so it is not http.ServeMux's own refusal")
	}
	if strings.Contains(resp.Header.Get("Allow"), "POST") {
		t.Fatalf("Allow = %q names POST, so some pattern claims it on TCP", resp.Header.Get("Allow"))
	}
	if svc, _ := store.Get().findServiceByID("phantom"); svc != nil {
		t.Fatal("the execute route must never have reached ServiceOps.Create on TCP")
	}
}

func TestListenLoopback_AbsentAddrBindsNothing(t *testing.T) {
	s := &FrontendServer{server: &http.Server{}}
	if err := s.ListenLoopback(""); err != nil {
		t.Fatalf("an absent address is not an error: %v", err)
	}
	if s.tcpLn != nil {
		t.Fatal("absent means no listener at all")
	}
}
