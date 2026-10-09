package main

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

func cosStartBase() cosStartRun {
	ok := frontendResponse{Status: http.StatusOK}
	const sid, tid, marker, prompt = "s1", "t1", "verify-n1-start", "verify-n1-term"
	startRow := func(phase, outcome, origin string) audit.AuditEvent {
		return cosRow(phase, outcome, `{"session_id":"s1","origin":"`+origin+`"}`)
	}
	launch := startRow("", "ok", cosOrigin)
	launch.Event = "session_launch"
	return cosStartRun{
		Project: "Acme", Marker: marker, TermPrompt: prompt, DialStatus: http.StatusSwitchingProtocols,
		Create:    frontendResponse{Status: http.StatusCreated, Body: []byte(`{"sessionId":"s1","mode":"headless","origin":"chief-of-staff"}`)},
		SessionID: sid, JoinSeen: true,
		Frames: []cosFrame{
			{Type: "llm_event", SessionID: sid, InitModel: agentModelID},
			{Type: "turn_done", SessionID: sid, Excerpt: marker},
			{Type: "session_state", SessionID: sid, State: "idle"},
		},
		List:       frontendResponse{Status: http.StatusOK, Body: []byte(`{"sessions":[{"id":"other"},{"id":"s1","origin":"chief-of-staff"}]}`)},
		LaunchRows: []audit.AuditEvent{launch},
		MsgRows:    []audit.AuditEvent{startRow("intent", "pending", cosOrigin), startRow("completion", "ok", cosOrigin)},
		Delete:     ok,
		TermCreate: frontendResponse{Status: http.StatusCreated, Body: []byte(`{"sessionId":"t1","mode":"terminal","origin":"chief-of-staff"}`)},
		TermID:     tid,
		TermList:   frontendResponse{Status: http.StatusOK, Body: []byte(`{"terminals":[{"id":"t1","origin":"chief-of-staff"}]}`)},
		TermDelete: ok,
		PS:         "/bin/zsh -l\nclaude --model haiku -- " + prompt + "\n",
	}
}

func TestClassifyCosStart(t *testing.T) {
	checkMuts(t, cosStartBase, classifyCosStart, []mutCase[cosStartRun]{
		{"all signals present", func(*cosStartRun) {}, statePass},
		{"run credential refused on /ws", func(r *cosStartRun) { r.DialStatus, r.DialErr = http.StatusUnauthorized, "bad" }, stateBlocked},
		{"start refused", func(r *cosStartRun) { r.Create, r.SessionID = frontendResponse{Status: http.StatusForbidden}, "" }, stateFail},
		{"start answers no origin", func(r *cosStartRun) { r.Create.Body = []byte(`{"sessionId":"s1","mode":"headless"}`) }, stateFail},
		{"never joined", func(r *cosStartRun) { r.JoinSeen = false }, stateFail},
		{"no turn_done with the marker", func(r *cosStartRun) { r.TurnWaitErr = true }, stateFail},
		{"init model is the alias", func(r *cosStartRun) { r.Frames[0].InitModel = agentModel }, stateFail},
		{"no init event", func(r *cosStartRun) { r.Frames = r.Frames[1:] }, stateFail},
		{"list row unmarked", func(r *cosStartRun) { r.List.Body = []byte(`{"sessions":[{"id":"s1"}]}`) }, stateFail},
		{"list row missing", func(r *cosStartRun) { r.List.Body = []byte(`{"sessions":[]}`) }, stateFail},
		{"audit unreadable", func(r *cosStartRun) { r.RowsErr = "relay audit failed" }, stateBlocked},
		{"launch row missing", func(r *cosStartRun) { r.LaunchRows = nil }, stateFail},
		{"launch row unmarked", func(r *cosStartRun) { r.LaunchRows[0].Args = []byte(`{"session_id":"s1"}`) }, stateFail},
		{"launch row not ok", func(r *cosStartRun) { r.LaunchRows[0].Outcome = "error" }, stateFail},
		{"message intent unmarked", func(r *cosStartRun) { r.MsgRows[0] = cosRow("intent", "pending", `{"session_id":"s1"}`) }, stateFail},
		{"message completion missing", func(r *cosStartRun) { r.MsgRows = r.MsgRows[:1] }, stateFail},
		{"headless session not deleted", func(r *cosStartRun) { r.Delete.Status = http.StatusInternalServerError }, stateFail},
		{"terminal start refused", func(r *cosStartRun) { r.TermCreate.Status, r.TermID = http.StatusBadRequest, "" }, stateFail},
		{"terminal row unmarked", func(r *cosStartRun) { r.TermList.Body = []byte(`{"terminals":[{"id":"t1"}]}`) }, stateFail},
		{"terminal row missing", func(r *cosStartRun) { r.TermList.Body = []byte(`{"terminals":[]}`) }, stateFail},
		{"terminal answers headless mode", func(r *cosStartRun) {
			r.TermCreate.Body = []byte(`{"sessionId":"t1","mode":"headless","origin":"chief-of-staff"}`)
		}, stateFail},
		{"argv lacks the end-of-options marker", func(r *cosStartRun) { r.PS = "claude --model haiku " + r.TermPrompt }, stateFail},
		{"argv lacks the alias", func(r *cosStartRun) { r.PS = "claude -- " + r.TermPrompt }, stateFail},
		{"argv has the full model id, not the alias", func(r *cosStartRun) { r.PS = "claude --model " + agentModelID + " -- " + r.TermPrompt }, stateFail},
		{"terminal not deleted", func(r *cosStartRun) { r.TermDelete.Status = http.StatusInternalServerError }, stateFail},
	}, nil)
}

