package config

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestCommandQueueRejectsInvalidCommands(t *testing.T) {
	if _, err := NewCommandQueue(0); err == nil {
		t.Fatal("NewCommandQueue(0) returned nil error")
	}
	q, err := NewCommandQueue(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Do(context.Background(), nil); !errors.Is(err, ErrNilCommand) {
		t.Fatalf("nil command error = %v, want %v", err, ErrNilCommand)
	}
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCommandQueueRunsFIFOAndCallerWaits(t *testing.T) {
	q, err := NewCommandQueue(1)
	if err != nil {
		t.Fatal(err)
	}

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- q.Do(context.Background(), func(context.Context) error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
	}()
	<-firstStarted

	var orderMu sync.Mutex
	var order []int
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- q.Do(context.Background(), func(context.Context) error {
			close(secondStarted)
			<-releaseSecond
			orderMu.Lock()
			order = append(order, 2)
			orderMu.Unlock()
			return nil
		})
	}()
	waitForPending(t, q, 1)

	thirdDone := make(chan error, 1)
	go func() {
		thirdDone <- q.Do(context.Background(), func(context.Context) error {
			orderMu.Lock()
			order = append(order, 3)
			orderMu.Unlock()
			return nil
		})
	}()

	close(releaseFirst)
	if err := receiveQueueResult(t, firstDone); err != nil {
		t.Fatal(err)
	}
	<-secondStarted
	select {
	case err := <-thirdDone:
		t.Fatalf("third command completed before second was released: %v", err)
	default:
	}
	close(releaseSecond)
	if err := receiveQueueResult(t, secondDone); err != nil {
		t.Fatal(err)
	}
	if err := receiveQueueResult(t, thirdDone); err != nil {
		t.Fatal(err)
	}

	orderMu.Lock()
	got := append([]int(nil), order...)
	orderMu.Unlock()
	if !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("execution order = %v, want [2 3]", got)
	}
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCommandQueueReturnsCommandErrors(t *testing.T) {
	q, err := NewCommandQueue(1)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("command failed")
	if err := q.Do(context.Background(), func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatalf("command error = %v, want %v", err, want)
	}
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCommandQueueCancellationBeforeAdmission(t *testing.T) {
	q, err := NewCommandQueue(1)
	if err != nil {
		t.Fatal(err)
	}
	alreadyCanceled, cancelBeforeAdmission := context.WithCancel(context.Background())
	cancelBeforeAdmission()
	if err := q.Do(alreadyCanceled, func(context.Context) error {
		t.Fatal("already-canceled command was admitted")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-canceled submission error = %v, want context.Canceled", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- q.Do(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	secondRan := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- q.Do(ctx, func(context.Context) error {
			close(secondRan)
			return nil
		})
	}()
	waitForPending(t, q, 1)
	thirdDone := make(chan error, 1)
	go func() {
		thirdDone <- q.Do(context.Background(), func(context.Context) error { return nil })
	}()
	cancel()
	if err := receiveQueueResult(t, secondDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled submission error = %v, want context.Canceled", err)
	}
	close(release)
	if err := receiveQueueResult(t, firstDone); err != nil {
		t.Fatal(err)
	}
	if err := receiveQueueResult(t, thirdDone); err != nil {
		t.Fatal(err)
	}
	select {
	case <-secondRan:
		t.Fatal("canceled command ran")
	default:
	}
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCommandQueueShutdownCancelsQueuedAndActiveCommands(t *testing.T) {
	q, err := NewCommandQueue(2)
	if err != nil {
		t.Fatal(err)
	}
	activeStarted := make(chan struct{})
	activeCanceled := make(chan struct{})
	activeDone := make(chan error, 1)
	go func() {
		activeDone <- q.Do(context.Background(), func(ctx context.Context) error {
			close(activeStarted)
			<-ctx.Done()
			close(activeCanceled)
			return ctx.Err()
		})
	}()
	<-activeStarted

	queuedRan := make(chan struct{})
	queuedDone := make(chan error, 1)
	go func() {
		queuedDone <- q.Do(context.Background(), func(context.Context) error {
			close(queuedRan)
			return nil
		})
	}()
	waitForPending(t, q, 1)
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-activeCanceled
	if err := receiveQueueResult(t, activeDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("active result = %v, want context.Canceled", err)
	}
	if err := receiveQueueResult(t, queuedDone); !errors.Is(err, ErrCommandQueueShutdown) {
		t.Fatalf("queued result = %v, want %v", err, ErrCommandQueueShutdown)
	}
	select {
	case <-queuedRan:
		t.Fatal("queued command ran during shutdown")
	default:
	}
	if err := q.Do(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrCommandQueueShutdown) {
		t.Fatalf("post-shutdown submission error = %v, want %v", err, ErrCommandQueueShutdown)
	}
}

func TestCommandQueueCloseDrainsAcceptedCommands(t *testing.T) {
	q, err := NewCommandQueue(1)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- q.Do(context.Background(), func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- q.Do(context.Background(), func(context.Context) error { return nil })
	}()
	waitForPending(t, q, 1)

	q.Close()
	if err := q.Do(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrCommandQueueClosed) {
		t.Fatalf("post-close submission error = %v, want %v", err, ErrCommandQueueClosed)
	}
	close(release)
	if err := receiveQueueResult(t, firstDone); err != nil {
		t.Fatal(err)
	}
	if err := receiveQueueResult(t, secondDone); err != nil {
		t.Fatal(err)
	}
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func receiveQueueResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for command result")
		return nil
	}
}

func waitForPending(t *testing.T, q *CommandQueue, want int) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		q.mu.Lock()
		if len(q.pending) >= want {
			q.mu.Unlock()
			return
		}
		wake := q.wake
		q.mu.Unlock()
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %d pending commands", want)
		}
	}
}
