package enrolment

// Unit tests for the mount-plane budget ledger (AdmitMountOp/Read/Write),
// added alongside the existing tool-plane ledger tests in
// cmd/relay/enrolment_budget_test.go (which exercise Admit/Charge/BudgetFor
// end to end through appRouter.CallTool and are left untouched here).
//
// These tests talk to *Budgets directly rather than through a router, since
// there is no router-level caller of the mount methods yet (that's P4/P5's
// job) — Budgets is the whole unit under test. Being in package enrolment
// (white-box), a couple of assertions peek at budgetWindow's own fields
// rather than adding a new DrawnBytes-shaped production seam for the mount
// series; that's reading an unexported field from a test in the same
// package, not a new exported test hook.

import (
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
)

// mountCaller builds a RemoteCaller identity directly — Admit/AdmitMountOp/
// AdmitMountRead/AdmitMountWrite all take one as a plain parameter, unlike
// the router-level tests which thread it through context.
func mountCaller(clientID string) bridge.RemoteCaller {
	return bridge.RemoteCaller{
		ClientID:    clientID,
		Fingerprint: "sha256:" + clientID, // any distinct string; this package never validates its shape
		RemoteAddr:  "127.0.0.1:52233",
	}
}

// peekWindow returns the *budgetWindow backing rc's fingerprint, creating it
// if absent (mirroring what any Admit/Charge call would do), locked for the
// caller to read fields directly.
func peekWindow(b *Budgets, rc bridge.RemoteCaller) (*budgetWindow, func()) {
	w, _ := b.windowFor(rc.Fingerprint)
	w.mu.Lock()
	return w, w.mu.Unlock
}

func wantThrottled(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: succeeded, want throttled", what)
	}
	if !strings.HasPrefix(err.Error(), `throttled: enrolment "`) {
		t.Errorf("%s: error %q does not start with the required prefix", what, err.Error())
	}
}

// ---------------------------------------------------------------------------
// AdmitMountOp
// ---------------------------------------------------------------------------

func TestAdmitMountOp_AdmitsUpToCapThenThrottles(t *testing.T) {
	var b Budgets
	rc := mountCaller("mount-ops")
	budget := config.EnrolmentBudget{WindowSeconds: 60, MountMaxOps: 3}

	for i := 1; i <= 3; i++ {
		if err := b.AdmitMountOp(rc, budget); err != nil {
			t.Fatalf("op %d within the cap was refused: %v", i, err)
		}
	}
	err := b.AdmitMountOp(rc, budget)
	wantThrottled(t, err, "op over the cap")
}

