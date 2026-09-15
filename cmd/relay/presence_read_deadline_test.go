package main

// This file reproduces and pins the fix for the presence-gate/read-deadline
// race described in requireGate's doc comment: frontendRouteReadDeadline
// bounds how long relay waits to receive a request BODY, but before the
// fix it also silently bounded a presence-gated handler's ENTIRE execution,
// including a human-timescale LocalAuthentication wait, because the same
// connection-level deadline feeds net/http's background close-detection
// reader that cancels r.Context() on read failure. A real human taking
// longer than the deadline to answer the dialog got "context canceled"
// while the dialog itself stayed on screen, orphaned.
//
// Every test here goes through a real FrontendServer on a real Unix socket
// (NewFrontendServer + Serve), not a bare http.ServeMux, so the whole chain
// — frontendCredentialAuth, withRelayRouteReadDeadline, ProjectOps.RotateToken,
// requireGate, presence.Gate.Require — runs exactly as production wires it.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// sleepyPresenceProvider mirrors LocalAuthProvider.Evaluate's own shape
// (internal/presence/localauth_darwin.go): block on a timer but still honor
// ctx.Done(). It stands in for a real human taking sleep to answer the
// macOS dialog, without needing a console or a real LocalAuthentication
// call.
type sleepyPresenceProvider struct{ sleep time.Duration }

func (p sleepyPresenceProvider) Evaluate(ctx context.Context, _ string) error {
	select {
	case <-time.After(p.sleep):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newPresenceDeadlineTestProject seeds a single project directly (bypassing
// the gated Create path, which would itself go through provider) and
// returns a ProjectOps whose Gate uses provider.
func newPresenceDeadlineTestProject(t *testing.T, provider presence.Provider) (store config.SettingsStore, ops *ProjectOps, projectID string) {
	t.Helper()
	projectID = "pgd-project"
	store = sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if err := store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects, config.Project{ID: projectID, Name: projectID})
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	gate, err := presence.NewGate(provider)
	if err != nil {
		t.Fatalf("presence.NewGate: %v", err)
	}
	ops = &ProjectOps{Store: store, Gate: gate, Issuance: enabledIssuanceRecorder(t)}
	return store, ops, projectID
}

// newRotateTokenTestServer wires ops into a real FrontendServer listening on
// a real Unix socket, and returns the socket path and a bearer that
// authenticates against it.
func newRotateTokenTestServer(t *testing.T, store config.SettingsStore, ops *ProjectOps) (sock, bearer string) {
	t.Helper()
	dir := mkShortTempDir(t, "pgd-")
	sock = filepath.Join(dir, "frontend.sock")
	bearer = "pgd-token"
	extMgr := mcpbroker.NewManager(nil)
	enhanced := NewEnhancedServiceRegistry(nil)

	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		seededEndpoint(t, store, sock, bearer),
		enhanced,
		nil, nil, nil, nil, nil, nil,
		ops,
		nil, nil, nil, nil, nil, nil,
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
	return sock, bearer
}

// TestRotateToken_SurvivesReadDeadlineDuringHumanPresenceWait is the
// reproduction: frontendRouteReadDeadline is shortened well below the
// presence wait, so the pre-fix code (which passed r.Context() straight
// into gate.Require with no suspension) fails this with "context
// canceled" every time. Post-fix, requireGate suspends the deadline for
// the wait, so the request succeeds once the (still slower) presence
// prompt resolves.
func TestRotateToken_SurvivesReadDeadlineDuringHumanPresenceWait(t *testing.T) {
	origDeadline := frontendRouteReadDeadline
	frontendRouteReadDeadline = 200 * time.Millisecond
	t.Cleanup(func() { frontendRouteReadDeadline = origDeadline })

	presenceWait := 2 * frontendRouteReadDeadline
	provider := sleepyPresenceProvider{sleep: presenceWait}
	store, ops, projectID := newPresenceDeadlineTestProject(t, provider)
	sock, bearer := newRotateTokenTestServer(t, store, ops)

	client := dialFrontendHTTP(sock)
	req, err := http.NewRequest(http.MethodPost, "http://unix/api/projects/"+projectID+"/rotate_token", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("rotate_token request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate_token must succeed once presence returns, even though the wait (%s) exceeded the read deadline (%s); got %d body=%s after %s",
			presenceWait, frontendRouteReadDeadline, resp.StatusCode, body, elapsed)
	}
	if elapsed < presenceWait {
		t.Fatalf("response returned after %s, before the %s presence wait finished — this test is not exercising a real cross-deadline wait", elapsed, presenceWait)
	}
	var decoded map[string]string
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, body)
	}
	if decoded["token"] == "" {
		t.Fatalf("expected a rotated token in the response body, got %s", body)
	}
}

