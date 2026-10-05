package audit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
)

type auditLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *auditLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// msgLines returns the decoded lines whose msg matches.
func (l *auditLogBuf) msgLines(t *testing.T, msg string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, raw := range strings.Split(l.b.String(), "\n") {
		if raw == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("not JSON: %v: %s", err, raw)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

func captureAuditLogs(t *testing.T) *auditLogBuf {
	t.Helper()
	buf := &auditLogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{
		DefaultService: "relay",
		Getenv:         func(string) string { return "" },
	})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type steppedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *steppedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *steppedClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestAuditRecorder_WriteFailureLogsOnceThenCount(t *testing.T) {
	logs := captureAuditLogs(t)
	clk := &steppedClock{t: time.Unix(1_700_000_000, 0)}
	rec := newTestAudit(t, nil)
	// Installed before the first event, so the writer goroutine sees it.
	rec.writeFailedLog = logging.NewRepeat(time.Minute, clk.now)
	if err := rec.CloseWriterForTest(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		rec.Record(AuditEvent{ID: "e"})
	}
	rec.Flush()
	if n := len(logs.msgLines(t, "audit log write failed")); n != 1 {
		t.Fatalf("4 failed writes logged %d lines, want 1", n)
	}

	clk.advance(time.Minute)
	rec.Record(AuditEvent{ID: "e"})
	rec.Flush()
	lines := logs.msgLines(t, "audit log write failed")
	if len(lines) != 2 || lines[1]["repeats"] != json.Number("3") {
		t.Fatalf("after the interval: %v, want a second line with repeats=3", lines)
	}
}

func TestAuditRecorder_QueueFullLogsOnceThenCountAndKeepsDropCounter(t *testing.T) {
	logs := captureAuditLogs(t)
	clk := &steppedClock{t: time.Unix(1_700_000_000, 0)}
	rec := newTestAudit(t, nil)
	rec.queueFullLog = logging.NewRepeat(time.Minute, clk.now)

	// Wedge the writer, then overflow the queue behind it.
	blocked := make(chan struct{})
	release := make(chan struct{})
	rec.SetSink(func(AuditEvent) {
		close(blocked)
		<-release
	})
	defer func() {
		rec.SetSink(nil)
		close(release)
	}()
	rec.Record(AuditEvent{ID: "wedge"})
	<-blocked
	const overflow = 20
	for i := 0; i < AuditQueueSize+overflow; i++ {
		rec.Record(AuditEvent{ID: "flood"})
	}

	if n := len(logs.msgLines(t, "audit log queue full, dropping events")); n != 1 {
		t.Fatalf("%d drops logged %d lines, want 1", overflow, n)
	}
	if rec.Dropped() != overflow {
		t.Errorf("Dropped() = %d, want %d: the counter must not depend on the limiter", rec.Dropped(), overflow)
	}

	clk.advance(time.Minute)
	rec.Record(AuditEvent{ID: "flood"})
	lines := logs.msgLines(t, "audit log queue full, dropping events")
	if len(lines) != 2 || lines[1]["repeats"] != json.Number("19") {
		t.Fatalf("after the interval: %v, want a second line with repeats=19", lines)
	}
}
