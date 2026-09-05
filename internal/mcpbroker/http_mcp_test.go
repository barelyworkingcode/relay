package mcpbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcp"
)

func newTestHTTPServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

func readJSONRPCID(r *http.Request) int64 {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	var req struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(body, &req)
	return req.ID
}

func TestHTTPMcpConn_SendRequest_Concurrent(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0

	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		id := readJSONRPCID(r)

		mu.Lock()
		requestCount++
		mu.Unlock()

		// Small delay to increase overlap window.
		time.Sleep(10 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"ok":true}}`, id)
	})
	defer srv.Close()

	cfg := config.ExternalMcp{
		ID:        "test",
		Transport: "http",
		URL:       srv.URL,
	}
	conn := newHTTPMcpConn(cfg)

	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := conn.SendRequest(ctx, "test/method", nil)
			if err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent SendRequest failed: %v", err)
	}

	mu.Lock()
	if requestCount != 10 {
		t.Errorf("expected 10 requests, got %d", requestCount)
	}
	mu.Unlock()
}

func TestHTTPMcpConn_SendRequest_401(t *testing.T) {
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer srv.Close()

	cfg := config.ExternalMcp{
		ID:        "test",
		Transport: "http",
		URL:       srv.URL,
	}
	conn := newHTTPMcpConn(cfg)

	_, err := conn.SendRequest(context.Background(), mcp.MethodInitialize, nil)
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("expected ErrAuthRequired, got: %v", err)
	}
}

func TestHTTPMcpConn_SendRequest_SessionID(t *testing.T) {
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		id := readJSONRPCID(r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-abc")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, id)
	})
	defer srv.Close()

	cfg := config.ExternalMcp{
		ID:        "test",
		Transport: "http",
		URL:       srv.URL,
	}
	conn := newHTTPMcpConn(cfg)

	_, err := conn.SendRequest(context.Background(), "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	conn.mu.Lock()
	sid := conn.sessionID
	conn.mu.Unlock()

	if sid != "session-abc" {
		t.Fatalf("expected sessionID 'session-abc', got %q", sid)
	}
}

func TestHTTPMcpConn_SSEResponse(t *testing.T) {
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		id := readJSONRPCID(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[]}}\n\n", id)
	})
	defer srv.Close()

	cfg := config.ExternalMcp{
		ID:        "test",
		Transport: "http",
		URL:       srv.URL,
	}
	conn := newHTTPMcpConn(cfg)

	result, err := conn.SendRequest(context.Background(), mcp.MethodToolsList, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(result), "tools") {
		t.Fatalf("expected tools in result, got: %s", string(result))
	}
}

func TestHTTPMcpConn_SSEMultiLineData(t *testing.T) {
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		id := readJSONRPCID(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\n", id)
		fmt.Fprintf(w, "data: \"result\":{\"multi\":true}}\n\n")
	})
	defer srv.Close()

	cfg := config.ExternalMcp{
		ID:        "test",
		Transport: "http",
		URL:       srv.URL,
	}
	conn := newHTTPMcpConn(cfg)

	result, err := conn.SendRequest(context.Background(), "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Multi bool `json:"multi"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("failed to parse result: %v (raw: %s)", err, string(result))
	}
	if !parsed.Multi {
		t.Fatalf("expected multi=true, got: %s", string(result))
	}
}

func TestHTTPMcpConn_SSENoTrailingBlankLine(t *testing.T) {
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		id := readJSONRPCID(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"ok\":true}}\n", id)
	})
	defer srv.Close()

	cfg := config.ExternalMcp{
		ID:        "test",
		Transport: "http",
		URL:       srv.URL,
	}
	conn := newHTTPMcpConn(cfg)

	result, err := conn.SendRequest(context.Background(), "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("failed to parse result: %v (raw: %s)", err, string(result))
	}
	if !parsed.OK {
		t.Fatalf("expected ok=true, got: %s", string(result))
	}
}

