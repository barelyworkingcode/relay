package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
)

const (
	dropInID               = "session-drop-in"
	dropInHostID           = "session-drop-in-host"
	dropInLocalHost        = "console"
	dropInResumeWait       = 20 * time.Second
	dropInRefusedID        = "session-drop-in-tool-refused"
	dropInHostPrefix       = "loopback-"
	dropInHostTarget       = "localhost"
	dropInProjectName      = "Drop-in Host "
	dropInStateDirName     = "grant-dropin-" // matched by verify-fixtures-removed's grant-* sweep
	dropInMarkerWait       = 60 * time.Second
	dropInToolWait         = 60 * time.Second
	dropInRefuseWithin     = 5 * time.Second
	dropInCols, dropInRows = 120, 40
)

// dropFrame is one /ws frame the drop-in journeys care about.
type dropFrame struct {
	Type       string
	SessionID  string
	State      string // session_state
	InitModel  string // llm_event system/init
	ToolName   string // llm_event: a tool_use block finished streaming
	TerminalID string // terminal_*
	Data       []byte // terminal_output data, terminal_joined scrollback
}

func parseDropFrame(raw []byte) dropFrame {
	var f struct {
		Type       string `json:"type"`
		SessionID  string `json:"sessionId"`
		State      string `json:"state"`
		TerminalID string `json:"terminalId"`
		Data       string `json:"data"`
		Scrollback string `json:"scrollback"`
		Event      struct {
			Type             string `json:"type"`
			Subtype          string `json:"subtype"`
			Model            string `json:"model"`
			ContentBlockStop bool   `json:"content_block_stop"`
			ContentBlock     *struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content_block"`
		} `json:"event"`
	}
	_ = json.Unmarshal(raw, &f)
	out := dropFrame{Type: f.Type, SessionID: f.SessionID, State: f.State, TerminalID: f.TerminalID}
	switch f.Type {
	case "terminal_output":
		out.Data, _ = base64.StdEncoding.DecodeString(f.Data)
	case "terminal_joined":
		out.Data, _ = base64.StdEncoding.DecodeString(f.Scrollback)
	case "llm_event":
		switch {
		case f.Event.Type == "system" && f.Event.Subtype == "init":
			out.InitModel = f.Event.Model
		case f.Event.Type == "assistant" && f.Event.ContentBlockStop && f.Event.ContentBlock != nil && f.Event.ContentBlock.Type == "tool_use":
			out.ToolName = f.Event.ContentBlock.Name
		}
	}
	return out
}

type dropLog struct {
	mu     sync.Mutex
	frames []dropFrame
}

func (l *dropLog) add(f dropFrame) {
	l.mu.Lock()
	l.frames = append(l.frames, f)
	l.mu.Unlock()
}

func (l *dropLog) snapshot() []dropFrame {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.frames)
}

func (l *dropLog) waitFor(ctx context.Context, d time.Duration, pred func([]dropFrame) bool) bool {
	return pollUntil(ctx, d, func() bool { return pred(l.snapshot()) })
}

