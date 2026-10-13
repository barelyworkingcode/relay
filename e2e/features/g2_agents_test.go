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

	"relaye2e/harness"
)

const agHostDeadline = 60 * time.Second

var agCreds = []harness.CredentialSpec{
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
	{Name: "runner", Classes: []string{"execute", "proxy", "read"}},
}

var agApprove = map[string]harness.Outcome{
	"project.grant": harness.OutcomeApprove,
	"project.agent": harness.OutcomeApprove,
}

type agEntry struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Model   string   `json:"model,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
}

type agWrite struct {
	ProjectID   string  `json:"projectId"`
	ProjectName string  `json:"projectName"`
	Machine     string  `json:"machine"`
	Entry       agEntry `json:"entry"`
}

type agErr struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Command string `json:"command"`
	Machine string `json:"machine"`
}

// agStart boots an instance that approves the agent gate and waits for the
// session host.
func agStart(t *testing.T, mut ...func(*harness.Options)) *harness.Instance {
	t.Helper()
	o := harness.Options{Credentials: agCreds, Presence: agApprove}
	for _, m := range mut {
		m(&o)
	}
	i := harness.Start(t, o)
	i.WaitSessionHost(agHostDeadline)
	return i
}

func agProject(t *testing.T, i *harness.Instance, name string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"allowed_templates": []string{"claude-code", "codex", "pi"}}
	for k, v := range extra {
		body[k] = v
	}
	return g1sCreateProject(t, i, name, body)
}

func agPost(t *testing.T, i *harness.Instance, pid string, e any) harness.Response {
	t.Helper()
	return i.HTTP(i.Credential("configurer")).Do("POST", "/api/projects/"+pid+"/agents", e)
}

// agAdd adds an entry over HTTP and fails t unless it answers 201.
func agAdd(t *testing.T, i *harness.Instance, pid string, e agEntry) agWrite {
	t.Helper()
	resp := agPost(t, i, pid, e)
	if resp.Status != http.StatusCreated {
		t.Fatalf("POST agents %+v answered %d, want 201: %s", e, resp.Status, resp.Body)
	}
	var w agWrite
	resp.JSON(t, &w)
	return w
}

func agList(t *testing.T, i *harness.Instance, pid string) []agEntry {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects/"+pid+"/agents", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET agents answered %d: %s", resp.Status, resp.Body)
	}
	var out struct {
		Entries []agEntry `json:"entries"`
	}
	resp.JSON(t, &out)
	return out.Entries
}

func agNames(es []agEntry) []string {
	names := []string{}
	for _, e := range es {
		names = append(names, e.Name)
	}
	return names
}

// agSimple checks a Simple-family refusal, {"error": message}, and returns the message.
func agSimple(t *testing.T, resp harness.Response, status int) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	resp.JSON(t, &e)
	if resp.Status != status || e.Error == "" {
		t.Fatalf("answered %d %s, want %d with an error message", resp.Status, resp.Body, status)
	}
	return e.Error
}

func agRefusal(t *testing.T, resp harness.Response, status int, code string) agErr {
	t.Helper()
	var e agErr
	resp.JSON(t, &e)
	if resp.Status != status || e.Error != code {
		t.Fatalf("answered %d %q, want %d %q: %s", resp.Status, e.Error, status, code, resp.Body)
	}
	return e
}

// agStoredProject reads the project record out of settings.json.
func agStoredProject(t *testing.T, i *harness.Instance, pid string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(i.ConfigDir, "settings.json"))
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}
	var s struct {
		Projects []map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decoding settings.json: %v", err)
	}
	for _, p := range s.Projects {
		if string(p["id"]) == `"`+pid+`"` {
			return p
		}
	}
	t.Fatalf("settings.json holds no project %s", pid)
	return nil
}

func agShQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// agWrapper writes an executable that appends its arguments as one line to log
// and then runs the fake agent of the given persona with the same arguments.
func agWrapper(t *testing.T, i *harness.Instance, path, log, persona string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	fake := filepath.Join(i.Home, ".local", "bin", persona)
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nexec %s \"$@\"\n", agShQuote(log), agShQuote(fake))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func agReadLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func agSame(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// agSessionStart starts a session by entry over the CLI and returns the result
// without judging it.
func agSessionStart(i *harness.Instance, pid, entry string) harness.Result {
	return i.CLI("session", "start", "--project", pid, "--entry", entry, "--json")
}

// agRunTurn sends one message and waits for the reply, which is the end of the
// turn, so the agent process has started.
func agRunTurn(t *testing.T, i *harness.Instance, res harness.Result) string {
	t.Helper()
	if res.Code != 0 {
		t.Fatalf("session start exited %d\nstdout: %s\nstderr: %s", res.Code, res.Stdout, res.Stderr)
	}
	var s struct {
		SessionID    string `json:"sessionId"`
		Model        string `json:"model"`
		ProviderType string `json:"providerType"`
	}
	res.JSON(t, &s)
	if s.SessionID == "" {
		t.Fatalf("session start printed no sessionId: %s", res.Stdout)
	}
	g1sSendText(t, i, s.SessionID, "hi", "echo: hi")
	return s.SessionID
}

// agStartArgvs lists the argv of every session start a fake agent persona logged.
func agStartArgvs(i *harness.Instance, persona string) [][]string {
	var out [][]string
	for _, c := range i.FakeAgentCalls(persona) {
		// A model-list probe also logs a start line; it is not a session.
		if c.Event == "start" && (len(c.Argv) == 0 || (c.Argv[0] != "debug" && c.Argv[0] != "--list-models")) {
			out = append(out, c.Argv)
		}
	}
	return out
}

func agHasPair(argv []string, flag, val string) bool {
	for k := 0; k+1 < len(argv); k++ {
		if argv[k] == flag && argv[k+1] == val {
			return true
		}
	}
	return false
}

// agHost registers a host project on the ssh stub. The project folder lives
// under the instance's remote root.
func agHost(t *testing.T, i *harness.Instance) (pid, remote string) {
	t.Helper()
	c := i.HTTP(i.Credential("configurer"))
	hr := c.Do("POST", "/api/hosts", map[string]any{"name": "testbox", "target": "acme@testbox"})
	if hr.Status != http.StatusCreated {
		t.Fatalf("POST /api/hosts answered %d, want 201: %s", hr.Status, hr.Body)
	}
	var h struct {
		ID string `json:"id"`
	}
	hr.JSON(t, &h)
	remote = i.RemoteRoot()
	dir := filepath.Join(remote, "boxproj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body, _ := json.Marshal(map[string]any{
		"name": "Acme Box", "path": dir, "host_id": h.ID, "allowed_templates": []string{"claude-code", "codex", "pi"},
	})
	res := i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("project create for a host project exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var p struct {
		ID string `json:"id"`
	}
	res.JSON(t, &p)
	return p.ID, remote
}

func agHostStart(t *testing.T) *harness.Instance {
	return agStart(t, func(o *harness.Options) { o.SSHStub = true })
}

func TestAgentListAddListRemove(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-list", nil)

	var empty struct {
		Entries []agEntry `json:"entries"`
	}
	i.MustCLI("project", "agents", "list", "--id", pid, "--json").JSON(t, &empty)
	if len(empty.Entries) != 0 {
		t.Fatalf("a new project lists %d agents, want none", len(empty.Entries))
	}

	add := func(args ...string) harness.Result {
		t.Helper()
		return i.MustCLI(append([]string{"project", "agents", "add", "--id", pid, "--json"}, args...)...)
	}
	add("--name", "researcher", "--kind", "claude", "--model", "sonnet")
	// Two entries of one kind under different names.
	add("--name", "reviewer", "--kind", "claude", "--model", "opus")
	add("--name", "piLocal", "--kind", "pi", "--model", "local/coder")
	last := add("--name", "scribe", "--kind", "codex")
	requireEvent(t, i, harness.EventQuery{Key: "agent.add", Trace: last.Trace, Fields: map[string]any{"status": "ok", "project_id": pid, "name": "scribe"}})

	var listed struct {
		Entries []agEntry `json:"entries"`
	}
	lr := i.MustCLI("project", "agents", "list", "--id", pid, "--json")
	lr.JSON(t, &listed)
	want := []agEntry{
		{Name: "researcher", Kind: "claude", Model: "sonnet"},
		{Name: "reviewer", Kind: "claude", Model: "opus"},
		{Name: "piLocal", Kind: "pi", Model: "local/coder"},
		{Name: "scribe", Kind: "codex"},
	}
	if !g2Equal(listed.Entries, want) {
		t.Fatalf("list after four adds is %+v, want %+v", listed.Entries, want)
	}
	requireEvent(t, i, harness.EventQuery{Key: "agent.list", Trace: lr.Trace, Fields: map[string]any{"status": "ok", "project_id": pid, "count": 4}})
	if got := agList(t, i, pid); !g2Equal(got, want) {
		t.Fatalf("GET agents answered %+v, want the CLI's list %+v", got, want)
	}
	var view struct {
		Agents []agEntry `json:"agents"`
	}
	i.HTTP(i.Credential("reader")).Do("GET", "/api/projects/"+pid, nil).JSON(t, &view)
	if !g2Equal(view.Agents, want) {
		t.Fatalf("the project view holds agents %+v, want %+v", view.Agents, want)
	}

	// An update merges the flags into the stored entry and may rename it.
	up := i.MustCLI("project", "agents", "update", "--id", pid, "--name", "researcher", "--new-name", "analyst", "--model", "haiku", "--json")
	requireEvent(t, i, harness.EventQuery{Key: "agent.update", Trace: up.Trace, Fields: map[string]any{"status": "ok", "project_id": pid, "name": "analyst"}})
	want[0] = agEntry{Name: "analyst", Kind: "claude", Model: "haiku"}
	if got := agList(t, i, pid); !g2Equal(got, want) {
		t.Fatalf("list after the update is %+v, want %+v", got, want)
	}

	rm := i.MustCLI("project", "agents", "remove", "--id", pid, "--name", "reviewer", "--json")
	requireEvent(t, i, harness.EventQuery{Key: "agent.remove", Trace: rm.Trace, Fields: map[string]any{"status": "ok", "project_id": pid, "name": "reviewer"}})
	want = append(want[:1], want[2:]...)
	if got := agList(t, i, pid); !g2Equal(got, want) {
		t.Fatalf("list after the remove is %+v, want %+v", got, want)
	}

	// Each write left a config_change row naming the action and the entry.
	rows := g2Rows(i, "config_change", "ok", map[string]any{"credential": "agent_entry", "subject": pid})
	actions := map[string]bool{}
	for _, row := range rows {
		grants, _ := row["grants"].([]any)
		for _, g := range grants {
			if s, _ := g.(string); strings.HasPrefix(s, "action=") {
				actions[s] = true
			}
		}
	}
	for _, a := range []string{"action=add", "action=update", "action=remove"} {
		if !actions[a] {
			t.Fatalf("no agent_entry config_change row carries %s; saw %v", a, actions)
		}
	}
}

func TestAgentKindRefused(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-kind", nil)

	resp := agPost(t, i, pid, agEntry{Name: "scout", Kind: "gemini"})
	e := agRefusal(t, resp, http.StatusBadRequest, "kind_invalid")
	if e.Message != "kind must be claude, codex or pi" {
		t.Fatalf("kind_invalid message is %q", e.Message)
	}
	requireEvent(t, i, harness.EventQuery{Key: "agent.add", Trace: resp.Trace, Fields: map[string]any{"status": "error", "reason": "invalid"}})

	r := i.CLI("project", "agents", "add", "--id", pid, "--name", "scout", "--kind", "gemini")
	if r.Code != 1 || !strings.Contains(string(r.Stderr), "kind must be claude, codex or pi") {
		t.Fatalf("CLI add with kind gemini exited %d with stderr %q, want 1 naming the valid kinds", r.Code, r.Stderr)
	}
	if got := agList(t, i, pid); len(got) != 0 {
		t.Fatalf("refused adds left %+v", got)
	}
	if _, has := agStoredProject(t, i, pid)["agents"]; has {
		t.Fatalf("refused adds left an agents key in settings.json")
	}
}

func TestAgentNameTaken(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	p1 := agProject(t, i, "acme-one", nil)
	p2 := agProject(t, i, "acme-two", nil)

	agAdd(t, i, p1, agEntry{Name: "Researcher", Kind: "claude", Model: "sonnet"})
	agAdd(t, i, p1, agEntry{Name: "other", Kind: "claude", Model: "sonnet"})

	// The clash ignores case.
	resp := agPost(t, i, p1, agEntry{Name: "researcher", Kind: "codex"})
	e := agRefusal(t, resp, http.StatusConflict, "name_taken")
	if !strings.Contains(e.Message, "is already an agent in acme-one") {
		t.Fatalf("name_taken message is %q", e.Message)
	}
	requireEvent(t, i, harness.EventQuery{Key: "agent.add", Trace: resp.Trace, Fields: map[string]any{"status": "error", "reason": "conflict"}})

	// A rename onto a taken name clashes too.
	put := i.HTTP(i.Credential("configurer")).Do("PUT", "/api/projects/"+p1+"/agents/other", agEntry{Name: "RESEARCHER", Kind: "claude", Model: "sonnet"})
	agRefusal(t, put, http.StatusConflict, "name_taken")

	if got := agNames(agList(t, i, p1)); !g2Equal(got, []string{"Researcher", "other"}) {
		t.Fatalf("project one lists %v after the refusals, want [Researcher other]", got)
	}

	// Another project may use the same name; each is its own entry.
	agAdd(t, i, p2, agEntry{Name: "Researcher", Kind: "claude", Model: "opus"})
	if got := agList(t, i, p1)[0]; got.Model != "sonnet" {
		t.Fatalf("project one's Researcher is %+v, want its own model sonnet", got)
	}
	if got := agList(t, i, p2); len(got) != 1 || got[0].Model != "opus" {
		t.Fatalf("project two lists %+v, want its own Researcher on opus", got)
	}
}

func TestAgentChatTemplateClash(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	chat := []map[string]any{{"id": "t_review", "name": "Reviewer", "model": "claude-sonnet"}}
	pid := agProject(t, i, "acme-chat", map[string]any{"chat_templates": chat})

	// An agent named like a chat template of the project is refused.
	resp := agPost(t, i, pid, agEntry{Name: "reviewer", Kind: "claude", Model: "sonnet"})
	e := agRefusal(t, resp, http.StatusConflict, "name_taken")
	if !strings.Contains(e.Message, "is already a chat template in acme-chat") {
		t.Fatalf("name_taken message is %q", e.Message)
	}

	// The reverse: a chat template named like an agent is refused on the
	// project door.
	agAdd(t, i, pid, agEntry{Name: "Researcher", Kind: "claude", Model: "sonnet"})
	both := append(chat, map[string]any{"id": "t_res", "name": "researcher", "model": "claude-sonnet"})
	put := i.HTTP(i.Credential("configurer")).Do("PUT", "/api/projects/"+pid, map[string]any{"chat_templates": both})
	if put.Status != http.StatusBadRequest || !strings.Contains(string(put.Body), `is already an agent in acme-chat`) {
		t.Fatalf("PUT a chat template named like an agent answered %d %s, want 400 \"... is already an agent in acme-chat\"", put.Status, put.Body)
	}
	if got := agNames(agList(t, i, pid)); !g2Equal(got, []string{"Researcher"}) {
		t.Fatalf("agents are %v after the clash, want [Researcher]", got)
	}
}

func TestAgentCommandResolvedLocal(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-cmd", nil)

	// Only the instance's login-shell rc puts this folder on PATH, so a hit
	// shows the lookup ran in an interactive login shell.
	bin := filepath.Join(i.Home, "acmebin")
	agWrapper(t, i, filepath.Join(bin, "mywrapper"), filepath.Join(i.Home, "unused.log"), "claude")
	rc := "export PATH=" + agShQuote(bin) + ":\"$PATH\"\n"
	if err := os.WriteFile(filepath.Join(i.Home, ".zshrc"), []byte(rc), 0o600); err != nil {
		t.Fatalf("writing .zshrc: %v", err)
	}

	r := i.MustCLI("project", "agents", "add", "--id", pid, "--name", "wrapped", "--kind", "claude", "--model", "sonnet",
		"--command", "mywrapper", "--arg", "--profile", "--arg", "work", "--json")
	var w agWrite
	r.JSON(t, &w)
	if w.Machine != "this Mac" {
		t.Fatalf("machine is %q, want \"this Mac\"", w.Machine)
	}
	if !filepath.IsAbs(w.Entry.Command) || !agSame(t, w.Entry.Command, filepath.Join(bin, "mywrapper")) {
		t.Fatalf("stored command is %q, want the absolute path of %s", w.Entry.Command, filepath.Join(bin, "mywrapper"))
	}
	if !g2Equal(w.Entry.Args, []string{"--profile", "work"}) {
		t.Fatalf("stored args are %v, want [--profile work] in order", w.Entry.Args)
	}
	requireEvent(t, i, harness.EventQuery{Key: "agent.add", Trace: r.Trace, Fields: map[string]any{"status": "ok", "machine": "this Mac"}})
	if got := agList(t, i, pid); len(got) != 1 || got[0].Command != w.Entry.Command {
		t.Fatalf("list holds %+v, want the stored absolute path %s", got, w.Entry.Command)
	}
}

func TestAgentCommandResolvedOnHost(t *testing.T) {
	t.Parallel()
	i := agHostStart(t)
	pid, remote := agHost(t, i)

	bin := filepath.Join(remote, "hostbin")
	agWrapper(t, i, filepath.Join(bin, "mywrapper"), filepath.Join(remote, "unused.log"), "claude")
	// The stub runs the lookup in a login sh with HOME at the remote root.
	if err := os.WriteFile(filepath.Join(remote, ".profile"), []byte("export PATH="+agShQuote(bin)+":\"$PATH\"\n"), 0o600); err != nil {
		t.Fatalf("writing .profile: %v", err)
	}

	before := len(i.SSHCalls())
	w := agAdd(t, i, pid, agEntry{Name: "wrapped", Kind: "claude", Model: "sonnet", Command: "mywrapper"})
	if w.Machine != "testbox" {
		t.Fatalf("machine is %q, want testbox", w.Machine)
	}
	if !agSame(t, w.Entry.Command, filepath.Join(bin, "mywrapper")) {
		t.Fatalf("stored command is %q, want the host's path %s", w.Entry.Command, filepath.Join(bin, "mywrapper"))
	}
	found := false
	for _, c := range i.SSHCalls()[before:] {
		found = found || (c.Dest == "acme@testbox" && strings.Contains(c.Script(), "command -v") && strings.Contains(c.Script(), "mywrapper"))
	}
	if !found {
		t.Fatalf("the ssh stub saw no lookup of mywrapper on acme@testbox: %+v", i.SSHCalls()[before:])
	}

	// pi is not available on a host project.
	e := agRefusal(t, agPost(t, i, pid, agEntry{Name: "scout", Kind: "pi", Model: "local/coder"}), http.StatusBadRequest, "kind_not_available_on_host")
	if e.Message != "pi is not available on a host project" {
		t.Fatalf("kind_not_available_on_host message is %q", e.Message)
	}
}

func TestAgentCommandNotFound(t *testing.T) {
	t.Parallel()
	i := agHostStart(t)
	local := agProject(t, i, "acme-nf", nil)
	hostPID, _ := agHost(t, i)

	// A name the login shell knows only as a builtin is not found either.
	for _, name := range []string{"nosuchwrapper", "cd"} {
		resp := agPost(t, i, local, agEntry{Name: "ghost", Kind: "claude", Model: "sonnet", Command: name})
		e := agRefusal(t, resp, http.StatusUnprocessableEntity, "command_not_found")
		if e.Command != name || e.Machine != "this Mac" || e.Message != name+" was not found on this Mac" {
			t.Fatalf("local %s refusal is %+v, want the command and \"this Mac\" named", name, e)
		}
		requireEvent(t, i, harness.EventQuery{Key: "agent.add", Trace: resp.Trace, Fields: map[string]any{"status": "error", "reason": "invalid"}})
	}
	r := i.CLI("project", "agents", "add", "--id", local, "--name", "ghost", "--kind", "claude", "--model", "sonnet", "--command", "nosuchwrapper")
	if r.Code != 1 || !strings.Contains(string(r.Stderr), "nosuchwrapper was not found on this Mac") {
		t.Fatalf("CLI add exited %d with stderr %q, want 1 naming the command and the machine", r.Code, r.Stderr)
	}

	e := agRefusal(t, agPost(t, i, hostPID, agEntry{Name: "ghost", Kind: "claude", Command: "mywrapper"}), http.StatusUnprocessableEntity, "command_not_found")
	if e.Command != "mywrapper" || e.Machine != "testbox" || e.Message != "mywrapper was not found on testbox" {
		t.Fatalf("host refusal is %+v, want mywrapper and testbox named", e)
	}
	if len(agList(t, i, local))+len(agList(t, i, hostPID)) != 0 {
		t.Fatalf("a refused add stored an entry")
	}
}

func TestAgentCommandRefusesShellQuoting(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-quote", nil)
	marker := filepath.Join(t.TempDir(), "marker")

	cmds := []string{
		`/tmp/x\'; touch ` + marker + `; #`,
		`/tmp/a'b`,
		`/tmp/a"b`,
		`/tmp/a$b`,
		"/tmp/a`b`",
		`/tmp/a\b`,
	}
	for _, c := range cmds {
		resp := agPost(t, i, pid, agEntry{Name: "ghost", Kind: "claude", Model: "sonnet", Command: c})
		e := agRefusal(t, resp, http.StatusBadRequest, "command_invalid")
		if e.Message != "command must be a program name or an absolute path" {
			t.Fatalf("refusal of %q has message %q", c, e.Message)
		}
	}
	r := i.CLI("project", "agents", "add", "--id", pid, "--name", "ghost", "--kind", "claude", "--model", "sonnet", "--command", cmds[0])
	if r.Code != 1 || !strings.Contains(string(r.Stderr), "command must be a program name or an absolute path") {
		t.Fatalf("CLI add exited %d with stderr %q, want 1 refusing the command grammar", r.Code, r.Stderr)
	}

	if n := len(agList(t, i, pid)); n != 0 {
		t.Fatalf("a refused add stored %d entries", n)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the command ran: marker stat err = %v", err)
	}
}

