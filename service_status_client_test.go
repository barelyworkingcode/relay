package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServiceServer is a Unix-socket HTTP server scripted to return canned
// responses keyed by (method, path). Used to drive ServiceStatusClient
// against a real loopback HTTP request without spinning up a real service.
type fakeServiceServer struct {
	t         *testing.T
	socket    string
	listener  net.Listener
	server    *http.Server
	mu        sync.Mutex
	responses map[string]fakeResponse
	requests  []recordedRequest
}

type fakeResponse struct {
	status int
	body   []byte
}

type recordedRequest struct {
	Method string
	Path   string
	Auth   string
}

func newFakeServiceServer(t *testing.T) *fakeServiceServer {
	t.Helper()
	// /tmp because t.TempDir paths blow past macOS's 104-char unix socket
	// limit (matches the relayLLM-side FakeBridge pattern).
	dir, err := os.MkdirTemp("/tmp", "fss")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	sockPath := filepath.Join(dir, "svc.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("listen unix: %v", err)
	}
	f := &fakeServiceServer{
		t:         t,
		socket:    sockPath,
		listener:  ln,
		responses: make(map[string]fakeResponse),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.server = &http.Server{Handler: mux}
	go f.server.Serve(ln)
	t.Cleanup(func() {
		_ = f.server.Close()
		_ = os.RemoveAll(dir)
	})
	return f
}

func (f *fakeServiceServer) script(method, path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[method+" "+path] = fakeResponse{status: status, body: []byte(body)}
}

func (f *fakeServiceServer) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func (f *fakeServiceServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Auth:   r.Header.Get("Authorization"),
	})
	resp, ok := f.responses[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(resp.status)
	_, _ = w.Write(resp.body)
}

// ---------------------------------------------------------------------------
// GetStatus
// ---------------------------------------------------------------------------

func TestServiceStatusClient_GetStatus_HappyPath(t *testing.T) {
	srv := newFakeServiceServer(t)
	srv.script("GET", "/api/status", 200, `{"uptimeSeconds":42,"instances":[]}`)

	client := NewServiceStatusClient(srv.socket, "tok123")
	got, err := client.GetStatus(context.Background(), "/api/status")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if parsed["uptimeSeconds"] != float64(42) {
		t.Errorf("uptimeSeconds: got %v", parsed["uptimeSeconds"])
	}

	// Confirm bearer token went out on the wire — this is the load-bearing
	// reason the per-service internal token exists.
	reqs := srv.recorded()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	if reqs[0].Auth != "Bearer tok123" {
		t.Errorf("Authorization: got %q, want Bearer tok123", reqs[0].Auth)
	}
}

func TestServiceStatusClient_GetStatus_ErrorBodyPropagates(t *testing.T) {
	srv := newFakeServiceServer(t)
	srv.script("GET", "/api/status", 500, `{"error":"boom"}`)

	client := NewServiceStatusClient(srv.socket, "tok")
	_, err := client.GetStatus(context.Background(), "/api/status")
	if err == nil {
		t.Fatal("expected error on 5xx, got nil")
	}
	// Service name wrapping is the caller's responsibility (poller/dispatcher);
	// the client surfaces the upstream status + body so callers can build a
	// readable message.
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "500") {
		t.Errorf("error should surface upstream status + body: %v", err)
	}
}

func TestServiceStatusClient_GetStatus_BodyCapped(t *testing.T) {
	// A buggy or hostile service streaming an unbounded body must not OOM the
	// tray — the poller fans out to every service each tick. The client caps
	// each read at maxStatusBodyBytes via io.LimitReader. Drop that reader and
	// this test fails (got would exceed the cap).
	srv := newFakeServiceServer(t)
	big := strings.Repeat("a", maxStatusBodyBytes+1024)
	srv.script("GET", "/api/status", 200, big)

	client := NewServiceStatusClient(srv.socket, "tok")
	got, err := client.GetStatus(context.Background(), "/api/status")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if len(got) != maxStatusBodyBytes {
		t.Errorf("body not capped: got %d bytes, want %d", len(got), maxStatusBodyBytes)
	}
}

func TestServiceStatusClient_GetStatus_DialFailureIsError(t *testing.T) {
	client := NewServiceStatusClient("/tmp/relay-test-nonexistent-socket.sock", "tok")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := client.GetStatus(ctx, "/api/status"); err == nil {
		t.Fatal("expected dial error on missing socket, got nil")
	}
}

// ---------------------------------------------------------------------------
// DoAction
// ---------------------------------------------------------------------------

func TestServiceStatusClient_DoAction_NoContentIsSuccess(t *testing.T) {
	srv := newFakeServiceServer(t)
	srv.script("DELETE", "/api/llama/instances/qwen3-8b", 204, "")

	client := NewServiceStatusClient(srv.socket, "tok")
	body, err := client.DoAction(context.Background(), "DELETE", "/api/llama/instances/qwen3-8b")
	if err != nil {
		t.Fatalf("DoAction: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("expected empty body, got %q", string(body))
	}
}

func TestServiceStatusClient_DoAction_4xxIsError(t *testing.T) {
	srv := newFakeServiceServer(t)
	srv.script("DELETE", "/api/llama/instances/missing", 404, `{"error":"no such instance"}`)

	client := NewServiceStatusClient(srv.socket, "tok")
	_, err := client.DoAction(context.Background(), "DELETE", "/api/llama/instances/missing")
	if err == nil {
		t.Fatal("expected error on 404")
	}
}
