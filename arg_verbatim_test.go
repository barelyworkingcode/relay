//go:build !windows

package main

// Relay forwards a caller's tool arguments; it does not re-serialise them
// (ADR-012, issue #40).
//
// The tests that matter here run against the REAL cmd/testmcp stdio child,
// because the property is about what a separate process receives on its stdin
// and no in-process mock can establish that. testmcp answers an unrecognised
// method by echoing the params it was given, so `tools/call` comes back as the
// exact bytes the child parsed off the wire.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// loneSurrogateArgs is issue #40's payload: `\ud800` is a legal JSON escape
// and an ILLEGAL UTF-16 code point on its own, so decoding it into a Go string
// substitutes U+FFFD and there is no way back. fsMCP refuses to write one; the
// bug was that relay silently repaired it first, so the refusal never fired.
const loneSurrogateArgs = `{"file_path":"/d0/modes/surrogate.txt","content":"a\ud800b"}`

// replacementUTF8 is U+FFFD as it appears in a JSON document once a Go string
// carrying it has been re-encoded.
const replacementUTF8 = "�"

// addConn registers any McpConnection under lock — including a real stdio one,
// which addMockConn's *mockMcpConn parameter cannot take.
func addConn(mgr *ExternalMcpManager, id string, conn McpConnection) {
	mgr.mu.Lock()
	mgr.conns[id] = conn
	mgr.mu.Unlock()
}

// echoedArguments pulls the `arguments` member back out of testmcp's echo of
// the tools/call params.
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

// A lone surrogate must arrive at the MCP as the caller wrote it. On main this
// fails with `a�b`: ExternalMcpManager.CallTool decoded the arguments into
// an interface{} and re-encoded them, and encoding/json substitutes U+FFFD for
// an unpaired surrogate on the way in.
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

// The surrogate is one instance of a general property, and the general property
// is the fix: what relay puts on the wire is json.Compact of what the caller
// sent, byte for byte. Every member below is something a decode/re-encode round
// trip silently rewrites.
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

// Invalid JSON is still refused rather than forwarded: relay does not decode
// the arguments, but it does not stop checking that they are arguments.
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

// The whole call path, end to end, with the audit recorder attached: the bytes
// the MCP received and the bytes the audit log recorded must be the same bytes,
// and both must be the caller's.
//
// This is the half of issue #40 that outlives the substitution itself. The
// audit log is the operator's ground truth; on main it recorded the ALREADY
// SUBSTITUTED value, so reconstructing the event from the log put the
// corruption on the client's side of a boundary it had not crossed.
func TestAudit_RecordsTheBytesTheMcpActuallyReceived(t *testing.T) {
	s := makeSettings(map[string]Permission{"fsmcp": PermOn}, nil, nil)
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
	if ev.Outcome != AuditOutcomeOK {
		t.Fatalf("outcome = %q (%s)", ev.Outcome, ev.Error)
	}
	if strings.Contains(string(ev.Args), replacementUTF8) {
		t.Errorf("the audit log recorded a substituted value: %s", ev.Args)
	}
	if string(ev.Args) != onWire {
		t.Errorf("the audit log and the MCP disagree about what was called:\n log  %s\n wire %s", ev.Args, onWire)
	}
}

// ---------------------------------------------------------------------------
// The redactor, directly
// ---------------------------------------------------------------------------

// redactArgs is where the audit's copy of the defect lived. It must now rewrite
// exactly one thing — a credential-like value — and copy the rest through.
func TestRedactArgs_CopiesEverythingItIsNotRedacting(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"lone surrogate survives", `{"content":"a\ud800b"}`, `{"content":"a\ud800b"}`},
		{"key order survives", `{"zebra":1,"apple":2}`, `{"zebra":1,"apple":2}`},
		{"duplicate keys survive", `{"dir":"/safe","dir":"/etc"}`, `{"dir":"/safe","dir":"/etc"}`},
		{"number spelling survives", `{"n":1.0,"big":12345678901234567890}`, `{"n":1.0,"big":12345678901234567890}`},
		{"whitespace is the one rewrite", "{\n  \"a\" : 1\n}", `{"a":1}`},
		{"nested values survive", `{"o":{"content":"x\udc00"},"a":[1,"y\ud800"]}`, `{"o":{"content":"x\udc00"},"a":[1,"y\ud800"]}`},
		{"a bare scalar survives", `"a\ud800b"`, `"a\ud800b"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, size, truncated := redactArgs(json.RawMessage(tc.in), 4096, nil)
			if truncated {
				t.Fatal("unexpectedly truncated")
			}
			if size != len(tc.in) {
				t.Errorf("size = %d, want %d (the size recorded is the size received)", size, len(tc.in))
			}
			if string(got) != tc.want {
				t.Errorf("redactArgs =\n got  %s\n want %s", got, tc.want)
			}
		})
	}
}

// Copying the bytes through must not have cost the redaction, which is the only
// reason this function decodes anything at all.
func TestRedactArgs_StillReplacesCredentialValues(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"top level", `{"api_key":"sk-1","path":"/tmp"}`, `{"api_key":"[redacted]","path":"/tmp"}`},
		{"case and substring", `{"MyPassWord":"hunter2"}`, `{"MyPassWord":"[redacted]"}`},
		{"nested object", `{"cfg":{"token":"t","host":"h"}}`, `{"cfg":{"token":"[redacted]","host":"h"}}`},
		{"inside an array", `{"list":[{"secret":"s"},{"ok":1}]}`, `{"list":[{"secret":"[redacted]"},{"ok":1}]}`},
		{"whole subtree", `{"credentials":{"a":1,"b":2}}`, `{"credentials":"[redacted]"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := redactArgs(json.RawMessage(tc.in), 4096, nil)
			if string(got) != tc.want {
				t.Errorf("redactArgs =\n got  %s\n want %s", got, tc.want)
			}
		})
	}

	// The operator's extra keys still apply.
	got, _, _ := redactArgs(json.RawMessage(`{"mailbox":"INBOX"}`), 4096, []string{"mailbox"})
	if string(got) != `{"mailbox":"[redacted]"}` {
		t.Errorf("extra redact key ignored: %s", got)
	}
}

