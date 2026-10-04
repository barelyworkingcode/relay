//go:build !windows

package mcpbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
)

type captureSink struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
}

func (c *captureSink) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *captureSink) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *captureSink) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type slogCapture struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *slogCapture) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *slogCapture) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureSlog(t *testing.T) *slogCapture {
	t.Helper()
	buf := &slogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{DefaultService: "relay"})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func startStderrMcp(t *testing.T, m *Manager, bin string) Connection {
	t.Helper()
	if err := m.Reload(context.Background(), "tm", &config.ExternalMcp{
		ID: "tm", DisplayName: "tm", Transport: "stdio", Command: bin,
	}); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	conn := m.ConnectionForTest("tm")
	if conn == nil {
		t.Fatal("testmcp did not come up")
	}
	return conn
}

func writeStderr(t *testing.T, conn Connection, text string) {
	t.Helper()
	if _, err := conn.SendRequest(context.Background(), "stderr", map[string]string{"text": text}); err != nil {
		t.Fatalf("stderr request: %v", err)
	}
}

func TestStdioMcpStderrLandsInItsOwnLog(t *testing.T) {
	sink := &captureSink{}
	var openedFor string
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	m.SetStderrLog(func(id string) (io.WriteCloser, error) { openedFor = id; return sink, nil })
	conn := startStderrMcp(t, m, buildTestMcpBinary(t))

	writeStderr(t, conn, "first line\nsecond line\n")
	m.Stop("tm") // waits for the child, so the copy is complete

	if openedFor != "tm" {
		t.Errorf("opener called with %q, want tm", openedFor)
	}
	if got := sink.String(); got != "first line\nsecond line\n" {
		t.Errorf("captured stderr = %q", got)
	}
	if !sink.closed {
		t.Error("log sink was not closed when the MCP stopped")
	}
}

func TestStdioMcpStderrOverLongLineIsCutAtTheCap(t *testing.T) {
	sink := &captureSink{}
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	m.SetStderrLog(func(string) (io.WriteCloser, error) { return sink, nil })
	conn := startStderrMcp(t, m, buildTestMcpBinary(t))

	writeStderr(t, conn, strings.Repeat("x", 40000)+"\nafter\n")
	m.Stop("tm")

	lines := strings.Split(strings.TrimSuffix(sink.String(), "\n"), "\n")
	if len(lines) != 2 || lines[1] != "after" {
		t.Fatalf("want the cut line then an intact next line, got %d lines", len(lines))
	}
	if n := len(lines[0]); n != 16<<10 {
		t.Errorf("over-long line kept %d bytes, want 16384", n)
	}
	if !strings.HasSuffix(sink.String(), "\n") {
		t.Error("cut line was not terminated with a newline")
	}
}

func TestStdioMcpStderrNeverInRelayStream(t *testing.T) {
	const canary = "CANARY-SECRET-9f3a"
	out := captureSlog(t)
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	m.SetStderrLog(func(string) (io.WriteCloser, error) { return &captureSink{}, nil })
	conn := startStderrMcp(t, m, buildTestMcpBinary(t))

	writeStderr(t, conn, canary+"\n")
	m.Stop("tm")

	if strings.Contains(out.String(), canary) {
		t.Errorf("child stderr reached relay's own log: %s", out.String())
	}
}

func TestStdioMcpStartsAndWarnsOnceWhenTheStderrLogCannotOpen(t *testing.T) {
	out := captureSlog(t)
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	m.SetStderrLog(func(string) (io.WriteCloser, error) { return nil, errors.New("disk full") })
	conn := startStderrMcp(t, m, buildTestMcpBinary(t))
	writeStderr(t, conn, "still works\n")

	var warns []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) != nil || line["op"] != "mcp.stderr" {
			continue
		}
		warns = append(warns, line)
	}
	if len(warns) != 1 {
		t.Fatalf("want exactly one mcp.stderr line, got %d: %s", len(warns), out.String())
	}
	w := warns[0]
	if w["level"] != "warn" || w["status"] != "error" {
		t.Errorf("level/status = %v/%v, want warn/error", w["level"], w["status"])
	}
	for _, k := range []string{"ts", "level", "msg", "service", "op", "status", "duration_ms", "error", "trace_id"} {
		if _, ok := w[k]; !ok {
			t.Errorf("warn line lacks schema key %q: %v", k, w)
		}
	}
}

func TestStdioMcpWithoutStderrOpenerDiscardsStderr(t *testing.T) {
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	conn := startStderrMcp(t, m, buildTestMcpBinary(t))
	writeStderr(t, conn, strings.Repeat("y", 200000)+"\n")
	if _, err := conn.SendRequest(context.Background(), "echo", map[string]any{"marker": "alive"}); err != nil {
		t.Fatalf("MCP stalled after heavy stderr with no opener: %v", err)
	}
}

type failingSink struct{}

func (failingSink) Write([]byte) (int, error) { return 0, errors.New("sink broken") }
func (failingSink) Close() error              { return nil }

// An error from Write would stop exec's copy and block the child on a full pipe.
func TestStderrLineWriterNeverReturnsAnError(t *testing.T) {
	w := newStderrLineWriter(failingSink{}, 16)
	for _, in := range []string{"short\n", strings.Repeat("z", 100) + "\n", "partial"} {
		if n, err := w.Write([]byte(in)); err != nil || n != len(in) {
			t.Errorf("Write(%q) = %d, %v; want %d, nil", in, n, err, len(in))
		}
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if n, err := w.Write([]byte("late\n")); err != nil || n != 5 {
		t.Errorf("Write after Close = %d, %v", n, err)
	}
}

// A grandchild in its own session keeps the stderr pipe open after relay
// kills the MCP's process group; Stop must still return promptly.
func TestStdioMcpCloseIsBoundedWhenAGrandchildHoldsStderr(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Fatalf("perl is required: %v", err)
	}
	bin := buildTestMcpBinary(t)
	m := NewManager(nil)
	t.Cleanup(m.StopAll)
	m.SetStderrLog(func(string) (io.WriteCloser, error) { return &captureSink{}, nil })
	script := `perl -e 'use POSIX; if (fork()==0) { POSIX::setsid(); exec "sleep", "8" }' ; exec "$0"`
	if err := m.Reload(context.Background(), "tm", &config.ExternalMcp{
		ID: "tm", DisplayName: "tm", Transport: "stdio", Command: "/bin/sh", Args: []string{"-c", script, bin},
	}); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	start := time.Now()
	m.Stop("tm")
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Stop took %v with a grandchild holding stderr; want bounded near 2s", d)
	}
}
