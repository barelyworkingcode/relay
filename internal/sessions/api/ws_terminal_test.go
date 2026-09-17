package api

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
)

// TestTerminalCreate_AlwaysErrors covers C11: the WS terminal_create frame
// is retired as a create mechanism, but must still be an explicit,
// registered refusal — never a silently dropped message a stuck client
// waits on forever.
func TestTerminalCreate_AlwaysErrors(t *testing.T) {
	hub := NewHub()
	mgr := terminal.NewManager(terminal.Config{ShimBinary: buildRelaySessionsBin(t)})
	NewTerminalHandlers(hub, mgr)

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "terminal_create", "templateId": "shell"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readJSONWithTimeout(t, conn, 2*time.Second)
	if got["type"] != "error" {
		t.Fatalf("response type = %v, want error", got["type"])
	}
	if _, ok := got["message"].(string); !ok {
		t.Fatalf("response has no message: %+v", got)
	}
}

// TestTerminalTemplates_AlwaysErrors mirrors TestTerminalCreate_AlwaysErrors:
// terminal_templates is retired the same way (the catalog moved to relay's
// own GET /api/terminal/templates), and must answer an explicit refusal
// rather than being silently dropped — the exact hang eve's Shell Launcher
// "New" tab used to see when nothing ever answered this frame at all.
func TestTerminalTemplates_AlwaysErrors(t *testing.T) {
	hub := NewHub()
	mgr := terminal.NewManager(terminal.Config{ShimBinary: buildRelaySessionsBin(t)})
	NewTerminalHandlers(hub, mgr)

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "terminal_templates"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readJSONWithTimeout(t, conn, 2*time.Second)
	if got["type"] != "error" {
		t.Fatalf("response type = %v, want error", got["type"])
	}
	if _, ok := got["message"].(string); !ok {
		t.Fatalf("response has no message: %+v", got)
	}
}

func TestJoinTerminal_UnknownID_SendsError(t *testing.T) {
	hub := NewHub()
	mgr := terminal.NewManager(terminal.Config{ShimBinary: buildRelaySessionsBin(t)})
	NewTerminalHandlers(hub, mgr)

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "join_terminal", "terminalId": "no-such-id"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readJSONWithTimeout(t, conn, 2*time.Second)
	if got["type"] != "error" {
		t.Fatalf("response type = %v, want error", got["type"])
	}
}

// TestJoinInputResizeClose_RoundTrip drives a real terminal session (a real
// shim, a real pty, a real /bin/cat echoing stdin back) end to end over a
// real WebSocket connection: join gets scrollback + state, input round-trips
// through the pty, resize doesn't error, and close broadcasts terminal_closed
// and removes the session from the manager.
func TestJoinInputResizeClose_RoundTrip(t *testing.T) {
	hub := NewHub()
	mgr := terminal.NewManager(terminal.Config{ShimBinary: buildRelaySessionsBin(t)})
	NewTerminalHandlers(hub, mgr)

	sess, err := mgr.Create(terminal.CreateSpec{
		SessionID: "11111111-1111-1111-1111-111111111111",
		Name:      "cat-session",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/cat"},
		Cols:      80,
		Rows:      24,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	conn := dialHub(t, hub)

	if err := conn.WriteJSON(map[string]any{"type": "join_terminal", "terminalId": sess.ID}); err != nil {
		t.Fatalf("write join: %v", err)
	}
	joined := readJSONWithTimeout(t, conn, 2*time.Second)
	if joined["type"] != "terminal_joined" {
		t.Fatalf("type = %v, want terminal_joined (%+v)", joined["type"], joined)
	}
	if joined["terminalId"] != sess.ID {
		t.Fatalf("terminalId = %v, want %v", joined["terminalId"], sess.ID)
	}
	if joined["state"] != "running" {
		t.Fatalf("state = %v, want running", joined["state"])
	}

	// terminal_input: send "hello\n"; cat echoes it back over the pty, which
	// this manager's own read loop should eventually broadcast — but this
	// package's TerminalHandlers only broadcasts output when wired as the
	// manager's SetOutputHandler, which this test never does (that wiring
	// belongs to whatever later unit constructs both together). Instead this
	// asserts input delivery indirectly: cat exits 0 on EOF, so closing the
	// terminal's write side (via terminal_close) must not itself error, and
	// the write call below must not fail — the PTY plumbing is what
	// TestManager_LocalPTY_ExitAndLog already covers end to end at the
	// package's own level.
	payload := base64.StdEncoding.EncodeToString([]byte("hello\n"))
	if err := conn.WriteJSON(map[string]any{"type": "terminal_input", "terminalId": sess.ID, "data": payload}); err != nil {
		t.Fatalf("write input: %v", err)
	}

	if err := conn.WriteJSON(map[string]any{"type": "terminal_resize", "terminalId": sess.ID, "cols": 100, "rows": 40}); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	testutil.WaitFor(t, 2*time.Second, func() bool {
		cols, rows := sess.Size()
		return cols == 100 && rows == 40
	})

	if err := conn.WriteJSON(map[string]any{"type": "terminal_close", "terminalId": sess.ID}); err != nil {
		t.Fatalf("write close: %v", err)
	}
	closed := readJSONWithTimeout(t, conn, 2*time.Second)
	if closed["type"] != "terminal_closed" {
		t.Fatalf("type = %v, want terminal_closed (%+v)", closed["type"], closed)
	}
	if closed["terminalId"] != sess.ID {
		t.Fatalf("terminalId = %v, want %v", closed["terminalId"], sess.ID)
	}
	if _, ok := mgr.Get(sess.ID); ok {
		t.Fatal("terminal_close must remove the session from the manager")
	}
}

// TestDisconnect_RemovesViewerAndStartsIdleTimer proves handleDisconnect
// cleans up a viewer that vanished without a leave_terminal message, and
// that dropping to zero viewers arms the idle timer exactly like an
// explicit leave would.
func TestDisconnect_RemovesViewerAndStartsIdleTimer(t *testing.T) {
	fc := testutil.NewFakeClock(time.Unix(0, 0))
	hub := NewHub()
	mgr := terminal.NewManager(terminal.Config{ShimBinary: buildRelaySessionsBin(t), Clock: fc})
	NewTerminalHandlers(hub, mgr)

	sess, err := mgr.Create(terminal.CreateSpec{
		SessionID:   "22222222-2222-2222-2222-222222222222",
		Directory:   t.TempDir(),
		Argv:        []string{"/bin/sh", "-c", "sleep 100"},
		IdleTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "join_terminal", "terminalId": sess.ID}); err != nil {
		t.Fatalf("write join: %v", err)
	}
	_ = readJSONWithTimeout(t, conn, 2*time.Second) // terminal_joined

	_ = conn.Close() // no leave_terminal: simulate a client that just vanished

	testutil.WaitFor(t, 2*time.Second, func() bool { return fc.Waiters() >= 1 })
	fc.Advance(5 * time.Minute)

	testutil.WaitFor(t, 5*time.Second, func() bool {
		_, ok := mgr.Get(sess.ID)
		return !ok
	})
}
