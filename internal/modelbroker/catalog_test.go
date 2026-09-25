package modelbroker

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func TestCache_SnapshotFetchesOnceWhenTTLNeverExpires(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		calls := 0
		c := NewCache(func(ctx context.Context) ([]Row, error) {
			calls++
			return []Row{{ID: "foo", OwnedBy: "llama.cpp"}}, nil
		}, ttl)
		clock := newFakeClock()
		c.SetClock(clock.Now)
		if calls != 0 {
			t.Fatalf("ttl %v: NewCache must not fetch eagerly, got %d calls", ttl, calls)
		}
		for i := 0; i < 2; i++ {
			if _, err := c.Snapshot(context.Background()); err != nil {
				t.Fatalf("ttl %v: Snapshot: %v", ttl, err)
			}
			clock.now = clock.now.Add(24 * time.Hour)
		}
		if calls != 1 {
			t.Fatalf("ttl %v: Snapshot called fetch %d times, want 1", ttl, calls)
		}
	}
}

func TestCache_SnapshotRefetchesOnceTTLHasElapsed(t *testing.T) {
	calls := 0
	c := NewCache(func(ctx context.Context) ([]Row, error) {
		calls++
		rows := []Row{{ID: "foo", OwnedBy: "llama.cpp"}}
		if calls > 1 {
			rows = append(rows, Row{ID: "bar", OwnedBy: "MLX"})
		}
		return rows, nil
	}, 30*time.Second)
	clock := newFakeClock()
	c.SetClock(clock.Now)
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	clock.now = clock.now.Add(29 * time.Second)
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot at +29s: %v", err)
	}
	if calls != 1 {
		t.Fatalf("at +29s: calls = %d, want 1", calls)
	}

	clock.now = clock.now.Add(time.Second)
	rows, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot at +30s: %v", err)
	}
	if calls != 2 {
		t.Fatalf("at +30s: calls = %d, want 2", calls)
	}
	if _, ok := findRow(rows, "bar"); !ok {
		t.Fatalf("at +30s: refreshed snapshot lacks the upstream addition: %v", rows)
	}
}

func TestCache_FailedRefreshErrorsThenRetries(t *testing.T) {
	calls := 0
	failing := false
	c := NewCache(func(ctx context.Context) ([]Row, error) {
		calls++
		if failing {
			return nil, errors.New("upstream unreachable")
		}
		return []Row{{ID: "foo", OwnedBy: "llama.cpp"}}, nil
	}, 30*time.Second)
	clock := newFakeClock()
	c.SetClock(clock.Now)
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	clock.now = clock.now.Add(30 * time.Second)
	failing = true
	rows, err := c.Snapshot(context.Background())
	if err == nil {
		t.Fatalf("expired Snapshot with a failing fetch: want an error, got rows %v", rows)
	}
	if len(rows) != 0 {
		t.Fatalf("a failed refresh served stale rows: %v", rows)
	}

	failing = false
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot after upstream recovered: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (fill, failed refresh, retry)", calls)
	}
}

func TestCache_ResolveFetchesAtMostOncePerCall(t *testing.T) {
	cases := []struct {
		name    string
		warm    bool
		advance time.Duration
		id      string
		want    int // fetches made by the Resolve call alone
	}{
		{"never filled, miss", false, 0, "absent", 1},
		{"expired, miss", true, 30 * time.Second, "absent", 1},
		{"expired, hit", true, 30 * time.Second, "foo", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := NewCache(func(ctx context.Context) ([]Row, error) {
				calls++
				return []Row{{ID: "foo", OwnedBy: "llama.cpp"}}, nil
			}, 30*time.Second)
			clock := newFakeClock()
			c.SetClock(clock.Now)
			if tc.warm {
				if _, err := c.Snapshot(context.Background()); err != nil {
					t.Fatalf("Snapshot: %v", err)
				}
			}
			clock.now = clock.now.Add(tc.advance)
			before := calls
			if _, err := c.Resolve(context.Background(), tc.id); err != nil {
				t.Fatalf("Resolve(%s): %v", tc.id, err)
			}
			if got := calls - before; got != tc.want {
				t.Fatalf("Resolve made %d fetches, want %d", got, tc.want)
			}
		})
	}
}

func TestCache_ResolveRefetchesExactlyOnceOnMiss(t *testing.T) {
	calls := 0
	rows := []Row{{ID: "foo", OwnedBy: "llama.cpp"}}
	c := NewCache(func(ctx context.Context) ([]Row, error) {
		calls++
		return rows, nil
	}, 0)

	// Warm the cache: one fetch.
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if calls != 1 {
		t.Fatalf("after Snapshot: calls = %d, want 1", calls)
	}

	// A hit needs no refetch.
	if _, err := c.Resolve(context.Background(), "foo"); err != nil {
		t.Fatalf("Resolve(foo): %v", err)
	}
	if calls != 1 {
		t.Fatalf("after Resolve(hit): calls = %d, want 1", calls)
	}

	// A miss triggers exactly one more fetch.
	if _, err := c.Resolve(context.Background(), "just-loaded"); err != nil {
		t.Fatalf("Resolve(miss): %v", err)
	}
	if calls != 2 {
		t.Fatalf("after Resolve(miss): calls = %d, want 2", calls)
	}
}

func TestCache_ResolveFindsRowThatAppearsOnRefetch(t *testing.T) {
	calls := 0
	c := NewCache(func(ctx context.Context) ([]Row, error) {
		calls++
		if calls == 1 {
			return []Row{{ID: "foo", OwnedBy: "llama.cpp"}}, nil
		}
		return []Row{{ID: "foo", OwnedBy: "llama.cpp"}, {ID: "bar", OwnedBy: "MLX"}}, nil
	}, 0)
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	rows, err := c.Resolve(context.Background(), "bar")
	if err != nil {
		t.Fatalf("Resolve(bar): %v", err)
	}
	if _, ok := findRow(rows, "bar"); !ok {
		t.Fatalf("expected bar to appear after the miss-triggered refetch, got %v", rows)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestCache_FetchErrorPropagates(t *testing.T) {
	wantErr := errors.New("upstream unreachable")
	c := NewCache(func(ctx context.Context) ([]Row, error) {
		return nil, wantErr
	}, 0)
	if _, err := c.Snapshot(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Snapshot error = %v, want %v", err, wantErr)
	}
}
