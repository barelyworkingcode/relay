package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

const g1sWait = 60 * time.Second

var g1sCredentials = []harness.CredentialSpec{
	{Name: "runner", Classes: []string{"execute"}},
	{Name: "proxy", Classes: []string{"proxy"}},
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
}

// g1sBoot starts an instance with the credentials and the grant outcome the
// session rows need, and waits for the session host (and the model host when
// the options attach one).
func g1sBoot(t *testing.T, o harness.Options) *harness.Instance {
	t.Helper()
	o.Credentials = append(append([]harness.CredentialSpec{}, g1sCredentials...), o.Credentials...)
	if o.Presence == nil {
		o.Presence = approveGrant
	}
	i := harness.Start(t, o)
	if o.FakeModelHost != nil {
		i.WaitModelHost(g1sWait)
	}
	i.WaitSessionHost(g1sWait)
	return i
}

// g1sProject creates a project whose folder is new; fields are merged into the
// project-create body.
func g1sProject(t *testing.T, i *harness.Instance, name string, fields map[string]any) project {
	t.Helper()
	dir := filepath.Join(i.Home, "work", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body := map[string]any{"name": name, "path": dir}
	for k, v := range fields {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the project body: %v", err)
	}
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: raw}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id")
	}
	return p
}

type g1sSession struct {
	SessionID    string `json:"sessionId"`
	ProviderType string `json:"providerType"`
}

// g1sStartSession runs `relay session start` and returns the session and the CLI result.
func g1sStartSession(t *testing.T, i *harness.Instance, projectID, model string, extra ...string) (g1sSession, harness.Result) {
	t.Helper()
	args := append([]string{"session", "start", "--project", projectID, "--model", model, "--json"}, extra...)
	r := i.MustCLI(args...)
	var s g1sSession
	r.JSON(t, &s)
	if s.SessionID == "" {
		t.Fatalf("session start printed no sessionId: %s", r.Stdout)
	}
	return s, r
}

type g1sListedSession struct {
	ID           string `json:"id"`
	ProjectID    string `json:"projectId"`
	Live         bool   `json:"live"`
	MessageCount int    `json:"messageCount"`
	Origin       string `json:"origin"`
	Headless     bool   `json:"headless"`
}

func g1sSessions(t *testing.T, i *harness.Instance) ([]g1sListedSession, harness.Result) {
	t.Helper()
	r := i.MustCLI("session", "list", "--json")
	var out struct {
		Sessions []g1sListedSession `json:"sessions"`
	}
	r.JSON(t, &out)
	return out.Sessions, r
}

func g1sFindSession(list []g1sListedSession, id string) (g1sListedSession, bool) {
	for _, s := range list {
		if s.ID == id {
			return s, true
		}
	}
	return g1sListedSession{}, false
}

func g1sSay(t *testing.T, i *harness.Instance, id, text string) string {
	t.Helper()
	r := i.MustCLI("session", "message", "--id", id, "--text", text, "--json")
	var out struct {
		Text string `json:"text"`
	}
	r.JSON(t, &out)
	return out.Text
}

func g1sAuditCount(i *harness.Instance, event, outcome string) int {
	return len(i.Audit(harness.AuditQuery{Event: event, Outcome: outcome}))
}

func TestChatSessionAnswers(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{FakeModelHost: fakeEchoModels()})
	p := g1sProject(t, i, "acme-chat", map[string]any{"allowed_templates": []string{"chat"}})

	s, r := g1sStartSession(t, i, p.ID, "fake-echo")
	if s.ProviderType != "chat" {
		t.Fatalf("providerType %q, want chat", s.ProviderType)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: r.Trace, Fields: map[string]any{"status": "ok", "session_id": s.SessionID, "kind": "chat", "project_id": p.ID}})
	if got := g1sAuditCount(i, "session_launch", "ok"); got != 1 {
		t.Fatalf("session_launch ok rows: %d, want 1", got)
	}
	if got := g1sSay(t, i, s.SessionID, "hi"); got != "echo: hi" {
		t.Fatalf("reply %q, want %q", got, "echo: hi")
	}

	// The power path: the same launch over the HTTP door.
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": "fake-echo"})
	if resp.Status != 201 {
		t.Fatalf("POST /api/sessions answered %d, want 201", resp.Status)
	}
	var created g1sSession
	resp.JSON(t, &created)
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "session_id": created.SessionID, "kind": "chat"}})
}