func pollUntil(ctx context.Context, d time.Duration, done func() bool) bool {
	for deadline := time.Now().Add(d); ; {
		if done() {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stateSeq holds when, from index from on, the session reports each state in
// order.
func stateSeq(id string, from int, states ...string) func([]dropFrame) bool {
	return func(fs []dropFrame) bool {
		next := 0
		for i := from; i < len(fs) && next < len(states); i++ {
			if fs[i].Type == "session_state" && fs[i].SessionID == id && fs[i].State == states[next] {
				next++
			}
		}
		return next == len(states)
	}
}

func frameSeen(from int, pred func(dropFrame) bool) func([]dropFrame) bool {
	return func(fs []dropFrame) bool {
		return from < len(fs) && slices.ContainsFunc(fs[from:], pred)
	}
}

func initModel(fs []dropFrame, id string) string {
	for _, f := range fs {
		if f.SessionID == id && f.InitModel != "" {
			return f.InitModel
		}
	}
	return ""
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

// terminalText is everything a terminal printed, escapes removed, from the
// scrollback of its join and every output frame after it.
func terminalText(fs []dropFrame, terminalID string) string {
	var b bytes.Buffer
	for _, f := range fs {
		if f.TerminalID == terminalID && (f.Type == "terminal_joined" || f.Type == "terminal_output") {
			b.Write(f.Data)
		}
	}
	return ansiEscape.ReplaceAllString(b.String(), "")
}

// trustPrompt is Claude's folder-trust question, shown the first time it runs
// interactively in a folder, matched only once its last line has drawn. The
// TUI positions words with cursor moves, so spaces may be missing.
var trustPrompt = regexp.MustCompile(`(?i)trust\s*this\s*folder[\s\S]*enter\s*to\s*confirm`)

// The trust prompt is answered in two writes: Down, then Enter once "Yes" is
// the selected row. The prompt can redraw back on the default "No, exit"
// after a key lands, so each redraw is answered again, a second apart and at
// most trustKeyLimit keys in all.
const (
	keyDown       = "\x1b[B"
	keyEnter      = "\r"
	trustKeyGap   = time.Second
	trustKeyLimit = 8
)

var (
	trustYesSelected = regexp.MustCompile(`^❯\s*Yes,?\s*I\s*trust`)
	trustNoSelected  = regexp.MustCompile(`^❯\s*No,?\s*exit`)
)

// awaitMarker reads text until it holds marker, answering a folder-trust
// prompt with Yes.
func awaitMarker(ctx context.Context, marker string, text func() string, send func(string)) bool {
	var last time.Time
	keys := 0
	return pollUntil(ctx, dropInMarkerWait, func() bool {
		t := text()
		if strings.Contains(t, marker) {
			return true
		}
		row := trustRow(t)
		if keys >= trustKeyLimit || time.Since(last) < trustKeyGap || row == "" || !trustPrompt.MatchString(t) {
			return false
		}
		key := keyDown
		if row == "yes" {
			key = keyEnter
		}
		send(key)
		keys++
		last = time.Now()
		return false
	})
}

// trustRow is the trust prompt's selected row in the last redraw: "yes",
// "no", or "" once the selection marker is on anything else, such as Claude's
// input box after the prompt is answered.
func trustRow(t string) string {
	last := strings.LastIndex(t, "❯")
	if last < 0 {
		return ""
	}
	switch tail := t[last:]; {
	case trustYesSelected.MatchString(tail):
		return "yes"
	case trustNoSelected.MatchString(tail):
		return "no"
	}
	return ""
}

// termOut is the byte stream of a bridge-attached terminal.
type termOut struct {
	mu     sync.Mutex
	b      bytes.Buffer
	exited bool
}

func (o *termOut) add(d []byte) { o.mu.Lock(); o.b.Write(d); o.mu.Unlock() }
func (o *termOut) setExit()     { o.mu.Lock(); o.exited = true; o.mu.Unlock() }
func (o *termOut) hasExit() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.exited
}
func (o *termOut) text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return ansiEscape.ReplaceAllString(o.b.String(), "")
}

// dropInLogLine is one op=session.drop_in line of relay.log.
type dropInLogLine struct {
	Status     string `json:"status"`
	Error      string `json:"error"`
	Host       string `json:"host"`
	TerminalID string `json:"terminal_id"`
}

func parseDropInLog(raw []byte, sessionID string) []dropInLogLine {
	var out []dropInLogLine
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var l struct {
			Op        string `json:"op"`
			SessionID string `json:"session_id"`
			dropInLogLine
		}
		if json.Unmarshal(line, &l) == nil && l.Op == "session.drop_in" && l.SessionID == sessionID {
			out = append(out, l.dropInLogLine)
		}
	}
	return out
}

func readDropInLog(e env, sessionID string) ([]dropInLogLine, error) {
	f, err := os.Open(filepath.Join(e.ConfigDir, "logs", "relay.log"))
	if err != nil {
		return nil, fmt.Errorf("relay log unreadable: %w", err)
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() > toolSearchLogTailSize {
		_, _ = f.Seek(fi.Size()-toolSearchLogTailSize, io.SeekStart)
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("relay log unreadable: %w", err)
	}
	return parseDropInLog(raw, sessionID), nil
}

// waitDropInLog reads the log until the session has a line or 5 s pass: relay
// writes it when the request ends.
func waitDropInLog(ctx context.Context, e env, sessionID string) (lines []dropInLogLine, errText string) {
	pollUntil(ctx, 5*time.Second, func() bool {
		var err error
		lines, err = readDropInLog(e, sessionID)
		errText = ""
		if err != nil {
			errText = err.Error()
		}
		return err != nil || len(lines) > 0
	})
	return lines, errText
}

// listedLive finds the session in a GET /api/sessions body and reports its
// live flag.
func listedLive(body []byte, id string) (found bool, live *bool) {
	var v struct {
		Sessions []struct {
			ID   string `json:"id"`
			Live *bool  `json:"live"`
		} `json:"sessions"`
	}
	_ = json.Unmarshal(body, &v)
	for _, s := range v.Sessions {
		if s.ID == id {
			return true, s.Live
		}
	}
	return false, nil
}

func dialFrontendWS(ctx context.Context, e env, token string) (*websocket.Conn, int, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: frontendRequestTimeout,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", e.FrontendSocket)
		},
	}
	conn, resp, err := dialer.DialContext(ctx, "ws://relay/ws", http.Header{"Authorization": {"Bearer " + token}})
	status := 0
	if resp != nil {
		status = resp.StatusCode
		_ = resp.Body.Close()
	}
	return conn, status, err
}

