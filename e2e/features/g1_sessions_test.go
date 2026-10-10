package features

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

const (
	// g1sHostDeadline bounds the wait for a session host, model host or
	// manifest to register after boot or restart.
	g1sHostDeadline = 60 * time.Second
	// g1sEventDeadline bounds the wait for an event that relay-sessions writes
	// after its answer.
	g1sEventDeadline = 30 * time.Second
	// g1sTmuxDeadline bounds the bounded poll for a tmux session on the host.
	// tmux gives relay no event, so the poll is the only signal (ruling on G1.12).
	g1sTmuxDeadline = 60 * time.Second
)

// g1sScope narrows a request to the Chief of Staff scope. The runner
// credential holds proxy, which the scope needs.
var g1sScope = http.Header{"X-Relay-Scope": {"chief-of-staff"}}

// g1sStates are the attention states a live tracked session can report
// (docs/session-host.md, "States"). "ended" never reaches a list row.
var g1sStates = []string{"starting", "running", "idle", "asking", "errored", "stalled"}

// g1sSleepTemplate is a terminal template that runs until it is stopped. It
// turns sandboxing off, so a test needs no Seatbelt profile.
var g1sSleepTemplate = json.RawMessage(`[{"id":"acme-sleep","name":"Acme sleep","command":"/bin/sleep","args":["600"],"sandbox":false}]`)

// g1sEchoModels is the fake model host's catalogue: a chat model that answers
// "echo: <text>" and a model relayLLM marks reserved for system use.
func g1sEchoModels() *harness.FakeModelHostSpec {
	return &harness.FakeModelHostSpec{
		ID: "acme-models",
		Models: []json.RawMessage{
			json.RawMessage(`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":8192}`),
			json.RawMessage(`{"id":"acme-system","object":"model","owned_by":"fake","context_length":8192,"system":true}`),
		},
	}
}

// g1sOptions adds the credentials every G1 test uses and approves the one
// owner gate the setup needs, project.grant, for project creation.
func g1sOptions(o harness.Options) harness.Options {
	o.Credentials = []harness.CredentialSpec{
		{Name: "runner", Classes: []string{"execute", "proxy", "read"}},
		{Name: "reader", Classes: []string{"read"}},
		{Name: "configurer", Classes: []string{"configure"}},
	}
	if o.Presence == nil {
		o.Presence = map[string]harness.Outcome{"project.grant": harness.OutcomeApprove}
	}
	return o
}

