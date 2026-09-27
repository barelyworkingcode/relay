package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

func TestVerifyModelPick(t *testing.T) {
	t.Setenv("RELAY_VERIFY_MODEL", "")
	if got := verifyModel(); got != "Chat" {
		t.Errorf("default model %q, want Chat", got)
	}
	cases := []struct{ body, want string }{
		{`{"models":[{"value":"Chat"}]}`, "Chat"},
		{`{"models":[{"value":"Other"},{"value":"acme-host/Chat"}]}`, "acme-host/Chat"},
		{`{"models":[{"value":"Chatty"},{"value":"acme-host/NotChat"}]}`, ""},
		{`{"models":[]}`, ""},
	}
	for _, c := range cases {
		if got := pickModel([]byte(c.body), "Chat"); got != c.want {
			t.Errorf("pickModel(%s) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestModelCallRowsKeepThisRunInAcme(t *testing.T) {
	e := blockedEnv(t)
	since := time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)
	e.RelayBin = fakeBin(t, e.BinDir, "relay-audit", `cat <<'EOF'
{"id":"old","ts":"2026-09-27T02:59:00Z","event":"model_call","actor":{"kind":"project_session","project_id":"p1"},"outcome":"ok"}
{"id":"other","ts":"2026-09-27T03:00:05Z","event":"model_call","actor":{"kind":"project_session","project_id":"p2"},"outcome":"ok"}
{"id":"mine","ts":"2026-09-27T03:00:06Z","event":"model_call","actor":{"kind":"project_session","project_id":"p1"},"outcome":"ok"}
EOF
`)
	rows, err := modelCallRows(context.Background(), e, since, "p1")
	if err != nil || len(rows) != 1 || rows[0].ID != "mine" {
		t.Fatalf("modelCallRows = %+v, %v; want only the row after the baseline in p1", rows, err)
	}
}

func chatBase() chatRun {
	ok := frontendResponse{Status: http.StatusOK}
	return chatRun{Models: ok, Want: "Chat", Model: "Chat", Create: frontendResponse{Status: http.StatusCreated}, SessionID: "s1",
		Message: ok, Reply: "ready", Delete: ok, List: ok, LaunchRow: &audit.AuditEvent{Outcome: audit.AuditOutcomeOK},
		ModelRows: []audit.AuditEvent{{Outcome: audit.AuditOutcomeOK, Model: "Chat"}}}
}

var hostDown = func(r *chatRun) {
	r.Message, r.Reply = frontendResponse{Status: http.StatusBadGateway}, ""
	r.ModelRows = []audit.AuditEvent{{Outcome: "error", Model: "Chat", Status: http.StatusServiceUnavailable}}
}

func TestClassifyChatLifecycle(t *testing.T) {
	checkMuts(t, chatBase, classifyChatLifecycle, []mutCase[chatRun]{
		{"launched, answered, deleted, audited", func(*chatRun) {}, statePass},
		{"run credential refused", func(r *chatRun) { r.Models = frontendResponse{Status: http.StatusUnauthorized} }, stateBlocked},
		{"launch credential refused", func(r *chatRun) { r.Create, r.SessionID = frontendResponse{Status: http.StatusUnauthorized}, "" }, stateBlocked},
		{"model not listed", func(r *chatRun) { r.Model = "" }, stateBlocked},
		{"model host 503", hostDown, stateBlocked},
		{"no reply within 60 s", func(r *chatRun) { r.Message, r.Reply = frontendResponse{TimedOut: true}, "" }, notPass},
		{"empty reply", func(r *chatRun) { r.Reply = " " }, notPass},
		{"delete failed", func(r *chatRun) { r.Delete.Status = http.StatusInternalServerError }, notPass},
		{"still listed", func(r *chatRun) { r.StillListedAfter = true }, notPass},
		{"launch not audited", func(r *chatRun) { r.LaunchRow = nil }, notPass},
		{"launch row not ok", func(r *chatRun) { r.LaunchRow.Outcome = "error" }, notPass},
	}, nil)
}

func TestClassifyModelCompletion(t *testing.T) {
	checkMuts(t, chatBase, classifyModelCompletion, []mutCase[chatRun]{
		{"listed, answered, model_call ok", func(*chatRun) {}, statePass},
		{"model not listed", func(r *chatRun) { r.Model = "" }, stateBlocked},
		{"model host 503", hostDown, stateBlocked},
		{"no model_call row", func(r *chatRun) { r.ModelRows = nil }, notPass},
		{"row for another model", func(r *chatRun) { r.ModelRows[0].Model = "Other" }, notPass},
		{"row not ok", func(r *chatRun) { r.ModelRows[0].Outcome = "error" }, notPass},
		{"no reply", func(r *chatRun) { r.Reply = "" }, notPass},
	}, nil)
}

func TestClassifyTerminalLifecycle(t *testing.T) {
	base := func() terminalRun {
		ok := frontendResponse{Status: http.StatusOK}
		return terminalRun{Create: frontendResponse{Status: http.StatusCreated}, TermID: "t1", ListBefore: ok, ListedBefore: true,
			Log: frontendResponse{Status: http.StatusOK, Body: []byte("$ ")}, Delete: ok, ListAfter: ok, Row: &audit.AuditEvent{Outcome: audit.AuditOutcomeOK}}
	}
	checkMuts(t, base, classifyTerminalLifecycle, []mutCase[terminalRun]{
		{"launched, listed, logged, deleted, audited", func(*terminalRun) {}, statePass},
		{"launch credential refused", func(r *terminalRun) { r.Create, r.TermID = frontendResponse{Status: http.StatusUnauthorized}, "" }, stateBlocked},
		{"run credential refused", func(r *terminalRun) {
			r.ListBefore, r.ListedBefore = frontendResponse{Status: http.StatusUnauthorized}, false
		}, stateBlocked},
		{"not listed", func(r *terminalRun) { r.ListedBefore = false }, notPass},
		{"log refused", func(r *terminalRun) { r.Log = frontendResponse{Status: http.StatusNotFound} }, notPass},
		{"log empty", func(r *terminalRun) { r.Log.Body = nil }, notPass},
		{"delete failed", func(r *terminalRun) { r.Delete.Status = http.StatusInternalServerError }, notPass},
		{"still listed", func(r *terminalRun) { r.ListedAfter = true }, notPass},
		{"launch not audited", func(r *terminalRun) { r.Row = nil }, notPass},
	}, nil)
}

var (
	probeAnswers  = probeView{List: execOut{Out: toolTable("testmcp_ping")}, Call: execOut{Out: `{"pong":true}`}}
	probeDisabled = probeView{List: execOut{Out: toolTable()},
		Call: execOut{Out: "error: access denied: tool 'testmcp_ping' is disabled for this token\r\n", Exit: 1}}
	editHeld = frontendResponse{TimedOut: true}
)

func TestClassifyDisabledTool(t *testing.T) {
	base := func() disabledRun {
		ok := frontendResponse{Status: http.StatusOK}
		return disabledRun{Before: probeAnswers, Disable: ok, After: probeDisabled, Restore: ok, Restored: probeAnswers.List}
	}
	checkMuts(t, base, classifyDisabledTool, []mutCase[disabledRun]{
		{"disabled, dropped, refused, restored", func(*disabledRun) {}, statePass},
		{"tool not answering before", func(r *disabledRun) { r.Before.Call.Exit = 1 }, notPass},
		{"edit held by a prompt", func(r *disabledRun) { r.Disable, r.After = editHeld, probeView{} }, stateFail},
		{"edit refused", func(r *disabledRun) {
			r.Disable, r.After = frontendResponse{Status: http.StatusBadRequest}, probeView{}
		}, notPass},
		{"still listed", func(r *disabledRun) { r.After.List = probeAnswers.List }, stateFail},
		{"still answers", func(r *disabledRun) { r.After.Call = probeAnswers.Call }, stateFail},
		{"restore refused", func(r *disabledRun) {
			r.Restore, r.Restored = frontendResponse{Status: http.StatusBadRequest}, execOut{}
		}, notPass},
		{"not restored", func(r *disabledRun) { r.Restored = probeDisabled.List }, notPass},
	}, nil)
}

func TestClassifyGrantNarrowing(t *testing.T) {
	narrowed := probeView{List: execOut{Out: toolTable()}, Call: execOut{Out: "error: access denied: " + narrowRefusal + "\r\n", Exit: 1}}
	base := func() narrowRun {
		return narrowRun{Before: probeAnswers, Narrow: frontendResponse{Status: http.StatusOK}, After: narrowed}
	}
	checkMuts(t, base, classifyGrantNarrowing, []mutCase[narrowRun]{
		{"narrowed, list empty, call refused", func(*narrowRun) {}, statePass},
		{"tool not listed before", func(r *narrowRun) { r.Before.List = narrowed.List }, notPass},
		{"edit held by a prompt", func(r *narrowRun) { r.Narrow, r.After = editHeld, probeView{} }, stateFail},
		{"edit refused", func(r *narrowRun) {
			r.Narrow, r.After = frontendResponse{Status: http.StatusInternalServerError}, probeView{}
		}, notPass},
		{"still listed", func(r *narrowRun) { r.After.List = probeAnswers.List }, stateFail},
		{"still answers", func(r *narrowRun) { r.After.Call = probeAnswers.Call }, stateFail},
		{"refused for another reason", func(r *narrowRun) { r.After.Call.Out = "error: dial unix: connection refused\r\n" }, stateFail},
	}, nil)
}

func TestClassifyServiceStartStop(t *testing.T) {
	base := func() startStopRun {
		ok := frontendResponse{Status: http.StatusOK}
		return startStopRun{Start: ok, Started: svcView{State: "running", PIDs: []int{101}}, Up: true, Stop: ok, Stopped: svcView{State: "-"}, Down: true}
	}
	checkMuts(t, base, classifyServiceStartStop, []mutCase[startStopRun]{
		{"started and stopped", func(*startStopRun) {}, statePass},
		{"start refused", func(r *startStopRun) { r.Start = frontendResponse{Status: http.StatusInternalServerError} }, notPass},
		{"not running within 5 s", func(r *startStopRun) { r.Started, r.Up = svcView{State: "-"}, false }, stateFail},
		{"stop refused", func(r *startStopRun) { r.Stop = frontendResponse{Status: http.StatusInternalServerError} }, notPass},
		{"still up after stop", func(r *startStopRun) { r.Stopped, r.Down = svcView{State: "running", PIDs: []int{101}}, false }, stateFail},
	}, nil)
}

func TestClassifyServiceRestart(t *testing.T) {
	base := func() crashRun {
		ok := frontendResponse{Status: http.StatusOK}
		return crashRun{Start: ok, Up: svcView{State: "running", PIDs: []int{101}}, UpOK: true, Killed: 101,
			After: svcView{State: "running", PIDs: []int{202}}, Restarted: true, SawAttempt1: true, Stop: ok}
	}
	checkMuts(t, base, classifyServiceRestart, []mutCase[crashRun]{
		{"restarted with a new pid", func(*crashRun) {}, statePass},
		{"start refused", func(r *crashRun) { r.Start = frontendResponse{Status: http.StatusInternalServerError} }, notPass},
		{"never up", func(r *crashRun) { r.Up, r.UpOK = svcView{State: "-"}, false }, notPass},
		{"not restarted", func(r *crashRun) { r.After, r.Restarted = svcView{State: "failed (exit -1)"}, false }, stateFail},
	}, nil)
	var details []string
	for _, saw := range []bool{true, false} {
		r := base()
		r.SawAttempt1 = saw
		got := classifyServiceRestart(r)
		checkDetail(t, got, "restarting (attempt 1")
		details = append(details, got.Detail)
	}
	if details[0] == details[1] {
		t.Errorf("the detail %q does not say whether attempt 1 was seen", details[0])
	}
}
