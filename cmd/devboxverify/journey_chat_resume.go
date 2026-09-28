package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/audit"
)

const chatResumeID = "session-chat-resume"

type chatResumeRun struct {
	chatRun          // launch, the first turn, the final delete and list, model_call rows
	EndStatus int    // the /ws handshake's HTTP status when the upgrade was refused
	EndErr    string // the end_session frame never reached relay-sessions
	EndRow    *audit.AuditEvent
	Dormant   frontendResponse // a message sent after the end, before the resume
	Resume    frontendResponse
	Resumed   bool
	Again     frontendResponse
	AgainText string
}

// runChatResume launches a chat in Acme, ends it over /ws the way eve's End
// button does (the provider stops, the record stays), resumes it through
// relay's own route, and asks for a second reply.
func runChatResume(ctx context.Context, e env) result {
	launch, run, res, ok := screenCreds(e, chatResumeID)
	if !ok {
		return res
	}
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return blocked(chatResumeID, err.Error())
	}
	start := time.Now().Add(-time.Second)
	r := chatResumeRun{chatRun: chatRun{Want: verifyModel(), Project: acme.Name}}
	r.Models = frontendDo(ctx, e, run, http.MethodGet, "/api/models", nil)
	if r.Models.Status == http.StatusOK {
		r.Model = pickModel(r.Models.Body, r.Want)
	}
	if r.Model == "" {
		return classifyChatResume(r)
	}
	body := jsonBody(map[string]string{"projectId": acmeID, "name": "verify-" + e.Nonce + "-resume", "model": r.Model})
	r.Create = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions", body, 30*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(r.Create.Body, &created)
	if r.SessionID = created.SessionID; r.Create.Status != http.StatusCreated || r.SessionID == "" {
		return classifyChatResume(r)
	}
	driveChatResume(ctx, e, launch, run, &r)
	cleanup := context.WithoutCancel(ctx)
	r.Delete = frontendDo(cleanup, e, run, http.MethodDelete, "/api/sessions/"+r.SessionID, nil)
	r.List = frontendDo(cleanup, e, run, http.MethodGet, "/api/sessions", nil)
	r.StillListedAfter = slices.Contains(listedIDs(r.List.Body, "sessions"), r.SessionID)
	r.ModelRows, r.RowsErr = modelCallRows(ctx, e, start, acmeID)
	return classifyChatResume(r)
}

// driveChatResume stops at the first step that failed; the caller deletes
// the session whatever happened here.
func driveChatResume(ctx context.Context, e env, launch, run string, r *chatResumeRun) {
	r.Message, r.Reply = chatTurn(ctx, e, run, r.SessionID, "Reply with the single word: ready")
	if r.Message.Status != http.StatusOK || strings.TrimSpace(r.Reply) == "" {
		return
	}
	if !endChatSession(ctx, e, run, r) {
		return
	}
	r.Dormant = frontendDoTimeout(ctx, e, run, http.MethodPost, "/api/sessions/"+r.SessionID+"/message",
		jsonBody(map[string]string{"text": "Reply with the single word: dormant"}), messageTimeout)
	if !resumeRequired(r.Dormant) {
		return
	}
	r.Resume = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions/"+r.SessionID+"/resume", nil, 30*time.Second)
	var resumed struct {
		Resumed bool `json:"resumed"`
	}
	_ = json.Unmarshal(r.Resume.Body, &resumed)
	if r.Resumed = resumed.Resumed; r.Resume.Status != http.StatusOK || !r.Resumed {
		return
	}
	r.Again, r.AgainText = chatTurn(ctx, e, run, r.SessionID, "Reply with the single word: again")
}

func resumeRequired(r frontendResponse) bool {
	return r.Status == http.StatusConflict && r.Error == "resume_required"
}

func chatTurn(ctx context.Context, e env, run, sessionID, text string) (frontendResponse, string) {
	resp := frontendDoTimeout(ctx, e, run, http.MethodPost, "/api/sessions/"+sessionID+"/message",
		jsonBody(map[string]string{"text": text}), messageTimeout)
	var reply struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(resp.Body, &reply)
	return resp, reply.Text
}