// g1sCreateProject creates a local project in a folder under the instance's
// home, with the extra body fields, and returns its id.
func g1sCreateProject(t *testing.T, i *harness.Instance, name string, extra map[string]any) string {
	t.Helper()
	dir := filepath.Join(i.Home, "work", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body := map[string]any{"name": name, "path": dir}
	for k, v := range extra {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the project body: %v", err)
	}
	res := i.CLIWith(harness.CLIOpts{Stdin: raw}, "project", "create", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("project create exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var p struct {
		ID string `json:"id"`
	}
	res.JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id: %s", res.Stdout)
	}
	return p.ID
}

// g1sEvent returns the first stored line matching q. A relay-process door
// writes its event before it answers, so the line is already there.
func g1sEvent(t *testing.T, i *harness.Instance, q harness.EventQuery) harness.Event {
	t.Helper()
	got := i.Events(q)
	if len(got) == 0 {
		t.Fatalf("no %s line matching %v for trace %q", q.Key, q.Fields, q.Trace)
	}
	return got[0]
}

// g1sAudit reports whether an audit row of the event and outcome names the
// project as its actor.
func g1sAudit(t *testing.T, i *harness.Instance, event, outcome, projectID string) bool {
	t.Helper()
	for _, row := range i.Audit(harness.AuditQuery{Event: event, Outcome: outcome}) {
		actor, _ := row["actor"].(map[string]any)
		if actor["project_id"] == projectID {
			return true
		}
	}
	return false
}

type g1sSessionRow struct {
	ID           string `json:"id"`
	Live         bool   `json:"live"`
	MessageCount int    `json:"messageCount"`
	Attention    *struct {
		State string `json:"state"`
	} `json:"attention"`
}

type g1sSessionList struct {
	Sessions []g1sSessionRow `json:"sessions"`
}

// g1sListSessions reads relay session list and returns the rows.
func g1sListSessions(t *testing.T, i *harness.Instance) (g1sSessionList, string) {
	t.Helper()
	res := i.MustCLI("session", "list", "--json")
	var out g1sSessionList
	res.JSON(t, &out)
	return out, res.Trace
}

func g1sFindSession(list g1sSessionList, id string) (g1sSessionRow, bool) {
	for _, s := range list.Sessions {
		if s.ID == id {
			return s, true
		}
	}
	return g1sSessionRow{}, false
}

// g1sLaunchClaude starts a claude session over the execute door and returns
// its id.
func g1sLaunchClaude(t *testing.T, i *harness.Instance, projectID string) string {
	t.Helper()
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions",
		map[string]any{"projectId": projectID, "model": "sonnet"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if resp.Status != 201 {
		t.Fatalf("POST /api/sessions for claude answered %d, want 201: %s", resp.Status, resp.Body)
	}
	var s struct {
		SessionID string `json:"sessionId"`
	}
	resp.JSON(t, &s)
	if s.SessionID == "" {
		t.Fatalf("the 201 body holds no sessionId: %s", resp.Body)
	}
	return s.SessionID
}

// g1sSendText sends text to a session through relay session message and
// checks the echo reply.
func g1sSendText(t *testing.T, i *harness.Instance, sessionID, text, want string) harness.Result {
	t.Helper()
	res := i.MustCLI("session", "message", "--id", sessionID, "--text", text, "--json")
	var reply struct {
		Text string `json:"text"`
	}
	res.JSON(t, &reply)
	if reply.Text != want {
		t.Fatalf("reply text %q, want %q", reply.Text, want)
	}
	return res
}

// g1sStartTerminal starts the sleep template in a project and returns the
// terminal id and the CLI trace.
func g1sStartTerminal(t *testing.T, i *harness.Instance, projectID string) (string, string) {
	t.Helper()
	res := i.MustCLI("terminal", "start", "--project", projectID, "--template", "acme-sleep", "--json")
	var out struct {
		TerminalID string `json:"terminalId"`
	}
	res.JSON(t, &out)
	if out.TerminalID == "" {
		t.Fatalf("terminal start printed no terminalId: %s", res.Stdout)
	}
	return out.TerminalID, res.Trace
}

// g1sCreateHost registers the loopback sshd as a host, adds a persistent
// tmux template to it and returns the host id.
func g1sCreateHost(t *testing.T, i *harness.Instance, ssh *harness.SSHHost) string {
	t.Helper()
	i.TrustSSHHost(ssh)
	c := i.HTTP(i.Credential("configurer"))
	create := c.Do("POST", "/api/hosts", map[string]any{
		"name": "acme-box", "target": ssh.Target, "port": ssh.Port, "identity_file": ssh.IdentityFile,
	})
	if create.Status != 201 {
		t.Fatalf("POST /api/hosts answered %d, want 201: %s", create.Status, create.Body)
	}
	var host struct {
		ID string `json:"id"`
	}
	create.JSON(t, &host)
	tmpl := c.Do("POST", "/api/hosts/"+host.ID+"/templates", map[string]any{
		"id": "acme-tmux", "name": "Acme tmux", "command": "/bin/sleep", "args": []string{"600"}, "persist": true,
	})
	if tmpl.Status != 201 {
		t.Fatalf("POST host template answered %d, want 201: %s", tmpl.Status, tmpl.Body)
	}
	return host.ID
}

type g1sPersistentRow struct {
	Name       string `json:"name"`
	TemplateID string `json:"template_id"`
	N          int    `json:"n"`
}

// g1sAwaitPersistent polls relay terminal persistent-list until the tmux
// session of the acme-tmux terminal shows up on the host. This is a bounded
// poll: tmux gives relay no event (ruling on G1.12). It returns the row and
// the trace of the list call that found it.
func g1sAwaitPersistent(t *testing.T, i *harness.Instance, projectID string) (g1sPersistentRow, string) {
	t.Helper()
	deadline := time.Now().Add(g1sTmuxDeadline)
	for {
		res := i.CLI("terminal", "persistent-list", "--project", projectID, "--json")
		if res.Code != 0 {
			t.Fatalf("persistent-list exited %d\nstderr: %s", res.Code, res.Stderr)
		}
		var rows []g1sPersistentRow
		res.JSON(t, &rows)
		for _, r := range rows {
			if strings.HasSuffix(r.Name, "-acme-tmux-1") {
				return r, res.Trace
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no acme-tmux persistent session on the host within %v", g1sTmuxDeadline)
		}
	}
}

func TestChatSessionAnswers(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{FakeModelHost: g1sEchoModels()}))
	i.WaitModelHost(g1sHostDeadline)
	i.WaitSessionHost(g1sHostDeadline)
	projectID := g1sCreateProject(t, i, "acme-chat", map[string]any{"allowed_templates": []string{"chat"}})

	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions",
		map[string]any{"projectId": projectID, "model": "fake-echo"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if resp.Status != 201 {
		t.Fatalf("a chat launch answered %d, want 201: %s", resp.Status, resp.Body)
	}
	var s struct {
		SessionID string `json:"sessionId"`
	}
	resp.JSON(t, &s)
	if s.SessionID == "" {
		t.Fatalf("the chat launch returned no sessionId: %s", resp.Body)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace,
		Fields: map[string]any{"status": "ok", "session_id": s.SessionID}})
	if !g1sAudit(t, i, "session_launch", "ok", projectID) {
		t.Fatalf("no ok session_launch audit row names project %s", projectID)
	}
	g1sSendText(t, i, s.SessionID, "hi", "echo: hi")
}

func TestSessionLaunchUngrantedRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)
	// No allowed_templates: the project grants no template to a claude launch.
	projectID := g1sCreateProject(t, i, "acme-ungranted", nil)

	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions",
		map[string]any{"projectId": projectID, "model": "sonnet"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if resp.Status != 403 {
		t.Fatalf("a launch in an ungranted project answered %d, want 403: %s", resp.Status, resp.Body)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace,
		Fields: map[string]any{"status": "denied", "reason": "template_not_allowed"}})
	if !g1sAudit(t, i, "session_launch", "denied", projectID) {
		t.Fatalf("no denied session_launch audit row names project %s", projectID)
	}
	if list, _ := g1sListSessions(t, i); len(list.Sessions) != 0 {
		t.Fatalf("a refused launch left %d sessions", len(list.Sessions))
	}
}

func TestSessionBlankModelRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)
	projectID := g1sCreateProject(t, i, "acme-blank", map[string]any{"allowed_templates": []string{"chat"}})

	// A blank model is a chat launch (docs/session-host.md, "A session names its model").
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions",
		map[string]any{"projectId": projectID, "model": ""}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if resp.Status != 400 {
		t.Fatalf("a blank-model launch answered %d, want 400: %s", resp.Status, resp.Body)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace,
		Fields: map[string]any{"status": "denied", "reason": "model_required"}})
	if !g1sAudit(t, i, "session_launch", "error", projectID) {
		t.Fatalf("no error session_launch audit row names project %s", projectID)
	}
	if list, _ := g1sListSessions(t, i); len(list.Sessions) != 0 {
		t.Fatalf("a refused launch left %d sessions", len(list.Sessions))
	}
}

func TestSystemModelChatRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{FakeModelHost: g1sEchoModels()}))
	i.WaitModelHost(g1sHostDeadline)
	i.WaitSessionHost(g1sHostDeadline)
	projectID := g1sCreateProject(t, i, "acme-system", map[string]any{"allowed_templates": []string{"chat"}})
	runner := i.SocketHTTP(i.Credential("runner"))

	refused := runner.Do("POST", "/api/sessions",
		map[string]any{"projectId": projectID, "model": "acme-system"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if refused.Status != 403 {
		t.Fatalf("a chat launch on a system model answered %d, want 403: %s", refused.Status, refused.Body)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: refused.Trace,
		Fields: map[string]any{"status": "denied", "reason": "model_system_only"}})
	if !g1sAudit(t, i, "session_launch", "denied", projectID) {
		t.Fatalf("no denied session_launch audit row names project %s", projectID)
	}

	// The same launch on a model that is not system-only starts, so the refusal
	// above comes from the system flag and not from the chat launch itself.
	ok := runner.Do("POST", "/api/sessions",
		map[string]any{"projectId": projectID, "model": "fake-echo"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if ok.Status != 201 {
		t.Fatalf("a chat launch on fake-echo answered %d, want 201: %s", ok.Status, ok.Body)
	}
	list, _ := g1sListSessions(t, i)
	if len(list.Sessions) != 1 {
		t.Fatalf("after one refused and one started launch the list holds %d sessions, want 1", len(list.Sessions))
	}
}

func TestSessionListAndAuth(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)
	projectID := g1sCreateProject(t, i, "acme-list", map[string]any{"allowed_templates": []string{"claude-code"}})
	id := g1sLaunchClaude(t, i, projectID)

	list, trace := g1sListSessions(t, i)
	row, ok := g1sFindSession(list, id)
	if !ok {
		t.Fatalf("session list holds %d rows and not %s", len(list.Sessions), id)
	}
	if !row.Live {
		t.Fatalf("a launched session is listed as not live")
	}
	if row.Attention == nil || !containsString(g1sStates, row.Attention.State) {
		t.Fatalf("a live claude session carries attention %+v, want one of %v", row.Attention, g1sStates)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.list", Trace: trace,
		Fields: map[string]any{"status": "ok", "count": len(list.Sessions)}})

	runner := i.SocketHTTP(i.Credential("runner"))
	if resp := runner.Do("GET", "/api/sessions", nil); resp.Status != 200 {
		t.Fatalf("GET /api/sessions with the execute and proxy class answered %d, want 200", resp.Status)
	}
	if resp := i.SocketHTTP(harness.Credential{}).Do("GET", "/api/sessions", nil); resp.Status != 401 {
		t.Fatalf("GET /api/sessions with no credential answered %d, want 401", resp.Status)
	}
	if resp := i.SocketHTTP(i.Credential("reader")).Do("GET", "/api/sessions", nil); resp.Status != 403 {
		t.Fatalf("GET /api/sessions with a read-only credential answered %d, want 403", resp.Status)
	}
}

