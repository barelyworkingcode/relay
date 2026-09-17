package hostapi_test

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
)

// startMountedServer is startServerWithManagers plus the three fields the
// eve-facing manifest surface (models, sessions, terminals, /ws) needs:
// Permissions, PiBinary, ModelSocket. A separate helper rather than adding
// these to startServerWithManagers itself — most existing hostapi tests
// exercise only /launch and /terminate and have no use for them.
func startMountedServer(t *testing.T, relayPID int) (srv *hostapi.Server, internalSock, bearer string, terminals *terminal.Manager, sessions *session.Manager) {
	t.Helper()
	terminals, sessions = buildManagers(t)
	dir := mkShortTempDir(t, "hostapi-mount-")
	internalSock = filepath.Join(dir, "internal.sock")
	hookSock := filepath.Join(dir, "hook.sock")
	bearer = "test-mount-bearer-secret"

	srv = hostapi.New(hostapi.Config{
		InternalSocket: internalSock,
		InternalBearer: bearer,
		RelayPID:       relayPID,
		HookSocket:     hookSock,
		Permissions:    permission.NewPermissionManager(),
	}, terminals, sessions)
	if err := srv.ListenInternal(); err != nil {
		t.Fatalf("ListenInternal: %v", err)
	}
	if err := srv.ListenHook(); err != nil {
		t.Fatalf("ListenHook: %v", err)
	}
	go func() { _ = srv.ServeInternal() }()
	go func() { _ = srv.ServeHook() }()
	t.Cleanup(srv.Close)
	return srv, internalSock, bearer, terminals, sessions
}

