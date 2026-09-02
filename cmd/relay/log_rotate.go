package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// One generation of history is kept by default (a ".1" backup), so any
// single log occupies at most ~2x this on disk.
const maxLogBytes = 8 << 20 // 8 MiB

// serviceLogDir returns the directory where rotated logs are stored: relay's
// own log, the audit log, and every managed service's merged stdout+stderr
// all share it.
func serviceLogDir() (string, error) {
	dir := filepath.Join(bridge.ConfigDir(), "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create log directory: %w", err)
	}
	return dir, nil
}

// Safe for concurrent use: slog writes relay's own log from many goroutines,
// while a managed service's merged stdout+stderr arrive on a single copy
// goroutine — both paths go through the same mutex.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	// 1 keeps only "<path>.1" (relay's own log and service logs, where recent
	// output is what matters); the audit log keeps more because history there
	// is the point.
	generations int
	f           *os.File
	size        int64
}

func openRotatingLog(path string) (*rotatingWriter, error) {
	return openRotatingLogSized(path, maxLogBytes)
}

func openRotatingLogSized(path string, maxBytes int64) (*rotatingWriter, error) {
	return openRotatingLogGenerations(path, maxBytes, 1)
}

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

// Caller holds mu (or is the constructor, before the writer is published).
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

// Caller holds mu and has already closed the current file. Renames are
// best-effort: a missing generation just means the log hasn't rotated that
// many times yet, which is not an error worth failing a write for.
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

// The audit log's fail-closed path writes and syncs its intent record before
// the MCP runs, so "written" there has to mean on disk rather than in the
// page cache: the failure this guards against is relay dying with the
// mailbox already read and the record still buffered.
func (w *rotatingWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return os.ErrInvalid
	}
	return w.f.Sync()
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
