package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrOwnedByAnotherTray means another Relay tray already holds dir.
var ErrOwnedByAnotherTray = errors.New("another Relay tray already owns this configuration directory")

const ownerLockName = "tray.lock"

// AcquireTrayOwnership takes the exclusive, non-blocking ownership lock for
// dir and returns its release function. It never waits and never disturbs the
// current owner: a second tray gets ErrOwnedByAnotherTray.
//
// This is deliberate: the lock is an flock on an open descriptor, so a crashed
// tray releases it with the process and no stale lock file can wedge startup.
// The file itself is never removed; unlinking it would let a second tray lock
// a fresh inode while the first still holds the old one.
func AcquireTrayOwnership(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create configuration directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, ownerLockName), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open ownership lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", ErrOwnedByAnotherTray, dir)
		}
		return nil, fmt.Errorf("lock configuration directory: %w", err)
	}
	return func() { f.Close() }, nil
}
