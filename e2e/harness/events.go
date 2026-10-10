package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"syscall"
	"time"
)

// Event is one stored log line (docs/events.md).
type Event map[string]any

// Str returns a top-level string field, or "" when absent or not a string.
func (e Event) Str(key string) string {
	s, _ := e[key].(string)
	return s
}

// EventQuery selects event lines. Every set field must match.
type EventQuery struct {
	Key, Trace string
	Since      time.Time
	Fields     map[string]any // exact match on top-level keys, status and reason included
}

func (q EventQuery) logsArgs(follow bool, timeout time.Duration, withEvent bool) []string {
	args := []string{"logs", "--json"}
	if follow {
		args = append(args, "--follow", "--timeout", timeout.String())
	}
	if withEvent && q.Key != "" {
		args = append(args, "--event", q.Key)
	}
	if q.Trace != "" {
		args = append(args, "--trace", q.Trace)
	}
	if !q.Since.IsZero() {
		args = append(args, "--since", q.Since.UTC().Format(time.RFC3339Nano))
	}
	return args
}

func (q EventQuery) matches(e Event) bool {
	if q.Key != "" && e.Str("event") != q.Key {
		return false
	}
	for k, want := range q.Fields {
		if !jsonEqual(e[k], want) {
			return false
		}
	}
	return true
}

// jsonEqual compares two values as JSON, so 3 equals float64(3).
func jsonEqual(got, want any) bool {
	g, err1 := json.Marshal(got)
	w, err2 := json.Marshal(want)
	if err1 != nil || err2 != nil {
		return false
	}
	var gv, wv any
	_ = json.Unmarshal(g, &gv)
	_ = json.Unmarshal(w, &wv)
	return reflect.DeepEqual(gv, wv)
}

func parseEvents(b []byte) []Event {
	var out []Event
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// Events reads the stored lines that match q: `relay logs --json`. Exit 1
// means no match and yields an empty slice.
func (i *Instance) Events(q EventQuery) []Event {
	i.t.Helper()
	r := i.CLI(q.logsArgs(false, 0, true)...)
	switch r.Code {
	case 0:
	case 1:
		return nil
	default:
		i.t.Fatalf("relay logs exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	var out []Event
	for _, e := range parseEvents(r.Stdout) {
		if q.matches(e) {
			out = append(out, e)
		}
	}
	return out
}

// WaitEvent blocks until an event matching q is stored, or fails t at the
// deadline. It follows the log: relay wakes on file changes, not a timer.
func (i *Instance) WaitEvent(q EventQuery, deadline time.Duration) Event {
	t := i.t
	t.Helper()
	if q.Key == "" {
		t.Fatalf("WaitEvent needs EventQuery.Key")
	}
	if len(q.Fields) == 0 {
		// relay's own --follow --event exits 0 on the first match.
		args := q.logsArgs(true, deadline, true)
		p := i.StartCLI(CLIOpts{Deadline: deadline + 30*time.Second}, args...)
		r := p.Wait()
		if r.Code != 0 {
			t.Fatalf("no %s within %v (relay logs exit %d)\nstderr: %s", q.Key, deadline, r.Code, r.Stderr)
		}
		evs := parseEvents(r.Stdout)
		if len(evs) == 0 {
			t.Fatalf("relay logs exited 0 for %s but printed no line", q.Key)
		}
		return evs[0]
	}
	// Field filters are matched here, over the whole stream.
	p := i.StartCLI(CLIOpts{Deadline: deadline + 30*time.Second}, q.logsArgs(true, deadline, false)...)
	for {
		line, ok := p.nextLine(deadline+20*time.Second, "waiting for "+q.Key)
		if !ok {
			t.Fatalf("no %s matching %v within %v", q.Key, q.Fields, deadline)
		}
		var e Event
		if json.Unmarshal(line, &e) != nil || !q.matches(e) {
			continue
		}
		p.Signal(syscall.SIGTERM)
		p.Wait()
		return e
	}
}

// AuditQuery selects audit rows.
type AuditQuery struct {
	Event, Outcome string
	Tail           int // default 1000
}

// Audit returns audit rows, oldest first: `relay audit --json`.
func (i *Instance) Audit(q AuditQuery) []map[string]any {
	i.t.Helper()
	tail := q.Tail
	if tail <= 0 {
		tail = 1000
	}
	args := []string{"audit", "--json", "--tail", strconv.Itoa(tail)}
	if q.Event != "" {
		args = append(args, "--event", q.Event)
	}
	if q.Outcome != "" {
		args = append(args, "--outcome", q.Outcome)
	}
	r := i.CLI(args...)
	if r.Code != 0 && r.Code != 1 {
		i.t.Fatalf("relay audit exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(r.Stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			i.t.Fatalf("relay audit printed a non-JSON line: %v\n%s", err, sc.Text())
		}
		out = append(out, row)
	}
	return out
}
