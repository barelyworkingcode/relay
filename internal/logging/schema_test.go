package logging_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
)

func TestHandlerLinesMatchSchema(t *testing.T) {
	logger, buf := newLogger(map[string]string{logging.EnvLogLevel: "debug"})
	ctx := logging.ContextWithTrace(context.Background(), logging.NewTraceID())
	long := strings.Repeat("é", 800)

	logger.DebugContext(ctx, "d")
	logger.InfoContext(ctx, "i", "op", "frontend.request", "status", "ok", "duration_ms", 12*time.Millisecond, "session_id", "p1")
	logger.WarnContext(ctx, "w", "status", "denied", "error", "http_401")
	logger.ErrorContext(ctx, "e", "error", errors.New("boom"))
	logger.InfoContext(ctx, "collide", "ts", 1, "level", 2, "msg", 3, "service", 4, "trace_id", 5, "status", 404, "op", "BAD", "duration_ms", -3)
	logger.WarnContext(ctx, long, "error", long)
	logger.With("job_id", "j1").WithGroup("g").Info("grouped", "k", "v")
	logger.Info("no trace")

	lines := splitLines(t, buf.String())
	if len(lines) != 8 {
		t.Fatalf("got %d lines, want 8", len(lines))
	}
	for _, l := range lines {
		mustValidate(t, l)
	}
}

func TestSchemaRejectsMalformedLines(t *testing.T) {
	good := `{"ts":"2026-01-02T03:04:05.678Z","level":"info","msg":"m","service":"s","op":"a.b","status":"ok","duration_ms":0,"error":"","trace_id":""`
	cases := map[string]string{
		"missing key":         `{"ts":"2026-01-02T03:04:05.678Z","level":"info","msg":"m","service":"s","op":"a.b","status":"ok","duration_ms":0,"error":""}`,
		"local time":          strings.Replace(good, "678Z", "678+02:00", 1) + "}",
		"fractional duration": strings.Replace(good, `"duration_ms":0`, `"duration_ms":1.5`, 1) + "}",
		"negative duration":   strings.Replace(good, `"duration_ms":0`, `"duration_ms":-1`, 1) + "}",
		"bad op":              strings.Replace(good, `"a.b"`, `"A.b"`, 1) + "}",
		"bad status":          strings.Replace(good, `"ok"`, `"fine"`, 1) + "}",
		"short trace":         strings.Replace(good, `"trace_id":""`, `"trace_id":"abc"`, 1) + "}",
		"empty session_id":    good + `,"session_id":""}`,
		"msg over 500 runes":  strings.Replace(good, `"m"`, `"`+strings.Repeat("é", 501)+`"`, 1) + "}",
		"empty service":       strings.Replace(good, `"service":"s"`, `"service":""`, 1) + "}",
	}
	for name, line := range cases {
		if err := validateLine(t, line); err == nil {
			t.Errorf("%s: line accepted", name)
		}
	}
	if err := validateLine(t, good+"}"); err != nil {
		t.Errorf("good line rejected: %v", err)
	}
}
