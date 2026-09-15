package hostapi_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
)

// startServer starts a real hostapi.Server on temp Unix sockets with
// RelayPID set to this test process's own pid — the test process is the
// "relay" that dials the internal socket, so its own peer token is exactly
// what a real relay's would be, without needing a second process. Tests
// that specifically exercise the wrong-peer-pid rejection configure a
// deliberately wrong RelayPID instead of using this helper.
func startServer(t *testing.T, relayPID int) (srv *hostapi.Server, internalSock, hookSock, bearer string) {
	t.Helper()
	relaySessionsBin, _ := buildBinaries(t)
	dir := mkShortTempDir(t, "hostapi-")
	internalSock = filepath.Join(dir, "internal.sock")
	hookSock = filepath.Join(dir, "hook.sock")
	bearer = "test-bearer-secret"

	srv = hostapi.New(hostapi.Config{
		InternalSocket: internalSock,
		InternalBearer: bearer,
		RelayPID:       relayPID,
		HookSocket:     hookSock,
		ShimBinary:     relaySessionsBin,
	})
	if err := srv.ListenInternal(); err != nil {
		t.Fatalf("ListenInternal: %v", err)
	}
	if err := srv.ListenHook(); err != nil {
		t.Fatalf("ListenHook: %v", err)
	}
	go func() { _ = srv.ServeInternal() }()
	go func() { _ = srv.ServeHook() }()
	t.Cleanup(srv.Close)
	return srv, internalSock, hookSock, bearer
}

func launchBody(sessionID string, argv []string) map[string]any {
	return map[string]any{
		"v":          1,
		"session_id": sessionID,
		"kind":       "pty",
		"argv":       argv,
	}
}

// TestLaunch_WrongBearer_Forbidden covers C5's host-side mutual check: a
// bearer that doesn't match must be refused with an empty 403, never a
// launch attempt.
func TestLaunch_WrongBearer_Forbidden(t *testing.T) {
	_, target := buildBinaries(t)
	_, internalSock, _, _ := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	resp := postJSON(t, client, "http://h/launch", "not-the-real-bearer", launchBody("s1", []string{target}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	buf := make([]byte, 1)
	if n, _ := resp.Body.Read(buf); n != 0 {
		t.Fatalf("403 body is not empty")
	}
}

// TestLaunch_WrongPeerPID_Forbidden covers the other half of the mutual
// check: even with the correct bearer, a caller whose peer pid doesn't
// match relay_pid must be refused.
func TestLaunch_WrongPeerPID_Forbidden(t *testing.T) {
	_, target := buildBinaries(t)
	wrongPID := os.Getpid() + 999999 // this test process's real pid can never equal this
	_, internalSock, _, bearer := startServer(t, wrongPID)
	client := unixClient(internalSock)

	resp := postJSON(t, client, "http://h/launch", bearer, launchBody("s1", []string{target}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestLaunch_FullPath_Succeeds is the "fake/minimal caller can successfully
// launch a shim via the full path" test: a real HTTP POST /launch, over a
// real Unix socket, spawning a real internal/sessions/shim child that
// spawns a real testtarget, ending in the documented 201 response shape.
func TestLaunch_FullPath_Succeeds(t *testing.T) {
	_, target := buildBinaries(t)
	dir := mkShortTempDir(t, "launch-ok-")
	marker := filepath.Join(dir, "marker.json")

	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	resp := postJSON(t, client, "http://h/launch", bearer, launchBody("sess-ok", []string{
		target, "-marker", marker, "-sleep", "200ms", "-exit-code", "0",
	}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var out hostapi.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.SessionID != "sess-ok" {
		t.Fatalf("session_id = %q, want sess-ok", out.SessionID)
	}
	if out.RootPID <= 0 {
		t.Fatalf("root_pid = %d, want > 0", out.RootPID)
	}
	var body map[string]any
	if err := json.Unmarshal(out.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["session_id"] != "sess-ok" || body["kind"] != "pty" {
		t.Fatalf("body = %+v, missing echoed session_id/kind", body)
	}

	waitForFile(t, marker, 5)
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var info struct {
		PID  int `json:"pid"`
		PPID int `json:"ppid"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	// root_pid is the shim's own pid (SH §4.2), never the target's: the
	// target is the shim's child, so its recorded ppid is what must match.
	if info.PPID != out.RootPID {
		t.Fatalf("target's ppid %d != response root_pid %d (root_pid must be the shim, not the target)", info.PPID, out.RootPID)
	}
	if info.PID == out.RootPID {
		t.Fatalf("target pid %d must not equal root_pid %d (they are different processes)", info.PID, out.RootPID)
	}
}

// TestLaunch_SessionExists covers C5's 409.
func TestLaunch_SessionExists(t *testing.T) {
	_, target := buildBinaries(t)
	dir := mkShortTempDir(t, "launch-dup-")
	marker := filepath.Join(dir, "marker.json")

	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	body := launchBody("sess-dup", []string{target, "-marker", marker, "-sleep", "500ms", "-exit-code", "0"})
	resp1 := postJSON(t, client, "http://h/launch", bearer, body)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first launch status = %d, want 201", resp1.StatusCode)
	}

	resp2 := postJSON(t, client, "http://h/launch", bearer, body)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("second launch status = %d, want 409", resp2.StatusCode)
	}
	var errBody hostapi.ErrorResponse
	if err := json.NewDecoder(resp2.Body).Decode(&errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Error != hostapi.ErrSessionExists {
		t.Fatalf("error = %q, want %q", errBody.Error, hostapi.ErrSessionExists)
	}
}

// TestLaunch_InvalidSpec covers 400s for the fields this skeleton validates.
func TestLaunch_InvalidSpec(t *testing.T) {
	_, target := buildBinaries(t)
	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	cases := map[string]map[string]any{
		"missing_v":          {"session_id": "s", "kind": "pty", "argv": []string{target}},
		"missing_session_id": {"v": 1, "kind": "pty", "argv": []string{target}},
		"missing_kind":       {"v": 1, "session_id": "s", "argv": []string{target}},
		"missing_argv":       {"v": 1, "session_id": "s", "kind": "pty"},
		"pty_requested":      {"v": 1, "session_id": "s", "kind": "pty", "argv": []string{target}, "pty": map[string]any{"cols": 80, "rows": 24}},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := postJSON(t, client, "http://h/launch", bearer, body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var errBody hostapi.ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if errBody.Error != hostapi.ErrInvalidSpec {
				t.Fatalf("error = %q, want %q", errBody.Error, hostapi.ErrInvalidSpec)
			}
		})
	}
}

func waitForFile(t *testing.T, path string, timeoutSeconds int) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
