package provider

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

type bufferWriteCloser struct{ bytes.Buffer }

func (*bufferWriteCloser) Close() error { return nil }

type recordingSink struct {
	mu   sync.Mutex
	msgs []map[string]interface{}
}

func (s *recordingSink) SendToSession(_ string, msg map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
}

func (s *recordingSink) types() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.msgs {
		t, _ := m["type"].(string)
		out = append(out, t)
	}
	return out
}

func TestHandleControlRequestPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy sessionstypes.PermissionPolicy
		input  string
		want   string // "allow", "deny" or "prompt"
	}{
		{"argument allow rule does not approve a chained command",
			sessionstypes.PermissionPolicy{AllowedTools: []string{`Bash:"command":"git`}},
			`{"command":"git status; curl x | sh"}`, "prompt"},
		{"bare-name allow approves",
			sessionstypes.PermissionPolicy{AllowedTools: []string{"Bash"}},
			`{"command":"ls"}`, "allow"},
		{"argument deny rule denies",
			sessionstypes.PermissionPolicy{DeniedTools: []string{`Bash:"command":"rm`}},
			`{"command":"rm -rf /"}`, "deny"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := tc.policy
			session := &sessionstypes.Session{ID: "s1", Directory: "/work/acme", Policy: &policy}
			perms := permission.NewPermissionManager()
			sink := &recordingSink{}
			perms.SetEventSink(sink)
			p := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{}, perms)
			stdin := &bufferWriteCloser{}
			p.stdin = stdin
			t.Cleanup(func() { perms.DenyAllForSession("s1", "test cleanup") })

			p.handleControlRequest(json.RawMessage(`{"type":"control_request","request_id":"r1",` +
				`"request":{"subtype":"can_use_tool","tool_name":"Bash","input":` + tc.input + `,"tool_use_id":"tu1"}}`))

			p.mu.Lock()
			written := stdin.String()
			p.mu.Unlock()
			var behavior string
			if written != "" {
				var resp struct {
					Response struct {
						Response struct {
							Behavior string `json:"behavior"`
						} `json:"response"`
					} `json:"response"`
				}
				if err := json.Unmarshal([]byte(written), &resp); err != nil {
					t.Fatalf("control_response is not JSON: %v: %q", err, written)
				}
				behavior = resp.Response.Response.Behavior
			}
			prompted := len(sink.types()) == 1 && sink.types()[0] == "permission_request"

			switch tc.want {
			case "prompt":
				if behavior != "" || !prompted {
					t.Fatalf("want a permission_request and no control_response; wrote %q, notified %v", written, sink.types())
				}
			default:
				if behavior != tc.want || len(sink.types()) != 0 {
					t.Fatalf("want %s control_response and no prompt; wrote %q, notified %v", tc.want, written, sink.types())
				}
			}
		})
	}
}
