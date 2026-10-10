package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"relaye2e/harness"
)

type g8CallRow struct {
	ID    string `json:"id"`
	Event string `json:"event"`
	Tool  string `json:"tool"`
}

func g8CallsInstance(t *testing.T, settings map[string]json.RawMessage) (*harness.Instance, string) {
	t.Helper()
	i := harness.Start(t, harness.Options{
		Credentials: readOnly,
		Settings:    settings,
		FakeMCPs:    acmeStdioMCP(),
		Presence: map[string]harness.Outcome{
			"project.grant":        harness.OutcomeApprove,
			"project.reveal_token": harness.OutcomeApprove,
		},
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)
	dir := filepath.Join(i.Home, "work", "acme-calls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body, _ := json.Marshal(map[string]any{"name": "acme-calls", "path": dir, "allowed_mcp_ids": []string{"acme-stdio"}})
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	var tok struct {
		Token string `json:"token"`
	}
	i.MustCLI("project", "token", "--id", p.ID, "--json").JSON(t, &tok)
	if tok.Token == "" {
		t.Fatalf("project token printed no token")
	}
	return i, tok.Token
}

func g8CallTool(t *testing.T, i *harness.Instance, token, tool string) {
	t.Helper()
	reply := i.BridgeSend(map[string]any{"type": "CallTool", "name": tool, "arguments": map[string]any{}, "token": token})
	if reply.Type != "Result" {
		t.Fatalf("CallTool %s answered %s (code %d), want Result", tool, reply.Type, reply.Code)
	}
}

func g8GetRows(t *testing.T, i *harness.Instance, path string) []g8CallRow {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", path, nil)
	if resp.Status != 200 {
		t.Fatalf("GET %s answered %d, want 200", path, resp.Status)
	}
	rows := []g8CallRow{}
	resp.JSON(t, &rows)
	return rows
}

func g8IDs(rows []g8CallRow) []string {
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

const g8RecentPath = "/api/audit/recent-tool-calls"

func TestRecentToolCallsSurviveRestart(t *testing.T) {
	t.Parallel()
	i, token := g8CallsInstance(t, nil)
	g8CallTool(t, i, token, "acme_ok")
	g8CallTool(t, i, token, "acme_echo")

	before := g8GetRows(t, i, g8RecentPath)
	if len(before) != 2 || before[0].Tool != "acme_echo" || before[1].Tool != "acme_ok" {
		t.Fatalf("%s lists %+v, want acme_echo then acme_ok", g8RecentPath, before)
	}

	i.Restart()

	// What the panel read before the fix: the in-memory ring, empty after a restart.
	t.Logf("GET /api/audit?event=call_tool after restart: %d rows", len(g8GetRows(t, i, "/api/audit?event=call_tool")))

	after := g8GetRows(t, i, g8RecentPath)
	if !slices.Equal(g8IDs(after), g8IDs(before)) {
		t.Fatalf("after restart %s lists %v, want %v", g8RecentPath, g8IDs(after), g8IDs(before))
	}
	cli := i.Audit(harness.AuditQuery{Event: "call_tool", Tail: 6}) // oldest first
	var want []string
	for _, row := range cli {
		want = append([]string{row["id"].(string)}, want...)
	}
	if !slices.Equal(g8IDs(after), want) {
		t.Fatalf("%s lists %v, relay audit --event call_tool lists (reversed) %v", g8RecentPath, g8IDs(after), want)
	}
}

func TestRecentToolCallsOutlastControlTraffic(t *testing.T) {
	t.Parallel()
	i, token := g8CallsInstance(t, map[string]json.RawMessage{"audit": json.RawMessage(`{"ring_size":4}`)})

	if rows := g8GetRows(t, i, g8RecentPath); len(rows) != 0 {
		t.Fatalf("%s lists %+v before any call, want []", g8RecentPath, rows)
	}
	g8CallTool(t, i, token, "acme_ok")
	g8CallTool(t, i, token, "acme_echo")
	for n := 0; n < 8; n++ {
		if resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/audit/log", nil); resp.Status != 200 {
			t.Fatalf("GET /api/audit/log answered %d, want 200", resp.Status)
		}
	}
	if ring := g8GetRows(t, i, "/api/audit?event=call_tool"); len(ring) != 0 {
		t.Fatalf("precondition: the ring still holds %d call_tool rows; control traffic did not push the calls out", len(ring))
	}

	rows := g8GetRows(t, i, g8RecentPath)
	if len(rows) != 2 || rows[0].Tool != "acme_echo" || rows[1].Tool != "acme_ok" {
		t.Fatalf("%s lists %+v, want acme_echo then acme_ok", g8RecentPath, rows)
	}
}

func TestRecentToolCallsCappedAtSix(t *testing.T) {
	t.Parallel()
	i, token := g8CallsInstance(t, nil)
	for n := 0; n < 7; n++ {
		g8CallTool(t, i, token, "acme_ok")
	}
	g8CallTool(t, i, token, "acme_echo")
	rows := g8GetRows(t, i, g8RecentPath)
	if len(rows) != 6 || rows[0].Tool != "acme_echo" {
		t.Fatalf("%s lists %d rows, first %q, want 6 rows led by acme_echo", g8RecentPath, len(rows), rows[0].Tool)
	}
}
