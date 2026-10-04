package provider

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/logging"
)

// headerRecorder is a fake broker handler that notes each request's trace
// header and path.
type headerRecorder struct {
	mu    sync.Mutex
	paths []string
	trace []string
}

func (h *headerRecorder) handle(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.paths = append(h.paths, r.URL.Path)
	h.trace = append(h.trace, r.Header.Get(logging.TraceHeader))
	h.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func TestChatTransport_NeverSendsTraceHeader(t *testing.T) {
	dir := shortTempDir(t)
	fakeSock := filepath.Join(dir, "fake-broker.sock")
	rec := &headerRecorder{}
	fakeBroker(t, fakeSock, rec.handle)

	cfg := ChatConfig{ModelSocket: filepath.Join(dir, "unreachable.sock"), ModelKey: "test-key"}
	cfg.dial = unixDialer(fakeSock)
	transport := newChatHTTPTransport(cfg, "sonnet", nil)

	ctx := logging.ContextWithTrace(context.Background(), "abcd1234efgh5678")
	resp, err := transport.PostChat(ctx, []map[string]any{{"role": "user", "content": "hi"}}, nil)
	if err != nil {
		t.Fatalf("PostChat: %v", err)
	}
	resp.Body.Close()
	if err := transport.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.paths) != 2 {
		t.Fatalf("seam saw %v, want PostChat and the /v1/models ping", rec.paths)
	}
	for i, got := range rec.trace {
		if got != "" {
			t.Errorf("request %s carried %s = %q, want none", rec.paths[i], logging.TraceHeader, got)
		}
	}
}

func TestModelProxy_AddsNoTraceHeaderToUntracedRequest(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "model.sock")
	rec := &headerRecorder{}
	fakeBroker(t, sock, rec.handle)

	baseURL, proxy, err := startModelProxy(sock)
	if err != nil {
		t.Fatalf("startModelProxy: %v", err)
	}
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.trace) != 1 {
		t.Fatalf("broker saw %d requests, want 1", len(rec.trace))
	}
	if rec.trace[0] != "" {
		t.Errorf("proxy added %s = %q to a request that had none", logging.TraceHeader, rec.trace[0])
	}
}
