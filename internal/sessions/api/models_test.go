package api

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeUnixServer starts a Unix-socket HTTP server at sockPath running
// handler, standing in for relay's model.sock. Closed automatically at test
// cleanup. Mirrors internal/sessions/provider's own fakeBroker test helper
// -- duplicated rather than shared, since exporting a test-only helper
// across an internal/ package boundary just to save a few lines isn't
// worth the added surface.
func fakeUnixServer(t *testing.T, sockPath string, handler http.HandlerFunc) {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// TestHandleModels_MergeGolden is the required "/api/models merge golden"
// test: relay-sessions' own fixed Claude aliases, pi's (empty, no pi binary
// configured) list, and a fake model broker's catalog, merged and stamped
// with per-provider capabilities. Compares the full response body
// byte-for-byte against testdata/models_golden.json.
//
// Regenerate the fixture after a deliberate change to the merge shape by
// running this test once with UPDATE_GOLDEN=1 set, then reviewing the diff.
func TestHandleModels_MergeGolden(t *testing.T) {
	// A short /tmp-rooted dir, not t.TempDir(): t.TempDir() nests under a
	// long per-test path that regularly blows macOS's ~104-byte sun_path
	// limit for the socket file below.
	dir, err := os.MkdirTemp("/tmp", "rh-api-models-")
	if err != nil {
		t.Fatalf("mkdir short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "model.sock")

	fakeUnixServer(t, sock, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected no Authorization header on the sessions-capability listing call, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[` +
			`{"id":"sonnet","object":"model","owned_by":"anthropic"},` +
			`{"id":"a","object":"model","owned_by":"llama.cpp"}` +
			`]}`))
	})

	cfg := ModelsConfig{
		PiBinary:    filepath.Join(dir, "no-such-pi-binary"),
		ModelSocket: sock,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	rec := httptest.NewRecorder()
	HandleModels(cfg, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, rec.Body.Bytes(), "", "  "); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	buf.WriteByte('\n')
	got := buf.Bytes()

	goldenPath := filepath.Join("testdata", "models_golden.json")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("response does not match %s\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
	}
}
