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
)

// newTestHTTPServer creates a test HTTP server that responds with valid JSON-RPC
// responses. The handler can be customized per test.
func newTestHTTPServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// jsonRPCHandler returns a handler that responds to any JSON-RPC request with
// a valid result containing the given payload.
func jsonRPCHandler(result string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()

		var req struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, req.ID, result)
	}
}

func TestHTTPMcpConn_SendRequest_Concurrent(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0

	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()

		var req struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(body, &req)

		mu.Lock()
		requestCount++
		mu.Unlock()

		// Small delay to increase overlap window.
		time.Sleep(10 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"ok":true}}`, req.ID)
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

	_, err := conn.SendRequest(context.Background(), "initialize", nil)
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if err != ErrAuthRequired {
		t.Fatalf("expected ErrAuthRequired, got: %v", err)
	}
}

func TestHTTPMcpConn_SendRequest_SessionID(t *testing.T) {
	srv := newTestHTTPServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()

		var req struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-abc")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID)
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
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()

		var req struct {
			ID int64 `json:"id"`
		}
		json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Write SSE data.
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[]}}\n\n", req.ID)
	})
	defer srv.Close()

	cfg := ExternalMcp{
		ID:        "test",
		Transport: "http",
		URL:       srv.URL,
	}
	conn := newHTTPMcpConn(cfg)

	result, err := conn.SendRequest(context.Background(), "tools/list", nil)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(result), "tools") {
		t.Fatalf("expected tools in result, got: %s", string(result))
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

	conn.SendNotification("notifications/initialized")

	select {
	case body := <-received:
		if !strings.Contains(body, "notifications/initialized") {
			t.Fatalf("expected method in body, got: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification not received within timeout")
	}
}
