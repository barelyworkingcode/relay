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

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
)

// buildManagers constructs a real terminal.Manager and session.Manager
// rooted under a fresh temp dir, ShimBinary set to the built relay-sessions
// binary — enough for a hermetic /launch dispatch test to spawn a real pty
// session, or a session-kind one via session.Manager.SetProviderFactory
// (session.Manager's own test-only seam, doc.go's package comment).
func buildManagers(t *testing.T) (*terminal.Manager, *session.Manager) {
	t.Helper()
	relaySessionsBin, _ := buildBinaries(t)
	dataDir := mkShortTempDir(t, "hostapi-data-")

	terminals := terminal.NewManager(terminal.Config{
		ShimBinary: relaySessionsBin,
		LogDir:     filepath.Join(dataDir, "terminal_logs"),
	})
	store := session.NewStore(filepath.Join(dataDir, "sessions"))
	sessions := session.NewManager(session.Config{}, store, nil)
	return terminals, sessions
}

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
	c := startChildPost(t, sockPath, path, body)
	return c.PID(), func() int {
		t.Helper()
		status, _ := c.Wait()
		return status
	}
}

// childPost is postAsChildProcess's request, kept so a caller can also read
// the response body or kill the client mid-request.
type childPost struct {
	t                    *testing.T
	cmd                  *exec.Cmd
	statusFile, bodyFile string
}

func startChildPost(t *testing.T, sockPath, path string, body any) *childPost {
	t.Helper()
	dir := mkShortTempDir(t, "curl-")
	reqFile := filepath.Join(dir, "body.json")
	c := &childPost{t: t, statusFile: filepath.Join(dir, "status.txt"), bodyFile: filepath.Join(dir, "resp.json")}

	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	if err := os.WriteFile(reqFile, b, 0o600); err != nil {
		t.Fatalf("write body file: %v", err)
	}

	script := fmt.Sprintf(
		"sleep 0.2; exec curl --unix-socket %q -s -o %q -w '%%{http_code}' -X POST -H 'Content-Type: application/json' --data-binary @%q http://h%s > %q",
		sockPath, c.bodyFile, reqFile, path, c.statusFile,
	)
	c.cmd = exec.Command("sh", "-c", script)
	c.cmd.Stderr = os.Stderr
	if err := c.cmd.Start(); err != nil {
		t.Fatalf("start curl helper: %v", err)
	}
	return c
}

func (c *childPost) PID() int { return c.cmd.Process.Pid }

// Kill drops the connection mid-request, the way a hook dies.
func (c *childPost) Kill() {
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}

func (c *childPost) Wait() (status int, body []byte) {
	c.t.Helper()
	if err := c.cmd.Wait(); err != nil {
		c.t.Fatalf("curl helper: %v", err)
	}
	raw, err := os.ReadFile(c.statusFile)
	if err != nil {
		c.t.Fatalf("read status file: %v", err)
	}
	if _, err := fmt.Sscanf(string(raw), "%d", &status); err != nil {
		c.t.Fatalf("parse status %q: %v", raw, err)
	}
	body, _ = os.ReadFile(c.bodyFile) // curl writes no file for an empty body
	return status, body
}

var (
	testClaudeOnce            sync.Once
	testClaudeBin, testMCPBin string
	testClaudeErr             error
)

// buildTestClaude builds cmd/testclaude and cmd/testmcp once per run. Its own
// Once, apart from buildBinaries, so the tests that don't drive a Claude
// session never depend on the stand-in building.
func buildTestClaude(t *testing.T) (testClaude, testMCP string) {
	t.Helper()
	testClaudeOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "hostapi-claude-bin-")
		if err != nil {
			testClaudeErr = err
			return
		}
		testClaudeBin = filepath.Join(dir, "testclaude")
		testMCPBin = filepath.Join(dir, "testmcp")
		for _, b := range []struct{ out, pkg string }{
			{testClaudeBin, "./cmd/testclaude"},
			{testMCPBin, "./cmd/testmcp"},
		} {
			cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
			cmd.Dir = repoRoot(t)
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				testClaudeErr = fmt.Errorf("build %s: %w", b.pkg, err)
				return
			}
		}
	})
	if testClaudeErr != nil {
		t.Fatalf("build testclaude: %v", testClaudeErr)
	}
	return testClaudeBin, testMCPBin
}
