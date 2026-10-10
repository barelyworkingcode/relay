// Package events writes relay-shaped event lines and the audit JSONL.
package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	RelayLog    = "relay.log"
	SessionsLog = "relaysessions.log"
	maxRunes    = 500
)

// Notice tells a follower that a log file grew.
type Notice struct {
	File string `json:"file"`
	Size int64  `json:"size"`
}

// Log appends event lines to logs/relay.log and logs/relaysessions.log.
type Log struct {
	dir   string
	mu    sync.Mutex
	files map[string]*logFile
	subs  map[int]chan Notice
	next  int
}

type logFile struct {
	f    *os.File
	size int64
}

// NewLog creates DIR/logs.
func NewLog(dir string) (*Log, error) {
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		return nil, err
	}
	return &Log{dir: dir, files: map[string]*logFile{}, subs: map[int]chan Notice{}}, nil
}

// Close closes the open files.
func (l *Log) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.files {
		f.f.Close()
	}
	l.files = map[string]*logFile{}
}

// Subscribe returns a channel that gets a notice per append. The channel is
// small and drops when full: a follower rescans every file on any notice, so a
// dropped notice behind a pending one loses nothing.
func (l *Log) Subscribe() (<-chan Notice, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.next
	l.next++
	ch := make(chan Notice, 64)
	l.subs[id] = ch
	return ch, func() {
		l.mu.Lock()
		delete(l.subs, id)
		l.mu.Unlock()
	}
}

func (l *Log) write(name string, line []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lf := l.files[name]
	if lf == nil {
		f, err := os.OpenFile(filepath.Join(l.dir, "logs", name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		st, _ := f.Stat()
		lf = &logFile{f: f}
		if st != nil {
			lf.size = st.Size()
		}
		l.files[name] = lf
	}
	n, _ := lf.f.Write(append(line, '\n'))
	lf.size += int64(n)
	for _, ch := range l.subs {
		select {
		case ch <- Notice{File: name, Size: lf.size}:
		default:
		}
	}
}

// Event is one operation's line, written by End.
type Event struct {
	log    *Log
	key    string
	trace  string
	start  time.Time
	file   string
	svc    string
	debug  bool
	quiet  bool
	fields map[string]any
}

// Opt changes where or how an event is written.
type Opt func(*Event)

// Sessions routes the event to relaysessions.log, as relay-sessions events are.
func Sessions() Opt {
	return func(e *Event) { e.file, e.svc = SessionsLog, "relaysessions" }
}

// Debug writes an ok event at level debug.
func Debug() Opt { return func(e *Event) { e.debug = true } }

// Quiet writes the event only when it is not ok.
func Quiet() Opt { return func(e *Event) { e.quiet = true } }

// Begin starts an event. The trace comes from ctx.
func (l *Log) Begin(ctx context.Context, key string, o ...Opt) *Event {
	e := &Event{log: l, key: key, trace: TraceFrom(ctx), start: time.Now(), file: RelayLog, svc: "relay", fields: map[string]any{}}
	for _, f := range o {
		f(e)
	}
	return e
}

// Set adds a per-event field. Strings are cut to 500 runes.
func (e *Event) Set(k string, v any) *Event {
	if s, ok := v.(string); ok {
		v = cut(s)
	}
	e.fields[k] = v
	return e
}

// End writes the line synchronously: it is on disk before the caller answers.
func (e *Event) End(status, reason string, err error) {
	if status == "ok" && e.quiet {
		return
	}
	level := "info"
	switch {
	case status == "ok" && e.debug:
		level = "debug"
	case status == "denied":
		level = "warn"
	case status == "error":
		level = "error"
		switch reason {
		case "invalid", "not_found", "conflict", "cancelled":
			level = "warn"
		}
	}
	msg := ""
	if err != nil && status != "ok" {
		msg = cut(err.Error())
	}
	line := map[string]any{}
	for k, v := range e.fields {
		line[k] = v
	}
	line["ts"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	line["level"] = level
	line["msg"] = e.key
	line["service"] = e.svc
	line["op"] = e.key
	line["status"] = status
	line["duration_ms"] = time.Since(e.start).Milliseconds()
	line["error"] = msg
	line["trace_id"] = e.trace
	line["event"] = e.key
	if status != "ok" && reason != "" {
		line["reason"] = reason
	}
	b, _ := json.Marshal(line)
	e.log.write(e.file, b)
}

func cut(s string) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes])
}

type traceKey struct{}

var traceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// ValidTrace reports whether id is an acceptable inbound trace id.
func ValidTrace(id string) bool { return traceRe.MatchString(id) }

// NewID returns 32 random lowercase hex characters.
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// NewUUID returns a random version 4 UUID in its 8-4-4-4-12 form.
func NewUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// WithTrace puts a trace id on ctx. An id outside the allowed shape is
// replaced by a fresh one, so a caller cannot forge a log line's trace.
func WithTrace(ctx context.Context, id string) context.Context {
	if !ValidTrace(id) {
		id = NewID()
	}
	return context.WithValue(ctx, traceKey{}, id)
}

// TraceFrom returns the trace on ctx, or "" outside any action.
func TraceFrom(ctx context.Context) string {
	s, _ := ctx.Value(traceKey{}).(string)
	return s
}
