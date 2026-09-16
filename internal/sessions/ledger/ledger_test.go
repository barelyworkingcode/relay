package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

func mustOpen(t *testing.T, dir string) *Ledger {
	t.Helper()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l
}

func TestLedger_PutGetRoundTrips(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)

	rec := Record{
		SessionID: "s1",
		Kind:      "claude",
		ProjectID: "p1",
		Directory: "/tmp/p1",
		Created:   time.Now().UTC().Truncate(time.Second),
		State:     StateLive,
	}
	if err := l.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := l.Get("s1")
	if !ok {
		t.Fatal("Get: not found after Put")
	}
	if got.ProjectID != "p1" || got.Kind != "claude" || got.State != StateLive {
		t.Fatalf("Get returned %+v", got)
	}

	// A fresh Open must see what the previous handle saved.
	l2 := mustOpen(t, dir)
	got2, ok := l2.Get("s1")
	if !ok || got2.SessionID != "s1" {
		t.Fatalf("reopened ledger: Get(s1) = %+v, %v", got2, ok)
	}
}

func TestLedger_SetState(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	if err := l.Put(Record{SessionID: "s1", State: StateLive}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	found, err := l.SetState("s1", StateDormant)
	if err != nil || !found {
		t.Fatalf("SetState(s1) = %v, %v", found, err)
	}
	got, _ := l.Get("s1")
	if got.State != StateDormant {
		t.Fatalf("state = %q, want dormant", got.State)
	}

	found, err = l.SetState("does-not-exist", StateDormant)
	if err != nil || found {
		t.Fatalf("SetState(unknown) = %v, %v, want false, nil", found, err)
	}
}

func TestLedger_Remove(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	if err := l.Put(Record{SessionID: "s1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := l.Remove("s1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := l.Get("s1"); ok {
		t.Fatal("Get after Remove still found the record")
	}
	// Removing an id that was never there is a no-op, not an error.
	if err := l.Remove("never-existed"); err != nil {
		t.Fatalf("Remove(unknown): %v", err)
	}
}

func TestLedger_AllIsSorted(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	for _, id := range []string{"zzz", "aaa", "mmm"} {
		if err := l.Put(Record{SessionID: id, Kind: "claude"}); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	all := l.All()
	if len(all) != 3 || all[0].SessionID != "aaa" || all[1].SessionID != "mmm" || all[2].SessionID != "zzz" {
		t.Fatalf("All() = %+v, want sorted [aaa mmm zzz]", all)
	}
}

func TestLedger_FileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes only")
	}
	dir := t.TempDir()
	l := mustOpen(t, dir)
	if err := l.Put(Record{SessionID: "s1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("ledger file mode = %o, want 0600", perm)
	}
}

// TestLedger_FileNeverCarriesModelKeyOrSecretShape is the required
// "ledger file 0600 with no rmk_/64-hex" security assertion: it byte-scans
// the ledger's actual serialized bytes on disk — not the in-memory Record —
// for the model-key prefix and for any substring shaped like a 64-lowercase-
// hex launch secret, over a ledger populated with records that look like
// real, populated sessions (including SessionRequest blobs carrying
// plausible-looking but non-credential fields).
func TestLedger_FileNeverCarriesModelKeyOrSecretShape(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)

	sessionRequest, _ := json.Marshal(map[string]any{
		"projectId": "p1",
		"directory": "/Users/alice/code/widget",
		"name":      "refactor pass",
		"model":     "claude-sonnet-4.5",
		"settings": map[string]any{
			"permissionMode": "default",
			"permissionPolicy": map[string]any{
				"allowedTools": []string{"Bash:git *", "Read"},
				"deniedTools":  []string{"Bash:rm *"},
				"defaultMode":  "default",
			},
		},
	})

	records := []Record{
		{
			SessionID:      "11111111-1111-1111-1111-111111111111",
			Kind:           "claude",
			ProjectID:      "p1",
			Directory:      "/Users/alice/code/widget",
			TemplateID:     "claude-code",
			Created:        time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
			State:          StateLive,
			SessionRequest: sessionRequest,
		},
		{
			SessionID: "22222222-2222-2222-2222-222222222222",
			Kind:      "pi",
			ProjectID: "p2",
			Directory: "/Users/alice/code/other",
			Created:   time.Date(2026, 9, 14, 9, 30, 0, 0, time.UTC),
			State:     StateDormant,
		},
	}
	for _, r := range records {
		if err := l.Put(r); err != nil {
			t.Fatalf("Put(%s): %v", r.SessionID, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if regexp.MustCompile(`rmk_`).Match(data) {
		t.Fatal("ledger file contains the model-key prefix rmk_")
	}
	if m := regexp.MustCompile(`[0-9a-f]{64}`).Find(data); m != nil {
		t.Fatalf("ledger file contains a 64-lowercase-hex substring shaped like a launch secret: %q", m)
	}

	info, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("ledger file mode = %o, want 0600", perm)
		}
	}
}

func TestLedger_OpenMissingFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	if all := l.All(); len(all) != 0 {
		t.Fatalf("All() on a fresh ledger = %+v, want empty", all)
	}
	if Exists(dir) {
		t.Fatal("Exists() true before any save")
	}
}

func TestLedger_ExistsAfterFirstSave(t *testing.T) {
	dir := t.TempDir()
	l := mustOpen(t, dir)
	if err := l.Put(Record{SessionID: "s1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !Exists(dir) {
		t.Fatal("Exists() false after a save")
	}
}
