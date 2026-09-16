package terminal

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	relaySessionsBin, _ := buildBinaries(t)
	return Config{ShimBinary: relaySessionsBin, LogDir: t.TempDir()}
}

// TestManager_LocalPTY_ExitAndLog spawns a real shell through the shim under
// a real pty, asserting the captured exit code, in-memory scrollback and
// on-disk log persistence — the ported equivalent of relayLLM's
// TestTerminalSession_PTYExitAndLog, now going through Manager+shim instead
// of a direct pty.StartWithSize.
func TestManager_LocalPTY_ExitAndLog(t *testing.T) {
	cfg := testConfig(t)
	mgr := NewManager(cfg)

	exitCh := make(chan int, 1)
	mgr.SetExitHandler(func(_ string, code int) { exitCh <- code })

	sess, err := mgr.Create(CreateSpec{
		// A real UUID, not a slug: log path validation (isValidTerminalID,
		// guarding against path traversal when a terminal id is later served
		// over HTTP) requires the UUID shape C5's session_id always has in
		// production (relay mints it).
		SessionID: "11111111-2222-3333-4444-555555555555",
		Name:      "test",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/sh", "-c", "echo hi-from-pty; exit 7"},
		Cols:      80,
		Rows:      24,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	select {
	case code := <-exitCh:
		if code != 7 {
			t.Fatalf("exit code = %d, want 7", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for exit")
	}

	deadline := time.Now().Add(2 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = readTerminalLog(cfg.LogDir, sess.ID)
		if bytes.Contains(data, []byte("hi-from-pty")) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Contains(data, []byte("hi-from-pty")) {
		t.Fatalf("log file missing expected output: %q", data)
	}
	if !bytes.Contains(sess.ScrollbackBytes(), []byte("hi-from-pty")) {
		t.Fatal("scrollback missing expected output")
	}
}

// TestManager_Create_SessionExists covers Manager's own duplicate-id guard,
// mirroring C5's 409 session_exists at this package's level.
func TestManager_Create_SessionExists(t *testing.T) {
	cfg := testConfig(t)
	mgr := NewManager(cfg)

	spec := CreateSpec{
		SessionID: "sess-dup",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/sh", "-c", "sleep 2"},
	}
	sess, err := mgr.Create(spec)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	_, err = mgr.Create(spec)
	if err == nil {
		t.Fatal("second Create with the same session id: want an error, got nil")
	}
	if !strings.Contains(err.Error(), ErrSessionExists.Error()) {
		t.Fatalf("error = %v, want to wrap ErrSessionExists", err)
	}
}

// TestManager_PTYLaunch_IdentityHelloViaFakeBridge covers a project-scoped
// (identity-bearing) pty launch end to end: a real fake bridge answers the
// shim's Hello, the shim reports hello_ok then started, and the resulting
// session is live. This is the "pty launch via shim with a fake bridge"
// case the plan's own required-test list names — hostapi itself cannot
// exercise this yet (it hard-refuses every pty LaunchRequest, doc.go), so
// this package's own Manager is what proves the shim integration works.
func TestManager_PTYLaunch_IdentityHelloViaFakeBridge(t *testing.T) {
	bridgeSock, received := startFakeBridge(t, fakeBridgeOK)
	relaySessionsBin, _ := buildBinaries(t)
	cfg := Config{ShimBinary: relaySessionsBin, LogDir: t.TempDir(), BridgeSocket: bridgeSock}
	mgr := NewManager(cfg)

	secret := strings.Repeat("a", 64)
	sess, err := mgr.Create(CreateSpec{
		SessionID: "sess-identity",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/sh", "-c", "sleep 5"},
		Identity:  &IdentitySpec{Secret: secret},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	select {
	case got := <-received:
		if got.Type != "Hello" || got.Kind != "project_session" || got.Name != sess.ID || got.Token != secret {
			t.Fatalf("hello = %+v, want {Hello project_session %s %s}", got, sess.ID, secret)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake bridge never received a Hello")
	}

	if !sess.Alive() {
		t.Fatal("session must be alive after a successful identity Hello")
	}
}

// TestManager_PTYLaunch_HelloRefused_IdentityRefusedError covers the other
// side: a bridge that refuses the Hello must fail Create with
// ErrIdentityRefused and never leave a live session behind.
func TestManager_PTYLaunch_HelloRefused_IdentityRefusedError(t *testing.T) {
	bridgeSock, _ := startFakeBridge(t, fakeBridgeRefuse)
	relaySessionsBin, _ := buildBinaries(t)
	cfg := Config{ShimBinary: relaySessionsBin, BridgeSocket: bridgeSock}
	mgr := NewManager(cfg)

	_, err := mgr.Create(CreateSpec{
		SessionID: "sess-refused",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/sh", "-c", "sleep 5"},
		Identity:  &IdentitySpec{Secret: strings.Repeat("b", 64)},
	})
	if err == nil {
		t.Fatal("want an error when the bridge refuses Hello")
	}
	if !strings.Contains(err.Error(), ErrIdentityRefused.Error()) {
		t.Fatalf("error = %v, want to wrap ErrIdentityRefused", err)
	}
	if _, ok := mgr.Get("sess-refused"); ok {
		t.Fatal("a refused launch must not leave a session in the manager's table")
	}
}

// TestManager_IdleTimeout proves a viewerless session is closed once its
// idle timeout elapses, driven by a FakeClock so the test never waits on
// real time.
func TestManager_IdleTimeout(t *testing.T) {
	fc := testutil.NewFakeClock(time.Unix(0, 0))
	relaySessionsBin, _ := buildBinaries(t)
	cfg := Config{ShimBinary: relaySessionsBin, Clock: fc}
	mgr := NewManager(cfg)

	sess, err := mgr.Create(CreateSpec{
		SessionID:   "sess-idle",
		Directory:   t.TempDir(),
		Argv:        []string{"/bin/sh", "-c", "sleep 100"},
		IdleTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	mgr.NotifyViewerChange(sess.ID, 0) // no viewers: arm the idle timer
	testutil.WaitFor(t, time.Second, func() bool { return fc.Waiters() >= 1 })

	fc.Advance(5 * time.Minute)

	testutil.WaitFor(t, 5*time.Second, func() bool {
		_, ok := mgr.Get(sess.ID)
		return !ok
	})
}

// TestManager_IdleTimeout_CancelledByViewer proves a viewer showing up
// before the deadline prevents the close.
func TestManager_IdleTimeout_CancelledByViewer(t *testing.T) {
	fc := testutil.NewFakeClock(time.Unix(0, 0))
	relaySessionsBin, _ := buildBinaries(t)
	cfg := Config{ShimBinary: relaySessionsBin, Clock: fc}
	mgr := NewManager(cfg)

	sess, err := mgr.Create(CreateSpec{
		SessionID:   "sess-idle-cancel",
		Directory:   t.TempDir(),
		Argv:        []string{"/bin/sh", "-c", "sleep 100"},
		IdleTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	mgr.NotifyViewerChange(sess.ID, 0)
	testutil.WaitFor(t, time.Second, func() bool { return fc.Waiters() >= 1 })
	mgr.NotifyViewerChange(sess.ID, 1) // a viewer joined: cancel the timer

	fc.Advance(5 * time.Minute)
	time.Sleep(50 * time.Millisecond) // give a wrongly-still-armed timer a chance to fire

	if _, ok := mgr.Get(sess.ID); !ok {
		t.Fatal("a session with a connected viewer must not be closed by the idle timer")
	}
}

// TestSession_CreatedBody_MatchesGoldenTerminalCreatedFrame pins C5's own
// rule: "For a terminal [the 201 body] equals relayLLM's WS terminal_created
// frame minus type." The golden map below is relayLLM's actual current
// frame (internal/api/ws.go's handleTerminalCreate sendJSON call) with the
// "type" key removed — copied by hand from that file, not re-derived, so a
// change to relayLLM's wire shape must be caught by a human updating both
// sides rather than this test silently tracking whatever this package
// happens to produce.
func TestSession_CreatedBody_MatchesGoldenTerminalCreatedFrame(t *testing.T) {
	cfg := testConfig(t)
	mgr := NewManager(cfg)

	sess, err := mgr.Create(CreateSpec{
		SessionID:  "sess-golden",
		TemplateID: "shell",
		Name:       "my shell",
		Directory:  "/tmp/proj",
		Argv:       []string{"/bin/sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	gotBytes, err := json.Marshal(sess.CreatedBody())
	if err != nil {
		t.Fatalf("marshal CreatedBody: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(gotBytes, &got); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}

	golden := map[string]any{
		"terminalId": "sess-golden",
		"templateId": "shell",
		"name":       "my shell",
		"directory":  "/tmp/proj",
		"host":       nil,
	}
	if len(got) != len(golden) {
		t.Fatalf("field count = %d, want %d (got=%v)", len(got), len(golden), got)
	}
	for k, want := range golden {
		if gv, ok := got[k]; !ok || gv != want {
			t.Errorf("field %q = %v, want %v", k, gv, want)
		}
	}
}