func TestAgentWriteNeedsPresence(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-gate", nil)
	agAdd(t, i, pid, agEntry{Name: "keeper", Kind: "claude", Model: "sonnet"})
	before := agList(t, i, pid)
	approved := len(g2Rows(i, "config_change", "ok", map[string]any{"credential": "agent_entry"}))

	i.SetPresence(map[string]harness.Outcome{"project.agent": harness.OutcomeDeny})
	cfg := i.HTTP(i.Credential("configurer"))
	base := "/api/projects/" + pid + "/agents"
	doors := []struct {
		name, event, via string
		call             func() (int, string)
	}{
		{"http POST", "agent.add", "http", func() (int, string) {
			r := cfg.Do("POST", base, agEntry{Name: "newcomer", Kind: "claude", Model: "sonnet"})
			return r.Status, r.Trace
		}},
		{"http PUT", "agent.update", "http", func() (int, string) {
			r := cfg.Do("PUT", base+"/keeper", agEntry{Name: "keeper", Kind: "claude", Model: "opus"})
			return r.Status, r.Trace
		}},
		{"http DELETE", "agent.remove", "http", func() (int, string) {
			r := cfg.Do("DELETE", base+"/keeper", nil)
			return r.Status, r.Trace
		}},
		{"cli add", "agent.add", "cli", func() (int, string) {
			r := i.CLI("project", "agents", "add", "--id", pid, "--name", "newcomer", "--kind", "claude", "--model", "sonnet")
			return r.Code, r.Trace
		}},
		{"cli update", "agent.update", "cli", func() (int, string) {
			r := i.CLI("project", "agents", "update", "--id", pid, "--name", "keeper", "--model", "opus")
			return r.Code, r.Trace
		}},
		{"cli remove", "agent.remove", "cli", func() (int, string) {
			r := i.CLI("project", "agents", "remove", "--id", pid, "--name", "keeper")
			return r.Code, r.Trace
		}},
	}
	for _, d := range doors {
		code, trace := d.call()
		want := http.StatusForbidden
		if d.via == "cli" {
			want = 1
		}
		if code != want {
			t.Fatalf("%s with the owner refusing answered %d, want %d", d.name, code, want)
		}
		g2RequireRefusal(t, i, d.event, trace, "project.agent", d.via)
		if got := agList(t, i, pid); !g2Equal(got, before) {
			t.Fatalf("%s changed the list to %+v, want %+v", d.name, got, before)
		}
	}
	if n := len(g2Rows(i, "config_change", "ok", map[string]any{"credential": "agent_entry"})); n != approved {
		t.Fatalf("refused writes left %d agent_entry config_change rows, want the %d from the approved add", n, approved)
	}
}

