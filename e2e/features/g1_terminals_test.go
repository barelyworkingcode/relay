package features

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"relaye2e/harness"
)

const (
	// g1tHostDeadline bounds the wait for the session host or model host to
	// register after boot.
	g1tHostDeadline = 60 * time.Second
	// g1tEventDeadline bounds the wait for an event that relay-sessions writes
	// after its answer, or that follows a process ending.
	g1tEventDeadline = 60 * time.Second
	// g1tFrameDeadline bounds one WebSocket frame. It is an upper bound: a
	// frame that is already queued arrives at once.
	g1tFrameDeadline = 30 * time.Second
	// g1tFrameLimit bounds how many frames a turn may send before turn_done.
	g1tFrameLimit = 50
)

// g1tCreds are the planted credentials every test in this file may use.
var g1tCreds = []harness.CredentialSpec{
	{Name: "runner", Classes: []string{"execute"}},
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
	{Name: "mounter", Classes: []string{"proxy"}},
	{Name: "scoped", Classes: []string{"proxy", "execute"}},
}

var g1tApprove = map[string]harness.Outcome{
	"project.grant":        harness.OutcomeApprove,
	"project.reveal_token": harness.OutcomeApprove,
}

// g1tScope is the header that narrows a request to the Chief of Staff scope.
var g1tScope = http.Header{"X-Relay-Scope": {"chief-of-staff"}}

// g1tSandboxTemplates are terminal templates whose commands read and write
// relative to the folder the sandboxed command runs in.
var g1tSandboxTemplates = json.RawMessage(`[
 {"id":"acme-pwd","name":"Acme pwd","command":"/bin/pwd","args":[],"sandbox":true},
 {"id":"acme-other","name":"Acme other","command":"/bin/pwd","args":[],"sandbox":true},
 {"id":"acme-read-own","name":"Acme read own","command":"/bin/cat","args":["own.txt"],"sandbox":true},
 {"id":"acme-read-other","name":"Acme read other","command":"/bin/cat","args":["../acme-b/other.txt"],"sandbox":true},
 {"id":"acme-write-own","name":"Acme write own","command":"/bin/sh","args":["-c","echo acme > written.txt"],"sandbox":true}
]`)

// g1tShellTemplate is a terminal template that keeps running until stopped.
var g1tShellTemplate = json.RawMessage(`[{"id":"acme-shell","name":"Acme shell","command":"/bin/sleep","args":["600"],"sandbox":false}]`)

// g1tEchoHost is a model host with one chat model whose context window is
// large enough that no tool search is needed.
func g1tEchoHost() *harness.FakeModelHostSpec {
	return &harness.FakeModelHostSpec{
		ID:     "acme-models",
		Models: []json.RawMessage{json.RawMessage(`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":8192}`)},
	}
}

type g1tProj struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type g1tRow struct {
	ID       string `json:"id"`
	Live     bool   `json:"live"`
	Headless bool   `json:"headless"`
	Origin   string `json:"origin"`
}

type g1tFrame struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	State     string `json:"state"`
}

// g1tDir is the folder a test project of the given name lives in.
func g1tDir(i *harness.Instance, name string) string {
	return filepath.Join(i.Home, "work", name)
}

// g1tCreate runs `relay project create` with the body and returns the project.
func g1tCreate(t *testing.T, i *harness.Instance, body map[string]any) g1tProj {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the project body: %v", err)
	}
	var p g1tProj
	r := i.CLIWith(harness.CLIOpts{Stdin: b}, "project", "create", "--file", "-", "--json")
	r.JSON(t, &p)
	if r.Code != 0 || p.ID == "" {
		t.Fatalf("project create exited %d and printed no id: %s", r.Code, r.Stdout)
	}
	return p
}

