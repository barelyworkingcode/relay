package hostapi_test

import (
	"bytes"
	"context"
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

var (
	buildOnce        sync.Once
	relaySessionsBin string
	testTargetBin    string
	buildErr         error
)

// buildBinaries builds cmd/relaysessions and cmd/testtarget once per test
// run, mirroring cmd/relay's buildTestMcpBinary/support_test.go convention.
func buildBinaries(t *testing.T) (relaySessions, testTarget string) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "hostapi-bin-")
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

// unixClient dials a Unix socket for a test HTTP request from this test
// process's own pid — enough for /launch's tests, which check peer identity
// against a configured RelayPID rather than real C3 ancestry. /permission
// tests need a distinct child process instead (postAsChildProcess, below):
// this package's hostapi.Server runs in-process in every test here, so its
// own HostPID() is this test binary's pid, and membership.Resolve's walk
// stops dead at "peer == host" — a session root would have to be a
// different, real process for the walk to ever reach it.
func unixClient(sockPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
		Timeout: 10 * time.Second,
	}
}

func postJSON(t *testing.T, client *http.Client, url, bearer string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// postAsChildProcess POSTs body to path over sockPath from a genuine, separate
// OS process (curl), so the connection's real kernel peer pid is that
// process's, not this test binary's. It returns that pid immediately after
// starting — before curl has necessarily connected — so a caller can
// register it as a membership root first: the outer `sh` sleeps briefly
// before `exec`ing into curl, which keeps the pid unchanged (exec never
// resets a process's kernel-recorded start time) while guaranteeing the
// registration happens before the real connection attempt.
func postAsChildProcess(t *testing.T, sockPath, path string, body any) (pid int, wait func() (status int)) {
	t.Helper()
	dir := mkShortTempDir(t, "curl-")
	bodyFile := filepath.Join(dir, "body.json")
	outFile := filepath.Join(dir, "status.txt")

	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	if err := os.WriteFile(bodyFile, b, 0o600); err != nil {
		t.Fatalf("write body file: %v", err)
	}

	script := fmt.Sprintf(
		"sleep 0.2; exec curl --unix-socket %q -s -o /dev/null -w '%%{http_code}' -X POST -H 'Content-Type: application/json' --data-binary @%q http://h%s > %q",
		sockPath, bodyFile, path, outFile,
	)
	cmd := exec.Command("sh", "-c", script)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start curl helper: %v", err)
	}
	return cmd.Process.Pid, func() int {
		t.Helper()
		if err := cmd.Wait(); err != nil {
			t.Fatalf("curl helper: %v", err)
		}
		raw, err := os.ReadFile(outFile)
		if err != nil {
			t.Fatalf("read status file: %v", err)
		}
		var code int
		if _, err := fmt.Sscanf(string(raw), "%d", &code); err != nil {
			t.Fatalf("parse status %q: %v", raw, err)
		}
		return code
	}
}
