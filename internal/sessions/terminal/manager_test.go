package terminal

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
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

// TestSession_Close_DoesNotDeadlock_RawModeFullQueue reproduces F1: a target
// that puts its tty into raw mode and then never reads stdin (a wedged TUI,
// a Ctrl-Z'd process) lets the pty's input queue fill from a Write, which
// then blocks inside the write syscall. Close must still be able to tear
// the session down — closing the pty fd is what wakes the parked Write —
// rather than deadlocking on a mutex the blocked Write is holding.
func TestSession_Close_DoesNotDeadlock_RawModeFullQueue(t *testing.T) {
	cfg := testConfig(t)
	mgr := NewManager(cfg)

	sess, err := mgr.Create(CreateSpec{
		SessionID: "11111111-2222-3333-4444-555555555577",
		Directory: t.TempDir(),
		// stty raw disables canonical mode so the kernel queues bytes
		// instead of discarding them; the target then never reads stdin at
		// all, so nothing ever drains that queue.
		Argv: []string{"/bin/sh", "-c", "stty raw -echo; sleep 60"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer mgr.Close(sess.ID)

	time.Sleep(200 * time.Millisecond) // let stty actually take effect

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		chunk := bytes.Repeat([]byte{'x'}, 4096)
		for i := 0; i < 256; i++ {
			if sess.Write(chunk) != nil {
				return
			}
		}
	}()

	// Give the writer time to actually fill the queue and block inside the
	// pty write syscall before Close races it.
	time.Sleep(300 * time.Millisecond)

	closeDone := make(chan struct{})
	go func() {
		sess.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked against a raw-mode pty with a full write queue")
	}

	select {
	case <-writeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked Write call never returned after Close")
	}
}

// TestManager_Create_ConcurrentDuplicateID_NoOrphan reproduces F2: two
// concurrent Creates for the same session id must not both spawn a live
// process. The existence check and the table reservation happen under the
// same lock acquisition, so exactly one of the two racing calls must ever
// see the id as free.
func TestManager_Create_ConcurrentDuplicateID_NoOrphan(t *testing.T) {
	cfg := testConfig(t)
	mgr := NewManager(cfg)

	spec := CreateSpec{
		SessionID: "sess-race-dup",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/sh", "-c", "sleep 5"},
	}

	const n = 6
	var wg sync.WaitGroup
	results := make([]error, n)
	sessions := make([]*Session, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			s, err := mgr.Create(spec)
			results[i] = err
			sessions[i] = s
		}()
	}
	wg.Wait()

	var succeeded, refused int
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
			if sessions[i] == nil {
				t.Error("nil error but nil session")
			}
		case errors.Is(err, ErrSessionExists):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("succeeded = %d, want exactly 1 (results=%v)", succeeded, results)
	}
	if refused != n-1 {
		t.Fatalf("refused = %d, want %d", refused, n-1)
	}

	live := mgr.ListSummary()
	if len(live) != 1 {
		t.Fatalf("manager table has %d live entries, want 1: %+v", len(live), live)
	}

	for _, s := range sessions {
		if s != nil {
			mgr.Close(s.ID)
		}
	}
}

// TestManager_StopAll_DuringLaunch_LeavesNoOrphan covers the second half of
// F2: a StopAll landing while a Create is still in the Hello-wait window
// (spawned but not yet published to the table) must still end up closing
// that process, not merely ignore it because it wasn't "fully created" yet.
func TestManager_StopAll_DuringLaunch_LeavesNoOrphan(t *testing.T) {
	bridgeSock, _ := startFakeBridge(t, fakeBridgeSlowOK)
	relaySessionsBin, _ := buildBinaries(t)
	cfg := Config{ShimBinary: relaySessionsBin, LogDir: t.TempDir(), BridgeSocket: bridgeSock}
	mgr := NewManager(cfg)

	secret := strings.Repeat("c", 64)
	createErrCh := make(chan error, 1)
	createSessCh := make(chan *Session, 1)
	go func() {
		s, err := mgr.Create(CreateSpec{
			SessionID: "sess-stopall-race",
			Directory: t.TempDir(),
			Argv:      []string{"/bin/sh", "-c", "sleep 5"},
			Identity:  &IdentitySpec{Secret: secret},
		})
		createErrCh <- err
		createSessCh <- s
	}()

	// Land inside the fake bridge's artificial delay, i.e. while the slot is
	// still marked launching and there is no *Session in the table yet.
	time.Sleep(100 * time.Millisecond)
	mgr.StopAll()

	var err error
	select {
	case err = <-createErrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Create never returned after racing StopAll")
	}
	sess := <-createSessCh

	if err == nil {
		if _, ok := mgr.Get(sess.ID); ok {
			t.Fatal("a session created during StopAll must not remain live in the table")
		}
		testutil.WaitFor(t, 5*time.Second, func() bool { return !sess.Alive() })
	}

	if got := mgr.ListSummary(); len(got) != 0 {
		t.Fatalf("table not empty after StopAll raced a launch: %+v", got)
	}
}

