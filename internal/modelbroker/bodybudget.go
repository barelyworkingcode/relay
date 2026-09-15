package modelbroker

import (
	"context"
	"sync"
)

// BodyBudget is a weighted semaphore bounding the total number of bytes the
// model endpoint will commit to in-flight request-body handling at once,
// across every concurrent caller and both listeners (relay#116 re-review,
// S6). A single near-cap JSON body already amplifies several-fold across
// extraction and rewrite (docs/model-endpoint.md's Limits section has the
// measured number); without a shared ceiling, that amplification multiplies
// again by however many callers connect at once, which — unlike the body
// cap itself — nothing in this package otherwise bounds.
//
// Acquire is weighted by the CALLER-SUPPLIED weight, which model_endpoint.go
// always passes as the matched route's body cap, never the request's actual
// (or declared) size: relay reads up to that cap regardless of what a
// caller claims up front (readCapped enforces it independent of
// Content-Length), so the worst case for an admitted request is always
// cap-sized — accounting by anything smaller would let a caller under-report
// its size to buy extra concurrency the cap was supposed to rule out.
type BodyBudget struct {
	mu        sync.Mutex
	cond      *sync.Cond
	capacity  int64
	available int64
}

// NewBodyBudget creates a budget with the given total capacity, in bytes.
func NewBodyBudget(capacity int64) *BodyBudget {
	b := &BodyBudget{capacity: capacity, available: capacity}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Acquire blocks until weight bytes are available or ctx is done, whichever
// comes first, returning whether it succeeded. A weight larger than the
// budget's whole capacity can never be satisfied and fails immediately
// rather than blocking until ctx's deadline for no reason.
func (b *BodyBudget) Acquire(ctx context.Context, weight int64) bool {
	if weight <= 0 {
		return true
	}
	if weight > b.capacity {
		return false
	}

	// sync.Cond has no context-aware wait, so a goroutine watching ctx
	// wakes every blocked waiter on cancellation; each re-checks ctx.Err()
	// itself before deciding whether to keep waiting. The goroutine exits
	// via stop as soon as Acquire returns by any path, successful or not.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			b.cond.Broadcast()
		case <-stop:
		}
	}()

	b.mu.Lock()
	defer b.mu.Unlock()
	for b.available < weight {
		if ctx.Err() != nil {
			return false
		}
		b.cond.Wait()
	}
	if ctx.Err() != nil {
		return false
	}
	b.available -= weight
	return true
}

// Release returns weight bytes to the budget, waking any waiter that might
// now be satisfiable.
func (b *BodyBudget) Release(weight int64) {
	if weight <= 0 {
		return
	}
	b.mu.Lock()
	b.available += weight
	b.mu.Unlock()
	b.cond.Broadcast()
}

// InFlight reports the number of bytes currently committed, for tests that
// need to observe admission without racing on wall-clock timing.
func (b *BodyBudget) InFlight() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capacity - b.available
}
