//go:build darwin

package membership

import (
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// wakeIdent is the EVFILT_USER identifier cancel() triggers to wake the
// watcher goroutine. It only needs to be unique within one kq — each
// WatchExit call owns its own kqueue — so a constant is fine.
const wakeIdent = 1

// WatchExit watches pid for exit and calls onExit exactly once when it does.
// want pins the exact process instance being watched — the caller already
// read it via Resolve or an Info call.
//
// An error return means the process is already gone, or is not the one the
// caller meant (pid was recycled between the caller's read and this call):
// treat it exactly like an exit and end the session. errors.Is(err,
// ErrExited) distinguishes that case from an infrastructure failure (kqueue
// itself unavailable), which is not the same as the process having exited.
//
// cancel stops the watch. It blocks until the watch goroutine has fully
// stopped, which guarantees onExit never fires after cancel returns; do not
// call cancel from inside onExit itself; onExit runs on the watch goroutine
// and cancel waits for that same goroutine to finish, cancel is safe to call
// more than once and after onExit has already fired.
func WatchExit(pid int, want ProcInfo, onExit func()) (cancel func(), err error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, fmt.Errorf("membership: kqueue: %w", err)
	}

	changes := []unix.Kevent_t{
		{
			Ident:  uint64(pid),
			Filter: unix.EVFILT_PROC,
			Flags:  unix.EV_ADD | unix.EV_ENABLE,
			Fflags: unix.NOTE_EXIT,
		},
		{
			// Registered once up front, alongside the real watch, so
			// cancel() only ever has to trigger it, never add it — adding a
			// filter and triggering it are different kevent calls, and
			// cancel() must not race the watcher goroutine to register
			// anything.
			Ident:  wakeIdent,
			Filter: unix.EVFILT_USER,
			Flags:  unix.EV_ADD | unix.EV_CLEAR,
		},
	}
	if _, regErr := unix.Kevent(kq, changes, nil, nil); regErr != nil {
		_ = unix.Close(kq)
		if regErr == unix.ESRCH {
			return nil, fmt.Errorf("membership: pid %d: %w", pid, ErrExited)
		}
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
		return nil, fmt.Errorf("membership: pid %d no longer matches the watched process: %w", pid, ErrExited)
	}

	var cancelled atomic.Bool
	var fireOnce sync.Once
	stopped := make(chan struct{})

	go func() {
		// This is deliberate: only this goroutine ever closes kq, and only
		// as its last act. cancel() cannot close it directly — a close
		// racing this goroutine's own blocked kevent() call (which retries
		// on EINTR, and Go's preemption signal delivers plenty of those)
		// would free the fd number for reuse by a totally unrelated
		// concurrent WatchExit while this goroutine is still using it,
		// letting it steal that watch's exit event or hand its own exit
		// event to nobody. Waking this goroutine via EVFILT_USER and letting
		// it close its own fd removes that race entirely.
		defer close(stopped)
		defer func() { _ = unix.Close(kq) }()

		events := make([]unix.Kevent_t, 1)
		for {
			n, kerr := unix.Kevent(kq, nil, events, nil)
			if kerr != nil {
				if kerr == unix.EINTR {
					continue
				}
				return
			}
			if n == 0 {
				continue
			}
			ev := events[0]

			if ev.Ident == wakeIdent && ev.Filter == unix.EVFILT_USER {
				return // cancelled; no exit observed
			}
			if cancelled.Load() {
				// A real exit event arrived at essentially the same moment
				// as cancellation; cancel() already committed to "onExit
				// will not fire" by the time it observes this goroutine
				// stopping, so honor that rather than firing late.
				return
			}
			if ev.Ident == uint64(pid) && (ev.Flags&unix.EV_ERROR != 0 || ev.Fflags&unix.NOTE_EXIT != 0) {
				fireOnce.Do(onExit)
				return
			}
			// Any other event is unexpected given what this kq registers;
			// keep waiting rather than treating it as an exit.
		}
	}()

	cancel = func() {
		cancelled.Store(true)
		trigger := []unix.Kevent_t{{
			Ident:  wakeIdent,
			Filter: unix.EVFILT_USER,
			Fflags: unix.NOTE_TRIGGER,
		}}
		// A failure here (e.g. the watch already stopped and closed kq)
		// just means there is nothing left to wake; <-stopped below still
		// returns immediately in that case.
		_, _ = unix.Kevent(kq, trigger, nil, nil)
		<-stopped
	}
	return cancel, nil
}
