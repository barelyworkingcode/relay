package main

import (
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

func ptr[T any](v T) *T { return &v }

const convID = "7b0f5c1e-3a2d-4e8f-9c61-2d4a8b7e0f13"

func dropInLegBase(host string, local bool) dropInLeg {
	const sid, marker = "s1", "verify-n1-done"
	l := dropInLeg{
		ClaudeID: convID,
		Host:     host, Marker: marker, DialStatus: http.StatusSwitchingProtocols,
		Create: frontendResponse{Status: http.StatusCreated}, SessionID: sid, JoinSeen: true,
		Message:    frontendResponse{Status: http.StatusOK},
		Frames:     []dropFrame{{Type: "llm_event", SessionID: sid, InitModel: agentModelID}},
		IdleBefore: true, TerminalID: "t1", MarkerSeen: true, ExitSeen: true, IdleAfter: true,
		LogLines: []dropInLogLine{{Status: "ok", Host: host, TerminalID: "t1"}},
		Delete:   frontendResponse{Status: http.StatusNoContent},
	}
	l.CheckHeld = true
	l.HeldExited, l.HeldFound, l.HeldLive = true, true, ptr(false)
	l.HeldList = frontendResponse{Status: http.StatusOK}
	if !local {
		l.ResumeSeen, l.TranscriptSeen = true, true
	}
	if local {
		l.CheckAudit = true
		l.HeldRunning = true
		l.EndRow = &audit.AuditEvent{Args: []byte(`{"reason":"closed"}`)}
		l.LaunchRow = &audit.AuditEvent{Outcome: "ok"}
	}
	return l
}

func TestLegProblem(t *testing.T) {
	shared := []mutCase[dropInLeg]{
		{"every promise held", func(*dropInLeg) {}, statePass},
		{"run credential refused on /ws", func(l *dropInLeg) { l.DialStatus, l.DialErr = http.StatusUnauthorized, "bad handshake" }, stateBlocked},
		{"never joined", func(l *dropInLeg) { l.JoinSeen = false }, stateFail},
		{"turn timed out", func(l *dropInLeg) { l.Message = frontendResponse{TimedOut: true} }, stateFail},
		{"first turn never ended", func(l *dropInLeg) { l.IdleBefore = false }, stateFail},
		{"drop-in refused", func(l *dropInLeg) { l.Refusal = "tool_running" }, stateFail},
		{"drop-in refused inside a session", func(l *dropInLeg) { l.Refusal = "inside_session" }, stateBlocked},
		{"headless process never exited", func(l *dropInLeg) { l.HeldExited = false }, stateFail},
		{"row still live", func(l *dropInLeg) { l.HeldLive = ptr(true) }, stateFail},
		{"row has no live flag", func(l *dropInLeg) { l.HeldLive = nil }, stateFail},
		{"session missing from the list", func(l *dropInLeg) { l.HeldFound = false }, stateFail},
		{"no idle after the terminal ended", func(l *dropInLeg) { l.IdleAfter = false }, stateFail},
		{"log unreadable", func(l *dropInLeg) { l.LogErr = "relay log unreadable" }, stateBlocked},
		{"no log line", func(l *dropInLeg) { l.LogLines = nil }, stateFail},
		{"two log lines", func(l *dropInLeg) { l.LogLines = append(l.LogLines, l.LogLines[0]) }, stateFail},
		{"log line denied", func(l *dropInLeg) { l.LogLines[0].Status, l.LogLines[0].Error = "denied", "dropped_in" }, stateFail},
		{"log line names another host", func(l *dropInLeg) { l.LogLines[0].Host = "elsewhere" }, stateFail},
		{"log line names another terminal", func(l *dropInLeg) { l.LogLines[0].TerminalID = "t2" }, stateFail},
		{"delete failed", func(l *dropInLeg) { l.Delete.Status = http.StatusInternalServerError }, stateFail},
	}
	check := func(t *testing.T, host string, local bool, cases []mutCase[dropInLeg]) {
		checkMuts(t, func() dropInLeg { return dropInLegBase(host, local) }, func(l dropInLeg) result {
			if p := legProblem(l, local); p != nil {
				return *p
			}
			return result{State: statePass}
		}, cases, nil)
	}
	t.Run("local", func(t *testing.T) {
		check(t, "console", true, append(slices.Clone(shared), []mutCase[dropInLeg]{
			{"no init event", func(l *dropInLeg) { l.Frames = nil }, stateFail},
			{"model not Haiku 5.5", func(l *dropInLeg) { l.Frames[0].InitModel = "claude-haiku-5" }, stateBlocked},
			{"marker missing from the terminal", func(l *dropInLeg) { l.MarkerSeen = false }, stateFail},
			{"no running frame at the hold", func(l *dropInLeg) { l.HeldRunning = false }, stateFail},
			{"exit never came, connection closed", func(l *dropInLeg) { l.ExitSeen, l.ClosedConn = false, true }, statePass},
			{"no session_end row", func(l *dropInLeg) { l.EndRow = nil }, stateFail},
			{"session_end reason not closed", func(l *dropInLeg) { l.EndRow.Args = []byte(`{"reason":"exited"}`) }, stateFail},
			{"no session_launch row", func(l *dropInLeg) { l.LaunchRow = nil }, stateFail},
		}...))
	})
	t.Run("host", func(t *testing.T) {
		check(t, "loopback-n1", false, append(slices.Clone(shared), []mutCase[dropInLeg]{
			{"no handoff after the first turn", func(l *dropInLeg) { l.Refusal = "status 409 turn_failed" }, stateFail},
			{"first turn ended without init and not errored", func(l *dropInLeg) { l.Frames = nil }, stateFail},
			{"model not Haiku 5.5", func(l *dropInLeg) { l.Frames[0].InitModel = "claude-haiku-5" }, stateBlocked},
			{"message route answered an error", func(l *dropInLeg) { l.Message = frontendResponse{Status: http.StatusBadGateway} }, notPass},
			{"claudeSessionId empty", func(l *dropInLeg) { l.ClaudeID = "" }, stateFail},
			{"claudeSessionId not a UUID", func(l *dropInLeg) { l.ClaudeID = "s1" }, stateFail},
			{"no claude --resume on the host", func(l *dropInLeg) { l.ResumeSeen = false }, stateFail},
			{"host transcript for the id lacks the first prompt", func(l *dropInLeg) { l.TranscriptSeen = false }, stateFail},
			{"terminal never ended", func(l *dropInLeg) { l.ExitSeen = false }, stateFail},
			{"host launch refused", func(l *dropInLeg) { l.Create, l.SessionID = frontendResponse{Status: http.StatusBadGateway}, "" }, stateBlocked},
		}...))
	})
}

func TestFirstTurnEnded(t *testing.T) {
	st := func(s string) dropFrame { return dropFrame{Type: "session_state", SessionID: "s1", State: s} }
	cases := []struct {
		name                   string
		fs                     []dropFrame
		host                   bool
		wantEnded, wantErrored bool
	}{
		{"local idle", []dropFrame{st("running"), st("idle")}, false, true, false},
		{"host idle", []dropFrame{st("running"), st("idle")}, true, true, false},
		{"host errored", []dropFrame{st("running"), st("errored")}, true, true, true},
		{"local errored is no end", []dropFrame{st("running"), st("errored")}, false, false, false},
		{"still running", []dropFrame{st("running")}, true, false, false},
		{"another session's idle", []dropFrame{st("running"), {Type: "session_state", SessionID: "s2", State: "idle"}}, true, false, false},
	}
	for _, c := range cases {
		ended, errored := firstTurnEnded(c.fs, "s1", c.host)
		if ended != c.wantEnded || errored != c.wantErrored {
			t.Errorf("%s: firstTurnEnded = %v, %v, want %v, %v", c.name, ended, errored, c.wantEnded, c.wantErrored)
		}
	}
}

func TestResumeLaunched(t *testing.T) {
	const id = convID
	cases := []struct {
		name, ps string
		want     bool
	}{
		{"resume of the id", "  /bin/zsh\n/opt/bin/claude --model haiku --resume " + id + "\n", true},
		{"another id", "/opt/bin/claude --resume 00000000-0000-4000-8000-000000000000\n", false},
		{"id without resume", "/opt/bin/claude --session-id " + id + "\n", false},
		{"resume of the id by another program", "/usr/bin/other --resume " + id + "\n", false},
		{"an ssh client carrying the command", "ssh -tt localhost cd /w && claude --resume " + id + "\n", false},
		{"headless relaunch with --print", "/opt/bin/claude --print --resume " + id + "\n", false},
		{"headless relaunch with -p", "/opt/bin/claude -p hi --resume " + id + "\n", false},
		{"empty table", "", false},
	}
	for _, c := range cases {
		if got := resumeLaunched(c.ps, id); got != c.want {
			t.Errorf("%s: resumeLaunched = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestClassifyDropIn(t *testing.T) {
	base := func() dropInRun { return dropInRun{Project: "Acme", Local: dropInLegBase("console", true)} }
	checkMuts(t, base, classifyDropIn, []mutCase[dropInRun]{
		{"local leg passes", func(*dropInRun) {}, statePass},
		{"local leg fails", func(r *dropInRun) { r.Local.MarkerSeen = false }, stateFail},
	}, nil)
}

func TestClassifyDropInHost(t *testing.T) {
	base := func() dropInHostRun { return dropInHostRun{Leg: dropInLegBase("loopback-n1", false)} }
	checkMuts(t, base, classifyDropInHost, []mutCase[dropInHostRun]{
		{"host leg passes", func(*dropInHostRun) {}, statePass},
		{"loopback host not created", func(r *dropInHostRun) { r.Setup = "POST /api/hosts status 502" }, stateBlocked},
		{"host leg fails", func(r *dropInHostRun) { r.Leg.ResumeSeen = false }, stateFail},
		{"first turn errored, as with a claude that cannot sign in", func(r *dropInHostRun) { r.Leg.TurnErrored = true }, stateFail},
		{"marker missing from the resumed terminal", func(r *dropInHostRun) { r.Leg.MarkerSeen = false }, stateFail},
		{"terminal deleted, no exit frame", func(r *dropInHostRun) { r.Leg.Deleted = true }, statePass},
		{"teardown left a host behind", func(r *dropInHostRun) { r.Teardown = "; teardown: DELETE host status 500" }, stateFail},
	}, map[string]string{
		"first turn errored, as with a claude that cannot sign in": "ended errored",
		"marker missing from the resumed terminal":                 "never appeared in the resumed terminal",
		"terminal deleted, no exit frame":                          "terminal deleted instead",
	})
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
		{"model not Haiku 5.5", func(r *toolRefusedRun) { r.Frames[0].InitModel = "claude-haiku-5" }, stateBlocked},
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
		{"init with its conversation id", `{"type":"llm_event","sessionId":"s1","event":{"type":"system","subtype":"init","model":"m","session_id":"c1"}}`,
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

func TestTrustPromptMatchesOnlyOnceDrawn(t *testing.T) {
	partial := "Do you trust the files in this folder?\r\r\nClaudeCode'llbeabletoread"
	drawn := "Isthisaprojectyoucreatedoroneyoutrust?\r\r\n❯No,exit\r\r\nYes,Itrustthisfolder\r\r\n\r\r\nEntertoconfirm·Esctocancel"
	if trustPrompt.MatchString(partial) {
		t.Errorf("matched a prompt still drawing: %q", partial)
	}
	if !trustPrompt.MatchString(drawn) {
		t.Errorf("did not match the drawn prompt: %q", drawn)
	}
}

func TestTrustRowReadsTheLastRedraw(t *testing.T) {
	prompt := "❯No,exit\r\r\nYes,Itrustthisfolder\r\r\nEntertoconfirm"
	cases := []struct{ text, want string }{
		{prompt, "no"},
		{prompt + "\r No, exit\r❯Yes, I trust this folder\r\r\n", "yes"},
		{prompt + "\r No, exit\r❯Yes, I trust this folder\r\n\r❯No, exit\r Yes, I trust this folder", "no"},
		{prompt + "\r No, exit\r❯Yes, I trust this folder\r\n❯ Try \"write a test\"", ""},
	}
	for _, c := range cases {
		if got := trustRow(c.text); got != c.want {
			t.Errorf("trustRow(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
