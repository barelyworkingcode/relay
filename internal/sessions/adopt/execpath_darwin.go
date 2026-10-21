//go:build darwin

package adopt

import (
	"bytes"
	"encoding/binary"
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

// execArgs reads only argc arguments. Environment bytes in the same kernel
// buffer never enter the result, including when Node has erased argv slots.
func execArgs(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	if len(raw) < 5 || len(raw) > 1<<20 {
		return nil, errors.New("adopt: invalid procargs size")
	}
	argc := int(binary.NativeEndian.Uint32(raw[:4]))
	if argc < 1 || argc > 4096 {
		return nil, errors.New("adopt: invalid argc")
	}
	end := bytes.IndexByte(raw[4:], 0)
	if end < 1 {
		return nil, errors.New("adopt: missing exec path")
	}
	offset := 4 + end + 1
	for offset < len(raw) && raw[offset] == 0 {
		offset++
	}
	args := make([]string, 0, argc)
	for i := 0; i < argc; i++ {
		if offset >= len(raw) {
			return nil, errors.New("adopt: incomplete argv")
		}
		end := bytes.IndexByte(raw[offset:], 0)
		if end < 0 {
			return nil, errors.New("adopt: unterminated argv")
		}
		args = append(args, string(raw[offset:offset+end]))
		offset += end + 1
	}
	return args, nil
}
