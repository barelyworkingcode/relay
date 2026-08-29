//go:build darwin

package bridge

import (
	"net"
	"syscall"
)

// Darwin's LOCAL_PEERPID socket option, from <sys/un.h> and <sys/socket.h>.
// Not exported by the syscall package, so spelled out here.
const (
	solLocal     = 0     // SOL_LOCAL
	localPeerPID = 0x002 // LOCAL_PEERPID
)

// PeerPID is kernel-attested, unlike the caller-asserted Cwd on a
// BridgeRequest: the pid comes from the socket itself, so a caller cannot
// claim to be another process. It is used for audit attribution only and
// never for authorization — a pid is reusable and racy, which is fine for
// "who called this" and not fine for "may they call it".
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
	var inner error
	if err := raw.Control(func(fd uintptr) {
		pid, inner = syscall.GetsockoptInt(int(fd), solLocal, localPeerPID)
	}); err != nil || inner != nil {
		return 0
	}
	return pid
}
