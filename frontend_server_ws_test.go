package main

// WebSocket coverage through the REAL FrontendServer (bearer auth + model
// guard + dispatcher), not the bare dispatcher the existing WS tests dial.
// The front-door bearer must gate WS upgrades exactly as it gates HTTP — an
// unauthenticated upgrade must never reach an upstream service. Also covers
// the upstream-dial-failure close frame. Both paths were previously untested
// at the server seam.

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startFrontendServerWith brings up a real FrontendServer over a Unix socket
// with the given bearer token and a pre-populated enhanced registry. Returns
// the socket path.
func startFrontendServerWith(t *testing.T, token string, enhanced *EnhancedServiceRegistry) string {
	t.Helper()
	dir := mkShortTempDir(t, "fe-ws-")
	sock := filepath.Join(dir, "frontend.sock")

	store := NewSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	extMgr := NewExternalMcpManager(nil)
	srv, err := NewFrontendServer(store, extMgr, extMgr, Endpoint{Socket: sock, Token: token}, enhanced, nil, nil)
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
	return sock
}

func wsDialerOverUnix(sock string) *websocket.Dialer {
	return &websocket.Dialer{
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		HandshakeTimeout: 2 * time.Second,
	}
}

// echoWSService returns a FakeService that upgrades and echoes `echo:<msg>`.
func echoWSService(t *testing.T) *FakeService {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			_ = c.WriteMessage(mt, []byte("echo:"+string(msg)))
		}
	})
	return NewFakeService(t, FakeServiceOptions{
		ServiceID: "svc-ws",
		Manifest:  newManifest("/ws"),
		Handler:   handler,
	})
}

func TestFrontendServer_WSUpgrade_RejectsUnauthenticated(t *testing.T) {
	enhanced := NewEnhancedServiceRegistry(nil)
	fake := echoWSService(t)
	if err := enhanced.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest()); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	sock := startFrontendServerWith(t, "good-token", enhanced)

	// No Authorization header → the bearer middleware must 401 the upgrade
	// before it ever reaches the dispatcher/upstream.
	_, resp, err := wsDialerOverUnix(sock).Dial("ws://unix/ws", nil)
	if err == nil {
		t.Fatal("expected WS handshake failure without bearer token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 on unauthenticated WS upgrade, got resp=%v", resp)
	}
	// The upstream service must not have seen anything.
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("unauthenticated WS upgrade reached upstream (%d requests)", n)
	}
}

func TestFrontendServer_WSUpgrade_AllowsAuthenticated(t *testing.T) {
	enhanced := NewEnhancedServiceRegistry(nil)
	fake := echoWSService(t)
	if err := enhanced.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest()); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	sock := startFrontendServerWith(t, "good-token", enhanced)

	hdr := http.Header{"Authorization": {"Bearer good-token"}}
	conn, _, err := wsDialerOverUnix(sock).Dial("ws://unix/ws", hdr)
	assertNoErr(t, err, "authenticated WS dial")
	defer conn.Close()

	_ = conn.WriteMessage(websocket.TextMessage, []byte("hi"))
	_, msg, err := conn.ReadMessage()
	assertNoErr(t, err, "WS read")
	if string(msg) != "echo:hi" {
		t.Fatalf("WS echo = %q, want echo:hi", msg)
	}

	foundWS := false
	for _, rq := range fake.Requests() {
		if rq.WasWebSocket {
			foundWS = true
		}
	}
	if !foundWS {
		t.Error("upstream did not record a WebSocket upgrade")
	}
}

// A half-open upstream peer — one that stops responding without sending a
// close frame (network partition, hung process) — must be reaped by the read
// deadline rather than hanging the proxy forever. We point relay at an upstream
// that upgrades but never reads (so it never auto-pongs relay's pings); the
// upstream-side read deadline must trip and tear down the client conn. If the
// keepalive logic regresses (deadline not set, or wsPingPeriod >= wsPongWait),
// the client read never returns and this test times out.
func TestFrontendServer_WSHalfOpenUpstreamIsReaped(t *testing.T) {
	// Shorten the keepalive window so the reap happens in test time.
	oldWait, oldPing := wsPongWait, wsPingPeriod
	wsPongWait, wsPingPeriod = 300*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { wsPongWait, wsPingPeriod = oldWait, oldPing })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	silent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		<-release // never read/write → never auto-pongs relay's pings
		_ = c.Close()
	})
	fake := NewFakeService(t, FakeServiceOptions{
		ServiceID: "svc-silent", Manifest: newManifest("/ws"), Handler: silent,
	})

	enhanced := NewEnhancedServiceRegistry(nil)
	if err := enhanced.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest()); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	sock := startFrontendServerWith(t, "good-token", enhanced)

	hdr := http.Header{"Authorization": {"Bearer good-token"}}
	conn, _, err := wsDialerOverUnix(sock).Dial("ws://unix/ws", hdr)
	assertNoErr(t, err, "client WS dial")
	defer conn.Close()

	// The client read must return (with an error) once relay reaps the silent
	// upstream — not hang. A generous ceiling well above wsPongWait.
	errCh := make(chan error, 1)
	go func() { _, _, e := conn.ReadMessage(); errCh <- e }()
	select {
	case e := <-errCh:
		if e == nil {
			t.Fatal("expected teardown error after half-open upstream reaped")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("half-open upstream was not reaped: client read never returned")
	}
}

// When the resolved service's internal socket is unreachable, relay upgrades
// the client (so the dial succeeds) and then sends a 1011 close frame rather
// than leaving the client hanging.
func TestFrontendServer_WSUpstreamDialFailure_ClosesClient(t *testing.T) {
	enhanced := NewEnhancedServiceRegistry(nil)
	deadSock := filepath.Join(mkShortTempDir(t, "dead-"), "nope.sock") // never bound
	if err := enhanced.RegisterManifest("svc-dead", deadSock, "tok", newManifest("/ws")); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	sock := startFrontendServerWith(t, "good-token", enhanced)

	hdr := http.Header{"Authorization": {"Bearer good-token"}}
	conn, _, err := wsDialerOverUnix(sock).Dial("ws://unix/ws", hdr)
	assertNoErr(t, err, "client WS dial (relay upgrades before dialing upstream)")
	defer conn.Close()

	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("expected a close frame when the upstream socket is unreachable")
	}
	if ce, ok := err.(*websocket.CloseError); !ok || ce.Code != websocket.CloseInternalServerErr {
		t.Fatalf("want CloseInternalServerErr (1011), got %v", err)
	}
}
