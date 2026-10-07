package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
)

const originSessionID = "33333333-3333-3333-3333-333333333333"

// startOriginHost runs the real relay-sessions launch door on a unix socket,
// the way relay reaches it, over a session manager with a stand-in provider.
func startOriginHost(t *testing.T) (client *http.Client, bearer string, mgr *session.Manager) {
	t.Helper()
	mgr, _ = newTestManager(t)
	mgr.SetProviderFactory(factoryReturning(&fakeProvider{}))
	dir, err := os.MkdirTemp("/tmp", "origin-host-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "internal.sock")
	bearer = "test-bearer-secret"
	srv := hostapi.New(hostapi.Config{
		InternalSocket: sock, InternalBearer: bearer, RelayPID: os.Getpid(), HookSocket: filepath.Join(dir, "hook.sock"),
	}, terminal.NewManager(terminal.Config{LogDir: filepath.Join(dir, "logs")}), mgr)
	if err := srv.ListenInternal(); err != nil {
		t.Fatalf("ListenInternal: %v", err)
	}
	go func() { _ = srv.ServeInternal() }()
	t.Cleanup(srv.Close)
	client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	return client, bearer, mgr
}

func postLaunch(t *testing.T, client *http.Client, bearer string, body map[string]any) (int, hostapi.ErrorResponse) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, "http://h/launch", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /launch: %v", err)
	}
	defer resp.Body.Close()
	var e hostapi.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&e)
	return resp.StatusCode, e
}

func claudeLaunch(origin string) map[string]any {
	b := map[string]any{
		"v": 1, "session_id": originSessionID, "kind": "claude",
		"session_request": map[string]any{
			"projectId": "proj-1", "directory": "/tmp/acme", "model": "haiku",
			"settings": map[string]any{"headless": true, "agent": true},
		},
	}
	if origin != "" {
		b["origin"] = origin
	}
	return b
}

func TestLaunchOrigin_HostRefusesAnyOriginButChiefOfStaff(t *testing.T) {
	client, bearer, mgr := startOriginHost(t)
	status, e := postLaunch(t, client, bearer, claudeLaunch("person"))
	if status != http.StatusBadRequest || e.Error != hostapi.ErrInvalidSpec {
		t.Fatalf("got %d %+v, want 400 %s", status, e, hostapi.ErrInvalidSpec)
	}
	if _, ok := mgr.Get(originSessionID); ok {
		t.Fatal("the host created a session for a refused origin")
	}
}

func TestLaunchOrigin_ChiefOfStaffReachesTheSessionAndItsListRow(t *testing.T) {
	client, bearer, mgr := startOriginHost(t)
	if status, e := postLaunch(t, client, bearer, claudeLaunch("chief-of-staff")); status != http.StatusCreated {
		t.Fatalf("status = %d (%+v), want 201", status, e)
	}
	sess, ok := mgr.Get(originSessionID)
	if !ok || sess.Origin != "chief-of-staff" {
		t.Fatalf("session = %+v, ok = %v, want origin chief-of-staff", sess, ok)
	}
	rows := mgr.List()
	if len(rows) != 1 {
		t.Fatalf("List = %+v, want the one agent session", rows)
	}
	raw, _ := json.Marshal(rows[0])
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if wire["origin"] != "chief-of-staff" {
		t.Fatalf("list row = %s, want origin chief-of-staff", raw)
	}
}

func TestLaunchOrigin_PersonSessionRowHasNoOrigin(t *testing.T) {
	client, bearer, mgr := startOriginHost(t)
	if status, e := postLaunch(t, client, bearer, claudeLaunch("")); status != http.StatusCreated {
		t.Fatalf("status = %d (%+v), want 201", status, e)
	}
	raw, _ := json.Marshal(mgr.List())
	var rows []map[string]any
	_ = json.Unmarshal(raw, &rows)
	if len(rows) != 1 {
		t.Fatalf("List = %s, want one row", raw)
	}
	if _, has := rows[0]["origin"]; has {
		t.Fatalf("a person's session row carries origin: %s", raw)
	}
}