func TestAgentStartByEntry(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-start", nil)
	agAdd(t, i, pid, agEntry{Name: "researcher", Kind: "claude", Model: "sonnet"})
	agAdd(t, i, pid, agEntry{Name: "reviewer", Kind: "claude", Model: "opus"})

	res := agSessionStart(i, pid, "Researcher")
	agRunTurn(t, i, res)
	var s struct {
		Model        string `json:"model"`
		ProviderType string `json:"providerType"`
	}
	res.JSON(t, &s)
	if s.ProviderType != "claude" || s.Model != "sonnet" {
		t.Fatalf("the session is %+v, want claude on sonnet", s)
	}
	// With no command the kind's default binary ran, with the entry's model.
	starts := agStartArgvs(i, "claude")
	if len(starts) != 1 || !agHasPair(starts[0], "--model", "sonnet") {
		t.Fatalf("the default claude started as %v, want one start with --model sonnet", starts)
	}
	// The typed name matches without regard to case, so the event and the
	// audit row may carry the stored spelling.
	ev := requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: res.Trace, Fields: map[string]any{"status": "ok"}})
	if !strings.EqualFold(ev.Str("entry"), "researcher") {
		t.Fatalf("the session.launch event names entry %q, want researcher", ev.Str("entry"))
	}
	named := false
	for _, row := range g2RowsWait(t, i, "session_launch", "ok", nil) {
		args, _ := row["args"].(map[string]any)
		entry, _ := args["entry"].(string)
		named = named || strings.EqualFold(entry, "researcher")
	}
	if !named {
		t.Fatalf("no ok session_launch audit row names the entry")
	}

	// Naming both a model and an entry is refused before any launch.
	r := i.CLI("session", "start", "--project", pid, "--entry", "reviewer", "--model", "haiku")
	if r.Code != 1 || !strings.Contains(string(r.Stderr), "send model or entry, not both") {
		t.Fatalf("start with model and entry exited %d with stderr %q, want 1 \"send model or entry, not both\"", r.Code, r.Stderr)
	}
}

