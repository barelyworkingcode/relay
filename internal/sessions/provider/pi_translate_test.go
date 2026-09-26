package provider

import (
	"encoding/json"
	"strings"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const piUnrecognisedMsg = "pi: unrecognised event type"

type piHandlerCall struct {
	kind string
	data string
}

func recordPiCalls(t *testing.T, sessionID string) (*PiProvider, *[]piHandlerCall) {
	t.Helper()
	calls := &[]piHandlerCall{}
	p := NewPiProvider(&sessionstypes.Session{ID: sessionID, Model: "pi/acme/Chat"}, func(kind string, data json.RawMessage) {
		*calls = append(*calls, piHandlerCall{kind: kind, data: string(data)})
	}, PiConfig{})
	return p, calls
}

type piWarnRecord struct {
	Level   string `json:"level"`
	Msg     string `json:"msg"`
	Session string `json:"session"`
	Type    string `json:"type"`
}

func piUnrecognisedRecords(b *warnBuffer) []piWarnRecord {
	var out []piWarnRecord
	for _, l := range strings.Split(b.all(), "\n") {
		var r piWarnRecord
		if json.Unmarshal([]byte(l), &r) == nil && r.Msg == piUnrecognisedMsg {
			out = append(out, r)
		}
	}
	return out
}

func TestPiAgentSettledIsSwallowed(t *testing.T) {
	logs := captureDebugJSON(t)
	p, calls := recordPiCalls(t, "p1")

	feedPiLines(p, `{"type":"agent_settled"}`)

	if len(*calls) != 0 {
		t.Errorf("agent_settled must reach no handler, got %+v", *calls)
	}
	if recs := piUnrecognisedRecords(logs); len(recs) != 0 {
		t.Errorf("agent_settled must not log %q, got %+v", piUnrecognisedMsg, recs)
	}
}

func TestPiUnknownEventForwardsOriginalBytes(t *testing.T) {
	const line = `{"x": 1, "type": "future_event"}`
	p, calls := recordPiCalls(t, "p1")

	feedPiLines(p, line)

	if len(*calls) != 1 {
		t.Fatalf("want exactly one handler call, got %+v", *calls)
	}
	if got := (*calls)[0]; got.kind != "raw_output" || got.data != line {
		t.Errorf("want raw_output %s, got %s %s", line, got.kind, got.data)
	}
}

func TestPiUnknownEventWarnsOncePerTypeAndForwardsEach(t *testing.T) {
	logs := captureDebugJSON(t)
	p, calls := recordPiCalls(t, "p1")

	feedPiLines(p,
		`{"type":"future_event","x":1}`,
		`{"type":"future_event","x":2}`,
		`{"type":"other_event"}`,
	)

	raw := 0
	for _, c := range *calls {
		if c.kind == "raw_output" {
			raw++
		}
	}
	if raw != 3 {
		t.Errorf("want 3 raw_output calls, got %+v", *calls)
	}
	recs := piUnrecognisedRecords(logs)
	want := []piWarnRecord{
		{Level: "WARN", Msg: piUnrecognisedMsg, Session: "p1", Type: "future_event"},
		{Level: "WARN", Msg: piUnrecognisedMsg, Session: "p1", Type: "other_event"},
	}
	if len(recs) != len(want) {
		t.Fatalf("want %d %q records, got %+v; log:\n%s", len(want), piUnrecognisedMsg, recs, logs.all())
	}
	for i := range want {
		if recs[i] != want[i] {
			t.Errorf("record %d: want %+v, got %+v", i, want[i], recs[i])
		}
	}
}
