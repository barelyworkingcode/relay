package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Tests for the front-door HTTP+WS dispatcher. Covers:
//   - longest-prefix-match wiring (LookupByPath is unit-tested elsewhere;
//     here we verify the dispatcher actually USES it)
//   - 404 when no service claims the path
//   - inbound Authorization is stripped, declared internal token injected
//   - request body is preserved
//   - WS upgrade round-trip with token injection
//   - WS proxy doesn't leak goroutines after both sides close

func TestFrontendDispatcher_404OnUnknownPath(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)
	dispatcher := NewFrontendDispatcher(registry)
	srv := httptest.NewServer(dispatcher)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/no/such/route")
	assertNoErr(t, err, "GET /no/such/route")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown path; got %d", resp.StatusCode)
	}
}

func TestFrontendDispatcher_RoutesAndInjectsToken(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)
	fake := NewFakeService(t, FakeServiceOptions{
		ServiceID: "svc-a",
		Manifest:  newManifest("/api/a/"),
	})
	if err := registry.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest()); err != nil {
		t.Fatalf("register: %v", err)
	}
	dispatcher := NewFrontendDispatcher(registry)
	srv := httptest.NewServer(dispatcher)
	defer srv.Close()

	// Send a request carrying a frontend Authorization header that should
	// be stripped before reaching the upstream service.
	req, _ := http.NewRequest("POST", srv.URL+"/api/a/echo?x=1", strings.NewReader(`{"hello":"world"}`))
	req.Header.Set("Authorization", "Bearer FRONTEND-TOKEN-MUST-NOT-LEAK")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	assertNoErr(t, err, "dispatch POST")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200; got %d", resp.StatusCode)
	}

	got := fake.LastRequest()
	if got == nil {
		t.Fatal("FakeService recorded no request")
	}
	if got.Method != "POST" || got.Path != "/api/a/echo" {
		t.Fatalf("upstream got %s %s; want POST /api/a/echo", got.Method, got.Path)
	}
	if got.Query.Get("x") != "1" {
		t.Fatalf("query string lost; got %v", got.Query)
	}
	if string(got.Body) != `{"hello":"world"}` {
		t.Fatalf("body lost; got %q", got.Body)
	}
	// Authorization-header rewrite: must NOT carry the inbound token, MUST
	// carry the service-declared token.
	auth := got.Headers.Get("Authorization")
	if strings.Contains(auth, "FRONTEND-TOKEN-MUST-NOT-LEAK") {
		t.Fatalf("inbound Authorization leaked to upstream: %q", auth)
	}
	if auth != "Bearer "+fake.Token() {
		t.Fatalf("upstream Authorization = %q; want %q", auth, "Bearer "+fake.Token())
	}
}

func TestFrontendDispatcher_LongestPrefixWins(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)
	fakeOuter := NewFakeService(t, FakeServiceOptions{
		ServiceID: "outer",
		Manifest:  newManifest("/api/"),
	})
	fakeInner := NewFakeService(t, FakeServiceOptions{
		ServiceID: "inner",
		Manifest:  newManifest("/api/sessions/"),
	})
	_ = registry.RegisterManifest(fakeOuter.ServiceID(), fakeOuter.Socket(), fakeOuter.Token(), fakeOuter.Manifest())
	_ = registry.RegisterManifest(fakeInner.ServiceID(), fakeInner.Socket(), fakeInner.Token(), fakeInner.Manifest())

	dispatcher := NewFrontendDispatcher(registry)
	srv := httptest.NewServer(dispatcher)
	defer srv.Close()

	// /api/sessions/123 → inner; /api/other → outer
	resp1, err := http.Get(srv.URL + "/api/sessions/123")
	assertNoErr(t, err, "GET sessions")
	_, _ = io.Copy(io.Discard, resp1.Body)
	resp1.Body.Close()

	resp2, err := http.Get(srv.URL + "/api/other")
	assertNoErr(t, err, "GET other")
	_, _ = io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()

	if len(fakeInner.Requests()) != 1 || fakeInner.LastRequest().Path != "/api/sessions/123" {
		t.Fatalf("inner did not receive sessions request; reqs=%v", fakeInner.Requests())
	}
	if len(fakeOuter.Requests()) != 1 || fakeOuter.LastRequest().Path != "/api/other" {
		t.Fatalf("outer did not receive other request; reqs=%v", fakeOuter.Requests())
	}
}

func TestFrontendDispatcher_WSUpgradeAndForward(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)

	// Upstream WS handler: echoes any text frame as `echo:<msg>`.
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	wsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authorization must be the service-declared token.
		if r.Header.Get("Authorization") == "" {
			t.Errorf("upstream WS upgrade missing Authorization header")
		}
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

	fake := NewFakeService(t, FakeServiceOptions{
		ServiceID: "svc-ws",
		Manifest:  newManifest("/ws"),
		Handler:   wsHandler,
	})
	_ = registry.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest())

	dispatcher := NewFrontendDispatcher(registry)
	srv := httptest.NewServer(dispatcher)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	clientConn, _, err := dialer.Dial(wsURL, nil)
	assertNoErr(t, err, "client WS dial")
	defer clientConn.Close()

	_ = clientConn.WriteMessage(websocket.TextMessage, []byte("hello"))
	_, msg, err := clientConn.ReadMessage()
	assertNoErr(t, err, "client WS read")
	if string(msg) != "echo:hello" {
		t.Fatalf("WS echo wrong; got %q want echo:hello", msg)
	}
}

func TestFrontendDispatcher_WSNoGoroutineLeak(t *testing.T) {
	// Regression guard: the proxyWS goroutine pair must terminate when
	// either side closes. Run a small batch of WS sessions and check the
	// goroutine count returns to baseline.
	registry := NewEnhancedServiceRegistry(nil)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	wsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage() // wait for one msg then exit, closing
	})

	fake := NewFakeService(t, FakeServiceOptions{
		ServiceID: "svc-ws-leak",
		Manifest:  newManifest("/ws"),
		Handler:   wsHandler,
	})
	_ = registry.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest())

	dispatcher := NewFrontendDispatcher(registry)
	srv := httptest.NewServer(dispatcher)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	// Settle baseline.
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 5; i++ {
		dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
		conn, _, err := dialer.Dial(wsURL, nil)
		assertNoErr(t, err, "WS dial iter %d", i)
		_ = conn.WriteMessage(websocket.TextMessage, []byte("bye"))
		conn.Close()
	}

	// Give the proxy goroutines time to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= base+2 {
			return // OK
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine leak after 5 WS sessions: base=%d now=%d", base, runtime.NumGoroutine())
}

