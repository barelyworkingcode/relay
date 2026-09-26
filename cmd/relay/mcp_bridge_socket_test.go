//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
)

var (
	relayBinOnce sync.Once
	relayBinPath string
	relayBinErr  error
)

// buildRelayBinary builds ./cmd/relay once per run. The stdio server has to
// run as its own process under an environment the test controls, which
// runCLISubprocess cannot give it: that helper always adds --config-dir.
func buildRelayBinary(t *testing.T) string {
	t.Helper()
	relayBinOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "relay-bin-")
		if err != nil {
			relayBinErr = err
			return
		}
		path := filepath.Join(dir, "relay")
		cmd := exec.Command("go", "build", "-o", path, "./cmd/relay")
		cmd.Dir = repoRoot(t)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			relayBinErr = err
			return
		}
		relayBinPath = path
	})
	if relayBinErr != nil {
		t.Fatalf("build cmd/relay: %v", relayBinErr)
	}
	return relayBinPath
}

// decoySocket accepts and drops every connection, counting them.
type decoySocket struct{ accepted atomic.Int32 }

func listenDecoy(t *testing.T, path string) *decoySocket {
	t.Helper()
	assertNoErr(t, os.MkdirAll(filepath.Dir(path), 0o700), "mkdir decoy dir")
	ln, err := net.Listen("unix", path)
	assertNoErr(t, err, "listen decoy %s", path)
	t.Cleanup(func() { _ = ln.Close() })
	d := &decoySocket{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			d.accepted.Add(1)
			_ = conn.Close()
		}
	}()
	return d
}

// childHome is a temp HOME for a `relay mcp` child, and the default bridge
// socket that child derives from it.
func childHome(t *testing.T) (home, defaultSock string) {
	home = mkShortTempDir(t, "rm-home-")
	return home, filepath.Join(home, "Library", "Application Support", "relay", "relay.sock")
}

type relayMcpRun struct {
	stdout, stderr string
	exitCode       int
}

// runRelayMcp runs `relay [args] mcp --token …` with exactly env plus HOME
// and XDG_CONFIG_HOME, feeds it initialize and tools/list, then closes stdin.
func runRelayMcp(t *testing.T, home, dir string, env []string, args ...string) relayMcpRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	argv := append(append([]string{}, args...), "mcp", "--token", testToken)
	cmd := exec.CommandContext(ctx, buildRelayBinary(t), argv...)
	cmd.Dir = dir
	cmd.Env = append([]string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=/usr/bin:/bin"}, env...)
	cmd.Stdin = strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("relay mcp did not exit within 20s; stderr:\n%s", stderr.String())
	}
	run := relayMcpRun{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("run relay mcp: %v", err)
	}
	return run
}

// serveAcmeBridge starts a bridge server whose project token sees one MCP
// with an echo tool, in a sandboxed config dir, and returns its socket.
func serveAcmeBridge(t *testing.T) string {
	t.Helper()
	mkSandboxRelayHome(t)
	return startAcmeBridge(t)
}

// startAcmeBridge is serveAcmeBridge in whatever config dir is already set.
func startAcmeBridge(t *testing.T) string {
	t.Helper()
	router := setupRouter(t, map[string]config.Permission{"acme": config.PermOn}, nil, nil,
		map[string]*mockMcpConn{"acme": newMockConn("acme", localTools("echo"), okHandler(`{"content":[]}`))})
	bsrv, err := bridge.NewBridgeServer(context.Background(), router)
	assertNoErr(t, err, "NewBridgeServer")
	go func() { _ = bsrv.Serve() }()
	t.Cleanup(bsrv.Close)
	return bridge.SocketPath()
}

func assertListedEcho(t *testing.T, run relayMcpRun) {
	t.Helper()
	for _, line := range strings.Split(run.stdout, "\n") {
		var resp struct {
			ID     int `json:"id"`
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(line), &resp) != nil || resp.ID != 2 {
			continue
		}
		for _, tool := range resp.Result.Tools {
			if tool.Name == "echo" {
				return
			}
		}
	}
	t.Fatalf("tools/list did not return echo (exit %d)\nstdout:\n%s\nstderr:\n%s", run.exitCode, run.stdout, run.stderr)
}

