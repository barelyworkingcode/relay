package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"relaygo/mcp"
)

// newTestHTTPServer creates a test HTTP server that responds with valid JSON-RPC
// responses. The handler can be customized per test.
func newTestHTTPServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// readJSONRPCID extracts the request ID from a JSON-RPC request body.
// Centralizes the body-read + unmarshal pattern used across HTTP test handlers.
func readJSONRPCID(r *http.Request) int64 {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	var req struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(body, &req)
	return req.ID
}

// jsonRPCHandler returns a handler that responds to any JSON-RPC request with
// a valid result containing the given payload.
func jsonRPCHandler(result string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := readJSONRPCID(r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, id, result)
	}
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

	cfg := ExternalMcp{
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

	cfg := ExternalMcp{
		ID:        "test",
		Transport: "http",
		URL:       srv.URL,
	}
	conn := newHTTPMcpConn(cfg)

	_, err := conn.SendRequest(context.Background(), mcp.MethodInitialize, nil)
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if err != ErrAuthRequired {
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

	cfg := ExternalMcp{
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
		// Write SSE data.
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[]}}\n\n", id)
	})
	defer srv.Close()

	cfg := ExternalMcp{
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
		// Multi-line SSE data: JSON split across two data: lines, terminated by blank line.
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\n", id)
		fmt.Fprintf(w, "data: \"result\":{\"multi\":true}}\n\n")
	})
	defer srv.Close()

	cfg := ExternalMcp{
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
		// Data line with no trailing blank line — stream ends after data.
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"ok\":true}}\n", id)
	})
	defer srv.Close()

	cfg := ExternalMcp{
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
		// Notification (no id) before the actual response.
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"found\":true}}\n\n", id)
	})
	defer srv.Close()

	cfg := ExternalMcp{
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

func TestHTTPMcpConn_SendNotification(t *testing.T) {
	received := make(chan string, 1)
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		received <- string(body)
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	cfg := ExternalMcp{
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
