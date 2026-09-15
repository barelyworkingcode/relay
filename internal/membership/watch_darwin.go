//go:build darwin

package membership

import (
	"fmt"
	"sync"

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

// testHookBeforeFireGate and testHookBeforeOnExit are unexported seams the
// tests use to force an exact interleaving against a concurrent cancel();
// both are nil in production and cost nothing when unset.
//
// testHookBeforeFireGate runs on the watcher goroutine right after it
// decides an event warrants firing (a real exit, EV_ERROR, or the
// fail-closed kevent-error path) but before it takes fireMu to check
// cancellation.
//
// testHookBeforeOnExit runs after that check has already committed to
// firing — started is set, fireMu released — but before onExit is actually
// called.
var (
	testHookBeforeFireGate func()
	testHookBeforeOnExit   func()
)

// WatchExit watches pid for exit and calls onExit at most once when it
// does. want pins the exact process instance being watched — the caller
// already read it via Resolve or an Info call.
//
// An error return means the process is already gone, or is not the one the
// caller meant (pid was recycled between the caller's read and this call):
// treat it exactly like an exit and end the session. errors.Is(err,
// ErrExited) distinguishes that case from an infrastructure failure (kqueue
// itself unavailable), which is not the same as the process having exited.
//
// cancel stops the watch. After cancel returns, onExit will not start. If
// onExit had already started before cancel was called, it may still be
// running when cancel returns — cancel does not wait for it. cancel never
// blocks, may be called from inside onExit, from any other goroutine, and
// any number of times.
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

	// This is deliberate: cancelled and started are two plain fields behind
	// one mutex, not independent atomics. The earlier design used separate
	// atomic flags (cancelled, firing) that cancel() and the fire path each
	// read and wrote without a shared critical section — which left a real
	// gap between "an exit event passed the cancelled check" and "firing
	// was actually recorded," during which cancel() could see firing=false,
	// commit to waiting for a stop signal that would now never come (the
	// fire path was about to run onExit, not return), and deadlock against
	// a reentrant cancel() call from inside that very onExit. Serializing
	// both fields through fireMu removes the gap entirely: whichever side
	// reaches the critical section first fully determines the outcome, and
	// cancel() never has to wait on anything to stay correct.
	var fireMu sync.Mutex
	cancelled := false
	started := false

	fire := func() {
		if testHookBeforeFireGate != nil {
			testHookBeforeFireGate()
		}
		fireMu.Lock()
		if cancelled || started {
			fireMu.Unlock()
			return
		}
		started = true
		fireMu.Unlock()

		if testHookBeforeOnExit != nil {
			testHookBeforeOnExit()
		}
		onExit()
	}

	go func() {
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
				// anything, which would be worse than ending it early. It
				// goes through the same fire() gate as a real exit, so a
				// concurrent cancel() still wins if it got there first.
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
			fireMu.Lock()
			cancelled = true
			fireMu.Unlock()

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
			// Deliberately no wait here: blocking until the watcher
			// goroutine stops is exactly what let a reentrant cancel() from
			// inside onExit deadlock against an external caller's cancel()
			// holding this same sync.Once. Once cancelled is recorded above,
			// any fire() that hasn't already committed (started) will see
			// it and refuse to run; one that already committed is allowed
			// to keep running, per the doc comment.
		})
	}
	return cancel, nil
}
