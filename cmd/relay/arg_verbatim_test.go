//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"testing"
)

// `\ud800` is a legal JSON escape and an ILLEGAL UTF-16 code point on its own,
// so decoding it into a Go string substitutes U+FFFD and there is no way back.
const loneSurrogateArgs = `{"file_path":"/d0/modes/surrogate.txt","content":"a\ud800b"}`

const replacementUTF8 = "�"

// addConn accepts any McpConnection, including a real stdio one — addMockConn's
// *mockMcpConn parameter cannot.
func addConn(mgr *ExternalMcpManager, id string, conn McpConnection) {
	mgr.mu.Lock()
	mgr.conns[id] = conn
	mgr.mu.Unlock()
}

func echoedArguments(t *testing.T, result json.RawMessage) json.RawMessage {
	t.Helper()
	var echoed struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(result, &echoed); err != nil {
		t.Fatalf("decode echoed params %q: %v", result, err)
	}
	return echoed.Arguments
}

// A lone surrogate must arrive at the MCP as the caller wrote it: decoding
// arguments into an interface{} and re-encoding them substitutes U+FFFD for an
// unpaired surrogate on the way in.
func TestCallTool_LoneSurrogateReachesTheMcpVerbatim(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	addConn(mgr, "fsmcp", newTestMcpConn(t))

	res, err := mgr.CallTool(context.Background(), "fsmcp", "fs_write",
		json.RawMessage(loneSurrogateArgs), nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	got := string(echoedArguments(t, res))
	if strings.Contains(got, replacementUTF8) {
		t.Errorf("relay substituted U+FFFD for the caller's lone surrogate: %s", got)
	}
	if got != loneSurrogateArgs {
		t.Errorf("arguments reached the MCP modified:\n got  %s\n want %s", got, loneSurrogateArgs)
	}
}

// What relay puts on the wire is json.Compact of what the caller sent, byte
// for byte; every case below is something a decode/re-encode round trip would
// silently rewrite.
func TestCallTool_ArgumentBytesAreForwardedVerbatim(t *testing.T) {
	cases := []struct {
		name string
		args string
		// want is the form expected on the wire; empty means "the bytes sent".
		want string
		why  string
	}{
		{"lone surrogate", `{"content":"a\ud800b"}`, "",
			"decoding into a Go string substitutes U+FFFD"},
		{"key order", `{"zebra":1,"apple":2,"mango":3}`, "",
			"a Go map re-encodes its keys sorted"},
		{"duplicate keys", `{"dir":"/safe","dir":"/etc"}`, "",
			"a Go map keeps only the last of a repeated key, and which one an MCP honours is the MCP's business"},
		{"number spelling", `{"n":1.0,"big":12345678901234567890,"exp":1e2}`, "",
			"float64 round-trips 1.0 as 1, overflows a 20-digit integer, and rewrites 1e2 as 100"},
		{"redundant escapes", `{"s":"A\/b"}`, "",
			"a re-encode collapses \\u0041 to A and \\/ to /"},
		{"html characters", `{"q":"a<b&c>d"}`, "",
			"json.Marshal escapes <, > and & by default; SetEscapeHTML(false) is what stops it"},
		{"insignificant whitespace", "{\n  \"a\" : 1\n}", `{"a":1}`,
			"an encoder compacts, and this is the one difference relay cannot suppress that costs nothing"},
	}

	mgr := NewExternalMcpManager(nil)
	addConn(mgr, "peer", newTestMcpConn(t))

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := mgr.CallTool(context.Background(), "peer", "probe",
				json.RawMessage(tc.args), nil)
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			want := tc.want
			if want == "" {
				want = tc.args
			}
			if got := string(echoedArguments(t, res)); got != want {
				t.Errorf("arguments rewritten in flight (%s):\n got  %s\n want %s", tc.why, got, want)
			}
		})
	}
}

func TestCallTool_StillRefusesArgumentsThatAreNotJSON(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	addConn(mgr, "peer", newTestMcpConn(t))

	_, err := mgr.CallTool(context.Background(), "peer", "probe",
		json.RawMessage(`{"unterminated":`), nil)
	if err == nil {
		t.Fatal("malformed arguments were forwarded instead of refused")
	}
	if !strings.Contains(err.Error(), "invalid tool arguments") {
		t.Errorf("error = %q, want it to name invalid tool arguments", err)
	}
}

func TestAudit_RecordsTheBytesTheMcpActuallyReceived(t *testing.T) {
	s := makeSettings(map[string]config.Permission{"fsmcp": config.PermOn}, nil, nil)
	mgr := NewExternalMcpManager(nil)
	conn := newTestMcpConn(t)
	conn.SetTools(simpleTools("fs_write"))
	addConn(mgr, "fsmcp", conn)

	r := newTestRouter(t, s, mgr)
	rec := newTestAudit(t, nil)
	r.audit = rec

	res, err := r.CallTool(context.Background(), "fs_write",
		json.RawMessage(loneSurrogateArgs), testToken)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	onWire := string(echoedArguments(t, res))
	if onWire != loneSurrogateArgs {
		t.Errorf("the MCP received modified arguments:\n got  %s\n want %s", onWire, loneSurrogateArgs)
	}

	ev := onlyEvent(t, readLoggedEvents(t, rec))
	if ev.Outcome != audit.AuditOutcomeOK {
		t.Fatalf("outcome = %q (%s)", ev.Outcome, ev.Error)
	}
	if strings.Contains(string(ev.Args), replacementUTF8) {
		t.Errorf("the audit log recorded a substituted value: %s", ev.Args)
	}
	if string(ev.Args) != onWire {
		t.Errorf("the audit log and the MCP disagree about what was called:\n log  %s\n wire %s", ev.Args, onWire)
	}
}

func TestCallTool_UnicodeLineSeparatorsAreReSpelledButNotChanged(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	addConn(mgr, "peer", newTestMcpConn(t))

	args := json.RawMessage("{\"s\":\"a\u2028b\u2029c\"}")
	res, err := mgr.CallTool(context.Background(), "peer", "probe", args, nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	got := echoedArguments(t, res)
	if string(got) != `{"s":"a\u2028b\u2029c"}` {
		t.Errorf("unexpected wire form: %s", got)
	}
	var sent, arrived struct {
		S string `json:"s"`
	}
	if err := json.Unmarshal(args, &sent); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &arrived); err != nil {
		t.Fatal(err)
	}
	if sent.S != arrived.S {
		t.Errorf("the re-spelling changed the value: %q != %q", arrived.S, sent.S)
	}
}
