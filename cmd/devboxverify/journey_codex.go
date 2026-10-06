package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	codexID        = "session-codex"
	codexValue     = "codex/gpt-6-luna"
	codexSlug      = "gpt-6-luna"
	codexTurnWait  = 120 * time.Second
	codexFrameWait = 15 * time.Second
)

// frameTextDelta is the synthetic frame type parseCodexFrame gives an
// assistant text delta; Excerpt carries the delta text.
const frameTextDelta = "text_delta"

// parseCodexFrame reads what parseAgentFrame reads, and also reports an
// assistant text delta, which parseAgentFrame does not.
func parseCodexFrame(raw []byte) agentFrame {
	f := parseAgentFrame(raw)
	if f.Type != "llm_event" {
		return f
	}
	var e struct {
		SessionID string `json:"sessionId"`
		Event     struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Event.Type == "assistant" && e.Event.Delta.Type == "text_delta" {
		return agentFrame{Type: frameTextDelta, SessionID: e.SessionID, Excerpt: e.Event.Delta.Text}
	}
	return f
}

// codexRun is everything the journey saw, for classifyCodex.
type codexRun struct {
	Project     string
	Marker      string
	Models      frontendResponse
	DialStatus  int
	DialErr     string
	Create      frontendResponse
	SessionID   string
	JoinSeen    bool
	SendErr     string
	Frames      []agentFrame
	IdleWaitErr bool
	IdleList    frontendResponse
	IdleRow     listedAttention
	EndErr      string
	EndedSeen   bool
	EndedList   frontendResponse
	EndedRow    listedAttention
	Delete      frontendResponse
}

// runCodex launches a Codex session in Acme, runs one turn over /ws, and
// checks the frames and the list row. Every non-pass outcome is a FAIL: the
// journey has no NOTRUN or BLOCKED of its own. The session is deleted whatever
// happened, and the delete ignores cancellation.
func runCodex(ctx context.Context, e env) (out result) {
	launch, run, res, ok := screenCreds(e, codexID)
	if !ok {
		return res
	}
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return result{codexID, stateFail, err.Error()}
	}
	r := codexRun{Project: acme.Name, Marker: "verify-" + e.Nonce + "-codex"}
	r.Models = frontendDo(ctx, e, run, http.MethodGet, "/api/models", nil)
	if r.Models.Status != http.StatusOK || pickModel(r.Models.Body, codexValue) != codexValue {
		return classifyCodex(r)
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: frontendRequestTimeout,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", e.FrontendSocket)
		},
	}
	conn, resp, err := dialer.DialContext(ctx, "ws://relay/ws", http.Header{"Authorization": {"Bearer " + run}})
	if resp != nil {
		r.DialStatus = resp.StatusCode
		_ = resp.Body.Close()
	}
	if err != nil {
		r.DialErr = err.Error()
		return classifyCodex(r)
	}
	defer func() { _ = conn.Close() }()
	log := &frameLog{}
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			log.add(parseCodexFrame(raw))
		}
	}()

	body := jsonBody(map[string]any{"projectId": acmeID, "name": "verify-" + e.Nonce + "-codex", "model": codexValue,
		"settings": map[string]bool{"useRelayTools": true}, "appendClaudeMd": true})
	r.Create = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions", body, 30*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(r.Create.Body, &created)
	if r.SessionID = created.SessionID; r.Create.Status != http.StatusCreated || r.SessionID == "" {
		return classifyCodex(r)
	}
	defer func() {
		r.Delete = frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, "/api/sessions/"+r.SessionID, nil)
		out = classifyCodex(r)
	}()
	driveCodex(ctx, e, conn, log, run, &r)
	return result{}
}

func driveCodex(ctx context.Context, e env, conn *websocket.Conn, log *frameLog, run string, r *codexRun) {
	id := r.SessionID
	_ = conn.WriteMessage(websocket.TextMessage, jsonBody(map[string]string{"type": "join_session", "sessionId": id}))
	r.JoinSeen = log.waitFor(ctx, codexFrameWait, func(fs []agentFrame) bool {
		for _, f := range fs {
			if f.Type == "session_joined" && f.SessionID == id {
				return true
			}
		}
		return false
	})
	if r.JoinSeen {
		send := jsonBody(map[string]string{"type": "send_message", "sessionId": id, "text": "Reply with exactly: " + r.Marker})
		if err := conn.WriteMessage(websocket.TextMessage, send); err != nil {
			r.SendErr = "send_message frame: " + err.Error()
		} else if r.IdleWaitErr = !log.waitFor(ctx, codexTurnWait, idleAfterRunning(id)); !r.IdleWaitErr {
			r.IdleList = frontendDo(ctx, e, run, http.MethodGet, "/api/sessions", nil)
			r.IdleRow = listedRow(r.IdleList.Body, id)
		}
	}
	if r.JoinSeen {
		if err := conn.WriteMessage(websocket.TextMessage, jsonBody(map[string]string{"type": "end_session", "sessionId": id})); err != nil {
			r.EndErr = "end_session frame: " + err.Error()
		} else {
			r.EndedSeen = log.waitFor(ctx, codexFrameWait, hasState(id, "ended"))
			r.EndedList = frontendDo(ctx, e, run, http.MethodGet, "/api/sessions", nil)
			r.EndedRow = listedRow(r.EndedList.Body, id)
		}
	}
	r.Frames = log.snapshot()
}

