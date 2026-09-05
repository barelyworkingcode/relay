package relayfs

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"

	"github.com/hugelgupf/p9/linux"
)

// toErrno maps a Go error from an os.Root operation (or anything else this
// package produces) to the Linux errno the 9P wire needs. Every message a
// caller of this package sees is a bare linux.Errno value — never a raw Go
// error whose text might embed this Root's own host path (os.Root embeds
// the ROOT-RELATIVE name a caller passed in its *fs.PathError, never the
// root's own absolute host path, but this function still never inspects
// err.Error() for content — only for the one structural match below that
// the standard library gives no sentinel for).
//
// It returns a plain error on purpose, nil for a nil input: linux.Errno(0)
// would be a non-nil typed error ("Success") and would read as a failure
// to whatever checks the return for nil.
func toErrno(err error) error {
	if err == nil {
		return nil
	}
	// Already a linux.Errno (returned directly by this package's own
	// code for a specific containment-table rule): pass it through.
	var le linux.Errno
	if errors.As(err, &le) {
		return le
	}
	// errno-level checks run before the sentinel match below, because the
	// standard library deliberately collapses distinct errnos onto one
	// sentinel: ENOTEMPTY counts as ErrExist on unix, and an rmdir of a
	// non-empty directory must reach the wire as ENOTEMPTY, not EEXIST.
	var se syscall.Errno
	if errors.As(err, &se) && se == syscall.ENOTEMPTY {
		return linux.ENOTEMPTY
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return linux.ENOENT
	case errors.Is(err, fs.ErrExist):
		return linux.EEXIST
	case errors.Is(err, fs.ErrPermission):
		return linux.EACCES
	case isPathEscape(err):
		// os.Root's containment refusal. Never reachable through a real
		// walk (checkSafeName already refused ".."; every component this
		// package appends is a validated single name), but a defence in
		// depth path for anything unforeseen.
		return linux.EACCES
	case isNotADirectory(err):
		return linux.ENOTDIR
	default:
		return linux.EIO
	}
}

// isPathEscape recognises os.Root's own "path escapes from parent" refusal
// — see fsmcp/internal/fsapi/root.go's identical helper and its comment for
// why this is a string match rather than a sentinel (the standard library
// doesn't export one).
func isPathEscape(err error) bool {
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return false
	}
	return strings.Contains(pe.Err.Error(), "path escapes from parent")
}

// isNotADirectory recognises a path component that exists but is not a
// directory (e.g. "file.txt/x").
func isNotADirectory(err error) bool {
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return false
	}
	return strings.Contains(pe.Err.Error(), "not a directory")
}
