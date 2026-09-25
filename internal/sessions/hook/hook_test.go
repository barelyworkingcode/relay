package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/hook"
)

func envFunc(vals map[string]string) func(string) string {
	return func(k string) string { return vals[k] }
}

// TestRun_NoHookSocket_NoOp covers the "not launched under a session host"
// case: exit 0, no dial attempted, no output.
func TestRun_NoHookSocket_NoOp(t *testing.T) {
	var out bytes.Buffer
	code := hook.Run(hook.Deps{
		Stdin:   strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`),
		Stdout:  &out,
		Environ: envFunc(nil),
		Dial: func(ctx context.Context) (net.Conn, error) {
			t.Fatal("should never dial with no hook socket configured")
			return nil, nil
		},
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

// TestRun_InteractiveTool_SkipsRoundTrip: these tools get no round trip and
// no output, leaving the decision to Claude Code.
func TestRun_InteractiveTool_SkipsRoundTrip(t *testing.T) {
	for _, tool := range []string{"ExitPlanMode", "AskUserQuestion", "ToolSearch"} {
		t.Run(tool, func(t *testing.T) {
			var dialed bool
			var out bytes.Buffer
			code := hook.Run(hook.Deps{
				Stdin:   strings.NewReader(`{"tool_name":"` + tool + `","tool_input":{}}`),
				Stdout:  &out,
				Environ: envFunc(map[string]string{hook.EnvHookSocket: "/some/sock", hook.EnvSessionID: "s1"}),
				Dial: func(ctx context.Context) (net.Conn, error) {
					dialed = true
					return nil, errors.New("must not dial for an interactive tool")
				},
			})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if dialed {
				t.Fatal("dialed the host for an interactive tool")
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", out.String())
			}
		})
	}
}

// fakePermissionServer serves POST /permission and hands the test the
// decoded request body it received, so tests can assert on the exact wire
// shape the hook sends.
func fakePermissionServer(t *testing.T, decision, reason string) (dial func(ctx context.Context) (net.Conn, error), received chan map[string]any) {
	t.Helper()
	received = make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/permission" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		received <- body
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision": decision, "reason": reason})
	}))
	t.Cleanup(srv.Close)
	dial = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(srv.URL, "http://"))
	}
	return dial, received
}

// TestRun_FullRoundTrip proves the hook reads Claude's stdin JSON, posts
// today's /api/permission body shape (session id from RELAY_SESSION_ID, no
// Authorization header — C6 admits by membership, not a bearer) to
// /permission, and prints Claude's hookSpecificOutput contract on stdout.
func TestRun_FullRoundTrip(t *testing.T) {
	dial, received := fakePermissionServer(t, "allow", "looks safe")

	var out bytes.Buffer
	code := hook.Run(hook.Deps{
		Stdin:  strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls -la"},"tool_use_id":"tu-1"}`),
		Stdout: &out,
		Environ: envFunc(map[string]string{
			hook.EnvHookSocket: "unused-but-present",
			hook.EnvSessionID:  "sess-42",
		}),
		Dial: dial,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var req map[string]any
	select {
	case req = <-received:
	default:
		t.Fatal("server never received a request")
	}
	if req["sessionId"] != "sess-42" {
		t.Fatalf("sessionId = %v, want sess-42", req["sessionId"])
	}
	if req["toolName"] != "Bash" {
		t.Fatalf("toolName = %v, want Bash", req["toolName"])
	}
	if req["toolUseId"] != "tu-1" {
		t.Fatalf("toolUseId = %v, want tu-1", req["toolUseId"])
	}
	if !strings.Contains(req["toolInput"].(string), "ls -la") {
		t.Fatalf("toolInput = %v, want to contain the raw JSON", req["toolInput"])
	}

	var stdout map[string]any
	if err := json.Unmarshal(out.Bytes(), &stdout); err != nil {
		t.Fatalf("stdout is not valid JSON: %v (%q)", err, out.String())
	}
	hso, ok := stdout["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("stdout missing hookSpecificOutput: %v", stdout)
	}
	if hso["hookEventName"] != "PreToolUse" || hso["permissionDecision"] != "allow" || hso["permissionDecisionReason"] != "looks safe" {
		t.Fatalf("hookSpecificOutput = %v", hso)
	}
}

// TestRun_NoAuthorizationHeader proves the hook never sends a bearer: C6
// admits the call by C3 process-ancestry membership on the accepted
// connection, not a credential the hook process could leak.
func TestRun_NoAuthorizationHeader(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"decision": "deny", "reason": "x"})
	}))
	t.Cleanup(srv.Close)

	code := hook.Run(hook.Deps{
		Stdin:  strings.NewReader(`{"tool_name":"Bash","tool_input":{}}`),
		Stdout: &bytes.Buffer{},
		Environ: envFunc(map[string]string{
			hook.EnvHookSocket: "unused",
			hook.EnvSessionID:  "s1",
		}),
		Dial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(srv.URL, "http://"))
		},
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if sawAuth != "" {
		t.Fatalf("Authorization header = %q, want none", sawAuth)
	}
}

