package adopt

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/barelyworkingcode/relay/internal/membership"
)

// watchExit is membership.WatchExit's shape: it calls onExit once when the
// watched process exits, and fails with membership.ErrExited when it is
// already gone or is no longer the process want describes.
type watchExit func(pid int, want membership.ProcInfo, onExit func()) (cancel func(), err error)

// ladder sends one signal at a time to a process it has checked is the one the
// candidate names, and waits for it to exit against the injected clock.
type ladder struct {
	clock Clock
	proc  membership.Source
	watch watchExit
}

// step is one rung. It re-reads the pid's start time and sends nothing when it
// no longer matches the candidate (ErrChanged). It arms the wait and the exit
// watch before the signal goes out, so whoever sees the signal can already move
// the clock or end the process. exited is true when the process is gone at the
// end of the wait, or was gone before the signal; sent is nil when no signal
// went out.
func (l ladder) step(ctx context.Context, c Candidate, sig syscall.Signal, name string, wait time.Duration) (sent *Signal, exited bool, err error) {
	info, ok := l.proc.Info(c.PID)
	if !ok {
		return nil, true, nil
	}
	if !sameStart(info, c.ProcStart) {
		return nil, false, ErrChanged
	}

	at := l.clock.Now()
	timer := l.clock.After(wait)
	gone := make(chan struct{})
	cancel, err := l.watch(c.PID, info, func() { close(gone) })
	if errors.Is(err, membership.ErrExited) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("adopt: watch pid exit: %w", err)
	}
	defer cancel()

	if err := syscall.Kill(c.PID, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("adopt: signal %s: %w", name, err)
	}
	sent = &Signal{Name: name, At: at}

	select {
	case <-gone:
		return sent, true, nil
	case <-ctx.Done():
		return sent, false, ctx.Err()
	case <-timer:
		select {
		case <-gone:
			return sent, true, nil
		default:
			return sent, false, nil
		}
	}
}

// sameStart compares the kernel's start time to the candidate's, microsecond
// for microsecond.
func sameStart(info membership.ProcInfo, start time.Time) bool {
	return info.StartSec == start.Unix() && int64(info.StartUsec) == int64(start.Nanosecond()/1000)
}

func startTime(info membership.ProcInfo) time.Time {
	return time.Unix(info.StartSec, int64(info.StartUsec)*1000).UTC()
}

// interrupt sends SIGINT and waits InterruptWait for the process to exit.
func (l ladder) interrupt(ctx context.Context, c Candidate) ([]Signal, error) {
	sent, _, err := l.step(ctx, c, syscall.SIGINT, "INT", InterruptWait)
	return signals(nil, sent), err
}

// stop sends SIGTERM when the process lives, waits TermWait, then SIGKILL and
// KillWait. It fails with ErrStopFailed when the process outlives both.
func (l ladder) stop(ctx context.Context, c Candidate) ([]Signal, error) {
	var out []Signal
	sent, exited, err := l.step(ctx, c, syscall.SIGTERM, "TERM", TermWait)
	out = signals(out, sent)
	if err != nil || exited {
		return out, err
	}
	sent, exited, err = l.step(ctx, c, syscall.SIGKILL, "KILL", KillWait)
	out = signals(out, sent)
	if err != nil {
		return out, err
	}
	if !exited {
		return out, ErrStopFailed
	}
	return out, nil
}

func signals(out []Signal, sent *Signal) []Signal {
	if sent == nil {
		return out
	}
	return append(out, *sent)
}
