package logging

import (
	"context"
	"log/slog"
	"regexp"
	"sync"
	"time"
)

// Outcome is the result of one operation. It is written as the line's status.
type Outcome string

const (
	OutcomeOK     Outcome = "ok"
	OutcomeError  Outcome = "error"
	OutcomeDenied Outcome = "denied"
)

// EventKeyPattern is the naming rule for event keys: <domain>.<action> or
// <domain>.<object>.<action>.
var EventKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

var reasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// reservedEventKeys can never be set as a per-event field: they are the
// standard keys plus the two event keys, and a field with one of those names
// would shadow them.
var reservedEventKeys = map[string]bool{
	"ts": true, "level": true, "msg": true, "service": true, "op": true,
	"status": true, "duration_ms": true, "error": true, "trace_id": true,
	"event": true, "reason": true,
}

// Event is one operation's log line, written once by End.
type Event struct {
	ctx   context.Context
	key   string
	start time.Time
	attrs []slog.Attr
	quiet bool
	done  bool
	mu    sync.Mutex
}

// BeginEvent starts one operation's clock. An invalid key makes End write an
// error line with no event key and error "invalid_event_key".
func BeginEvent(ctx context.Context, key string) *Event {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Event{ctx: ctx, key: key, start: time.Now()}
}

// Set adds a per-event field (string, bool, int, int64, []string). A standard
// key, event or reason is dropped, as is any other value type: a field is an
// id, a name, a count or a flag, never a structure.
func (e *Event) Set(key string, value any) *Event {
	if e == nil || reservedEventKeys[key] || key == "" {
		return e
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch v := value.(type) {
	case string:
		e.attrs = append(e.attrs, slog.String(key, Truncate(v)))
	case bool:
		e.attrs = append(e.attrs, slog.Bool(key, v))
	case int:
		e.attrs = append(e.attrs, slog.Int(key, v))
	case int64:
		e.attrs = append(e.attrs, slog.Int64(key, v))
	case []string:
		cut := make([]string, len(v))
		for i, s := range v {
			cut[i] = Truncate(s)
		}
		e.attrs = append(e.attrs, slog.Any(key, cut))
	}
	return e
}

// Quiet marks a poll read: End writes nothing when the outcome is ok.
func (e *Event) Quiet() *Event {
	if e != nil {
		e.mu.Lock()
		e.quiet = true
		e.mu.Unlock()
	}
	return e
}

// End writes the line once; later calls do nothing. The line is written
// before End returns, so a caller that ends the event before it answers has
// its line on disk before the answer leaves.
func (e *Event) End(outcome Outcome, reason string, err error) {
	if e == nil {
		return
	}
	// Held for the whole write: End is idempotent under concurrent callers,
	// and exactly one of them writes the line.
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return
	}
	e.done = true

	if outcome != OutcomeOK && outcome != OutcomeDenied && outcome != OutcomeError {
		outcome, reason = OutcomeError, "internal"
	}
	if outcome == OutcomeOK && e.quiet {
		return
	}

	errText := ""
	if err != nil {
		errText = err.Error()
	}
	validKey := EventKeyPattern.MatchString(e.key)
	if !validKey {
		outcome, reason, errText = OutcomeError, "", "invalid_event_key"
	}

	attrs := make([]slog.Attr, 0, len(e.attrs)+7)
	op, msg := e.key, e.key
	if !validKey {
		op, msg = "log.event", "invalid event key"
	}
	attrs = append(attrs,
		slog.String("op", op),
		slog.String("status", string(outcome)),
		slog.Int64("duration_ms", time.Since(e.start).Milliseconds()),
		slog.String("error", errText),
	)
	if validKey {
		attrs = append(attrs, slog.String("event", e.key))
		if outcome != OutcomeOK {
			if !reasonPattern.MatchString(reason) {
				reason = "internal"
			}
			attrs = append(attrs, slog.String("reason", reason))
		}
		attrs = append(attrs, e.attrs...)
	}
	slog.LogAttrs(e.ctx, eventLevel(outcome, reason), msg, attrs...)
}

func eventLevel(outcome Outcome, reason string) slog.Level {
	switch outcome {
	case OutcomeOK:
		return slog.LevelInfo
	case OutcomeDenied:
		return slog.LevelWarn
	}
	switch reason {
	case "invalid", "not_found", "conflict", "cancelled":
		return slog.LevelWarn
	}
	return slog.LevelError
}

// OutcomeForHTTPStatus maps a response status to an outcome and reason:
// 2xx/3xx ok; 401 denied/unauthorized; 403 denied/not_granted; 404
// error/not_found; 409 error/conflict; 429 denied/throttled; 503
// error/unavailable; 504 error/timeout; other 4xx error/invalid; other 5xx
// error/internal.
func OutcomeForHTTPStatus(code int) (Outcome, string) {
	switch {
	case code < 400:
		return OutcomeOK, ""
	case code == 401:
		return OutcomeDenied, "unauthorized"
	case code == 403:
		return OutcomeDenied, "not_granted"
	case code == 404:
		return OutcomeError, "not_found"
	case code == 409:
		return OutcomeError, "conflict"
	case code == 429:
		return OutcomeDenied, "throttled"
	case code == 503:
		return OutcomeError, "unavailable"
	case code == 504:
		return OutcomeError, "timeout"
	case code < 500:
		return OutcomeError, "invalid"
	}
	return OutcomeError, "internal"
}
