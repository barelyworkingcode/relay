package main

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

func ptr[T any](v T) *T { return &v }

func dropInLegBase(host string, local bool) dropInLeg {
	const sid, marker = "s1", "verify-n1-done"
	l := dropInLeg{
		Host: host, Marker: marker, DialStatus: http.StatusSwitchingProtocols,
		Create: frontendResponse{Status: http.StatusCreated}, SessionID: sid, JoinSeen: true,
		Message:    frontendResponse{Status: http.StatusOK},
		Frames:     []dropFrame{{Type: "llm_event", SessionID: sid, InitModel: agentModelID}},
		IdleBefore: true, TerminalID: "t1", MarkerSeen: true, ExitSeen: true, IdleAfter: true,
		LogLines: []dropInLogLine{{Status: "ok", Host: host, TerminalID: "t1"}},
		Delete:   frontendResponse{Status: http.StatusNoContent},
	}
	if local {
		l.CheckHeld, l.CheckAudit = true, true
		l.HeldRunning, l.HeldExited, l.HeldFound, l.HeldLive = true, true, true, ptr(false)
		l.HeldList = frontendResponse{Status: http.StatusOK}
		l.EndRow = &audit.AuditEvent{Args: []byte(`{"reason":"closed"}`)}
		l.LaunchRow = &audit.AuditEvent{Outcome: "ok"}
	}
	return l
}

func TestLegProblem(t *testing.T) {
	for _, local := range []bool{true, false} {
		host, name := "loopback-n1", "host"
		if local {
			host, name = "console", "local"
		}
		base := func() dropInLeg { return dropInLegBase(host, local) }
		cases := []mutCase[dropInLeg]{
			{"every promise held", func(*dropInLeg) {}, statePass},
			{"run credential refused on /ws", func(l *dropInLeg) { l.DialStatus, l.DialErr = http.StatusUnauthorized, "bad handshake" }, stateBlocked},
			{"never joined", func(l *dropInLeg) { l.JoinSeen = false }, notPass},
			{"turn timed out", func(l *dropInLeg) { l.Message = frontendResponse{TimedOut: true} }, notPass},
			{"no init event", func(l *dropInLeg) { l.Frames = nil }, notPass},
			{"model not Haiku 4.5", func(l *dropInLeg) { l.Frames[0].InitModel = "claude-haiku-5" }, stateBlocked},
			{"no idle after the turn", func(l *dropInLeg) { l.IdleBefore = false }, notPass},
			{"drop-in refused", func(l *dropInLeg) { l.Refusal = "tool_running" }, notPass},
			{"drop-in refused inside a session", func(l *dropInLeg) { l.Refusal = "inside_session" }, stateBlocked},
			{"marker missing from the terminal", func(l *dropInLeg) { l.MarkerSeen = false }, notPass},
			{"no idle after the terminal ended", func(l *dropInLeg) { l.IdleAfter = false }, notPass},
			{"log unreadable", func(l *dropInLeg) { l.LogErr = "relay log unreadable" }, stateBlocked},
			{"no log line", func(l *dropInLeg) { l.LogLines = nil }, notPass},
			{"two log lines", func(l *dropInLeg) { l.LogLines = append(l.LogLines, l.LogLines[0]) }, notPass},
			{"log line denied", func(l *dropInLeg) { l.LogLines[0].Status, l.LogLines[0].Error = "denied", "dropped_in" }, notPass},
			{"log line names another host", func(l *dropInLeg) { l.LogLines[0].Host = "elsewhere" }, notPass},
			{"log line names another terminal", func(l *dropInLeg) { l.LogLines[0].TerminalID = "t2" }, notPass},
			{"delete failed", func(l *dropInLeg) { l.Delete.Status = http.StatusInternalServerError }, notPass},
		}
		if local {
			cases = append(cases, []mutCase[dropInLeg]{
				{"no running frame at the hold", func(l *dropInLeg) { l.HeldRunning = false }, notPass},
				{"headless process never exited", func(l *dropInLeg) { l.HeldExited = false }, notPass},
				{"row still live", func(l *dropInLeg) { l.HeldLive = ptr(true) }, notPass},
				{"row has no live flag", func(l *dropInLeg) { l.HeldLive = nil }, notPass},
				{"session missing from the list", func(l *dropInLeg) { l.HeldFound = false }, notPass},
				{"exit never came, connection closed", func(l *dropInLeg) { l.ExitSeen, l.ClosedConn = false, true }, statePass},
				{"no session_end row", func(l *dropInLeg) { l.EndRow = nil }, notPass},
				{"session_end reason not closed", func(l *dropInLeg) { l.EndRow.Args = []byte(`{"reason":"exited"}`) }, notPass},
				{"no session_launch row", func(l *dropInLeg) { l.LaunchRow = nil }, notPass},
			}...)
		} else {
			cases = append(cases,
				mutCase[dropInLeg]{"no terminal_exit", func(l *dropInLeg) { l.ExitSeen = false }, notPass},
				mutCase[dropInLeg]{"host launch refused", func(l *dropInLeg) { l.Create, l.SessionID = frontendResponse{Status: http.StatusBadGateway}, "" }, stateBlocked})
		}
		t.Run(name, func(t *testing.T) {
			checkMuts(t, base, func(l dropInLeg) result {
				if p := legProblem(l, local); p != nil {
					return *p
				}
				return result{State: statePass}
			}, cases, nil)
		})
	}
}