// endChatSession sends end_session on the frontend socket's /ws and holds
// the connection open until relay records the session_end, so the frame
// cannot be lost to an early close. The end frame gets no reply of its own.
func endChatSession(ctx context.Context, e env, run string, r *chatResumeRun) bool {
	dialer := websocket.Dialer{
		HandshakeTimeout: frontendRequestTimeout,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", e.FrontendSocket)
		},
	}
	conn, resp, err := dialer.DialContext(ctx, "ws://relay/ws", http.Header{"Authorization": {"Bearer " + run}})
	if resp != nil {
		r.EndStatus = resp.StatusCode
		_ = resp.Body.Close()
	}
	if err != nil {
		r.EndErr = "GET /ws: " + err.Error()
		return false
	}
	defer func() { _ = conn.Close() }()
	frame := jsonBody(map[string]string{"type": "end_session", "sessionId": r.SessionID})
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		r.EndErr = "end_session frame: " + err.Error()
		return false
	}
	r.EndRow = auditEventRow(ctx, e, audit.AuditEventSessionEnd, r.SessionID)
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	return r.EndRow != nil
}

func classifyChatResume(r chatResumeRun) result {
	const id = chatResumeID
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case r.Models.Status == 0:
		return blocked(id, "frontend socket unreachable")
	case r.Models.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401)")
	case r.Models.Status != http.StatusOK:
		return fail(fmt.Sprintf("GET /api/models status %d: %s", r.Models.Status, r.Models.Error))
	case r.Model == "":
		return blocked(id, fmt.Sprintf("model %q not in GET /api/models; set RELAY_VERIFY_MODEL", r.Want))
	case r.Create.Status != http.StatusCreated || r.SessionID == "":
		return launchRefusal(id, "/api/sessions", r.Project, r.Create)
	case hostUnavailable(r.ModelRows):
		return blocked(id, "the model host answered 503")
	case r.Message.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on the message route")
	case r.Message.TimedOut:
		return fail("no reply within 60 s")
	case r.Message.Status != http.StatusOK:
		return fail(fmt.Sprintf("message status %d: %s", r.Message.Status, r.Message.Error))
	case strings.TrimSpace(r.Reply) == "":
		return fail("200 with an empty reply")
	case r.EndStatus == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on /ws")
	case r.EndErr != "":
		return fail(r.EndErr)
	case r.EndRow == nil:
		return fail("no session_end row within 5 s of end_session")
	case !resumeRequired(r.Dormant):
		return fail(fmt.Sprintf("message after end_session status %d %q, want 409 resume_required", r.Dormant.Status, r.Dormant.Error))
	case r.Resume.Status == http.StatusUnauthorized:
		return blocked(id, "P4 execute credential refused (401) on resume")
	case r.Resume.Status != http.StatusOK:
		return fail(fmt.Sprintf("POST /api/sessions/{id}/resume status %d: %s", r.Resume.Status, r.Resume.Error))
	case !r.Resumed:
		return fail("resume answered 200 with resumed false: relay still holds the ended session as live")
	case r.Again.TimedOut:
		return fail("no reply within 60 s after resume")
	case r.Again.Status != http.StatusOK:
		return fail(fmt.Sprintf("message after resume status %d: %s", r.Again.Status, r.Again.Error))
	case strings.TrimSpace(r.AgainText) == "":
		return fail("200 with an empty reply after resume")
	case r.Delete.Status/100 != 2:
		return fail(fmt.Sprintf("DELETE status %d: session %s may still be running", r.Delete.Status, r.SessionID))
	case r.List.Status != http.StatusOK:
		return fail(fmt.Sprintf("GET /api/sessions status %d", r.List.Status))
	case r.StillListedAfter:
		return fail("session still listed after DELETE")
	}
	return result{id, statePass, "chat in " + r.Project + " with " + r.Model + " answered, ended, refused a message while dormant, resumed, answered again; deleted"}
}