// TestManager_Create_IdentityWithoutBridgeSocket_Refused covers F3's second
// half: an identity-bearing launch with no host-configured bridge socket has
// nothing legitimate for the shim to Hello against, so Create must refuse it
// outright rather than let whatever ends up in RELAY_BRIDGE_SOCKET decide.
func TestManager_Create_IdentityWithoutBridgeSocket_Refused(t *testing.T) {
	relaySessionsBin, _ := buildBinaries(t)
	mgr := NewManager(Config{ShimBinary: relaySessionsBin})

	_, err := mgr.Create(CreateSpec{
		SessionID: "sess-no-bridge",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/sh", "-c", "sleep 5"},
		Identity:  &IdentitySpec{Secret: strings.Repeat("d", 64)},
	})
	if !errors.Is(err, ErrNoBridgeSocket) {
		t.Fatalf("err = %v, want ErrNoBridgeSocket", err)
	}
	if _, ok := mgr.Get("sess-no-bridge"); ok {
		t.Fatal("a refused create must not leave a session in the manager's table")
	}
}

// TestBuildShimEnv_HostSocketsAlwaysWin covers F3's first half: a caller
// naming RELAY_BRIDGE_SOCKET/RELAY_MODEL_SOCKET/RELAY_SESSION_ID in Env must
// never survive the merge, even when cfg has real values to override them
// with — the host's own values must win unconditionally, not merely because
// the caller's value happened to lose a key collision.
func TestBuildShimEnv_HostSocketsAlwaysWin(t *testing.T) {
	spec := CreateSpec{
		SessionID: "sess-x",
		Env: map[string]string{
			"RELAY_BRIDGE_SOCKET": "/evil/bridge.sock",
			"RELAY_MODEL_SOCKET":  "/evil/model.sock",
			"RELAY_SESSION_ID":    "spoofed-id",
			"RELAY_ANYTHING_ELSE": "leak",
			"TERM":                "xterm-256color",
		},
	}
	cfg := Config{BridgeSocket: "/real/bridge.sock", ModelSocket: "/real/model.sock"}

	env := envMap(t, buildShimEnv(spec, cfg))
	if env["RELAY_BRIDGE_SOCKET"] != "/real/bridge.sock" {
		t.Errorf("RELAY_BRIDGE_SOCKET = %q, want the host's value", env["RELAY_BRIDGE_SOCKET"])
	}
	if env["RELAY_MODEL_SOCKET"] != "/real/model.sock" {
		t.Errorf("RELAY_MODEL_SOCKET = %q, want the host's value", env["RELAY_MODEL_SOCKET"])
	}
	if env["RELAY_SESSION_ID"] != "sess-x" {
		t.Errorf("RELAY_SESSION_ID = %q, want spec.SessionID", env["RELAY_SESSION_ID"])
	}
	if _, leaked := env["RELAY_ANYTHING_ELSE"]; leaked {
		t.Error("a caller-supplied RELAY_-prefixed key the host never sets must still be stripped")
	}
	if env["TERM"] != "xterm-256color" {
		t.Error("a non-RELAY_ key must still pass through")
	}
}

// TestBuildShimEnv_HostSocketsWin_EvenWhenHostHasNoValue is the omission
// case F3 calls out explicitly: an empty cfg (nothing for the host to
// override with) must not let a caller-supplied RELAY_BRIDGE_SOCKET/
// RELAY_MODEL_SOCKET survive by default.
func TestBuildShimEnv_HostSocketsWin_EvenWhenHostHasNoValue(t *testing.T) {
	spec := CreateSpec{
		SessionID: "sess-y",
		Env: map[string]string{
			"RELAY_BRIDGE_SOCKET": "/evil/bridge.sock",
			"RELAY_MODEL_SOCKET":  "/evil/model.sock",
		},
	}
	env := envMap(t, buildShimEnv(spec, Config{}))
	if _, ok := env["RELAY_BRIDGE_SOCKET"]; ok {
		t.Errorf("RELAY_BRIDGE_SOCKET leaked a caller value with no host override: %q", env["RELAY_BRIDGE_SOCKET"])
	}
	if _, ok := env["RELAY_MODEL_SOCKET"]; ok {
		t.Errorf("RELAY_MODEL_SOCKET leaked a caller value with no host override: %q", env["RELAY_MODEL_SOCKET"])
	}
}

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}

// TestCreateSpec_Validate_RejectsSandboxWithEmptyProfilePath covers F4: a
// Sandbox requested with an empty ProfilePath must be a hard refusal, not a
// silent no-op that runs the target unconfined.
func TestCreateSpec_Validate_RejectsSandboxWithEmptyProfilePath(t *testing.T) {
	spec := CreateSpec{
		SessionID: "sess-sandbox",
		Argv:      []string{"/bin/sh"},
		Sandbox:   &SandboxSpec{ProfilePath: ""},
	}
	if err := spec.validate(); err == nil {
		t.Fatal("want an error for a Sandbox with an empty ProfilePath")
	}
}

func TestManager_Create_RejectsSandboxWithEmptyProfilePath(t *testing.T) {
	relaySessionsBin, _ := buildBinaries(t)
	mgr := NewManager(Config{ShimBinary: relaySessionsBin})

	_, err := mgr.Create(CreateSpec{
		SessionID: "sess-sandbox-2",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/sh", "-c", "sleep 1"},
		Sandbox:   &SandboxSpec{ProfilePath: ""},
	})
	if err == nil {
		t.Fatal("want an error for a Sandbox with an empty ProfilePath")
	}
	if _, ok := mgr.Get("sess-sandbox-2"); ok {
		t.Fatal("a refused create must not leave a session in the manager's table")
	}
}
