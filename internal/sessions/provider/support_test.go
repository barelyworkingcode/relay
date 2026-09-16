package provider

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
