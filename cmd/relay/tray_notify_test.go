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
	"log/slog"
	"os"
	"strings"
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
// Rule 1: gen must rise
// ---------------------------------------------------------------------------

// A gen equal to or below the one already seen (a repaint with no new row, a
// wrapped counter, a stale read) stays silent however much time passes.
func TestTick_NonRisingGenNeverNotifies(t *testing.T) {
	for _, tc := range []struct {
		name          string
		baseline, gen uint64
		repeats       int
		advance       time.Duration
	}{
		{"unchanged", 5, 5, 200, time.Minute}, // plenty past every interval and window bound
		{"lower", 10, 3, 1, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, clk, calls := newTestNotifier()
			n.tick(tc.baseline, 1, notifierLive, notifierMaxLive, false)
			*calls = nil

			for i := 0; i < tc.repeats; i++ {
				clk.advance(tc.advance)
				n.tick(tc.gen, 1, notifierLive, notifierMaxLive, false)
			}
			if len(*calls) != 0 {
				t.Errorf("gen(%d) <= lastGen(%d) across %d calls produced %d notifications, want 0",
					tc.gen, tc.baseline, tc.repeats, len(*calls))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Rules 2-4: unapproved, live<maxLive, !settingsOpen — each silences the
// notification but must NOT stop lastGen from advancing (requirement 4
// applies to every one of these gates, not only the rate limiter).
// ---------------------------------------------------------------------------

func TestTick_SilencingGateStillAdvancesGen(t *testing.T) {
	for _, tc := range []struct {
		name         string
		unapproved   int
		live         int
		settingsOpen bool
	}{
		{"zero unapproved", 0, notifierLive, false},
		{"full table", 1, notifierMaxLive, false}, // live == maxLive
		{"settings open", 1, notifierLive, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, clk, calls := newTestNotifier()
			n.tick(1, tc.unapproved, tc.live, notifierMaxLive, tc.settingsOpen)
			if len(*calls) != 0 {
				t.Fatalf("%s notified anyway", tc.name)
			}
			// Prove lastGen moved: a later call with every gate open but the
			// SAME gen must still be silent, and one with a higher gen must fire.
			clk.advance(time.Minute)
			n.tick(1, 1, notifierLive, notifierMaxLive, false)
			if len(*calls) != 0 {
				t.Fatalf("gen(1) fired again after lastGen should already have reached 1 from the %s call", tc.name)
			}
			clk.advance(time.Minute)
			n.tick(2, 1, notifierLive, notifierMaxLive, false)
			if len(*calls) != 1 {
				t.Fatalf("a genuinely new gen after a silent %s call produced %d notifications, want 1", tc.name, len(*calls))
			}
		})
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

func TestTick_NotificationTitleAndBody(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unapproved int
		wantBody   string
	}{
		{"singular", 1, "1 machine is waiting to be registered.\n" +
			"Open Settings → Remote Clients to compare its code and approve. Nothing is granted until you do."},
		{"plural", 3, "3 machines are waiting to be registered.\n" +
			"Open Settings → Remote Clients to compare its code and approve. Nothing is granted until you do."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, _, calls := newTestNotifier()
			n.tick(1, tc.unapproved, notifierLive, notifierMaxLive, false)
			if len(*calls) != 1 {
				t.Fatalf("got %d notifications for one tick with unapproved=%d, want exactly 1", len(*calls), tc.unapproved)
			}
			got := (*calls)[0]
			if got.title != "Relay" {
				t.Errorf("title = %q, want %q", got.title, "Relay")
			}
			if got.body != tc.wantBody {
				t.Errorf("body = %q, want %q", got.body, tc.wantBody)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The denial report — the seam Objective-C uses to reach relay's own log
// ---------------------------------------------------------------------------

// macOS denies notification authorization outright for an ad-hoc-signed,
// non-notarised LSUIElement bundle: no prompt, no user action, and nothing
// to find afterwards except "Allow notifications: Off" in System Settings.
// reportNotificationsDenied is the whole of relay's answer to that, so what
// is pinned here is the two properties that make it worth having — it says
// enough to act on, and it says it once.

// slog's TextHandler quotes the message, escaping the two quoted phrases
// inside it, so the constant does not appear in the output verbatim. Its
// quote-free head does, and is what the counts below match on.
var denialWarningHead = notificationsDeniedWarning[:strings.IndexByte(notificationsDeniedWarning, '"')]

func denialLogCapture(t *testing.T) *lrSyncBuffer {
	t.Helper()
	notificationsDeniedReported.Store(false)
	t.Cleanup(func() { notificationsDeniedReported.Store(false) })

	logs := &lrSyncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

// The obligations of the message, not its wording: a rewrite may say this
// any way it likes as long as it still names the setting, says the count is
// not lost with it, and says whose setting it is.
func TestNotificationsDeniedWarning_SaysWhatHappenedAndWhatToDo(t *testing.T) {
	for _, want := range []struct{ what, substr string }{
		{"where the switch is", "System Settings"},
		{"which switch", "Notifications"},
		{"that the tray line still carries the count", "Pending enrolment requests: N"},
		{"the same table from a terminal", "relay enrol requests"},
		{"that this is a per-user setting", "per-user"},
	} {
		if !strings.Contains(notificationsDeniedWarning, want.substr) {
			t.Errorf("the denial warning does not say %s (no %q):\n%s",
				want.what, want.substr, notificationsDeniedWarning)
		}
	}
}

// A denial is a persistent state, not an event — macOS answers every later
// request the same way. One line is diagnosable; one per notification is a
// flood that would itself need suppressing.
func TestReportNotificationsDenied_WarnsOnceCarryingTheErrorDetail(t *testing.T) {
	logs := denialLogCapture(t)

	reportNotificationsDenied("Notifications are not allowed for this application")

	if got := strings.Count(logs.String(), denialWarningHead); got != 1 {
		t.Fatalf("the first denial produced %d warnings, want 1:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "Notifications are not allowed for this application") {
		t.Errorf("the warning dropped the error macOS gave back:\n%s", logs.String())
	}

	for i := 0; i < 50; i++ {
		reportNotificationsDenied("Notifications are not allowed for this application")
	}
	if got := strings.Count(logs.String(), denialWarningHead); got != 1 {
		t.Fatalf("51 denials produced %d warnings, want exactly 1", got)
	}
}

// The common case: macOS refuses with granted=NO and no NSError at all. The
// line must still be emitted, and must not carry an empty error= key that
// reads as "something went wrong and relay does not know what".
func TestReportNotificationsDenied_WarnsWithNoErrorAttached(t *testing.T) {
	logs := denialLogCapture(t)

	reportNotificationsDenied("")

	if got := strings.Count(logs.String(), denialWarningHead); got != 1 {
		t.Fatalf("a denial with no error produced %d warnings, want 1:\n%s", got, logs.String())
	}
	if strings.Contains(logs.String(), "error=") {
		t.Errorf("a denial with no error still logged an error key:\n%s", logs.String())
	}
}

// ---------------------------------------------------------------------------
// The no-bundle-identifier guard
// ---------------------------------------------------------------------------

// UNUserNotificationCenter.currentNotificationCenter THROWS for a process
// with no bundle identifier, which is every `go test` binary and every bare
// ./relay. Notify must stay a no-op there rather than crash, and must stay a
// SILENT one: this suite would otherwise warn about a denial on every run of
// a path that was never denied anything.
func TestDarwinPlatformNotify_WithoutABundleIdentifierIsASilentNoOp(t *testing.T) {
	if id := os.Getenv("__CFBundleIdentifier"); id != "" {
		t.Skipf("this binary is running inside a bundle (%s); the guard under test is the no-bundle case", id)
	}
	logs := denialLogCapture(t)

	p := NewPlatform()
	for i := 0; i < 3; i++ {
		p.Notify("Relay", "1 machine is waiting to be registered.")
	}

	if got := logs.String(); got != "" {
		t.Fatalf("Notify without a bundle identifier logged:\n%s", got)
	}
	if notificationsDeniedReported.Load() {
		t.Fatal("Notify without a bundle identifier reported a denial; nothing was ever asked")
	}
}