// g1tProject creates a local project whose folder exists, with the extra
// fields added to the body.
func g1tProject(t *testing.T, i *harness.Instance, name string, extra map[string]any) g1tProj {
	t.Helper()
	dir := g1tDir(i, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body := map[string]any{"name": name, "path": dir}
	for k, v := range extra {
		body[k] = v
	}
	return g1tCreate(t, i, body)
}

// g1tStartSession starts a session through the CLI and returns its id.
// settings is a JSON object, or "" for none.
func g1tStartSession(t *testing.T, i *harness.Instance, projectID, model, settings string) string {
	t.Helper()
	args := []string{"session", "start", "--project", projectID, "--model", model, "--json"}
	if settings != "" {
		args = append(args, "--settings", settings)
	}
	var s struct {
		SessionID string `json:"sessionId"`
	}
	i.MustCLI(args...).JSON(t, &s)
	if s.SessionID == "" {
		t.Fatalf("session start on %s printed no sessionId", model)
	}
	return s.SessionID
}

// g1tSessions lists the sessions through the CLI, keyed by id.
func g1tSessions(t *testing.T, i *harness.Instance) map[string]g1tRow {
	t.Helper()
	var list struct {
		Sessions []g1tRow `json:"sessions"`
	}
	i.MustCLI("session", "list", "--json").JSON(t, &list)
	rows := map[string]g1tRow{}
	for _, r := range list.Sessions {
		rows[r.ID] = r
	}
	return rows
}

// g1tEvent returns the first stored event matching q. It fails the test when
// none is stored; use it only for events relay itself writes before answering.
func g1tEvent(t *testing.T, i *harness.Instance, q harness.EventQuery) harness.Event {
	t.Helper()
	got := i.Events(q)
	if len(got) == 0 {
		t.Fatalf("no %s event matched trace %q fields %v", q.Key, q.Trace, q.Fields)
	}
	return got[0]
}

// g1tAuditCount counts the audit rows of an event and outcome.
func g1tAuditCount(i *harness.Instance, event, outcome string) int {
	return len(i.Audit(harness.AuditQuery{Event: event, Outcome: outcome}))
}

// g1tAwaitTurnDone reads frames on ws until the session's turn_done arrives
// and returns the session_state values it saw on the way.
func g1tAwaitTurnDone(t *testing.T, ws *harness.WSConn, sid string) []string {
	t.Helper()
	var states []string
	for n := 0; n < g1tFrameLimit; n++ {
		var f g1tFrame
		if err := json.Unmarshal(ws.Next(g1tFrameDeadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.SessionID != sid {
			continue
		}
		switch f.Type {
		case "session_state":
			states = append(states, f.State)
		case "turn_done":
			return states
		}
	}
	t.Fatalf("no turn_done for session %s in %d frames", sid, g1tFrameLimit)
	return nil
}

// g1tTurn sends one message on ws and waits for its turn_done.
func g1tTurn(t *testing.T, ws *harness.WSConn, sid, text, trace string) []string {
	t.Helper()
	ws.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": text, "trace_id": trace})
	return g1tAwaitTurnDone(t, ws, sid)
}

// g1tToolNames returns the tool names of every chat-completions request in calls.
// It reads the OpenAI shape: tools[].function.name, or tools[].name.
func g1tToolNames(calls []harness.Call) []string {
	var names []string
	for _, c := range calls {
		if c.Method != "POST /v1/chat/completions" {
			continue
		}
		var body struct {
			Tools []struct {
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if json.Unmarshal(c.Params, &body) != nil {
			continue
		}
		for _, tool := range body.Tools {
			if tool.Name != "" {
				names = append(names, tool.Name)
			} else {
				names = append(names, tool.Function.Name)
			}
		}
	}
	return names
}

// g1tChatPosts returns the chat-completions calls the fake model host logged
// after index from.
func g1tChatPosts(i *harness.Instance, from int) []harness.Call {
	calls := i.FakeModelHostCalls()
	if from > len(calls) {
		return nil
	}
	return calls[from:]
}

// g1tCallCount is the number of calls the fake model host has logged, so a
// caller can take the calls a later step adds with g1tChatPosts.
func g1tCallCount(i *harness.Instance) int {
	return len(i.FakeModelHostCalls())
}

func TestSandboxRunsInProject(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds,
		Presence:    g1tApprove,
		Settings:    map[string]json.RawMessage{"terminal_templates": g1tSandboxTemplates},
	})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-sandbox", map[string]any{"allowed_templates": []string{"acme-pwd"}})
	dir := g1tDir(i, "acme-sandbox")

	trace := harness.NewTrace(t)
	if res := i.StartCLITTY(harness.CLIOpts{Trace: trace}, dir, "sandbox", "acme-pwd").Wait(); res.Code != 0 {
		t.Fatalf("relay sandbox of an allowed template from the project folder exited %d, want 0", res.Code)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "sandbox.attach", Trace: trace,
		Fields: map[string]any{"status": "ok", "template": "acme-pwd", "project_id": p.ID}})

	loose := filepath.Join(i.Home, "loose")
	if err := os.MkdirAll(loose, 0o700); err != nil {
		t.Fatalf("creating %s: %v", loose, err)
	}
	trace = harness.NewTrace(t)
	if res := i.StartCLITTY(harness.CLIOpts{Trace: trace}, loose, "sandbox", "acme-pwd").Wait(); res.Code == 0 {
		t.Fatalf("relay sandbox from a folder in no project exited 0, want a refusal")
	}
	g1tEvent(t, i, harness.EventQuery{Key: "sandbox.attach", Trace: trace,
		Fields: map[string]any{"status": "error", "reason": "not_found"}})

	trace = harness.NewTrace(t)
	if res := i.StartCLITTY(harness.CLIOpts{Trace: trace}, dir, "sandbox", "acme-other").Wait(); res.Code == 0 {
		t.Fatalf("relay sandbox of a template outside allowed_templates exited 0, want a refusal")
	}
	g1tEvent(t, i, harness.EventQuery{Key: "sandbox.attach", Trace: trace,
		Fields: map[string]any{"status": "denied"}})
}

func TestSandboxContainment(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds,
		Presence:    g1tApprove,
		Settings:    map[string]json.RawMessage{"terminal_templates": g1tSandboxTemplates},
	})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-a", map[string]any{"allowed_templates": []string{"acme-read-own", "acme-read-other", "acme-write-own"}})
	g1tProject(t, i, "acme-b", map[string]any{"allowed_templates": []string{}})
	if err := os.WriteFile(filepath.Join(g1tDir(i, "acme-b"), "other.txt"), []byte("acme-b\n"), 0o600); err != nil {
		t.Fatalf("writing the other project's file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(g1tDir(i, "acme-a"), "own.txt"), []byte("acme-a\n"), 0o600); err != nil {
		t.Fatalf("writing the project's own file: %v", err)
	}
	dir := g1tDir(i, "acme-a")

	trace := harness.NewTrace(t)
	if res := i.StartCLITTY(harness.CLIOpts{Trace: trace}, dir, "sandbox", "acme-read-own").Wait(); res.Code != 0 {
		t.Fatalf("a sandboxed read of the project's own file exited %d, want 0", res.Code)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "sandbox.attach", Trace: trace,
		Fields: map[string]any{"status": "ok", "project_id": p.ID}})

	if res := i.StartCLITTY(harness.CLIOpts{}, dir, "sandbox", "acme-write-own").Wait(); res.Code != 0 {
		t.Fatalf("a sandboxed write inside the project exited %d, want 0", res.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "written.txt")); err != nil {
		t.Fatalf("the sandboxed write left no file in the project: %v", err)
	}

	trace = harness.NewTrace(t)
	if res := i.StartCLITTY(harness.CLIOpts{Trace: trace}, dir, "sandbox", "acme-read-other").Wait(); res.Code == 0 {
		t.Fatalf("a sandboxed read of another project's file exited 0, want a refusal")
	}
	g1tEvent(t, i, harness.EventQuery{Key: "sandbox.attach", Trace: trace,
		Fields: map[string]any{"status": "ok", "project_id": p.ID}})
}

