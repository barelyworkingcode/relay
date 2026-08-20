package main

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"relaygo/bridge"
)

// Per-enrolment rate and volume budgets (ADR-010 decision 7).
//
// ADR-008 bought detection; nothing bought interdiction. A remote grant of
// mail_search + mail_get_emails can drain a mailbox as fast as Mail.app
// answers, fully logged and wholly unimpeded — the audit log a faithful record
// of the exact threat ADR-009 names. This file is the interdiction: the only
// thing in the design that makes an exfiltration attempt *stop* rather than be
// written down.
//
// Both limits are enforced in appRouter.CallTool, the chokepoint ADR-008 chose
// and for the same reason: every transport already funnels through it and the
// actor is known by the time it runs.
//
// Rate and volume are budgeted together because they fail differently. A call
// cap alone does not stop a slow drain, and a mailbox exfiltrated over six
// hours is exfiltrated. A byte cap alone does not stop a client hammering a
// cheap tool.
//
// THE ENROLMENT IS THE UNIT, NOT THE PROJECT. The enrolment is the unit of
// compromise — a stolen key is one enrolment's key and nothing else — so it is
// the unit that carries the cap. Two agents sharing a grant have independent
// budgets, and a compromised one cannot consume its neighbour's. A project
// ceiling is a coherent later addition (it protects the resource rather than
// bounding a single compromise) and is deliberately not this.

// enrolmentBudgets holds the rolling-window accounting for every enrolment
// that has called recently.
//
// The zero value is ready to use, and that is deliberate: it makes enforcement
// the default rather than something a constructor has to remember to switch
// on. An appRouter assembled by hand — in a test, in a future call site — still
// budgets its remote callers, so the failure mode of forgetting is a bounded
// caller rather than an unlimited one.
type enrolmentBudgets struct {
	// mu guards the map and the clock only. All the actual accounting happens
	// under the per-enrolment lock inside budgetWindow, so two enrolments
	// calling concurrently contend for nothing more than a map read: one noisy
	// client must not be able to serialise the others behind it, which would
	// be a denial of service built out of the thing meant to prevent one.
	mu      sync.RWMutex
	windows map[string]*budgetWindow

	// clock is injected by tests so window rollover is deterministic. Nil
	// means time.Now. A budget whose expiry can only be observed by sleeping
	// is a budget that gets tested with a sleep, and a sleeping test is a
	// flaky test.
	clock func() time.Time
}

// budgetWindow is one enrolment's rolling window.
//
// Both series are kept as timestamped entries rather than as counters reset on
// a clock boundary. A fixed bucket hands an attacker 2x the budget for free by
// straddling the reset — spend it all just before the boundary, spend it all
// again just after — which is exactly the burst the cap exists to refuse.
type budgetWindow struct {
	mu sync.Mutex
	// calls holds the admission time of every call still inside the window.
	// Bounded by MaxCalls: a call that would exceed the cap is refused and
	// never appended, so this cannot grow past the budget it enforces.
	calls []time.Time
	// volume holds the completion time and size of every result still inside
	// the window, and bytes is their running sum. Bounded by MaxCalls for the
	// same reason.
	volume []volumeSample
	bytes  int64
}

type volumeSample struct {
	at    time.Time
	bytes int64
}

// enrolmentBudget resolves the budget governing one remote caller.
//
// Lookup is by the certificate fingerprint the connection attested, not by the
// client id it names: the fingerprint is the identity that was proven, and a
// re-issued certificate under a recycled client id is a different key that
// deserves a fresh ledger rather than an inherited one.
//
// A fingerprint that resolves to no enrolment gets the conservative defaults,
// never an exemption. Decision 2's listener closes a connection whose
// certificate does not resolve, so this should be unreachable — which is
// precisely why it must not read as "unlimited" if it ever is reached. Refusing
// the call outright would be the wrong shape here: whether a caller holds a
// grant at all is an authorization question this budget check has no business
// answering, and mislabelling it would put `throttled` in the log where
// `denied` belongs.
func (s *Settings) enrolmentBudget(rc bridge.RemoteCaller) EnrolmentBudget {
	if s != nil {
		if e := s.FindEnrolmentByFingerprint(rc.Fingerprint); e != nil {
			// normalize again on read rather than trusting the stored record:
			// settings.json is hand-editable, and a zero field there must fill
			// with the default rather than switch the budget off.
			return normalizeEnrolmentBudget(e.Budget)
		}
	}
	return normalizeEnrolmentBudget(EnrolmentBudget{})
}

// admit reserves one call against the enrolment's budget, or returns the error
// that refuses it. Called BEFORE the MCP is invoked: a rate refusal that let
// the tool run first would have already read the mailbox it was refusing to
// let anyone read.
//
// Both limits are checked here, but only the rate limit is bounded in advance.
// The volume check is necessarily retrospective — see charge.
//
// A reserved call is never handed back, not even when the call is refused
// downstream (a failed intent record, say). Releasing it would let a caller
// retry a failing call without limit, which is the one shape of traffic a rate
// cap most obviously has to bound; counting an attempt that relay itself
// refused costs a legitimate client one slot out of a window.
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
	// >= rather than >: the budget is "at most MaxResultBytes per window", so
	// once the window holds that many the allowance is spent. Fail closed on
	// the boundary rather than granting one more call at exactly the cap.
	if w.bytes >= budget.MaxResultBytes {
		return fmt.Errorf("throttled: enrolment %q has drawn %d of %d result bytes in the last %ds",
			rc.ClientID, w.bytes, budget.MaxResultBytes, budget.WindowSeconds)
	}
	w.calls = append(w.calls, now)
	return nil
}

// charge records the size of a completed result against the volume budget.
//
// THIS RUNS AFTER THE CALL, AND THAT IS A REAL LIMIT, NOT AN OVERSIGHT. Result
// size is not knowable before the MCP answers, so a call that pushes the total
// over the cap completes and returns its bytes; the NEXT call is the one
// refused. The guarantee is therefore "at most one call's worth of overshoot",
// not "never more than MaxResultBytes leaves the host". Anything stronger would
// need a per-call size ceiling or a streaming cutoff, which is a different
// mechanism from a rolling budget and is not what decision 7 specifies.
//
// n is the same quantity the audit layer records as ResultBytes — the length of
// the raw MCP result — deliberately reusing that notion rather than inventing a
// second measurement that could disagree with the log.
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

// windowFor returns the enrolment's window and the current time, creating the
// window on first use.
//
// Windows are never reclaimed, and nothing here sweeps them. The key space is
// the set of enrolled certificate fingerprints — decision 2's listener closes
// any connection whose certificate does not resolve to an enrolment, so no
// unauthenticated caller can mint keys here — and each window self-prunes to at
// most MaxCalls entries. Reclaiming an idle window would save a few hundred
// bytes per enrolment ever created and would open a race where a sweep drops
// the window a long-running call is about to charge its bytes to, which is
// exactly the accounting a slow drain would like to see lost.
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
	// Re-check: two connections from one enrolment can arrive here together,
	// and they must end up on the same ledger or the cap is per-goroutine.
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

// setClock installs a clock for deterministic tests. Not used in production,
// where clock stays nil and time.Now is read directly.
func (b *enrolmentBudgets) setClock(fn func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clock = fn
}

// prune drops every entry that has aged out of the window. Entries are
// appended in non-decreasing time order, so expiry is always a prefix.
//
// An entry exactly cutoff-old is dropped: with cutoff = now-window, a call made
// exactly one window ago no longer counts, which is what "per rolling window"
// means and what makes the boundary deterministic to test.
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
