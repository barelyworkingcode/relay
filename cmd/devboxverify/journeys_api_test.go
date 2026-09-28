package main

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
)

// toolTable renders a tool list the way relay mcp call --list prints it,
// after the terminal's echo of the command.
func toolTable(tools ...string) string {
	out := "'/opt/relay' mcp call --list\r\n"
	if len(tools) == 0 {
		return out + "no tools available for this token\r\n"
	}
	out += "TOOL                DESCRIPTION\r\n"
	for _, tool := range tools {
		out += tool + "  does a thing\r\n"
	}
	return out + "\r\n2 tools available\r\n"
}

func TestAttachRefusal(t *testing.T) {
	if _, refused := attachRefusal("j1", ""); refused {
		t.Error("an attached session read as refused")
	}
	for _, reason := range []string{"template_unknown", "template_not_allowed", "inside_session", "peer_confined"} {
		res, refused := attachRefusal("j1", reason)
		if !refused || res.State != stateBlocked {
			t.Errorf("attach refused %s = %+v, %v; want BLOCKED", reason, res, refused)
		}
	}
}

func TestClassifyToolsThroughBridge(t *testing.T) {
	cases := []mutCase[toolsRun]{
		{"mail tools listed, mail answers, contacts denied", func(*toolsRun) {}, statePass},
		{"no tools listed", func(r *toolsRun) { r.List.Out = toolTable() }, stateFail},
		{"a tool outside mail_*", func(r *toolsRun) { r.List.Out = toolTable("mail_list_accounts", "contacts_list") }, stateFail},
		{"list failed", func(r *toolsRun) { r.List.Exit = 1 }, notPass},
		{"allowed call failed", func(r *toolsRun) { r.Allowed.Exit = 1 }, stateFail},
		{"denied call answered", func(r *toolsRun) { r.Denied = execOut{Out: "{}", Exit: 0} }, stateFail},
		{"denied call failed without a denial", func(r *toolsRun) { r.Denied.Out = "error: bridge closed" }, notPass},
		{"prefix from the world's tools", func(r *toolsRun) { r.MCP.Tools, r.List.Out = "contacts_*", toolTable("contacts_list") }, statePass},
		{"only one trailing star trimmed", func(r *toolsRun) { r.MCP.Tools = "mail_**" }, stateFail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := toolsRun{
				MCP:     worldMCP{ID: "macmcp", Tools: "mail_*"},
				List:    execOut{Out: toolTable("mail_list_accounts", "mail_send")},
				Allowed: execOut{Out: `{"accounts":["Alice"]}`},
				Denied:  execOut{Out: "error: access denied: MCP 'macmcp' tool 'contacts_list' is not allowed", Exit: 1},
			}
			c.mut(&r)
			checkState(t, classifyToolsThroughBridge(r), c.want)
		})
	}
}

func TestClassifyToolCallAudited(t *testing.T) {
	callRow := func(tool, outcome string) audit.AuditEvent {
		return audit.AuditEvent{Tool: tool, McpID: "macmcp", Outcome: outcome,
			Actor: audit.AuditActor{Kind: audit.AuditActorProjectSession, ProjectID: "p1", SessionID: "s1"}}
	}
	cases := []mutCase[auditedRun]{
		{"one row per call", func(*auditedRun) {}, statePass},
		{"allowed call not recorded", func(r *auditedRun) { r.Rows = r.Rows[1:] }, stateFail},
		{"denied call not recorded", func(r *auditedRun) { r.Rows = r.Rows[:1] }, stateFail},
		{"a call recorded twice", func(r *auditedRun) { r.Rows = append(r.Rows, r.Rows[0]) }, stateFail},
		{"rows carry a phase", func(r *auditedRun) { r.Rows[0].Phase = "intent" }, stateFail},
		{"actor not a project session", func(r *auditedRun) { r.Rows[1].Actor.Kind = "operator" }, stateFail},
		{"rows for another project", func(r *auditedRun) { r.Rows[0].Actor.ProjectID = "p2" }, stateFail},
		{"rows for another session", func(r *auditedRun) { r.Rows[0].Actor.SessionID, r.Rows[1].Actor.SessionID = "s2", "s2" }, stateFail},
		{"rows for another mcp", func(r *auditedRun) { r.Rows[1].McpID = "fsmcp" }, stateFail},
		{"allowed call recorded as denied", func(r *auditedRun) { r.Rows[0].Outcome = audit.AuditOutcomeDenied }, stateFail},
		{"denied call recorded as ok", func(r *auditedRun) { r.Rows[1].Outcome = audit.AuditOutcomeOK }, stateFail},
		{"rows for the world's mcp", func(r *auditedRun) { r.MCP.ID, r.Rows[0].McpID, r.Rows[1].McpID = "fsmcp", "fsmcp", "fsmcp" }, statePass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := auditedRun{SessionID: "s1", AcmeID: "p1", MCP: worldMCP{ID: "macmcp", Tools: "mail_*"}, Rows: []audit.AuditEvent{
				callRow("mail_list_accounts", audit.AuditOutcomeOK), callRow("contacts_list", audit.AuditOutcomeDenied)}}
			c.mut(&r)
			checkState(t, classifyToolCallAudited(r), c.want)
		})
	}
}

func TestExecOutputDropsEchoes(t *testing.T) {
	const line, tag = "'/opt/relay' mcp call --tool testmcp_ping --args '{}'", "0a1b2c3d4e5f"
	markerEcho := `printf '%s%s:%d\n' 'DBVM' '` + tag + `' "$?"`
	cases := []struct{ name, transcript, want string }{
		{"line-editing shell", "$ " + line + "\r\noutput one\r\nlast output\r\n$ " + markerEcho + "\r\n", "output one\r\nlast output\r\n"},
		{"cooked echo", line + "\r\n" + markerEcho + "\r\noutput one\r\nlast output\r\n", "output one\r\nlast output\r\n"},
		{"wrapped echo", "$ '/opt/relay' mcp call --tool test \rmcp_ping --args '{}'\r\nlast output\r\n$ " + markerEcho + "\r\n", "last output\r\n"},
		{"no echo", "first output\r\nlast output\r\n", "first output\r\nlast output\r\n"},
	}
	for _, c := range cases {
		got := execOutput(c.transcript, line, tag)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if lastLine(got) != "last output" {
			t.Errorf("%s: lastLine %q, want the last output line", c.name, lastLine(got))
		}
	}
}