func containsString(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

func TestSessionMessage(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)
	projectID := g1sCreateProject(t, i, "acme-message", map[string]any{"allowed_templates": []string{"claude-code"}})
	id := g1sLaunchClaude(t, i, projectID)

	sent := g1sSendText(t, i, id, "hi", "echo: hi")
	i.WaitEvent(harness.EventQuery{Key: "session.message", Trace: sent.Trace,
		Fields: map[string]any{"status": "ok", "session_id": id}}, g1sEventDeadline)

	if r := i.CLI("session", "message", "--id", "acme-missing-session", "--text", "hi"); r.Code != 1 {
		t.Fatalf("a message to an unknown session exited %d, want 1", r.Code)
	}
	missing := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions/acme-missing-session/message",
		map[string]any{"text": "hi"})
	if missing.Status != 404 {
		t.Fatalf("a message to an unknown session answered %d, want 404", missing.Status)
	}
}

func TestSessionStop(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)
	projectID := g1sCreateProject(t, i, "acme-stop", map[string]any{"allowed_templates": []string{"claude-code"}})
	id := g1sLaunchClaude(t, i, projectID)

	stop := i.MustCLI("session", "stop", "--id", id, "--json")
	var stopped struct {
		ID string `json:"id"`
	}
	stop.JSON(t, &stopped)
	if stopped.ID != id {
		t.Fatalf("session stop named %q, want %q", stopped.ID, id)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.delete", Trace: stop.Trace,
		Fields: map[string]any{"status": "ok", "session_id": id}}, g1sEventDeadline)
	if list, _ := g1sListSessions(t, i); len(list.Sessions) != 0 {
		if _, still := g1sFindSession(list, id); still {
			t.Fatalf("a stopped session is still listed")
		}
	}

	// A stop of an id the host does not hold still succeeds.
	if r := i.CLI("session", "stop", "--id", "acme-missing-session"); r.Code != 0 {
		t.Fatalf("stopping an unknown session exited %d, want 0", r.Code)
	}
	gone := i.SocketHTTP(i.Credential("runner")).Do("DELETE", "/api/sessions/acme-missing-session", nil)
	if gone.Status != 204 {
		t.Fatalf("DELETE of an unknown session answered %d, want 204", gone.Status)
	}
}

