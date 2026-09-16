package provider

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// relaySessionsBin is built once per test run into a short /tmp dir (macOS
// caps sun_path at 104 chars), mirroring internal/sessions/terminal and
// internal/sessions/hostapi's own buildBinaries convention.
var (
	buildRelaySessionsOnce sync.Once
	relaySessionsBin       string
	buildRelaySessionsErr  error
)

// buildRelaySessionsBinary returns the path to a real, freshly built
// relay-sessions binary -- used to exercise buildChatMCPManager's tool
// child against the actual `relay-sessions exec` shim, not a stand-in.
func buildRelaySessionsBinary(t *testing.T) string {
	t.Helper()
	buildRelaySessionsOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "rh-provider-bin-")
		if err != nil {
			buildRelaySessionsErr = err
			return
		}
		relaySessionsBin = filepath.Join(dir, "relay-sessions")
		root := repoRoot(t)
		cmd := exec.Command("go", "build", "-o", relaySessionsBin, "./cmd/relaysessions")
		cmd.Dir = root
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			buildRelaySessionsErr = fmt.Errorf("build relay-sessions: %w", err)
		}
	})
	if buildRelaySessionsErr != nil {
		t.Fatalf("build relay-sessions binary: %v", buildRelaySessionsErr)
	}
	return relaySessionsBin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/sessions/provider -> repo root is three levels up.
	return filepath.Join(dir, "..", "..", "..")
}

type capturedHello struct {
	Type  string `json:"type"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

// startFakeBridge serves the Hello wire shape C6 step 3 sends
// ({"type":"Hello","kind":"project_session","name":"<id>","token":"<secret>"}),
// always answering OK -- the one mode this package's own shim-spawn test
// needs. Mirrors internal/sessions/terminal's own startFakeBridge
// (duplicated rather than shared: a cross-package test helper isn't worth
// the added surface for a few lines).
func startFakeBridge(t *testing.T) (sockPath string, received chan capturedHello) {
	t.Helper()
	dir := shortTempDir(t)
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
			go serveFakeBridgeConn(conn, received)
		}
	}()
	return sockPath, received
}

func serveFakeBridgeConn(conn net.Conn, received chan capturedHello) {
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

	resp := map[string]any{
		"type": "OK",
		"data": map[string]any{
			"kind":       "project_session",
			"service_id": req.Name,
			"relay_pid":  os.Getpid(),
		},
	}
	b, _ := json.Marshal(resp)
	b = append(b, '\n')
	_, _ = conn.Write(b)
}

// shortTempDir returns a fresh directory directly under /tmp, short enough
// to hold a Unix socket path under macOS's ~104-byte sun_path limit — t.TempDir()
// nests under a long per-test path that regularly blows that budget for a
// socket file. Mirrors internal/sessions/terminal's own test convention.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rh-provider-")
	if err != nil {
		t.Fatalf("mkdir short temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeBroker starts a Unix-socket HTTP server at sockPath running handler,
// standing in for relay's model.sock. Closed automatically at test cleanup.
func fakeBroker(t *testing.T, sockPath string, handler http.HandlerFunc) {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// writeEnvArgvDumpScript writes a shell script that, on exec, writes its own
// environment and argv to the two files named by the RH_TEST_OUT_ENV and
// RH_TEST_OUT_ARGV env vars (set by the test via t.Setenv so they survive
// into the child's inherited environment), then exits immediately. Used to
// inspect a *real* spawned process's *real* environment and argv on disk —
// the standard internal/sessions/terminal's own review already holds env
// stripping to, per this unit's own instructions — rather than only unit
// testing the argv/env-builder functions in isolation.
func writeEnvArgvDumpScript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "dump.sh")
	// Builds each dump in a *.tmp file and mv's it into place at the very
	// end: mv within the same directory is atomic, so a concurrent reader
	// polling for the final path only ever observes "absent" or "fully
	// written", never a truncated-but-not-yet-appended partial write.
	script := "#!/bin/sh\n" +
		"env > \"$RH_TEST_OUT_ENV.tmp\"\n" +
		"mv \"$RH_TEST_OUT_ENV.tmp\" \"$RH_TEST_OUT_ENV\"\n" +
		": > \"$RH_TEST_OUT_ARGV.tmp\"\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$RH_TEST_OUT_ARGV.tmp\"; done\n" +
		"mv \"$RH_TEST_OUT_ARGV.tmp\" \"$RH_TEST_OUT_ARGV\"\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write dump script: %v", err)
	}
	return path
}

// waitForFile polls until path exists or the deadline passes.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var lines []string
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, string(data[start:i]))
			}
			start = i + 1
		}
	}
	return lines
}