func TestSessionLaunchUngrantedRefused(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{})
	p := g1sProject(t, i, "acme-ungranted", map[string]any{"allowed_templates": []string{"shell"}})
	body := map[string]any{"projectId": p.ID, "model": "sonnet"}

	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions", body)
	if resp.Status != 403 {
		t.Fatalf("a launch without claude-code in allowed_templates answered %d, want 403", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace, Fields: map[string]any{"status": "denied"}})
	if got := g1sAuditCount(i, "session_launch", "denied"); got != 1 {
		t.Fatalf("session_launch denied rows: %d, want 1", got)
	}

	weak := i.SocketHTTP(i.Credential("reader")).Do("POST", "/api/sessions", body)
	if weak.Status != 403 {
		t.Fatalf("a launch with a read-only credential answered %d, want 403", weak.Status)
	}
	if got := i.Events(harness.EventQuery{Key: "session.launch", Trace: weak.Trace}); len(got) != 0 {
		t.Fatalf("a class refusal wrote %d session.launch lines", len(got))
	}
	if list, _ := g1sSessions(t, i); len(list) != 0 {
		t.Fatalf("a refused launch left %d sessions", len(list))
	}
}

func TestSessionBlankModelRefused(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{})
	p := g1sProject(t, i, "acme-blank", map[string]any{"allowed_templates": []string{"chat"}})
	before, _ := g1sSessions(t, i)

	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": ""})
	if resp.Status != 400 {
		t.Fatalf("a blank model answered %d, want 400", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace, Fields: map[string]any{"status": "denied"}})
	if got := g1sAuditCount(i, "session_launch", "error"); got != 1 {
		t.Fatalf("session_launch error rows: %d, want 1", got)
	}
	after, _ := g1sSessions(t, i)
	if len(after) != len(before) {
		t.Fatalf("session list went from %d to %d after a refused launch", len(before), len(after))
	}
}

func TestSystemModelChatRefused(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{FakeModelHost: &harness.FakeModelHostSpec{
		ID: "acme-models",
		Models: []json.RawMessage{
			json.RawMessage(`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":8192}`),
			json.RawMessage(`{"id":"fake-system","object":"model","owned_by":"fake","context_length":8192,"system":true}`),
		},
	}})
	// The catalogue read decides the refusal; a model list proves relay has it.
	if _, ids := modelIDs(t, i); !hasAll(ids, "fake-echo") {
		t.Fatalf("model list %v lacks fake-echo", ids)
	}
	p := g1sProject(t, i, "acme-system", map[string]any{"allowed_templates": []string{"chat"}})

	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": "fake-system"})
	if resp.Status != 403 {
		t.Fatalf("a chat launch on a system model answered %d, want 403", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace, Fields: map[string]any{"status": "denied"}})
	if list, _ := g1sSessions(t, i); len(list) != 0 {
		t.Fatalf("a refused launch left %d sessions", len(list))
	}
}