func TestAdmitMountOp_DoesNotTouchToolPlaneCallsSeries(t *testing.T) {
	var b Budgets
	rc := mountCaller("mount-ops-isolation")
	budget := config.EnrolmentBudget{WindowSeconds: 60, MountMaxOps: 5, MaxCalls: 1, MaxResultBytes: 1 << 30}

	for i := 0; i < 5; i++ {
		if err := b.AdmitMountOp(rc, budget); err != nil {
			t.Fatalf("mount op %d: %v", i, err)
		}
	}
	// The tool-plane call cap (1) is untouched by five mount ops: the first
	// tool-plane Admit must still succeed.
	if err := b.Admit(rc, budget); err != nil {
		t.Fatalf("tool-plane Admit was starved by mount ops: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AdmitMountRead — the sharpest edge case: exact boundary
// ---------------------------------------------------------------------------

func TestAdmitMountRead_ExactRemainingAllowanceAdmitted_NextByteRefused(t *testing.T) {
	var b Budgets
	rc := mountCaller("mount-read-boundary")
	budget := config.EnrolmentBudget{WindowSeconds: 60, MountMaxReadBytes: 100}

	if err := b.AdmitMountRead(rc, budget, 60); err != nil {
		t.Fatalf("first read (60 of 100) was refused: %v", err)
	}
	// Remaining allowance is exactly 40; a read of exactly 40 must succeed.
	if err := b.AdmitMountRead(rc, budget, 40); err != nil {
		t.Fatalf("second read of exactly the remaining allowance (40) was refused: %v", err)
	}
	// The allowance is now fully spent; even n=1 must be refused.
	err := b.AdmitMountRead(rc, budget, 1)
	wantThrottled(t, err, "read of 1 byte after the allowance was exactly exhausted")
}

func TestAdmitMountRead_OneByteOverRefusesWithoutPartialCharge(t *testing.T) {
	var b Budgets
	rc := mountCaller("mount-read-overshoot")
	budget := config.EnrolmentBudget{WindowSeconds: 60, MountMaxReadBytes: 100}

	err := b.AdmitMountRead(rc, budget, 101)
	wantThrottled(t, err, "a single read one byte over the cap")

	// Refused entirely, not partially charged: the full 100 must still be
	// available afterwards.
	if err := b.AdmitMountRead(rc, budget, 100); err != nil {
		t.Fatalf("full allowance unavailable after a refused overshoot: %v", err)
	}
}

func TestAdmitMountRead_NonPositiveIsNoOp(t *testing.T) {
	var b Budgets
	rc := mountCaller("mount-read-noop")
	budget := config.EnrolmentBudget{WindowSeconds: 60, MountMaxReadBytes: 10}

	if err := b.AdmitMountRead(rc, budget, 0); err != nil {
		t.Fatalf("n=0 was refused: %v", err)
	}
	if err := b.AdmitMountRead(rc, budget, -5); err != nil {
		t.Fatalf("n<0 was refused: %v", err)
	}
	// Nothing charged: the full 10-byte allowance is still there.
	if err := b.AdmitMountRead(rc, budget, 10); err != nil {
		t.Fatalf("allowance was consumed by a non-positive n: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AdmitMountWrite — the write-side counterpart, same shape
// ---------------------------------------------------------------------------

func TestAdmitMountWrite_ExactRemainingAllowanceAdmitted_NextByteRefused(t *testing.T) {
	var b Budgets
	rc := mountCaller("mount-write-boundary")
	budget := config.EnrolmentBudget{WindowSeconds: 60, MountMaxWriteBytes: 50}

	if err := b.AdmitMountWrite(rc, budget, 30); err != nil {
		t.Fatalf("first write (30 of 50) was refused: %v", err)
	}
	if err := b.AdmitMountWrite(rc, budget, 20); err != nil {
		t.Fatalf("second write of exactly the remaining allowance (20) was refused: %v", err)
	}
	err := b.AdmitMountWrite(rc, budget, 1)
	wantThrottled(t, err, "write of 1 byte after the allowance was exactly exhausted")
}

func TestAdmitMountWrite_NonPositiveIsNoOp(t *testing.T) {
	var b Budgets
	rc := mountCaller("mount-write-noop")
	budget := config.EnrolmentBudget{WindowSeconds: 60, MountMaxWriteBytes: 10}

	if err := b.AdmitMountWrite(rc, budget, 0); err != nil {
		t.Fatalf("n=0 was refused: %v", err)
	}
	if err := b.AdmitMountWrite(rc, budget, -1); err != nil {
		t.Fatalf("n<0 was refused: %v", err)
	}
	if err := b.AdmitMountWrite(rc, budget, 10); err != nil {
		t.Fatalf("allowance was consumed by a non-positive n: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Independence: mount read/write/ops and the tool-plane calls/bytes series
// are tracked separately on the same *Budgets/fingerprint.
// ---------------------------------------------------------------------------

func TestMountSeries_IndependentFromToolPlaneSeries(t *testing.T) {
	var b Budgets
	rc := mountCaller("series-independence")
	budget := config.EnrolmentBudget{
		WindowSeconds:      3600,
		MaxCalls:           100,
		MaxResultBytes:     1000,
		MountMaxOps:        100,
		MountMaxReadBytes:  1000,
		MountMaxWriteBytes: 1000,
	}

	// Spend the entire tool-plane volume budget.
	b.Charge(rc, budget, 1000)
	// Mount reads and writes must be completely unaffected by that charge.
	if err := b.AdmitMountRead(rc, budget, 1000); err != nil {
		t.Fatalf("mount read starved by a tool-plane Charge: %v", err)
	}
	if err := b.AdmitMountWrite(rc, budget, 1000); err != nil {
		t.Fatalf("mount write starved by a tool-plane Charge: %v", err)
	}

	// And the reverse: mount reads/writes must never touch the tool-plane
	// DrawnBytes total, which the router's own tests read via this seam.
	if got := b.DrawnBytes(rc.Fingerprint); got != 1000 {
		t.Errorf("tool-plane drawn bytes = %d, want exactly the 1000 charged via Charge (mount reads/writes must not add to it)", got)
	}

	// A fresh caller proves the same thing from the other direction: mount
	// traffic alone must not touch the tool-plane series at all.
	rc2 := mountCaller("series-independence-2")
	if err := b.AdmitMountRead(rc2, budget, 500); err != nil {
		t.Fatalf("mount read: %v", err)
	}
	if err := b.AdmitMountWrite(rc2, budget, 500); err != nil {
		t.Fatalf("mount write: %v", err)
	}
	for i := 0; i < 100; i++ {
		if err := b.AdmitMountOp(rc2, budget); err != nil {
			t.Fatalf("mount op %d: %v", i, err)
		}
	}
	if got := b.DrawnBytes(rc2.Fingerprint); got != 0 {
		t.Errorf("tool-plane drawn bytes = %d after only mount traffic, want 0", got)
	}
	// A tool-plane call must still be fully admissible: mount ops must not
	// have touched the calls series either.
	if err := b.Admit(rc2, budget); err != nil {
		t.Fatalf("tool-plane Admit starved by mount ops/reads/writes: %v", err)
	}

	// White-box check that the mount series themselves hold exactly what
	// was charged to them, on both callers, independently of each other and
	// of the tool-plane fields.
	w1, unlock1 := peekWindow(&b, rc)
	mountRead1, mountWrite1 := w1.mountReadBytes, w1.mountWriteBytes
	unlock1()
	if mountRead1 != 1000 || mountWrite1 != 1000 {
		t.Errorf("rc mount series = read %d write %d, want 1000/1000", mountRead1, mountWrite1)
	}

	w2, unlock2 := peekWindow(&b, rc2)
	mountRead2, mountWrite2, mountOps2, toolBytes2 := w2.mountReadBytes, w2.mountWriteBytes, len(w2.mountOps), w2.bytes
	unlock2()
	if mountRead2 != 500 || mountWrite2 != 500 || mountOps2 != 100 || toolBytes2 != 0 {
		t.Errorf("rc2 series = read %d write %d ops %d toolBytes %d, want 500/500/100/0", mountRead2, mountWrite2, mountOps2, toolBytes2)
	}
}

// ---------------------------------------------------------------------------
// Pruning: the mount series age out of the rolling window the same way the
// existing calls/volume series do, driven by the SetClock test seam.
// ---------------------------------------------------------------------------

func TestMountOps_AgeOutOfTheRollingWindow(t *testing.T) {
	var b Budgets
	clk := newFakeMountClock()
	b.SetClock(clk.now)
	rc := mountCaller("mount-ops-window")
	budget := config.EnrolmentBudget{WindowSeconds: 60, MountMaxOps: 2}

	if err := b.AdmitMountOp(rc, budget); err != nil {
		t.Fatalf("op 1: %v", err)
	}
	clk.advance(30 * time.Second)
	if err := b.AdmitMountOp(rc, budget); err != nil {
		t.Fatalf("op 2: %v", err)
	}
	if err := b.AdmitMountOp(rc, budget); err == nil {
		t.Fatal("op 3 inside a full window succeeded")
	}

	// Just short of the first op ageing out: still refused.
	clk.advance(29 * time.Second)
	if err := b.AdmitMountOp(rc, budget); err == nil {
		t.Fatal("allowance returned before the window actually rolled")
	}

	// t+60 from op 1: it has aged out, exactly one slot is free.
	clk.advance(time.Second)
	if err := b.AdmitMountOp(rc, budget); err != nil {
		t.Fatalf("op after the window rolled was refused: %v", err)
	}
	if err := b.AdmitMountOp(rc, budget); err == nil {
		t.Fatal("window reset as a fixed bucket: more than one slot came back at once")
	}
}

func TestMountReadWriteBytes_AgeOutOfTheRollingWindow(t *testing.T) {
	var b Budgets
	clk := newFakeMountClock()
	b.SetClock(clk.now)
	rc := mountCaller("mount-bytes-window")
	budget := config.EnrolmentBudget{WindowSeconds: 60, MountMaxReadBytes: 100, MountMaxWriteBytes: 100}

	if err := b.AdmitMountRead(rc, budget, 100); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if err := b.AdmitMountWrite(rc, budget, 100); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := b.AdmitMountRead(rc, budget, 1); err == nil {
		t.Fatal("read ran with the read allowance already fully spent")
	}
	if err := b.AdmitMountWrite(rc, budget, 1); err == nil {
		t.Fatal("write ran with the write allowance already fully spent")
	}

	clk.advance(60 * time.Second)
	if err := b.AdmitMountRead(rc, budget, 100); err != nil {
		t.Fatalf("read did not age out of the window: %v", err)
	}
	if err := b.AdmitMountWrite(rc, budget, 100); err != nil {
		t.Fatalf("write did not age out of the window: %v", err)
	}
}

// fakeMountClock is a minimal injectable clock, local to this file so these
// tests do not depend on cmd/relay's fakeClock (a different package).
type fakeMountClock struct {
	t time.Time
}

func newFakeMountClock() *fakeMountClock {
	return &fakeMountClock{t: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeMountClock) now() time.Time { return c.t }

func (c *fakeMountClock) advance(d time.Duration) { c.t = c.t.Add(d) }
