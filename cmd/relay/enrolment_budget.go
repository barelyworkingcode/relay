package main

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

type enrolmentBudgets struct {
	mu      sync.RWMutex
	windows map[string]*budgetWindow

	clock func() time.Time
}

// budgetWindow is one enrolment's rolling window. Both series are kept as
// timestamped entries rather than as counters reset on a clock boundary: a
// fixed bucket hands an attacker 2x the budget for free by straddling the
// reset — spend it all just before the boundary, spend it all again just
// after.
type budgetWindow struct {
	mu     sync.Mutex
	calls  []time.Time
	volume []volumeSample
	bytes  int64
}

type volumeSample struct {
	at    time.Time
	bytes int64
}

func (s *Settings) enrolmentBudget(rc bridge.RemoteCaller) EnrolmentBudget {
	if s != nil {
		if e := s.FindEnrolmentByFingerprint(rc.Fingerprint); e != nil {
			return normalizeEnrolmentBudget(e.Budget)
		}
	}
	return normalizeEnrolmentBudget(EnrolmentBudget{})
}

func (b *enrolmentBudgets) admit(rc bridge.RemoteCaller, budget EnrolmentBudget) error {
	budget = normalizeEnrolmentBudget(budget)
	span := time.Duration(budget.WindowSeconds) * time.Second
	w, now := b.windowFor(rc.Fingerprint)

	w.mu.Lock()
	defer w.mu.Unlock()
	w.prune(now.Add(-span))

	if len(w.calls) >= budget.MaxCalls {
		return fmt.Errorf("throttled: enrolment %q has used %d of %d calls in the last %ds",
			rc.ClientID, len(w.calls), budget.MaxCalls, budget.WindowSeconds)
	}
	// >= rather than >: the budget is "at most MaxResultBytes per window",
	// so once the window holds that many the allowance is spent. Fail
	// closed on the boundary rather than granting one more call at exactly
	// the cap.
	if w.bytes >= budget.MaxResultBytes {
		return fmt.Errorf("throttled: enrolment %q has drawn %d of %d result bytes in the last %ds",
			rc.ClientID, w.bytes, budget.MaxResultBytes, budget.WindowSeconds)
	}
	w.calls = append(w.calls, now)
	return nil
}

// charge records the size of a completed result against the volume
// budget. This runs after the call, and that is a real limit, not an
// oversight: result size is not knowable before the MCP answers, so a call
// that pushes the total over the cap completes and returns its bytes — the
// NEXT call is the one refused. The guarantee is therefore "at most one
// call's worth of overshoot", not "never more than MaxResultBytes leaves
// the host".
//
// n is the same quantity the audit layer records as ResultBytes,
// deliberately reused rather than a second measurement that could
// disagree with the log.
func (b *enrolmentBudgets) charge(rc bridge.RemoteCaller, budget EnrolmentBudget, n int) {
	if n <= 0 {
		return
	}
	budget = normalizeEnrolmentBudget(budget)
	span := time.Duration(budget.WindowSeconds) * time.Second
	w, now := b.windowFor(rc.Fingerprint)

	w.mu.Lock()
	defer w.mu.Unlock()
	w.prune(now.Add(-span))
	w.volume = append(w.volume, volumeSample{at: now, bytes: int64(n)})
	w.bytes += int64(n)
}

// windowFor creates the enrolment's window on first use. Windows are never
// reclaimed, and nothing here sweeps them: the key space is the set of
// enrolled certificate fingerprints, so no unauthenticated caller can mint
// keys here, and each window self-prunes. Reclaiming an idle window would
// save a few hundred bytes per enrolment ever created and would open a
// race where a sweep drops the window a long-running call is about to
// charge its bytes to.
func (b *enrolmentBudgets) windowFor(fingerprint string) (*budgetWindow, time.Time) {
	b.mu.RLock()
	w, clock := b.windows[fingerprint], b.clock
	b.mu.RUnlock()

	now := time.Now()
	if clock != nil {
		now = clock()
	}
	if w != nil {
		return w, now
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	// Re-check: two connections from one enrolment can arrive here
	// together, and they must end up on the same ledger or the cap is
	// per-goroutine.
	if w := b.windows[fingerprint]; w != nil {
		return w, now
	}
	if b.windows == nil {
		b.windows = make(map[string]*budgetWindow)
	}
	w = &budgetWindow{}
	b.windows[fingerprint] = w
	return w, now
}

func (b *enrolmentBudgets) setClock(fn func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clock = fn
}

func (w *budgetWindow) prune(cutoff time.Time) {
	i := 0
	for i < len(w.calls) && !w.calls[i].After(cutoff) {
		i++
	}
	if i > 0 {
		w.calls = slices.Delete(w.calls, 0, i)
	}
	j := 0
	for j < len(w.volume) && !w.volume[j].at.After(cutoff) {
		w.bytes -= w.volume[j].bytes
		j++
	}
	if j > 0 {
		w.volume = slices.Delete(w.volume, 0, j)
	}
}
