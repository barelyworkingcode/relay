// Package peer reads the process id of a Unix socket's peer with the standard
// library only, so the build needs no cgo.
package peer

import (
	"errors"
	"net"
	"syscall"
)

// PID returns the peer's pid. It fails when the connection is not a Unix
// connection or the kernel does not answer.
func PID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("peer: not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var serr error
	if err := raw.Control(func(fd uintptr) { pid, serr = peerPID(int(fd)) }); err != nil {
		return 0, err
	}
	if serr == nil && pid <= 0 {
		serr = syscall.EINVAL
	}
	return pid, serr
}
