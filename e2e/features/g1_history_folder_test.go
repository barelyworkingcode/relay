package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"relaye2e/harness"
)

const g1hClaudeTemplate = `[{"id":"claude-code","name":"Claude Code","command":"claude","sandbox":true,
 "read_write":["~/.claude"],"read":["~/.local/bin"]}]`

// g1hJoinHistory runs one claude turn in a project folder named dirName, then
// joins the session on a fresh /ws and returns the session_joined history.
func g1hJoinHistory(t *testing.T, dirName, text string) (history []struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}) {
	t.Helper()
	// The claude-code template grants ~/.claude read-write, as the shipped
	// template does, so the CLI can write its transcript. The command is found
	// under ~/.local/bin.
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove,
		Settings: map[string]json.RawMessage{"terminal_templates": json.RawMessage(g1hClaudeTemplate)},
	})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, dirName, map[string]any{"allowed_templates": []string{"claude-code"}})
	sid := g1tStartSession(t, i, p.ID, "sonnet", "")

	first := i.WebSocket("/ws", i.Credential("mounter"))
	first.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1tTurn(t, first, sid, text, harness.NewTrace(t))
	first.Close()

	// The transcript exists before the turn's result, so the rejoin needs no wait.
	if _, err := os.Stat(filepath.Join(i.Home, ".claude", "projects")); err != nil {
		t.Fatalf("the fake claude wrote no transcript tree: %v", err)
	}
	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	for n := 0; n < g1tFrameLimit; n++ {
		var f struct {
			Type    string `json:"type"`
			History []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"history"`
		}
		if err := json.Unmarshal(ws.Next(g1tFrameDeadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.Type == "session_joined" {
			return f.History
		}
	}
	t.Fatalf("no session_joined in %d frames", g1tFrameLimit)
	return nil
}

func g1hCheckHistory(t *testing.T, dirName string) {
	t.Helper()
	const text = "hello history"
	hist := g1hJoinHistory(t, dirName, text)
	var gotUser, gotAssistant bool
	for _, e := range hist {
		switch e.Role {
		case "user":
			var s string
			if json.Unmarshal(e.Content, &s) == nil && s == text {
				gotUser = true
			}
		case "assistant":
			var blocks []struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(e.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Text == "echo: "+text {
						gotAssistant = true
					}
				}
			}
		}
	}
	if !gotAssistant {
		t.Fatalf("session_joined history has no assistant entry with the reply; history: %s", mustJSON(hist))
	}
	if !gotUser {
		t.Fatalf("session_joined history has no user entry with the prompt; history: %s", mustJSON(hist))
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A project folder with a space and a dot: Claude CLI writes the transcript
// under a name with every non-alphanumeric replaced by "-", and join_session
// must read that folder (docs/session-host.md, "History on join").
func TestJoinHistoryFolderWithSpaceAndDot(t *testing.T) {
	t.Parallel()
	g1hCheckHistory(t, "Acme Corp.v2")
}

// Control: a folder of letters, digits and hyphens encodes the same either way.
func TestJoinHistoryFolderPlain(t *testing.T) {
	t.Parallel()
	g1hCheckHistory(t, "acme-plain-2")
}
