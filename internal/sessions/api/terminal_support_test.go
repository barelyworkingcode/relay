package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Binaries are built once per test run into a short /tmp dir, mirroring
// internal/sessions/terminal's own buildBinaries convention — duplicated
// locally rather than exported from that package, since only this test
// file needs a real shim to drive a terminal.Manager end to end over WS.
var (
	buildOnce        sync.Once
	relaySessionsBin string
	buildErr         error
)

func buildRelaySessionsBin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "api-term-bin-")
		if err != nil {
			buildErr = err
			return
		}
		relaySessionsBin = filepath.Join(dir, "relay-sessions")
		root := repoRoot(t)
		cmd := exec.Command("go", "build", "-o", relaySessionsBin, "./cmd/relaysessions")
		cmd.Dir = root
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			buildErr = fmt.Errorf("build cmd/relaysessions: %w", err)
		}
	})
	if buildErr != nil {
		t.Fatalf("build relay-sessions binary: %v", buildErr)
	}
	return relaySessionsBin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/sessions/api -> repo root is three levels up.
	return filepath.Join(dir, "..", "..", "..")
}

// dialHub upgrades an httptest server around hub.HandleUpgrade and returns a
// connected client. t.Cleanup closes it.
func dialHub(t *testing.T, hub *Hub) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(hub.HandleUpgrade))
	t.Cleanup(srv.Close)

	wsURL := "ws" + srv.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readJSONWithTimeout(t *testing.T, conn *websocket.Conn, timeout time.Duration) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var msg map[string]any
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read json: %v", err)
	}
	return msg
}
