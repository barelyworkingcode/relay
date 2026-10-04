package logging_test

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
		ok   bool
	}{
		{"error", slog.LevelError, true},
		{"warn", slog.LevelWarn, true},
		{"info", slog.LevelInfo, true},
		{"debug", slog.LevelDebug, true},
		{"DEBUG", slog.LevelDebug, true},
		{"  Warn\n", slog.LevelWarn, true},
		{"", 0, false},
		{"warning", 0, false},
		{"trace", 0, false},
	}
	for _, c := range cases {
		got, ok := logging.ParseLevel(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestEnvLevelFiltersOutput(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want []string
	}{
		{nil, []string{"info", "warn", "error"}},
		{map[string]string{logging.EnvLogLevel: ""}, []string{"info", "warn", "error"}},
		{map[string]string{logging.EnvLogLevel: "error"}, []string{"error"}},
		{map[string]string{logging.EnvLogLevel: " WARN "}, []string{"warn", "error"}},
		{map[string]string{logging.EnvLogLevel: "debug"}, []string{"debug", "info", "warn", "error"}},
		{map[string]string{logging.EnvLogLevel: "bogus"}, []string{"info", "warn", "error"}},
	}
	for _, c := range cases {
		logger, buf := newLogger(c.env)
		ctx := context.Background()
		for _, l := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
			logger.Log(ctx, l, "m")
		}
		var got []string
		for _, m := range parseLines(t, buf.String()) {
			got = append(got, m["level"].(string))
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("env %v: levels = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestInstallInvalidLevelWarnsOnceWithoutEchoing(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	buf := &lockedBuf{}
	logging.Install(buf, logging.Options{
		DefaultService: testService,
		Getenv:         envOf(map[string]string{logging.EnvLogLevel: "CANARY-LEVEL"}),
	})
	lines := splitLines(t, buf.String())
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want exactly one warn: %q", len(lines), buf.String())
	}
	mustValidate(t, lines[0])
	m := parseLines(t, buf.String())[0]
	if m["level"] != "warn" || m["op"] != "log.level" || m["status"] != "error" || m["error"] != "invalid_level" {
		t.Errorf("warn line wrong: %s", lines[0])
	}
	if strings.Contains(buf.String(), "CANARY-LEVEL") {
		t.Error("invalid value echoed")
	}
	slog.Debug("hidden")
	slog.Info("shown")
	out := buf.String()
	if strings.Contains(out, "hidden") || !strings.Contains(out, "shown") {
		t.Errorf("default level is not info after invalid value: %q", out)
	}
}

func TestInstallValidLevelWritesNothingAndSetsDefault(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	buf := &lockedBuf{}
	logging.Install(buf, logging.Options{DefaultService: testService, Getenv: envOf(map[string]string{logging.EnvLogLevel: "warn"})})
	if buf.String() != "" {
		t.Fatalf("valid level wrote output: %q", buf.String())
	}
	slog.Info("quiet")
	slog.Warn("loud")
	lines := parseLines(t, buf.String())
	if len(lines) != 1 || lines[0]["msg"] != "loud" {
		t.Errorf("lines = %v", lines)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func expiryWarns(t *testing.T, out string) int {
	t.Helper()
	n := 0
	for _, l := range splitLines(t, out) {
		mustValidate(t, l)
	}
	for _, m := range parseLines(t, out) {
		if m["op"] == "log.level" {
			n++
			if m["level"] != "warn" || m["status"] != "error" || m["error"] != "debug_window_expired" {
				t.Errorf("expiry warn wrong: %v", m)
			}
		}
	}
	return n
}

func TestDebugWindowExpires(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: t0}
	buf := &lockedBuf{}
	h := logging.NewHandler(buf, logging.Options{
		DefaultService: testService,
		Getenv:         envOf(map[string]string{logging.EnvLogLevel: "debug"}),
		Now:            clk.Now,
	})
	logger := slog.New(h)
	ctx := context.Background()

	clk.set(t0.Add(logging.DebugWindow - time.Second))
	logger.Debug("early")
	if !strings.Contains(buf.String(), "early") || expiryWarns(t, buf.String()) != 0 {
		t.Fatalf("at 29:59 debug must be written with no warn: %q", buf.String())
	}

	clk.set(t0.Add(logging.DebugWindow))
	if h.Enabled(ctx, slog.LevelDebug) {
		t.Error("debug still enabled at 30:00")
	}
	logger.Debug("late")
	logger.Info("after")
	clk.set(t0.Add(3 * time.Hour))
	logger.Info("much later")
	out := buf.String()
	if strings.Contains(out, `"msg":"late"`) {
		t.Error("debug line written after the window")
	}
	if !strings.Contains(out, `"msg":"after"`) || !strings.Contains(out, `"msg":"much later"`) {
		t.Errorf("info lines lost: %q", out)
	}
	if n := expiryWarns(t, out); n != 1 {
		t.Errorf("expiry warns = %d, want exactly 1", n)
	}
}

func TestDebugWindowExpiryWarnsOnceUnderConcurrency(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: t0}
	buf := &lockedBuf{}
	logger := slog.New(logging.NewHandler(buf, logging.Options{
		DefaultService: testService,
		Getenv:         envOf(map[string]string{logging.EnvLogLevel: "debug"}),
		Now:            clk.Now,
	}))
	clk.set(t0.Add(logging.DebugWindow))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("m")
			logger.Debug("d")
		}()
	}
	wg.Wait()
	if n := expiryWarns(t, buf.String()); n != 1 {
		t.Errorf("expiry warns = %d, want exactly 1", n)
	}
}

func TestNonDebugLevelsNeverWarnOfExpiry(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: t0}
	buf := &lockedBuf{}
	logger := slog.New(logging.NewHandler(buf, logging.Options{
		DefaultService: testService,
		Getenv:         envOf(map[string]string{logging.EnvLogLevel: "info"}),
		Now:            clk.Now,
	}))
	clk.set(t0.Add(2 * time.Hour))
	logger.Info("m")
	if n := expiryWarns(t, buf.String()); n != 0 {
		t.Errorf("expiry warns = %d, want 0", n)
	}
}
