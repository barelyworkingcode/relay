package main

import (
	"net/http"
	"testing"
)

func codexBase() codexRun {
	ok := frontendResponse{Status: http.StatusOK}
	const sid, marker = "s1", "verify-n1-codex"
	st := func(state, since string) agentFrame {
		return agentFrame{Type: "session_state", SessionID: sid, State: state, Since: since}
	}
	return codexRun{
		Project: "Acme", Marker: marker,
		Models:     frontendResponse{Status: http.StatusOK, Body: []byte(`{"models":[{"value":"codex/gpt-6-luna","group":"Codex"}]}`)},
		DialStatus: http.StatusSwitchingProtocols,
		Create:     frontendResponse{Status: http.StatusCreated}, SessionID: sid, JoinSeen: true,
		Frames: []agentFrame{
			st("starting", "t0"), {Type: "session_joined", SessionID: sid},
			st("running", "t1"),
			{Type: "llm_event", SessionID: sid, InitModel: "gpt-6-luna"},
			{Type: frameTextDelta, SessionID: sid, Excerpt: marker},
			{Type: "turn_done", SessionID: sid, Excerpt: "ok " + marker},
			st("idle", "t2"), st("ended", "t3"),
			{Type: "turn_done", SessionID: "s2", Excerpt: "other"},
		},
		IdleList: ok, IdleRow: listedAttention{Found: true, HasState: true, State: "idle", Since: "t2"},
		EndedSeen: true, EndedList: ok, EndedRow: listedAttention{Found: true},
		Delete: ok,
	}
}

func TestClassifyCodex(t *testing.T) {
	checkMuts(t, codexBase, classifyCodex, []mutCase[codexRun]{
		{"all signals present", func(*codexRun) {}, statePass},
		{"models status not 200", func(r *codexRun) { r.Models = frontendResponse{Status: http.StatusInternalServerError} }, stateFail},
		{"codex model not listed", func(r *codexRun) { r.Models.Body = []byte(`{"models":[{"value":"haiku"}]}`) }, stateFail},
		{"run credential refused on /ws", func(r *codexRun) { r.DialStatus, r.DialErr = http.StatusUnauthorized, "bad handshake" }, stateFail},
		{"socket unreachable", func(r *codexRun) { r.DialStatus, r.DialErr = 0, "dial unix" }, stateFail},
		{"launch refused 403", func(r *codexRun) { r.Create, r.SessionID = frontendResponse{Status: http.StatusForbidden}, "" }, stateFail},
		{"launch 201 without id", func(r *codexRun) { r.SessionID = "" }, stateFail},
		{"never joined", func(r *codexRun) { r.JoinSeen = false }, stateFail},
		{"send_message frame failed", func(r *codexRun) { r.SendErr = "send_message frame: closed" }, stateFail},
		{"no idle frame in time", func(r *codexRun) { r.IdleWaitErr = true }, stateFail},
		{"idle before running", func(r *codexRun) { r.Frames[2].State = "idle" }, stateFail},
		{"no init event", func(r *codexRun) { r.Frames[3].InitModel = "" }, stateFail},
		{"different model", func(r *codexRun) { r.Frames[3].InitModel = "gpt-5" }, stateFail},
		{"no text delta", func(r *codexRun) { r.Frames[4].Type = "x" }, stateFail},
		{"no turn_done", func(r *codexRun) { r.Frames[5].Type = "x" }, stateFail},
		{"excerpt lacks the marker", func(r *codexRun) { r.Frames[5].Excerpt = "other text" }, stateFail},
		{"two turn_done before idle", func(r *codexRun) { r.Frames = append([]agentFrame{r.Frames[5]}, r.Frames...) }, stateFail},
		{"list row not idle", func(r *codexRun) { r.IdleRow.State = "running" }, stateFail},
		{"list since differs from the frame", func(r *codexRun) { r.IdleRow.Since = "t9" }, stateFail},
		{"session missing from the list", func(r *codexRun) { r.IdleRow = listedAttention{} }, stateFail},
		{"no ended frame", func(r *codexRun) { r.EndedSeen = false; r.Frames[7].State = "idle" }, stateFail},
		{"attention after end", func(r *codexRun) { r.EndedRow = listedAttention{Found: true, HasState: true, State: "idle"} }, stateFail},
		{"delete failed", func(r *codexRun) { r.Delete.Status = http.StatusInternalServerError }, stateFail},
	}, nil)
}

func TestParseCodexFrame(t *testing.T) {
	d := parseCodexFrame([]byte(`{"type":"llm_event","sessionId":"s1","event":{"type":"assistant","index":0,"delta":{"type":"text_delta","text":"hi"}}}`))
	if d.Type != frameTextDelta || d.SessionID != "s1" || d.Excerpt != "hi" {
		t.Fatalf("text delta parsed as %+v", d)
	}
	if f := parseCodexFrame([]byte(`{"type":"llm_event","sessionId":"s1","event":{"type":"assistant","delta":{"type":"thinking_delta","thinking":"x"}}}`)); f.Type == frameTextDelta {
		t.Fatalf("thinking delta read as text: %+v", f)
	}
	if f := parseCodexFrame([]byte(`{"type":"llm_event","sessionId":"s1","event":{"type":"system","subtype":"init","model":"m"}}`)); f.InitModel != "m" {
		t.Fatalf("init frame parsed as %+v", f)
	}
}
