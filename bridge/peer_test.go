package bridge

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The peer pid is what lets the audit log name the process behind a tool call.
// It must be the *caller's* pid, read from the socket rather than asserted.
func TestPeerPID_ReportsConnectingProcess(t *testing.T) {
	// A short /tmp path: Unix socket paths cap at ~104 chars and t.TempDir()
	// routinely exceeds that on macOS.
	dir, err := os.MkdirTemp("/tmp", "peer")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	got := make(chan int, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- -1
			return
		}
		defer conn.Close()
		got <- PeerPID(conn)
	}()

	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	pid := <-got
	// Both ends are this process, so the peer pid is our own.
	if want := os.Getpid(); pid != want {
		t.Errorf("PeerPID = %d, want %d (this process)", pid, want)
	}
}

// A non-Unix connection has no peer pid to read; "unknown" is a valid answer
// and must not be an error.
func TestPeerPID_ZeroForNonUnixConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	got := make(chan int, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- -1
			return
		}
		defer conn.Close()
		got <- PeerPID(conn)
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if pid := <-got; pid != 0 {
		t.Errorf("PeerPID over TCP = %d, want 0", pid)
	}
}

func TestCallerPIDContext(t *testing.T) {
	ctx := context.Background()
	if got := CallerPIDFromContext(ctx); got != 0 {
		t.Errorf("bare context yields pid %d, want 0", got)
	}
	if got := CallerPIDFromContext(WithCallerPID(ctx, 4242)); got != 4242 {
		t.Errorf("CallerPIDFromContext = %d, want 4242", got)
	}
	// A zero or negative pid means "unknown" and must not be stored.
	if got := CallerPIDFromContext(WithCallerPID(ctx, 0)); got != 0 {
		t.Errorf("zero pid was stored, got %d", got)
	}
}