// collectFrames feeds every frame of conn to log until the connection ends.
func collectFrames(conn *websocket.Conn, log *dropLog) {
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			log.add(parseDropFrame(raw))
		}
	}()
}

func wsSend(conn *websocket.Conn, v any) error {
	return conn.WriteMessage(websocket.TextMessage, jsonBody(v))
}

func launchHeadlessAgent(ctx context.Context, e env, launch, projectID, name string) (frontendResponse, string) {
	resp := frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions", jsonBody(map[string]any{
		"projectId": projectID, "name": name, "model": agentModel,
		"settings": map[string]bool{"headless": true, "agent": true},
	}), 60*time.Second)
	var created struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(resp.Body, &created)
	return resp, created.SessionID
}

// ---- session-drop-in ----

// dropInLeg is everything one leg (local door or host door) saw.
type dropInLeg struct {
	Host, Marker string // expected host in the log line; the turn's marker
	DialStatus   int
	DialErr      string
	Create       frontendResponse
	SessionID    string
	JoinSeen     bool
	Message      frontendResponse
	Frames       []dropFrame
	IdleBefore   bool   // the first turn ended: an idle frame, or on the host door an errored one
	TurnErrored  bool   // host door: the first turn ended errored (the host's claude cannot sign in)
	ClaudeID     string // host door: claudeSessionId from the 201 body
	ResumeSeen   bool   // host door: the host's process table holds claude --resume <ClaudeID>
	ResumeErr    string // host door: the last error reading the process table
	Mark         int    // frames received before the drop-in request
	CheckHeld    bool   // the hold shows: process exit, live:false
	Refusal      string
	TerminalID   string
	HeldRunning  bool
	HeldExited   bool
	HeldList     frontendResponse
	HeldFound    bool
	HeldLive     *bool
	MarkerSeen   bool
	ScreenTail   string // the last of the terminal's text when the marker never came
	ExitSeen     bool   // exit frame or terminal_exit after /exit
	ClosedConn   bool   // local: no exit frame in 15 s, the connection was closed instead
	IdleAfter    bool
	LogLines     []dropInLogLine
	LogErr       string
	CheckAudit   bool
	EndRow       *audit.AuditEvent
	LaunchRow    *audit.AuditEvent
	Delete       frontendResponse
}

type dropInRun struct {
	Project string
	Local   dropInLeg
}

type dropInHostRun struct {
	Setup    string // loopback host or its project could not be set up
	Leg      dropInLeg
	Teardown string
}

func runDropIn(ctx context.Context, e env) result {
	launch, run, res, ok := screenCreds(e, dropInID)
	if !ok {
		return res
	}
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return blocked(dropInID, err.Error())
	}
	r := dropInRun{Project: acme.Name}
	r.Local = driveDropInLeg(ctx, e, launch, run, acmeID, "verify-"+e.Nonce+"-dropin", "verify-"+e.Nonce+"-done", dropInLocalHost, attachLocal(e, run))
	return classifyDropIn(r)
}

func runDropInHost(ctx context.Context, e env) result {
	launch, run, res, ok := screenCreds(e, dropInHostID)
	if !ok {
		return res
	}
	var r dropInHostRun
	r.Setup, r.Leg, r.Teardown = driveHostLeg(ctx, e, launch, run)
	return classifyDropInHost(r)
}

// driveHostLeg adds the loopback host and a project on it, drives the leg and
// removes everything it made, whatever the outcome.
func driveHostLeg(ctx context.Context, e env, launch, run string) (setup string, leg dropInLeg, teardown string) {
	hostName := dropInHostPrefix + e.Nonce
	hostResp := frontendDoTimeout(ctx, e, run, http.MethodPost, "/api/hosts", jsonBody(map[string]string{
		"name": hostName, "target": dropInHostTarget, "tmux_path": "/usr/bin/tmux",
	}), hostCreateTimeout)
	hostID, errText := createdID("POST /api/hosts", hostResp)
	if hostID == "" {
		return errText, leg, ""
	}
	var projectID string
	dir := filepath.Join(gateStateDir(), dropInStateDirName+e.Nonce)
	defer func() {
		teardown = teardownSlowRoute(context.WithoutCancel(ctx), e, run, projectID, hostID)
		if err := os.RemoveAll(dir); err != nil {
			teardown += "; teardown: cannot remove the project folder"
		}
	}()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "cannot create the project folder: " + err.Error(), leg, ""
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	projName := dropInProjectName + e.Nonce
	projResp, d := gatedFrontend(ctx, e, run, http.MethodPost, "/api/projects", jsonBody(map[string]any{
		"name": projName, "path": dir, "host_id": hostID, "allowed_templates": []string{"claude-code"},
	}), fmt.Sprintf("%q", projName))
	if projectID, errText = createdID("POST /api/projects", projResp); projectID == "" {
		return fmt.Sprintf("%s (prompt: %s %s)", errText, d.Outcome, d.Detail), leg, ""
	}
	leg = driveDropInLeg(ctx, e, launch, run, projectID, "verify-"+e.Nonce+"-dropin-host", "verify-"+e.Nonce+"-host", hostName, attachHost(e, launch, run))
	return "", leg, ""
}