func TestHTTPMcpConn_SSEInterleavedNotification(t *testing.T) {
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		id := readJSONRPCID(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"found\":true}}\n\n", id)
	})
	defer srv.Close()

	cfg := config.ExternalMcp{
		ID:        "test",
		Transport: "http",
		URL:       srv.URL,
	}
	conn := newHTTPMcpConn(cfg)

	result, err := conn.SendRequest(context.Background(), "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Found bool `json:"found"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("failed to parse result: %v (raw: %s)", err, string(result))
	}
	if !parsed.Found {
		t.Fatalf("expected found=true, got: %s", string(result))
	}
}

func TestHTTPMcpConn_SendRequest_TimesOutHungServerEvenWithNoCallerDeadline(t *testing.T) {
	old := MCPRequestTimeout
	MCPRequestTimeout = 150 * time.Millisecond
	defer func() { MCPRequestTimeout = old }()

	// Subtle: defer order matters here. close(release) is deferred after
	// srv.Close(), so it runs first (LIFO) and unparks the handler, letting
	// srv.Close() drain instead of hanging.
	release := make(chan struct{})
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	defer srv.Close()
	defer close(release)

	conn := newHTTPMcpConn(config.ExternalMcp{ID: "t", Transport: "http", URL: srv.URL})
	start := time.Now()
	if _, err := conn.SendRequest(context.Background(), "slow", nil); err == nil {
		t.Fatal("expected a timeout error from a hung server")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("SendRequest did not honor MCPRequestTimeout; took %s", elapsed)
	}
}

func TestHTTPMcpConn_SendRequest_RejectsOversizedSessionID(t *testing.T) {
	huge := strings.Repeat("x", maxSessionIDLen+1)
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		id := readJSONRPCID(r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", huge)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, id)
	})
	defer srv.Close()

	conn := newHTTPMcpConn(config.ExternalMcp{ID: "t", Transport: "http", URL: srv.URL})
	_, err := conn.SendRequest(context.Background(), "test", nil)
	if err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("expected oversized-session-id error, got: %v", err)
	}
	conn.mu.Lock()
	sid := conn.sessionID
	conn.mu.Unlock()
	if sid != "" {
		t.Fatalf("oversized session ID must not be stored (got %d bytes)", len(sid))
	}
}

// Deliberate: a stale past expiry would make every subsequent request
// believe a refresh is due — a refresh storm.
func TestHTTPMcpConn_RefreshWithoutExpiresIn_DisablesProactiveRefresh(t *testing.T) {
	conn := newHTTPMcpConn(config.ExternalMcp{ID: "t", Transport: "http", URL: "https://example.test/mcp"})
	conn.oauth.refreshToken = "rt"
	conn.oauth.tokenExpiry = time.Now().Add(-time.Hour) // stale/past expiry

	conn.applyRefreshedToken(&oauthMetadata{}, &oauthTokenResponse{AccessToken: "new-at"})

	conn.mu.Lock()
	exp := conn.oauth.tokenExpiry
	at := conn.oauth.accessToken
	conn.mu.Unlock()
	if at != "new-at" {
		t.Fatalf("access token not applied, got %q", at)
	}
	if !exp.IsZero() {
		t.Fatalf("expiry should be zeroed when expires_in is absent, got %v", exp)
	}
	if _, needs := conn.tokenRefreshSnapshot(); needs {
		t.Fatal("a token with no expiry must not be eligible for proactive refresh (CR-4 storm)")
	}
}

func TestHTTPMcpConn_SendNotification(t *testing.T) {
	received := make(chan string, 1)
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		received <- string(body)
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	cfg := config.ExternalMcp{
		ID:        "test",
		Transport: "http",
		URL:       srv.URL,
	}
	conn := newHTTPMcpConn(cfg)

	conn.SendNotification(mcp.MethodInitialized)

	select {
	case body := <-received:
		if !strings.Contains(body, "notifications/initialized") {
			t.Fatalf("expected method in body, got: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification not received within timeout")
	}
}
