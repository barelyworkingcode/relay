package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	agentStateID   = "session-agent-state"
	agentModel     = "haiku"
	agentModelID   = "claude-haiku-5-5"
	agentFrameWait = 15 * time.Second
)

// agentFrame is one /ws frame the journey cares about.
type agentFrame struct {
	Type      string
	SessionID string
	State     string // session_state
	Since     string // session_state
	Excerpt   string // turn_done
	InitModel string // llm_event system/init
	Model     string // session_joined
}

func parseAgentFrame(raw []byte) agentFrame {
	var f struct {
		Type      string `json:"type"`
		SessionID string `json:"sessionId"`
		State     string `json:"state"`
		Since     string `json:"since"`
		Excerpt   string `json:"excerpt"`
		Model     string `json:"model"`
		Event     struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Model   string `json:"model"`
		} `json:"event"`
	}
	_ = json.Unmarshal(raw, &f)
	out := agentFrame{Type: f.Type, SessionID: f.SessionID, State: f.State, Since: f.Since, Excerpt: f.Excerpt}
	switch {
	case f.Type == "session_joined":
		out.Model = f.Model
	case f.Type == "llm_event" && f.Event.Type == "system" && f.Event.Subtype == "init":
		out.InitModel = f.Event.Model
	}
	return out
}

// frameLog collects every frame a connection receives, in order.
type frameLog struct {
	mu     sync.Mutex
	frames []agentFrame
}

func (l *frameLog) add(f agentFrame) {
	l.mu.Lock()
	l.frames = append(l.frames, f)
	l.mu.Unlock()
}

func (l *frameLog) snapshot() []agentFrame {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]agentFrame(nil), l.frames...)
}

// waitFor polls the log until pred holds or the wait ends.
func (l *frameLog) waitFor(ctx context.Context, d time.Duration, pred func([]agentFrame) bool) bool {
	for deadline := time.Now().Add(d); ; {
		if pred(l.snapshot()) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// idleAfterRunning skips the launch's starting → idle frame, which the
// connection always sees because it dials before the launch.
func idleAfterRunning(id string) func([]agentFrame) bool {
	return func(fs []agentFrame) bool {
		running := false
		for _, f := range fs {
			if f.Type != "session_state" || f.SessionID != id {
				continue
			}
			if f.State == "running" {
				running = true
			} else if running && f.State == "idle" {
				return true
			}
		}
		return false
	}
}

func hasState(id, state string) func([]agentFrame) bool {
	return func(fs []agentFrame) bool {
		for _, f := range fs {
			if f.Type == "session_state" && f.SessionID == id && f.State == state {
				return true
			}
		}
		return false
	}
}

type listedAttention struct {
	Found    bool
	State    string
	Since    string
	HasState bool
}

// agentStateRun is everything the journey saw, for classifyAgentState.
type agentStateRun struct {
	Project     string
	Marker      string
	DialStatus  int
	DialErr     string
	Create      frontendResponse
	SessionID   string
	JoinSeen    bool
	Message     frontendResponse
	Frames      []agentFrame // every frame the connection received
	IdleList    frontendResponse
	IdleRow     listedAttention
	EndErr      string
	EndedSeen   bool
	EndedList   frontendResponse
	EndedRow    listedAttention
	Delete      frontendResponse
	LogLines    []string // session.state lines for the id, raw
	LogErr      string
	IdleWaitErr bool // no idle frame within the wait
}

// runAgentState launches a headless agent session in Acme on Haiku, runs one
// turn, and checks the frames, the list row and the log relay-sessions wrote.
// The session is deleted whatever happened, and the delete ignores
// cancellation.
func runAgentState(ctx context.Context, e env) (out result) {
	launch, run, res, ok := screenCreds(e, agentStateID)
	if !ok {
		return res
	}
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return blocked(agentStateID, err.Error())
	}
	r := agentStateRun{Project: acme.Name, Marker: "verify-" + e.Nonce + "-done"}
	// The connection is dialled before the launch so the starting and running
	// frames cannot be missed, and it never joins before the id is known: the
	// state frames reach every connection.
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
		return classifyAgentState(r)
	}
	defer func() { _ = conn.Close() }()
	log := &frameLog{}
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			log.add(parseAgentFrame(raw))
		}
	}()

	body := jsonBody(map[string]any{"projectId": acmeID, "name": "verify-" + e.Nonce + "-agent", "model": agentModel,
		"settings": map[string]bool{"headless": true, "agent": true}})
	r.Create = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions", body, 30*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(r.Create.Body, &created)
	if r.SessionID = created.SessionID; r.Create.Status != http.StatusCreated || r.SessionID == "" {
		return classifyAgentState(r)
	}
	// Deferred so a panic mid-turn still deletes the session; the result is
	// classified after the delete has been recorded.
	defer func() {
		r.Delete = frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, "/api/sessions/"+r.SessionID, nil)
		out = classifyAgentState(r)
	}()
	driveAgentState(ctx, e, conn, log, run, &r)
	return result{}
}

