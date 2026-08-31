package main

// pendingEnrolmentNotifier is the only surface a network stranger can drive:
// lodging a CSR to the enrolment-request listener moves `gen`. Every test
// here is really a test of the bound on THAT, so the counting has to be
// exact rather than "roughly right" — a notifier that fires twice as often
// as it should is a banner an attacker can spam, and one that stops firing
// after the cap trips is a flood relay never surfaces at all.
//
// Uses the fakeClock already defined in enrolment_budget_test.go rather than
// a second clock type in this package.

import (
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type notifyCall struct {
	title, body string
}

// newTestNotifier wires a notifier to a fakeClock and a recording notify
// func, and returns a *[]notifyCall the test can inspect after driving tick.
func newTestNotifier() (*pendingEnrolmentNotifier, *fakeClock, *[]notifyCall) {
	clk := newFakeClock()
	calls := &[]notifyCall{}
	n := newPendingEnrolmentNotifier(clk.now, func(title, body string) {
		*calls = append(*calls, notifyCall{title, body})
	})
	return n, clk, calls
}

const (
	notifierLive    = 3
	notifierMaxLive = 10
)

// ---------------------------------------------------------------------------
// Constructor defaults
// ---------------------------------------------------------------------------

func TestNewPendingEnrolmentNotifier_NilDefaults(t *testing.T) {
	n := newPendingEnrolmentNotifier(nil, nil)
	if n.now == nil {
		t.Fatal("now must default to a real clock (time.Now), not stay nil")
	}
	if n.notify == nil {
		t.Fatal("notify must default to a no-op, not stay nil")
	}

	before := time.Now()
	got := n.now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("default clock did not read time.Now(): got %v, want between %v and %v", got, before, after)
	}

	// Must not panic — this is the substituted no-op notify, driven through
	// a real tick with every gate satisfied.
	n.tick(1, 1, notifierLive, notifierMaxLive, false)
}

// ---------------------------------------------------------------------------
// Rule 1: gen must rise
// ---------------------------------------------------------------------------

