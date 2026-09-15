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
// WatchExit call owns its own kqueue — so a constant is fine, but that also
// means every watch's wake trigger looks identical to every other watch's:
// nothing about the trigger itself names which kq it was meant for. Only
// sending it to the right fd number, and never to a closed or reused one,
// keeps that safe (see kqMu below).
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
// cancel stops the watch and blocks until it has fully stopped, so onExit
// never fires after cancel returns — except when cancel is called from
// inside onExit itself, which cannot block on its own completion; that call
// returns immediately once the cancellation is recorded. cancel is
// idempotent: later calls, from any goroutine, are no-ops.
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

	// This is deliberate: kq's lifetime is owned jointly by the watcher
	// goroutine and cancel(), and kqMu is what makes that safe. Closing kq
	// and sending it a wake trigger must never happen concurrently — a
	// trigger sent to an fd number that was *just* closed can land on a
	// completely different, concurrently-opened WatchExit's kqueue instead
	// (every watch shares the same wakeIdent), silently cancelling the
	// wrong watch. Holding kqMu across both "am I already closed" and the
	// actual close, or the actual trigger send, means whichever happens
	// first is what the other observes: the goroutine's close always either
	// fully precedes or fully follows any given trigger attempt, never
	// interleaves with it.
	var kqMu sync.Mutex
	kqClosed := false

	closeKQ := func() {
		kqMu.Lock()
		defer kqMu.Unlock()
		if kqClosed {
			return
		}
		kqClosed = true
		_ = unix.Close(kq)
	}

	var cancelled atomic.Bool
	var firing atomic.Bool
	var fireOnce sync.Once
	stopped := make(chan struct{})

	fire := func() {
		firing.Store(true)
		fireOnce.Do(onExit)
		firing.Store(false)
	}

	go func() {
		defer close(stopped)
		defer closeKQ()

		events := make([]unix.Kevent_t, 1)
		for {
			n, kerr := unix.Kevent(kq, nil, events, nil)
			if kerr != nil {
				if kerr == unix.EINTR {
					continue
				}
				// This is deliberate: an unexpected kqueue error leaves no
				// way to tell whether pid is still alive. Firing onExit
				// fails closed — the caller ends the session — rather than
				// leaving a watch that has silently stopped monitoring
				// anything, which would be worse than ending it early.
				fire()
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
				fire()
				return
			}
			// Any other event is unexpected given what this kq registers;
			// keep waiting rather than treating it as an exit.
		}
	}()

	var cancelOnce sync.Once
	cancel = func() {
		cancelOnce.Do(func() {
			cancelled.Store(true)

			kqMu.Lock()
			if !kqClosed {
				trigger := []unix.Kevent_t{{
					Ident:  wakeIdent,
					Filter: unix.EVFILT_USER,
					Fflags: unix.NOTE_TRIGGER,
				}}
				for {
					_, terr := unix.Kevent(kq, trigger, nil, nil)
					if terr == unix.EINTR {
						continue
					}
					break
				}
			}
			kqMu.Unlock()

			if firing.Load() {
				// Reentrant: onExit (running on the watcher goroutine)
				// called cancel(). stopped won't close until onExit
				// returns, and onExit hasn't returned yet — we're inside
				// it — so waiting here would deadlock forever.
				return
			}
			<-stopped
		})
	}
	return cancel, nil
}
