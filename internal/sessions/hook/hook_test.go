package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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

// TestRun_InteractiveTool_SkipsRoundTrip covers ExitPlanMode/AskUserQuestion:
// no permission round trip at all, matching relayLLM's cmd/hook.
func TestRun_InteractiveTool_SkipsRoundTrip(t *testing.T) {
	for _, tool := range []string{"ExitPlanMode", "AskUserQuestion"} {
		t.Run(tool, func(t *testing.T) {
			var out bytes.Buffer
			code := hook.Run(hook.Deps{
				Stdin:   strings.NewReader(`{"tool_name":"` + tool + `","tool_input":{}}`),
				Stdout:  &out,
				Environ: envFunc(map[string]string{hook.EnvHookSocket: "/some/sock"}),
				Dial: func(ctx context.Context) (net.Conn, error) {
					t.Fatal("must not dial for an interactive tool")
					return nil, nil
				},
			})
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
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

// TestRun_RefusedOrUnreachable_FailsOpen covers every local/network failure
// mode: the hook must exit 0 either way (Claude Code falls back to its own
// checks), matching relayLLM's hook's own fail-open posture — and, per C6,
// a membership refusal on /permission is just another network-shaped
// failure to this client (it never inspects the status code specially).
func TestRun_RefusedOrUnreachable_FailsOpen(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		var out bytes.Buffer
		code := hook.Run(hook.Deps{
			Stdin:  strings.NewReader(`{"tool_name":"Bash","tool_input":{}}`),
			Stdout: &out,
			Environ: envFunc(map[string]string{
				hook.EnvHookSocket: "/no/such/socket",
				hook.EnvSessionID:  "s1",
			}),
			Dial: func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", "/no/such/socket")
			},
		})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		if out.Len() != 0 {
			t.Fatalf("stdout = %q, want empty on failure", out.String())
		}
	})

	t.Run("host refuses (403)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		t.Cleanup(srv.Close)

		var out bytes.Buffer
		code := hook.Run(hook.Deps{
			Stdin:  strings.NewReader(`{"tool_name":"Bash","tool_input":{}}`),
			Stdout: &out,
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
		if out.Len() != 0 {
			t.Fatalf("stdout = %q, want empty on a refusal", out.String())
		}
	})

	t.Run("malformed stdin", func(t *testing.T) {
		var out bytes.Buffer
		code := hook.Run(hook.Deps{
			Stdin:  strings.NewReader(`not json`),
			Stdout: &out,
			Environ: envFunc(map[string]string{
				hook.EnvHookSocket: "unused",
			}),
			Dial: func(ctx context.Context) (net.Conn, error) {
				t.Fatal("must not dial on unparseable stdin")
				return nil, nil
			},
		})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	})
}