func TestClassifyDropIn(t *testing.T) {
	base := func() dropInRun {
		return dropInRun{Project: "Acme", Local: dropInLegBase("console", true), HostRan: true, HostLeg: dropInLegBase("loopback-n1", false)}
	}
	checkMuts(t, base, classifyDropIn, []mutCase[dropInRun]{
		{"both legs pass", func(*dropInRun) {}, statePass},
		{"local leg fails", func(r *dropInRun) { r.Local.MarkerSeen = false }, stateFail},
		{"local failure outranks a host setup problem", func(r *dropInRun) { r.Local.MarkerSeen, r.HostRan, r.HostSetup = false, false, "x" }, stateFail},
		{"loopback host not created", func(r *dropInRun) { r.HostSetup = "POST /api/hosts status 502" }, stateBlocked},
		{"host leg fails", func(r *dropInRun) { r.HostLeg.MarkerSeen = false }, stateFail},
		{"teardown left a host behind", func(r *dropInRun) { r.HostTeardown = "; teardown: DELETE host status 500" }, notPass},
	}, nil)
}

func toolRefusedBase() toolRefusedRun {
	const sid = "s1"
	return toolRefusedRun{
		Project: "Acme", DialStatus: http.StatusSwitchingProtocols,
		Create: frontendResponse{Status: http.StatusCreated}, SessionID: sid, JoinSeen: true,
		Frames:   []dropFrame{{Type: "llm_event", SessionID: sid, InitModel: agentModelID}},
		ToolName: "Bash",
		DropIn: frontendResponse{Status: http.StatusConflict, Error: "tool_running",
			Body: []byte(`{"error":"tool_running","message":"a tool is running (Bash); wait for it to finish"}`)},
		Elapsed: 200 * time.Millisecond, ListStatus: http.StatusOK, Found: true, Live: ptr(true),
		TermsBefore: []string{"a"}, TermsAfter: []string{"a"},
		LogLines: []dropInLogLine{{Status: "denied", Error: "tool_running", Host: "console"}},
		Delete:   frontendResponse{Status: http.StatusNoContent},
	}
}

func TestClassifyToolRefused(t *testing.T) {
	checkMuts(t, toolRefusedBase, classifyToolRefused, []mutCase[toolRefusedRun]{
		{"refused while the tool runs", func(*toolRefusedRun) {}, statePass},
		{"socket unreachable", func(r *toolRefusedRun) { r.DialStatus, r.DialErr = 0, "dial unix" }, stateBlocked},
		{"launch refused", func(r *toolRefusedRun) { r.Create, r.SessionID = frontendResponse{Status: http.StatusForbidden}, "" }, stateBlocked},
		{"no tool call", func(r *toolRefusedRun) { r.ToolName = "" }, stateBlocked},
		{"model not Haiku 4.5", func(r *toolRefusedRun) { r.Frames[0].InitModel = "claude-haiku-5" }, stateBlocked},
		{"drop-in succeeded", func(r *toolRefusedRun) { r.DropIn = frontendResponse{Status: http.StatusCreated} }, notPass},
		{"another refusal code", func(r *toolRefusedRun) { r.DropIn.Error = "turn_timeout" }, notPass},
		{"message does not name the tool", func(r *toolRefusedRun) { r.DropIn.Body = []byte(`{"error":"tool_running","message":"busy"}`) }, notPass},
		{"refusal took 5 s", func(r *toolRefusedRun) { r.Elapsed = 5 * time.Second }, notPass},
		{"agent no longer live", func(r *toolRefusedRun) { r.Live = ptr(false) }, notPass},
		{"a terminal was started", func(r *toolRefusedRun) { r.TermsAfter = []string{"a", "b"} }, notPass},
		{"no log line", func(r *toolRefusedRun) { r.LogLines = nil }, notPass},
		{"log line ok", func(r *toolRefusedRun) { r.LogLines[0].Status, r.LogLines[0].Error = "ok", "" }, notPass},
		{"log line wrong code", func(r *toolRefusedRun) { r.LogLines[0].Error = "dropped_in" }, notPass},
		{"delete failed", func(r *toolRefusedRun) { r.Delete.Status = http.StatusInternalServerError }, notPass},
	}, nil)
}

