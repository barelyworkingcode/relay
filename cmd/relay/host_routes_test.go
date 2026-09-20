package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// stubSSHRunner replaces sshhost's real exec seam for the duration of a
// test, so exercising HostOps/host_routes.go never spawns a real ssh
// process (docs/ssh-hosts.md: hermetic tests must not shell out).
func stubSSHRunner(t *testing.T, fn func(ctx context.Context, name string, args []string) ([]byte, []byte, error)) {
	t.Helper()
	t.Cleanup(sshhost.SetRunnerForTest(fn))
}

func mustUnmarshal(t *testing.T, body []byte, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
}

func newHostRoutesServer(t *testing.T) (*httptest.Server, config.SettingsStore, *HostOps) {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	ops := &HostOps{Store: store, Auditor: enabledIssuanceRecorder(t)}
	mux := http.NewServeMux()
	RegisterHostRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, ops)
	return httptest.NewServer(mux), store, ops
}

func TestHostRoutes_CreateProbesSynchronouslyAndGet(t *testing.T) {
	stubSSHRunner(t, func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		return []byte(cannedProbeOutputForTest), nil, nil
	})
	srv, _, _ := newHostRoutesServer(t)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/hosts", map[string]any{
		"name": "devbox", "target": "admin@devbox.local",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var created hostView
	mustUnmarshal(t, body, &created)
	if created.ID == "" || created.Name != "devbox" {
		t.Fatalf("unexpected created host: %+v", created)
	}
	if created.Probe == nil || !created.Probe.OK {
		t.Fatalf("expected a successful synchronous probe, got %+v", created.Probe)
	}
	if created.Probe.ClaudePath == "" {
		t.Fatalf("expected claude_path to be discovered, got %+v", created.Probe)
	}
	if len(created.SSHArgv) == 0 || created.SSHArgv[0] != "ssh" {
		t.Fatalf("expected an ssh_argv prefix, got %v", created.SSHArgv)
	}

	resp2, body2 := doJSON(t, "GET", srv.URL+"/api/hosts/"+created.ID, nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", resp2.StatusCode, body2)
	}
	var fetched hostView
	mustUnmarshal(t, body2, &fetched)
	if fetched.ID != created.ID {
		t.Fatalf("expected the same host back, got %+v", fetched)
	}
}

func TestHostRoutes_CreateValidation(t *testing.T) {
	srv, _, _ := newHostRoutesServer(t)
	defer srv.Close()

	resp, _ := doJSON(t, "POST", srv.URL+"/api/hosts", map[string]any{"name": "", "target": "x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty name, got %d", resp.StatusCode)
	}
}

func TestHostRoutes_List(t *testing.T) {
	stubSSHRunner(t, func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		return []byte(cannedProbeOutputForTest), nil, nil
	})
	srv, _, _ := newHostRoutesServer(t)
	defer srv.Close()

	doJSON(t, "POST", srv.URL+"/api/hosts", map[string]any{"name": "a", "target": "admin@a"})
	doJSON(t, "POST", srv.URL+"/api/hosts", map[string]any{"name": "b", "target": "admin@b"})

	resp, body := doJSON(t, "GET", srv.URL+"/api/hosts", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var list []hostView
	mustUnmarshal(t, body, &list)
	if len(list) != 2 {
		t.Fatalf("expected 2 hosts, got %d: %+v", len(list), list)
	}
}

func TestHostRoutes_UpdateReprobesOnlyWhenConnectionChanges(t *testing.T) {
	calls := 0
	stubSSHRunner(t, func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		// hostToView renders a live "-O check" on every response; only a
		// real discovery/version call (carrying "-T") counts as a probe.
		if argvContains(args, "-T") {
			calls++
		}
		return []byte(cannedProbeOutputForTest), nil, nil
	})
	srv, _, _ := newHostRoutesServer(t)
	defer srv.Close()

	_, body := doJSON(t, "POST", srv.URL+"/api/hosts", map[string]any{"name": "devbox", "target": "admin@devbox.local"})
	var created hostView
	mustUnmarshal(t, body, &created)
	callsAfterCreate := calls // create's synchronous probe(s) — node + claude version calls too
	if callsAfterCreate == 0 {
		t.Fatal("expected create to have probed at least once")
	}

	// A rename alone must not re-probe.
	resp, body2 := doJSON(t, "PUT", srv.URL+"/api/hosts/"+created.ID, map[string]any{"name": "devbox2"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", resp.StatusCode, body2)
	}
	if calls != callsAfterCreate {
		t.Fatalf("expected no re-probe on a rename, calls went from %d to %d", callsAfterCreate, calls)
	}

	// Changing the target must re-probe.
	resp3, body3 := doJSON(t, "PUT", srv.URL+"/api/hosts/"+created.ID, map[string]any{"target": "admin@devbox2.local"})
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("PUT status = %d, body = %s", resp3.StatusCode, body3)
	}
	if calls <= callsAfterCreate {
		t.Fatalf("expected a re-probe after changing target, calls stayed at %d", calls)
	}
}