// TestRun_FailureModes_FailClosedWithDeny: any failure after the hook
// socket is configured must print an explicit deny, never exit silently,
// because silence lets Claude Code's own check refuse MCP tools anyway.
func TestRun_FailureModes_FailClosedWithDeny(t *testing.T) {
	const bashStdin = `{"tool_name":"Bash","tool_input":{"command":"ls"},"tool_use_id":"tu-1"}`
	withSession := map[string]string{hook.EnvHookSocket: "unused", hook.EnvSessionID: "s1"}

	cases := []struct {
		name  string
		stdin string
		env   map[string]string
		dial  func(t *testing.T) func(ctx context.Context) (net.Conn, error)
	}{
		{"host answers 403", bashStdin, withSession, statusServer(http.StatusForbidden, "")},
		{"host answers 500", bashStdin, withSession, statusServer(http.StatusInternalServerError, `{"decision":"allow"}`)},
		{"body is not JSON", bashStdin, withSession, statusServer(http.StatusOK, "not json")},
		{"decision is ask", bashStdin, withSession, statusServer(http.StatusOK, `{"decision":"ask","reason":"x"}`)},
		{"decision is empty", bashStdin, withSession, statusServer(http.StatusOK, `{"decision":"","reason":"x"}`)},
		{"dial refused", bashStdin, withSession, func(t *testing.T) func(ctx context.Context) (net.Conn, error) {
			sock := filepath.Join(t.TempDir(), "absent.sock")
			return func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			}
		}},
		{"malformed stdin", "not json", withSession, statusServer(http.StatusOK, `{"decision":"allow","reason":"x"}`)},
		{"empty session id", bashStdin, map[string]string{hook.EnvHookSocket: "unused"}, statusServer(http.StatusOK, `{"decision":"allow","reason":"x"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			code := hook.Run(hook.Deps{
				Stdin:   strings.NewReader(tc.stdin),
				Stdout:  &out,
				Environ: envFunc(tc.env),
				Dial:    tc.dial(t),
			})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			hso := singleDecisionLine(t, out.String())
			if hso["hookEventName"] != "PreToolUse" || hso["permissionDecision"] != "deny" {
				t.Fatalf("hookSpecificOutput = %v, want a PreToolUse deny", hso)
			}
			reason, _ := hso["permissionDecisionReason"].(string)
			if !strings.HasPrefix(reason, "relay-sessions hook: ") {
				t.Fatalf("reason = %q, want prefix %q", reason, "relay-sessions hook: ")
			}
		})
	}
}

// TestRun_HostDeny_PassedThrough: a 200 deny reaches Claude Code verbatim.
func TestRun_HostDeny_PassedThrough(t *testing.T) {
	dial, _ := fakePermissionServer(t, "deny", "Denied by user")
	var out bytes.Buffer
	code := hook.Run(hook.Deps{
		Stdin:   strings.NewReader(`{"tool_name":"mcp__relay__fs_list","tool_input":{}}`),
		Stdout:  &out,
		Environ: envFunc(map[string]string{hook.EnvHookSocket: "unused", hook.EnvSessionID: "s1"}),
		Dial:    dial,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	hso := singleDecisionLine(t, out.String())
	if hso["permissionDecision"] != "deny" || hso["permissionDecisionReason"] != "Denied by user" {
		t.Fatalf("hookSpecificOutput = %v, want the host's deny verbatim", hso)
	}
}

func statusServer(status int, body string) func(t *testing.T) func(ctx context.Context) (net.Conn, error) {
	return func(t *testing.T) func(ctx context.Context) (net.Conn, error) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(srv.URL, "http://"))
		}
	}
}

// singleDecisionLine asserts stdout is exactly one JSON line and returns its
// hookSpecificOutput object.
func singleDecisionLine(t *testing.T, stdout string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if stdout == "" || len(lines) != 1 {
		t.Fatalf("stdout = %q, want exactly one line", stdout)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, stdout)
	}
	hso, ok := decoded["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("stdout missing hookSpecificOutput: %v", decoded)
	}
	return hso
}