func TestSessionListAndAuth(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{})
	p := g1sProject(t, i, "acme-list", map[string]any{"allowed_templates": []string{"claude-code"}})
	s, _ := g1sStartSession(t, i, p.ID, "sonnet")

	list, r := g1sSessions(t, i)
	got, ok := g1sFindSession(list, s.SessionID)
	if !ok || !got.Live || got.ProjectID != p.ID {
		t.Fatalf("session list %+v lacks the live session %s of %s", list, s.SessionID, p.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.list", Trace: r.Trace, Fields: map[string]any{"status": "ok", "count": len(list)}})

	anon := i.SocketHTTP(harness.Credential{}).Do("GET", "/api/sessions", nil)
	if anon.Status != 401 {
		t.Fatalf("GET /api/sessions with no header answered %d, want 401", anon.Status)
	}
	viaHTTP := i.SocketHTTP(i.Credential("proxy")).Do("GET", "/api/sessions", nil)
	if viaHTTP.Status != 200 {
		t.Fatalf("GET /api/sessions with a proxy credential answered %d, want 200", viaHTTP.Status)
	}
}

func TestSessionMessage(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{})
	p := g1sProject(t, i, "acme-message", map[string]any{"allowed_templates": []string{"claude-code"}})
	s, _ := g1sStartSession(t, i, p.ID, "sonnet")

	if got := g1sSay(t, i, s.SessionID, "hi"); got != "echo: hi" {
		t.Fatalf("reply %q, want %q", got, "echo: hi")
	}
	i.WaitEvent(harness.EventQuery{Key: "session.message", Fields: map[string]any{"status": "ok", "session_id": s.SessionID}}, g1sWait)

	if r := i.CLI("session", "message", "--id", "no-such-session", "--text", "hi", "--json"); r.Code != 1 {
		t.Fatalf("a message to an unknown session exited %d, want 1", r.Code)
	}
}