func TestAgentStartEntryScopedToProject(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	p1 := agProject(t, i, "acme-one", nil)
	p2 := agProject(t, i, "acme-two", nil)
	p3 := agProject(t, i, "acme-three", nil)
	agAdd(t, i, p1, agEntry{Name: "researcher", Kind: "claude", Model: "sonnet"})
	agAdd(t, i, p2, agEntry{Name: "researcher", Kind: "claude", Model: "opus"})

	agRunTurn(t, i, agSessionStart(i, p2, "researcher"))
	starts := agStartArgvs(i, "claude")
	if len(starts) != 1 || !agHasPair(starts[0], "--model", "opus") {
		t.Fatalf("a start in project two ran %v, want one start with its own model opus", starts)
	}

	r := agSessionStart(i, p3, "researcher")
	if r.Code != 1 || !strings.Contains(string(r.Stderr), `no agent "researcher" in acme-three`) {
		t.Fatalf("a start in a project without the entry exited %d with stderr %q, want 1 naming the project", r.Code, r.Stderr)
	}
	if got := agStartArgvs(i, "claude"); len(got) != 1 {
		t.Fatalf("the refused start still launched: %d claude starts, want 1", len(got))
	}
}

func TestAgentStartUnknownEntry(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-unknown", nil)
	empty := agProject(t, i, "acme-bare", nil)
	agAdd(t, i, pid, agEntry{Name: "zeta", Kind: "claude", Model: "sonnet"})
	agAdd(t, i, pid, agEntry{Name: "alpha", Kind: "claude", Model: "sonnet"})

	run := i.SocketHTTP(i.Credential("runner"))
	resp := run.Do("POST", "/api/sessions", map[string]any{"projectId": pid, "entry": "nope"})
	if msg := agSimple(t, resp, http.StatusNotFound); msg != `no agent "nope" in acme-unknown; agents: zeta, alpha` {
		t.Fatalf("entry_not_found message is %q", msg)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace, Fields: map[string]any{"status": "denied", "reason": "entry_not_found"}})
	if rows := g2RowsWait(t, i, "session_launch", "denied", nil); len(rows) == 0 {
		t.Fatalf("no denied session_launch audit row for the refused start")
	}

	bare := run.Do("POST", "/api/sessions", map[string]any{"projectId": empty, "entry": "nope"})
	if msg := agSimple(t, bare, http.StatusNotFound); msg != `no agent "nope" in acme-bare; acme-bare has no agents` {
		t.Fatalf("entry_not_found message for a project with no agents is %q", msg)
	}

	// The answers are the end of the launch path: nothing started.
	if got := agStartArgvs(i, "claude"); len(got) != 0 {
		t.Fatalf("a refused start launched an agent: %v", got)
	}
	list, _ := g1sListSessions(t, i)
	if len(list.Sessions) != 0 {
		t.Fatalf("a refused start left %d sessions", len(list.Sessions))
	}
}