func TestSessionResume(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)
	projectID := g1sCreateProject(t, i, "acme-resume", map[string]any{"allowed_templates": []string{"claude-code"}})
	id := g1sLaunchClaude(t, i, projectID)
	g1sSendText(t, i, id, "hi", "echo: hi")

	before, _ := g1sListSessions(t, i)
	beforeRow, ok := g1sFindSession(before, id)
	if !ok {
		t.Fatalf("session list holds no %s before the restart", id)
	}

	// Every session is dormant after a relay restart. Wait for the session host
	// to register again: the earlier registration line is already in the log.
	since := time.Now()
	i.Restart()
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Since: since,
		Fields: map[string]any{"service_id": "relaysessions", "status": "ok"}}, g1sHostDeadline)

	dormant, _ := g1sListSessions(t, i)
	row, ok := g1sFindSession(dormant, id)
	if !ok {
		t.Fatalf("session list holds no %s after the restart", id)
	}
	if row.Live {
		t.Fatalf("a session is live after a relay restart")
	}

	res := i.MustCLI("session", "resume", "--id", id, "--json")
	var resumed struct {
		SessionID string `json:"session_id"`
		Resumed   bool   `json:"resumed"`
	}
	res.JSON(t, &resumed)
	if !resumed.Resumed || resumed.SessionID != id {
		t.Fatalf("resume of a dormant session printed %+v, want resumed true for %s", resumed, id)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.resume", Trace: res.Trace,
		Fields: map[string]any{"status": "ok", "session_id": id}})
	if !g1sAudit(t, i, "session_resume", "ok", projectID) {
		t.Fatalf("no ok session_resume audit row names project %s", projectID)
	}

	after, _ := g1sListSessions(t, i)
	afterRow, ok := g1sFindSession(after, id)
	if !ok || afterRow.MessageCount != beforeRow.MessageCount {
		t.Fatalf("resumed session has %d messages, want the %d it had before the restart", afterRow.MessageCount, beforeRow.MessageCount)
	}
	g1sSendText(t, i, id, "again", "echo: again")

	again := i.MustCLI("session", "resume", "--id", id, "--json")
	var live struct {
		Resumed bool `json:"resumed"`
	}
	again.JSON(t, &live)
	if live.Resumed {
		t.Fatalf("resume of a live session answered resumed true")
	}

	stop := i.MustCLI("session", "stop", "--id", id)
	i.WaitEvent(harness.EventQuery{Key: "session.delete", Trace: stop.Trace,
		Fields: map[string]any{"status": "ok", "session_id": id}}, g1sEventDeadline)
	if r := i.CLI("session", "resume", "--id", id); r.Code != 1 {
		t.Fatalf("resume of a stopped session exited %d, want 1", r.Code)
	}
	gone := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions/"+id+"/resume", nil)
	if gone.Status != 404 {
		t.Fatalf("resume of a stopped session answered %d, want 404", gone.Status)
	}
	if rows := i.Audit(harness.AuditQuery{Event: "session_resume", Outcome: "not_found"}); len(rows) == 0 {
		t.Fatalf("no not_found session_resume audit row for the stopped session")
	}
}