type attachFunc func(ctx context.Context, l *dropInLeg, log *dropLog, conn *websocket.Conn)

// driveDropInLeg launches a headless Haiku agent, runs one turn, calls attach
// to take it over, then collects the observations. It stops at the first step
// that failed and leaves the verdict to the classifier. The terminal and the
// session are deleted whatever happened, ignoring cancellation.
func driveDropInLeg(ctx context.Context, e env, launch, run, projectID, name, marker, host string, attach attachFunc) (l dropInLeg) {
	hostDoor := host != dropInLocalHost
	l = dropInLeg{Host: host, Marker: marker, CheckHeld: true, CheckAudit: !hostDoor}
	conn, status, err := dialFrontendWS(ctx, e, run)
	l.DialStatus = status
	if err != nil {
		l.DialErr = err.Error()
		return l
	}
	defer func() { _ = conn.Close() }()
	log := &dropLog{}
	collectFrames(conn, log)

	l.Create, l.SessionID = launchHeadlessAgent(ctx, e, launch, projectID, name)
	if l.Create.Status != http.StatusCreated || l.SessionID == "" {
		return l
	}
	id := l.SessionID
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		if l.TerminalID != "" {
			frontendDo(cleanup, e, run, http.MethodDelete, "/api/terminals/"+l.TerminalID, nil)
		}
		l.Delete = frontendDo(cleanup, e, run, http.MethodDelete, "/api/sessions/"+id, nil)
		l.Frames = log.snapshot()
	}()

	_ = wsSend(conn, map[string]string{"type": "join_session", "sessionId": id})
	l.JoinSeen = log.waitFor(ctx, agentFrameWait, frameSeen(0, func(f dropFrame) bool { return f.Type == "session_joined" && f.SessionID == id }))
	if !l.JoinSeen {
		return l
	}
	l.Message, _ = chatTurn(ctx, e, run, id, "Reply with exactly: "+marker)
	if l.Message.Status != http.StatusOK {
		return l
	}
	turnEnd := stateSeq(id, 0, "running", "idle")
	if hostDoor {
		errored := stateSeq(id, 0, "running", "errored")
		turnEnd = func(fs []dropFrame) bool { return stateSeq(id, 0, "running", "idle")(fs) || errored(fs) }
	}
	if l.IdleBefore = log.waitFor(ctx, agentFrameWait, turnEnd); !l.IdleBefore {
		return l
	}
	l.TurnErrored = hostDoor && !stateSeq(id, 0, "running", "idle")(log.snapshot())
	l.Mark = len(log.snapshot())
	attach(ctx, &l, log, conn)
	if l.TerminalID == "" {
		return l
	}
	l.IdleAfter = log.waitFor(ctx, 20*time.Second, stateSeq(id, l.Mark, "running", "idle"))
	l.LogLines, l.LogErr = waitDropInLog(ctx, e, id)
	if l.CheckAudit {
		l.EndRow = auditEventRow(ctx, e, audit.AuditEventSessionEnd, id)
		l.LaunchRow = auditEventRow(ctx, e, audit.AuditEventSessionLaunch, l.TerminalID)
	}
	return l
}

// observeHold reads what the hold looks like right after the door answered:
// the running frame, the headless process's exit and the list row.
func observeHold(ctx context.Context, e env, run string, l *dropInLeg, log *dropLog) {
	id := l.SessionID
	if l.Host == dropInLocalHost {
		l.HeldRunning = log.waitFor(ctx, agentFrameWait, stateSeq(id, l.Mark, "running"))
	}
	l.HeldExited = log.waitFor(ctx, agentFrameWait, frameSeen(l.Mark, func(f dropFrame) bool { return f.Type == "process_exited" && f.SessionID == id }))
	l.HeldList = frontendDo(ctx, e, run, http.MethodGet, "/api/sessions", nil)
	l.HeldFound, l.HeldLive = listedLive(l.HeldList.Body, id)
}

