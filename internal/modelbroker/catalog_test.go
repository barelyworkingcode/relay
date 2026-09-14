package modelbroker

import (
	"context"
	"errors"
	"testing"
)

func TestCache_SnapshotFetchesOnce(t *testing.T) {
	calls := 0
	c := NewCache(func(ctx context.Context) ([]Row, error) {
		calls++
		return []Row{{ID: "foo", OwnedBy: "llama.cpp"}}, nil
	})
	if calls != 0 {
		t.Fatalf("NewCache must not fetch eagerly, got %d calls", calls)
	}
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Snapshot called fetch %d times, want 1 (no TTL policy in this package)", calls)
	}
}

func TestCache_ResolveRefetchesExactlyOnceOnMiss(t *testing.T) {
	calls := 0
	rows := []Row{{ID: "foo", OwnedBy: "llama.cpp"}}
	c := NewCache(func(ctx context.Context) ([]Row, error) {
		calls++
		return rows, nil
	})

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
	})
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
	})
	if _, err := c.Snapshot(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Snapshot error = %v, want %v", err, wantErr)
	}
}