func TestTerminalStart(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{
		Settings: map[string]json.RawMessage{"terminal_templates": g1sSleepTemplate},
	}))
	i.WaitSessionHost(g1sHostDeadline)
	granted := g1sCreateProject(t, i, "acme-term-granted", map[string]any{"allowed_templates": []string{"acme-sleep"}})
	ungranted := g1sCreateProject(t, i, "acme-term-ungranted", nil)

	terminalID, trace := g1sStartTerminal(t, i, granted)
	if terminalID == "" {
		t.Fatalf("terminal start returned no id")
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: trace,
		Fields: map[string]any{"status": "ok", "kind": "pty"}})
	if !g1sAudit(t, i, "session_launch", "ok", granted) {
		t.Fatalf("no ok session_launch audit row names project %s", granted)
	}

	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/terminals",
		map[string]any{"projectId": ungranted, "templateId": "acme-sleep"}, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if resp.Status != 403 {
		t.Fatalf("a terminal from a template the project does not allow answered %d, want 403: %s", resp.Status, resp.Body)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace,
		Fields: map[string]any{"status": "denied", "reason": "template_not_allowed"}})
}

func TestTerminalList(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{
		Settings:      map[string]json.RawMessage{"terminal_templates": g1sSleepTemplate},
		FakeModelHost: g1sEchoModels(),
	}))
	i.WaitSessionHost(g1sHostDeadline)
	i.WaitModelHost(g1sHostDeadline)
	projectID := g1sCreateProject(t, i, "acme-term-list", map[string]any{"allowed_templates": []string{"acme-sleep", "claude-code"}})
	terminalID, _ := g1sStartTerminal(t, i, projectID)

	// A Chief of Staff terminal carries the chief-of-staff origin in the list
	// (docs/session-host.md, "Session origin"). Its start answers 201.
	cos := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/chief-of-staff/sessions",
		map[string]any{"projectId": projectID, "prompt": "acme hello", "model": "sonnet", "mode": "terminal"},
		harness.ReqOpts{Header: g1sScope, Trace: harness.NewTrace(t)})
	if cos.Status != 201 {
		t.Fatalf("a Chief of Staff terminal start answered %d, want 201: %s", cos.Status, cos.Body)
	}

	res := i.MustCLI("terminal", "list", "--json")
	var out struct {
		Terminals []struct {
			ID         string `json:"id"`
			TemplateID string `json:"templateId"`
			Origin     string `json:"origin"`
		} `json:"terminals"`
	}
	res.JSON(t, &out)
	found := false
	cosRows := 0
	for _, term := range out.Terminals {
		if term.ID == terminalID {
			found = term.TemplateID == "acme-sleep"
			if term.Origin != "" {
				t.Fatalf("a person's terminal row %s has origin %q, want none", terminalID, term.Origin)
			}
		}
		if term.Origin == "chief-of-staff" {
			cosRows++
			if term.TemplateID != "claude-code" {
				t.Fatalf("the Chief of Staff terminal row has template %q, want claude-code", term.TemplateID)
			}
		}
	}
	if !found {
		t.Fatalf("terminal list holds %d terminals and not %s with template acme-sleep", len(out.Terminals), terminalID)
	}
	if cosRows != 1 {
		t.Fatalf("terminal list holds %d rows with origin chief-of-staff, want 1", cosRows)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "terminal.list", Trace: res.Trace,
		Fields: map[string]any{"status": "ok", "count": len(out.Terminals)}})
}

