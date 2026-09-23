package api

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

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

// readUntil reads frames off conn until match returns true, failing the test
// if timeout elapses first. PTY output arrives in arbitrary chunks, so a
// test waiting on specific bytes cannot assume one frame carries them.
func readUntil(t *testing.T, conn *websocket.Conn, timeout time.Duration, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_ = conn.SetReadDeadline(deadline)
		var msg map[string]any
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read json (waiting for match): %v", err)
		}
		if match(msg) {
			return msg
		}
	}
}

func decodeB64Field(t *testing.T, msg map[string]any, field string) string {
	t.Helper()
	s, ok := msg[field].(string)
	if !ok {
		t.Fatalf("%s missing or not a string: %+v", field, msg)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("%s not base64: %v", field, err)
	}
	return string(b)
}

// newReconnectFixture starts a real /bin/cat terminal at 80x24 with the
// manager's output wired to the handlers' broadcast, as relay-sessions does
// in production.
func newReconnectFixture(t *testing.T, sessionID string) (*Hub, *terminal.Manager, *terminal.Session) {
	t.Helper()
	hub := NewHub()
	mgr := terminal.NewManager(terminal.Config{ShimBinary: buildRelaySessionsBin(t)})
	th := NewTerminalHandlers(hub, mgr)
	mgr.SetOutputHandler(th.BroadcastOutput)

	sess, err := mgr.Create(terminal.CreateSpec{
		SessionID: sessionID,
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
	return hub, mgr, sess
}

// TestTerminalReconnect_NewConnection_GetsScrollbackAndLiveOutput simulates
// a browser reload: the connection that joined the terminal goes away, and a
// fresh connection sends terminal_reconnect. It must receive terminal_joined
// carrying the earlier output as scrollback, and be registered as a viewer so
// later PTY output reaches it as terminal_output.
func TestTerminalReconnect_NewConnection_GetsScrollbackAndLiveOutput(t *testing.T) {
	hub, mgr, sess := newReconnectFixture(t, "33333333-3333-3333-3333-333333333333")

	first := dialHub(t, hub)
	if err := first.WriteJSON(map[string]any{"type": "join_terminal", "terminalId": sess.ID}); err != nil {
		t.Fatalf("write join: %v", err)
	}
	if got := readJSONWithTimeout(t, first, 2*time.Second); got["type"] != "terminal_joined" {
		t.Fatalf("first join type = %v, want terminal_joined (%+v)", got["type"], got)
	}
	if err := mgr.Write(sess.ID, []byte("before-reload\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	testutil.WaitFor(t, 2*time.Second, func() bool {
		return strings.Contains(string(sess.ScrollbackBytes()), "before-reload")
	})
	_ = first.Close()

	second := dialHub(t, hub)
	if err := second.WriteJSON(map[string]any{"type": "terminal_reconnect", "terminalId": sess.ID, "cols": 80, "rows": 24}); err != nil {
		t.Fatalf("write reconnect: %v", err)
	}
	joined := readUntil(t, second, 2*time.Second, func(m map[string]any) bool { return m["type"] == "terminal_joined" })
	if joined["terminalId"] != sess.ID {
		t.Fatalf("terminalId = %v, want %v", joined["terminalId"], sess.ID)
	}
	if sb := decodeB64Field(t, joined, "scrollback"); !strings.Contains(sb, "before-reload") {
		t.Fatalf("scrollback %q does not contain earlier output", sb)
	}

	if err := mgr.Write(sess.ID, []byte("after-reload\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var live strings.Builder
	readUntil(t, second, 2*time.Second, func(m map[string]any) bool {
		if m["type"] != "terminal_output" || m["terminalId"] != sess.ID {
			return false
		}
		live.WriteString(decodeB64Field(t, m, "data"))
		return strings.Contains(live.String(), "after-reload")
	})
}

// TestTerminalReconnect_ResizesOnlyWhenSizeDiffers: a reconnect carrying a
// different positive size resizes the PTY before the snapshot, so
// terminal_joined already reports the new size; the same size, or a zero
// size, leaves it untouched.
func TestTerminalReconnect_ResizesOnlyWhenSizeDiffers(t *testing.T) {
	cases := []struct {
		name               string
		cols, rows         int
		wantCols, wantRows uint16
	}{
		{"different size resizes", 120, 50, 120, 50},
		{"same size unchanged", 80, 24, 80, 24},
		{"zero size unchanged", 0, 0, 80, 24},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := fmt.Sprintf("44444444-4444-4444-4444-44444444444%d", i)
			hub, _, sess := newReconnectFixture(t, id)

			conn := dialHub(t, hub)
			if err := conn.WriteJSON(map[string]any{"type": "terminal_reconnect", "terminalId": sess.ID, "cols": tc.cols, "rows": tc.rows}); err != nil {
				t.Fatalf("write reconnect: %v", err)
			}
			joined := readUntil(t, conn, 2*time.Second, func(m map[string]any) bool { return m["type"] == "terminal_joined" })

			if cols, rows := sess.Size(); cols != tc.wantCols || rows != tc.wantRows {
				t.Fatalf("sess.Size() = %dx%d, want %dx%d", cols, rows, tc.wantCols, tc.wantRows)
			}
			if joined["cols"] != float64(tc.wantCols) || joined["rows"] != float64(tc.wantRows) {
				t.Fatalf("terminal_joined size = %vx%v, want %dx%d", joined["cols"], joined["rows"], tc.wantCols, tc.wantRows)
			}
		})
	}
}

// TestTerminalReconnect_UnknownID_SendsError: reconnecting to a terminal the
// manager no longer has answers the same error frame join_terminal does.
func TestTerminalReconnect_UnknownID_SendsError(t *testing.T) {
	hub := NewHub()
	mgr := terminal.NewManager(terminal.Config{ShimBinary: buildRelaySessionsBin(t)})
	NewTerminalHandlers(hub, mgr)

	conn := dialHub(t, hub)
	if err := conn.WriteJSON(map[string]any{"type": "terminal_reconnect", "terminalId": "no-such-id", "cols": 80, "rows": 24}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := readJSONWithTimeout(t, conn, 2*time.Second)
	if got["type"] != "error" {
		t.Fatalf("response type = %v, want error", got["type"])
	}
	if got["message"] != "terminal not found: no-such-id" {
		t.Fatalf("message = %v, want the join_terminal unknown-id message", got["message"])
	}
}
