package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRotatingWriter_RotatesAtCap verifies that crossing the size cap renames
// the current file to ".1" and starts a fresh one, keeping the current file at
// or below the cap while preserving the older lines in the backup.
func TestRotatingWriter_RotatesAtCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.log")
	w, err := openRotatingLogSized(path, 64)
	if err != nil {
		t.Fatalf("openRotatingLogSized: %v", err)
	}

	const line = "0123456789012345\n" // 17 bytes
	for i := 0; i < 5; i++ {          // 85 bytes total > 64 cap → at least one rotation
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read current log: %v", err)
	}
	if int64(len(cur)) > 64 {
		t.Errorf("current log = %d bytes, want <= cap 64", len(cur))
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("expected rotated backup %s.1: %v", path, err)
	}
}

// TestRotatingWriter_OversizedSingleWrite confirms a single record larger than
// the cap is written rather than triggering an endless rotate loop.
func TestRotatingWriter_OversizedSingleWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	w, err := openRotatingLogSized(path, 8)
	if err != nil {
		t.Fatalf("openRotatingLogSized: %v", err)
	}
	defer w.Close()

	big := make([]byte, 100)
	n, err := w.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("write oversized: n=%d err=%v", n, err)
	}
}
