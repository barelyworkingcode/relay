package features

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"relaye2e/harness"
)

// The system message of a tool-search chat must tell the model that the index
// names are skills, that tool_search comes first, and that call_tool never does.
func TestChatToolSearchIndexSaysSkillsAreNotTools(t *testing.T) {
	t.Parallel()
	bulk := harness.Catalogue{}
	for n := 0; n < 4; n++ {
		desc := strings.Repeat("Look up acme records by key and return every field. ", 40)
		bulk.Tools = append(bulk.Tools, json.RawMessage(`{"name":"acme_bulk_`+string(rune('0'+n))+`","description":`+
			fmt.Sprintf("%q", desc)+`,"inputSchema":{"type":"object","properties":{}}}`))
	}
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds,
		Presence:    g1tApprove,
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: bulk}},
		FakeModelHost: &harness.FakeModelHostSpec{
			ID: "acme-models",
			Models: []json.RawMessage{json.RawMessage(
				`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":64}`)},
		},
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, g1tHostDeadline)
	i.WaitModelHost(g1tHostDeadline)
	i.WaitSessionHost(g1tHostDeadline)

	grant := map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}, "allowed_templates": []string{"chat"}, "allowed_models": []string{"fake-echo"}}
	skilled := g1tProject(t, i, "acme-skilled", grant)
	i.MustCLI("project", "regen-skill", "--id", skilled.ID, "--json")
	skills, _ := filepath.Glob(filepath.Join(g1tDir(i, "acme-skilled"), ".claude", "skills", "*", "SKILL.md"))
	if len(skills) == 0 {
		t.Fatalf("regen-skill wrote no skill under the project directory")
	}
	skillName := filepath.Base(filepath.Dir(skills[0]))

	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	sid := g1tStartSession(t, i, skilled.ID, "fake-echo", `{"useRelayTools":true}`)
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	before := g1tCallCount(i)
	ws.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "find acme", "trace_id": harness.NewTrace(t)})
	i.WaitEvent(harness.EventQuery{Key: "chat.turn", Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)

	var system string
	for _, c := range g1tChatPosts(i, before) {
		if c.Method != "POST /v1/chat/completions" {
			continue
		}
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(c.Params, &body) != nil {
			continue
		}
		for _, m := range body.Messages {
			if m.Role != "system" {
				continue
			}
			var s string
			if json.Unmarshal(m.Content, &s) != nil {
				s = string(m.Content)
			}
			system += s + "\n"
		}
	}
	if system == "" {
		t.Fatalf("the model received no system message")
	}
	for _, want := range []struct{ what, text string }{
		{"the statement that the listed names are skills, not tools", "The names below are skills, not tools: never call a skill name."},
		{"the statement that tool_search is the first step", "Your first step for any task that needs a hidden tool is tool_search"},
		{"the rule against call_tool before tool_search", "Never call call_tool before tool_search."},
		{"the skill's index line", "(skill " + skillName + "; search for it, do not call it)"},
	} {
		if !strings.Contains(system, want.text) {
			t.Fatalf("the system message lacks %s (%q):\n%s", want.what, want.text, system)
		}
	}
	for _, line := range strings.Split(system, "\n") {
		if strings.HasPrefix(line, "- "+skillName+":") {
			t.Fatalf("an index line leads with the skill name: %q", line)
		}
	}
}
