package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/gorilla/websocket"
)

const (
	cosID             = "chief-of-staff-send"
	cosScope          = "chief-of-staff"
	cosOrigin         = "chief-of-staff"
	cosDeniedScope    = "outside chief-of-staff scope"
	cosDeniedClass    = "class not granted"
	cosReadOnlyReason = "chief-of-staff scope is read-only"
	cosWait           = 15 * time.Second
	cosTurnWait       = 60 * time.Second
)

// cosHist is one message of a session_joined history.
type cosHist struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Origin  string          `json:"origin"`
	Error   string          `json:"error"`
}

// cosFrame is one /ws frame the journey cares about.
type cosFrame struct {
	Type      string
	SessionID string
	State     string    // session_state
	Excerpt   string    // turn_done
	InitModel string    // llm_event system/init
	Text      string    // user_message
	Origin    string    // user_message
	History   []cosHist // session_joined
	Limit     limitFrame
}

func parseCosFrame(raw []byte) cosFrame {
	var f struct {
		Type      string    `json:"type"`
		SessionID string    `json:"sessionId"`
		State     string    `json:"state"`
		Excerpt   string    `json:"excerpt"`
		Text      string    `json:"text"`
		Origin    string    `json:"origin"`
		History   []cosHist `json:"history"`
		Event     struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Model   string `json:"model"`
		} `json:"event"`
	}
	_ = json.Unmarshal(raw, &f)
	out := cosFrame{Type: f.Type, SessionID: f.SessionID, State: f.State, Excerpt: f.Excerpt, Text: f.Text, Origin: f.Origin, History: f.History}
	if f.Type == "llm_event" && f.Event.Type == "system" && f.Event.Subtype == "init" {
		out.InitModel = f.Event.Model
	}
	out.Limit = parseLimitFrame(raw)
	if f.Type == "session_joined" {
		out.Limit = historyLimit(f.History)
	}
	return out
}

// cosConn is a /ws connection that records every frame and how it closed.
type cosConn struct {
	conn     *websocket.Conn
	mu       sync.Mutex
	frames   []cosFrame
	closeErr error
	done     chan struct{}
}

// dialCos dials /ws with the run credential, in the Chief of Staff scope when
// scoped. The handshake status is returned even when the dial fails.
func dialCos(ctx context.Context, e env, run string, scoped bool) (*cosConn, int, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: frontendRequestTimeout,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", e.FrontendSocket)
		},
	}
	h := http.Header{"Authorization": {"Bearer " + run}}
	if scoped {
		h.Set("X-Relay-Scope", cosScope)
	}
	conn, resp, err := dialer.DialContext(ctx, "ws://relay/ws", h)
	status := 0
	if resp != nil {
		status = resp.StatusCode
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, status, err
	}
	c := &cosConn{conn: conn, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				c.mu.Lock()
				c.closeErr = err
				c.mu.Unlock()
				return
			}
			f := parseCosFrame(raw)
			c.mu.Lock()
			c.frames = append(c.frames, f)
			c.mu.Unlock()
		}
	}()
	return c, status, nil
}

func (c *cosConn) snapshot() []cosFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cosFrame(nil), c.frames...)
}

// closeCode is the websocket close code the server sent, or 0.
func (c *cosConn) closeCode() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ce *websocket.CloseError
	if errors.As(c.closeErr, &ce) {
		return ce.Code
	}
	return 0
}

// closeReason is the reason text of the close frame the server sent, or "".
func (c *cosConn) closeReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ce *websocket.CloseError
	if errors.As(c.closeErr, &ce) {
		return ce.Text
	}
	return ""
}

func (c *cosConn) send(v any) error {
	return c.conn.WriteMessage(websocket.TextMessage, jsonBody(v))
}

