package provider

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// TestStartModelProxy_HeaderlessRequestNeverReachesBroker is the fix for the
// unauthenticated-access shape: a request with neither Authorization nor
// x-api-key must get a 401 from the proxy itself, and — the property that
// actually matters — must never cause the Unix socket to be dialed at all,
// since dialing it authenticates as relay-sessions' own trusted identity
// regardless of what the original caller sent.
func TestStartModelProxy_HeaderlessRequestNeverReachesBroker(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "model.sock")

	var brokerContacted atomic.Bool
	fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
		brokerContacted.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	baseURL, proxy, err := startModelProxy(sock)
	if err != nil {
		t.Fatalf("startModelProxy: %v", err)
	}
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/models", strings.NewReader("ping"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if brokerContacted.Load() {
		t.Error("fake broker was contacted for a headerless request; it must never be dialed")
	}
}

// TestStartModelProxy_ConnectionHeaderCannotStripCredential is the
// regression test for the gap in requireAPIKeyHeader's fix: a client can
// name Authorization (or X-Api-Key) in its own Connection header to get
// net/http's stock hop-by-hop stripping to remove it from the outbound
// request after requireAPIKeyHeader has already let the request through.
// The broker must still see the real credential.
func TestStartModelProxy_ConnectionHeaderCannotStripCredential(t *testing.T) {
	for _, tc := range []struct {
		name       string
		headerName string
		headerVal  string
	}{
		{"Authorization", "Authorization", "Bearer sneaky"},
		{"X-Api-Key", "X-Api-Key", "sneaky2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortTempDir(t)
			sock := filepath.Join(dir, "model.sock")

			var brokerSawAuth, brokerSawKey string
			fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
				brokerSawAuth = r.Header.Get("Authorization")
				brokerSawKey = r.Header.Get("X-Api-Key")
				w.WriteHeader(http.StatusOK)
			})

			baseURL, proxy, err := startModelProxy(sock)
			if err != nil {
				t.Fatalf("startModelProxy: %v", err)
			}
			defer proxy.Close()

			req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/models", strings.NewReader("ping"))
			req.Header.Set(tc.headerName, tc.headerVal)
			req.Header.Set("Connection", tc.headerName)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}

			switch tc.headerName {
			case "Authorization":
				if brokerSawAuth != tc.headerVal {
					t.Errorf("brokerSawAuth = %q, want %q", brokerSawAuth, tc.headerVal)
				}
			case "X-Api-Key":
				if brokerSawKey != tc.headerVal {
					t.Errorf("brokerSawKey = %q, want %q", brokerSawKey, tc.headerVal)
				}
			}
		})
	}
}

// TestStartModelProxy_XAPIKeyHeaderAloneIsAccepted confirms the alternate
// accepted header is actually honored, not just Authorization.
func TestStartModelProxy_XAPIKeyHeaderAloneIsAccepted(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "model.sock")

	var brokerContacted atomic.Bool
	fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
		brokerContacted.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	baseURL, proxy, err := startModelProxy(sock)
	if err != nil {
		t.Fatalf("startModelProxy: %v", err)
	}
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/models", strings.NewReader("ping"))
	req.Header.Set("x-api-key", "test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if !brokerContacted.Load() {
		t.Error("fake broker was not contacted for a request with x-api-key set")
	}
}
