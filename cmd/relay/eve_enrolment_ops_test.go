package main

// Unit coverage for EveEnrolmentOps (docs/eve-passkey-enrolment.md): Open's
// expiry and replace-not-accumulate behaviour, Status open/closed
// (including the unparseable-expiry-reads-closed rule every other anchor in
// this package follows), Consume's single-use guarantee under a concurrent
// burst, and nil-safety across all three methods. The presence-gate wiring
// itself (nil Gate refuses, an allowing gate succeeds, issuance auditing off
// refuses before the prompt) is proven once, generically, by
// presence_gate_wiring_test.go's pgwCases entry for "eve.enrolment.open" —
// this file does not repeat that.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
)

func eveOpsStore(t *testing.T) config.SettingsStore {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	return store
}

func TestEveEnrolmentOps_OpenSetsExpiryAboutFiveMinutesOut(t *testing.T) {
	store := eveOpsStore(t)
	ops := &EveEnrolmentOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}

	before := time.Now().UTC()
	view, err := ops.Open(context.Background(), auditViaCLI)
	assertNoErr(t, err, "Open")
	after := time.Now().UTC()

	if !view.Open {
		t.Fatalf("Open's own view reports closed: %+v", view)
	}
	at, err := time.Parse(time.RFC3339, view.Expires)
	assertNoErr(t, err, "parse Expires")

	// RFC3339 truncates to whole seconds rather than rounding, so the
	// parsed expiry can read up to a second earlier than the wall-clock
	// bound taken just before Open ran; the lower bound allows for that.
	if at.Before(before.Add(eveEnrolmentTTL-time.Second)) || at.After(after.Add(eveEnrolmentTTL+time.Second)) {
		t.Fatalf("expiry %s is not ~%s from now (window [%s, %s])", at, eveEnrolmentTTL, before.Add(eveEnrolmentTTL), after.Add(eveEnrolmentTTL))
	}

	stored := store.Get().EveEnrolment
	if stored == nil || stored.Expires != view.Expires {
		t.Fatalf("settings.json's eve_enrolment does not match the returned view: stored=%+v view=%+v", stored, view)
	}
}

// EveEnrolment is a single pointer field, not a slice — there is no way to
// "accumulate" a second window structurally. What this proves instead is
// that Open unconditionally overwrites whatever was there, rather than
// refusing or merging with a pre-existing record: a stale window with a
// distinguishable sentinel expiry must be gone after a fresh Open, replaced
// by exactly the expiry Open itself returns.
func TestEveEnrolmentOps_OpenReplacesRatherThanAccumulates(t *testing.T) {
	store := eveOpsStore(t)
	ops := &EveEnrolmentOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}

	const sentinel = "2099-01-01T00:00:00Z"
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EveEnrolment = &config.EveEnrolmentWindow{Expires: sentinel}
	}), "seed a pre-existing window")

	view, err := ops.Open(context.Background(), auditViaCLI)
	assertNoErr(t, err, "Open")

	if view.Expires == sentinel {
		t.Fatalf("Open returned the pre-existing sentinel expiry instead of minting a fresh one")
	}
	stored := store.Get().EveEnrolment
	if stored == nil || stored.Expires != view.Expires {
		t.Fatalf("settings.json still carries the old window after Open: stored=%+v want=%q", stored, view.Expires)
	}
}

func TestEveEnrolmentOps_StatusOpenAndClosed(t *testing.T) {
	store := eveOpsStore(t)
	ops := &EveEnrolmentOps{Store: store}

	if s := ops.Status(); s.Open {
		t.Fatalf("Status reports open with no window ever minted: %+v", s)
	}

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EveEnrolment = &config.EveEnrolmentWindow{Expires: time.Now().UTC().Add(time.Minute).Format(time.RFC3339)}
	}), "seed an open window")
	if s := ops.Status(); !s.Open {
		t.Fatalf("Status reports closed with a live window on disk: %+v", s)
	}

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EveEnrolment = &config.EveEnrolmentWindow{Expires: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)}
	}), "seed an expired window")
	if s := ops.Status(); s.Open {
		t.Fatalf("Status reports open for an expired window: %+v", s)
	}
}

// An Expires relay cannot parse reads as expired — the same rule
// consumeBootstrapCode's own comment gives for LoginBootstrap, and
// APICredential.Expired gives for a credential's own lifetime.
func TestEveEnrolmentOps_UnparseableExpiryReadsClosed(t *testing.T) {
	store := eveOpsStore(t)
	ops := &EveEnrolmentOps{Store: store}

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.EveEnrolment = &config.EveEnrolmentWindow{Expires: "not-a-timestamp"}
	}), "seed an unparseable window")

	if s := ops.Status(); s.Open {
		t.Fatalf("Status reports open for an unparseable expiry: %+v", s)
	}
	if _, err := ops.Consume(context.Background(), eveEnrolmentClaim{IP: "10.0.0.1"}); !errors.Is(err, errEveEnrolmentClosed) {
		t.Fatalf("Consume on an unparseable expiry: err = %v, want errEveEnrolmentClosed", err)
	}
}

func TestEveEnrolmentOps_ConsumeSpendsTheWindowOnce(t *testing.T) {
	store := eveOpsStore(t)
	ops := &EveEnrolmentOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}

	opened, err := ops.Open(context.Background(), auditViaCLI)
	assertNoErr(t, err, "Open")

	consumed, err := ops.Consume(context.Background(), eveEnrolmentClaim{IP: "10.0.1.7", Label: "test-browser"})
	assertNoErr(t, err, "first Consume")
	if consumed.Expires != opened.Expires {
		t.Fatalf("Consume's expires = %q, want the window's own %q", consumed.Expires, opened.Expires)
	}
	if store.Get().EveEnrolment != nil {
		t.Fatalf("the window is still on disk after a successful Consume: %+v", store.Get().EveEnrolment)
	}

	if _, err := ops.Consume(context.Background(), eveEnrolmentClaim{IP: "10.0.1.8"}); !errors.Is(err, errEveEnrolmentClosed) {
		t.Fatalf("replay Consume: err = %v, want errEveEnrolmentClosed", err)
	}
}

