package features

import (
	"encoding/json"
	"strings"
	"testing"

	"relaye2e/harness"
)

// A claude turn the provider ended with an API error keeps its code in the
// session_joined history: a code of the allowed shape as is, anything else as
// "unknown", and no error key on a normal turn (docs/session-host.md, "History
// on join").
func TestJoinHistoryCarriesTurnErrorCode(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove,
		Settings: map[string]json.RawMessage{"terminal_templates": json.RawMessage(g1hClaudeTemplate)},
	})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-turn-errors", map[string]any{"allowed_templates": []string{"claude-code"}})
	sid := g1tStartSession(t, i, p.ID, "sonnet", "")

	first := i.WebSocket("/ws", i.Credential("mounter"))
	first.Send(map[string]any{"type": "join_session", "sessionId": sid})
	longCode := strings.Repeat("a", 40)
	prompts := []string{"hello", "!error rate_limit", "!error " + longCode}
	for _, text := range prompts {
		g1tTurn(t, first, sid, text, harness.NewTrace(t))
	}
	first.Close()

	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	type entry struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Error   *string         `json:"error,omitempty"`
	}
	var history []entry
	var joined bool
	for n := 0; n < g1tFrameLimit && !joined; n++ {
		var f struct {
			Type    string  `json:"type"`
			History []entry `json:"history"`
		}
		if err := json.Unmarshal(ws.Next(g1tFrameDeadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.Type == "session_joined" {
			history, joined = f.History, true
		}
	}
	if !joined {
		t.Fatalf("no session_joined in %d frames", g1tFrameLimit)
	}

	var assistants []entry
	for _, e := range history {
		if e.Role == "assistant" {
			assistants = append(assistants, e)
		}
	}
	if len(assistants) != len(prompts) {
		t.Fatalf("want %d assistant entries, got %d; history: %s", len(prompts), len(assistants), mustJSON(history))
	}
	want := []string{"", "rate_limit", "unknown"}
	for n, w := range want {
		got := assistants[n].Error
		if w == "" {
			if got != nil {
				t.Errorf("turn %d: want no error key, got %q; history: %s", n, *got, mustJSON(history))
			}
			continue
		}
		if got == nil || *got != w {
			t.Errorf("turn %d: want error %q, got %v", n, w, got)
		}
	}
}
