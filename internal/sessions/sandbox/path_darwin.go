//go:build darwin

package sandbox

import (
	"bytes"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// onDiskPath returns p spelled the way the volume stores it, or p unchanged
// when the kernel will not say. It asks the kernel for the path of an open
// descriptor (F_GETPATH), which is the canonical spelling Seatbelt matches
// against; it also maps the data-volume firmlinks back to their `/Users` and
// `/private` names. p must already be symlink-free.
func onDiskPath(p string) string {
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return p
	}
	defer unix.Close(fd)

	buf := make([]byte, unix.PathMax)
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0])))); err != nil {
		return p
	}
	runtime.KeepAlive(buf)
	n := bytes.IndexByte(buf, 0)
	if n <= 0 {
		return p
	}
	return string(buf[:n])
}