// attachLocal is the CLI's request on relay.sock.
func attachLocal(e env, run string) attachFunc {
	return func(ctx context.Context, l *dropInLeg, log *dropLog, _ *websocket.Conn) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(e.ConfigDir, "relay.sock"))
		if err != nil {
			l.Refusal = "bridge socket unreachable"
			return
		}
		defer func() { _ = conn.Close() }()
		args, _ := json.Marshal(bridge.DropInAttachRequest{SessionID: l.SessionID, Cols: dropInCols, Rows: dropInRows})
		r := bufio.NewReader(conn)
		var ack bridge.BridgeResponse
		if err = json.NewEncoder(conn).Encode(bridge.BridgeRequest{Type: bridge.ReqDropInAttach, Arguments: args}); err == nil {
			var line []byte
			if line, err = r.ReadBytes('\n'); err == nil {
				err = json.Unmarshal(line, &ack)
			}
		}
		switch {
		case err != nil:
			l.Refusal = "no answer to the attach request"
			return
		case ack.Type != bridge.RespAttached:
			var ref bridge.SandboxRefusal
			if json.Unmarshal(ack.Data, &ref) != nil || ref.Reason == "" {
				ref.Reason = "error"
			}
			l.Refusal = ref.Reason
			return
		}
		var res bridge.DropInAttachResult
		_ = json.Unmarshal(ack.Data, &res)
		l.TerminalID = res.TerminalID
		out := &termOut{}
		go func() {
			for {
				line, err := r.ReadBytes('\n')
				if err != nil {
					return
				}
				var f bridge.StreamFrame
				if json.Unmarshal(line, &f) != nil {
					continue
				}
				switch f.Type {
				case bridge.StreamOutput:
					out.add(f.Data)
				case bridge.StreamExit:
					out.setExit()
					return
				}
			}
		}()
		send := func(s string) {
			_ = json.NewEncoder(conn).Encode(bridge.StreamFrame{Type: bridge.StreamInput, Data: []byte(s)})
		}
		observeHold(ctx, e, run, l, log)
		if l.MarkerSeen = awaitMarker(ctx, l.Marker, out.text, send); !l.MarkerSeen {
			l.ScreenTail = tail(out.text(), 300)
			return
		}
		send("/exit\r")
		if l.ExitSeen = pollUntil(ctx, 15*time.Second, out.hasExit); !l.ExitSeen {
			l.ClosedConn = true // closing the window hands the session back too
		}
	}
}

// attachHost is eve's door: POST /api/sessions/{id}/drop-in, then the
// handoff and the host's --resume launch observed. The launch is read from the
// host's process table over SSH, not from terminal output, which a claude that
// cannot sign in would not show.
func attachHost(e env, launch, run string) attachFunc {
	return func(ctx context.Context, l *dropInLeg, log *dropLog, conn *websocket.Conn) {
		resp := frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions/"+l.SessionID+"/drop-in", []byte(`{}`), 75*time.Second)
		if resp.Status != http.StatusCreated {
			l.Refusal = fmt.Sprintf("status %d %s", resp.Status, resp.Error)
			return
		}
		var body struct {
			ClaudeSessionID string `json:"claudeSessionId"`
			Terminal        struct {
				TerminalID string `json:"terminalId"`
			} `json:"terminal"`
		}
		_ = json.Unmarshal(resp.Body, &body)
		if l.TerminalID = body.Terminal.TerminalID; l.TerminalID == "" {
			l.Refusal = "201 without a terminal id"
			return
		}
		tid := l.TerminalID
		l.ClaudeID = body.ClaudeSessionID
		observeHold(ctx, e, run, l, log)
		if !validUUID(l.ClaudeID) {
			return
		}
		_ = wsSend(conn, map[string]string{"type": "join_terminal", "terminalId": tid})
		l.ResumeSeen = pollUntil(ctx, dropInResumeWait, func() bool {
			var out []byte
			out, l.ResumeErr = hostProcessTable(ctx)
			return resumeLaunched(string(out), l.ClaudeID)
		})
		_ = wsSend(conn, map[string]string{"type": "terminal_input", "terminalId": tid, "data": base64.StdEncoding.EncodeToString([]byte("/exit\r"))})
		exited := func(fs []dropFrame) bool {
			return frameSeen(0, func(f dropFrame) bool { return f.Type == "terminal_exit" && f.TerminalID == tid })(fs)
		}
		if l.ExitSeen = log.waitFor(ctx, 15*time.Second, exited); !l.ExitSeen {
			del := frontendDo(ctx, e, run, http.MethodDelete, "/api/terminals/"+tid, nil)
			l.ExitSeen = del.Status/100 == 2
		}
	}
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validUUID(s string) bool { return uuidPattern.MatchString(s) }

// hostProcessTable is every command line running on the loopback host's login
// user, read the way relay reaches the host.
func hostProcessTable(ctx context.Context) ([]byte, string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", dropInHostTarget, "ps", "-axwwo", "command").Output()
	if err != nil {
		return out, "ssh ps: " + err.Error()
	}
	return out, ""
}

// resumeLaunched holds when a command line in ps output runs claude with
// --resume <id>.
func resumeLaunched(ps, id string) bool {
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || filepath.Base(f[0]) != "claude" {
			continue // the process itself, not a shell or ssh client carrying its command line
		}
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "--resume" && f[i+1] == id {
				return true
			}
		}
	}
	return false
}

