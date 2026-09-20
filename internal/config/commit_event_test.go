package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

type commitFixture struct {
	store  *FileSettingsStore
	queue  *CommandQueue
	events atomic.Int64
}

func newCommitFixture(t *testing.T, store *FileSettingsStore) *commitFixture {
	t.Helper()
	q, err := NewCommandQueue(4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })
	f := &commitFixture{store: store, queue: q}
	q.SetCommitObserver(store.Commits, func() { f.events.Add(1) })
	return f
}

func newInitializedStore(t *testing.T) *FileSettingsStore {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	if err := store.EnsureInitialized(); err != nil {
		t.Fatal(err)
	}
	return store
}

func (f *commitFixture) run(cmd Command) error {
	return f.queue.Do(context.Background(), cmd)
}

func (f *commitFixture) want(t *testing.T, n int64, why string) {
	t.Helper()
	if got := f.events.Load(); got != n {
		t.Fatalf("%s: %d events, want %d", why, got, n)
	}
}

func TestCommitEventFiresOncePerCommandHoweverManySavesItMakes(t *testing.T) {
	f := newCommitFixture(t, newInitializedStore(t))
	var seenByObserver string
	f.queue.SetCommitObserver(f.store.Commits, func() {
		f.events.Add(1)
		seenByObserver = f.store.Get().Hosts[len(f.store.Get().Hosts)-1].Name
	})
	err := f.run(func(context.Context) error {
		if err := f.store.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: "h1", Name: "first"}) }); err != nil {
			return err
		}
		return f.store.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: "h2", Name: "second"}) })
	})
	if err != nil {
		t.Fatal(err)
	}
	f.want(t, 1, "two saves in one command")
	if seenByObserver != "second" {
		t.Fatalf("observer saw %q: the event fired before the last commit was visible", seenByObserver)
	}
}

func TestCommitEventIsAbsentWhenNothingWasPersisted(t *testing.T) {
	f := newCommitFixture(t, newInitializedStore(t))
	declined := errors.New("declined")

	if err := f.run(func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	f.want(t, 0, "a successful command that persisted nothing")

	err := f.run(func(context.Context) error {
		return f.store.WithDeclinable(func(s *Settings) error {
			s.Hosts = append(s.Hosts, Host{ID: "h"})
			return declined
		})
	})
	if !errors.Is(err, declined) {
		t.Fatalf("err = %v, want the decline", err)
	}
	f.want(t, 0, "a declined command")
	if len(f.store.Get().Hosts) != 0 {
		t.Fatal("a declined command left its mutation in the cache")
	}
}

func TestCommitEventIsAbsentWhenPersistenceFails(t *testing.T) {
	// A sealer-less store refuses every write: the failed-persistence shape.
	dir := mkEmptySandboxRelayHome(t)
	f := newCommitFixture(t, NewSettingsStoreAt(dir))
	err := f.run(func(context.Context) error {
		return f.store.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: "h"}) })
	})
	if !errors.Is(err, ErrSealerRequired) {
		t.Fatalf("err = %v, want ErrSealerRequired", err)
	}
	f.want(t, 0, "failed persistence")
	if len(f.store.Get().Hosts) != 0 {
		t.Fatal("failed persistence changed the cached state")
	}
}

func TestCommitEventFiresWhenACommandPersistsAndThenFails(t *testing.T) {
	f := newCommitFixture(t, newInitializedStore(t))
	after := errors.New("effect failed")
	err := f.run(func(context.Context) error {
		if err := f.store.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: "h"}) }); err != nil {
			return err
		}
		return after
	})
	if !errors.Is(err, after) {
		t.Fatalf("err = %v", err)
	}
	f.want(t, 1, "the committed state changed even though the command reported failure")
}

func TestCommitEventsFollowAdmissionOrderOneToOne(t *testing.T) {
	f := newCommitFixture(t, newInitializedStore(t))
	for i := 0; i < 3; i++ {
		id := string(rune('a' + i))
		if err := f.run(func(context.Context) error {
			return f.store.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: id}) })
		}); err != nil {
			t.Fatal(err)
		}
		f.want(t, int64(i+1), "one event per committed command")
	}
}

type importFixture struct {
	*commitFixture
	dir string
}

func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	owner := sealedSettingsStoreAt(dir)
	if err := owner.EnsureInitialized(); err != nil {
		t.Fatal(err)
	}
	owner.OwnExclusively()
	return &importFixture{commitFixture: newCommitFixture(t, owner), dir: dir}
}

// handEdit writes settings.json behind the owner's back through a second store.
func (f *importFixture) handEdit(t *testing.T, id string) {
	t.Helper()
	editor := sealedSettingsStoreAt(f.dir)
	if err := editor.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: id, Name: "edited"}) }); err != nil {
		t.Fatal(err)
	}
}

func (f *importFixture) importFile() (changed bool, err error) {
	err = f.run(func(context.Context) error {
		changed, err = f.store.ImportFile()
		return err
	})
	return
}

func TestValidHandEditIsPickedUpThroughTheQueueWithOneEvent(t *testing.T) {
	f := newImportFixture(t)
	f.handEdit(t, "h1")

	if f.store.ReloadIfChanged() != nil || len(FreshSettings(f.store).Hosts) != 0 {
		t.Fatal("an owned store must not see the edit before it is imported")
	}
	changed, err := f.importFile()
	if err != nil || !changed {
		t.Fatalf("import = %v, %v; want the edit adopted", changed, err)
	}
	if hosts := f.store.Get().Hosts; len(hosts) != 1 || hosts[0].ID != "h1" {
		t.Fatalf("hosts after import = %+v", hosts)
	}
	f.want(t, 1, "one adopted edit")

	if changed, err := f.importFile(); err != nil || changed {
		t.Fatalf("re-import = %v, %v; want a no-op", changed, err)
	}
	f.want(t, 1, "re-importing identical bytes")
}

