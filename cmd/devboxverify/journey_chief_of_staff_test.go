package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/gorilla/websocket"
)

func cosHistEntry(text, origin string) cosHist {
	c, _ := json.Marshal(text)
	return cosHist{Role: "user", Content: c, Origin: origin}
}

func cosRow(phase, outcome, args string) audit.AuditEvent {
	return audit.AuditEvent{ID: "r1", TS: time.Unix(1000, 0), Event: "session_message", Phase: phase, Outcome: outcome, Args: json.RawMessage(args)}
}

func chiefOfStaffBase() cosRun {
	ok := frontendResponse{Status: http.StatusOK}
	denied := frontendResponse{Status: http.StatusForbidden}
	const sid, person, cos = "s1", "verify-n1-person", "verify-n1-cos"
	since := time.Unix(2000, 0)
	deny := func(method, path, reason string) audit.AuditEvent {
		return audit.AuditEvent{Event: audit.AuditEventControlDecision, Outcome: audit.AuditOutcomeDenied, TS: since.Add(time.Second),
			Method: method, Path: path, Error: reason}
	}
	st := func(s string) cosFrame { return cosFrame{Type: "session_state", SessionID: sid, State: s} }
	return cosRun{
		Project: "Acme", PersonText: person, CosText: cos,
		DialStatus: 101, CosDialStatus: 101,
		Create: frontendResponse{Status: http.StatusCreated}, SessionID: sid, JoinSeen: true,
		Person: ok, Send: frontendResponse{Status: http.StatusAccepted, Body: []byte(`{"sessionId":"s1","origin":"chief-of-staff"}`)},
		List: ok, ListRow: listedAttention{Found: true, HasState: true, State: "idle"},
		NegProjects: denied, NegMessage: denied, NegUnscoped: denied,
		WSClosed: true, WSCloseCode: websocket.ClosePolicyViolation, WSCloseReason: cosReadOnlyReason, RejoinSeen: true,
		Frames: []cosFrame{
			{Type: "llm_event", SessionID: sid, InitModel: agentModelID},
			{Type: "user_message", SessionID: sid, Text: "Reply with exactly: " + person},
			{Type: "user_message", SessionID: sid, Text: "Reply with exactly: " + cos, Origin: cosOrigin},
			{Type: "session_joined", SessionID: sid, History: []cosHist{cosHistEntry("Reply with exactly: "+person, ""), cosHistEntry("Reply with exactly: "+cos, cosOrigin)}},
			{Type: "session_joined", SessionID: sid, History: []cosHist{cosHistEntry("Reply with exactly: "+person, ""), cosHistEntry("Reply with exactly: "+cos, cosOrigin)}},
		},
		CosFrames: []cosFrame{st("running"), st("idle")},
		NegSince:  since,
		Rows: []audit.AuditEvent{
			cosRow("intent", "pending", `{"session_id":"s1","origin":"chief-of-staff","text":"Reply with exactly: verify-n1-cos","text_bytes":33}`),
			cosRow("completion", "ok", `{"session_id":"s1","origin":"chief-of-staff","text_bytes":33}`),
			cosRow("intent", "pending", `{"session_id":"other","origin":"chief-of-staff","text":"verify-n1-person"}`), // another session
		},
		Decisions: []audit.AuditEvent{
			deny("GET", "/api/projects", "outside chief-of-staff scope"),
			deny("POST", "/api/sessions/s1/message", "outside chief-of-staff scope"),
			deny("POST", "/api/chief-of-staff/messages", "class not granted"),
		},
		Delete: ok,
	}
}