func TestTerminalLog(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{
		Settings: map[string]json.RawMessage{"terminal_templates": g1sSleepTemplate},
	}))
	i.WaitSessionHost(g1sHostDeadline)
	projectID := g1sCreateProject(t, i, "acme-term-log", map[string]any{"allowed_templates": []string{"acme-sleep"}})
	terminalID, _ := g1sStartTerminal(t, i, projectID)

	res := i.MustCLI("terminal", "log", "--id", terminalID, "--json")
	var out map[string]any
	res.JSON(t, &out)
	if out["id"] != terminalID {
		t.Fatalf("terminal log names %v, want %s", out["id"], terminalID)
	}
	if _, ok := out["log"].(string); !ok {
		t.Fatalf("terminal log holds no log string: %v", out)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "terminal.log", Trace: res.Trace,
		Fields: map[string]any{"status": "ok", "terminal_id": terminalID}})

	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("reading random bytes for a terminal id: %v", err)
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	unknown := fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])

	if r := i.CLI("terminal", "log", "--id", unknown); r.Code != 1 {
		t.Fatalf("log of an unknown terminal exited %d, want 1", r.Code)
	}
	missing := i.SocketHTTP(i.Credential("runner")).Do("GET", "/api/terminals/"+unknown+"/log", nil)
	if missing.Status != 404 {
		t.Fatalf("log of an unknown terminal answered %d, want 404", missing.Status)
	}
	malformed := i.SocketHTTP(i.Credential("runner")).Do("GET", "/api/terminals/acme-missing-terminal/log", nil)
	if malformed.Status != 400 {
		t.Fatalf("log of a malformed terminal id answered %d, want 400", malformed.Status)
	}
}

func TestTerminalStop(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{
		Settings: map[string]json.RawMessage{"terminal_templates": g1sSleepTemplate},
	}))
	i.WaitSessionHost(g1sHostDeadline)
	projectID := g1sCreateProject(t, i, "acme-term-stop", map[string]any{"allowed_templates": []string{"acme-sleep"}})
	terminalID, _ := g1sStartTerminal(t, i, projectID)

	stop := i.MustCLI("terminal", "stop", "--id", terminalID, "--json")
	var stopped struct {
		ID string `json:"id"`
	}
	stop.JSON(t, &stopped)
	if stopped.ID != terminalID {
		t.Fatalf("terminal stop named %q, want %q", stopped.ID, terminalID)
	}
	i.WaitEvent(harness.EventQuery{Key: "terminal.delete", Trace: stop.Trace,
		Fields: map[string]any{"status": "ok", "terminal_id": terminalID}}, g1sEventDeadline)

	list := i.MustCLI("terminal", "list", "--json")
	var out struct {
		Terminals []struct {
			ID string `json:"id"`
		} `json:"terminals"`
	}
	list.JSON(t, &out)
	for _, term := range out.Terminals {
		if term.ID == terminalID {
			t.Fatalf("a stopped terminal is still listed")
		}
	}

	if r := i.CLI("terminal", "stop", "--id", "acme-missing-terminal"); r.Code != 1 {
		t.Fatalf("stop of an unknown terminal exited %d, want 1", r.Code)
	}
	gone := i.SocketHTTP(i.Credential("runner")).Do("DELETE", "/api/terminals/acme-missing-terminal", nil)
	if gone.Status != 404 {
		t.Fatalf("DELETE of an unknown terminal answered %d, want 404", gone.Status)
	}
}

