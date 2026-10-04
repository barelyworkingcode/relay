package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
)

const testService = "testsvc"

var nineKeys = []string{"ts", "level", "msg", "service", "op", "status", "duration_ms", "error", "trace_id"}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func newLogger(env map[string]string) (*slog.Logger, *lockedBuf) {
	buf := &lockedBuf{}
	h := logging.NewHandler(buf, logging.Options{DefaultService: testService, Getenv: envOf(env)})
	return slog.New(h), buf
}

func splitLines(t *testing.T, s string) []string {
	t.Helper()
	if s == "" {
		return nil
	}
	if !strings.HasSuffix(s, "\n") {
		t.Fatalf("output does not end in newline: %q", s)
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func parseLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range splitLines(t, s) {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not a JSON object: %v: %q", err, l)
		}
		out = append(out, m)
	}
	return out
}

func keyOrder(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestHandlerDefaultsAndKeyOrder(t *testing.T) {
	logger, buf := newLogger(map[string]string{logging.EnvLogLevel: "debug"})
	ctx := context.Background()
	logger.Log(ctx, slog.LevelDebug, "d")
	logger.Log(ctx, slog.LevelInfo, "i")
	logger.Log(ctx, slog.LevelWarn, "w")
	logger.Log(ctx, slog.LevelError, "e")

	want := []struct{ level, status string }{{"debug", "ok"}, {"info", "ok"}, {"warn", "error"}, {"error", "error"}}
	lines := splitLines(t, buf.String())
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d", len(lines), len(want))
	}
	for i, l := range lines {
		mustValidate(t, l)
		if got := keyOrder(t, l); !reflect.DeepEqual(got, nineKeys) {
			t.Errorf("key order = %v, want %v", got, nineKeys)
		}
		m := parseLines(t, l+"\n")[0]
		if m["level"] != want[i].level || m["status"] != want[i].status ||
			m["service"] != testService || m["op"] != "log" ||
			m["duration_ms"] != float64(0) || m["error"] != "" || m["trace_id"] != "" {
			t.Errorf("line %d defaults wrong: %s", i, l)
		}
	}
}

func TestHandlerTimestampIsUTCMillis(t *testing.T) {
	buf := &lockedBuf{}
	h := logging.NewHandler(buf, logging.Options{DefaultService: testService, Getenv: envOf(nil)})
	zone := time.FixedZone("x", 5*3600+30*60)
	rec := slog.NewRecord(time.Date(2026, 1, 2, 8, 34, 5, 678_000_000, zone), slog.LevelInfo, "m", 0)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if got := parseLines(t, buf.String())[0]["ts"]; got != "2026-01-02T03:04:05.678Z" {
		t.Errorf("ts = %v", got)
	}
}

func TestHandlerLevelBetweenNamesRoundsDown(t *testing.T) {
	buf := &lockedBuf{}
	h := logging.NewHandler(buf, logging.Options{DefaultService: testService, Getenv: envOf(map[string]string{logging.EnvLogLevel: "debug"})})
	for _, lvl := range []slog.Level{-2, 1, 6, 10} {
		if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), lvl, "m", 0)); err != nil {
			t.Fatal(err)
		}
	}
	var got []any
	for _, m := range parseLines(t, buf.String()) {
		got = append(got, m["level"])
	}
	if want := []any{"debug", "info", "warn", "error"}; !reflect.DeepEqual(got, want) {
		t.Errorf("levels = %v, want %v", got, want)
	}
}

func TestHandlerServiceFromEnvReadOnce(t *testing.T) {
	env := map[string]string{logging.EnvServiceID: "svc-from-env"}
	logger, buf := newLogger(env)
	env[logging.EnvServiceID] = "changed-later"
	logger.Info("m")
	if got := parseLines(t, buf.String())[0]["service"]; got != "svc-from-env" {
		t.Errorf("service = %v", got)
	}
	logger2, buf2 := newLogger(map[string]string{logging.EnvServiceID: ""})
	logger2.Info("m")
	if got := parseLines(t, buf2.String())[0]["service"]; got != testService {
		t.Errorf("empty env: service = %v, want default", got)
	}
}