func assertExitedLoudly(t *testing.T, run relayMcpRun, wantPrefix string) {
	t.Helper()
	if run.exitCode != 1 || run.stdout != "" {
		t.Fatalf("exit %d with stdout %q, want exit 1 before serving anything; stderr:\n%s", run.exitCode, run.stdout, run.stderr)
	}
	lines := strings.Split(strings.TrimRight(run.stderr, "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], wantPrefix) {
		t.Fatalf("stderr = %q, want one line starting %q", run.stderr, wantPrefix)
	}
}

func assertUntouched(t *testing.T, name string, d *decoySocket) {
	t.Helper()
	if n := d.accepted.Load(); n != 0 {
		t.Fatalf("%s accepted %d connection(s), want 0", name, n)
	}
}

func TestRelayMcp_DialsRelayBridgeSocketFromEnv(t *testing.T) {
	sock := serveAcmeBridge(t)
	home, defaultSock := childHome(t)
	decoy := listenDecoy(t, defaultSock)

	run := runRelayMcp(t, home, home, []string{"RELAY_BRIDGE_SOCKET=" + sock})

	assertUntouched(t, "the default socket", decoy)
	assertListedEcho(t, run)
}

func TestRelayMcp_ConfigDirFlagOutranksEnv(t *testing.T) {
	sock := serveAcmeBridge(t)
	home, _ := childHome(t)
	envSock := filepath.Join(mkShortTempDir(t, "rm-env-"), "relay.sock")
	decoy := listenDecoy(t, envSock)

	run := runRelayMcp(t, home, home, []string{"RELAY_BRIDGE_SOCKET=" + envSock}, "--config-dir", filepath.Dir(sock))

	assertUntouched(t, "the RELAY_BRIDGE_SOCKET decoy", decoy)
	assertListedEcho(t, run)
}

func TestRelayMcp_UnreachableEnvSocketExitsLoudly(t *testing.T) {
	mkSandboxRelayHome(t)
	home, defaultSock := childHome(t)
	decoy := listenDecoy(t, defaultSock)
	absent := filepath.Join(mkShortTempDir(t, "rm-absent-"), "relay.sock")

	run := runRelayMcp(t, home, home, []string{"RELAY_BRIDGE_SOCKET=" + absent})

	assertUntouched(t, "the default socket", decoy)
	assertExitedLoudly(t, run, "error: relay mcp: bridge socket "+absent+" (from RELAY_BRIDGE_SOCKET) is unreachable: ")
}

// A live socket at the relative path proves the refusal is about the value's
// form, not about nothing listening there.
func TestRelayMcp_RelativeEnvSocketRefused(t *testing.T) {
	mkSandboxRelayHome(t)
	home, defaultSock := childHome(t)
	defaultDecoy := listenDecoy(t, defaultSock)
	cwd := mkShortTempDir(t, "rm-cwd-")
	relativeDecoy := listenDecoy(t, filepath.Join(cwd, "relay.sock"))

	run := runRelayMcp(t, home, cwd, []string{"RELAY_BRIDGE_SOCKET=relay.sock"})

	assertUntouched(t, "the default socket", defaultDecoy)
	assertUntouched(t, "the socket at the relative path", relativeDecoy)
	assertExitedLoudly(t, run, `error: relay mcp: RELAY_BRIDGE_SOCKET must be an absolute path, got "relay.sock"`)
}

func TestRelayMcp_UnreachableDefaultSocketExitsLoudly(t *testing.T) {
	mkSandboxRelayHome(t)
	home, defaultSock := childHome(t)

	run := runRelayMcp(t, home, home, nil)

	assertExitedLoudly(t, run, "error: relay mcp: bridge socket "+defaultSock+" (from default) is unreachable: ")
}

func TestRelayMcp_DefaultSocketStillServes(t *testing.T) {
	mkSandboxRelayHome(t)
	home, defaultSock := childHome(t)
	bridge.SetConfigDirForTest(filepath.Dir(defaultSock))
	if sock := startAcmeBridge(t); sock != defaultSock {
		t.Fatalf("bridge listens on %s, want the child's default %s", sock, defaultSock)
	}

	run := runRelayMcp(t, home, home, nil)

	assertListedEcho(t, run)
}