func TestSessionStop(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{})
	p := g1sProject(t, i, "acme-stop", map[string]any{"allowed_templates": []string{"claude-code"}})
	s, _ := g1sStartSession(t, i, p.ID, "sonnet")

	r := i.MustCLI("session", "stop", "--id", s.SessionID, "--json")
	var stopped struct {
		ID string `json:"id"`
	}
	r.JSON(t, &stopped)
	if stopped.ID != s.SessionID {
		t.Fatalf("session stop printed id %q, want %s", stopped.ID, s.SessionID)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.delete", Fields: map[string]any{"status": "ok", "session_id": s.SessionID}}, g1sWait)
	list, _ := g1sSessions(t, i)
	if _, ok := g1sFindSession(list, s.SessionID); ok {
		t.Fatalf("session list still holds the stopped session %s", s.SessionID)
	}

	unknown := i.CLI("session", "stop", "--id", "no-such-session", "--json")
	if unknown.Code != 0 {
		t.Fatalf("stopping an unknown session exited %d, want 0: %s", unknown.Code, unknown.Stderr)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.delete", Trace: unknown.Trace, Fields: map[string]any{"status": "ok"}}, g1sWait)
}

func TestSessionResume(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{})
	p := g1sProject(t, i, "acme-resume", map[string]any{"allowed_templates": []string{"claude-code"}})
	s, _ := g1sStartSession(t, i, p.ID, "sonnet")
	if got := g1sSay(t, i, s.SessionID, "hi"); got != "echo: hi" {
		t.Fatalf("reply %q, want %q", got, "echo: hi")
	}

	type resumed struct {
		SessionID string `json:"session_id"`
		Resumed   bool   `json:"resumed"`
	}
	var live resumed
	liveRes := i.MustCLI("session", "resume", "--id", s.SessionID, "--json")
	liveRes.JSON(t, &live)
	if live.Resumed {
		t.Fatalf("resuming a live session answered resumed:true")
	}

	// A restart ages every live session to dormant: the stopped state a resume brings back.
	i.Restart()
	i.WaitSessionHost(g1sWait)
	if list, _ := g1sSessions(t, i); len(list) != 1 || list[0].Live {
		t.Fatalf("after a restart the list holds %+v, want the one dormant session", list)
	}

	resumeRowsBefore := g1sAuditCount(i, "session_resume", "ok")
	res := i.MustCLI("session", "resume", "--id", s.SessionID, "--json")
	var back resumed
	res.JSON(t, &back)
	if !back.Resumed || back.SessionID != s.SessionID {
		t.Fatalf("session resume printed %+v, want resumed:true for %s", back, s.SessionID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.resume", Trace: res.Trace, Fields: map[string]any{"status": "ok", "session_id": s.SessionID}})
	if got := g1sAuditCount(i, "session_resume", "ok"); got != resumeRowsBefore+1 {
		t.Fatalf("session_resume ok rows: %d after the dormant resume, want %d", got, resumeRowsBefore+1)
	}
	list, _ := g1sSessions(t, i)
	got, ok := g1sFindSession(list, s.SessionID)
	if !ok || !got.Live || got.MessageCount < 1 {
		t.Fatalf("after resume the list holds %+v for %s, want a live session with its history", got, s.SessionID)
	}

	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions/no-such-session/resume", nil)
	if resp.Status != 404 {
		t.Fatalf("resuming an unknown session answered %d, want 404", resp.Status)
	}
	if got := g1sAuditCount(i, "session_resume", "not_found"); got != 1 {
		t.Fatalf("session_resume not_found rows: %d, want 1", got)
	}
}

// g1sAddTemplate creates a console terminal template over HTTP.
func g1sAddTemplate(t *testing.T, i *harness.Instance, tmpl map[string]any) {
	t.Helper()
	resp := i.SocketHTTP(i.Credential("configurer")).Do("POST", "/api/terminal/templates", tmpl)
	if resp.Status != 201 {
		t.Fatalf("POST /api/terminal/templates answered %d, want 201: %s", resp.Status, resp.Body)
	}
}

type g1sTerminal struct {
	TerminalID string `json:"terminalId"`
}

func g1sStartTerminal(t *testing.T, i *harness.Instance, projectID, template string) (g1sTerminal, harness.Result) {
	t.Helper()
	r := i.MustCLI("terminal", "start", "--project", projectID, "--template", template, "--json")
	var term g1sTerminal
	r.JSON(t, &term)
	if term.TerminalID == "" {
		t.Fatalf("terminal start printed no terminalId: %s", r.Stdout)
	}
	return term, r
}

type g1sListedTerminal struct {
	ID         string `json:"id"`
	TemplateID string `json:"templateId"`
	State      string `json:"state"`
	Origin     string `json:"origin"`
}

func g1sTerminals(t *testing.T, i *harness.Instance) ([]g1sListedTerminal, harness.Result) {
	t.Helper()
	r := i.MustCLI("terminal", "list", "--json")
	var out struct {
		Terminals []g1sListedTerminal `json:"terminals"`
	}
	r.JSON(t, &out)
	return out.Terminals, r
}

func g1sFindTerminal(list []g1sListedTerminal, id string) (g1sListedTerminal, bool) {
	for _, x := range list {
		if x.ID == id {
			return x, true
		}
	}
	return g1sListedTerminal{}, false
}

func TestTerminalStart(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{})
	g1sAddTemplate(t, i, map[string]any{"id": "acme-blocked", "name": "Acme blocked", "command": "/bin/cat"})
	p := g1sProject(t, i, "acme-terminal", map[string]any{"allowed_templates": []string{"shell"}})

	term, r := g1sStartTerminal(t, i, p.ID, "shell")
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: r.Trace, Fields: map[string]any{"status": "ok", "session_id": term.TerminalID, "kind": "pty"}})
	var started struct {
		Directory string `json:"directory"`
	}
	r.JSON(t, &started)
	folder, err := filepath.EvalSymlinks(filepath.Join(i.Home, "work", "acme-terminal"))
	if err != nil {
		t.Fatalf("resolving the project folder: %v", err)
	}
	if got, err := filepath.EvalSymlinks(started.Directory); err != nil || got != folder {
		t.Fatalf("terminal directory %q, want the project folder %q", started.Directory, folder)
	}

	denied := i.CLI("terminal", "start", "--project", p.ID, "--template", "acme-blocked", "--json")
	if denied.Code != 1 {
		t.Fatalf("a template outside allowed_templates exited %d, want 1", denied.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: denied.Trace, Fields: map[string]any{"status": "denied"}})
	if got := g1sAuditCount(i, "session_launch", "denied"); got != 1 {
		t.Fatalf("session_launch denied rows: %d, want 1", got)
	}
}

func TestTerminalList(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{})
	p := g1sProject(t, i, "acme-terminal-list", map[string]any{"allowed_templates": []string{"shell"}})
	term, _ := g1sStartTerminal(t, i, p.ID, "shell")

	list, r := g1sTerminals(t, i)
	got, ok := g1sFindTerminal(list, term.TerminalID)
	if !ok || got.TemplateID != "shell" || got.Origin != "" {
		t.Fatalf("terminal list %+v lacks %s on template shell started by a person", list, term.TerminalID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "terminal.list", Trace: r.Trace, Fields: map[string]any{"status": "ok", "count": len(list)}})
}

