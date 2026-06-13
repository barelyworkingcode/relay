package main

import (
	"os"
	"path/filepath"
	"sync"
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

// TestRotatingWriter_ConcurrentWritesAreSafe backs the "safe for concurrent
// use" claim in the doc comment: slog writes relay.log from many goroutines.
// A small cap forces frequent rotation under contention so the rotate path
// (close → rename → reopen) is exercised concurrently. Run under -race to catch
// a dropped or mis-scoped lock; every Write must also report its full length.
func TestRotatingWriter_ConcurrentWritesAreSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.log")
	w, err := openRotatingLogSized(path, 4096) // small cap → many rotations under load
	if err != nil {
		t.Fatalf("openRotatingLogSized: %v", err)
	}
	defer w.Close()

	const goroutines = 16
	const perGoroutine = 200
	rec := []byte("a representative structured-log line of some length\n")

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*perGoroutine)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				n, err := w.Write(rec)
				if err != nil {
					errs <- err
					return
				}
				if n != len(rec) {
					errs <- os.ErrInvalid
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent write failed: %v", e)
	}
}