func TestDropInHandsOver(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials:   g1tCreds,
		Presence:      g1tApprove,
		FakeModelHost: g1tEchoHost(),
	})
	i.WaitModelHost(g1tHostDeadline)
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-dropin", map[string]any{"allowed_templates": []string{"claude-code", "chat"}})
	dir := g1tDir(i, "acme-dropin")

	// A headless session is one started with settings headless: true; agent:
	// true keeps it tracked (docs/routes.md, POST /api/sessions, settings).
	sid := g1tStartSession(t, i, p.ID, "sonnet", `{"headless":true,"agent":true}`)
	i.MustCLI("session", "message", "--id", sid, "--text", "hi", "--json")

	// The /ws connection opens before the drop-in starts, so the hand-back's
	// session_state frame cannot pass it by.
	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	dropStart := time.Now()
	tty := i.StartCLITTY(harness.CLIOpts{}, dir, "drop-in", sid)
	i.WaitEvent(harness.EventQuery{Key: "session.drop_in",
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)
	tty.Hangup()
	tty.Wait()
	if _, ok := g1tSessions(t, i)[sid]; !ok {
		t.Fatalf("session %s is not listed after the terminal closed and handed it back", sid)
	}
	g1tAwaitHandBack(t, ws, sid, dropStart)

	// A handed-back session is dormant, so a resume brings it back, and the
	// resumed session answers a message.
	resumed := i.MustCLI("session", "resume", "--id", sid, "--json")
	var back struct {
		SessionID string `json:"session_id"`
		Resumed   bool   `json:"resumed"`
	}
	resumed.JSON(t, &back)
	if !back.Resumed || back.SessionID != sid {
		t.Fatalf("resume after the hand-back printed %+v, want resumed true for %s", back, sid)
	}
	g1sSendText(t, i, sid, "again", "echo: again")

	chat := g1tStartSession(t, i, p.ID, "fake-echo", "")
	trace := harness.NewTrace(t)
	if res := i.StartCLITTY(harness.CLIOpts{Trace: trace}, dir, "drop-in", chat).Wait(); res.Code == 0 {
		t.Fatalf("drop-in of a chat session exited 0, want a refusal")
	}
	g1tEvent(t, i, harness.EventQuery{Key: "session.drop_in", Trace: trace,
		Fields: map[string]any{"status": "denied", "session_id": chat}})
}