func TestAgentStartModelNotOffered(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-offer", nil)
	agAdd(t, i, pid, agEntry{Name: "oddclaude", Kind: "claude", Model: "gpt-x"})
	agAdd(t, i, pid, agEntry{Name: "oddcodex", Kind: "codex", Model: "gpt-x"})

	run := i.SocketHTTP(i.Credential("runner"))
	for _, c := range []struct {
		entry, kind string
		offered     []string
	}{
		{"oddclaude", "claude", []string{"haiku", "sonnet", "opus"}},
		{"oddcodex", "codex", []string{"fake-echo"}},
	} {
		resp := run.Do("POST", "/api/sessions", map[string]any{"projectId": pid, "entry": c.entry})
		msg := agSimple(t, resp, http.StatusBadRequest)
		if !strings.Contains(msg, `model "gpt-x" is not offered for `+c.kind) {
			t.Fatalf("%s message is %q", c.entry, msg)
		}
		for _, m := range c.offered {
			if !strings.Contains(msg, m) {
				t.Fatalf("%s message %q does not list the offered model %s", c.entry, msg, m)
			}
		}
		requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace, Fields: map[string]any{"status": "denied", "reason": "model_not_offered"}})
	}
	if got := len(agStartArgvs(i, "claude")) + len(agStartArgvs(i, "codex")); got != 0 {
		t.Fatalf("a refused start launched %d agents", got)
	}
}