func getWithBearer(t *testing.T, client *http.Client, url, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func deleteWithBearer(t *testing.T, client *http.Client, url, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestMount_Models_OKWithBearer proves GET /api/models is actually mounted
// and reachable with the correct peer+bearer — the exact gap
// docs/session-host.md's "what is not built yet" gap 2 named: this surface
// was built and unit-tested (internal/sessions/api) but never mounted onto
// any real HTTP server.
func TestMount_Models_OKWithBearer(t *testing.T) {
	_, internalSock, bearer, _, _ := startMountedServer(t, os.Getpid())
	client := unixClient(internalSock)

	resp := getWithBearer(t, client, "http://h/api/models", bearer)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["models"]; !ok {
		t.Fatalf("response missing \"models\": %+v", body)
	}
	if _, ok := body["providerSettings"]; !ok {
		t.Fatalf("response missing \"providerSettings\": %+v", body)
	}
}

// TestMount_Models_NoOrWrongBearer_Forbidden covers both halves of the
// guarded wrapper: an absent bearer and an incorrect one must both be
// refused with 403, and — since checkInternalPeer runs before any handler
// work — that refusal must not depend on the request otherwise being
// well-formed.
func TestMount_Models_NoOrWrongBearer_Forbidden(t *testing.T) {
	_, internalSock, _, _, _ := startMountedServer(t, os.Getpid())
	client := unixClient(internalSock)

	for name, bearer := range map[string]string{"no_bearer": "", "wrong_bearer": "not-the-real-bearer"} {
		t.Run(name, func(t *testing.T) {
			resp := getWithBearer(t, client, "http://h/api/models", bearer)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

// TestMount_Terminals_EmptyList proves GET /api/terminals is mounted and
// answers the documented object-wrapped shape for an empty host.
func TestMount_Terminals_EmptyList(t *testing.T) {
	_, internalSock, bearer, _, _ := startMountedServer(t, os.Getpid())
	client := unixClient(internalSock)

	resp := getWithBearer(t, client, "http://h/api/terminals", bearer)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Terminals []any `json:"terminals"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Terminals == nil || len(body.Terminals) != 0 {
		t.Fatalf("terminals = %+v, want an empty (non-nil-shaped) list", body.Terminals)
	}
}

// TestMount_TerminalLog_UnknownID_NotFound proves GET
// /api/terminals/{id}/log is mounted and reaches the real handler (a 404
// for an unknown id, not a blanket 403 or a mux miss).
func TestMount_TerminalLog_UnknownID_NotFound(t *testing.T) {
	_, internalSock, bearer, _, _ := startMountedServer(t, os.Getpid())
	client := unixClient(internalSock)

	resp := getWithBearer(t, client, "http://h/api/terminals/99999999-9999-9999-9999-999999999999/log", bearer)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestMount_DeleteSession_NoContent proves DELETE /api/sessions/{id} is
// mounted: 204 unconditionally, matching HandleDeleteSession's own
// documented "deleting an id nothing knows about is not an error" contract.
func TestMount_DeleteSession_NoContent(t *testing.T) {
	_, internalSock, bearer, _, _ := startMountedServer(t, os.Getpid())
	client := unixClient(internalSock)

	resp := deleteWithBearer(t, client, "http://h/api/sessions/11111111-1111-1111-1111-111111111111", bearer)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

// dialMountedWS dials /ws over internalSock with a real gorilla/websocket
// client, the same shape frontend_dispatcher.go's proxyWS uses to reach this
// exact socket in production.
func dialMountedWS(internalSock, bearer string) (*websocket.Conn, *http.Response, error) {
	dialer := &websocket.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) {
			return net.Dial("unix", internalSock)
		},
		HandshakeTimeout: 5 * time.Second,
	}
	header := http.Header{}
	if bearer != "" {
		header.Set("Authorization", "Bearer "+bearer)
	}
	return dialer.Dial("ws://internal.relay.localsocket/ws", header)
}

// TestMount_WS_Upgrade_And_List drives a real WebSocket handshake against
// /ws over the internal socket, then a real terminal_list round trip — the
// message eve's own message-dispatcher sends and expects a shaped reply to.
// This is the exact request eve's Shell Launcher dialog 404's on today
// without this mount (background section of the fix this test covers).
func TestMount_WS_Upgrade_And_List(t *testing.T) {
	_, internalSock, bearer, _, _ := startMountedServer(t, os.Getpid())

	conn, resp, err := dialMountedWS(internalSock, bearer)
	if err != nil {
		t.Fatalf("WS dial with correct bearer: %v (resp=%+v)", err, resp)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", resp.StatusCode)
	}

	if err := conn.WriteJSON(map[string]any{"type": "terminal_list"}); err != nil {
		t.Fatalf("write terminal_list: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got map[string]any
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if got["type"] != "terminal_list" {
		t.Fatalf("reply type = %v, want terminal_list (%+v)", got["type"], got)
	}
	if _, ok := got["terminals"]; !ok {
		t.Fatalf("reply missing \"terminals\": %+v", got)
	}
}

// TestMount_WS_NoBearer_HandshakeFails403 proves the guard runs before
// hub.HandleUpgrade ever writes the 101: a caller with no bearer must get a
// failed handshake (403), never a *websocket.Conn.
func TestMount_WS_NoBearer_HandshakeFails403(t *testing.T) {
	_, internalSock, _, _, _ := startMountedServer(t, os.Getpid())

	conn, resp, err := dialMountedWS(internalSock, "")
	if err == nil {
		conn.Close()
		t.Fatal("want the handshake to fail with no bearer, got a live connection")
	}
	if resp == nil {
		t.Fatal("want a non-nil response even on handshake failure")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("handshake status = %d, want 403", resp.StatusCode)
	}
}

// TestMount_Launch_StillAnswersAsBefore is a regression check: mounting the
// new eve-facing routes on the same mux as /launch must not have disturbed
// /launch's own existing behavior (its own dedicated tests in launch_test.go
// cover the full launch path; this only pins that it is still there,
// unshadowed by any of the new patterns).
func TestMount_Launch_StillAnswersAsBefore(t *testing.T) {
	_, internalSock, bearer, _, _ := startMountedServer(t, os.Getpid())
	client := unixClient(internalSock)

	resp := postJSON(t, client, "http://h/launch", bearer, map[string]any{
		"v": 1, "session_id": "s", "kind": "not-a-real-kind", "argv": []string{"/bin/true"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unknown kind, same as before this mount)", resp.StatusCode)
	}
}

// TestMount_TerminalExit_FanOut_BridgeAndWS_BothFire is the specific
// regression the last-write-wins hazard (terminals.SetExitHandler being
// called twice) would cause if BroadcastExit had been wired as a second
// SetExitHandler call instead of a direct call inside onTerminalExit: a
// real, short-lived pty session's exit must reach BOTH relay's bridge
// report (SetExitHandler's own fn) AND every WS viewer joined to it
// (terminal_exit frame).
func TestMount_TerminalExit_FanOut_BridgeAndWS_BothFire(t *testing.T) {
	srv, internalSock, bearer, _, _ := startMountedServer(t, os.Getpid())

	bridgeExit := make(chan struct {
		id       string
		exitCode int
		reason   string
	}, 1)
	srv.SetExitHandler(func(id string, rootPID, exitCode int, reason string) {
		bridgeExit <- struct {
			id       string
			exitCode int
			reason   string
		}{id, exitCode, reason}
	})

	client := unixClient(internalSock)
	const sessionID = "22222222-3333-4444-5555-666666666666"
	resp := postJSON(t, client, "http://h/launch", bearer, launchBody(sessionID, []string{"/bin/sh", "-c", "exit 3"}))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("launch status = %d, want 201", resp.StatusCode)
	}

	// Join over WS before the (short-lived) session exits, so this
	// connection is a real viewer by the time onTerminalExit fires.
	conn, wsResp, err := dialMountedWS(internalSock, bearer)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	defer conn.Close()
	wsResp.Body.Close()
	if err := conn.WriteJSON(map[string]any{"type": "join_terminal", "terminalId": sessionID}); err != nil {
		t.Fatalf("write join_terminal: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var joined map[string]any
	if err := conn.ReadJSON(&joined); err != nil {
		t.Fatalf("read join reply: %v", err)
	}
	if joined["type"] != "terminal_joined" && joined["type"] != "terminal_exit" {
		t.Fatalf("first frame type = %v, want terminal_joined (or terminal_exit if it already exited): %+v", joined["type"], joined)
	}

	select {
	case ev := <-bridgeExit:
		if ev.id != sessionID {
			t.Fatalf("bridge exit id = %q, want %q", ev.id, sessionID)
		}
		if ev.exitCode != 3 {
			t.Fatalf("bridge exit code = %d, want 3", ev.exitCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the bridge-facing SetExitHandler callback")
	}

	// The WS viewer must also see a terminal_exit frame — either as the
	// second frame (if join raced ahead of the exit) or, if the session had
	// already exited by the time join ran, folded into terminal_joined's own
	// immediate follow-up (see TerminalHandlers.join's own "state == stopped"
	// branch) — already consumed above as `joined` in that case.
	if joined["type"] == "terminal_exit" {
		if int(joined["exitCode"].(float64)) != 3 {
			t.Fatalf("exitCode = %v, want 3", joined["exitCode"])
		}
		return
	}
	var exitFrame map[string]any
	if err := conn.ReadJSON(&exitFrame); err != nil {
		t.Fatalf("read terminal_exit frame: %v", err)
	}
	if exitFrame["type"] != "terminal_exit" {
		t.Fatalf("frame type = %v, want terminal_exit (%+v)", exitFrame["type"], exitFrame)
	}
	if int(exitFrame["exitCode"].(float64)) != 3 {
		t.Fatalf("exitCode = %v, want 3", exitFrame["exitCode"])
	}
}
