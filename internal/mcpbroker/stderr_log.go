package mcpbroker

import (
	"io"
	"sync"
	"time"
)

// StderrLogOpener opens the sink for one external MCP's captured stderr.
type StderrLogOpener func(id string) (io.WriteCloser, error)

const (
	maxStderrLineBytes = 16 << 10
	// Bounds exec's Wait when a grandchild keeps the stderr pipe open.
	mcpStderrWaitDelay = 2 * time.Second
)

// SetStderrLog installs the opener used for every later stdio spawn.
func (m *Manager) SetStderrLog(open StderrLogOpener) {
	m.mu.Lock()
	m.openStderr = open
	m.mu.Unlock()
}

// stderrLineWriter turns a byte stream into whole-line writes on dst.
type stderrLineWriter struct {
	mu       sync.Mutex
	dst      io.WriteCloser
	max      int
	buf      []byte
	dropping bool // inside an over-long line, discarding up to its newline
	closed   bool
}

// newStderrLineWriter's Write never returns an error: exec's copy goroutine
// stops on the first one and the child would then block on a full pipe.
func newStderrLineWriter(dst io.WriteCloser, max int) io.WriteCloser {
	return &stderrLineWriter{dst: dst, max: max}
}

func (w *stderrLineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return len(p), nil
	}
	rest := p
	for len(rest) > 0 {
		nl := -1
		for i, b := range rest {
			if b == '\n' {
				nl = i
				break
			}
		}
		chunk := rest
		if nl >= 0 {
			chunk = rest[:nl]
		}
		if !w.dropping {
			room := w.max - len(w.buf)
			if len(chunk) > room {
				w.buf = append(w.buf, chunk[:room]...)
				w.dropping = true
			} else {
				w.buf = append(w.buf, chunk...)
			}
		}
		if nl < 0 {
			break
		}
		w.flushLine()
		rest = rest[nl+1:]
	}
	return len(p), nil
}

// flushLine emits the buffered line with its newline; the caller holds w.mu.
func (w *stderrLineWriter) flushLine() {
	w.buf = append(w.buf, '\n')
	_, _ = w.dst.Write(w.buf)
	w.buf = w.buf[:0]
	w.dropping = false
}

func (w *stderrLineWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if len(w.buf) > 0 || w.dropping {
		w.flushLine()
	}
	return w.dst.Close()
}
