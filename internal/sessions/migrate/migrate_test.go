package migrate_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/migrate"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRun_CopiesNamedDirsAndWritesMarker(t *testing.T) {
	relayLLMDir := t.TempDir()
	hostDir := filepath.Join(t.TempDir(), "host")

	writeFile(t, filepath.Join(relayLLMDir, "sessions", "s1.json"), `{"sessionId":"s1"}`)
	writeFile(t, filepath.Join(relayLLMDir, "pi-sessions", "sub", "20240101_abc.jsonl"), `{}`)
	writeFile(t, filepath.Join(relayLLMDir, "terminal_logs", "t1.head.log"), `hello`)
	writeFile(t, filepath.Join(relayLLMDir, "not-migrated", "x.txt"), `should not be copied`)

	if err := migrate.Run(relayLLMDir, hostDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, rel := range []string{
		filepath.Join("sessions", "s1.json"),
		filepath.Join("pi-sessions", "sub", "20240101_abc.jsonl"),
		filepath.Join("terminal_logs", "t1.head.log"),
	} {
		if _, err := os.Stat(filepath.Join(hostDir, rel)); err != nil {
			t.Fatalf("expected %s to be copied: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(hostDir, "not-migrated")); !os.IsNotExist(err) {
		t.Fatal("Run copied a directory outside its named list")
	}
	if !migrate.Done(hostDir) {
		t.Fatal("Done() false after a successful Run")
	}

	data, err := os.ReadFile(filepath.Join(hostDir, "sessions", "s1.json"))
	if err != nil || string(data) != `{"sessionId":"s1"}` {
		t.Fatalf("copied file content mismatch: %q, err=%v", data, err)
	}
}

func TestRun_IdempotentSecondCallIsNoop(t *testing.T) {
	relayLLMDir := t.TempDir()
	hostDir := filepath.Join(t.TempDir(), "host")
	writeFile(t, filepath.Join(relayLLMDir, "sessions", "s1.json"), `{"sessionId":"s1"}`)

	if err := migrate.Run(relayLLMDir, hostDir); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Change the source and the destination copy after the first run: a
	// second Run must leave the host copy untouched (marker respected),
	// not re-copy and clobber whatever the host has done with it since.
	if err := os.WriteFile(filepath.Join(relayLLMDir, "sessions", "s1.json"), []byte(`{"sessionId":"s1","tampered":true}`), 0o600); err != nil {
		t.Fatalf("tamper source: %v", err)
	}
	hostCopy := filepath.Join(hostDir, "sessions", "s1.json")
	if err := os.WriteFile(hostCopy, []byte(`{"sessionId":"s1","hostOwned":true}`), 0o600); err != nil {
		t.Fatalf("tamper host copy: %v", err)
	}

	if err := migrate.Run(relayLLMDir, hostDir); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	data, err := os.ReadFile(hostCopy)
	if err != nil {
		t.Fatalf("read host copy: %v", err)
	}
	if string(data) != `{"sessionId":"s1","hostOwned":true}` {
		t.Fatalf("second Run overwrote the host's own copy: %q", data)
	}
}

func TestRun_MissingRelayLLMDataDir_NotAnError(t *testing.T) {
	hostDir := filepath.Join(t.TempDir(), "host")
	missing := filepath.Join(t.TempDir(), "never-existed")

	if err := migrate.Run(missing, hostDir); err != nil {
		t.Fatalf("Run with no relayLLM data dir: %v", err)
	}
	if !migrate.Done(hostDir) {
		t.Fatal("Done() false after Run with nothing to copy")
	}
}