// TestSlowBodyUpload_StillHitsReadDeadlineOnUngatedRoute is the "don't just
// delete the protection" check: an ungated route (POST /api/audit/export,
// which decodes a JSON body but never reaches requireGate) must still be
// cut off close to frontendRouteReadDeadline when the body never finishes
// arriving, exactly as before this fix.
func TestSlowBodyUpload_StillHitsReadDeadlineOnUngatedRoute(t *testing.T) {
	origDeadline := frontendRouteReadDeadline
	frontendRouteReadDeadline = 150 * time.Millisecond
	t.Cleanup(func() { frontendRouteReadDeadline = origDeadline })

	dir := mkShortTempDir(t, "pgd-body-")
	sock := filepath.Join(dir, "frontend.sock")
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	rec := enabledIssuanceRecorder(t)
	auditOps := &audit.AuditOps{Audit: rec}
	extMgr := mcpbroker.NewManager(nil)
	enhanced := NewEnhancedServiceRegistry(nil)
	bearer := "pgd-body-token"

	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		seededEndpoint(t, store, sock, bearer),
		enhanced,
		nil, nil, nil, nil,
		auditOps,
		nil, nil, nil, nil, nil, nil, nil, nil,
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

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`{"project_id":"x"`)) // never closes the object: body never completes
		time.Sleep(2 * time.Second)
		_ = pw.CloseWithError(errors.New("test: body deliberately never completes"))
	}()

	client := dialFrontendHTTP(sock)
	req, err := http.NewRequest(http.MethodPost, "http://unix/api/audit/export", pr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, doErr := client.Do(req)
	elapsed := time.Since(start)
	if resp != nil {
		defer resp.Body.Close()
	}

	if elapsed > time.Second {
		t.Fatalf("a slow/hanging body upload on an ungated route must still be cut off near frontendRouteReadDeadline (%s); took %s (response err=%v)",
			frontendRouteReadDeadline, elapsed, doErr)
	}
	if doErr == nil && resp.StatusCode == http.StatusOK {
		t.Fatalf("expected the never-completing body to fail, got 200 OK")
	}
}

// TestRotateToken_RealClientDisconnectStillCancelsPresenceWait is the other
// half of the "don't defeat the protection" requirement: suspending the
// artificial deadline during a presence wait must not stop a GENUINE
// connection close from cancelling that wait. It writes the request
// directly onto a raw Unix socket connection (so it controls exactly when
// the connection closes) and closes it itself shortly after the request
// reaches the presence prompt, then asserts Evaluate observed ctx.Done()
// rather than running to completion.
func TestRotateToken_RealClientDisconnectStillCancelsPresenceWait(t *testing.T) {
	resultCh := make(chan error, 1)
	provider := recordingSlowPresenceProvider{sleep: 5 * time.Second, resultCh: resultCh}
	store, ops, projectID := newPresenceDeadlineTestProject(t, provider)
	sock, bearer := newRotateTokenTestServer(t, store, ops)

	conn := dialUnixWithTimeout(t, sock, 2*time.Second)
	req, err := http.NewRequest(http.MethodPost, "http://unix/api/projects/"+projectID+"/rotate_token", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Give the handler time to reach requireGate and start blocking in
	// Evaluate's select before pulling the connection out from under it.
	time.Sleep(150 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatalf("close connection: %v", err)
	}

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a real client disconnect must cancel the presence wait with context.Canceled, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the presence wait was not cancelled by the client closing the connection")
	}
}

// recordingSlowPresenceProvider is sleepyPresenceProvider plus a channel
// that reports which branch of Evaluate's select fired, since the disconnect
// test has no other way to observe what happened inside the handler
// goroutine (RotateToken never completes on this path, so there is no HTTP
// response to inspect).
type recordingSlowPresenceProvider struct {
	sleep    time.Duration
	resultCh chan error
}

func (p recordingSlowPresenceProvider) Evaluate(ctx context.Context, _ string) error {
	select {
	case <-time.After(p.sleep):
		p.resultCh <- nil
		return nil
	case <-ctx.Done():
		p.resultCh <- ctx.Err()
		return ctx.Err()
	}
}