// g1tAwaitHandBack reads /ws frames until sid's session_state reads idle with
// a since stamp after the drop-in began. The idle that the session's last
// message left behind is earlier than that stamp, so it does not count.
func g1tAwaitHandBack(t *testing.T, ws *harness.WSConn, sid string, dropStart time.Time) {
	t.Helper()
	for n := 0; n < g1tFrameLimit; n++ {
		var f struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			State     string `json:"state"`
			Since     string `json:"since"`
		}
		if err := json.Unmarshal(ws.Next(g1tFrameDeadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		if f.Type != "session_state" || f.SessionID != sid || f.State != "idle" {
			continue
		}
		since, err := time.Parse(time.RFC3339Nano, f.Since)
		if err != nil {
			t.Fatalf("session_state since %q is not RFC 3339: %v", f.Since, err)
		}
		if since.After(dropStart) {
			return
		}
	}
	t.Fatalf("session %s did not read idle after the drop-in in %d frames", sid, g1tFrameLimit)
}

func TestLaunchAuditCapped(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-audit", map[string]any{"allowed_templates": []string{"claude-code"}})
	runner := i.SocketHTTP(i.Credential("runner"))

	longID := strings.Repeat("a", 200)
	before := g1tAuditCount(i, "session_launch", "denied")
	trace := harness.NewTrace(t)
	resp := runner.Do("POST", "/api/sessions", map[string]any{"projectId": longID, "model": "sonnet"}, harness.ReqOpts{Trace: trace})
	if resp.Status != 403 {
		t.Fatalf("a launch for an unknown project answered %d, want 403", resp.Status)
	}
	rows := i.Audit(harness.AuditQuery{Event: "session_launch", Outcome: "denied"})
	if len(rows) != before+1 {
		t.Fatalf("a refused launch wrote %d session_launch denied rows, want one more than %d", len(rows)-before, before)
	}
	actor, _ := rows[len(rows)-1]["actor"].(map[string]any)
	got, _ := actor["project_id"].(string)
	// The cap keeps 64 leading runes and ends the value in an ellipsis.
	if !strings.HasSuffix(got, "…") || utf8.RuneCountInString(got) != 65 || !strings.HasPrefix(longID, strings.TrimSuffix(got, "…")) {
		t.Fatalf("the row's project_id is %d runes, want 64 leading runes and an ellipsis", utf8.RuneCountInString(got))
	}

	before = g1tAuditCount(i, "session_launch", "")
	oversized := []byte(`{"projectId":"` + p.ID + `","name":"` + strings.Repeat("x", 1<<20+1) + `"}`)
	big := runner.Do("POST", "/api/sessions", oversized)
	if big.Status != 413 {
		t.Fatalf("a launch body over 1 MiB answered %d, want 413", big.Status)
	}
	if after := g1tAuditCount(i, "session_launch", ""); after != before {
		t.Fatalf("an oversized launch wrote %d session_launch rows, want none", after-before)
	}
}

func TestSessionModeChange(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-mode", map[string]any{"allowed_templates": []string{"claude-code"}})
	sid := g1tStartSession(t, i, p.ID, "sonnet", "")

	for _, mode := range []string{"plan", "acme-not-a-mode"} {
		r := i.CLI("session", "mode", "--id", sid, "--mode", mode, "--json")
		if r.Code != 1 {
			t.Fatalf("a mode change to %q on a local claude session exited %d, want 1 (resume_required)", mode, r.Code)
		}
		g1tEvent(t, i, harness.EventQuery{Key: "session.mode", Trace: r.Trace,
			Fields: map[string]any{"status": "error", "reason": "internal", "session_id": sid}})
	}
	if row, ok := g1tSessions(t, i)[sid]; !ok || !row.Live {
		t.Fatalf("the session is not live after a refused mode change: listed=%v", ok)
	}
}

func TestTemplateListAndGet(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds,
		Presence:    g1tApprove,
		Settings:    map[string]json.RawMessage{"terminal_templates": g1tShellTemplate},
	})
	p := g1tProject(t, i, "acme-templates", map[string]any{"allowed_templates": []string{"acme-shell"}})
	reader := i.SocketHTTP(i.Credential("reader"))

	trace := harness.NewTrace(t)
	list := reader.Do("GET", "/api/terminal/templates?project="+p.ID, nil, harness.ReqOpts{Trace: trace})
	if list.Status != 200 {
		t.Fatalf("GET /api/terminal/templates answered %d, want 200", list.Status)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	list.JSON(t, &rows)
	found := false
	for _, r := range rows {
		found = found || r.ID == "acme-shell"
	}
	if !found {
		t.Fatalf("the template list holds %d rows and not acme-shell", len(rows))
	}
	g1tEvent(t, i, harness.EventQuery{Key: "template.list", Trace: trace,
		Fields: map[string]any{"status": "ok", "count": float64(len(rows))}})

	trace = harness.NewTrace(t)
	one := reader.Do("GET", "/api/terminal/templates/acme-shell", nil, harness.ReqOpts{Trace: trace})
	if one.Status != 200 {
		t.Fatalf("GET one template answered %d, want 200", one.Status)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "template.get", Trace: trace,
		Fields: map[string]any{"status": "ok", "template_id": "acme-shell"}})

	trace = harness.NewTrace(t)
	missing := reader.Do("GET", "/api/terminal/templates/acme-missing", nil, harness.ReqOpts{Trace: trace})
	if missing.Status != 404 {
		t.Fatalf("GET an unknown template answered %d, want 404", missing.Status)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "template.get", Trace: trace,
		Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestTemplateCRUD(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	p := g1tProject(t, i, "acme-crud", map[string]any{"allowed_templates": []string{"*"}})
	cfg := i.SocketHTTP(i.Credential("configurer"))
	reader := i.SocketHTTP(i.Credential("reader"))
	listed := func() []map[string]any {
		t.Helper()
		var rows []map[string]any
		reader.Do("GET", "/api/terminal/templates?project="+p.ID, nil).JSON(t, &rows)
		return rows
	}
	named := func() string {
		for _, row := range listed() {
			if row["id"] == "acme-crud" {
				n, _ := row["name"].(string)
				return n
			}
		}
		return ""
	}

	trace := harness.NewTrace(t)
	created := cfg.Do("POST", "/api/terminal/templates", map[string]any{
		"id": "acme-crud", "name": "Acme CRUD", "command": "/bin/sleep", "args": []string{"600"}, "sandbox": false,
	}, harness.ReqOpts{Trace: trace})
	if created.Status != 201 {
		t.Fatalf("create answered %d, want 201", created.Status)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "template.create", Trace: trace,
		Fields: map[string]any{"status": "ok", "template_id": "acme-crud"}})
	if named() != "Acme CRUD" {
		t.Fatalf("the list does not show the created template")
	}

	trace = harness.NewTrace(t)
	updated := cfg.Do("PUT", "/api/terminal/templates/acme-crud", map[string]any{
		"name": "Acme CRUD v2", "command": "/bin/sleep", "args": []string{"600"}, "sandbox": false,
	}, harness.ReqOpts{Trace: trace})
	if updated.Status != 200 {
		t.Fatalf("update answered %d, want 200", updated.Status)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "template.update", Trace: trace,
		Fields: map[string]any{"status": "ok", "template_id": "acme-crud"}})
	if named() != "Acme CRUD v2" {
		t.Fatalf("the list does not show the updated template")
	}

	trace = harness.NewTrace(t)
	removed := cfg.Do("DELETE", "/api/terminal/templates/acme-crud", nil, harness.ReqOpts{Trace: trace})
	if removed.Status != 204 {
		t.Fatalf("remove answered %d, want 204", removed.Status)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "template.remove", Trace: trace,
		Fields: map[string]any{"status": "ok", "template_id": "acme-crud"}})
	if named() != "" {
		t.Fatalf("the list still shows the removed template")
	}

	trace = harness.NewTrace(t)
	bad := cfg.Do("POST", "/api/terminal/templates", map[string]any{"id": "acme-nameless", "command": "/bin/sleep"}, harness.ReqOpts{Trace: trace})
	if bad.Status != 400 {
		t.Fatalf("a template with no name answered %d, want 400", bad.Status)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "template.create", Trace: trace,
		Fields: map[string]any{"status": "error", "reason": "invalid"}})
}

