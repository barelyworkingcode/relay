package terminal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// Binaries are built once per test run into a short /tmp dir (macOS caps
// sun_path at 104 chars), mirroring internal/sessions/hostapi and
// internal/sessions/shim's own buildBinaries convention.
var (
	buildOnce        sync.Once
	relaySessionsBin string
	testTargetBin    string
	buildErr         error
)

func buildBinaries(t *testing.T) (relaySessions, testTarget string) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "term-bin-")
		if err != nil {
			buildErr = err
			return
		}
		relaySessionsBin = filepath.Join(dir, "relay-sessions")
		testTargetBin = filepath.Join(dir, "testtarget")
		root := repoRoot(t)
		for _, b := range []struct{ out, pkg string }{
			{relaySessionsBin, "./cmd/relaysessions"},
			{testTargetBin, "./cmd/testtarget"},
		} {
			cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
			cmd.Dir = root
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				buildErr = fmt.Errorf("build %s: %w", b.pkg, err)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatalf("build test binaries: %v", buildErr)
	}
	return relaySessionsBin, testTargetBin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/sessions/terminal -> repo root is three levels up.
	return filepath.Join(dir, "..", "..", "..")
}

func mkShortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("mkShortTempDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeBridgeMode selects how the fake bridge answers a Hello, matching
// internal/sessions/shim's own fixture so the two suites exercise identical
// wire behavior.
type fakeBridgeMode int

const (
	fakeBridgeOK fakeBridgeMode = iota
	fakeBridgeRefuse
)

type capturedHello struct {
	Type  string `json:"type"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

// startFakeBridge serves the Hello wire shape C6 step 3 sends:
// {"type":"Hello","kind":"project_session","name":"<id>","token":"<secret>"}
func startFakeBridge(t *testing.T, mode fakeBridgeMode) (sockPath string, received chan capturedHello) {
	t.Helper()
	dir := mkShortTempDir(t, "fb-")
	sockPath = filepath.Join(dir, "bridge.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("fake bridge listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	received = make(chan capturedHello, 8)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeBridgeConn(conn, mode, received)
		}
	}()
	return sockPath, received
}

func serveFakeBridgeConn(conn net.Conn, mode fakeBridgeMode, received chan capturedHello) {
	defer func() { _ = conn.Close() }()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	var req capturedHello
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return
	}
	received <- req

	var resp map[string]any
	if mode == fakeBridgeOK {
		resp = map[string]any{
			"type": "OK",
			"data": map[string]any{
				"kind":       "project_session",
				"service_id": req.Name,
				"relay_pid":  os.Getpid(),
			},
		}
	} else {
		resp = map[string]any{"type": "Error", "code": 403, "message": "refused"}
	}
	b, _ := json.Marshal(resp)
	b = append(b, '\n')
	_, _ = conn.Write(b)
}