func TestAgentStartPiBrokerModel(t *testing.T) {
	t.Parallel()
	models := &harness.FakeModelHostSpec{ID: "acme-models", Models: []json.RawMessage{
		json.RawMessage(`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":8192}`),
		json.RawMessage(`{"id":"local/coder","object":"model","owned_by":"fake","context_length":8192}`),
	}}
	i := agStart(t, func(o *harness.Options) { o.FakeModelHost = models })
	i.WaitModelHost(agHostDeadline)
	pid := agProject(t, i, "acme-pi", nil)
	agAdd(t, i, pid, agEntry{Name: "piLocal", Kind: "pi", Model: "local/coder"})

	res := agSessionStart(i, pid, "piLocal")
	agRunTurn(t, i, res)
	starts := agStartArgvs(i, "pi")
	if len(starts) != 1 || !agHasPair(starts[0], "--model", "local/coder") {
		t.Fatalf("the pi entry started as %v, want one start with --model local/coder", starts)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: res.Trace, Fields: map[string]any{"status": "ok", "entry": "piLocal"}})
}

func TestAgentStartLauncherLocal(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-launch", nil)
	// The log sits in the project folder, which the session may write.
	log := filepath.Join(i.Home, "work", "acme-launch", "wrapper.log")
	wrapper := filepath.Join(i.Home, "acmebin", "mywrapper")
	agWrapper(t, i, wrapper, log, "claude")
	agAdd(t, i, pid, agEntry{Name: "wrapped", Kind: "claude", Model: "sonnet", Command: wrapper, Args: []string{"--profile", "work"}})

	res := agSessionStart(i, pid, "wrapped")
	agRunTurn(t, i, res)
	lines := agReadLines(t, log)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "--profile work ") || !strings.Contains(lines[0], "--model sonnet") {
		t.Fatalf("the wrapper ran as %q, want one run with --profile work before the kind's own arguments including --model sonnet", lines)
	}
	if got := agStartArgvs(i, "claude"); len(got) != 1 {
		t.Fatalf("the wrapper handed over to %d claude starts, want 1", len(got))
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: res.Trace, Fields: map[string]any{"status": "ok", "entry": "wrapped", "command": wrapper}})
}

