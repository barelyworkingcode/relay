package provider

import (
	"encoding/json"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// A failed Claude turn must reach the session layer as message_complete with
// {"isError":true}; a good turn carries no data, so no frame shape changes.
func TestClaudeResult_MessageCompleteDataMarksFailedTurns(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // "" = nil data
	}{
		{"success", `{"type":"result","subtype":"success","is_error":false}`, ""},
		{"is_error true", `{"type":"result","subtype":"success","is_error":true}`, `{"isError":true}`},
		{"error subtype", `{"type":"result","subtype":"error_max_turns","is_error":false}`, `{"isError":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []json.RawMessage
			p := NewClaudeProvider(&sessionstypes.Session{Model: "sonnet"}, func(ev string, data json.RawMessage) {
				if ev == "message_complete" {
					got = append(got, data)
				}
			}, ClaudeConfig{}, nil)
			p.processLine(json.RawMessage(tc.line), nil)

			if len(got) != 1 {
				t.Fatalf("message_complete count = %d, want 1", len(got))
			}
			if string(got[0]) != tc.want {
				t.Fatalf("message_complete data = %q, want %q", got[0], tc.want)
			}
		})
	}
}
