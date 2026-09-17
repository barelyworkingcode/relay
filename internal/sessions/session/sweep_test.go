package session_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
)

func writeSessionFile(t *testing.T, dir, id, body string, age time.Duration) {
	t.Helper()
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestSweepSessions_RemovesOnlyOldHeadless(t *testing.T) {
	dir := t.TempDir()
	writeSessionFile(t, dir, "old-headless", `{"sessionId":"old-headless","headless":true}`, 10*24*time.Hour)
	writeSessionFile(t, dir, "new-headless", `{"sessionId":"new-headless","headless":true}`, time.Hour)
	writeSessionFile(t, dir, "old-normal", `{"sessionId":"old-normal","headless":false}`, 10*24*time.Hour)
	writeSessionFile(t, dir, "with-pi", `{"sessionId":"with-pi","headless":false,"providerState":{"piSessionId":"pi-abc"}}`, time.Hour)

	removed, livePi, err := session.SweepSessions(dir, session.DefaultHeadlessMaxAge)
	if err != nil {
		t.Fatalf("SweepSessions: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "old-headless.json")); !os.IsNotExist(err) {
		t.Fatal("old-headless.json should have been removed")
	}
	for _, keep := range []string{"new-headless", "old-normal", "with-pi"} {
		if _, err := os.Stat(filepath.Join(dir, keep+".json")); err != nil {
			t.Fatalf("%s.json should have survived: %v", keep, err)
		}
	}
	if _, ok := livePi["pi-abc"]; !ok {
		t.Fatalf("livePi = %v, want pi-abc present", livePi)
	}
}

func TestSweepSessions_MissingDir(t *testing.T) {
	removed, livePi, err := session.SweepSessions(filepath.Join(t.TempDir(), "does-not-exist"), time.Hour)
	if err != nil {
		t.Fatalf("SweepSessions on missing dir: %v", err)
	}
	if removed != 0 || len(livePi) != 0 {
		t.Fatalf("removed=%d livePi=%v, want 0/empty", removed, livePi)
	}
}

func TestSweepOrphanedPiSessions(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "20240101_orphan-id.jsonl")
	live := filepath.Join(dir, "20240101_live-id.jsonl")
	tooNew := filepath.Join(dir, "20240101_new-orphan.jsonl")

	for _, p := range []string{orphan, live, tooNew} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{orphan, live} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}
	// tooNew keeps its fresh mtime: within minAge, so it must survive even
	// though its piSessionId isn't in livePi — the race cushion the ported
	// doc comment names.

	livePi := map[string]struct{}{"live-id": {}}
	removed, err := session.SweepOrphanedPiSessions(dir, livePi, session.DefaultPiOrphanMinAge)
	if err != nil {
		t.Fatalf("SweepOrphanedPiSessions: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan file should have been removed")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("live-referenced file should have survived")
	}
	if _, err := os.Stat(tooNew); err != nil {
		t.Fatal("too-new orphan should have survived the minAge cushion")
	}
}
