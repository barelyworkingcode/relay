//go:build darwin

package adopt

import (
	"bytes"
	"errors"

	"golang.org/x/sys/unix"
)

// execPath is the path the kernel executed for pid. It is not ps's comm, which
// reflects how a program was launched: kern.procargs2 holds a 4-byte argc and
// then the NUL-terminated exec path.
func execPath(pid int) (string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	if len(raw) < 5 {
		return "", errors.New("adopt: short procargs")
	}
	end := bytes.IndexByte(raw[4:], 0)
	if end <= 0 {
		return "", errors.New("adopt: no exec path in procargs")
	}
	return string(raw[4 : 4+end]), nil
}