func TestPersistentSessionList(t *testing.T) {
	t.Parallel()
	ssh := harness.StartSSHHost(t)
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)
	hostID := g1sCreateHost(t, i, ssh)
	hostProject := g1sCreateProject(t, i, "acme-remote", map[string]any{
		"host_id": hostID, "allowed_templates": []string{"acme-tmux"},
	})
	i.MustCLI("terminal", "start", "--project", hostProject, "--template", "acme-tmux", "--json")

	row, trace := g1sAwaitPersistent(t, i, hostProject)
	if row.TemplateID != "acme-tmux" || !strings.HasPrefix(row.Name, "relay-") {
		t.Fatalf("persistent session %+v, want an acme-tmux session named relay-...", row)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.persistent.list", Trace: trace,
		Fields: map[string]any{"status": "ok", "project_id": hostProject}})

	localProject := g1sCreateProject(t, i, "acme-local-plain", nil)
	if r := i.CLI("terminal", "persistent-list", "--project", localProject); r.Code != 1 {
		t.Fatalf("persistent-list of a project without a host exited %d, want 1", r.Code)
	}
	refused := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects/"+localProject+"/persistent-sessions", nil)
	if refused.Status != 404 {
		t.Fatalf("persistent sessions of a project without a host answered %d, want 404", refused.Status)
	}
}

func TestPersistentSessionKill(t *testing.T) {
	t.Parallel()
	ssh := harness.StartSSHHost(t)
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)
	hostID := g1sCreateHost(t, i, ssh)
	hostProject := g1sCreateProject(t, i, "acme-kill", map[string]any{
		"host_id": hostID, "allowed_templates": []string{"acme-tmux"},
	})
	i.MustCLI("terminal", "start", "--project", hostProject, "--template", "acme-tmux", "--json")
	row, _ := g1sAwaitPersistent(t, i, hostProject)

	kill := i.MustCLI("terminal", "persistent-kill", "--project", hostProject, "--name", row.Name, "--json")
	var killed struct {
		ProjectID string `json:"project_id"`
		Name      string `json:"name"`
	}
	kill.JSON(t, &killed)
	if killed.ProjectID != hostProject || killed.Name != row.Name {
		t.Fatalf("persistent-kill printed %+v, want %s and %s", killed, hostProject, row.Name)
	}
	g1sEvent(t, i, harness.EventQuery{Key: "session.persistent.kill", Trace: kill.Trace,
		Fields: map[string]any{"status": "ok", "project_id": hostProject}})

	after := i.MustCLI("terminal", "persistent-list", "--project", hostProject, "--json")
	var rows []g1sPersistentRow
	after.JSON(t, &rows)
	for _, r := range rows {
		if r.Name == row.Name {
			t.Fatalf("a killed persistent session is still listed")
		}
	}

	// The same name with a higher ordinal is a name the host does not hold.
	unknown := strings.TrimSuffix(row.Name, "1") + "9"
	if r := i.CLI("terminal", "persistent-kill", "--project", hostProject, "--name", unknown); r.Code != 1 {
		t.Fatalf("kill of an unknown persistent session exited %d, want 1", r.Code)
	}
	gone := i.HTTP(i.Credential("configurer")).Do("DELETE", "/api/projects/"+hostProject+"/persistent-sessions/"+unknown, nil)
	if gone.Status != 404 {
		t.Fatalf("DELETE of an unknown persistent session answered %d, want 404", gone.Status)
	}
}

func TestSessionMountClassChecked(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)

	if resp := i.SocketHTTP(harness.Credential{}).Do("GET", "/api/models", nil); resp.Status != 401 {
		t.Fatalf("GET /api/models through the / mount with no credential answered %d, want 401", resp.Status)
	}
	if resp := i.SocketHTTP(i.Credential("reader")).Do("GET", "/api/models", nil); resp.Status != 403 {
		t.Fatalf("GET /api/models through the / mount with a read-only credential answered %d, want 403", resp.Status)
	}
	resp := i.SocketHTTP(i.Credential("runner")).Do("GET", "/api/models", nil, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if resp.Status != 200 {
		t.Fatalf("GET /api/models with the proxy class answered %d, want 200: %s", resp.Status, resp.Body)
	}
	var models struct {
		Models []json.RawMessage `json:"models"`
	}
	resp.JSON(t, &models)
	i.WaitEvent(harness.EventQuery{Key: "model.list", Trace: resp.Trace,
		Fields: map[string]any{"status": "ok"}}, g1sEventDeadline)
}