func TestClassifyChiefOfStaff(t *testing.T) {
	checkMuts(t, chiefOfStaffBase, classifyChiefOfStaff, []mutCase[cosRun]{
		{"all signals present", func(*cosRun) {}, statePass},
		{"run credential refused on /ws", func(r *cosRun) { r.DialStatus, r.DialErr = http.StatusUnauthorized, "bad handshake" }, stateBlocked},
		{"launch refused", func(r *cosRun) { r.Create, r.SessionID = frontendResponse{Status: http.StatusForbidden}, "" }, stateBlocked},
		{"model not Haiku 4.5", func(r *cosRun) { r.Frames[0].InitModel = "claude-haiku-5" }, stateBlocked},
		{"audit log unreadable", func(r *cosRun) { r.RowsErr = "relay audit failed" }, stateBlocked},
		{"log_args off: no text", func(r *cosRun) {
			r.Rows[0] = cosRow("intent", "pending", `{"session_id":"s1","origin":"chief-of-staff","text_bytes":33}`)
		}, statePass},
		{"wrong text_bytes", func(r *cosRun) {
			r.Rows[0] = cosRow("intent", "pending", `{"session_id":"s1","origin":"chief-of-staff","text_bytes":31}`)
		}, notPass},
		{"no text_bytes", func(r *cosRun) {
			r.Rows[0] = cosRow("intent", "pending", `{"session_id":"s1","origin":"chief-of-staff"}`)
		}, notPass},
		{"send refused", func(r *cosRun) { r.Send = frontendResponse{Status: http.StatusConflict} }, notPass},
		{"send answers the person's origin", func(r *cosRun) { r.Send.Body = []byte(`{"origin":""}`) }, notPass},
		{"no turn_done for the send", func(r *cosRun) { r.TurnWaitErr = true }, notPass},
		{"list row not idle", func(r *cosRun) { r.ListRow.State = "running" }, notPass},
		{"scoped project read allowed", func(r *cosRun) { r.NegProjects.Status = http.StatusOK }, notPass},
		{"scoped message send allowed", func(r *cosRun) { r.NegMessage.Status = http.StatusAccepted }, notPass},
		{"unscoped send allowed", func(r *cosRun) { r.NegUnscoped.Status = http.StatusAccepted }, notPass},
		{"scoped /ws stays open", func(r *cosRun) { r.WSClosed, r.WSCloseCode = false, 0 }, notPass},
		{"scoped /ws closes with the wrong code", func(r *cosRun) { r.WSCloseCode = websocket.CloseNormalClosure }, notPass},
		{"scoped /ws closes with the wrong reason", func(r *cosRun) { r.WSCloseReason = "bye" }, notPass},
		{"scoped /ws answers a join", func(r *cosRun) { r.CosFrames = append(r.CosFrames, cosFrame{Type: "session_joined", SessionID: "s1"}) }, notPass},
		{"forged person origin recorded live", func(r *cosRun) { r.Frames[1].Origin = cosOrigin }, notPass},
		{"chief-of-staff text unmarked live", func(r *cosRun) { r.Frames[2].Origin = "" }, notPass},
		{"no live frame for the send", func(r *cosRun) { r.Frames[2].Type = "x" }, notPass},
		{"person marked in the history", func(r *cosRun) { r.Frames[4].History[0].Origin = cosOrigin }, notPass},
		{"send unmarked in the history", func(r *cosRun) { r.Frames[4].History[1].Origin = "" }, notPass},
		{"never re-joined", func(r *cosRun) { r.RejoinSeen = false }, notPass},
		{"intent missing", func(r *cosRun) { r.Rows = r.Rows[1:] }, notPass},
		{"completion missing", func(r *cosRun) { r.Rows = append(r.Rows[:1], r.Rows[2]) }, notPass},
		{"second intent for the session", func(r *cosRun) {
			r.Rows = append(r.Rows, cosRow("intent", "pending", `{"session_id":"s1","origin":"chief-of-staff","text":"x-cos"}`))
		}, notPass},
		{"completion id differs", func(r *cosRun) { r.Rows[1].ID = "r2" }, notPass},
		{"intent not pending", func(r *cosRun) { r.Rows[0].Outcome = "ok" }, notPass},
		{"completion not ok", func(r *cosRun) { r.Rows[1].Outcome = "error" }, notPass},
		{"row without ts", func(r *cosRun) { r.Rows[1].TS = time.Time{} }, notPass},
		{"row origin missing", func(r *cosRun) {
			r.Rows[1] = cosRow("completion", "ok", `{"session_id":"s1","text_bytes":33}`)
		}, notPass},
		{"intent text lacks the send", func(r *cosRun) {
			r.Rows[0] = cosRow("intent", "pending", `{"session_id":"s1","origin":"chief-of-staff","text":"hello"}`)
		}, notPass},
		{"intent text has the nonce marker but not the full send", func(r *cosRun) {
			r.Rows[0] = cosRow("intent", "pending", `{"session_id":"s1","origin":"chief-of-staff","text":"verify-n1-cos","text_bytes":33}`)
		}, notPass},
		{"a row holds the person's text", func(r *cosRun) {
			r.Rows[1] = cosRow("completion", "ok", `{"session_id":"s1","origin":"chief-of-staff","text":"verify-n1-person"}`)
		}, notPass},
		{"scope denial lacks its reason", func(r *cosRun) { r.Decisions[0].Error = "forbidden" }, notPass},
		{"unscoped denial has the scope reason", func(r *cosRun) { r.Decisions[2].Error = "outside chief-of-staff scope" }, notPass},
		{"denial recorded before the negatives", func(r *cosRun) { r.Decisions[1].TS = r.NegSince.Add(-time.Hour) }, notPass},
		{"denial row outcome not denied", func(r *cosRun) { r.Decisions[1].Outcome = audit.AuditOutcomeOK }, notPass},
		{"delete failed", func(r *cosRun) { r.Delete.Status = http.StatusInternalServerError }, notPass},
	}, nil)
}