func cosOutsideBase() (cosOutsideRun, string) {
	const pid = "p1"
	row := audit.AuditEvent{Event: "session_launch", Outcome: audit.AuditOutcomeDenied, TS: time.Unix(2001, 0),
		Actor: audit.AuditActor{ProjectID: pid}, Args: []byte(`{"origin":"chief-of-staff"}`)}
	return cosOutsideRun{
		Project: "Acme", Since: time.Unix(2000, 0),
		Dotdot:  frontendResponse{Status: http.StatusBadRequest, Body: []byte(`{"error":"folder_invalid"}`)},
		Symlink: frontendResponse{Status: http.StatusForbidden, Body: []byte(`{"error":"directory_outside_project"}`)},
		Rows:    []audit.AuditEvent{row},
	}, pid
}

func TestClassifyCosOutside(t *testing.T) {
	cases := []mutCase[cosOutsideRun]{
		{"both refused, denied row", func(*cosOutsideRun) {}, statePass},
		{"run credential refused", func(r *cosOutsideRun) { r.Dotdot.Status = http.StatusUnauthorized }, stateBlocked},
		{"dotdot started a session", func(r *cosOutsideRun) { r.Dotdot = frontendResponse{Status: http.StatusCreated} }, stateFail},
		{"dotdot refused with another code", func(r *cosOutsideRun) { r.Dotdot.Body = []byte(`{"error":"folder_not_found"}`) }, stateFail},
		{"symlink started a session", func(r *cosOutsideRun) { r.Symlink = frontendResponse{Status: http.StatusCreated} }, stateFail},
		{"symlink refused as not found", func(r *cosOutsideRun) {
			r.Symlink = frontendResponse{Status: http.StatusBadRequest, Body: []byte(`{"error":"folder_not_found"}`)}
		}, stateFail},
		{"audit unreadable", func(r *cosOutsideRun) { r.RowsErr = "relay audit failed" }, stateBlocked},
		{"no denied row", func(r *cosOutsideRun) { r.Rows = nil }, stateFail},
		{"row outcome ok", func(r *cosOutsideRun) { r.Rows[0].Outcome = audit.AuditOutcomeOK }, stateFail},
		{"denied row predates the requests", func(r *cosOutsideRun) { r.Rows[0].TS = r.Since.Add(-time.Hour) }, stateFail},
		{"denied row for another project", func(r *cosOutsideRun) { r.Rows[0].Actor.ProjectID = "p2" }, stateFail},
		{"denied row without origin", func(r *cosOutsideRun) { r.Rows[0].Args = []byte(`{}`) }, stateFail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, pid := cosOutsideBase()
			c.mut(&r)
			checkState(t, classifyCosOutside(r, pid), c.want)
		})
	}
}

func hostPS(sid string) string {
	script := base64.StdEncoding.EncodeToString([]byte("exec env 'RELAY_SESSION_ID'='" + sid + "' '/usr/bin/claude' '--print'"))
	return "/bin/zsh -l\n/usr/bin/ssh -o BatchMode=yes localhost -T -- sh -c 'eval \"$(printf %s " + script + " | base64 -d)\"'\n"
}