// driveAgentState stops at the first step that failed and leaves the rest to
// the classifier. The caller's defer deletes the session.
func driveAgentState(ctx context.Context, e env, conn *websocket.Conn, log *frameLog, run string, r *agentStateRun) {
	id := r.SessionID
	_ = conn.WriteMessage(websocket.TextMessage, jsonBody(map[string]string{"type": "join_session", "sessionId": id}))
	r.JoinSeen = log.waitFor(ctx, agentFrameWait, func(fs []agentFrame) bool {
		for _, f := range fs {
			if f.Type == "session_joined" && f.SessionID == id {
				return true
			}
		}
		return false
	})
	if r.JoinSeen {
		r.Message, _ = chatTurn(ctx, e, run, id, "Reply with exactly: "+r.Marker)
	}
	if r.Message.Status == http.StatusOK {
		idle := idleAfterRunning(id)
		if r.IdleWaitErr = !log.waitFor(ctx, agentFrameWait, idle); !r.IdleWaitErr {
			r.IdleList = frontendDo(ctx, e, run, http.MethodGet, "/api/sessions", nil)
			r.IdleRow = listedRow(r.IdleList.Body, id)
		}
	}
	if r.JoinSeen {
		if err := conn.WriteMessage(websocket.TextMessage, jsonBody(map[string]string{"type": "end_session", "sessionId": id})); err != nil {
			r.EndErr = "end_session frame: " + err.Error()
		} else {
			r.EndedSeen = log.waitFor(ctx, agentFrameWait, hasState(id, "ended"))
			r.EndedList = frontendDo(ctx, e, run, http.MethodGet, "/api/sessions", nil)
			r.EndedRow = listedRow(r.EndedList.Body, id)
		}
	}
	r.Frames = log.snapshot()
	logPath := filepath.Join(e.ConfigDir, "logs", "relaysessions.log")
	for deadline := time.Now().Add(5 * time.Second); ; {
		lines, err := readStateLogLines(logPath, id)
		r.LogLines, r.LogErr = lines, ""
		if err != nil {
			r.LogErr = err.Error()
		}
		if err != nil || len(lines) >= countStateFrames(r.Frames, id) || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// listedRow finds the session in a GET /api/sessions body.
func listedRow(body []byte, id string) listedAttention {
	var v struct {
		Sessions []struct {
			ID        string `json:"id"`
			Attention *struct {
				State string `json:"state"`
				Since string `json:"since"`
			} `json:"attention"`
		} `json:"sessions"`
	}
	_ = json.Unmarshal(body, &v)
	for _, s := range v.Sessions {
		if s.ID != id {
			continue
		}
		out := listedAttention{Found: true}
		if s.Attention != nil {
			out.HasState, out.State, out.Since = true, s.Attention.State, s.Attention.Since
		}
		return out
	}
	return listedAttention{}
}

func countStateFrames(fs []agentFrame, id string) int {
	n := 0
	for _, f := range fs {
		if f.Type == "session_state" && f.SessionID == id {
			n++
		}
	}
	return n
}

// readStateLogLines returns the raw op=session.state lines that name the
// session, from the tail of relay-sessions' log.
func readStateLogLines(path, sessionID string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("relay-sessions log unreadable: %w", err)
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() > toolSearchLogTailSize {
		_, _ = f.Seek(fi.Size()-toolSearchLogTailSize, io.SeekStart)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("relay-sessions log unreadable: %w", err)
	}
	return parseStateLogLines(raw, sessionID), nil
}

func parseStateLogLines(raw []byte, sessionID string) []string {
	var out []string
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var l struct {
			Op        string `json:"op"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(line, &l) == nil && l.Op == "session.state" && l.SessionID == sessionID {
			out = append(out, string(line))
		}
	}
	return out
}

// firstIndex is the index of the first frame at or after from for which pred
// holds, or -1.
func firstIndex(fs []agentFrame, from int, pred func(agentFrame) bool) int {
	for i := from; i < len(fs); i++ {
		if pred(fs[i]) {
			return i
		}
	}
	return -1
}

func classifyAgentState(r agentStateRun) result {
	const id = agentStateID
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case r.DialStatus == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on /ws")
	case r.DialErr != "" && r.DialStatus == 0:
		return blocked(id, "frontend socket unreachable: "+r.DialErr)
	case r.DialErr != "":
		return fail(fmt.Sprintf("GET /ws status %d: %s", r.DialStatus, r.DialErr))
	case r.Create.Status != http.StatusCreated || r.SessionID == "":
		return launchRefusal(id, "/api/sessions", r.Project, r.Create)
	case !r.JoinSeen:
		return fail("no session_joined frame after join_session")
	}
	var model string
	for _, f := range r.Frames {
		if f.Type == "llm_event" && f.SessionID == r.SessionID && f.InitModel != "" {
			model = f.InitModel
			break
		}
	}
	switch {
	case r.Message.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on the message route")
	case r.Message.TimedOut:
		return fail("no reply within 60 s")
	case r.Message.Status != http.StatusOK:
		return fail(fmt.Sprintf("message status %d: %s", r.Message.Status, r.Message.Error))
	case model == "":
		return fail("no system/init event with a model after the turn")
	case model != agentModelID:
		return blocked(id, "model is not Haiku: "+model)
	case r.Delete.Status/100 != 2:
		return fail(fmt.Sprintf("DELETE status %d: session %s may still be running", r.Delete.Status, r.SessionID))
	case r.IdleWaitErr:
		return fail("no idle session_state frame within 15 s of the reply")
	}
	sid := r.SessionID
	isState := func(s string) func(agentFrame) bool {
		return func(f agentFrame) bool { return f.Type == "session_state" && f.SessionID == sid && f.State == s }
	}
	runI := firstIndex(r.Frames, 0, isState("running"))
	idleI := -1
	if runI >= 0 {
		idleI = firstIndex(r.Frames, runI, isState("idle"))
	}
	switch {
	case runI < 0:
		return fail("no running session_state frame")
	case idleI < 0:
		return fail("no idle session_state frame after running")
	}
	turns, excerpt := 0, ""
	for _, f := range r.Frames[:idleI] {
		if f.Type == "turn_done" && f.SessionID == sid {
			turns++
			excerpt = f.Excerpt
		}
	}
	idleSince := r.Frames[idleI].Since
	// The list is read before end_session, so the row matches the last idle
	// frame that precedes the ended one.
	endI := firstIndex(r.Frames, idleI, isState("ended"))
	for i, f := range r.Frames {
		if i >= idleI && (endI < 0 || i < endI) && isState("idle")(f) {
			idleSince = f.Since
		}
	}
	switch {
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
	case r.LogErr != "":
		return blocked(id, r.LogErr)
	}
	if want := countStateFrames(r.Frames, sid); len(r.LogLines) != want {
		return fail(fmt.Sprintf("relay-sessions log has %d op=session.state lines for the session, %d session_state frames seen", len(r.LogLines), want))
	}
	for _, l := range r.LogLines {
		if strings.Contains(l, r.Marker) {
			return fail("a session.state log line contains the reply text")
		}
	}
	return result{id, statePass, fmt.Sprintf("headless agent in %s on %s: running, idle, ended frames in order; one turn_done carried the reply; list row idle with the frame's since, then no attention; %d log lines, none with text",
		r.Project, model, len(r.LogLines))}
}
