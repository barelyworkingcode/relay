package peer

import "syscall"

// solLocal and localPeerPID are the Darwin values of SOL_LOCAL and
// LOCAL_PEERPID, which package syscall does not export.
const (
	solLocal     = 0
	localPeerPID = 0x002
)

func peerPID(fd int) (int, error) {
	return syscall.GetsockoptInt(fd, solLocal, localPeerPID)
}
