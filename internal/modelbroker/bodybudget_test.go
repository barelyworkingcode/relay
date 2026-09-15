package modelbroker

import (
	"context"
	"testing"
	"time"
)

// TestBodyBudget_SecondAcquireBlocksUntilFirstReleases is BodyBudget's own
// proof of the concurrency limit model_endpoint.go relies on: with a
// capacity that fits exactly one weight-sized admission, a second Acquire
// must not return until the first Release runs — proven by ordering, not by
// how long the second call happens to take.
func TestBodyBudget_SecondAcquireBlocksUntilFirstReleases(t *testing.T) {
	b := NewBodyBudget(10)

	if !b.Acquire(context.Background(), 10) {
		t.Fatal("first Acquire at exactly the capacity should succeed")
	}
	if got := b.InFlight(); got != 10 {
		t.Fatalf("InFlight = %d, want 10", got)
	}

	secondDone := make(chan bool, 1)
	go func() {
		secondDone <- b.Acquire(context.Background(), 10)
	}()

	select {
	case <-secondDone:
		t.Fatal("second Acquire returned before the first Release — the budget did not block")
	case <-time.After(100 * time.Millisecond):
		// Still blocked, as required. Not itself the proof (a slow machine
		// could produce this by accident) — the proof is that it unblocks
		// immediately below, once and only once Release runs.
	}

	b.Release(10)

	select {
	case ok := <-secondDone:
		if !ok {
			t.Fatal("second Acquire failed after capacity was released")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire never unblocked after Release")
	}
	if got := b.InFlight(); got != 10 {
		t.Fatalf("InFlight after second acquire = %d, want 10", got)
	}
}

// TestBodyBudget_ManyConcurrentAcquiresNeverExceedCapacity fires many more
// concurrent acquirers than the budget can admit at once and proves, via
// InFlight() sampled from inside every admitted holder (never via elapsed
// time), that the observed concurrent total never exceeds capacity.
func TestBodyBudget_ManyConcurrentAcquiresNeverExceedCapacity(t *testing.T) {
	const (
		weight     = 4
		slots      = 3
		capacity   = weight * slots
		goroutines = 25
	)
	b := NewBodyBudget(capacity)

	var mu chan struct{} = make(chan struct{}, 1)
	mu <- struct{}{}
	peak := 0
	current := 0

	done := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if !b.Acquire(context.Background(), weight) {
				t.Errorf("Acquire failed unexpectedly")
				return
			}
			<-mu
			current++
			if current > peak {
				peak = current
			}
			if got := b.InFlight(); got > capacity {
				t.Errorf("InFlight = %d, want <= capacity %d", got, capacity)
			}
			mu <- struct{}{}

			// Hold briefly so other admitted goroutines have a real chance
			// to overlap with this one before releasing.
			time.Sleep(5 * time.Millisecond)

			<-mu
			current--
			mu <- struct{}{}
			b.Release(weight)
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	if peak > slots {
		t.Fatalf("peak concurrent admissions = %d, want <= %d (capacity/weight)", peak, slots)
	}
	if got := b.InFlight(); got != 0 {
		t.Fatalf("InFlight after every goroutine released = %d, want 0", got)
	}
}

// TestBodyBudget_AcquireTimesOutWhenNeverReleased proves the bounded-wait
// half of the design: a blocked Acquire gives up as soon as its context
// expires rather than waiting forever, and returns the capacity to
// "available" for it (no leaked partial admission on a failed Acquire).
func TestBodyBudget_AcquireTimesOutWhenNeverReleased(t *testing.T) {
	b := NewBodyBudget(10)
	if !b.Acquire(context.Background(), 10) {
		t.Fatal("first Acquire should succeed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if b.Acquire(ctx, 10) {
		t.Fatal("second Acquire should have timed out; capacity was never released")
	}
	if got := b.InFlight(); got != 10 {
		t.Fatalf("InFlight after a failed Acquire = %d, want 10 (only the first holder's)", got)
	}
}

// TestBodyBudget_WeightAboveCapacityNeverAdmitted proves a weight that can
// never fit refuses immediately instead of blocking until ctx's deadline
// for a request the budget could never satisfy.
func TestBodyBudget_WeightAboveCapacityNeverAdmitted(t *testing.T) {
	b := NewBodyBudget(10)
	start := time.Now()
	if b.Acquire(context.Background(), 11) {
		t.Fatal("a weight larger than capacity must never be admitted")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("Acquire took %v for an unsatisfiable weight; want an immediate refusal", elapsed)
	}
}
