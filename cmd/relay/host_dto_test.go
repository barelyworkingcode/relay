package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

// TestCachedHostCheck_CachesWithinTTL is item 3's cache regression test:
// hostStatus must not run a fresh `ssh -O check` on every read within
// hostStatusCacheTTL.
func TestCachedHostCheck_CachesWithinTTL(t *testing.T) {
	var checkCalls int32
	stubSSHRunner(t, func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		if argvContains(args, "check") {
			atomic.AddInt32(&checkCalls, 1)
			return nil, nil, nil // exit 0 == a live ControlMaster
		}
		return []byte(cannedProbeOutputForTest), nil, nil
	})
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	ops := &HostOps{Store: store}
	created, err := ops.Create(context.Background(), hostFields{Name: "devbox", Target: "admin@devbox.local"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h, _ := config.FindHostByID(store.Get(), created.ID)
	for i := 0; i < 5; i++ {
		if got := hostStatus(*h); got != "connected" {
			t.Fatalf("hostStatus = %q, want connected", got)
		}
	}
	if got := atomic.LoadInt32(&checkCalls); got != 1 {
		t.Fatalf("expected exactly 1 ssh -O check call across 5 reads inside the TTL window, got %d", got)
	}
}

// TestHostOps_Disconnect_InvalidatesStatusCache: Disconnect deliberately
// tears the ControlMaster down, so the very next status read must not
// report a cached "connected" from before the disconnect.
func TestHostOps_Disconnect_InvalidatesStatusCache(t *testing.T) {
	var checkCalls int32
	stubSSHRunner(t, func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		switch {
		case argvContains(args, "check"):
			atomic.AddInt32(&checkCalls, 1)
			return nil, nil, nil
		case argvContains(args, "exit"):
			return nil, nil, nil
		default:
			return []byte(cannedProbeOutputForTest), nil, nil
		}
	})
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	ops := &HostOps{Store: store}
	created, err := ops.Create(context.Background(), hostFields{Name: "devbox", Target: "admin@devbox.local"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h, _ := config.FindHostByID(store.Get(), created.ID)

	hostStatus(*h)
	if got := atomic.LoadInt32(&checkCalls); got != 1 {
		t.Fatalf("expected 1 check call before disconnect, got %d", got)
	}

	if _, found, err := ops.Disconnect(created.ID); err != nil || !found {
		t.Fatalf("Disconnect: found=%v err=%v", found, err)
	}

	hostStatus(*h)
	if got := atomic.LoadInt32(&checkCalls); got != 2 {
		t.Fatalf("expected disconnect to invalidate the cache and force a fresh check, got %d calls", got)
	}
}

// TestHostsToView_ChecksHostsConcurrently is item 3's concurrency
// regression test: rendering a list of hosts must not serialize one
// ssh -O check per host end to end.
func TestHostsToView_ChecksHostsConcurrently(t *testing.T) {
	const numHosts = 16
	const checkDelay = 100 * time.Millisecond
	stubSSHRunner(t, func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		if argvContains(args, "check") {
			time.Sleep(checkDelay)
			return nil, nil, nil
		}
		return []byte(cannedProbeOutputForTest), nil, nil
	})
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	ops := &HostOps{Store: store}
	for i := 0; i < numHosts; i++ {
		if _, err := ops.Create(context.Background(), hostFields{
			Name:   "devbox" + string(rune('a'+i)),
			Target: "admin@devbox.local",
		}); err != nil {
			t.Fatalf("Create host %d: %v", i, err)
		}
	}

	start := time.Now()
	views := hostsToView(ops.List())
	elapsed := time.Since(start)

	if len(views) != numHosts {
		t.Fatalf("expected %d views, got %d", numHosts, len(views))
	}
	for _, v := range views {
		if v.Status != "connected" {
			t.Fatalf("expected every host connected, got %+v", v)
		}
	}
	// Serialized, numHosts checks at checkDelay each would take >= 1.6s.
	// hostListCheckWorkers bounds this to ceil(numHosts/workers) batches;
	// generous slack keeps this hermetic under load without losing the
	// property being tested (concurrency, not a tight latency budget).
	maxExpected := checkDelay * 6
	if elapsed >= maxExpected {
		t.Fatalf("hostsToView took %v, expected well under the %v serialized bound (workers should run these concurrently)", elapsed, maxExpected)
	}
}
