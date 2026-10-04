package logging_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
)

// repeatCapture installs the real handler as the default logger. Tests using
// it must not run in parallel.
func repeatCapture(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{
		DefaultService: testService,
		Getenv:         func(string) string { return "" },
	})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func repeatLines(t *testing.T, buf *lockedBuf) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		mustValidate(t, raw)
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("not JSON: %v: %s", err, raw)
		}
		out = append(out, m)
	}
	return out
}

type repeatClock struct{ t time.Time }

func (c *repeatClock) now() time.Time          { return c.t }
func (c *repeatClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestRepeat_FirstLogsSuppressedCountedThenCarriedAsRepeats(t *testing.T) {
	buf := repeatCapture(t)
	clk := &repeatClock{t: time.Unix(1_700_000_000, 0)}
	r := logging.NewRepeat(10*time.Second, clk.now)
	ctx := context.Background()

	if !r.Log(ctx, slog.LevelWarn, "disk failed", slog.String("path", "p1")) {
		t.Fatal("first occurrence was suppressed")
	}
	for i := 0; i < 3; i++ {
		clk.advance(2 * time.Second)
		if r.Log(ctx, slog.LevelWarn, "disk failed") {
			t.Fatalf("occurrence %d inside the interval was written", i+2)
		}
	}
	clk.advance(10 * time.Second)
	if !r.Log(ctx, slog.LevelWarn, "disk failed", slog.String("path", "p1")) {
		t.Fatal("occurrence after the interval was suppressed")
	}

	lines := repeatLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), buf.String())
	}
	if _, has := lines[0]["repeats"]; has {
		t.Errorf("first line carries repeats: %v", lines[0])
	}
	if lines[1]["repeats"] != json.Number("3") {
		t.Errorf("second line repeats = %v, want 3", lines[1]["repeats"])
	}
}

func TestRepeat_NoRepeatsKeyWhenNothingWasSuppressed(t *testing.T) {
	buf := repeatCapture(t)
	clk := &repeatClock{t: time.Unix(1_700_000_000, 0)}
	r := logging.NewRepeat(10*time.Second, clk.now)

	r.Log(context.Background(), slog.LevelWarn, "x")
	clk.advance(11 * time.Second)
	r.Log(context.Background(), slog.LevelWarn, "x")

	for _, m := range repeatLines(t, buf) {
		if _, has := m["repeats"]; has {
			t.Errorf("line has repeats with nothing suppressed: %v", m)
		}
	}
}

func TestRepeat_NonPositiveIntervalMeansOneMinute(t *testing.T) {
	for _, iv := range []time.Duration{0, -time.Second} {
		clk := &repeatClock{t: time.Unix(1_700_000_000, 0)}
		r := logging.NewRepeat(iv, clk.now)
		repeatCapture(t)
		r.Log(context.Background(), slog.LevelWarn, "x")
		clk.advance(59 * time.Second)
		if r.Log(context.Background(), slog.LevelWarn, "x") {
			t.Errorf("interval %v: written at 59s", iv)
		}
		clk.advance(time.Second)
		if !r.Log(context.Background(), slog.LevelWarn, "x") {
			t.Errorf("interval %v: suppressed at 60s", iv)
		}
	}
}

func TestRepeat_LineCarriesTraceFromContext(t *testing.T) {
	buf := repeatCapture(t)
	id := logging.NewTraceID()
	logging.NewRepeat(0, nil).Log(logging.ContextWithTrace(context.Background(), id), slog.LevelWarn, "x")
	lines := repeatLines(t, buf)
	if len(lines) != 1 || lines[0]["trace_id"] != id {
		t.Fatalf("lines = %v, want one with trace_id %s", lines, id)
	}
}

func TestRepeat_ConcurrentCallsWriteOneLine(t *testing.T) {
	buf := repeatCapture(t)
	clk := &repeatClock{t: time.Unix(1_700_000_000, 0)}
	r := logging.NewRepeat(time.Minute, clk.now)

	var wrote atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r.Log(context.Background(), slog.LevelWarn, "x") {
				wrote.Add(1)
			}
		}()
	}
	wg.Wait()
	if wrote.Load() != 1 || len(repeatLines(t, buf)) != 1 {
		t.Fatalf("wrote=%d lines=%d, want 1 and 1", wrote.Load(), len(repeatLines(t, buf)))
	}
}

func TestRepeat_NilReceiverLogsEveryTime(t *testing.T) {
	buf := repeatCapture(t)
	var r *logging.Repeat
	for i := 0; i < 3; i++ {
		if !r.Log(context.Background(), slog.LevelWarn, "x") {
			t.Fatal("nil Repeat suppressed a line")
		}
	}
	if n := len(repeatLines(t, buf)); n != 3 {
		t.Fatalf("lines = %d, want 3", n)
	}
}