func TestParseDropFrame(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("hi"))
	cases := []struct {
		name, raw string
		want      dropFrame
	}{
		{"tool_use block finished", `{"type":"llm_event","sessionId":"s1","event":{"type":"assistant","content_block_stop":true,"content_block":{"type":"tool_use","name":"Bash"}}}`,
			dropFrame{Type: "llm_event", SessionID: "s1", ToolName: "Bash"}},
		{"text block finished is no tool", `{"type":"llm_event","sessionId":"s1","event":{"type":"assistant","content_block_stop":true}}`,
			dropFrame{Type: "llm_event", SessionID: "s1"}},
		{"tool_use block start is not finished", `{"type":"llm_event","sessionId":"s1","event":{"type":"assistant","content_block":{"type":"tool_use","name":"Bash"}}}`,
			dropFrame{Type: "llm_event", SessionID: "s1"}},
		{"init", `{"type":"llm_event","sessionId":"s1","event":{"type":"system","subtype":"init","model":"m"}}`,
			dropFrame{Type: "llm_event", SessionID: "s1", InitModel: "m"}},
		{"terminal output", `{"type":"terminal_output","terminalId":"t1","data":"` + enc + `"}`,
			dropFrame{Type: "terminal_output", TerminalID: "t1", Data: []byte("hi")}},
		{"terminal join carries scrollback", `{"type":"terminal_joined","terminalId":"t1","scrollback":"` + enc + `"}`,
			dropFrame{Type: "terminal_joined", TerminalID: "t1", Data: []byte("hi")}},
	}
	for _, c := range cases {
		got := parseDropFrame([]byte(c.raw))
		if got.Type != c.want.Type || got.SessionID != c.want.SessionID || got.InitModel != c.want.InitModel ||
			got.ToolName != c.want.ToolName || got.TerminalID != c.want.TerminalID || string(got.Data) != string(c.want.Data) {
			t.Errorf("%s: parsed %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestTerminalTextStripsEscapesAndKeepsOneTerminal(t *testing.T) {
	fs := []dropFrame{
		{Type: "terminal_joined", TerminalID: "t1", Data: []byte("\x1b[2Jverify-")},
		{Type: "terminal_output", TerminalID: "t2", Data: []byte("other")},
		{Type: "terminal_output", TerminalID: "t1", Data: []byte("\x1b[1;31mn1\x1b[0m-done")},
	}
	if got := terminalText(fs, "t1"); got != "verify-n1-done" {
		t.Fatalf("terminalText = %q", got)
	}
}

func TestParseDropInLog(t *testing.T) {
	raw := []byte(`{"msg":"session drop-in","op":"session.drop_in","status":"ok","error":"","session_id":"s1","host":"console","terminal_id":"t1"}
{"op":"session.drop_in","status":"denied","error":"tool_running","session_id":"s2","host":"console"}
{"op":"session.state","session_id":"s1"}
not json
`)
	got := parseDropInLog(raw, "s1")
	if len(got) != 1 || got[0].Status != "ok" || got[0].Host != "console" || got[0].TerminalID != "t1" {
		t.Fatalf("parseDropInLog = %+v", got)
	}
}

func TestStateSeqOrdersFromTheMark(t *testing.T) {
	st := func(s string) dropFrame { return dropFrame{Type: "session_state", SessionID: "s1", State: s} }
	fs := []dropFrame{st("running"), st("idle"), st("idle"), st("running")}
	if !stateSeq("s1", 0, "running", "idle")(fs) {
		t.Error("running then idle not found from 0")
	}
	if stateSeq("s1", 2, "running", "idle")(fs) {
		t.Error("an idle before the mark counted")
	}
}

func TestFixturesRemovedSweepsDropInFixtures(t *testing.T) {
	hs := []hostEntry{{"1", "loopback-n1", "localhost"}, {"2", "loopback-n2", "10.0.0.5"}, {"3", "mine", "localhost"}}
	if got := blackholeHosts(hs); len(got) != 1 || got[0] != "1" {
		t.Errorf("hosts swept = %v, want only the loopback host targeting localhost", got)
	}
	rs := []grantRecord{{ID: "p1", Name: "Drop-in Host n1"}, {ID: "p2", Name: "Acme Corp"}}
	if got := verifyProjects(rs); len(got) != 1 || got[0] != "p1" {
		t.Errorf("projects swept = %v, want only the drop-in host project", got)
	}
	if !strings.HasPrefix(dropInStateDirName, "grant-") {
		t.Error("the drop-in project folder is outside the grant-* sweep")
	}
}
