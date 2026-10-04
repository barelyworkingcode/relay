// Package logging implements the relay logging standard (docs/logging-standard.md)
// as a slog.Handler: one JSON object per line carrying nine fixed keys.
package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	EnvLogLevel  = "RELAY_LOG_LEVEL"
	EnvServiceID = "RELAY_SERVICE_ID"
	DebugWindow  = 30 * time.Minute
	MaxTextChars = 500 // runes

	tsLayout = "2006-01-02T15:04:05.000Z"
)

// Options configures a Handler.
type Options struct {
	DefaultService string              // required
	Getenv         func(string) string // nil = os.Getenv; read once inside NewHandler
	Now            func() time.Time    // nil = time.Now; drives the debug window only
}

var opPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// core is the state every clone from WithAttrs/WithGroup shares.
type core struct {
	mu      sync.Mutex // serialises Write so a line is never interleaved
	w       io.Writer
	service string
	base    slog.Level
	invalid bool // RELAY_LOG_LEVEL was set to something unusable

	debug   bool
	until   time.Time
	now     func() time.Time
	expired atomic.Bool
}

type pathAttr struct {
	path []string
	attr slog.Attr
}

// Handler writes the standard line format. Use NewHandler or Install.
type Handler struct {
	c      *core
	attrs  []pathAttr
	groups []string
}

// NewHandler reads the environment once and returns a handler writing to w.
func NewHandler(w io.Writer, opts Options) *Handler {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	c := &core{w: w, service: opts.DefaultService, base: slog.LevelInfo, now: now}
	if id := getenv(EnvServiceID); id != "" {
		c.service = id
	}
	if raw := getenv(EnvLogLevel); raw != "" {
		if lvl, ok := ParseLevel(raw); ok {
			c.base = lvl
		} else {
			c.invalid = true
		}
	}
	if c.base == slog.LevelDebug {
		c.debug = true
		c.until = now().Add(DebugWindow)
	}
	return &Handler{c: c}
}

// Install builds a handler, makes it the slog default, and reports an invalid
// RELAY_LOG_LEVEL with one warn line. The rejected value is not echoed.
func Install(w io.Writer, opts Options) *Handler {
	h := NewHandler(w, opts)
	slog.SetDefault(slog.New(h))
	if h.c.invalid {
		h.c.synthetic("invalid log level, using info", "invalid_level")
	}
	return h
}

// checkWindow switches debug off exactly once, the first time it runs at or
// after the deadline. There is no timer; every Enabled and Handle call checks.
func (c *core) checkWindow() {
	if !c.debug || c.expired.Load() || c.now().Before(c.until) {
		return
	}
	if c.expired.CompareAndSwap(false, true) {
		c.synthetic("debug logging expired, level is now info", "debug_window_expired")
	}
}

func (c *core) minLevel() slog.Level {
	if c.debug && c.expired.Load() {
		return slog.LevelInfo
	}
	return c.base
}

func (c *core) synthetic(msg, code string) {
	h := &Handler{c: c}
	r := slog.NewRecord(time.Now(), slog.LevelWarn, msg, 0)
	r.AddAttrs(slog.String("op", "log.level"), slog.String("status", "error"), slog.String("error", code))
	h.format(context.Background(), r)
}

func (h *Handler) Enabled(_ context.Context, l slog.Level) bool {
	h.c.checkWindow()
	return l >= h.c.minLevel()
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	n := *h
	n.attrs = append([]pathAttr(nil), h.attrs...)
	for _, a := range attrs {
		n.attrs = append(n.attrs, pathAttr{path: h.groups, attr: a})
	}
	return &n
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.groups = append(append([]string(nil), h.groups...), name)
	return &n
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	h.c.checkWindow()
	return h.format(ctx, r)
}

type node struct{ items []item }

type item struct {
	key   string
	val   slog.Value
	child *node
}

func (n *node) at(path []string) *node {
	cur := n
	for _, name := range path {
		var next *node
		for i := range cur.items {
			if cur.items[i].child != nil && cur.items[i].key == name {
				next = cur.items[i].child
				break
			}
		}
		if next == nil {
			next = &node{}
			cur.items = append(cur.items, item{key: name, child: next})
		}
		cur = next
	}
	return cur
}

func (n *node) set(key string, v slog.Value) {
	for i := range n.items {
		if n.items[i].child == nil && n.items[i].key == key {
			n.items[i].val = v
			return
		}
	}
	n.items = append(n.items, item{key: key, val: v})
}

// state collects one line: the four caller-settable standard fields plus the
// remaining attrs as a tree.
type state struct {
	root     node
	op       string
	status   string
	err      string
	duration uint64
	hasOp    bool
	hasStat  bool
	hasErr   bool
	hasDur   bool
}

// Top-level caller keys that would shadow a fixed key.
var shadowing = map[string]bool{"ts": true, "level": true, "msg": true, "service": true, "trace_id": true}