func TestEveEnrolmentOps_ConsumeWithNoWindowRefuses(t *testing.T) {
	store := eveOpsStore(t)
	ops := &EveEnrolmentOps{Store: store}

	if _, err := ops.Consume(context.Background(), eveEnrolmentClaim{IP: "10.0.1.9"}); !errors.Is(err, errEveEnrolmentClosed) {
		t.Fatalf("Consume with nothing open: err = %v, want errEveEnrolmentClosed", err)
	}
}

// The design's whole atomicity claim: two browsers racing the same window
// inside a concurrent burst of Consume calls must produce exactly one
// winner, never zero and never more than one — proof that Consume really
// runs inside one store.With rather than a read-then-delete with a gap a
// second goroutine can land in.
func TestEveEnrolmentOps_ConsumeUnderConcurrentBurstHasExactlyOneWinner(t *testing.T) {
	store := eveOpsStore(t)
	ops := &EveEnrolmentOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}
	_, err := ops.Open(context.Background(), auditViaCLI)
	assertNoErr(t, err, "Open")

	const n = 25
	var wins int32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := ops.Consume(context.Background(), eveEnrolmentClaim{IP: "10.0.2.1"}); err == nil {
				atomic.AddInt32(&wins, 1)
			}
		}(i)
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("got %d winner(s) racing one window, want exactly 1", wins)
	}
	if store.Get().EveEnrolment != nil {
		t.Fatalf("the window is still on disk after the burst: %+v", store.Get().EveEnrolment)
	}
}

func TestEveEnrolmentOps_NilOpsRefuseEveryMethod(t *testing.T) {
	var ops *EveEnrolmentOps

	if _, err := ops.Open(context.Background(), auditViaCLI); !errors.Is(err, errEveEnrolmentOpsUnavailable) {
		t.Errorf("nil Open: err = %v, want errEveEnrolmentOpsUnavailable", err)
	}
	if s := ops.Status(); s.Open {
		t.Errorf("nil Status reports open: %+v", s)
	}
	if _, err := ops.Consume(context.Background(), eveEnrolmentClaim{IP: "10.0.0.1"}); !errors.Is(err, errEveEnrolmentOpsUnavailable) {
		t.Errorf("nil Consume: err = %v, want errEveEnrolmentOpsUnavailable", err)
	}
}

// Open records the issuance (Subject: the expiry, Revoked: false) and
// Consume records the consumption (Subject: the claimant's IP, Name: its
// label, Revoked: true) — the two directions docs/eve-passkey-enrolment.md
// describes, told apart in the log the same way a passkey's mint and revoke
// are.
func TestEveEnrolmentOps_AuditsOpenAndConsumeDistinctly(t *testing.T) {
	store := eveOpsStore(t)
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	rec, err := audit.NewAuditRecorder(nil, logPath, openAuditWriter)
	assertNoErr(t, err, "NewAuditRecorder")
	ops := &EveEnrolmentOps{Store: store, Gate: allowGate(t), Audit: rec}

	_, err = ops.Open(context.Background(), auditViaCLI)
	assertNoErr(t, err, "Open")
	_, err = ops.Consume(context.Background(), eveEnrolmentClaim{IP: "10.0.3.1", Label: "consume-test-browser"})
	assertNoErr(t, err, "Consume")

	rec.Close() // flush and wait for the writer goroutine before reading
	data, err := os.ReadFile(logPath)
	assertNoErr(t, err, "read audit log")
	events := aiParse(t, string(data))
	var opened, closed *audit.AuditEvent
	for i := range events {
		if events[i].Credential != auditCredentialEveEnrolment {
			continue
		}
		switch events[i].Event {
		case audit.AuditEventCredentialRevoked:
			closed = &events[i]
		case audit.AuditEventCredentialIssued:
			opened = &events[i]
		}
	}
	if opened == nil {
		t.Fatalf("no eve_enrolment issuance record found; events=%+v", events)
	}
	if closed == nil {
		t.Fatalf("no eve_enrolment consumption record found; events=%+v", events)
	}
	if closed.Subject != "10.0.3.1" {
		t.Errorf("consume record Subject = %q, want the claimant IP", closed.Subject)
	}
	if closed.SubjectName != "consume-test-browser" {
		t.Errorf("consume record SubjectName = %q, want the claimant label", closed.SubjectName)
	}
}

func TestEveEnrolmentRemaining_FormatsAndClosesWithTheWindow(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	w := &config.EveEnrolmentWindow{Expires: now.Add(4*time.Minute + 12*time.Second).Format(time.RFC3339)}

	remaining, open := eveEnrolmentRemaining(w, now)
	if !open {
		t.Fatal("a window five minutes from expiry reports closed")
	}
	if remaining != "4m 12s left" {
		t.Fatalf("remaining = %q, want %q", remaining, "4m 12s left")
	}

	if _, open := eveEnrolmentRemaining(nil, now); open {
		t.Error("a nil window reports open")
	}
	expired := &config.EveEnrolmentWindow{Expires: now.Add(-time.Second).Format(time.RFC3339)}
	if _, open := eveEnrolmentRemaining(expired, now); open {
		t.Error("an expired window reports open")
	}
}