// The bound is what makes "store the caller's bytes" affordable: arguments are
// unbounded (a file write carries its whole content) and the log is append-only.
// Over the cap the record degrades to a truncated JSON string, and it still
// parses — every line in the log must.
func TestRedactArgs_StaysBoundedAndParseable(t *testing.T) {
	big := `{"content":"` + strings.Repeat("x", 5000) + `"}`
	got, size, truncated := redactArgs(json.RawMessage(big), 128, nil)
	if !truncated {
		t.Fatal("an over-cap payload was not marked truncated")
	}
	if size != len(big) {
		t.Errorf("size = %d, want the full %d: the record says how much was sent, not how much was kept", size, len(big))
	}
	if len(got) > 160 {
		t.Errorf("capped record is %d bytes, well past the 128-byte cap", len(got))
	}
	var s string
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("a truncated record must still be a valid JSON string, got %s: %v", got, err)
	}

	// Arguments that are not JSON at all are still recorded as attempted.
	got, _, _ = redactArgs(json.RawMessage(`{"broken":`), 4096, nil)
	if err := json.Unmarshal(got, &s); err != nil || s != `{"broken":` {
		t.Errorf("malformed arguments = %s, want them recorded verbatim as a JSON string", got)
	}
}

// The one rewrite relay cannot suppress, pinned so it is a measured exception
// rather than an oversight: Go's encoder spells a raw U+2028/U+2029 inside a
// json.RawMessage as `\u2028`/`\u2029` even with SetEscapeHTML(false).
//
// It is not the bug this file exists for. `\u2028` decodes to U+2028 and to
// nothing else, so no receiving MCP can tell the difference and nothing is
// lost — which is exactly the line ADR-012 draws: relay may not change what a
// document MEANS, and does not claim to preserve how it was spelled.
func TestCallTool_UnicodeLineSeparatorsAreReSpelledButNotChanged(t *testing.T) {
	mgr := NewExternalMcpManager(nil)
	addConn(mgr, "peer", newTestMcpConn(t))

	// Written with Go escapes so the two invisible characters are visible here.
	args := json.RawMessage("{\"s\":\"a\u2028b\u2029c\"}")
	res, err := mgr.CallTool(context.Background(), "peer", "probe", args, nil)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	got := echoedArguments(t, res)
	if string(got) != `{"s":"a\u2028b\u2029c"}` {
		t.Errorf("unexpected wire form: %s", got)
	}
	// The property that actually matters: same document either way.
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

// Shapes the byte walk has to get right that a decode/re-encode never had to
// think about: an empty object, an escape inside a KEY, and nesting inside an
// array. Every result must still parse, because docs/audit-log.md promises
// every line of the log does.
func TestRedactArgs_EdgeShapesStillParse(t *testing.T) {
	for _, in := range []string{
		`{}`,
		`[]`,
		`{"a":{}}`,
		`{"a\ud800b":1}`,
		`{"a\"b":1,"c\\":2}`,
		`{"api_ke\u0079A":"s"}`,
		`[[{"token":"t"}],{"n":[1,2]}]`,
		`null`,
		`{"deep":{"deeper":{"deepest":{"password":"p","ok":"o"}}}}`,
	} {
		got, _, truncated := redactArgs(json.RawMessage(in), 4096, nil)
		if truncated {
			t.Errorf("%s: unexpectedly truncated", in)
			continue
		}
		if !json.Valid(got) {
			t.Errorf("%s: produced invalid JSON: %s", in, got)
		}
	}

	// An escape inside a key is compared case-insensitively as the DECODED
	// key, and written back as the bytes it arrived as.
	got, _, _ := redactArgs(json.RawMessage(`{"api_ke\u0079A":"s"}`), 4096, nil)
	if string(got) != `{"api_ke\u0079A":"[redacted]"}` {
		t.Errorf("escaped key = %s, want the key's own bytes with the value redacted", got)
	}
	got, _, _ = redactArgs(json.RawMessage(`{"deep":{"deeper":{"password":"p","ok":"o"}}}`), 4096, nil)
	if string(got) != `{"deep":{"deeper":{"password":"[redacted]","ok":"o"}}}` {
		t.Errorf("deep redaction = %s", got)
	}
}