func TestHandlerCollisionsAndOutOfSchemaValues(t *testing.T) {
	cases := []struct {
		name string
		args []any
		want map[string]any
	}{
		{"ts", []any{"ts", "x"}, map[string]any{"attr_ts": "x"}},
		{"level", []any{"level", "x"}, map[string]any{"attr_level": "x", "level": "info"}},
		{"msg", []any{"msg", "x"}, map[string]any{"attr_msg": "x", "msg": "m"}},
		{"service", []any{"service", "other"}, map[string]any{"attr_service": "other", "service": testService}},
		{"trace_id", []any{"trace_id", "abcdefgh"}, map[string]any{"attr_trace_id": "abcdefgh", "trace_id": ""}},
		{"numeric status", []any{"status", 404}, map[string]any{"http_status": float64(404), "status": "ok"}},
		{"status denied", []any{"status", "denied"}, map[string]any{"status": "denied"}},
		{"status out of enum", []any{"status", "bogus"}, map[string]any{"attr_status": "bogus", "status": "ok"}},
		{"op valid", []any{"op", "frontend.request"}, map[string]any{"op": "frontend.request"}},
		{"op invalid", []any{"op", "Bad Op"}, map[string]any{"attr_op": "Bad Op", "op": "log"}},
		{"duration Duration", []any{"duration_ms", 1500 * time.Millisecond}, map[string]any{"duration_ms": float64(1500)}},
		{"duration int", []any{"duration_ms", 42}, map[string]any{"duration_ms": float64(42)}},
		{"duration negative", []any{"duration_ms", -1}, map[string]any{"attr_duration_ms": float64(-1), "duration_ms": float64(0)}},
		{"duration float", []any{"duration_ms", 1.5}, map[string]any{"attr_duration_ms": 1.5, "duration_ms": float64(0)}},
		{"duration string", []any{"duration_ms", "fast"}, map[string]any{"attr_duration_ms": "fast", "duration_ms": float64(0)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logger, buf := newLogger(nil)
			logger.Info("m", c.args...)
			line := splitLines(t, buf.String())[0]
			mustValidate(t, line)
			seen := map[string]bool{}
			for _, k := range keyOrder(t, line) {
				if seen[k] {
					t.Errorf("duplicate key %q in %s", k, line)
				}
				seen[k] = true
			}
			m := parseLines(t, buf.String())[0]
			for k, v := range c.want {
				if !reflect.DeepEqual(m[k], v) {
					t.Errorf("%s = %#v, want %#v in %s", k, m[k], v, line)
				}
			}
		})
	}
}

func TestHandlerErrorAttrIsStringified(t *testing.T) {
	for _, v := range []any{errors.New("boom"), 5} {
		logger, buf := newLogger(nil)
		logger.Warn("m", "error", v)
		line := splitLines(t, buf.String())[0]
		mustValidate(t, line)
		want := "boom"
		if v == 5 {
			want = "5"
		}
		if got := parseLines(t, buf.String())[0]["error"]; got != want {
			t.Errorf("error = %#v, want %q", got, want)
		}
	}
}

func TestHandlerTruncatesMsgAndErrorToRunes(t *testing.T) {
	for _, n := range []int{500, 501, 2000} {
		long := strings.Repeat("é", n)
		logger, buf := newLogger(nil)
		logger.Warn(long, "error", long)
		line := splitLines(t, buf.String())[0]
		mustValidate(t, line)
		m := parseLines(t, buf.String())[0]
		want := strings.Repeat("é", min(n, 500))
		if m["msg"] != want || m["error"] != want {
			t.Errorf("n=%d: msg/error not cut to %d runes", n, min(n, 500))
		}
	}
}