func classifyCodex(r codexRun) result {
	const id = codexID
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case r.Models.Status != http.StatusOK:
		return fail(fmt.Sprintf("GET /api/models status %d: %s", r.Models.Status, r.Models.Error))
	case pickModel(r.Models.Body, codexValue) != codexValue:
		return fail("GET /api/models does not list " + codexValue)
	case r.DialErr != "":
		return fail(fmt.Sprintf("GET /ws status %d: %s", r.DialStatus, r.DialErr))
	case r.Create.Status != http.StatusCreated || r.SessionID == "":
		return fail(fmt.Sprintf("POST /api/sessions for %s status %d: %s", r.Project, r.Create.Status, r.Create.Error))
	case !r.JoinSeen:
		return fail("no session_joined frame after join_session")
	case r.SendErr != "":
		return fail(r.SendErr)
	}
	sid := r.SessionID
	var model string
	for _, f := range r.Frames {
		if f.Type == "llm_event" && f.SessionID == sid && f.InitModel != "" {
			model = f.InitModel
			break
		}
	}
	isState := func(s string) func(agentFrame) bool {
		return func(f agentFrame) bool { return f.Type == "session_state" && f.SessionID == sid && f.State == s }
	}
	runI := firstIndex(r.Frames, 0, isState("running"))
	idleI := -1
	if runI >= 0 {
		idleI = firstIndex(r.Frames, runI, isState("idle"))
	}
	switch {
	case r.IdleWaitErr:
		return fail("no idle session_state frame after running within the turn wait")
	case runI < 0:
		return fail("no running session_state frame")
	case idleI < 0:
		return fail("no idle session_state frame after running")
	case model == "":
		return fail("no system/init event with a model")
	case model != codexSlug:
		return fail(fmt.Sprintf("system/init model %q, want %q", model, codexSlug))
	}
	deltas, turns, excerpt := 0, 0, ""
	for _, f := range r.Frames[:idleI] {
		switch {
		case f.SessionID != sid:
		case f.Type == frameTextDelta:
			deltas++
		case f.Type == "turn_done":
			turns++
			excerpt = f.Excerpt
		}
	}
	idleSince := r.Frames[idleI].Since
	endI := firstIndex(r.Frames, idleI, isState("ended"))
	for i, f := range r.Frames {
		if i >= idleI && (endI < 0 || i < endI) && isState("idle")(f) {
			idleSince = f.Since
		}
	}
	switch {
	case deltas == 0:
		return fail("no assistant text delta before the idle frame")
	case turns != 1:
		return fail(fmt.Sprintf("%d turn_done frames before the idle frame, want 1", turns))
	case !strings.Contains(excerpt, r.Marker):
		return fail(fmt.Sprintf("turn_done excerpt %q does not contain %q", excerpt, r.Marker))
	case r.IdleList.Status != http.StatusOK:
		return fail(fmt.Sprintf("GET /api/sessions status %d after idle", r.IdleList.Status))
	case !r.IdleRow.Found:
		return fail("session not in GET /api/sessions after idle")
	case r.IdleRow.State != "idle":
		return fail(fmt.Sprintf("list row attention.state %q after idle, want idle", r.IdleRow.State))
	case r.IdleRow.Since != idleSince:
		return fail(fmt.Sprintf("list row attention.since %q, idle frame since %q", r.IdleRow.Since, idleSince))
	case r.EndErr != "":
		return fail(r.EndErr)
	case !r.EndedSeen || endI < 0:
		return fail("no ended session_state frame within 15 s of end_session")
	case r.EndedList.Status != http.StatusOK:
		return fail(fmt.Sprintf("GET /api/sessions status %d after end", r.EndedList.Status))
	case r.EndedRow.HasState:
		return fail(fmt.Sprintf("list row still carries attention %q after the session ended", r.EndedRow.State))
	case r.Delete.Status/100 != 2:
		return fail(fmt.Sprintf("DELETE status %d: session %s may still be running", r.Delete.Status, sid))
	}
	return result{id, statePass, fmt.Sprintf("codex session in %s on %s: running, idle, ended frames in order; text deltas and one turn_done carried the reply; list row idle with the frame's since, then no attention",
		r.Project, model)}
}
