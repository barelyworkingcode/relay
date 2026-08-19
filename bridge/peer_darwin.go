//go:build darwin

package bridge

import (
	"net"
	"syscall"
)

// Darwin's LOCAL_PEERPID socket option, from <sys/un.h> and <sys/socket.h>.
// Not exported by the syscall package, so they're spelled out here.
const (
	solLocal     = 0     // SOL_LOCAL
	localPeerPID = 0x002 // LOCAL_PEERPID
)

// PeerPID returns the process id at the other end of a Unix-domain connection,
// or 0 when it can't be determined (not a unix socket, peer already exited,
// or the syscall failed).
//
// This is kernel-attested, unlike the caller-asserted Cwd on a BridgeRequest:
// the pid comes from the socket itself, so a caller cannot claim to be another
// process. It is used for audit attribution only and never for authorization —
// a pid is reusable and racy, which is fine for "who called this" and not fine
// for "may they call it".
func PeerPID(conn net.Conn) int {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0
	}
	var pid int
	// Control runs fn with the socket fd, guaranteeing it stays valid for the
	// duration; the inner error is captured separately from Control's own.
	var inner error
	if err := raw.Control(func(fd uintptr) {
		pid, inner = syscall.GetsockoptInt(int(fd), solLocal, localPeerPID)
	}); err != nil || inner != nil {
		return 0
	}
	return pid
}