func rowReason(row *audit.AuditEvent) string {
	var a struct {
		Reason string `json:"reason"`
	}
	if row != nil {
		_ = json.Unmarshal(row.Args, &a)
	}
	return a.Reason
}

// legProblem is nil when the leg held every promise. A refusal that says the
// world is not as expected reads BLOCKED; anything else is a FAIL. On the host
// door the first turn may end errored, since the host's claude may not be
// signed in; what it proves is the handoff and the --resume launch.
func legProblem(l dropInLeg, local bool) *result {
	id := dropInHostID
	if local {
		id = dropInID
	}
	fail := func(f string, a ...any) *result { return &result{id, stateFail, fmt.Sprintf(f, a...)} }
	block := func(d string) *result { r := blocked(id, d); return &r }
	switch {
	case l.DialStatus == http.StatusUnauthorized:
		return block("run credential refused (401) on /ws")
	case l.DialErr != "" && l.DialStatus == 0:
		return block("frontend socket unreachable: " + l.DialErr)
	case l.DialErr != "":
		return fail("GET /ws status %d: %s", l.DialStatus, l.DialErr)
	case l.Create.Status != http.StatusCreated || l.SessionID == "":
		r := launchRefusal(id, "/api/sessions", l.Host, l.Create)
		if !local && r.State == stateFail {
			r.State = stateBlocked
			r.Detail = "host launch refused (setup P10): " + r.Detail
		}
		return &r
	case !l.JoinSeen:
		return fail("no session_joined frame after join_session")
	case l.Message.Status == http.StatusUnauthorized:
		return block("run credential refused (401) on the message route")
	case l.Message.TimedOut:
		return fail("no reply within 60 s")
	case l.Message.Status != http.StatusOK:
		return fail("message status %d: %s", l.Message.Status, l.Message.Error)
	}
	switch m := initModel(l.Frames, l.SessionID); {
	case m == "":
		return fail("no system/init event with a model after the turn")
	case m != "" && m != agentModelID:
		return block("model is not Haiku: " + m)
	case !l.IdleBefore:
		return fail("the first turn ended neither idle nor errored")
	case l.Refusal == "inside_session" || l.Refusal == "peer_confined":
		return block("drop-in refused " + l.Refusal + ": run the harness from an operator shell")
	case l.Refusal != "":
		return fail("drop-in refused: %s", l.Refusal)
	case local && l.CheckHeld && !l.HeldRunning:
		return fail("no running session_state frame after the drop-in answered")
	case l.CheckHeld && !l.HeldExited:
		return fail("no process_exited frame for the agent after the drop-in answered")
	case l.CheckHeld && l.HeldList.Status != http.StatusOK:
		return fail("GET /api/sessions status %d after the drop-in", l.HeldList.Status)
	case l.CheckHeld && !l.HeldFound:
		return fail("session not in GET /api/sessions after the drop-in")
	case l.CheckHeld && (l.HeldLive == nil || *l.HeldLive):
		return fail("list row live is not false while the terminal holds the session")
	case !local && !validUUID(l.ClaudeID):
		return fail("drop-in answer carries claudeSessionId %q, want a UUID", l.ClaudeID)
	case !local && !l.ResumeSeen:
		return fail("no claude --resume %s in the host's process table within 20 s (%s)", l.ClaudeID, l.ResumeErr)
	case local && !l.MarkerSeen:
		return fail("the first turn's marker %q never appeared in the resumed terminal; it ended with %q", l.Marker, l.ScreenTail)
	case !local && !l.ExitSeen:
		return fail("no terminal_exit within 15 s of /exit, and DELETE /api/terminals failed")
	case !l.IdleAfter:
		return fail("no running then idle session_state frames after the terminal ended")
	case l.LogErr != "":
		return block(l.LogErr)
	case len(l.LogLines) != 1:
		return fail("relay.log has %d op=session.drop_in lines for the session, want 1", len(l.LogLines))
	}
	ll := l.LogLines[0]
	switch {
	case ll.Status != "ok" || ll.Error != "":
		return fail("session.drop_in line status %q error %q, want ok and empty", ll.Status, ll.Error)
	case ll.Host != l.Host:
		return fail("session.drop_in line host %q, want %q", ll.Host, l.Host)
	case ll.TerminalID != l.TerminalID:
		return fail("session.drop_in line terminal_id %q, want %q", ll.TerminalID, l.TerminalID)
	case l.CheckAudit && l.EndRow == nil:
		return fail("no session_end row for the agent within 5 s")
	case l.CheckAudit && rowReason(l.EndRow) != "closed":
		return fail("session_end reason %q, want closed", rowReason(l.EndRow))
	case l.CheckAudit && l.LaunchRow == nil:
		return fail("no session_launch row for the terminal within 5 s")
	case l.CheckAudit && l.LaunchRow.Outcome != "ok":
		return fail("session_launch for the terminal outcome %q", l.LaunchRow.Outcome)
	case l.Delete.Status/100 != 2:
		return fail("DELETE status %d: session %s may still be running", l.Delete.Status, l.SessionID)
	}
	return nil
}

