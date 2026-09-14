//go:build darwin

package membership

import (
	"fmt"
	"sync"

	"golang.org/x/sys/unix"
)

// WatchExit watches pid for exit and calls onExit exactly once when it does.
// want pins the exact process instance being watched — the caller already
// read it via Resolve or an Info call — so that if pid has already exited
// and been recycled by the time the kqueue registration lands, WatchExit
// returns an error instead of silently watching an unrelated process.
//
// cancel stops the watch and releases its kqueue descriptor. It is safe to
// call more than once and safe to call after onExit has already fired.
func WatchExit(pid int, want ProcInfo, onExit func()) (cancel func(), err error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("membership: kqueue: %w", err)
	}

	changes := []unix.Kevent_t{{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ENABLE,
		Fflags: unix.NOTE_EXIT,
	}}
	if _, regErr := unix.Kevent(kq, changes, nil, nil); regErr != nil {
		_ = unix.Close(kq)
		return nil, fmt.Errorf("membership: register EVFILT_PROC for pid %d: %w", pid, regErr)
	}

	// This is subtle: registering interest in pid's exit, and pid still
	// being the process "want" describes, are two different facts. Between
	// the caller reading want and this registration landing, pid could have
	// exited and the kernel could have handed the same number to an
	// unrelated process — the registration above would then silently watch
	// that impostor. Re-reading the start time closes the race the same way
	// the post-match recheck in Resolve does.
	got, stillThere := NewSource().Info(pid)
	if !stillThere || got.StartSec != want.StartSec || got.StartUsec != want.StartUsec {
		_ = unix.Close(kq)
		return nil, fmt.Errorf("membership: pid %d no longer matches the watched process", pid)
	}

	var closeOnce sync.Once
	closeKQ := func() { closeOnce.Do(func() { _ = unix.Close(kq) }) }
	var fireOnce sync.Once

	go func() {
		events := make([]unix.Kevent_t, 1)
		for {
			n, kerr := unix.Kevent(kq, nil, events, nil)
			if kerr != nil {
				if kerr == unix.EINTR {
					continue
				}
				// EBADF here means cancel() closed kq; any other error
				// means the kqueue is unusable either way. Both end the
				// watch silently rather than firing a spurious exit.
				return
			}
			if n > 0 {
				fireOnce.Do(onExit)
				closeKQ()
				return
			}
		}
	}()

	return closeKQ, nil
}
