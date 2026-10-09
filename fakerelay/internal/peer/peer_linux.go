package peer

import "syscall"

func peerPID(fd int) (int, error) {
	cred, err := syscall.GetsockoptUcred(fd, syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(cred.Pid), nil
}
