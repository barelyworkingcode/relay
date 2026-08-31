package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// sdSettingsPath is where every store in this file writes.
func sdSettingsPath(dir string) string { return filepath.Join(dir, "settings.json") }

func sdStat(t *testing.T, dir string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(sdSettingsPath(dir))
	if err != nil {
		t.Fatalf("stat settings.json: %v", err)
	}
	return info
}

func sdRead(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(sdSettingsPath(dir))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	return data
}

// A callback that declines must leave the file alone — not rewrite it with the
// same bytes. Identical content is not the property being tested: a rewrite is
// a chance for a concurrent writer's change to be lost, so the assertion is
// that no new file was renamed over this one.
func TestSettingsStore_ADeclinedCallbackWritesNothing(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if err := store.With(func(s *Settings) { s.AdminSecret = NewSecret("before") }); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := sdStat(t, dir)
	beforeBytes := sdRead(t, dir)

	refusal := fmt.Errorf("declined on purpose")
	err := store.WithDeclinable(func(s *Settings) error {
		s.AdminSecret = NewSecret("after")
		return refusal
	})
	if err != refusal {
		t.Fatalf("WithDeclinable returned %v, want the callback's own error %v", err, refusal)
	}

	after := sdStat(t, dir)
	if !os.SameFile(before, after) {
		t.Fatal("a declined write replaced settings.json (a new file was renamed over it)")
	}
	if got := sdRead(t, dir); string(got) != string(beforeBytes) {
		t.Fatalf("a declined write changed settings.json:\n before %s\n after  %s", beforeBytes, got)
	}
	if got, _ := store.Get().AdminSecret.Reveal(); got != "before" {
		t.Fatalf("the declined mutation reached the cache: admin secret = %q", got)
	}

	// The other half: a callback that returns nil still saves.
	if err := store.WithDeclinable(func(s *Settings) error { s.AdminSecret = NewSecret("committed"); return nil }); err != nil {
		t.Fatalf("WithDeclinable (committing): %v", err)
	}
	if got, _ := store.Get().AdminSecret.Reveal(); got != "committed" {
		t.Fatalf("admin secret = %q after a committing callback, want committed", got)
	}
}

// The reviewer's control/flood comparison, made deterministic. The loss it
// reproduces needs one writer holding a view that predates another writer's
// commit, which is the ordinary cross-process case: the tray's store and a CLI
// process's store both write settings.json and nothing locks it. The staleness
// is pinned open here by restoring the modtime the first store last saw —
// docs/tokens.md already names the modtime as the only signal — rather than by
// racing two goroutines and hoping the interleaving lands.
func TestSettingsStore_ADeclinedWriteCannotLoseAnotherWritersChange(t *testing.T) {
	const canary = "sd-canary-credential"

	run := func(t *testing.T, flood int) *Settings {
		t.Helper()
		dir := mkEmptySandboxRelayHome(t)
		stale := sealedSettingsStoreAt(dir)
		if err := stale.EnsureInitialized(); err != nil {
			t.Fatalf("EnsureInitialized: %v", err)
		}
		if err := stale.With(func(s *Settings) { s.AdminSecret = NewSecret("seeded") }); err != nil {
			t.Fatalf("seed: %v", err)
		}
		seen := sdStat(t, dir).ModTime()

		// A second store standing in for `relay credential mint` in its own
		// process: it commits, and is told the write succeeded.
		committer := sealedSettingsStoreAt(dir)
		if err := committer.With(func(s *Settings) { s.AdminSecret = NewSecret(canary) }); err != nil {
			t.Fatalf("committing writer: %v", err)
		}
		if err := os.Chtimes(sdSettingsPath(dir), seen, seen); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if got := sdStat(t, dir).ModTime(); !got.Equal(seen) {
			t.Fatalf("could not pin the modtime: got %v, want %v", got, seen)
		}

		for i := 0; i < flood; i++ {
			err := stale.WithDeclinable(func(s *Settings) error {
				return fmt.Errorf("refused %d", i)
			})
			if err == nil {
				t.Fatalf("refusal %d reported success", i)
			}
		}
		return sealedSettingsStoreAt(dir).Reload()
	}

	t.Run("control", func(t *testing.T) {
		if got, _ := run(t, 0).AdminSecret.Reveal(); got != canary {
			t.Fatalf("with no refusals at all the committed change is %q, want %q — the harness itself loses it", got, canary)
		}
	})

	t.Run("flood", func(t *testing.T) {
		if got, _ := run(t, 200).AdminSecret.Reveal(); got != canary {
			t.Fatalf("200 refused writes lost a change another writer was told had succeeded: admin secret = %q, want %q", got, canary)
		}
	})
}

// Two stores writing at once must never leave an unparseable file behind. They
// are separate values with separate mutexes, which is what a second process is
// from this one's point of view, and the payloads differ in length so a
// staging file shared between them tears visibly rather than by luck.
func TestSettingsStore_ConcurrentWritersNeverProduceAnUnparseableFile(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	seed := sealedSettingsStoreAt(dir)
	if err := seed.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}

	const (
		writers = 6
		rounds  = 12
	)
	bulk := func(n int) []Project {
		out := make([]Project, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, Project{
				ID:            fmt.Sprintf("project-%04d", i),
				Name:          strings.Repeat("n", 120),
				Path:          "/tmp/" + strings.Repeat("p", 200),
				AllowedMcpIDs: []string{"*"},
				AllowedModels: []string{},
			})
		}
		return out
	}

	parse := func(t *testing.T, where string) {
		t.Helper()
		data, err := os.ReadFile(sdSettingsPath(dir))
		if err != nil {
			if os.IsNotExist(err) {
				return
			}
			t.Errorf("%s: read settings.json: %v", where, err)
			return
		}
		var s Settings
		if err := json.Unmarshal(data, &s); err != nil {
			t.Errorf("%s: settings.json is unparseable after concurrent writes (%d bytes): %v\ntail: %q",
				where, len(data), err, tailOf(data, 120))
		}
	}

	done := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-done:
				return
			default:
				parse(t, "during the writes")
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			store := sealedSettingsStoreAt(dir)
			for r := 0; r < rounds; r++ {
				if err := store.With(func(s *Settings) {
					s.AdminSecret = NewSecret(fmt.Sprintf("writer-%d-round-%d", w, r))
					s.Projects = bulk(200 + w*90)
				}); err != nil {
					t.Errorf("writer %d round %d: %v", w, r, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(done)
	readers.Wait()

	parse(t, "after the writes")

	// A staging file left behind would be a second defect wearing the same
	// clothes: the unique name must still be renamed away, not accumulated.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a staging file survived the writes: %s", e.Name())
		}
	}
}

func tailOf(data []byte, n int) string {
	if len(data) <= n {
		return string(data)
	}
	return string(data[len(data)-n:])
}
