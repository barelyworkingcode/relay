//go:build live

package sandbox

// Live tier (docs/testing-roadmap.md): these run a real
// /usr/bin/sandbox-exec against a profile this package rendered and assert
// what the kernel actually allowed, not what a tool reported about itself.
// Everything they touch — projects, sockets, listeners, the profile — is
// created under t.TempDir(); no part of this file reads or writes relay's
// real config directory, its sockets, or any live session's state.
//
// A machine without sandbox-exec sees a skip, never a failure.

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sandboxProbe is the action a re-exec of this test binary performs inside
// the sandbox. The helper process exits 0 when the operation succeeded and 1
// when the kernel refused it, which is the only thing the parent asserts on.
const probeEnv = "RELAY_SANDBOX_PROBE"

func TestSandboxProbeHelper(t *testing.T) {
	action := os.Getenv(probeEnv)
	if action == "" {
		t.Skip("helper process only; driven by the live tests in this file")
	}
	verb, arg, _ := strings.Cut(action, " ")
	var err error
	switch verb {
	case "write":
		err = os.WriteFile(arg, []byte("probe\n"), 0o600)
	case "read":
		_, err = os.ReadFile(arg)
	case "connect-unix":
		var c net.Conn
		c, err = net.Dial("unix", arg)
		if c != nil {
			_ = c.Close()
		}
	case "connect-tcp":
		var c net.Conn
		c, err = net.Dial("tcp", arg)
		if c != nil {
			_ = c.Close()
		}
	case "exec":
		err = exec.Command(arg, "-p", fmt.Sprint(os.Getpid())).Run()
	default:
		fmt.Fprintf(os.Stderr, "unknown probe %q\n", action)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe %q: %v\n", action, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runProbe runs one probe inside the sandbox and reports whether the
// operation succeeded.
func runProbe(t *testing.T, profile, action string) bool {
	t.Helper()
	cmd := exec.Command(sandboxExecPath, "-f", profile, "--",
		os.Args[0], "-test.run=^TestSandboxProbeHelper$")
	cmd.Env = append(os.Environ(), probeEnv+"="+action)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("probe %q did not run: %v\n%s", action, err, out)
	}
	t.Logf("probe %q refused: %s", action, out)
	return false
}

// liveFixture is one session's worth of C7: a project the session owns,
// another project it must not reach, a stand-in relay directory holding two
// sockets, and two loopback listeners.
type liveFixture struct {
	profile      string
	projA, projB string
	secret       string
	allowSock    string
	denySock     string
	allowAddr    string
	denyAddr     string
}

func newLiveFixture(t *testing.T) liveFixture {
	t.Helper()
	if err := Available(); err != nil {
		t.Skipf("sandbox-exec unavailable: %v", err)
	}
	// /tmp, not t.TempDir(): macOS's per-test temp path is long enough that
	// a socket under it overflows sun_path's 104 bytes, the same reason
	// internal/sshhost's own tests root themselves here.
	root, err := os.MkdirTemp("/tmp", "relay-sandbox-live-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f := liveFixture{
		projA: filepath.Join(root, "projA"),
		projB: filepath.Join(root, "projB"),
	}
	relayDir := filepath.Join(root, "relay")
	for _, dir := range []string{f.projA, f.projB, relayDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	f.secret = filepath.Join(f.projB, "secret.txt")
	if err := os.WriteFile(f.secret, []byte("other project's secret\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	f.allowSock = listenUnix(t, filepath.Join(relayDir, "relay.sock"))
	f.denySock = listenUnix(t, filepath.Join(relayDir, "relay-frontend.sock"))
	f.allowAddr = listenTCP(t)
	f.denyAddr = listenTCP(t)

	spec := Spec{
		WriteAllowDirs:       []string{f.projA, "/dev"},
		ReadDeny:             []string{f.projB, relayDir},
		UnixConnectDenyDirs:  []string{relayDir},
		UnixConnectAllow:     []string{f.allowSock},
		TCPLoopbackDeny:      []int{port(t, f.denyAddr)},
		TCPLoopbackAllow:     []int{port(t, f.allowAddr)},
		DenySetIDExec:        true,
		WriteAllowFiles:      []string{filepath.Join(f.projA, "state.json")},
		UnixConnectDenyPaths: []string{f.denySock},
	}
	profile, err := Write(filepath.Join(root, "profiles"), "live-session", spec)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	f.profile = profile
	return f
}

func listenUnix(t *testing.T, path string) string {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAll(ln)
	return path
}

func listenTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go acceptAll(ln)
	return ln.Addr().String()
}

func acceptAll(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

func port(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	var n int
	if _, err := fmt.Sscanf(p, "%d", &n); err != nil {
		t.Fatalf("parse port %q: %v", p, err)
	}
	return n
}

func TestLive_WriteOutsideTheProjectIsDenied(t *testing.T) {
	f := newLiveFixture(t)
	if !runProbe(t, f.profile, "write "+filepath.Join(f.projA, "allowed.txt")) {
		t.Error("a write inside the project was denied")
	}
	outside := filepath.Join(filepath.Dir(f.projA), "escaped.txt")
	if runProbe(t, f.profile, "write "+outside) {
		t.Error("a write outside the project succeeded")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("the denied write left a file on disk")
	}
}

func TestLive_OtherProjectIsUnreadableAndUnchanged(t *testing.T) {
	f := newLiveFixture(t)
	before := fileDigest(t, f.secret)
	if runProbe(t, f.profile, "read "+f.secret) {
		t.Error("another project's file was readable")
	}
	if runProbe(t, f.profile, "write "+f.secret) {
		t.Error("another project's file was writable")
	}
	if after := fileDigest(t, f.secret); after != before {
		t.Error("another project's file changed on disk")
	}
}

func TestLive_OnlyTheAllowedSocketsAreReachable(t *testing.T) {
	f := newLiveFixture(t)
	if !runProbe(t, f.profile, "connect-unix "+f.allowSock) {
		t.Error("relay.sock was not reachable")
	}
	if runProbe(t, f.profile, "connect-unix "+f.denySock) {
		t.Error("the frontend socket was reachable")
	}
	if !runProbe(t, f.profile, "connect-tcp "+f.allowAddr) {
		t.Error("the allowed loopback port was not reachable")
	}
	if runProbe(t, f.profile, "connect-tcp "+f.denyAddr) {
		t.Error("a denied loopback port was reachable")
	}
}

// TestLive_SetIDExecIsDenied uses /bin/ps, which is setgid on macOS and is
// the exact binary SP2 row 9 measured — an agent under this profile loses
// ps, deliberately.
func TestLive_SetIDExecIsDenied(t *testing.T) {
	f := newLiveFixture(t)
	if _, err := os.Stat("/bin/ps"); err != nil {
		t.Skipf("no /bin/ps: %v", err)
	}
	if runProbe(t, f.profile, "exec /bin/ps") {
		t.Error("a setuid/setgid binary was executable")
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