func TestHistoryMark(t *testing.T) {
	h := []cosHist{cosHistEntry("a verify-x", ""), {Role: "assistant", Content: json.RawMessage(`"verify-y"`), Origin: "z"}, cosHistEntry("b verify-y", cosOrigin)}
	if o, ok := historyMark(h, "verify-y"); !ok || o != cosOrigin {
		t.Fatalf("assistant entry matched or user entry missed: %q %v", o, ok)
	}
	if _, ok := historyMark(h, "verify-q"); ok {
		t.Fatal("absent marker found")
	}
}

func TestParseCosFrame(t *testing.T) {
	f := parseCosFrame([]byte(`{"type":"user_message","sessionId":"s1","text":"hi","origin":"chief-of-staff"}`))
	if f.Text != "hi" || f.Origin != cosOrigin || f.SessionID != "s1" {
		t.Fatalf("user_message parsed as %+v", f)
	}
	j := parseCosFrame([]byte(`{"type":"session_joined","sessionId":"s1","history":[{"role":"user","content":"x","origin":"chief-of-staff"}]}`))
	if len(j.History) != 1 || j.History[0].Origin != cosOrigin {
		t.Fatalf("session_joined parsed as %+v", j)
	}
}

func TestCosTurnFinished(t *testing.T) {
	idle := cosFrame{Type: "session_state", SessionID: "s1", State: "idle"}
	done := cosFrame{Type: "turn_done", SessionID: "s1", Excerpt: "ok marker"}
	if cosTurnFinished("s1", "marker")([]cosFrame{idle, done}) {
		t.Fatal("an idle frame before the turn_done counted")
	}
	if !cosTurnFinished("s1", "marker")([]cosFrame{idle, done, idle}) {
		t.Fatal("turn_done then idle not recognised")
	}
	if cosTurnFinished("s1", "marker")([]cosFrame{{Type: "turn_done", SessionID: "s2", Excerpt: "marker"}, {Type: "session_state", SessionID: "s2", State: "idle"}}) {
		t.Fatal("another session's frames counted")
	}
}
