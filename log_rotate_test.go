package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

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

// Small cap forces frequent rotation under contention, exercising the rotate
// path (close → rename → reopen) concurrently. Run under -race.
func TestRotatingWriter_ConcurrentWritesAreSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.log")
	w, err := openRotatingLogSized(path, 4096)
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

// Audit retention keeps several generations, unlike relay's own log which keeps
// one. Each rotation must age the backups down rather than clobber ".1".
func TestRotatingWriter_KeepsMultipleGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	w, err := openRotatingLogGenerations(path, 8, 3)
	if err != nil {
		t.Fatalf("openRotatingLogGenerations: %v", err)
	}
	defer w.Close()

	// Each write exceeds the cap, so every write after the first rotates.
	for _, line := range []string{"aaaaaaaaaa\n", "bbbbbbbbbb\n", "cccccccccc\n", "dddddddddd\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}

	// Newest content in the live file, older content aging down .1 → .3.
	want := map[string]string{
		path:        "dddddddddd\n",
		path + ".1": "cccccccccc\n",
		path + ".2": "bbbbbbbbbb\n",
		path + ".3": "aaaaaaaaaa\n",
	}
	for p, contents := range want {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if string(got) != contents {
			t.Errorf("%s = %q, want %q", p, got, contents)
		}
	}

	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Errorf("generation .4 exists despite a retention of 3")
	}
}

func TestRotatingWriter_DefaultKeepsOneGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.log")

	w, err := openRotatingLogSized(path, 8)
	if err != nil {
		t.Fatalf("openRotatingLogSized: %v", err)
	}
	defer w.Close()

	for _, line := range []string{"aaaaaaaaaa\n", "bbbbbbbbbb\n", "cccccccccc\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Error("single-generation writer created a .2 backup")
	}
	got, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got) != "bbbbbbbbbb\n" {
		t.Errorf(".1 = %q, want the previous generation", got)
	}
}