func classifyDropIn(r dropInRun) result {
	if p := legProblem(r.Local, true); p != nil {
		return *p
	}
	path := "/exit ended the terminal"
	if r.Local.ClosedConn {
		path = "no exit frame within 15 s of /exit, so the connection was closed and the hold ended"
	}
	return result{dropInID, statePass, fmt.Sprintf("headless Haiku agent in %s taken over through the bridge (host console): hold, marker in the resumed terminal, %s, idle again, one ok log line, session_end closed, session_launch ok",
		r.Project, path)}
}

func classifyDropInHost(r dropInHostRun) result {
	if r.Setup != "" {
		return blocked(dropInHostID, "loopback host not set up (setup P10): "+r.Setup+r.Teardown)
	}
	if p := legProblem(r.Leg, false); p != nil {
		p.Detail += r.Teardown
		return *p
	}
	if r.Teardown != "" {
		return result{dropInHostID, stateFail, "host leg passed" + r.Teardown}
	}
	return result{dropInHostID, statePass, fmt.Sprintf("agent on host %s taken over through the HTTP door: 201 with a claudeSessionId, headless process stopped and row live false, claude --resume <that id> in the host's process table, terminal ended, idle again, one ok log line",
		r.Leg.Host)}
}

// ---- session-drop-in-tool-refused ----

type toolRefusedRun struct {
	Project     string
	DialStatus  int
	DialErr     string
	Create      frontendResponse
	SessionID   string
	JoinSeen    bool
	Frames      []dropFrame
	TurnEnded   bool             // the message request returned before any tool call
	Turn        frontendResponse // its answer, when TurnEnded
	ToolName    string           // from the tool_use frame; "" when none within 60 s
	DropIn      frontendResponse
	Elapsed     time.Duration
	ListStatus  int
	Found       bool
	Live        *bool
	TermsBefore []string
	TermsAfter  []string
	TermsErr    string
	LogLines    []dropInLogLine
	LogErr      string
	Delete      frontendResponse
}

func runDropInToolRefused(ctx context.Context, e env) (out result) {
	launch, run, res, ok := screenCreds(e, dropInRefusedID)
	if !ok {
		return res
	}
	acme, acmeID, err := grantedAcme(ctx, e)
	if err != nil {
		return blocked(dropInRefusedID, err.Error())
	}
	r := toolRefusedRun{Project: acme.Name}
	conn, status, err := dialFrontendWS(ctx, e, run)
	r.DialStatus = status
	if err != nil {
		r.DialErr = err.Error()
		return classifyToolRefused(r)
	}
	defer func() { _ = conn.Close() }()
	log := &dropLog{}
	collectFrames(conn, log)
	r.Create, r.SessionID = launchHeadlessAgent(ctx, e, launch, acmeID, "verify-"+e.Nonce+"-dropin-tool")
	if r.Create.Status != http.StatusCreated || r.SessionID == "" {
		return classifyToolRefused(r)
	}
	id := r.SessionID
	turnDone := make(chan struct{})
	var turn frontendResponse // written before turnDone closes
	turnEnded := func() bool {
		select {
		case <-turnDone:
			return true
		default:
			return false
		}
	}
	started := false
	defer func() {
		r.Delete = frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, "/api/sessions/"+id, nil)
		// The delete ends the turn, so its request returns; wait so none outlives the journey.
		if started {
			select {
			case <-turnDone:
			case <-time.After(10 * time.Second):
			}
		}
		r.Frames = log.snapshot()
		out = classifyToolRefused(r)
	}()
	_ = wsSend(conn, map[string]string{"type": "join_session", "sessionId": id})
	if r.JoinSeen = log.waitFor(ctx, agentFrameWait, frameSeen(0, func(f dropFrame) bool { return f.Type == "session_joined" && f.SessionID == id })); !r.JoinSeen {
		return result{}
	}
	started = true
	go func() {
		turn, _ = chatTurn(ctx, e, run, id, "Use the Bash tool to run `sleep 20`, then reply with exactly: verify-"+e.Nonce+"-slept")
		close(turnDone)
	}()
	toolFrame := func(f dropFrame) bool { return f.SessionID == id && f.ToolName != "" }
	log.waitFor(ctx, dropInToolWait, func(fs []dropFrame) bool { return frameSeen(0, toolFrame)(fs) || turnEnded() })
	if !frameSeen(0, toolFrame)(log.snapshot()) {
		if r.TurnEnded = turnEnded(); r.TurnEnded {
			r.Turn = turn
		}
		return result{}
	}
	for _, f := range log.snapshot() {
		if toolFrame(f) {
			r.ToolName = f.ToolName
			break
		}
	}
	r.TermsBefore, r.TermsErr = terminalIDs(ctx, e, run)
	start := time.Now()
	r.DropIn = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/sessions/"+id+"/drop-in", []byte(`{}`), 75*time.Second)
	r.Elapsed = time.Since(start)
	list := frontendDo(ctx, e, run, http.MethodGet, "/api/sessions", nil)
	r.ListStatus = list.Status
	r.Found, r.Live = listedLive(list.Body, id)
	var after string
	r.TermsAfter, after = terminalIDs(ctx, e, run)
	r.TermsErr += after
	r.LogLines, r.LogErr = waitDropInLog(ctx, e, id)
	return result{}
}