func TestHostRoutes_DeleteRefusedWhileReferenced(t *testing.T) {
	stubSSHRunner(t, func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		return []byte(cannedProbeOutputForTest), nil, nil
	})
	srv, store, _ := newHostRoutesServer(t)
	defer srv.Close()

	_, body := doJSON(t, "POST", srv.URL+"/api/hosts", map[string]any{"name": "devbox", "target": "admin@devbox.local"})
	var created hostView
	mustUnmarshal(t, body, &created)

	store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects, config.Project{ID: "p1", Name: "relayfs", HostID: created.ID})
	})

	resp, respBody := doJSON(t, "DELETE", srv.URL+"/api/hosts/"+created.ID, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), "relayfs") {
		t.Fatalf("expected the referencing project to be named, got %s", respBody)
	}

	// Clear the reference and it should succeed.
	store.With(func(s *config.Settings) { s.Projects[0].HostID = "" })
	resp2, _ := doJSON(t, "DELETE", srv.URL+"/api/hosts/"+created.ID, nil)
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 after clearing the reference, got %d", resp2.StatusCode)
	}
}

func TestHostRoutes_ProbeAndDisconnect(t *testing.T) {
	var allCalls [][]string
	stubSSHRunner(t, func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		allCalls = append(allCalls, args)
		if argvContains(args, "check") || argvContains(args, "exit") {
			return nil, nil, nil
		}
		return []byte(cannedProbeOutputForTest), nil, nil
	})
	srv, _, _ := newHostRoutesServer(t)
	defer srv.Close()

	_, body := doJSON(t, "POST", srv.URL+"/api/hosts", map[string]any{"name": "devbox", "target": "admin@devbox.local"})
	var created hostView
	mustUnmarshal(t, body, &created)

	resp, body2 := doJSON(t, "POST", srv.URL+"/api/hosts/"+created.ID+"/probe", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe status = %d, body = %s", resp.StatusCode, body2)
	}

	resp2, body3 := doJSON(t, "POST", srv.URL+"/api/hosts/"+created.ID+"/disconnect", nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("disconnect status = %d, body = %s", resp2.StatusCode, body3)
	}
	found := false
	for _, args := range allCalls {
		if argvContains(args, "exit") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected one call to reach ssh -O exit, got calls %v", allCalls)
	}
}

func argvContains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestHostRoutes_NotFound(t *testing.T) {
	srv, _, _ := newHostRoutesServer(t)
	defer srv.Close()

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/hosts/nope", nil},
		{"PUT", "/api/hosts/nope", map[string]any{}},
		{"DELETE", "/api/hosts/nope", nil},
		{"POST", "/api/hosts/nope/probe", nil},
		{"POST", "/api/hosts/nope/disconnect", nil},
	} {
		resp, body := doJSON(t, tc.method, srv.URL+tc.path, tc.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404, body=%s", tc.method, tc.path, resp.StatusCode, body)
		}
	}
}

func TestHostRoutes_CommitEventFiresOnMutation(t *testing.T) {
	stubSSHRunner(t, func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		return []byte(cannedProbeOutputForTest), nil, nil
	})
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	var fired atomic.Int64
	ops := &HostOps{Store: store, Queue: commitQueueFor(t, store, &fired)}
	mux := http.NewServeMux()
	RegisterHostRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, ops)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, body := doJSON(t, "POST", srv.URL+"/api/hosts", map[string]any{"name": "devbox", "target": "admin@devbox.local"})
	if fired.Load() == 0 {
		t.Fatal("expected a commit event on create")
	}
	var created hostView
	mustUnmarshal(t, body, &created)

	before := fired.Load()
	doJSON(t, "DELETE", srv.URL+"/api/hosts/"+created.ID, nil)
	if fired.Load() <= before {
		t.Fatal("expected a commit event on delete")
	}
}

// cannedProbeOutputForTest mirrors internal/sshhost's own canned fixture —
// duplicated here rather than exported, since only the sentinel SHAPE
// matters to this file's tests, not sshhost's internal constant values.
const cannedProbeOutputForTest = `@@RELAY_PROBE_OS@@
Darwin
@@RELAY_PROBE_ARCH@@
arm64
@@RELAY_PROBE_HOME@@
/Users/admin
@@RELAY_PROBE_SHELL@@
/bin/zsh
@@RELAY_PROBE_LOGIN_NODE@@
/opt/homebrew/bin/node
@@RELAY_PROBE_LOGIN_CLAUDE@@
/opt/homebrew/bin/claude
@@RELAY_PROBE_PLAIN_NODE@@
/opt/homebrew/bin/node
@@RELAY_PROBE_PLAIN_CLAUDE@@
/opt/homebrew/bin/claude
@@RELAY_PROBE_END@@
`
