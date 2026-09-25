package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// TestMain points HOME at a throwaway directory for the whole package, so a
// test that resolves a default bridge or model socket path cannot reach the
// real ~/Library/Application Support/relay. Tests that set their own config
// dir override layer over this.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "relaysessions-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "relaysessions tests: create sandbox home:", err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", home+"/.config")
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// TestReportSessionExited_DialsConfiguredSocket_NotRederivedDefault: under
// a bridge socket path this process was actually launched with — matching
// what a real `relay --config-dir` override
// produces — reportSessionExited must dial that path, not
// bridge.SocketPath()'s own rederived default. wrongDir stands in for that
// rederived default (bridge.ConfigDir()'s override, set here to a directory
// with no listener at all) — a dial that lands there instead proves the bug;
// a dial that lands at rightSock proves the fix.
func TestReportSessionExited_DialsConfiguredSocket_NotRederivedDefault(t *testing.T) {
	wrongDir := t.TempDir()
	bridge.SetConfigDirForTest(wrongDir)
	t.Cleanup(func() { bridge.SetConfigDirForTest("") })

	// /tmp directly, not t.TempDir(): macOS caps a Unix socket path at 104
	// chars, which t.TempDir() paths often exceed (internal/bridge/
	// contract_test.go's startTestBridge makes the same choice).
	rightDir, err := os.MkdirTemp("/tmp", "rsx")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rightDir) })
	rightSock := filepath.Join(rightDir, "relay.sock")

	ln, err := net.Listen("unix", rightSock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case accepted <- struct{}{}:
		default:
		}
		c.Close()
	}()

	reportSessionExited(rightSock, "sess-1", 0, 0, "exit")

	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("reportSessionExited never dialed the configured bridge socket (it dialed the rederived default instead, or nothing at all)")
	}
}

// TestDefaultDataDir_ReusesBridgeSocketDir: given a non-default bridgeSock,
// the data dir must sit beside it, not at a freshly rederived
// os.UserConfigDir().
func TestDefaultDataDir_ReusesBridgeSocketDir(t *testing.T) {
	got := defaultDataDir("/custom/cfg/relay.sock")
	want := filepath.Join("/custom/cfg", "sessions")
	if got != want {
		t.Fatalf("defaultDataDir(bridgeSock) = %q, want %q", got, want)
	}
}

// TestDefaultModelSocket_ReusesBridgeSocketDir mirrors
// TestDefaultDataDir_ReusesBridgeSocketDir for the model.sock fallback.
func TestDefaultModelSocket_ReusesBridgeSocketDir(t *testing.T) {
	got := defaultModelSocket("/custom/cfg/relay.sock")
	want := filepath.Join("/custom/cfg", "model.sock")
	if got != want {
		t.Fatalf("defaultModelSocket(bridgeSock) = %q, want %q", got, want)
	}
}

// TestParseServiceArgs_BridgeSocketFromEnv_PropagatesToDataDirAndModelSocket
// proves parseServiceArgs itself resolves bridgeSock (flag, else env) before
// computing the data-dir and model-socket defaults, rather than each
// re-deriving its own — the same wiring reportSessionExited relies on.
func TestParseServiceArgs_BridgeSocketFromEnv_PropagatesToDataDirAndModelSocket(t *testing.T) {
	t.Setenv(bridge.EnvBridgeSocket, "/custom/cfg/relay.sock")
	t.Setenv("RELAY_MODEL_SOCKET", "")

	cfg, err := parseServiceArgs([]string{"-internal-socket", "/tmp/i.sock", "-hook-socket", "/tmp/h.sock"})
	if err != nil {
		t.Fatalf("parseServiceArgs: %v", err)
	}
	if cfg.bridgeSocket != "/custom/cfg/relay.sock" {
		t.Fatalf("bridgeSocket = %q, want the env value", cfg.bridgeSocket)
	}
	if want := filepath.Join("/custom/cfg", "sessions"); cfg.dataDir != want {
		t.Fatalf("dataDir = %q, want %q", cfg.dataDir, want)
	}
	if want := filepath.Join("/custom/cfg", "model.sock"); cfg.modelSocket != want {
		t.Fatalf("modelSocket = %q, want %q", cfg.modelSocket, want)
	}
}