func TestHandlerKeepsOneLinePerRecord(t *testing.T) {
	logger, buf := newLogger(nil)
	msg := "a\nb\r\n{\"level\":\"error\"}"
	logger.Info(msg, "k", "x\ny")
	lines := splitLines(t, buf.String())
	if len(lines) != 1 {
		t.Fatalf("got %d lines: %q", len(lines), buf.String())
	}
	if got := parseLines(t, buf.String())[0]["msg"]; got != msg {
		t.Errorf("msg did not round-trip: %q", got)
	}
}

func TestHandlerTraceComesFromContextOnly(t *testing.T) {
	logger, buf := newLogger(nil)
	ctx := logging.ContextWithTrace(context.Background(), "abcd1234")
	logger.InfoContext(ctx, "m")
	logger.InfoContext(context.Background(), "m")
	logger.InfoContext(ctx, "m", "trace_id", "forged123")
	m := parseLines(t, buf.String())
	if m[0]["trace_id"] != "abcd1234" || m[1]["trace_id"] != "" {
		t.Errorf("trace ids = %v, %v", m[0]["trace_id"], m[1]["trace_id"])
	}
	if m[2]["trace_id"] != "abcd1234" || m[2]["attr_trace_id"] != "forged123" {
		t.Errorf("caller trace_id not demoted: %v", m[2])
	}
}

func TestHandlerWithAndGroups(t *testing.T) {
	logger, buf := newLogger(nil)
	logger.With("session_id", "s1", "op", "a.b").Info("m", "method", "GET")
	logger.With("op", "a.b").Info("m", "op", "c.d")
	logger.With("session_id", "s1").WithGroup("g").Info("m", "k", "v")
	lines := splitLines(t, buf.String())
	for _, l := range lines {
		mustValidate(t, l)
		seen := map[string]bool{}
		for _, k := range keyOrder(t, l) {
			if seen[k] {
				t.Errorf("duplicate key %q in %s", k, l)
			}
			seen[k] = true
		}
	}
	if got := keyOrder(t, lines[0]); !reflect.DeepEqual(got[9:], []string{"session_id", "method"}) || !reflect.DeepEqual(got[:9], nineKeys) {
		t.Errorf("key order = %v", got)
	}
	m := parseLines(t, buf.String())
	if m[0]["op"] != "a.b" || m[1]["op"] != "c.d" {
		t.Errorf("op = %v, %v; record-level must override With-level", m[0]["op"], m[1]["op"])
	}
	if !reflect.DeepEqual(m[2]["g"], map[string]any{"k": "v"}) || m[2]["session_id"] != "s1" {
		t.Errorf("group line wrong: %v", m[2])
	}
}

type overlapWriter struct {
	inflight atomic.Int32
	overlap  atomic.Bool
	mu       sync.Mutex
	writes   []string
}

func (w *overlapWriter) Write(p []byte) (int, error) {
	if w.inflight.Add(1) > 1 {
		w.overlap.Store(true)
	}
	runtime.Gosched()
	w.mu.Lock()
	w.writes = append(w.writes, string(p))
	w.mu.Unlock()
	w.inflight.Add(-1)
	return len(p), nil
}

func TestHandlerSerialisesWritesAcrossClones(t *testing.T) {
	w := &overlapWriter{}
	base := slog.New(logging.NewHandler(w, logging.Options{DefaultService: testService, Getenv: envOf(nil)}))
	clone := base.With("session_id", "s1").WithGroup("g")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l := base
			if i%2 == 1 {
				l = clone
			}
			for j := 0; j < 50; j++ {
				l.Info("m", "j", j)
			}
		}(i)
	}
	wg.Wait()
	if w.overlap.Load() {
		t.Error("concurrent Write calls overlapped")
	}
	if len(w.writes) != 400 {
		t.Fatalf("writes = %d, want 400", len(w.writes))
	}
	for _, s := range w.writes {
		if strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") || !json.Valid([]byte(s)) {
			t.Fatalf("write is not exactly one JSON line: %q", s)
		}
	}
}