func terminalIDs(ctx context.Context, e env, token string) ([]string, string) {
	resp := frontendDo(ctx, e, token, http.MethodGet, "/api/terminals", nil)
	if resp.Status != http.StatusOK {
		return nil, fmt.Sprintf("GET /api/terminals status %d", resp.Status)
	}
	return listedIDs(resp.Body, "terminals"), ""
}

func dropInMessage(body []byte) string {
	var v struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &v)
	return v.Message
}

func classifyToolRefused(r toolRefusedRun) result {
	const id = dropInRefusedID
	fail := func(f string, a ...any) result { return result{id, stateFail, fmt.Sprintf(f, a...)} }
	switch {
	case r.DialStatus == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on /ws")
	case r.DialErr != "" && r.DialStatus == 0:
		return blocked(id, "frontend socket unreachable: "+r.DialErr)
	case r.DialErr != "":
		return fail("GET /ws status %d: %s", r.DialStatus, r.DialErr)
	case r.Create.Status != http.StatusCreated || r.SessionID == "":
		return launchRefusal(id, "/api/sessions", r.Project, r.Create)
	case !r.JoinSeen:
		return fail("no session_joined frame after join_session")
	case r.Turn.Status == http.StatusUnauthorized:
		return blocked(id, "run credential refused (401) on the message route")
	case r.ToolName == "":
		return blocked(id, "the agent made no tool call within 60 s")
	}
	switch m := initModel(r.Frames, r.SessionID); {
	case m == "":
		return fail("no system/init event with a model")
	case m != agentModelID:
		return blocked(id, "model is not Haiku: "+m)
	case r.DropIn.TimedOut:
		return fail("drop-in during a tool call did not answer within 75 s")
	case r.DropIn.Status != http.StatusConflict || r.DropIn.Error != "tool_running":
		return fail("drop-in during a tool call: status %d error %q, want 409 tool_running", r.DropIn.Status, r.DropIn.Error)
	case !strings.Contains(dropInMessage(r.DropIn.Body), r.ToolName):
		return fail("tool_running message %q does not name %s", dropInMessage(r.DropIn.Body), r.ToolName)
	case r.Elapsed >= dropInRefuseWithin:
		return fail("tool_running took %s, want under %s", r.Elapsed.Round(100*time.Millisecond), dropInRefuseWithin)
	case r.ListStatus != http.StatusOK:
		return fail("GET /api/sessions status %d after the refusal", r.ListStatus)
	case !r.Found:
		return fail("session not in GET /api/sessions after the refusal")
	case r.Live == nil || !*r.Live:
		return fail("the refusal left the agent not live")
	case r.TermsErr != "":
		return blocked(id, r.TermsErr)
	case !slices.Equal(slices.Sorted(slices.Values(r.TermsBefore)), slices.Sorted(slices.Values(r.TermsAfter))):
		return fail("terminals before the refusal %v, after %v: it must start none", r.TermsBefore, r.TermsAfter)
	case r.LogErr != "":
		return blocked(id, r.LogErr)
	case len(r.LogLines) != 1:
		return fail("relay.log has %d op=session.drop_in lines for the session, want 1", len(r.LogLines))
	case r.LogLines[0].Status != "denied" || r.LogLines[0].Error != "tool_running":
		return fail("session.drop_in line status %q error %q, want denied tool_running", r.LogLines[0].Status, r.LogLines[0].Error)
	case r.Delete.Status/100 != 2:
		return fail("DELETE status %d: session %s may still be running", r.Delete.Status, r.SessionID)
	}
	return result{id, statePass, fmt.Sprintf("drop-in during %s in %s refused 409 tool_running in %s; agent still live, no terminal started, one denied log line",
		r.ToolName, r.Project, r.Elapsed.Round(10*time.Millisecond))}
}

// tail is the last n bytes of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