func TestChatToolSearch(t *testing.T) {
	t.Parallel()
	bulk := harness.Catalogue{}
	for n := 0; n < 4; n++ {
		desc := strings.Repeat("Look up acme records by key and return every field. ", 40)
		bulk.Tools = append(bulk.Tools, json.RawMessage(fmt.Sprintf(
			`{"name":"acme_bulk_%d","description":%q,"inputSchema":{"type":"object","properties":{}}}`, n, desc)))
	}
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds,
		Presence:    g1tApprove,
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: bulk}},
		FakeModelHost: &harness.FakeModelHostSpec{
			ID: "acme-models",
			Models: []json.RawMessage{json.RawMessage(
				`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":64}`)},
		},
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, g1tHostDeadline)
	i.WaitModelHost(g1tHostDeadline)
	i.WaitSessionHost(g1tHostDeadline)

	grant := map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}, "allowed_templates": []string{"chat"}, "allowed_models": []string{"fake-echo"}}
	skilled := g1tProject(t, i, "acme-skilled", grant)
	plain := g1tProject(t, i, "acme-plain", grant)
	i.MustCLI("project", "regen-skill", "--id", skilled.ID, "--json")

	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	settings := `{"useRelayTools":true}`

	sid := g1tStartSession(t, i, skilled.ID, "fake-echo", settings)
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	before := g1tCallCount(i)
	trace := harness.NewTrace(t)
	// A chat session's turn is proven by its chat.turn event. A chat session
	// is not tracked, so it sends no turn_done frame (docs/session-host.md,
	// "Which sessions are tracked"; docs/routes.md, ws:/ws turn_done).
	ws.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "find acme", "trace_id": trace})
	i.WaitEvent(harness.EventQuery{Key: "chat.turn", Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)
	names := g1tToolNames(g1tChatPosts(i, before))
	if !hasAll(names, "tool_search", "call_tool") {
		t.Fatalf("the model received tools %v, want tool_search and call_tool", names)
	}
	for _, n := range names {
		if strings.Contains(n, "acme_bulk_") {
			t.Fatalf("the model received the hidden tool %s with tool search on: %v", n, names)
		}
	}

	plainSID := g1tStartSession(t, i, plain.ID, "fake-echo", settings)
	ws.Send(map[string]any{"type": "join_session", "sessionId": plainSID})
	before = g1tCallCount(i)
	ws.Send(map[string]any{"type": "send_message", "sessionId": plainSID, "text": "find acme", "trace_id": harness.NewTrace(t)})
	i.WaitEvent(harness.EventQuery{Key: "chat.turn", Fields: map[string]any{"status": "ok", "session_id": plainSID}}, g1tEventDeadline)
	names = g1tToolNames(g1tChatPosts(i, before))
	if !hasAll(names, "acme_bulk_0", "acme_bulk_3") {
		t.Fatalf("a project with no skill sent tools %v, want every relay tool", names)
	}
}

func TestCodexModelOutsideAllowedRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-codex", map[string]any{
		"allowed_templates": []string{"codex"},
		"allowed_models":    []string{"codex/other"},
	})
	before := g1tAuditCount(i, "session_launch", "denied")
	trace := harness.NewTrace(t)
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions",
		map[string]any{"projectId": p.ID, "model": "codex/fake-echo"}, harness.ReqOpts{Trace: trace})
	if resp.Status != 403 {
		t.Fatalf("a codex launch outside allowed_models answered %d, want 403", resp.Status)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: trace, Fields: map[string]any{"status": "denied"}})
	if after := g1tAuditCount(i, "session_launch", "denied"); after != before+1 {
		t.Fatalf("the refused launch wrote %d session_launch denied rows, want 1", after-before)
	}
}

func TestAgentStateFrames(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	claude := g1tProject(t, i, "acme-claude", map[string]any{"allowed_templates": []string{"claude-code"}})
	codex := g1tProject(t, i, "acme-codex-states", map[string]any{"allowed_templates": []string{"codex"}})
	claudeSID := g1tStartSession(t, i, claude.ID, "sonnet", "")
	codexSID := g1tStartSession(t, i, codex.ID, "codex/fake-echo", "")

	ws := i.WebSocket("/ws", i.Credential("mounter"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": claudeSID})
	claudeTrace := harness.NewTrace(t)
	states := g1tTurn(t, ws, claudeSID, "hi", claudeTrace)
	if len(states) == 0 {
		t.Fatalf("a claude turn sent no session_state frame")
	}
	i.WaitEvent(harness.EventQuery{Key: "chat.turn", Trace: claudeTrace, Fields: map[string]any{"status": "ok", "session_id": claudeSID}}, g1tEventDeadline)
	i.WaitEvent(harness.EventQuery{Key: "session.state", Fields: map[string]any{"session_id": claudeSID}}, g1tEventDeadline)

	ws.Send(map[string]any{"type": "join_session", "sessionId": codexSID})
	codexTrace := harness.NewTrace(t)
	codexStates := g1tTurn(t, ws, codexSID, "hi", codexTrace)
	for _, s := range codexStates {
		if s == "asking" {
			t.Fatalf("a codex session reported the asking state: %v", codexStates)
		}
	}
	i.WaitEvent(harness.EventQuery{Key: "chat.turn", Trace: codexTrace, Fields: map[string]any{"status": "ok", "session_id": codexSID}}, g1tEventDeadline)
	if got := i.Events(harness.EventQuery{Key: "session.state", Fields: map[string]any{"session_id": codexSID, "to": "asking"}}); len(got) != 0 {
		t.Fatalf("a codex session logged %d session.state changes to asking", len(got))
	}
}

func TestChiefOfStaffScopeDoors(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-cos-scope", map[string]any{"allowed_templates": []string{"claude-code"}})
	sid := g1tStartSession(t, i, p.ID, "sonnet", "")
	scoped := i.SocketHTTP(i.Credential("scoped"))

	if list := scoped.Do("GET", "/api/sessions", nil, harness.ReqOpts{Header: g1tScope}); list.Status != 200 {
		t.Fatalf("a scoped GET /api/sessions answered %d, want 200", list.Status)
	}
	if other := scoped.Do("GET", "/api/projects", nil, harness.ReqOpts{Header: g1tScope}); other.Status != 403 {
		t.Fatalf("a scoped GET /api/projects answered %d, want 403", other.Status)
	}

	before := g1tAuditCount(i, "control_decision", "denied")
	launch := scoped.Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": "sonnet"}, harness.ReqOpts{Header: g1tScope})
	if launch.Status != 403 {
		t.Fatalf("a scoped POST /api/sessions answered %d, want 403", launch.Status)
	}
	if after := g1tAuditCount(i, "control_decision", "denied"); after != before+1 {
		t.Fatalf("the scoped refusal wrote %d control_decision denied rows, want 1", after-before)
	}

	ws := i.WebSocket("/ws", i.Credential("scoped"), harness.ReqOpts{Header: g1tScope})
	defer ws.Close()
	trace := harness.NewTrace(t)
	send := scoped.Do("POST", "/api/chief-of-staff/messages", map[string]any{"sessionId": sid, "text": "acme status"},
		harness.ReqOpts{Header: g1tScope, Trace: trace})
	if send.Status != 202 {
		t.Fatalf("a scoped send answered %d, want 202", send.Status)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "chief_of_staff.send", Trace: trace,
		Fields: map[string]any{"status": "ok", "session_id": sid}})
	g1tAwaitTurnDone(t, ws, sid)

	// The scoped /ws cannot write (docs/THREAT-MODEL.md 3a). This send_message
	// would start a turn if it were honoured, and a turn that runs writes
	// chat.turn with its trace (docs/routes.md, ws:/ws send_message).
	forbidden := harness.NewTrace(t)
	ws.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "acme forbidden", "trace_id": forbidden})

	// Checked before the control turn so a write by the scoped socket is named
	// as such, not reported as a missing control turn. The final check below
	// still runs after the control turn, which covers a turn that lands later.
	if got := i.Events(harness.EventQuery{Key: "chat.turn", Trace: forbidden}); len(got) != 0 {
		t.Fatalf("scoped /ws wrote: %d chat.turn lines for the scoped frame's trace, want none", len(got))
	}

	// Positive control: the same frame on an unscoped /ws with the same
	// credential runs a turn and writes chat.turn. The control is sent after
	// the scoped frame, so a turn the scoped socket had started is ahead of it.
	control := i.WebSocket("/ws", i.Credential("scoped"))
	defer control.Close()
	controlTrace := harness.NewTrace(t)
	g1tTurn(t, control, sid, "acme control", controlTrace)
	i.WaitEvent(harness.EventQuery{Key: "chat.turn", Trace: controlTrace,
		Fields: map[string]any{"status": "ok", "session_id": sid}}, g1tEventDeadline)
	if got := i.Events(harness.EventQuery{Key: "chat.turn", Trace: forbidden}); len(got) != 0 {
		t.Fatalf("a send_message on the scoped /ws wrote %d chat.turn lines, want none", len(got))
	}
}