func TestInvalidHandEditIsRejectedAndTheCurrentStateKept(t *testing.T) {
	f := newImportFixture(t)
	if err := f.run(func(context.Context) error {
		return f.store.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: "keep"}) })
	}); err != nil {
		t.Fatal(err)
	}
	f.want(t, 1, "seed")

	if err := os.WriteFile(filepath.Join(f.dir, "settings.json"), []byte(`{"hosts": [`), 0600); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.importFile(); err == nil || changed {
		t.Fatalf("import = %v, %v; want a rejection", changed, err)
	}
	if hosts := f.store.Get().Hosts; len(hosts) != 1 || hosts[0].ID != "keep" {
		t.Fatalf("a rejected edit changed state: %+v", hosts)
	}
	f.want(t, 1, "a rejected edit")

	if err := os.Remove(filepath.Join(f.dir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.importFile(); err != nil || changed || len(f.store.Get().Hosts) != 1 {
		t.Fatalf("a deleted file must leave state alone: %v, %v", changed, err)
	}
}

func TestTheTraysOwnWriteIsNotImported(t *testing.T) {
	f := newImportFixture(t)
	if err := f.run(func(context.Context) error {
		return f.store.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: "mine"}) })
	}); err != nil {
		t.Fatal(err)
	}
	f.want(t, 1, "the tray's own save")
	if changed, err := f.importFile(); err != nil || changed {
		t.Fatalf("own write imported: %v, %v", changed, err)
	}
	f.want(t, 1, "importing the tray's own write")
}

func TestHandEditIsOrderedWithQueuedMutationsByAdmission(t *testing.T) {
	f := newImportFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	blocker := make(chan error, 1)
	go func() {
		blocker <- f.run(func(context.Context) error { close(started); <-release; return nil })
	}()
	<-started
	f.handEdit(t, "edit")

	importDone, mutateDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := f.importFile(); importDone <- err }()
	if err := f.queue.WaitForPending(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	go func() {
		mutateDone <- f.run(func(context.Context) error {
			return f.store.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: "after"}) })
		})
	}()
	if err := f.queue.WaitForPending(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	close(release)
	for _, ch := range []chan error{blocker, importDone, mutateDone} {
		if err := <-ch; err != nil {
			t.Fatal(err)
		}
	}

	var ids []string
	for _, h := range f.store.Get().Hosts {
		ids = append(ids, h.ID)
	}
	if len(ids) != 2 || ids[0] != "edit" || ids[1] != "after" {
		t.Fatalf("hosts = %v, want the edit then the later mutation, both kept", ids)
	}
	f.want(t, 2, "one event for the import and one for the mutation")
}

func TestSettingsWatcherSeesInPlaceEditsAndRepeatedAtomicReplaces(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("v0"), 0600); err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 64)
	stop, err := WatchSettingsFile(dir, func() {
		b, _ := os.ReadFile(path)
		seen <- string(b)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	awaitContent := func(want string) {
		t.Helper()
		for got := range seen {
			if got == want {
				return
			}
		}
	}
	if err := os.WriteFile(path, []byte("in-place"), 0600); err != nil {
		t.Fatal(err)
	}
	awaitContent("in-place")
	for _, v := range []string{"replace-1", "replace-2"} {
		if err := AtomicWriteFile(path, []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
		awaitContent(v)
	}
	if err := os.WriteFile(path, []byte("after-replace-in-place"), 0600); err != nil {
		t.Fatal(err)
	}
	awaitContent("after-replace-in-place")
}

func TestOwnedStoreExplicitReloadIsTheRepairPrimitive(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	owner := sealedSettingsStoreAt(dir)
	if err := owner.EnsureInitialized(); err != nil {
		t.Fatal(err)
	}
	owner.OwnExclusively()
	editor := sealedSettingsStoreAt(dir)
	if err := editor.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: "h2"}) }); err != nil {
		t.Fatal(err)
	}
	if len(owner.Get().Hosts) != 0 {
		t.Fatal("precondition: owner has not seen the edit")
	}
	if len(owner.Reload().Hosts) != 1 {
		t.Fatal("an explicit Reload must adopt the file")
	}
}

func TestRestartFirstSnapshotEqualsTheLastCommittedFile(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	first := sealedSettingsStoreAt(dir)
	if err := first.EnsureInitialized(); err != nil {
		t.Fatal(err)
	}
	first.OwnExclusively()
	for _, id := range []string{"a", "b", "c"} {
		if err := first.With(func(s *Settings) { s.Hosts = append(s.Hosts, Host{ID: id}) }); err != nil {
			t.Fatal(err)
		}
	}
	want := first.Get()

	restarted := sealedSettingsStoreAt(dir)
	got := restarted.Get()
	if len(got.Hosts) != len(want.Hosts) {
		t.Fatalf("restart saw %d hosts, last commit had %d", len(got.Hosts), len(want.Hosts))
	}
	for i := range got.Hosts {
		if got.Hosts[i].ID != want.Hosts[i].ID {
			t.Fatalf("host %d = %q, want %q", i, got.Hosts[i].ID, want.Hosts[i].ID)
		}
	}
}