func TestTick_FirstCallWithZeroLastAtNotifies(t *testing.T) {
	n, _, calls := newTestNotifier()
	n.tick(1, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != 1 {
		t.Fatalf("first call = %d notifications, want 1 (lastAt's zero value must count as due)", len(*calls))
	}
}

func TestTick_UnchangedGenNeverNotifies(t *testing.T) {
	n, clk, calls := newTestNotifier()
	// One legitimate rise establishes a baseline lastGen != 0.
	n.tick(5, 1, notifierLive, notifierMaxLive, false)
	*calls = nil

	for i := 0; i < 200; i++ {
		clk.advance(time.Minute) // plenty past every interval and window bound
		n.tick(5, 1, notifierLive, notifierMaxLive, false)
	}
	if len(*calls) != 0 {
		t.Errorf("gen held at 5 across 200 calls produced %d notifications, want 0", len(*calls))
	}
}

func TestTick_LowerGenNeverNotifies(t *testing.T) {
	n, clk, calls := newTestNotifier()
	n.tick(10, 1, notifierLive, notifierMaxLive, false)
	*calls = nil
	clk.advance(time.Hour)
	// A lower gen than what's already been seen — e.g. gen wrapped or a
	// stale read — must stay silent, not just an equal one.
	n.tick(3, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != 0 {
		t.Errorf("gen(3) <= lastGen(10) notified anyway")
	}
}

// ---------------------------------------------------------------------------
// Rules 2-4: unapproved, live<maxLive, !settingsOpen — each silences the
// notification but must NOT stop lastGen from advancing (requirement 4
// applies to every one of these gates, not only the rate limiter).
// ---------------------------------------------------------------------------

func TestTick_ZeroUnapprovedIsSilentButGenAdvances(t *testing.T) {
	n, clk, calls := newTestNotifier()
	n.tick(1, 0, notifierLive, notifierMaxLive, false)
	if len(*calls) != 0 {
		t.Fatalf("unapproved=0 notified anyway")
	}
	// Prove lastGen moved: a later call with unapproved>0 but the SAME gen
	// must still be silent, and one with a higher gen must fire.
	clk.advance(time.Minute)
	n.tick(1, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != 0 {
		t.Fatalf("gen(1) fired again after lastGen should already have reached 1")
	}
	clk.advance(time.Minute)
	n.tick(2, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != 1 {
		t.Fatalf("a genuinely new gen after a silent unapproved=0 call produced %d notifications, want 1", len(*calls))
	}
}

func TestTick_FullTableIsSilentButGenAdvances(t *testing.T) {
	n, clk, calls := newTestNotifier()
	n.tick(1, 1, notifierMaxLive, notifierMaxLive, false) // live == maxLive: full
	if len(*calls) != 0 {
		t.Fatalf("live == maxLive notified anyway")
	}
	clk.advance(time.Minute)
	n.tick(1, 1, notifierLive, notifierMaxLive, false) // same gen, table has room now
	if len(*calls) != 0 {
		t.Fatalf("gen(1) fired again after lastGen should already have reached 1 from the full-table call")
	}
}

func TestTick_SettingsOpenIsSilentButGenAdvances(t *testing.T) {
	n, clk, calls := newTestNotifier()
	n.tick(1, 1, notifierLive, notifierMaxLive, true) // settings open
	if len(*calls) != 0 {
		t.Fatalf("settingsOpen=true notified anyway")
	}
	clk.advance(time.Minute)
	n.tick(1, 1, notifierLive, notifierMaxLive, false) // same gen, settings now closed
	if len(*calls) != 0 {
		t.Fatalf("gen(1) fired again after lastGen should already have reached 1 while settings was open")
	}
}

// maxLive <= 0 must read as "not full", never divide or panic.
func TestTick_MaxLiveZeroOrNegativeTreatsTableAsNotFull(t *testing.T) {
	for _, maxLive := range []int{0, -1, -100} {
		n, _, calls := newTestNotifier()
		n.tick(1, 1, 5, maxLive, false)
		if len(*calls) != 1 {
			t.Errorf("maxLive=%d: got %d notifications, want 1 (a non-positive cap must not read as full)", maxLive, len(*calls))
		}
	}
}

// ---------------------------------------------------------------------------
// Rule 5/6: coalescing — the counting behaviours that are the whole point
// ---------------------------------------------------------------------------

// 200 rising-gen ticks inside one simulated minute -> exactly one notification.
func TestTick_200RisingGenTicksInOneMinuteProduceExactlyOneNotification(t *testing.T) {
	n, clk, calls := newTestNotifier()
	step := time.Minute / 200
	for i := uint64(1); i <= 200; i++ {
		n.tick(i, 1, notifierLive, notifierMaxLive, false)
		clk.advance(step)
	}
	if len(*calls) != 1 {
		t.Fatalf("200 rising-gen ticks inside one minute produced %d notifications, want exactly 1", len(*calls))
	}
}

// An hour of rising-gen inserts produces at most six notifications — the
// hourly cap holds even though every individual tick clears the 60s interval.
func TestTick_AnHourOfInsertsProducesAtMostSix(t *testing.T) {
	n, clk, calls := newTestNotifier()
	gen := uint64(0)
	for elapsed := time.Duration(0); elapsed < time.Hour; elapsed += time.Second {
		gen++
		n.tick(gen, 1, notifierLive, notifierMaxLive, false)
		clk.advance(time.Second)
	}
	if len(*calls) > notifyMaxPerHour {
		t.Fatalf("an hour of rising-gen inserts produced %d notifications, want at most %d", len(*calls), notifyMaxPerHour)
	}
	if len(*calls) == 0 {
		t.Fatalf("an hour of rising-gen inserts produced zero notifications, want at least 1")
	}
}

// The interval alone (60s spacing, well under the hourly cap) must produce
// one notification per interval, not one total — pins the cap and the
// interval as two independent gates rather than the interval silently being
// the only one that matters.
func TestTick_SpacedPastIntervalNotifiesEachTime(t *testing.T) {
	n, clk, calls := newTestNotifier()
	gen := uint64(0)
	const rounds = 4 // stays under notifyMaxPerHour
	for i := 0; i < rounds; i++ {
		gen++
		n.tick(gen, 1, notifierLive, notifierMaxLive, false)
		clk.advance(notifyMinInterval + time.Second)
	}
	if len(*calls) != rounds {
		t.Fatalf("got %d notifications over %d intervals spaced past notifyMinInterval, want %d", len(*calls), rounds, rounds)
	}
}

// A call inside the interval is silent even with a fresh gen; the same gen
// retried once the interval has passed must NOT re-fire (lastGen already
// caught up), proving the interval and the gen check are both honoured.
func TestTick_WithinIntervalIsSilent(t *testing.T) {
	n, clk, calls := newTestNotifier()
	n.tick(1, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != 1 {
		t.Fatalf("first call = %d notifications, want 1", len(*calls))
	}
	clk.advance(notifyMinInterval - time.Second) // just short of the interval
	n.tick(2, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != 1 {
		t.Fatalf("a call inside notifyMinInterval notified anyway (%d total)", len(*calls))
	}
}

// The window prune happens before the cap check, so an hour after a flood
// the window is empty and a fresh notification is due again — a stale
// six-entry window from an hour ago must not block it.
func TestTick_WindowPruneFreesCapacityAfterAnHour(t *testing.T) {
	n, clk, calls := newTestNotifier()
	gen := uint64(0)
	// Trip the hourly cap.
	for i := 0; i < notifyMaxPerHour; i++ {
		gen++
		n.tick(gen, 1, notifierLive, notifierMaxLive, false)
		clk.advance(notifyMinInterval + time.Second)
	}
	if len(*calls) != notifyMaxPerHour {
		t.Fatalf("setup: got %d notifications, want the cap %d before proceeding", len(*calls), notifyMaxPerHour)
	}
	gen++
	n.tick(gen, 1, notifierLive, notifierMaxLive, false) // still capped
	if len(*calls) != notifyMaxPerHour {
		t.Fatalf("cap was not holding before the window aged out: got %d", len(*calls))
	}

	clk.advance(time.Hour) // every prior entry is now >= an hour old
	gen++
	n.tick(gen, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != notifyMaxPerHour+1 {
		t.Fatalf("after the window aged out, got %d notifications, want %d", len(*calls), notifyMaxPerHour+1)
	}
}

// ---------------------------------------------------------------------------
// Requirement 4 — the subtlest rule: lastGen advances even when suppressed,
// so a flood is swallowed rather than queued to fire the moment it can.
// ---------------------------------------------------------------------------

// Deliberate-break check: a wrong implementation that only assigns lastGen
// inside the "we are about to notify" branch would leave lastGen stuck at 1
// after the second, rate-limited tick. Once the clock clears the interval,
// tick(gen=2, ...) would then see gen(2) > lastGen(1) and fire a SECOND
// notification for a gen the caller never asked about again — the queued
// notification requirement 4 exists to forbid. This test drives exactly
// that sequence and asserts the count stays at 1.
func TestTick_SuppressedGenStillAdvancesLastGen_FloodIsSwallowedNotQueued(t *testing.T) {
	n, clk, calls := newTestNotifier()

	// t0: fires, establishes lastAt and lastGen=1.
	n.tick(1, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != 1 {
		t.Fatalf("setup: first tick produced %d notifications, want 1", len(*calls))
	}

	// Still t0 (no clock advance): a new gen arrives but the interval hasn't
	// passed, so this is suppressed by rule 5 alone — every other gate holds.
	n.tick(2, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != 1 {
		t.Fatalf("a rate-limited tick notified anyway: %d total", len(*calls))
	}

	// Advance well past notifyMinInterval with NO new gen.
	clk.advance(notifyMinInterval + time.Second)
	n.tick(2, 1, notifierLive, notifierMaxLive, false) // same gen as the suppressed call
	if len(*calls) != 1 {
		t.Fatalf("the suppressed gen fired once the interval passed: %d total, want 1 "+
			"(lastGen must have advanced to 2 on the suppressed call, not stayed at 1)", len(*calls))
	}
}

// Same property, tripped via the hourly cap instead of the interval: a flood
// that fills the window must not leave a queued notification behind either.
func TestTick_CapSuppressedGenStillAdvancesLastGen(t *testing.T) {
	n, clk, calls := newTestNotifier()
	gen := uint64(0)
	for i := 0; i < notifyMaxPerHour; i++ {
		gen++
		n.tick(gen, 1, notifierLive, notifierMaxLive, false)
		clk.advance(notifyMinInterval + time.Second)
	}
	if len(*calls) != notifyMaxPerHour {
		t.Fatalf("setup: got %d notifications, want the cap %d", len(*calls), notifyMaxPerHour)
	}

	// One more rise, refused by the cap (interval is clear, cap is not).
	gen++
	n.tick(gen, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != notifyMaxPerHour {
		t.Fatalf("cap did not hold: %d notifications", len(*calls))
	}

	// Advance the interval again with NO new gen. If lastGen had not moved
	// on the capped call, this retry of the SAME gen would look "new" and,
	// once the window ages out enough, fire — it must not.
	clk.advance(notifyMinInterval + time.Second)
	n.tick(gen, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != notifyMaxPerHour {
		t.Fatalf("a gen already seen (and capped) fired again: %d total, want %d", len(*calls), notifyMaxPerHour)
	}
}

// ---------------------------------------------------------------------------
// Notification content
// ---------------------------------------------------------------------------

func TestTick_NotificationTitleAndSingularBody(t *testing.T) {
	n, _, calls := newTestNotifier()
	n.tick(1, 1, notifierLive, notifierMaxLive, false)
	if len(*calls) != 1 {
		t.Fatalf("got %d notifications, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.title != "Relay" {
		t.Errorf("title = %q, want %q", got.title, "Relay")
	}
	wantBody := "1 machine is waiting to be registered.\n" +
		"Open Settings → Remote Clients to compare its code and approve. Nothing is granted until you do."
	if got.body != wantBody {
		t.Errorf("body = %q, want %q", got.body, wantBody)
	}
}

func TestTick_PluralBodySubstitutesTheCount(t *testing.T) {
	n, _, calls := newTestNotifier()
	n.tick(1, 3, notifierLive, notifierMaxLive, false)
	if len(*calls) != 1 {
		t.Fatalf("got %d notifications, want 1", len(*calls))
	}
	wantBody := fmt.Sprintf("%d machines are waiting to be registered.\n"+
		"Open Settings → Remote Clients to compare its code and approve. Nothing is granted until you do.", 3)
	if (*calls)[0].body != wantBody {
		t.Errorf("body = %q, want %q", (*calls)[0].body, wantBody)
	}
}

// tick must call notify at most once per call, even with a generous
// unapproved count that might tempt a "one notification per pending
// request" misreading of requirement 3.
func TestTick_AtMostOneNotifyCallPerTick(t *testing.T) {
	n, _, calls := newTestNotifier()
	n.tick(1, 50, notifierLive, notifierMaxLive, false)
	if len(*calls) != 1 {
		t.Errorf("got %d notify calls for one tick with unapproved=50, want exactly 1", len(*calls))
	}
}