func TestChiefOfStaffStart(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-cos-start", map[string]any{"allowed_templates": []string{"claude-code"}})
	if err := os.MkdirAll(filepath.Join(g1tDir(i, "acme-cos-start"), "notes"), 0o700); err != nil {
		t.Fatalf("creating the folder inside the project: %v", err)
	}
	outside := filepath.Join(i.Home, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("creating %s: %v", outside, err)
	}
	if err := os.Symlink(outside, filepath.Join(g1tDir(i, "acme-cos-start"), "escape")); err != nil {
		t.Fatalf("linking out of the project: %v", err)
	}
	remote := g1tCreate(t, i, map[string]any{"name": "acme-remote", "kind": "remote"})
	configurer := i.SocketHTTP(i.Credential("configurer"))
	var host struct {
		ID string `json:"id"`
	}
	configurer.Do("POST", "/api/hosts", map[string]any{"name": "acme-box", "target": "acme@127.0.0.1", "port": 2222}).JSON(t, &host)
	if host.ID == "" {
		t.Fatalf("creating the host record printed no id")
	}
	hosted := g1tCreate(t, i, map[string]any{"name": "acme-hosted", "host_id": host.ID, "path": "/srv/acme-hosted", "allowed_templates": []string{"claude-code"}})

	scoped := i.SocketHTTP(i.Credential("scoped"))
	start := func(body any, trace string) harness.Response {
		t.Helper()
		return scoped.Do("POST", "/api/chief-of-staff/sessions", body, harness.ReqOpts{Header: g1tScope, Trace: trace})
	}
	before := g1tAuditCount(i, "session_launch", "ok")

	rootTrace := harness.NewTrace(t)
	root := start(map[string]any{"projectId": p.ID, "prompt": "acme root", "model": "sonnet"}, rootTrace)
	if root.Status != 201 {
		t.Fatalf("a start in the project root answered %d, want 201", root.Status)
	}
	var rootBody struct {
		SessionID string `json:"sessionId"`
	}
	root.JSON(t, &rootBody)
	g1tEvent(t, i, harness.EventQuery{Key: "chief_of_staff.start", Trace: rootTrace,
		Fields: map[string]any{"status": "ok", "session_id": rootBody.SessionID, "project_id": p.ID}})

	inner := start(map[string]any{"projectId": p.ID, "folder": "notes", "prompt": "acme inner", "model": "sonnet"}, harness.NewTrace(t))
	if inner.Status != 201 {
		t.Fatalf("a start in a folder inside the project answered %d, want 201", inner.Status)
	}
	var innerBody struct {
		SessionID string `json:"sessionId"`
	}
	inner.JSON(t, &innerBody)

	rows := g1tSessions(t, i)
	for _, sid := range []string{rootBody.SessionID, innerBody.SessionID} {
		if rows[sid].Origin != "chief-of-staff" {
			t.Fatalf("session %s is listed with origin %q, want chief-of-staff", sid, rows[sid].Origin)
		}
	}
	if after := g1tAuditCount(i, "session_launch", "ok"); after != before+2 {
		t.Fatalf("two starts wrote %d session_launch ok rows, want 2", after-before)
	}

	for _, c := range []struct {
		name string
		body map[string]any
		code int
		key  string
		fld  map[string]any
	}{
		{"a folder that links out", map[string]any{"projectId": p.ID, "folder": "escape", "prompt": "acme out", "model": "sonnet"}, 403, "denied", nil},
		{"a remote project", map[string]any{"projectId": remote.ID, "prompt": "acme remote", "model": "sonnet"}, 403, "denied", nil},
		{"a terminal on a host", map[string]any{"projectId": hosted.ID, "prompt": "acme host", "model": "sonnet", "mode": "terminal"}, 400, "error", map[string]any{"reason": "invalid"}},
		{"a folder with a dot-dot segment", map[string]any{"projectId": p.ID, "folder": "../escape", "prompt": "acme dots", "model": "sonnet"}, 400, "error", map[string]any{"reason": "invalid"}},
	} {
		trace := harness.NewTrace(t)
		r := start(c.body, trace)
		if r.Status != c.code {
			t.Fatalf("%s answered %d, want %d", c.name, r.Status, c.code)
		}
		fields := map[string]any{"status": c.key}
		for k, v := range c.fld {
			fields[k] = v
		}
		g1tEvent(t, i, harness.EventQuery{Key: "chief_of_staff.start", Trace: trace, Fields: fields})
	}

	malformed := scoped.Do("POST", "/api/chief-of-staff/sessions", []byte("{not json"), harness.ReqOpts{Header: g1tScope})
	if malformed.Status != 400 {
		t.Fatalf("a malformed start body answered %d, want 400", malformed.Status)
	}
}

