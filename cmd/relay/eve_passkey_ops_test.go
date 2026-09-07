package main

// Unit coverage for EvePasskeyOps (docs/eve-passkey-enrolment.md's second
// half, decisions 8-13): Report's replace-and-stamp behaviour and its
// revocation-dropping rules, Revoke's three pre-gate refusals and its
// atomicity under a concurrent burst, List/Revocations projections, and
// Unrevoke. The presence-gate wiring itself (nil Gate refuses, an allowing
// gate succeeds, issuance auditing off refuses before the prompt) is proven
// once, generically, by presence_gate_wiring_test.go's pgwCases entry for
// "eve.passkey.revoke" -- this file does not repeat that.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func TestEvePasskeyOps_ReportReplacesWholesaleAndStampsReported(t *testing.T) {
	store := eveOpsStore(t)
	ops := &EvePasskeyOps{Store: store}

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "stale", Label: "stale-browser", Reported: "2020-01-01T00:00:00Z"}}
	}), "seed a stale mirror")

	err := ops.Report([]evePasskeyReportEntry{
		{ID: "fresh-1", Label: "iPhone", Created: "2026-09-07T10:00:00Z", LastUsed: "2026-09-07T11:00:00Z"},
	})
	assertNoErr(t, err, "Report")

	got := store.Get().EvePasskeys
	if len(got) != 1 || got[0].ID != "fresh-1" {
		t.Fatalf("Report did not replace wholesale: %+v", got)
	}
	if got[0].Reported == "" {
		t.Fatalf("Report did not stamp Reported: %+v", got[0])
	}
	if got[0].Label != "iPhone" || got[0].Created != "2026-09-07T10:00:00Z" || got[0].LastUsed != "2026-09-07T11:00:00Z" {
		t.Fatalf("Report did not carry every reported field through: %+v", got[0])
	}
}

// Decision 12: the report IS the acknowledgement. A pending revocation for
// an id no longer in the reported list is dropped -- eve applied it.
func TestEvePasskeyOps_ReportDropsRevocationsAbsentFromTheList(t *testing.T) {
	store := eveOpsStore(t)
	ops := &EvePasskeyOps{Store: store}

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "a"}, {ID: "b"}}
		s.EvePasskeyRevocations = []config.EvePasskeyRevocation{{ID: "a", Requested: "2026-09-07T00:00:00Z"}}
	}), "seed a pending revocation")

	// eve's report no longer lists "a" -- it applied the revocation.
	assertNoErr(t, ops.Report([]evePasskeyReportEntry{{ID: "b"}}), "Report")

	if revs := store.Get().EvePasskeyRevocations; len(revs) != 0 {
		t.Fatalf("revocation for an absent id survived Report: %+v", revs)
	}
}

// Decision 13: relay never lets its pending set outlive what would empty
// eve's last passkey, even from the report side -- if eve's report shows
// only one passkey left, any pending revocation for it (which would be
// stale in the ordinary case, since eve itself refuses to apply a
// last-credential revoke) is dropped rather than kept as a phantom that can
// never be satisfied.
func TestEvePasskeyOps_ReportDropsALastStandingRevocation(t *testing.T) {
	store := eveOpsStore(t)
	ops := &EvePasskeyOps{Store: store}

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "only"}}
		s.EvePasskeyRevocations = []config.EvePasskeyRevocation{{ID: "only", Requested: "2026-09-07T00:00:00Z"}}
	}), "seed a last-standing pending revocation")

	assertNoErr(t, ops.Report([]evePasskeyReportEntry{{ID: "only"}}), "Report")

	if revs := store.Get().EvePasskeyRevocations; len(revs) != 0 {
		t.Fatalf("a pending revocation for the report's only entry survived: %+v", revs)
	}
}

func evePasskeySeedTwo(t *testing.T, store config.SettingsStore) {
	t.Helper()
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{
			{ID: "p1", Label: "iPhone"},
			{ID: "p2", Label: "MacBook"},
		}
	}), "seed two eve passkeys")
}

func TestEvePasskeyOps_RevokeRefusesUnknownID(t *testing.T) {
	store := eveOpsStore(t)
	evePasskeySeedTwo(t, store)
	ops := &EvePasskeyOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}

	if _, err := ops.Revoke(context.Background(), "does-not-exist", auditViaCLI); !errors.Is(err, errEvePasskeyUnknown) {
		t.Fatalf("Revoke unknown id: err = %v, want errEvePasskeyUnknown", err)
	}
}

func TestEvePasskeyOps_RevokeRefusesAlreadyPending(t *testing.T) {
	store := eveOpsStore(t)
	evePasskeySeedTwo(t, store)
	ops := &EvePasskeyOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}

	_, err := ops.Revoke(context.Background(), "p1", auditViaCLI)
	assertNoErr(t, err, "first Revoke")

	if _, err := ops.Revoke(context.Background(), "p1", auditViaCLI); !errors.Is(err, errEvePasskeyAlreadyPending) {
		t.Fatalf("second Revoke of the same id: err = %v, want errEvePasskeyAlreadyPending", err)
	}
}

