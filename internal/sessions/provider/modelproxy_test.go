package provider

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartModelProxy_EmptySocketPathIsNoop(t *testing.T) {
	url, proxy, err := startModelProxy("")
	if err != nil {
		t.Fatalf("startModelProxy: %v", err)
	}
	if url != "" || proxy != nil {
		t.Fatalf("expected no-op, got url=%q proxy=%v", url, proxy)
	}
}

func TestStartModelProxy_ForwardsRequestAndHeaders(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "model.sock")

	fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "ping" {
			t.Errorf("body = %q", body)
		}
		w.WriteHeader(http.StatusTeapot)
	})

	baseURL, proxy, err := startModelProxy(sock)
	if err != nil {
		t.Fatalf("startModelProxy: %v", err)
	}
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/models", strings.NewReader("ping"))
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
}