func TestTerminalLog(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{})
	g1sAddTemplate(t, i, map[string]any{"id": "acme-echo", "name": "Acme echo", "command": "/bin/echo", "args": []string{"acme-marker"}})
	p := g1sProject(t, i, "acme-terminal-log", map[string]any{"allowed_templates": []string{"acme-echo"}})
	term, _ := g1sStartTerminal(t, i, p.ID, "acme-echo")
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, g1sWait)

	r := i.MustCLI("terminal", "log", "--id", term.TerminalID, "--json")
	var out struct {
		ID  string `json:"id"`
		Log string `json:"log"`
	}
	r.JSON(t, &out)
	if out.ID != term.TerminalID || !strings.Contains(out.Log, "acme-marker") {
		t.Fatalf("terminal log printed id %q and log %q, want %s holding acme-marker", out.ID, out.Log, term.TerminalID)
	}
	i.WaitEvent(harness.EventQuery{Key: "terminal.log", Trace: r.Trace, Fields: map[string]any{"status": "ok", "terminal_id": term.TerminalID}}, g1sWait)

	if bad := i.CLI("terminal", "log", "--id", "no-such-terminal", "--json"); bad.Code != 1 {
		t.Fatalf("the log of an unknown terminal exited %d, want 1", bad.Code)
	}
}

func TestTerminalStop(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{})
	p := g1sProject(t, i, "acme-terminal-stop", map[string]any{"allowed_templates": []string{"shell"}})
	term, _ := g1sStartTerminal(t, i, p.ID, "shell")

	r := i.MustCLI("terminal", "stop", "--id", term.TerminalID, "--json")
	i.WaitEvent(harness.EventQuery{Key: "terminal.delete", Trace: r.Trace, Fields: map[string]any{"status": "ok", "terminal_id": term.TerminalID}}, g1sWait)
	list, _ := g1sTerminals(t, i)
	if _, ok := g1sFindTerminal(list, term.TerminalID); ok {
		t.Fatalf("terminal list still holds the stopped terminal %s", term.TerminalID)
	}
	if bad := i.CLI("terminal", "stop", "--id", "no-such-terminal", "--json"); bad.Code != 1 {
		t.Fatalf("stopping an unknown terminal exited %d, want 1", bad.Code)
	}
}

func TestSessionMountClassChecked(t *testing.T) {
	t.Parallel()
	i := g1sBoot(t, harness.Options{FakeModelHost: fakeEchoModels()})
	proxy := i.SocketHTTP(i.Credential("proxy"))

	resp := proxy.Do("GET", "/api/models", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/models with a proxy credential answered %d, want 200", resp.Status)
	}
	i.WaitEvent(harness.EventQuery{Key: "model.list", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}}, g1sWait)

	if anon := i.SocketHTTP(harness.Credential{}).Do("GET", "/api/models", nil); anon.Status != 401 {
		t.Fatalf("GET /api/models with no credential answered %d, want 401", anon.Status)
	}
	if weak := i.SocketHTTP(i.Credential("reader")).Do("GET", "/api/models", nil); weak.Status != 403 {
		t.Fatalf("GET /api/models with a read credential answered %d, want 403", weak.Status)
	}

	// The client reuses its connection, so the second call rides the first's.
	again := proxy.Do("GET", "/api/models", nil)
	if again.Status != 200 {
		t.Fatalf("the second GET /api/models answered %d, want 200", again.Status)
	}
}

type g1sPersistent struct {
	Name       string `json:"name"`
	TemplateID string `json:"template_id"`
}