func TestReadOnlyProjectsSession(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	work := g1tProject(t, i, "acme-work", map[string]any{"allowed_templates": []string{"claude-code", "pi"}})
	g1tProject(t, i, "acme-other-proj", map[string]any{"allowed_templates": []string{"claude-code"}})
	note := filepath.Join(g1tDir(i, "acme-other-proj"), "note.txt")
	if err := os.WriteFile(note, []byte("acme note\n"), 0o600); err != nil {
		t.Fatalf("writing the other project's note: %v", err)
	}
	written := filepath.Join(g1tDir(i, "acme-work"), "written.txt")
	runner := i.SocketHTTP(i.Credential("runner"))

	trace := harness.NewTrace(t)
	before := g1tAuditCount(i, "session_launch", "ok")
	launch := runner.Do("POST", "/api/sessions", map[string]any{
		"projectId": work.ID, "model": "sonnet", "settings": map[string]any{"readOnlyProjects": true},
	}, harness.ReqOpts{Trace: trace})
	if launch.Status != 201 {
		t.Fatalf("a read-only claude launch answered %d, want 201", launch.Status)
	}
	var s struct {
		SessionID string `json:"sessionId"`
	}
	launch.JSON(t, &s)
	g1tEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: trace, Fields: map[string]any{"status": "ok", "session_id": s.SessionID}})
	if after := g1tAuditCount(i, "session_launch", "ok"); after != before+1 {
		t.Fatalf("the read-only launch wrote %d session_launch ok rows, want 1", after-before)
	}

	read := i.MustCLI("session", "message", "--id", s.SessionID, "--text", "!sh /bin/cat "+note, "--json")
	var reply struct {
		Text string `json:"text"`
	}
	read.JSON(t, &reply)
	if reply.Text != "exit: 0" {
		t.Fatalf("a read of another project's file answered %q, want exit: 0", reply.Text)
	}

	write := i.MustCLI("session", "message", "--id", s.SessionID, "--text", "!sh touch "+written, "--json")
	write.JSON(t, &reply)
	if reply.Text == "exit: 0" {
		t.Fatalf("a write to a project folder answered exit: 0, want a refusal")
	}
	if _, err := os.Stat(written); err == nil {
		t.Fatalf("the refused write left a file in the project folder")
	}

	pi := runner.Do("POST", "/api/sessions", map[string]any{
		"projectId": work.ID, "model": "pi/fake/fake-echo", "settings": map[string]any{"readOnlyProjects": true},
	}, harness.ReqOpts{Trace: trace})
	if pi.Status != 400 {
		t.Fatalf("a read-only pi launch answered %d, want 400", pi.Status)
	}
	g1tEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: trace,
		Fields: map[string]any{"status": "denied", "reason": "read_only_needs_claude"}})

	g1tProject(t, i, "acme-late", nil)
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": s.SessionID}}, g1tEventDeadline)
}

func TestSessionExitRecorded(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g1tCreds, Presence: g1tApprove})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, "acme-exit", map[string]any{"allowed_templates": []string{"claude-code"}})
	sid := g1tStartSession(t, i, p.ID, "sonnet", "")

	i.MustCLI("session", "stop", "--id", sid, "--json")
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": sid}}, g1tEventDeadline)

	var ended map[string]any
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_end"}) {
		args, _ := row["args"].(map[string]any)
		if args["session_id"] == sid {
			ended = row
		}
	}
	if ended == nil {
		t.Fatalf("no session_end row names session %s", sid)
	}
	if ended["outcome"] != "ok" {
		t.Fatalf("the session_end row's outcome is %v, want ok", ended["outcome"])
	}
	if _, listed := g1tSessions(t, i)[sid]; listed {
		t.Fatalf("session %s is still listed after it exited", sid)
	}
}
