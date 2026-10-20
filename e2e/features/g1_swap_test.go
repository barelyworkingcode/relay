package features

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaye2e/harness"
)

// The swap rows of G1.16 and the single start path of G1.26: a drop-in swaps
// a headless session to a terminal, and closing the terminal swaps it back
// with no further message (docs/session-host.md, "Drop-in"; docs/routes.md).

// g1wLocal boots an instance whose project allows the claude-code template,
// starts a headless tracked session and runs one turn, so the session has a
// conversation. It returns the instance, the project folder, the session id
// and the conversation id the headless start named with --session-id.
func g1wLocal(t *testing.T, auditOff bool) (*harness.Instance, string, string, string) {
	t.Helper()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-swap", map[string]any{"allowed_templates": []string{"claude-code", "chat"}})
	if auditOff {
		// Creating a project is a gated write that auditing off refuses, so the
		// project is made first and auditing is turned off in settings.json.
		i.Stop()
		path := filepath.Join(i.ConfigDir, "settings.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading settings.json: %v", err)
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(raw, &settings); err != nil {
			t.Fatalf("decoding settings.json: %v", err)
		}
		settings["audit"] = json.RawMessage(`{"enabled": false}`)
		if raw, err = json.Marshal(settings); err != nil {
			t.Fatalf("encoding settings.json: %v", err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("writing settings.json: %v", err)
		}
		i.Restart()
		i.WaitSessionHost(g1tHostDeadline)
	}
	sid := g1tStartSession(t, i, p.ID, "sonnet", `{"headless":true,"agent":true}`)
	g1sSendText(t, i, sid, "hi", "echo: hi")
	starts := agStartArgvs(i, "claude")
	if len(starts) != 1 {
		t.Fatalf("the session made %d claude starts, want 1", len(starts))
	}
	conv := ""
	for k := 0; k+1 < len(starts[0]); k++ {
		if starts[0][k] == "--session-id" {
			conv = starts[0][k+1]
		}
	}
	if conv == "" {
		t.Fatalf("the headless start %v names no --session-id", starts[0])
	}
	return i, g1tDir(i, "acme-swap"), sid, conv
}

// g1wDropIn takes sid over through the HTTP door and returns the terminal id.
func g1wDropIn(t *testing.T, i *harness.Instance, sid string) string {
	t.Helper()
	resp := i.SocketHTTP(i.Credential("scoped")).Do("POST", "/api/sessions/"+sid+"/drop-in", map[string]any{})
	var out struct {
		Terminal struct {
			TerminalID string `json:"terminalId"`
		} `json:"terminal"`
	}
	resp.JSON(t, &out)
	if resp.Status != http.StatusCreated || out.Terminal.TerminalID == "" {
		t.Fatalf("drop-in answered %d with terminal %q, want 201 and a terminal: %s", resp.Status, out.Terminal.TerminalID, resp.Body)
	}
	return out.Terminal.TerminalID
}

// g1wCloseTerminal ends the drop-in terminal and waits for the hand back that
// follows, so the resumed agent is up before the test ends.
func g1wCloseTerminal(t *testing.T, i *harness.Instance, sid, tid string) harness.Event {
	t.Helper()
	if del := i.SocketHTTP(i.Credential("scoped")).Do("DELETE", "/api/terminals/"+tid, nil); del.Status != http.StatusNoContent {
		t.Fatalf("DELETE /api/terminals/%s answered %d, want 204: %s", tid, del.Status, del.Body)
	}
	return i.WaitEvent(harness.EventQuery{Key: "session.hand_back",
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)
}

func g1wResumeFlags(starts [][]string, conv string) int {
	n := 0
	for _, argv := range starts {
		if agHasPair(argv, "--resume", conv) {
			n++
		}
	}
	return n
}

// g1wNotResumed checks the session is live again: a resume has nothing to do.
func g1wNotResumed(t *testing.T, i *harness.Instance, sid string) {
	t.Helper()
	var back struct {
		SessionID string `json:"session_id"`
		Resumed   bool   `json:"resumed"`
	}
	i.MustCLI("session", "resume", "--id", sid, "--json").JSON(t, &back)
	if back.Resumed || back.SessionID != sid {
		t.Fatalf("resume after the hand back printed %+v, want resumed false for %s", back, sid)
	}
}

// Criteria 1 and 2: closing the drop-in terminal resumes the session headless
// on the same conversation, with no resume by hand.
func TestSwapBackResumesAtOnce(t *testing.T) {
	t.Parallel()
	i, dir, sid, conv := g1wLocal(t, false)

	tty := i.StartCLITTY(harness.CLIOpts{}, dir, "drop-in", sid)
	i.WaitEvent(harness.EventQuery{Key: "session.drop_in",
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)
	// The terminal's claude logs its start once it runs; no event marks it.
	g11dUntil(t, "the drop-in terminal to start claude with --resume "+conv, func() bool {
		return g1wResumeFlags(agStartArgvs(i, "claude"), conv) == 1
	})
	tty.Hangup()
	tty.Wait()

	back := i.WaitEvent(harness.EventQuery{Key: "session.hand_back",
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)
	if back.Str("terminal_id") == "" || back.Str("host") != "console" {
		t.Fatalf("session.hand_back is %v, want a terminal_id and host console", back)
	}

	g1wNotResumed(t, i, sid)
	g1sSendText(t, i, sid, "again", "echo: again")
	starts := agStartArgvs(i, "claude")
	if len(starts) != 3 || g1wResumeFlags(starts, conv) != 2 {
		t.Fatalf("claude starts are %v, want the headless start, the terminal and the resumed headless start, both later ones with --resume %s", starts, conv)
	}
}

// Criterion 2 on a host: the HTTP door's terminal ends and generation g2
// starts on the same conversation.
func TestSwapBackHostResumesAtOnce(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, `{"headless":true,"agent":true}`)
	r.run(t, sid, "g1")
	g1sSendText(t, i, sid, "hi", "echo: hi")
	conv := r.conversationID(t, sid, "g1")

	resp := i.SocketHTTP(i.Credential("scoped")).Do("POST", "/api/sessions/"+sid+"/drop-in", map[string]any{})
	var out struct {
		ClaudeSessionID string `json:"claudeSessionId"`
		Terminal        struct {
			TerminalID string `json:"terminalId"`
		} `json:"terminal"`
	}
	resp.JSON(t, &out)
	if resp.Status != http.StatusCreated || out.Terminal.TerminalID == "" || out.ClaudeSessionID != conv {
		t.Fatalf("drop-in answered %d with terminal %q and conversation %q, want 201, a terminal and %s: %s",
			resp.Status, out.Terminal.TerminalID, out.ClaudeSessionID, conv, resp.Body)
	}
	g11dUntil(t, "the drop-in terminal to start claude with --resume "+conv, func() bool {
		return g1wResumeFlags(r.hostStarts(t), conv) == 1
	})

	g1wCloseTerminal(t, i, sid, out.Terminal.TerminalID)

	if g2 := r.run(t, sid, "g2"); g2.Generation != "g2" {
		t.Fatalf("runner.json after the hand back reads %+v, want generation g2", g2)
	}
	if got := r.hostStarts(t); g1wResumeFlags(got, conv) != 2 {
		t.Fatalf("the host saw starts %v, want the terminal and generation g2 both with --resume %s", got, conv)
	}
	g1wNotResumed(t, i, sid)
	g1sSendText(t, i, sid, "again", "echo: again")
}

// Criterion 3: while a terminal holds the session, every send door answers
// dropped_in.
func TestSwapSendsDroppedInWhileHeld(t *testing.T) {
	t.Parallel()
	i, _, sid, _ := g1wLocal(t, false)
	tid := g1wDropIn(t, i, sid)

	for _, verb := range []string{"message", "send"} {
		res := i.CLI("session", verb, "--id", sid, "--text", "while held", "--json")
		if res.Code != 1 || g1vErrCode(res) != "dropped_in" {
			t.Fatalf("session %s while held exited %d with code %q, want 1 and dropped_in\nstdout: %s\nstderr: %s",
				verb, res.Code, g1vErrCode(res), res.Stdout, res.Stderr)
		}
	}

	ws := i.WebSocket("/ws", i.Credential("scoped"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	ws.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "while held"})
	frames := g1aNext(t, ws, g1tFrameDeadline, func(f map[string]any) bool { return f["type"] == "error" })
	if code := frames[len(frames)-1]["code"]; code != "dropped_in" {
		t.Fatalf("the send_message error frame has code %v, want dropped_in", code)
	}

	cos := i.SocketHTTP(i.Credential("scoped")).Do("POST", "/api/chief-of-staff/messages",
		map[string]any{"sessionId": sid, "text": "while held"}, harness.ReqOpts{Header: g1tScope})
	var refused struct {
		Error string `json:"error"`
	}
	cos.JSON(t, &refused)
	if cos.Status != http.StatusConflict || refused.Error != "dropped_in" {
		t.Fatalf("a Chief of Staff message while held answered %d %q, want 409 dropped_in", cos.Status, refused.Error)
	}
	completions := 0
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_message"}) {
		args, _ := row["args"].(map[string]any)
		if args["session_id"] == sid && row["phase"] == "completion" {
			completions++
			if row["outcome"] != "error" || row["error"] != "dropped_in" {
				t.Fatalf("the session_message completion is %v, want outcome error and error dropped_in", row)
			}
		}
	}
	if completions != 1 {
		t.Fatalf("%d session_message completion rows name the session, want 1", completions)
	}

	g1wCloseTerminal(t, i, sid, tid)
}

// Criterion 6: a session in a turn that waits on a permission is taken after
// the turn ends, not before.
func TestSwapWaitsForTurnEnd(t *testing.T) {
	t.Parallel()
	r := g11dStart(t)
	i := r.i
	sid := r.startSession(t, `{"headless":true,"agent":true}`)
	r.run(t, sid, "g1")

	watch := i.WebSocket("/ws", i.Credential("mounter"))
	defer watch.Close()
	watch.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1vSend(t, i, sid, "!ask Write")
	b367Next(t, watch, sid, "session_state", "asking")

	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	var ask g11rPermission
	for n := 0; ask.PermissionID == ""; n++ {
		if n == 4*g1tFrameLimit {
			t.Fatalf("no permission_request for %s in %d frames after the join", sid, n)
		}
		var f g11rPermission
		if err := json.Unmarshal(ws.Next(g1tFrameDeadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.Type == "permission_request" && f.SessionID == sid {
			ask = f
		}
	}

	answered := make(chan harness.Response, 1)
	go func() {
		answered <- i.SocketHTTP(i.Credential("scoped")).Do("POST", "/api/sessions/"+sid+"/drop-in", map[string]any{})
	}()
	// The to_terminal intent is written before the handoff waits, so a row
	// proves the request is in relay and holding for the turn.
	g4Wait(t, i, g4Has(func(row g4Row) bool {
		return row.obj("args").str("session_id") == sid && row.obj("args").str("direction") == "to_terminal"
	}), "--event", "session_swap")
	if got := i.Events(harness.EventQuery{Key: "session.drop_in", Fields: map[string]any{"session_id": sid}}); len(got) != 0 {
		t.Fatalf("the drop-in answered while the turn still waited on a permission: %v", got)
	}

	approved := time.Now()
	ws.Send(map[string]any{"type": "permission_response", "permissionId": ask.PermissionID, "approved": true})
	resp := <-answered
	var out struct {
		Terminal struct {
			TerminalID string `json:"terminalId"`
		} `json:"terminal"`
	}
	resp.JSON(t, &out)
	if resp.Status != http.StatusCreated || out.Terminal.TerminalID == "" {
		t.Fatalf("drop-in after the turn ended answered %d: %s", resp.Status, resp.Body)
	}

	idle := i.WaitEvent(harness.EventQuery{Key: "session.state", Since: approved,
		Fields: map[string]any{"session_id": sid, "to": "idle"}}, g1tEventDeadline)
	dropped := i.WaitEvent(harness.EventQuery{Key: "session.drop_in",
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)
	idleAt, err1 := time.Parse(time.RFC3339Nano, idle.Str("ts"))
	dropAt, err2 := time.Parse(time.RFC3339Nano, dropped.Str("ts"))
	if err1 != nil || err2 != nil {
		t.Fatalf("event timestamps %q and %q do not parse: %v %v", idle.Str("ts"), dropped.Str("ts"), err1, err2)
	}
	if dropAt.Before(idleAt) {
		t.Fatalf("session.drop_in at %s came before the session read idle at %s", dropAt, idleAt)
	}

	g1wCloseTerminal(t, i, sid, out.Terminal.TerminalID)
}

func g1wTime(t *testing.T, row map[string]any) time.Time {
	t.Helper()
	s, _ := row["ts"].(string)
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("audit row ts %q does not parse: %v", s, err)
	}
	return at
}

// Epic: every swap writes an intent row before it acts, then a completion
// row with the same id, in both directions.
func TestSwapAuditRows(t *testing.T) {
	t.Parallel()
	i, _, sid, _ := g1wLocal(t, false)
	tid := g1wDropIn(t, i, sid)
	g1wCloseTerminal(t, i, sid, tid)

	var rows []map[string]any
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_swap"}) {
		if args, _ := row["args"].(map[string]any); args["session_id"] == sid {
			rows = append(rows, row)
		}
	}
	if len(rows) != 4 {
		t.Fatalf("%d session_swap rows name the session, want 4: %v", len(rows), rows)
	}
	pair := map[string]map[string]map[string]any{}
	for _, row := range rows {
		args, _ := row["args"].(map[string]any)
		dir, _ := args["direction"].(string)
		phase, _ := row["phase"].(string)
		if pair[dir] == nil {
			pair[dir] = map[string]map[string]any{}
		}
		pair[dir][phase] = row
	}
	creds := i.Credential("scoped")
	for _, dir := range []string{"to_terminal", "to_headless"} {
		intent, done := pair[dir]["intent"], pair[dir]["completion"]
		if intent == nil || done == nil {
			t.Fatalf("direction %s has rows %v, want an intent and a completion", dir, pair[dir])
		}
		if intent["id"] == "" || intent["id"] != done["id"] {
			t.Fatalf("%s intent id %v and completion id %v differ", dir, intent["id"], done["id"])
		}
		if intent["outcome"] != "pending" || done["outcome"] != "ok" {
			t.Fatalf("%s outcomes are %v and %v, want pending and ok", dir, intent["outcome"], done["outcome"])
		}
		actor, _ := intent["actor"].(map[string]any)
		if actor["kind"] != "control" || actor["auth"] != "token" || actor["cred_id"] != creds.ID {
			t.Fatalf("%s intent actor is %v, want the drop-in's control credential %s", dir, actor, creds.ID)
		}
		args, _ := intent["args"].(map[string]any)
		if args["project_id"] == "" || args["project_id"] == nil {
			t.Fatalf("%s intent carries no project_id: %v", dir, intent)
		}
		if _, has := args["host_id"]; has {
			t.Fatalf("%s intent of a local project carries host_id: %v", dir, intent)
		}
		doneArgs, _ := done["args"].(map[string]any)
		if doneArgs["terminal_id"] != tid {
			t.Fatalf("%s completion names terminal %v, want %s", dir, doneArgs["terminal_id"], tid)
		}
	}

	var ended, resumed []map[string]any
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_end"}) {
		if args, _ := row["args"].(map[string]any); args["session_id"] == sid {
			ended = append(ended, row)
		}
	}
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_resume"}) {
		if args, _ := row["args"].(map[string]any); args["session_id"] == sid {
			resumed = append(resumed, row)
		}
	}
	if len(ended) == 0 || len(resumed) == 0 {
		t.Fatalf("the swap left %d session_end and %d session_resume rows for the session, want both", len(ended), len(resumed))
	}
	if g1wTime(t, pair["to_terminal"]["intent"]).After(g1wTime(t, ended[0])) {
		t.Fatalf("the to_terminal intent at %v came after the agent's session_end at %v", pair["to_terminal"]["intent"]["ts"], ended[0]["ts"])
	}
	if g1wTime(t, pair["to_headless"]["intent"]).After(g1wTime(t, resumed[0])) {
		t.Fatalf("the to_headless intent at %v came after session_resume at %v", pair["to_headless"]["intent"]["ts"], resumed[0]["ts"])
	}
}

// Epic: with auditing off a drop-in is refused 503 and stops nothing.
func TestSwapRefusedWhenAuditOff(t *testing.T) {
	t.Parallel()
	i, dir, sid, _ := g1wLocal(t, true)
	before := len(agStartArgvs(i, "claude"))

	resp := i.SocketHTTP(i.Credential("scoped")).Do("POST", "/api/sessions/"+sid+"/drop-in", map[string]any{})
	var refused struct {
		Error string `json:"error"`
	}
	resp.JSON(t, &refused)
	if resp.Status != http.StatusServiceUnavailable || refused.Error != "audit_unavailable" {
		t.Fatalf("a drop-in with auditing off answered %d %q, want 503 audit_unavailable: %s", resp.Status, refused.Error, resp.Body)
	}
	if res := i.StartCLITTY(harness.CLIOpts{}, dir, "drop-in", sid).Wait(); res.Code == 0 {
		t.Fatalf("relay drop-in with auditing off exited 0, want a refusal")
	}

	if row, ok := g1tSessions(t, i)[sid]; !ok || !row.Live {
		t.Fatalf("the session is not live after the refused drop-ins: listed=%v", ok)
	}
	g1sSendText(t, i, sid, "still here", "echo: still here")
	if after := len(agStartArgvs(i, "claude")); after != before {
		t.Fatalf("the refused drop-ins caused %d new claude starts, want none", after-before)
	}
	var terminals struct {
		Terminals []json.RawMessage `json:"terminals"`
	}
	i.MustCLI("terminal", "list", "--json").JSON(t, &terminals)
	if len(terminals.Terminals) != 0 {
		t.Fatalf("the refused drop-ins left %d terminals", len(terminals.Terminals))
	}
}

// Criterion 4: a mode terminal start runs the same headless start, local and
// on a host, and delivers the prompt through the send door.
func TestCosStartTerminalModeIsHeadless(t *testing.T) {
	t.Parallel()
	i := cosStart(t, func(o *harness.Options) {
		o.SSHStub = true
		o.FakeModelHost = g1sEchoModels()
	})
	i.WaitModelHost(agHostDeadline)
	local := agProject(t, i, "acme-cos-mode", map[string]any{"allowed_templates": []string{"claude-code", "chat"}})
	agAdd(t, i, local, agEntry{Name: "Researcher", Kind: "claude", Model: "sonnet"})
	hosted, _ := agHost(t, i)

	start := func(body map[string]any) (string, harness.Response) {
		t.Helper()
		body["prompt"] = "acme hello"
		resp := cosDo(t, i, "POST", "/api/chief-of-staff/sessions", body)
		if resp.Status != http.StatusCreated {
			t.Fatalf("a terminal-mode start %v answered %d, want 201: %s", body, resp.Status, resp.Body)
		}
		var out struct {
			SessionID string `json:"sessionId"`
			Mode      string `json:"mode"`
			Origin    string `json:"origin"`
		}
		resp.JSON(t, &out)
		if out.Mode != "headless" || out.Origin != "chief-of-staff" || out.SessionID == "" {
			t.Fatalf("a terminal-mode start answered %+v, want mode headless, origin chief-of-staff and a session", out)
		}
		return out.SessionID, resp
	}
	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"local claude", map[string]any{"projectId": local, "model": "sonnet", "mode": "terminal"}},
		{"entry", map[string]any{"projectId": local, "entry": "Researcher", "mode": "terminal"}},
		{"non-Claude model", map[string]any{"projectId": local, "model": "fake-echo", "mode": "terminal"}},
		{"host", map[string]any{"projectId": hosted, "model": "sonnet", "mode": "terminal"}},
	} {
		sid, _ := start(c.body)
		if row, ok := g1tSessions(t, i)[sid]; !ok || row.Origin != "chief-of-staff" {
			t.Fatalf("%s: session %s is listed=%v with origin %q, want chief-of-staff", c.name, sid, ok, row.Origin)
		}
		var intent, completion string
		for _, row := range i.Audit(harness.AuditQuery{Event: "session_message"}) {
			if args, _ := row["args"].(map[string]any); args["session_id"] == sid {
				if row["phase"] == "intent" {
					intent, _ = row["outcome"].(string)
				}
				if row["phase"] == "completion" {
					completion, _ = row["outcome"].(string)
				}
			}
		}
		if intent != "pending" || completion != "ok" {
			t.Fatalf("%s: session_message outcomes are %q and %q, want a pending intent and an ok completion", c.name, intent, completion)
		}
	}

	var terminals struct {
		Terminals []struct {
			Origin string `json:"origin"`
		} `json:"terminals"`
	}
	i.MustCLI("terminal", "list", "--json").JSON(t, &terminals)
	for _, term := range terminals.Terminals {
		if term.Origin != "" {
			t.Fatalf("terminal list holds a row with origin %q after terminal-mode starts, want none", term.Origin)
		}
	}

	resp := cosDo(t, i, "POST", "/api/chief-of-staff/sessions",
		map[string]any{"projectId": local, "model": "sonnet", "prompt": "acme hello", "mode": "bogus"})
	cosRefusal(t, resp, http.StatusBadRequest, "mode_invalid")
}