// g1sHostProject registers the SSH host, gives it a persistent template and
// creates a project on it, then starts one terminal from that template.
// tmux gives relay no event to report, so the caller polls for the session.
func g1sHostProject(t *testing.T, i *harness.Instance, host *harness.SSHHost) project {
	t.Helper()
	i.TrustSSHHost(host)
	api := i.HTTP(i.Credential("configurer"))
	create := api.Do("POST", "/api/hosts", map[string]any{
		"name": "acme-box", "target": host.Target, "port": host.Port, "identity_file": host.IdentityFile,
	})
	if create.Status != 201 {
		t.Fatalf("POST /api/hosts answered %d, want 201", create.Status)
	}
	var h struct {
		ID string `json:"id"`
	}
	create.JSON(t, &h)
	tmpl := api.Do("POST", "/api/hosts/"+h.ID+"/templates", map[string]any{"id": "acme-persist", "name": "Acme persistent", "persist": true})
	if tmpl.Status != 201 {
		t.Fatalf("POST /api/hosts/{id}/templates answered %d, want 201", tmpl.Status)
	}
	p := g1sProject(t, i, "acme-hosted", map[string]any{"host_id": h.ID})
	return p
}

func g1sPersistentList(t *testing.T, i *harness.Instance, projectID string) ([]g1sPersistent, harness.Result) {
	t.Helper()
	r := i.MustCLI("terminal", "persistent-list", "--project", projectID, "--json")
	var list []g1sPersistent
	r.JSON(t, &list)
	return list, r
}

// g1sAwaitPersistent polls persistent-list until the host reports a session.
func g1sAwaitPersistent(t *testing.T, i *harness.Instance, projectID string) ([]g1sPersistent, harness.Result) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		list, r := g1sPersistentList(t, i, projectID)
		if len(list) > 0 {
			return list, r
		}
		select {
		case <-deadline.C:
			t.Fatalf("the host listed no persistent session within 30s for project %s", projectID)
		case <-tick.C:
		}
	}
}

func TestPersistentSessionList(t *testing.T) {
	t.Parallel()
	host := harness.StartSSHHost(t)
	i := g1sBoot(t, harness.Options{})
	p := g1sHostProject(t, i, host)
	t.Cleanup(func() {
		// Best effort: leave no tmux session behind on the machine.
		list, _ := g1sPersistentList(t, i, p.ID)
		for _, s := range list {
			i.CLI("terminal", "persistent-kill", "--project", p.ID, "--name", s.Name)
		}
	})
	g1sStartTerminal(t, i, p.ID, "acme-persist")

	list, r := g1sAwaitPersistent(t, i, p.ID)
	if len(list) != 1 || list[0].TemplateID != "acme-persist" || list[0].Name == "" {
		t.Fatalf("persistent-list returned %+v, want one session of template acme-persist", list)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.persistent.list", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})

	local := g1sProject(t, i, "acme-local", nil)
	if bad := i.CLI("terminal", "persistent-list", "--project", local.ID, "--json"); bad.Code != 1 {
		t.Fatalf("persistent-list for a local project exited %d, want 1", bad.Code)
	}
}

func TestPersistentSessionKill(t *testing.T) {
	t.Parallel()
	host := harness.StartSSHHost(t)
	i := g1sBoot(t, harness.Options{})
	p := g1sHostProject(t, i, host)
	t.Cleanup(func() {
		list, _ := g1sPersistentList(t, i, p.ID)
		for _, s := range list {
			i.CLI("terminal", "persistent-kill", "--project", p.ID, "--name", s.Name)
		}
	})
	g1sStartTerminal(t, i, p.ID, "acme-persist")
	list, _ := g1sAwaitPersistent(t, i, p.ID)
	name := list[0].Name

	r := i.MustCLI("terminal", "persistent-kill", "--project", p.ID, "--name", name, "--json")
	var killed struct {
		ProjectID string `json:"project_id"`
		Name      string `json:"name"`
	}
	r.JSON(t, &killed)
	if killed.ProjectID != p.ID || killed.Name != name {
		t.Fatalf("persistent-kill printed %+v, want project %s name %s", killed, p.ID, name)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.persistent.kill", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})
	after, _ := g1sPersistentList(t, i, p.ID)
	for _, s := range after {
		if s.Name == name {
			t.Fatalf("persistent-list still holds the killed session %s", name)
		}
	}
	if bad := i.CLI("terminal", "persistent-kill", "--project", p.ID, "--name", name, "--json"); bad.Code != 1 {
		t.Fatalf("killing an unknown persistent session exited %d, want 1", bad.Code)
	}
}
