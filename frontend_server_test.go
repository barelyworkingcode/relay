package main

import (
	"context"
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

	srv, err := NewFrontendServer(
		store,
		NewExternalMcpManager(nil),
		Endpoint{Socket: sock, Token: token},
		enhanced,
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

func TestFrontendServer_EmptyToken_DisablesAuth(t *testing.T) {
	// Dev mode: no token set → no auth check.
	_, sock := newTestFrontendServer(t, "")
	client := dialFrontendHTTP(sock)

	resp, err := client.Get("http://unix/api/anything")
	assertNoErr(t, err, "GET")
	defer resp.Body.Close()

	// 404 is fine — proves we got past the auth layer to the dispatcher.
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("empty token must skip auth; got 401")
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

// Sanity: helper strings stay grep-able under refactor.
var _ = strings.TrimSpace
