package main

import (
	"net/http"
	"testing"
)

func agentStateBase() agentStateRun {
	ok := frontendResponse{Status: http.StatusOK}
	const sid, marker = "s1", "verify-n1-done"
	st := func(state, since string) agentFrame {
		return agentFrame{Type: "session_state", SessionID: sid, State: state, Since: since}
	}
	return agentStateRun{
		Project: "Acme", Marker: marker, DialStatus: http.StatusSwitchingProtocols,
		Create: frontendResponse{Status: http.StatusCreated}, SessionID: sid, JoinSeen: true, Message: ok,
		Frames: []agentFrame{
			st("starting", "t0"), {Type: "session_joined", SessionID: sid, Model: "haiku"},
			{Type: "llm_event", SessionID: sid, InitModel: "claude-haiku-5-5"},
			st("running", "t1"),
			{Type: "turn_done", SessionID: sid, Excerpt: "ok " + marker},
			st("idle", "t2"), st("ended", "t3"),
			{Type: "session_state", SessionID: "s2", State: "running", Since: "other"}, // another session's frames are ignored
		},
		IdleList: ok, IdleRow: listedAttention{Found: true, HasState: true, State: "idle", Since: "t2"},
		EndedSeen: true, EndedList: ok, EndedRow: listedAttention{Found: true},
		Delete: ok,
		LogLines: []string{`{"op":"session.state","to":"starting"}`, `{"op":"session.state","to":"running"}`,
			`{"op":"session.state","to":"idle"}`, `{"op":"session.state","to":"ended"}`},
	}
}

func TestClassifyAgentState(t *testing.T) {
	checkMuts(t, agentStateBase, classifyAgentState, []mutCase[agentStateRun]{
		{"all signals present", func(*agentStateRun) {}, statePass},
		{"run credential refused on /ws", func(r *agentStateRun) { r.DialStatus, r.DialErr = http.StatusUnauthorized, "bad handshake" }, stateBlocked},
		{"socket unreachable", func(r *agentStateRun) { r.DialStatus, r.DialErr = 0, "dial unix" }, stateBlocked},
		{"launch refused", func(r *agentStateRun) { r.Create, r.SessionID = frontendResponse{Status: http.StatusForbidden}, "" }, stateBlocked},
		{"never joined", func(r *agentStateRun) { r.JoinSeen = false }, notPass},
		{"model not Haiku 5.5", func(r *agentStateRun) { r.Frames[2].InitModel = "claude-haiku-5" }, stateBlocked},
		{"no init event", func(r *agentStateRun) { r.Frames[2].InitModel = "" }, notPass},
		{"message timed out", func(r *agentStateRun) { r.Message = frontendResponse{TimedOut: true} }, notPass},
		{"delete failed", func(r *agentStateRun) { r.Delete.Status = http.StatusInternalServerError }, notPass},
		{"no idle frame", func(r *agentStateRun) { r.IdleWaitErr = true }, notPass},
		{"idle before running", func(r *agentStateRun) { r.Frames[3].State = "idle" }, notPass},
		{"no turn_done", func(r *agentStateRun) { r.Frames[4].Type = "x" }, notPass},
		{"excerpt lacks the marker", func(r *agentStateRun) { r.Frames[4].Excerpt = "other text" }, notPass},
		{"two turn_done before idle", func(r *agentStateRun) { r.Frames = append([]agentFrame{r.Frames[4]}, r.Frames...) }, notPass},
		{"list row not idle", func(r *agentStateRun) { r.IdleRow.State = "running" }, notPass},
		{"list since differs from the frame", func(r *agentStateRun) { r.IdleRow.Since = "t9" }, notPass},
		{"session missing from the list", func(r *agentStateRun) { r.IdleRow = listedAttention{} }, notPass},
		{"no ended frame", func(r *agentStateRun) { r.EndedSeen = false; r.Frames[6].State = "idle" }, notPass},
		{"attention after end", func(r *agentStateRun) { r.EndedRow = listedAttention{Found: true, HasState: true, State: "idle"} }, notPass},
		{"log unreadable", func(r *agentStateRun) { r.LogErr = "relay-sessions log unreadable" }, stateBlocked},
		{"log line missing", func(r *agentStateRun) { r.LogLines = r.LogLines[:3] }, notPass},
		{"log line extra", func(r *agentStateRun) { r.LogLines = append(r.LogLines, r.LogLines[0]) }, notPass},
		{"log line carries the text", func(r *agentStateRun) { r.LogLines[2] = `{"op":"session.state","text":"verify-n1-done"}` }, notPass},
	}, nil)
}

func TestParseAgentFrame(t *testing.T) {
	f := parseAgentFrame([]byte(`{"type":"llm_event","sessionId":"s1","event":{"type":"system","subtype":"init","model":"m"}}`))
	if f.InitModel != "m" || f.SessionID != "s1" {
		t.Fatalf("init frame parsed as %+v", f)
	}
	if f := parseAgentFrame([]byte(`{"type":"llm_event","sessionId":"s1","event":{"type":"assistant","model":"m"}}`)); f.InitModel != "" {
		t.Fatalf("non-init event read as init: %+v", f)
	}
}

func TestParseStateLogLines(t *testing.T) {
	raw := []byte(`{"op":"session.state","session_id":"s1","to":"idle"}
{"op":"session.state","session_id":"s2","to":"idle"}
{"op":"chat.tool_search","session_id":"s1"}
not json
`)
	if got := parseStateLogLines(raw, "s1"); len(got) != 1 {
		t.Fatalf("got %d lines for s1, want 1: %v", len(got), got)
	}
}
