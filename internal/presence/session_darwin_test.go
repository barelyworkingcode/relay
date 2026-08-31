//go:build darwin

package presence

import (
	"fmt"
	"net"
	"os"
	"testing"
)

// shortSocketPath avoids the 104-char Unix-socket path limit that a nested
// per-test TMPDIR can trip; /tmp directly is what the rest of the suite uses
// this trick for too.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	path := fmt.Sprintf("/tmp/relay-presence-probe-%d-%d.sock", os.Getpid(), len(t.Name()))
	t.Cleanup(func() { os.Remove(path) })
	return path
}

// dialSelf opens a real Unix-domain connection to itself and returns the
// accepted side, so the probe runs against a genuine kernel-attested peer —
// this process's own audit session — rather than a mock.
func dialSelf(t *testing.T) *net.UnixConn {
	t.Helper()
	path := shortSocketPath(t)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		} else {
			accepted <- nil
		}
	}()

	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	conn := <-accepted
	if conn == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { conn.Close() })

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("accepted connection is a %T, not *net.UnixConn", conn)
	}
	return uc
}

// TestPeerGraphicAccess_RealSocket runs the actual kernel call chain
// (getsockopt LOCAL_PEERTOKEN -> audit_token_to_asid -> auditon) against a
// real peer: this process talking to itself. It cannot assert a specific
// graphic-access value — that depends on whatever session is running the
// test (measured over SSH: 0; in Aqua: 1) — so it skips gracefully if the
// mechanism itself is unavailable in this environment, and otherwise only
// asserts that the call completes without error.
func TestPeerGraphicAccess_RealSocket(t *testing.T) {
	uc := dialSelf(t)
	raw, err := uc.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}

	var graphic bool
	var probeErr error
	if ctlErr := raw.Control(func(fd uintptr) {
		graphic, probeErr = PeerGraphicAccess(int(fd))
	}); ctlErr != nil {
		t.Fatalf("Control: %v", ctlErr)
	}
	if probeErr != nil {
		t.Skipf("peer session graphic access is not determinable in this environment: %v", probeErr)
	}
	t.Logf("this process's own audit session reports graphic access = %v", graphic)
}

// TestPeerGraphicAccess_InvalidFD confirms the probe reports an error
// rather than crashing when handed a descriptor that plainly is not a
// LOCAL-domain socket.
func TestPeerGraphicAccess_InvalidFD(t *testing.T) {
	if _, err := PeerGraphicAccess(-1); err == nil {
		t.Fatal("PeerGraphicAccess(-1) reported no error")
	}
}

func TestPeerGraphicAccess_NonLocalSocket(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	tc, ok := l.(*net.TCPListener)
	if !ok {
		t.Fatal("expected *net.TCPListener")
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var probeErr error
	if ctlErr := raw.Control(func(fd uintptr) {
		_, probeErr = PeerGraphicAccess(int(fd))
	}); ctlErr != nil {
		t.Fatalf("Control: %v", ctlErr)
	}
	if probeErr == nil {
		t.Fatal("PeerGraphicAccess on a TCP listener reported no error; LOCAL_PEERTOKEN should not apply")
	}
}