func cosHostBase() cosHostRun {
	ok := frontendResponse{Status: http.StatusOK}
	const sid, hid = "s1", "h1"
	row := func(event, phase, outcome string) audit.AuditEvent {
		return audit.AuditEvent{Event: event, Phase: phase, Outcome: outcome,
			Args: []byte(`{"session_id":"s1","origin":"chief-of-staff","host_id":"h1"}`)}
	}
	return cosHostRun{
		HostID: hid, Host: "loopback-n1-cos", Project: "Drop-in Host cos n1", Marker: "verify-n1-host", Marker2: "verify-n1-host2",
		DialStatus: http.StatusSwitchingProtocols,
		Create:     frontendResponse{Status: http.StatusCreated, Body: []byte(`{"sessionId":"s1","mode":"headless","origin":"chief-of-staff"}`)},
		SessionID:  sid, JoinSeen: true,
		List:   frontendResponse{Status: http.StatusOK, Body: []byte(`{"sessions":[{"id":"s1","origin":"chief-of-staff"}]}`)},
		PS:     hostPS(sid),
		Send:   frontendResponse{Status: http.StatusAccepted},
		Launch: []audit.AuditEvent{row("session_launch", "", "ok")},
		Msgs: []audit.AuditEvent{row("session_message", "intent", "pending"), row("session_message", "completion", "ok"),
			row("session_message", "intent", "pending"), row("session_message", "completion", "ok")},
		Delete: ok,
	}
}

func TestClassifyCosStartHost(t *testing.T) {
	checkMuts(t, cosHostBase, classifyCosStartHost, []mutCase[cosHostRun]{
		{"all signals present", func(*cosHostRun) {}, statePass},
		{"host or project not set up", func(r *cosHostRun) { r.Setup = "POST /api/hosts status 500" }, stateBlocked},
		{"run credential refused on /ws", func(r *cosHostRun) { r.DialStatus, r.DialErr = http.StatusUnauthorized, "bad" }, stateBlocked},
		{"start refused", func(r *cosHostRun) { r.Create, r.SessionID = frontendResponse{Status: http.StatusForbidden}, "" }, stateFail},
		{"start answers no origin", func(r *cosHostRun) { r.Create.Body = []byte(`{"sessionId":"s1","mode":"headless"}`) }, stateFail},
		{"never joined", func(r *cosHostRun) { r.JoinSeen = false }, stateFail},
		{"no first turn_done", func(r *cosHostRun) { r.TurnWaitErr = true }, stateFail},
		{"list row unmarked", func(r *cosHostRun) { r.List.Body = []byte(`{"sessions":[{"id":"s1"}]}`) }, stateFail},
		{"process list unreadable", func(r *cosHostRun) { r.PSErr = "ps failed" }, stateFail},
		{"no ssh child", func(r *cosHostRun) { r.PS = "/bin/zsh -l\n" }, stateFail},
		{"ssh child lacks -T", func(r *cosHostRun) { r.PS = strings.Replace(r.PS, " -T -- ", " -tt -- ", 1) }, stateFail},
		{"ssh child names another session", func(r *cosHostRun) { r.PS = hostPS("s2") }, stateFail},
		{"send refused", func(r *cosHostRun) { r.Send.Status = http.StatusForbidden }, stateFail},
		{"no second turn_done", func(r *cosHostRun) { r.Turn2Err = true }, stateFail},
		{"audit unreadable", func(r *cosHostRun) { r.RowsErr = "relay audit failed" }, stateBlocked},
		{"launch row missing", func(r *cosHostRun) { r.Launch = nil }, stateFail},
		{"launch row without host_id", func(r *cosHostRun) {
			r.Launch[0].Args = []byte(`{"session_id":"s1","origin":"chief-of-staff"}`)
		}, stateFail},
		{"launch row not ok", func(r *cosHostRun) { r.Launch[0].Outcome = "error" }, stateFail},
		{"launch row unmarked", func(r *cosHostRun) { r.Launch[0].Args = []byte(`{"session_id":"s1","host_id":"h1"}`) }, stateFail},
		{"send intent missing", func(r *cosHostRun) { r.Msgs = append(r.Msgs[:2:2], r.Msgs[3]) }, stateFail},
		{"send completion missing", func(r *cosHostRun) { r.Msgs = r.Msgs[:3] }, stateFail},
		{"message row unmarked", func(r *cosHostRun) { r.Msgs[2].Args = []byte(`{"session_id":"s1"}`) }, stateFail},
		{"session not deleted", func(r *cosHostRun) { r.Delete.Status = http.StatusInternalServerError }, stateFail},
		{"fixtures not removed", func(r *cosHostRun) { r.Teardown = "; teardown: DELETE host status 500" }, stateFail},
	}, nil)
}