func (c *cosConn) waitFor(ctx context.Context, d time.Duration, pred func([]cosFrame) bool) bool {
	for deadline := time.Now().Add(d); ; {
		if pred(c.snapshot()) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func cosJoined(id string, n int) func([]cosFrame) bool {
	return func(fs []cosFrame) bool {
		seen := 0
		for _, f := range fs {
			if f.Type == "session_joined" && f.SessionID == id {
				seen++
			}
		}
		return seen >= n
	}
}

// cosIdleAfterRunning is true once the session ran and then went idle.
func cosIdleAfterRunning(id string) func([]cosFrame) bool {
	return func(fs []cosFrame) bool {
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

// cosTurnFinished is true when a turn_done with the marker is followed by an
// idle frame, whatever came before it.
func cosTurnFinished(id, marker string) func([]cosFrame) bool {
	return func(fs []cosFrame) bool {
		done := false
		for _, f := range fs {
			switch {
			case f.SessionID != id:
			case f.Type == "turn_done" && strings.Contains(f.Excerpt, marker):
				done = true
			case done && f.Type == "session_state" && f.State == "idle":
				return true
			}
		}
		return false
	}
}

// cosRequest sends a frontend-socket request, in the scope when scoped.
func cosRequest(ctx context.Context, e env, token string, scoped bool, method, path string, body []byte, timeout time.Duration) frontendResponse {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://relay"+path, rd)
	if err != nil {
		return frontendResponse{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if scoped {
		req.Header.Set("X-Relay-Scope", cosScope)
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", e.FrontendSocket)
	}}}
	resp, err := client.Do(req)
	if err != nil {
		var ne net.Error
		return frontendResponse{TimedOut: errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()}
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(io.LimitReader(resp.Body, 256<<10))
	var b struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(buf.String())
	if json.Unmarshal(buf.Bytes(), &b) == nil && b.Error != "" {
		msg = b.Error
	}
	return frontendResponse{Status: resp.StatusCode, Body: buf.Bytes(), Error: msg}
}

type cosRun struct {
	Project    string
	PersonText string
	CosText    string

	DialStatus, CosDialStatus int
	DialErr, CosDialErr       string

	Create    frontendResponse
	SessionID string
	JoinSeen  bool

	Person        frontendResponse
	PersonIdleErr bool // no idle frame after the person's turn
	Send          frontendResponse
	TurnWaitErr   bool // no turn_done and idle on the Chief of Staff connection
	List          frontendResponse
	ListRow       listedAttention

	NegProjects, NegMessage, NegUnscoped frontendResponse
	WSJoinWriteErr                       string
	WSCloseCode                          int
	WSCloseReason                        string
	WSClosed                             bool

	RejoinSeen bool
	Frames     []cosFrame // observer
	CosFrames  []cosFrame // Chief of Staff connection

	NegSince  time.Time
	Rows      []audit.AuditEvent // session_message rows naming the session
	RowsErr   string
	Decisions []audit.AuditEvent
	DecErr    string

	EndErr string
	Delete frontendResponse
}

func runChiefOfStaffSend(ctx context.Context, e env) (out result) {
	launch, run, res, ok := screenCreds(e, cosID)
	if !ok {
		return res
	}
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return blocked(cosID, err.Error())
	}
	r := cosRun{Project: acme.Name, PersonText: "verify-" + e.Nonce + "-person", CosText: "verify-" + e.Nonce + "-cos"}
	// Both connections are dialled before the launch so no state frame is
	// missed; neither joins until the id is known.
	obs, status, err := dialCos(ctx, e, run, false)
	r.DialStatus = status
	if err != nil {
		r.DialErr = err.Error()
		return classifyChiefOfStaff(r)
	}
	defer func() { _ = obs.conn.Close() }()
	cos, status, err := dialCos(ctx, e, run, true)
	r.CosDialStatus = status
	if err != nil {
		r.CosDialErr = err.Error()
		return classifyChiefOfStaff(r)
	}
	defer func() { _ = cos.conn.Close() }()

	body := jsonBody(map[string]any{"projectId": acmeID, "name": "verify-" + e.Nonce + "-cos", "model": agentModel,
		"settings": map[string]bool{"headless": true, "agent": true}})
	r.Create = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions", body, 30*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(r.Create.Body, &created)
	if r.SessionID = created.SessionID; r.Create.Status != http.StatusCreated || r.SessionID == "" {
		return classifyChiefOfStaff(r)
	}
	// Deferred so a panic mid-journey still deletes the session, and the
	// delete ignores cancellation.
	defer func() {
		r.Delete = frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, "/api/sessions/"+r.SessionID, nil)
		out = classifyChiefOfStaff(r)
	}()
	driveChiefOfStaff(ctx, e, obs, cos, run, &r)
	return result{}
}

func driveChiefOfStaff(ctx context.Context, e env, obs, cos *cosConn, run string, r *cosRun) {
	id := r.SessionID
	_ = obs.send(map[string]string{"type": "join_session", "sessionId": id})
	if r.JoinSeen = obs.waitFor(ctx, cosWait, cosJoined(id, 1)); !r.JoinSeen {
		return
	}
	// The person's turn carries a forged origin; relay must ignore it.
	r.Person = frontendDoTimeout(ctx, e, run, http.MethodPost, "/api/sessions/"+id+"/message",
		jsonBody(map[string]string{"text": "Reply with exactly: " + r.PersonText, "origin": cosOrigin}), messageTimeout)
	if r.Person.Status != http.StatusOK {
		return
	}
	if r.PersonIdleErr = !cos.waitFor(ctx, cosWait, cosIdleAfterRunning(id)); r.PersonIdleErr {
		// A rate-limited turn ends errored, never idle; classification still reads its frames.
		r.Frames = obs.snapshot()
		return
	}
	r.Send = cosRequest(ctx, e, run, true, http.MethodPost, "/api/chief-of-staff/messages",
		jsonBody(map[string]string{"sessionId": id, "text": cosSendText(r.CosText)}), frontendRequestTimeout)
	if r.Send.Status == http.StatusAccepted {
		if r.TurnWaitErr = !cos.waitFor(ctx, cosTurnWait, cosTurnFinished(id, r.CosText)); !r.TurnWaitErr {
			r.List = cosRequest(ctx, e, run, true, http.MethodGet, "/api/sessions", nil, frontendRequestTimeout)
			r.ListRow = listedRow(r.List.Body, id)
		}
	}

	r.NegSince = time.Now().Add(-time.Second)
	r.NegProjects = cosRequest(ctx, e, run, true, http.MethodGet, "/api/projects", nil, frontendRequestTimeout)
	r.NegMessage = cosRequest(ctx, e, run, true, http.MethodPost, "/api/sessions/"+id+"/message",
		jsonBody(map[string]string{"text": "must not be sent"}), frontendRequestTimeout)
	r.NegUnscoped = cosRequest(ctx, e, run, false, http.MethodPost, "/api/chief-of-staff/messages",
		jsonBody(map[string]string{"sessionId": id, "text": "must not be sent"}), frontendRequestTimeout)
	if err := cos.send(map[string]string{"type": "join_session", "sessionId": id}); err != nil {
		r.WSJoinWriteErr = err.Error()
	}
	select {
	case <-cos.done:
		r.WSClosed = true
	case <-time.After(cosWait):
	case <-ctx.Done():
	}
	r.WSCloseCode = cos.closeCode()
	r.WSCloseReason = cos.closeReason()

	_ = obs.send(map[string]string{"type": "join_session", "sessionId": id})
	r.RejoinSeen = obs.waitFor(ctx, cosWait, cosJoined(id, 2))
	r.Frames, r.CosFrames = obs.snapshot(), cos.snapshot()

	r.Rows, r.RowsErr = sessionMessageRows(ctx, e, id)
	r.Decisions, r.DecErr = controlDecisionRows(ctx, e)

	if err := obs.send(map[string]string{"type": "end_session", "sessionId": id}); err != nil {
		r.EndErr = "end_session frame: " + err.Error()
		return
	}
	obs.waitFor(ctx, cosWait, func(fs []cosFrame) bool {
		for _, f := range fs {
			if f.Type == "session_state" && f.SessionID == id && f.State == "ended" {
				return true
			}
		}
		return false
	})
}

func auditJSONRows(ctx context.Context, e env, args ...string) ([]audit.AuditEvent, error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, append([]string{"audit"}, args...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("relay audit %s failed", args[1])
	}
	var rows []audit.AuditEvent
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var row audit.AuditEvent
		if json.Unmarshal(line, &row) != nil {
			return nil, errors.New("relay audit printed unreadable JSON")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// sessionMessageRows polls until the completion row for the session is
// written or the wait ends.
func sessionMessageRows(ctx context.Context, e env, id string) ([]audit.AuditEvent, string) {
	for deadline := time.Now().Add(5 * time.Second); ; {
		rows, err := auditJSONRows(ctx, e, "--event", string(audit.AuditEventSessionMessage), "--grep", id, "--json", "--tail", "50")
		if err != nil {
			return nil, err.Error()
		}
		if len(messageRowsFor(rows, id, audit.AuditPhaseCompletion)) > 0 || time.Now().After(deadline) || ctx.Err() != nil {
			return rows, ""
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func controlDecisionRows(ctx context.Context, e env) ([]audit.AuditEvent, string) {
	rows, err := auditJSONRows(ctx, e, "--event", audit.AuditEventControlDecision, "--json", "--tail", "100")
	if err != nil {
		return nil, err.Error()
	}
	return rows, ""
}

type cosArgs struct {
	SessionID string  `json:"session_id"`
	Origin    string  `json:"origin"`
	Text      *string `json:"text"`
	TextBytes int     `json:"text_bytes"`
}

// cosSendText is the text the Chief of Staff's turn sends.
func cosSendText(marker string) string { return "Reply with exactly: " + marker }

func rowArgs(row audit.AuditEvent) cosArgs {
	var a cosArgs
	_ = json.Unmarshal(row.Args, &a)
	return a
}

// messageRowsFor returns the session_message rows of one phase for the session.
func messageRowsFor(rows []audit.AuditEvent, id, phase string) []audit.AuditEvent {
	var out []audit.AuditEvent
	for _, row := range rows {
		if row.Event == string(audit.AuditEventSessionMessage) && row.Phase == phase && rowArgs(row).SessionID == id {
			out = append(out, row)
		}
	}
	return out
}

// deniedWith reports whether a denied control_decision row since the cutoff
// names the method and path and carries the reason.
func deniedWith(rows []audit.AuditEvent, since time.Time, method, path, reason string) bool {
	for _, row := range rows {
		if row.Event == audit.AuditEventControlDecision && row.Outcome == audit.AuditOutcomeDenied && !row.TS.Before(since) &&
			row.Method == method && row.Path == path && strings.Contains(row.Error, reason) {
			return true
		}
	}
	return false
}

// historyMark finds the history entry whose content holds the marker and
// returns its origin.
func historyMark(h []cosHist, marker string) (origin string, found bool) {
	for _, m := range h {
		if m.Role == "user" && strings.Contains(string(m.Content), marker) {
			return m.Origin, true
		}
	}
	return "", false
}

// userFrameOrigins returns the origin of every live user_message frame for the
// session whose text holds the marker.
func userFrameOrigins(fs []cosFrame, id, marker string) []string {
	var out []string
	for _, f := range fs {
		if f.Type == "user_message" && f.SessionID == id && strings.Contains(f.Text, marker) {
			out = append(out, f.Origin)
		}
	}
	return out
}

func classifyChiefOfStaff(r cosRun) result {
	const id = cosID
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	switch {
	case r.DialStatus == http.StatusUnauthorized || r.CosDialStatus == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on /ws")
	case r.DialErr != "" && r.DialStatus == 0:
		return blocked(id, "frontend socket unreachable: "+r.DialErr)
	case r.DialErr != "":
		return fail("GET /ws status %d: %s", r.DialStatus, r.DialErr)
	case r.CosDialErr != "":
		return fail("GET /ws in the chief-of-staff scope: status %d: %s", r.CosDialStatus, r.CosDialErr)
	case r.Create.Status != http.StatusCreated || r.SessionID == "":
		return launchRefusal(id, "/api/sessions", r.Project, r.Create)
	case !r.JoinSeen:
		return fail("no session_joined frame after join_session")
	}
	sid := r.SessionID
	limit := sessionLimit(r.Frames, sid)
	model := ""
	for _, f := range r.Frames {
		if f.Type == "llm_event" && f.SessionID == sid && f.InitModel != "" {
			model = f.InitModel
			break
		}
	}
	switch {
	case r.Person.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on the message route")
	case r.Person.TimedOut:
		return fail("no reply to the person's message within 60 s")
	case r.Person.Status != http.StatusOK:
		return fail("person's message status %d: %s", r.Person.Status, r.Person.Error)
	case model == "":
		return limit.turnFail(id, "no system/init event with a model after the turn")
	case model != agentModelID:
		return blocked(id, "model is not Haiku: "+model)
	case r.Delete.Status/100 != 2:
		return fail("DELETE status %d: session %s may still be running", r.Delete.Status, sid)
	case r.PersonIdleErr:
		return limit.turnFail(id, "no idle session_state frame after the person's turn")
	case r.Send.TimedOut:
		return fail("POST /api/chief-of-staff/messages got no answer")
	case r.Send.Status != http.StatusAccepted:
		return fail("POST /api/chief-of-staff/messages status %d, want 202: %s", r.Send.Status, r.Send.Error)
	}
	var sent struct {
		Origin string `json:"origin"`
	}
	_ = json.Unmarshal(r.Send.Body, &sent)
	switch {
	case sent.Origin != cosOrigin:
		return fail("send answered origin %q, want %q", sent.Origin, cosOrigin)
	case r.TurnWaitErr:
		return limit.turnFail(id, "no turn_done carrying the reply, then idle, on the chief-of-staff connection")
	case r.List.Status != http.StatusOK:
		return fail("scoped GET /api/sessions status %d, want 200", r.List.Status)
	case !r.ListRow.Found:
		return fail("session not in the scoped GET /api/sessions")
	case r.ListRow.State != "idle":
		return fail("scoped list row attention.state %q after the turn, want idle", r.ListRow.State)
	case r.NegProjects.Status != http.StatusForbidden:
		return fail("scoped GET /api/projects status %d, want 403", r.NegProjects.Status)
	case r.NegMessage.Status != http.StatusForbidden:
		return fail("scoped POST /api/sessions/{id}/message status %d, want 403", r.NegMessage.Status)
	case r.NegUnscoped.Status != http.StatusForbidden:
		return fail("unscoped POST /api/chief-of-staff/messages status %d, want 403", r.NegUnscoped.Status)
	case !r.WSClosed:
		return fail("the chief-of-staff /ws stayed open after a join_session frame")
	case r.WSCloseCode != websocket.ClosePolicyViolation:
		return fail("the chief-of-staff /ws closed with code %d, want 1008", r.WSCloseCode)
	case r.WSCloseReason != cosReadOnlyReason:
		return fail("the chief-of-staff /ws closed with reason %q, want %q", r.WSCloseReason, cosReadOnlyReason)
	case countType(r.CosFrames, "session_joined") > 0:
		return fail("a session_joined frame reached the chief-of-staff /ws")
	case !r.RejoinSeen:
		return fail("no session_joined frame after the observer re-joined")
	case r.EndErr != "":
		return fail("%s", r.EndErr)
	case r.RowsErr != "":
		return blocked(id, r.RowsErr)
	case r.DecErr != "":
		return blocked(id, r.DecErr)
	}
	if d := classifyMarks(r); d != "" {
		return fail("%s", d)
	}
	if d := classifyAudit(r); d != "" {
		return fail("%s", d)
	}
	return result{id, statePass, fmt.Sprintf("headless agent in %s on %s: the chief-of-staff send marked in the live frame and the re-joined history, the person's message (forged origin ignored) unmarked; one intent and one completion row; scoped requests refused with the scope and class reasons; the scoped /ws closed 1008 on a write",
		r.Project, model)}
}

func countType(fs []cosFrame, t string) int {
	n := 0
	for _, f := range fs {
		if f.Type == t {
			n++
		}
	}
	return n
}

// classifyMarks checks the mark on the live frames and the re-joined history.
func classifyMarks(r cosRun) string {
	sid := r.SessionID
	for _, c := range []struct{ name, marker, want string }{
		{"person", r.PersonText, ""}, {"chief-of-staff", r.CosText, cosOrigin},
	} {
		live := userFrameOrigins(r.Frames, sid, c.marker)
		if len(live) == 0 {
			return fmt.Sprintf("no live user_message frame for the %s text", c.name)
		}
		for _, o := range live {
			if o != c.want {
				return fmt.Sprintf("live user_message for the %s text carries origin %q, want %q", c.name, o, c.want)
			}
		}
		var hist []cosHist
		for _, f := range r.Frames {
			if f.Type == "session_joined" && f.SessionID == sid {
				hist = f.History // the last join is the re-join
			}
		}
		o, found := historyMark(hist, c.marker)
		if !found {
			return fmt.Sprintf("the re-joined history has no %s message", c.name)
		}
		if o != c.want {
			return fmt.Sprintf("re-joined history marks the %s message origin %q, want %q", c.name, o, c.want)
		}
	}
	return ""
}

// classifyAudit checks the session_message pair and the denied decisions.
func classifyAudit(r cosRun) string {
	sid := r.SessionID
	intents := messageRowsFor(r.Rows, sid, audit.AuditPhaseIntent)
	completions := messageRowsFor(r.Rows, sid, audit.AuditPhaseCompletion)
	if len(intents) != 1 || len(completions) != 1 {
		return fmt.Sprintf("%d intent and %d completion session_message rows for the session, want 1 and 1", len(intents), len(completions))
	}
	in, done := intents[0], completions[0]
	switch {
	case in.ID == "" || in.ID != done.ID:
		return fmt.Sprintf("intent id %q and completion id %q differ or are empty", in.ID, done.ID)
	case in.Outcome != audit.AuditOutcomePending:
		return fmt.Sprintf("intent outcome %q, want pending", in.Outcome)
	case done.Outcome != audit.AuditOutcomeOK:
		return fmt.Sprintf("completion outcome %q, want ok", done.Outcome)
	case in.TS.IsZero() || done.TS.IsZero():
		return "a session_message row has no ts"
	case rowArgs(in).Origin != cosOrigin || rowArgs(done).Origin != cosOrigin:
		return fmt.Sprintf("session_message origin %q (intent) and %q (completion), want %q", rowArgs(in).Origin, rowArgs(done).Origin, cosOrigin)
	}
	for _, row := range r.Rows {
		raw, _ := json.Marshal(row)
		if rowArgs(row).SessionID == sid && strings.Contains(string(raw), r.PersonText) {
			return "a session_message row contains the person's text"
		}
	}
	// The text is logged only with audit.log_args, so its absence is fine;
	// its byte length is always recorded.
	args := rowArgs(in)
	switch {
	case args.TextBytes != len(cosSendText(r.CosText)):
		return fmt.Sprintf("intent text_bytes %d, want %d", args.TextBytes, len(cosSendText(r.CosText)))
	case args.Text != nil && !strings.Contains(*args.Text, cosSendText(r.CosText)):
		return fmt.Sprintf("intent text %q does not contain the chief-of-staff text", *args.Text)
	}
	for _, d := range []struct{ method, path, reason string }{
		{http.MethodGet, "/api/projects", cosDeniedScope},
		{http.MethodPost, "/api/sessions/" + sid + "/message", cosDeniedScope},
		{http.MethodPost, "/api/chief-of-staff/messages", cosDeniedClass},
	} {
		if !deniedWith(r.Decisions, r.NegSince, d.method, d.path, d.reason) {
			return fmt.Sprintf("no denied control_decision row for %s %s with %q", d.method, d.path, d.reason)
		}
	}
	return ""
}
