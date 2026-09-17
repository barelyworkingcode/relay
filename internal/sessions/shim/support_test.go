package shim_test

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
// sun_path at 104 chars, so any fixture that opens a Unix socket alongside
// these binaries needs a short base dir too), mirroring cmd/relay's own
// buildTestMcpBinary pattern.
var (
	buildOnce        sync.Once
	relaySessionsBin string
	testTargetBin    string
	buildErr         error
)

func buildBinaries(t *testing.T) (relaySessions, testTarget string) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "rs-bin-")
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
	// internal/sessions/shim -> repo root is three levels up.
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

// fakeBridgeMode selects how the fake bridge answers a Hello, so tests can
// drive both the accept and refuse paths of C6 step 3 without a real relay.
type fakeBridgeMode int

const (
	fakeBridgeOK fakeBridgeMode = iota
	fakeBridgeRefuse
)

// capturedHello is exactly what the fake bridge decoded off the wire, so a
// test can assert on the request the shim actually sent rather than only on
// its own answer. A shim that sent "token":"" (or the wrong kind, or the
// wrong name) must fail these assertions even though the fake bridge would
// otherwise have no opinion about it.
type capturedHello struct {
	Type  string `json:"type"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

// startFakeBridge serves exactly the Hello wire shape C6 step 3 sends:
// {"type":"Hello","kind":"project_session","name":"<id>","token":"<secret>"}
// answered with either an OK carrying {kind,service_id,relay_pid} or an
// error type. It is deliberately as small as this file's own hello.go
// counterpart — a real bridge speaks much more, but the shim only ever
// sends this one request over this connection. The returned channel
// receives one capturedHello per accepted connection (buffered generously;
// tests that don't drain it are still fine, it just never blocks the
// server goroutine because it's never read past capacity in practice).
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
	switch mode {
	case fakeBridgeOK:
		resp = map[string]any{
			"type": "OK",
			"data": map[string]any{
				"kind":       "project_session",
				"service_id": req.Name,
				"relay_pid":  os.Getpid(),
			},
		}
	default:
		resp = map[string]any{"type": "Error", "code": 403, "message": "refused"}
	}
	b, _ := json.Marshal(resp)
	b = append(b, '\n')
	_, _ = conn.Write(b)
}

// wantHello fatals unless got matches the exact wire shape C6 step 3
// requires: type Hello, kind project_session, and the given name/token.
func wantHello(t *testing.T, got capturedHello, wantName, wantToken string) {
	t.Helper()
	if got.Type != "Hello" {
		t.Fatalf("hello type = %q, want %q", got.Type, "Hello")
	}
	if got.Kind != "project_session" {
		t.Fatalf("hello kind = %q, want %q", got.Kind, "project_session")
	}
	if got.Name != wantName {
		t.Fatalf("hello name = %q, want %q", got.Name, wantName)
	}
	if got.Token != wantToken {
		t.Fatalf("hello token = %q, want %q", got.Token, wantToken)
	}
}