func TestEvePasskeyOps_RevokeRefusesTheLastCredential(t *testing.T) {
	store := eveOpsStore(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EvePasskeys = []config.EvePasskey{{ID: "only"}}
	}), "seed one eve passkey")
	ops := &EvePasskeyOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}

	if _, err := ops.Revoke(context.Background(), "only", auditViaCLI); !errors.Is(err, errEvePasskeyLast) {
		t.Fatalf("Revoke the last credential: err = %v, want errEvePasskeyLast", err)
	}
	if revs := store.Get().EvePasskeyRevocations; len(revs) != 0 {
		t.Fatalf("a refused revoke wrote a pending record anyway: %+v", revs)
	}
}

// Revoking down to one non-pending credential (via two revokes) must refuse
// on the second: pending count is counted against the last-credential rule
// too, not just the raw list length.
func TestEvePasskeyOps_RevokeRefusesWhenOnlyOneNonPendingWouldRemain(t *testing.T) {
	store := eveOpsStore(t)
	evePasskeySeedTwo(t, store)
	ops := &EvePasskeyOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}

	_, err := ops.Revoke(context.Background(), "p1", auditViaCLI)
	assertNoErr(t, err, "revoke p1")

	if _, err := ops.Revoke(context.Background(), "p2", auditViaCLI); !errors.Is(err, errEvePasskeyLast) {
		t.Fatalf("revoking the only remaining non-pending credential: err = %v, want errEvePasskeyLast", err)
	}
}

func TestEvePasskeyOps_RevokeRecordsAPendingRevocation(t *testing.T) {
	store := eveOpsStore(t)
	evePasskeySeedTwo(t, store)
	ops := &EvePasskeyOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}

	rec, err := ops.Revoke(context.Background(), "p1", auditViaCLI)
	assertNoErr(t, err, "Revoke")
	if rec.ID != "p1" || rec.Requested == "" {
		t.Fatalf("Revoke's returned record is incomplete: %+v", rec)
	}

	revs := store.Get().EvePasskeyRevocations
	if len(revs) != 1 || revs[0].ID != "p1" {
		t.Fatalf("settings.json does not carry the pending revocation: %+v", revs)
	}

	views := ops.List()
	var p1, p2 evePasskeyView
	for _, v := range views {
		if v.ID == "p1" {
			p1 = v
		}
		if v.ID == "p2" {
			p2 = v
		}
	}
	if !p1.RevocationPending {
		t.Errorf("List does not mark p1's revocation pending: %+v", p1)
	}
	if p2.RevocationPending {
		t.Errorf("List marks p2's revocation pending when it is not: %+v", p2)
	}
	if got := ops.Revocations(); len(got) != 1 || got[0] != "p1" {
		t.Fatalf("Revocations() = %v, want [p1]", got)
	}
}

// The design's atomicity claim for Revoke, the same property
// TestEveEnrolmentOps_ConsumeUnderConcurrentBurstHasExactlyOneWinner proves
// for Consume: racing Revoke against the SAME id from many goroutines must
// leave exactly one pending record, never a duplicate racing past the
// already-pending check.
func TestEvePasskeyOps_RevokeUnderConcurrentBurstLeavesExactlyOnePendingRecord(t *testing.T) {
	store := eveOpsStore(t)
	evePasskeySeedTwo(t, store)
	ops := &EvePasskeyOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}

	const n = 25
	var wins int32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, err := ops.Revoke(context.Background(), "p1", auditViaCLI); err == nil {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("got %d winner(s) racing one revoke, want exactly 1", wins)
	}
	if revs := store.Get().EvePasskeyRevocations; len(revs) != 1 {
		t.Fatalf("settings.json has %d pending revocations after the burst, want exactly 1: %+v", len(revs), revs)
	}
}

func TestEvePasskeyOps_Unrevoke(t *testing.T) {
	store := eveOpsStore(t)
	evePasskeySeedTwo(t, store)
	ops := &EvePasskeyOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}

	_, err := ops.Revoke(context.Background(), "p1", auditViaCLI)
	assertNoErr(t, err, "Revoke")

	assertNoErr(t, ops.Unrevoke("p1"), "Unrevoke")
	if revs := store.Get().EvePasskeyRevocations; len(revs) != 0 {
		t.Fatalf("Unrevoke left a pending record behind: %+v", revs)
	}

	// Unrevoking an id with nothing pending is a harmless no-op.
	assertNoErr(t, ops.Unrevoke("does-not-exist"), "Unrevoke of an absent id")
}

func TestEvePasskeyOps_NilOpsRefuseEveryMethod(t *testing.T) {
	var ops *EvePasskeyOps

	if err := ops.Report(nil); !errors.Is(err, errEvePasskeyOpsUnavailable) {
		t.Errorf("nil Report: err = %v, want errEvePasskeyOpsUnavailable", err)
	}
	if got := ops.List(); len(got) != 0 {
		t.Errorf("nil List = %+v, want empty", got)
	}
	if got := ops.Revocations(); len(got) != 0 {
		t.Errorf("nil Revocations = %+v, want empty", got)
	}
	if _, err := ops.Revoke(context.Background(), "x", auditViaCLI); !errors.Is(err, errEvePasskeyOpsUnavailable) {
		t.Errorf("nil Revoke: err = %v, want errEvePasskeyOpsUnavailable", err)
	}
	if err := ops.Unrevoke("x"); !errors.Is(err, errEvePasskeyOpsUnavailable) {
		t.Errorf("nil Unrevoke: err = %v, want errEvePasskeyOpsUnavailable", err)
	}
}