func TestAgentStartLauncherOnHost(t *testing.T) {
	t.Parallel()
	i := agHostStart(t)
	pid, remote := agHost(t, i)
	log := filepath.Join(remote, "wrapper.log")
	wrapper := filepath.Join(remote, "hostbin", "mywrapper")
	agWrapper(t, i, wrapper, log, "claude")
	agAdd(t, i, pid, agEntry{Name: "wrapped", Kind: "claude", Model: "sonnet", Command: wrapper, Args: []string{"--profile", "work"}})

	res := agSessionStart(i, pid, "wrapped")
	agRunTurn(t, i, res)
	lines := agReadLines(t, log)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "--profile work ") {
		t.Fatalf("the host wrapper ran as %q, want one run with --profile work first", lines)
	}
	ran := false
	for _, c := range i.SSHCalls() {
		ran = ran || (c.Dest == "acme@testbox" && strings.Contains(c.Script(), wrapper))
	}
	if !ran {
		t.Fatalf("the ssh stub never carried the wrapper path %s to acme@testbox", wrapper)
	}
}

func TestAgentNoListAsToday(t *testing.T) {
	t.Parallel()
	i := agStart(t)
	pid := agProject(t, i, "acme-today", nil)

	if _, has := agStoredProject(t, i, pid)["agents"]; has {
		t.Fatalf("a project with no list stores an agents key")
	}
	var view map[string]json.RawMessage
	i.HTTP(i.Credential("reader")).Do("GET", "/api/projects/"+pid, nil).JSON(t, &view)
	if _, has := view["agents"]; has {
		t.Fatalf("the project view of a project with no list carries an agents key")
	}

	// A raw model start works as before.
	res := i.MustCLI("session", "start", "--project", pid, "--model", "sonnet", "--json")
	agRunTurn(t, i, res)

	// Removing the last entry returns the record to no key.
	agAdd(t, i, pid, agEntry{Name: "temp", Kind: "claude", Model: "sonnet"})
	if _, has := agStoredProject(t, i, pid)["agents"]; !has {
		t.Fatalf("an added entry is not stored under agents")
	}
	i.MustCLI("project", "agents", "remove", "--id", pid, "--name", "temp")
	if _, has := agStoredProject(t, i, pid)["agents"]; has {
		t.Fatalf("removing the last entry left an agents key in settings.json")
	}
}
