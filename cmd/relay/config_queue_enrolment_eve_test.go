package main

import (
	"context"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

func TestEnrolmentOpsCreateWaitsForQueueAndRecordsBeforeReturn(t *testing.T) {
	_, store := newEnrolmentSandbox(t)
	queue, err := config.NewCommandQueue(1)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })

	started := make(chan struct{})
	release := make(chan struct{})
	blockDone := make(chan error, 1)
	go func() {
		blockDone <- queue.Do(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	rec := enabledIssuanceRecorder(t)
	ops := &EnrolmentOps{Store: store, Queue: queue, Gate: allowGate(t), Audit: rec}
	created := make(chan error, 1)
	go func() {
		_, err := ops.Create(context.Background(), enrolmentFields{ClientID: "queued-enrolment"}, auditViaCLI, "")
		created <- err
	}()

	select {
	case err := <-created:
		t.Fatalf("queued enrolment mutation returned before the earlier command completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	assertNoErr(t, <-blockDone, "blocking command")
	assertNoErr(t, <-created, "Create")

	if got := len(store.Get().Enrolments); got != 1 {
		t.Fatalf("Create returned before persisting its enrolment: got %d records", got)
	}
	if events := readLoggedEvents(t, rec); len(events) != 1 {
		t.Fatalf("Create returned before recording its issuance: %+v", events)
	}
}

func TestEveEnrolmentOpenWaitsForQueueAndRecordsBeforeReturn(t *testing.T) {
	store := eveOpsStore(t)
	queue, err := config.NewCommandQueue(1)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })

	started := make(chan struct{})
	release := make(chan struct{})
	blockDone := make(chan error, 1)
	go func() {
		blockDone <- queue.Do(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	rec := enabledIssuanceRecorder(t)
	ops := &EveEnrolmentOps{Store: store, Queue: queue, Gate: allowGate(t), Audit: rec}
	opened := make(chan error, 1)
	go func() {
		_, err := ops.Open(context.Background(), auditViaCLI)
		opened <- err
	}()

	select {
	case err := <-opened:
		t.Fatalf("queued Eve mutation returned before the earlier command completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	assertNoErr(t, <-blockDone, "blocking command")
	assertNoErr(t, <-opened, "Open")

	if store.Get().EveEnrolment == nil {
		t.Fatal("Open returned before persisting the Eve enrolment window")
	}
	if events := readLoggedEvents(t, rec); len(events) != 1 {
		t.Fatalf("Open returned before recording the Eve issuance: %+v", events)
	}
}
