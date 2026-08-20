package main

import (
	"fmt"
	"os"
	"sync"
)

// maxLogBytes caps each log file before it rotates. One generation of history
// is kept (a ".1" backup), so any single log occupies at most ~2× this on disk.
// Applies to relay's own log (relay.log) and every managed service's merged
// stdout/stderr (<id>.log) — see serviceLogDir. Keeps disk bounded without an
// external log-rotation dependency.
const maxLogBytes = 8 << 20 // 8 MiB

// rotatingWriter is a minimal size-capped log writer. When a write would push
// the file past maxBytes, the current file is renamed to "<path>.1" and the
// older generations shift down ("<path>.1" → "<path>.2", and so on) until
// generations is reached, at which point the oldest is discarded. Safe for
// concurrent use: slog writes relay's own log from many goroutines, and a
// managed service's merged stdout+stderr arrive on a single copy goroutine.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	// generations is how many rotated backups to retain. 1 keeps only "<path>.1"
	// (relay's own log and service logs, where recent output is what matters);
	// the audit log keeps more because history there is the point.
	generations int
	f           *os.File
	size        int64
}

// openRotatingLog opens (creating/appending) a size-capped log file at path,
// using the default maxLogBytes cap and a single retained generation.
func openRotatingLog(path string) (*rotatingWriter, error) {
	return openRotatingLogSized(path, maxLogBytes)
}

// openRotatingLogSized is openRotatingLog with an explicit cap (used by tests).
func openRotatingLogSized(path string, maxBytes int64) (*rotatingWriter, error) {
	return openRotatingLogGenerations(path, maxBytes, 1)
}

// openRotatingLogGenerations is openRotatingLogSized with an explicit backup
// count. generations below 1 is clamped to 1.
func openRotatingLogGenerations(path string, maxBytes int64, generations int) (*rotatingWriter, error) {
	if generations < 1 {
		generations = 1
	}
	w := &rotatingWriter{path: path, maxBytes: maxBytes, generations: generations}
	if err := w.reopen(); err != nil {
		return nil, err
	}
	return w, nil
}

// reopen opens path for append and records its current size. Caller holds mu
// (or is the constructor, before the writer is published).
func (w *rotatingWriter) reopen() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, info.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Rotate before a write that would exceed the cap — unless the file is
	// empty, so a single oversized record is written rather than looping.
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		w.f.Close()
		w.shiftGenerations()
		if err := w.reopen(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// shiftGenerations ages the backups down by one and moves the current file
// into ".1". The oldest generation is dropped by being overwritten via rename.
// Caller holds mu and has already closed the current file.
//
// Renames are best-effort: a missing generation just means the log hasn't
// rotated that many times yet, which is not an error worth failing a write for.
func (w *rotatingWriter) shiftGenerations() {
	gens := w.generations
	if gens < 1 {
		gens = 1
	}
	for i := gens - 1; i >= 1; i-- {
		_ = os.Rename(
			fmt.Sprintf("%s.%d", w.path, i),
			fmt.Sprintf("%s.%d", w.path, i+1),
		)
	}
	_ = os.Rename(w.path, w.path+".1")
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
