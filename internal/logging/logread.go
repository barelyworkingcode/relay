package logging

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// LogFileNames lists the files `relay logs` reads under a logs directory, in
// merge order for equal timestamps: the older generation first.
func LogFileNames() []string {
	return []string{"relay.log.1", "relay.log", "relaysessions.log.1", "relaysessions.log"}
}

// Line is one stored log line with the three fields a filter reads.
type Line struct {
	Raw []byte // as stored, without the newline
	TS  time.Time
	// Key orders merged lines. It equals TS, except that a line with no
	// readable ts borrows the previous line's, so it sorts beside it while TS
	// stays zero and the line fails a --since bound.
	Key     time.Time
	TraceID string
	Event   string
	JSON    bool
}

// ParseLine reads the filterable fields. A line that is not a JSON object
// (a panic, third-party output) has JSON false and no fields.
func ParseLine(raw []byte) Line {
	l := Line{Raw: raw}
	if len(raw) == 0 || raw[0] != '{' {
		return l
	}
	var f struct {
		TS      string `json:"ts"`
		TraceID string `json:"trace_id"`
		Event   string `json:"event"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return l
	}
	l.JSON = true
	l.TraceID, l.Event = f.TraceID, f.Event
	if t, err := time.Parse(time.RFC3339Nano, f.TS); err == nil {
		l.TS, l.Key = t, t
	}
	return l
}

// Filter selects lines. Every set field must match.
type Filter struct {
	TraceID string
	Event   string
	Since   time.Time // zero = no lower bound
}

// Active reports whether any field is set.
func (f Filter) Active() bool {
	return f.TraceID != "" || f.Event != "" || !f.Since.IsZero()
}

// Match reports whether l passes. With no filter every line passes, a
// non-JSON one included; with any filter a non-JSON line never does, and a
// line whose ts cannot be parsed fails a Since bound.
func (f Filter) Match(l Line) bool {
	if !f.Active() {
		return true
	}
	if !l.JSON {
		return false
	}
	if f.TraceID != "" && l.TraceID != f.TraceID {
		return false
	}
	if f.Event != "" && l.Event != f.Event {
		return false
	}
	if !f.Since.IsZero() && (l.TS.IsZero() || l.TS.Before(f.Since)) {
		return false
	}
	return true
}

// ParseSince reads --since: an RFC 3339 time (fraction and zone optional, UTC
// when the zone is missing) or a positive Go duration meaning now minus it.
func ParseSince(s string, now time.Time) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--since duration must be positive, got %q", s)
		}
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("--since %q is not an RFC 3339 time or a positive duration such as 1h", s)
}

// FormatLine renders a line for people. It is not a stable interface; --json
// prints Raw instead.
func FormatLine(l Line) string {
	if !l.JSON {
		return string(l.Raw)
	}
	var m map[string]any
	if json.Unmarshal(l.Raw, &m) != nil {
		return string(l.Raw)
	}
	str := func(k string) string {
		v, _ := m[k].(string)
		return v
	}
	name := str("event")
	if name == "" {
		name = str("op")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %s %s %s", str("ts"), str("level"), str("service"), name, str("status"))
	if d, ok := m["duration_ms"].(float64); ok && d > 0 {
		fmt.Fprintf(&b, " %dms", int64(d))
	}
	if t := str("trace_id"); t != "" {
		fmt.Fprintf(&b, " trace=%s", t)
	}
	if r := str("reason"); r != "" {
		fmt.Fprintf(&b, " reason=%s", r)
	}
	if e := str("error"); e != "" {
		fmt.Fprintf(&b, " error=%q", e)
	}
	if msg := str("msg"); msg != "" && msg != name {
		fmt.Fprintf(&b, " msg=%q", msg)
	}
	for _, k := range sortedKeys(m) {
		switch k {
		case "ts", "level", "msg", "service", "op", "status", "duration_ms", "error", "trace_id", "event", "reason":
			continue
		}
		fmt.Fprintf(&b, " %s=%v", k, m[k])
	}
	return b.String()
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
