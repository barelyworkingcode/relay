package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

func queueBlocker(t *testing.T, queue *config.CommandQueue) func() {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- queue.Do(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	released := false
	finished := false
	releaseBlocker := func() {
		if !released {
			close(release)
			released = true
		}
		if !finished {
			assertNoErr(t, <-done, "blocking command")
			finished = true
		}
	}
	t.Cleanup(releaseBlocker)
	return releaseBlocker
}

func waitForMutationAdmission(t *testing.T, queue *config.CommandQueue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	assertNoErr(t, queue.WaitForPending(ctx, 1), "wait for mutation admission")
}

func assertMutationStillWaiting(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("queued mutation returned before the blocker completed: %v", err)
	default:
	}
}

func TestEnrolmentOpsCreateWaitsForQueueAndRecordsBeforeReturn(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	queue, err := config.NewCommandQueue(1)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	releaseBlocker := queueBlocker(t, queue)

	rec := enabledIssuanceRecorder(t)
	ops := &EnrolmentOps{Store: store, Queue: queue, Gate: allowGate(t), Audit: rec}
	created := make(chan error, 1)
	go func() {
		_, err := ops.Create(context.Background(), enrolmentFields{ClientID: "queued-enrolment"}, auditViaCLI, "")
		created <- err
	}()
	waitForMutationAdmission(t, queue)
	assertMutationStillWaiting(t, created)
	releaseBlocker()
	assertNoErr(t, <-created, "Create")

	if got := len(store.Get().Enrolments); got != 1 {
		t.Fatalf("Create returned before persisting its enrolment: got %d records", got)
	}
	if events := readLoggedEvents(t, rec); len(events) != 1 {
		t.Fatalf("Create returned before recording its issuance: %+v", events)
	}
}

func TestEnrolmentOpsCreateCompletesAfterCallerCancellation(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	queue, err := config.NewCommandQueue(1)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	releaseBlocker := queueBlocker(t, queue)

	rec := enabledIssuanceRecorder(t)
	ops := &EnrolmentOps{Store: store, Queue: queue, Gate: allowGate(t), Audit: rec}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	created := make(chan error, 1)
	go func() {
		_, err := ops.Create(ctx, enrolmentFields{ClientID: "canceled-enrolment"}, auditViaCLI, "")
		created <- err
	}()
	waitForMutationAdmission(t, queue)
	cancel()
	assertMutationStillWaiting(t, created)
	releaseBlocker()
	assertNoErr(t, <-created, "Create after cancellation")

	if got := len(store.Get().Enrolments); got != 1 {
		t.Fatalf("canceled caller returned before persisting its enrolment: got %d records", got)
	}
	if events := readLoggedEvents(t, rec); len(events) != 1 {
		t.Fatalf("canceled caller returned before recording its issuance: %+v", events)
	}
}

func TestEveEnrolmentOpenWaitsForQueueAndRecordsBeforeReturn(t *testing.T) {
	store := eveOpsStore(t)
	queue, err := config.NewCommandQueue(1)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	releaseBlocker := queueBlocker(t, queue)

	rec := enabledIssuanceRecorder(t)
	ops := &EveEnrolmentOps{Store: store, Queue: queue, Gate: allowGate(t), Audit: rec}
	opened := make(chan error, 1)
	go func() {
		_, err := ops.Open(context.Background(), auditViaCLI)
		opened <- err
	}()
	waitForMutationAdmission(t, queue)
	assertMutationStillWaiting(t, opened)
	releaseBlocker()
	assertNoErr(t, <-opened, "Open")

	if store.Get().EveEnrolment == nil {
		t.Fatal("Open returned before persisting the Eve enrolment window")
	}
	if events := readLoggedEvents(t, rec); len(events) != 1 {
		t.Fatalf("Open returned before recording the Eve issuance: %+v", events)
	}
}

func TestEvePasskeyReportWaitsForQueue(t *testing.T) {
	store := eveOpsStore(t)
	queue, err := config.NewCommandQueue(1)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	releaseBlocker := queueBlocker(t, queue)

	ops := &EvePasskeyOps{Store: store, Queue: queue}
	reported := make(chan error, 1)
	go func() {
		reported <- ops.Report([]evePasskeyReportEntry{{ID: "queued-passkey", Label: "browser"}})
	}()
	waitForMutationAdmission(t, queue)
	assertMutationStillWaiting(t, reported)
	releaseBlocker()
	assertNoErr(t, <-reported, "Report")

	if got := store.Get().EvePasskeys; len(got) != 1 || got[0].ID != "queued-passkey" {
		t.Fatalf("Report returned before persisting its mirror: %+v", got)
	}
}

func TestEnrolmentOpsSetRemoteConfigRefusesStaleApprovalDecision(t *testing.T) {
	store := eveOpsStore(t)
	queue, err := config.NewCommandQueue(1)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	releaseBlocker := queueBlocker(t, queue)

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &EnrolmentOps{Store: store, Queue: queue, Gate: gate, Issuance: pgwWithIssuance(t)}
	removed := make(chan error, 1)
	go func() {
		_, err := ops.SetRemoteConfig(context.Background(), remoteConfigFields{Remove: true}, auditViaCLI, "")
		removed <- err
	}()
	waitForMutationAdmission(t, queue)

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.Remote = &config.RemoteConfig{Listen: "127.0.0.1:9910", Enabled: boolPtr(true)}
	}), "seed remote config after admission")
	releaseBlocker()
	if err := <-removed; !errors.Is(err, errRemoteConfigChangedDuringApproval) {
		t.Fatalf("Remove committed on a stale approval decision: err = %v, want errRemoteConfigChangedDuringApproval", err)
	}
	if store.Get().Remote == nil {
		t.Fatal("a queued Remove cleared remote config after its gate refused")
	}
}
