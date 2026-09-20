package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

func newTemplateQueueOps(t *testing.T, capacity int) (*TemplateOps, config.SettingsStore, *config.CommandQueue) {
	t.Helper()
	store := eveOpsStore(t)
	queue, err := config.NewCommandQueue(capacity)
	assertNoErr(t, err, "NewCommandQueue")
	t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })
	return &TemplateOps{Store: store, Queue: queue}, store, queue
}

func templateByID(store config.SettingsStore, id string) (config.TerminalTemplate, bool) {
	all := store.Get().TerminalTemplates
	i := slices.IndexFunc(all, func(x config.TerminalTemplate) bool { return x.ID == id })
	if i < 0 {
		return config.TerminalTemplate{}, false
	}
	return all[i], true
}

func TestTemplateOpsMutationsWaitForQueue(t *testing.T) {
	ops, store, queue := newTemplateQueueOps(t, 1)
	releaseBlocker := queueBlocker(t, queue)

	created := make(chan error, 1)
	go func() {
		created <- ops.Create(context.Background(), config.TerminalTemplate{ID: "queued-template", Name: "Queued"})
	}()
	waitForMutationAdmission(t, queue)
	assertMutationStillWaiting(t, created)
	if _, ok := templateByID(store, "queued-template"); ok {
		t.Fatal("Create persisted before the queue admitted it")
	}
	releaseBlocker()
	assertNoErr(t, <-created, "Create")
	if _, ok := templateByID(store, "queued-template"); !ok {
		t.Fatal("Create returned before persisting its template")
	}
}

func TestTemplateOpsQueuedCreateUpdateRemoveRunInAdmissionOrder(t *testing.T) {
	ops, store, queue := newTemplateQueueOps(t, 4)
	releaseBlocker := queueBlocker(t, queue)
	ctx := context.Background()

	// Each step is admitted before the next starts, so the queue order is
	// fixed: Create, Update, Remove, Create again. Out of order, Update or
	// Remove would fail with not-found or the last Create with a conflict.
	steps := []func() error{
		func() error { return ops.Create(ctx, config.TerminalTemplate{ID: "seq", Name: "one"}) },
		func() error { return ops.Update(ctx, config.TerminalTemplate{ID: "seq", Name: "two"}) },
		func() error { return ops.Remove(ctx, "seq") },
		func() error { return ops.Create(ctx, config.TerminalTemplate{ID: "seq", Name: "three"}) },
	}
	results := make([]chan error, len(steps))
	for i, step := range steps {
		results[i] = make(chan error, 1)
		go func() { results[i] <- step() }()
		waitForPending(t, queue, i+1)
	}
	releaseBlocker()
	for i, ch := range results {
		assertNoErr(t, <-ch, fmt.Sprintf("step %d", i))
	}
	if got, ok := templateByID(store, "seq"); !ok || got.Name != "three" {
		t.Fatalf("final template = %+v (present %v), want name %q", got, ok, "three")
	}
}

func TestTemplateOpsConcurrentCreatesAllSurvive(t *testing.T) {
	const writers = 8
	ops, store, queue := newTemplateQueueOps(t, writers)
	releaseBlocker := queueBlocker(t, queue)

	results := make(chan error, writers)
	for i := 0; i < writers; i++ {
		go func() {
			results <- ops.Create(context.Background(), config.TerminalTemplate{ID: fmt.Sprintf("bulk-%d", i), Name: "Bulk"})
		}()
	}
	waitForPending(t, queue, writers)
	releaseBlocker()
	for i := 0; i < writers; i++ {
		assertNoErr(t, <-results, "Create")
	}
	for i := 0; i < writers; i++ {
		if _, ok := templateByID(store, fmt.Sprintf("bulk-%d", i)); !ok {
			t.Fatalf("template bulk-%d was lost", i)
		}
	}
}

func TestTemplateOpsCanceledBeforeAdmissionDoesNotMutate(t *testing.T) {
	ops, store, _ := newTemplateQueueOps(t, 1)
	assertNoErr(t, ops.Create(context.Background(), config.TerminalTemplate{ID: "keep", Name: "before"}), "seed")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ops.Create(ctx, config.TerminalTemplate{ID: "new", Name: "New"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create err = %v, want context.Canceled", err)
	}
	if err := ops.Update(ctx, config.TerminalTemplate{ID: "keep", Name: "after"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Update err = %v, want context.Canceled", err)
	}
	if err := ops.Remove(ctx, "keep"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Remove err = %v, want context.Canceled", err)
	}
	if _, ok := templateByID(store, "new"); ok {
		t.Fatal("canceled Create mutated settings")
	}
	if got, ok := templateByID(store, "keep"); !ok || got.Name != "before" {
		t.Fatalf("canceled Update/Remove mutated settings: %+v (present %v)", got, ok)
	}
}

func TestTemplateOpsQueuedErrorsStayTyped(t *testing.T) {
	ops, _, _ := newTemplateQueueOps(t, 1)
	ctx := context.Background()
	assertNoErr(t, ops.Create(ctx, config.TerminalTemplate{ID: "dup", Name: "Dup"}), "seed")
	if err := ops.Create(ctx, config.TerminalTemplate{ID: "dup", Name: "Dup"}); !errors.Is(err, errTemplateExists) {
		t.Fatalf("duplicate Create err = %v, want errTemplateExists", err)
	}
	if err := ops.Update(ctx, config.TerminalTemplate{ID: "nope", Name: "Nope"}); !errors.Is(err, errTemplateNotFound) {
		t.Fatalf("Update err = %v, want errTemplateNotFound", err)
	}
	if err := ops.Remove(ctx, "nope"); !errors.Is(err, errTemplateNotFound) {
		t.Fatalf("Remove err = %v, want errTemplateNotFound", err)
	}
	if err := ops.Create(ctx, config.TerminalTemplate{ID: "bad id", Name: "Bad"}); !errors.Is(err, errTemplateInvalid) {
		t.Fatalf("invalid Create err = %v, want errTemplateInvalid", err)
	}
}

func waitForPending(t *testing.T, queue *config.CommandQueue, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	assertNoErr(t, queue.WaitForPending(ctx, n), "wait for pending commands")
}