func (s *state) add(path []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	v := a.Value
	if v.Kind() == slog.KindGroup {
		members := v.Group()
		if len(members) == 0 {
			return
		}
		sub := path
		if a.Key != "" {
			key := a.Key
			if len(path) == 0 && (shadowing[key] || isStandardKey(key)) {
				key = "attr_" + key
			}
			sub = append(append([]string(nil), path...), key)
		}
		for _, m := range members {
			s.add(sub, m)
		}
		return
	}
	key := a.Key
	switch key {
	case "op":
		if v.Kind() == slog.KindString && opPattern.MatchString(v.String()) {
			s.op, s.hasOp = v.String(), true
			return
		}
		key = "attr_op"
	case "status":
		if v.Kind() != slog.KindString {
			key = "http_status"
			break
		}
		switch v.String() {
		case "ok", "error", "denied":
			s.status, s.hasStat = v.String(), true
			return
		}
		key = "attr_status"
	case "duration_ms":
		if ms, ok := durationMillis(v); ok {
			s.duration, s.hasDur = ms, true
			return
		}
		key = "attr_duration_ms"
	case "error":
		s.err, s.hasErr = truncate(valueString(v)), true
		return
	default:
		if len(path) == 0 && shadowing[key] {
			key = "attr_" + key
		}
	}
	s.root.at(path).set(key, v)
}

func isStandardKey(k string) bool {
	switch k {
	case "op", "status", "duration_ms", "error":
		return true
	}
	return false
}

func durationMillis(v slog.Value) (uint64, bool) {
	switch v.Kind() {
	case slog.KindDuration:
		if d := v.Duration(); d >= 0 {
			return uint64(d.Milliseconds()), true
		}
	case slog.KindInt64:
		if i := v.Int64(); i >= 0 {
			return uint64(i), true
		}
	case slog.KindUint64:
		return v.Uint64(), true
	}
	return 0, false
}

func valueString(v slog.Value) string {
	if v.Kind() == slog.KindAny {
		if e, ok := v.Any().(error); ok {
			return e.Error()
		}
	}
	return v.String()
}

func truncate(s string) string {
	if len(s) <= MaxTextChars || utf8.RuneCountInString(s) <= MaxTextChars {
		return s
	}
	n := 0
	for i := range s {
		if n == MaxTextChars {
			return s[:i]
		}
		n++
	}
	return s
}

func (h *Handler) format(ctx context.Context, r slog.Record) error {
	var s state
	for _, pa := range h.attrs {
		s.add(pa.path, pa.attr)
	}
	r.Attrs(func(a slog.Attr) bool {
		s.add(h.groups, a)
		return true
	})

	if !s.hasOp {
		s.op = "log"
	}
	if !s.hasStat {
		s.status = "ok"
		if r.Level >= slog.LevelWarn {
			s.status = "error"
		}
	}

	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}

	var b bytes.Buffer
	b.WriteString(`{"ts":`)
	writeString(&b, ts.UTC().Format(tsLayout))
	b.WriteString(`,"level":`)
	writeString(&b, levelName(r.Level))
	b.WriteString(`,"msg":`)
	writeString(&b, truncate(r.Message))
	b.WriteString(`,"service":`)
	writeString(&b, h.c.service)
	b.WriteString(`,"op":`)
	writeString(&b, s.op)
	b.WriteString(`,"status":`)
	writeString(&b, s.status)
	b.WriteString(`,"duration_ms":`)
	b.WriteString(strconv.FormatUint(s.duration, 10))
	b.WriteString(`,"error":`)
	writeString(&b, s.err)
	b.WriteString(`,"trace_id":`)
	writeString(&b, TraceFromContext(ctx))
	writeItems(&b, s.root.items, true)
	b.WriteString("}\n")

	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	_, err := h.c.w.Write(b.Bytes())
	return err
}

func writeItems(b *bytes.Buffer, items []item, lead bool) {
	for i, it := range items {
		if lead || i > 0 {
			b.WriteByte(',')
		}
		writeString(b, it.key)
		b.WriteByte(':')
		if it.child != nil {
			b.WriteByte('{')
			writeItems(b, it.child.items, false)
			b.WriteByte('}')
			continue
		}
		writeValue(b, it.val)
	}
}

func writeString(b *bytes.Buffer, s string) {
	enc, _ := json.Marshal(s) // a string cannot fail to marshal
	b.Write(enc)
}

func writeValue(b *bytes.Buffer, v slog.Value) {
	switch v.Kind() {
	case slog.KindString:
		writeString(b, v.String())
	case slog.KindInt64:
		b.WriteString(strconv.FormatInt(v.Int64(), 10))
	case slog.KindUint64:
		b.WriteString(strconv.FormatUint(v.Uint64(), 10))
	case slog.KindBool:
		b.WriteString(strconv.FormatBool(v.Bool()))
	case slog.KindFloat64:
		if f := v.Float64(); math.IsNaN(f) || math.IsInf(f, 0) {
			writeString(b, strconv.FormatFloat(f, 'g', -1, 64))
		} else {
			b.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
		}
	case slog.KindDuration:
		b.WriteString(strconv.FormatInt(int64(v.Duration()), 10))
	case slog.KindTime:
		writeString(b, v.Time().UTC().Format(time.RFC3339Nano))
	default:
		if e, ok := v.Any().(error); ok {
			writeString(b, e.Error())
			return
		}
		enc, err := json.Marshal(v.Any())
		if err != nil || bytes.ContainsAny(enc, "\n") {
			writeString(b, fmt.Sprint(v.Any()))
			return
		}
		b.Write(enc)
	}
}
