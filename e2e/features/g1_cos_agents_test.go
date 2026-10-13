package features

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"relaye2e/harness"
)

var cosCreds = []harness.CredentialSpec{
	{Name: "cos", Classes: []string{"proxy", "execute", "read"}},
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
}

var cosApprove = map[string]harness.Outcome{
	"project.grant":         harness.OutcomeApprove,
	"project.agent":         harness.OutcomeApprove,
	"project.chat_template": harness.OutcomeApprove,
}

// cosStart boots an instance that approves the project gates and waits for
// the session host.
func cosStart(t *testing.T, mut ...func(*harness.Options)) *harness.Instance {
	t.Helper()
	o := harness.Options{Credentials: cosCreds, Presence: cosApprove}
	for _, m := range mut {
		m(&o)
	}
	i := harness.Start(t, o)
	i.WaitSessionHost(agHostDeadline)
	return i
}

// cosDo sends one request under the Chief of Staff scope.
func cosDo(t *testing.T, i *harness.Instance, method, path string, body any) harness.Response {
	return i.SocketHTTP(i.Credential("cos")).Do(method, path, body, harness.ReqOpts{Header: g1tScope, Trace: harness.NewTrace(t)})
}

type cosError struct {
	Error   string   `json:"error"`
	Message string   `json:"message"`
	Choices []string `json:"choices"`
	Command string   `json:"command"`
	Machine string   `json:"machine"`
}

func cosRefusal(t *testing.T, resp harness.Response, status int, code string) cosError {
	t.Helper()
	var e cosError
	resp.JSON(t, &e)
	if resp.Status != status || e.Error != code {
		t.Fatalf("answered %d %q, want %d %q: %s", resp.Status, e.Error, status, code, resp.Body)
	}
	return e
}

func cosListPath(pid string) string { return "/api/chief-of-staff/projects/" + pid + "/agents" }

func cosEntries(t *testing.T, resp harness.Response) []map[string]any {
	t.Helper()
	if resp.Status != http.StatusOK {
		t.Fatalf("list answered %d, want 200: %s", resp.Status, resp.Body)
	}
	var out struct {
		ProjectID   string           `json:"projectId"`
		ProjectName string           `json:"projectName"`
		Machine     string           `json:"machine"`
		Entries     []map[string]any `json:"entries"`
	}
	resp.JSON(t, &out)
	if out.Machine != "this Mac" {
		t.Fatalf("list names machine %q, want \"this Mac\": %s", out.Machine, resp.Body)
	}
	return out.Entries
}

func cosChats(names ...string) []map[string]any {
	var out []map[string]any
	for _, n := range names {
		out = append(out, map[string]any{"id": "t_" + strings.ToLower(n), "name": n, "model": "fake-echo", "system_prompt": "acme secret system prompt"})
	}
	return out
}

func cosKeys(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// cosProjectView reads a project through the configure-class read door.
func cosProjectView(t *testing.T, i *harness.Instance, pid string) map[string]json.RawMessage {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects/"+pid, nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET project answered %d: %s", resp.Status, resp.Body)
	}
	var v map[string]json.RawMessage
	resp.JSON(t, &v)
	return v
}

// cosLaunchRows reads the session_launch rows (fail-open, so bounded) that
// carry the entry name and the outcome.
func cosLaunchRows(t *testing.T, i *harness.Instance, outcome, entry string) []g4Row {
	t.Helper()
	rows := g4Wait(t, i, g4Has(func(r g4Row) bool {
		return r.str("outcome") == outcome && strings.EqualFold(r.obj("args").str("entry"), entry)
	}), "--event", "session_launch")
	return g4Where(rows, func(r g4Row) bool {
		return r.str("outcome") == outcome && strings.EqualFold(r.obj("args").str("entry"), entry)
	})
}

func TestCosListsAgents(t *testing.T) {
	t.Parallel()
	i := cosStart(t)
	pid := agProject(t, i, "acme-list", nil)
	wrapper := filepath.Join(i.Home, "acmebin", "mywrapper")
	agWrapper(t, i, wrapper, filepath.Join(i.Home, "unused.log"), "claude")
	agAdd(t, i, pid, agEntry{Name: "Researcher", Kind: "claude", Model: "sonnet", Command: wrapper, Args: []string{"--acme-private-flag"}})
	agAdd(t, i, pid, agEntry{Name: "scribe", Kind: "codex"})

	resp := cosDo(t, i, "GET", cosListPath(pid), nil)
	rows := cosEntries(t, resp)
	if len(rows) != 2 {
		t.Fatalf("list holds %d rows, want 2: %s", len(rows), resp.Body)
	}
	if rows[0]["type"] != "agent" || rows[0]["name"] != "Researcher" || rows[0]["kind"] != "claude" || rows[0]["model"] != "sonnet" {
		t.Fatalf("first row is %v, want the Researcher claude agent on sonnet", rows[0])
	}
	if cmd, _ := rows[0]["command"].(string); !filepath.IsAbs(cmd) || !agSame(t, cmd, wrapper) {
		t.Fatalf("first row command is %q, want the absolute path of %s", cmd, wrapper)
	}
	if _, has := rows[0]["args"]; has || strings.Contains(string(resp.Body), "acme-private-flag") {
		t.Fatalf("the list carries the stored args: %s", resp.Body)
	}
	if rows[1]["name"] != "scribe" || rows[1]["kind"] != "codex" || rows[1]["model"] != nil || rows[1]["command"] != nil {
		t.Fatalf("second row is %v, want scribe on codex with no model and no command", rows[1])
	}
	requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.agents.list", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "project_id": pid, "count": 2}})

	remote := g1tCreate(t, i, map[string]any{"name": "acme-remote", "kind": "remote"})
	for _, id := range []string{"p_missing", remote.ID} {
		r := cosDo(t, i, "GET", cosListPath(id), nil)
		cosRefusal(t, r, http.StatusForbidden, "project_not_available")
		requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.agents.list", Trace: r.Trace, Fields: map[string]any{"status": "denied"}})
	}

	// The same credential without the scope header is not a Chief of Staff caller.
	if r := i.SocketHTTP(i.Credential("cos")).Do("GET", cosListPath(pid), nil); r.Status != http.StatusForbidden {
		t.Fatalf("an unscoped list answered %d, want 403: %s", r.Status, r.Body)
	}
}

func TestCosListsChatTemplates(t *testing.T) {
	t.Parallel()
	i := cosStart(t)
	pid := agProject(t, i, "acme-chats", map[string]any{"chat_templates": cosChats("Zed", "Alpha", "alpha")})
	agAdd(t, i, pid, agEntry{Name: "scribe", Kind: "codex"})

	resp := cosDo(t, i, "GET", cosListPath(pid), nil)
	rows := cosEntries(t, resp)
	var order []string
	for _, r := range rows {
		order = append(order, r["type"].(string)+":"+r["name"].(string))
	}
	if want := []string{"agent:scribe", "chat:Zed", "chat:Alpha", "chat:alpha"}; !g2Equal(order, want) {
		t.Fatalf("list order is %v, want agents first then chats as stored, duplicates kept: %v", order, want)
	}
	if got := cosKeys(rows[1]); !g2Equal(got, []string{"model", "name", "type"}) || rows[1]["model"] != "fake-echo" {
		t.Fatalf("chat row has keys %v, want exactly type, name and model", got)
	}
	if strings.Contains(string(resp.Body), "acme secret system prompt") || strings.Contains(string(resp.Body), "t_zed") {
		t.Fatalf("the list leaks a chat template's prompt or id: %s", resp.Body)
	}
}

func TestCosStartsAgentByEntry(t *testing.T) {
	t.Parallel()
	i := cosStart(t)
	pid := agProject(t, i, "acme-entry", nil)
	agAdd(t, i, pid, agEntry{Name: "Researcher", Kind: "claude", Model: "sonnet"})
	ws := i.WebSocket("/ws", i.Credential("cos"), harness.ReqOpts{Header: g1tScope})
	defer ws.Close()

	resp := cosDo(t, i, "POST", "/api/chief-of-staff/sessions", map[string]any{"projectId": pid, "prompt": "acme hello", "entry": "rEsEaRcHeR"})
	if resp.Status != http.StatusCreated {
		t.Fatalf("a start by entry answered %d, want 201: %s", resp.Status, resp.Body)
	}
	var s struct {
		SessionID string `json:"sessionId"`
		Entry     string `json:"entry"`
		EntryType string `json:"entryType"`
	}
	resp.JSON(t, &s)
	if s.Entry != "Researcher" || s.EntryType != "agent" {
		t.Fatalf("the result names entry %q of type %q, want the stored spelling Researcher and agent", s.Entry, s.EntryType)
	}
	requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.start", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "session_id": s.SessionID, "entry": "Researcher", "entry_type": "agent"}})

	// The intent row is durable before the 201.
	intents := 0
	for _, r := range g4Audit(t, i, "--event", "session_message") {
		if r.obj("args").str("session_id") == s.SessionID && r.str("phase") == "intent" {
			intents++
		}
	}
	if intents != 1 {
		t.Fatalf("the start left %d session_message intent rows, want 1", intents)
	}
	g1tAwaitTurnDone(t, ws, s.SessionID)

	if starts := agStartArgvs(i, "claude"); len(starts) != 1 || !agHasPair(starts[0], "--model", "sonnet") {
		t.Fatalf("the claude agent started as %v, want one start with --model sonnet", starts)
	}
	rows := cosLaunchRows(t, i, "ok", "Researcher")
	if len(rows) != 1 || rows[0].obj("args").str("origin") != "chief-of-staff" || rows[0].obj("args").str("session_id") != s.SessionID {
		t.Fatalf("ok session_launch rows naming the entry are %v, want one with origin chief-of-staff for session %s", rows, s.SessionID)
	}
}

func TestCosStartEntryRefusals(t *testing.T) {
	t.Parallel()
	i := cosStart(t)
	pid := agProject(t, i, "acme-refuse", map[string]any{"chat_templates": cosChats("Planner")})
	agAdd(t, i, pid, agEntry{Name: "codex", Kind: "codex"})
	agAdd(t, i, pid, agEntry{Name: "mywrapper", Kind: "claude", Model: "sonnet"})
	agAdd(t, i, pid, agEntry{Name: "oddclaude", Kind: "claude", Model: "gpt-x"})
	twins := agProject(t, i, "acme-twins", map[string]any{"chat_templates": cosChats("Twin", "twin")})

	start := func(body map[string]any) harness.Response {
		body["prompt"] = "acme hello"
		return cosDo(t, i, "POST", "/api/chief-of-staff/sessions", body)
	}
	for _, c := range []struct {
		name, entry string
		body        map[string]any
		status      int
		code        string
		choices     []string
		denied      bool
	}{
		{"unknown entry", "codx", map[string]any{"projectId": pid, "entry": "codx"}, 404, "entry_not_found", []string{"codex", "mywrapper", "oddclaude", "Planner"}, true},
		{"two chats of one name", "twin", map[string]any{"projectId": twins, "entry": "twin"}, 409, "entry_ambiguous", []string{"Twin"}, true},
		{"a model the kind does not offer", "oddclaude", map[string]any{"projectId": pid, "entry": "oddclaude"}, 400, "model_not_offered", []string{"haiku", "sonnet", "opus"}, true},
		{"model and entry", "", map[string]any{"projectId": pid, "entry": "codex", "model": "sonnet"}, 400, "model_or_entry", nil, false},
		{"neither model nor entry", "", map[string]any{"projectId": pid}, 400, "model_or_entry", nil, false},
		{"entry with a terminal", "", map[string]any{"projectId": pid, "entry": "codex", "mode": "terminal"}, 400, "mode_invalid", nil, false},
	} {
		resp := start(c.body)
		e := cosRefusal(t, resp, c.status, c.code)
		if !g2Equal(e.Choices, c.choices) && !(len(e.Choices) == 0 && c.choices == nil) {
			t.Fatalf("%s answered choices %v, want %v", c.name, e.Choices, c.choices)
		}
		if c.denied {
			requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.start", Trace: resp.Trace, Fields: map[string]any{"status": "denied", "reason": c.code}})
			if rows := cosLaunchRows(t, i, "denied", c.entry); len(rows) == 0 {
				t.Fatalf("%s wrote no denied session_launch row", c.name)
			}
		}
	}

	// Every refusal ended before a launch.
	if got := len(agStartArgvs(i, "claude")) + len(agStartArgvs(i, "codex")); got != 0 {
		t.Fatalf("a refused start launched %d agents", got)
	}
	if list, _ := g1sListSessions(t, i); len(list.Sessions) != 0 {
		t.Fatalf("refused starts left %d sessions", len(list.Sessions))
	}
}

func TestCosStartsChatTemplate(t *testing.T) {
	t.Parallel()
	i := cosStart(t, func(o *harness.Options) {
		o.FakeModelHost = g1tEchoHost()
		o.FakeMCPs = acmeStdioMCP()
	})
	i.WaitModelHost(agHostDeadline)
	g2WaitMCPUp(i, "acme-stdio")
	const marker = "ACME-MARKER-4417"
	const prompt = "You are the acme researcher."
	tmpl := []map[string]any{{"id": "t_res", "name": "Researcher", "model": "fake-echo", "system_prompt": prompt}}
	pid := agProject(t, i, "acme-chat", map[string]any{
		"chat_templates": tmpl, "allowed_templates": []string{"chat"}, "allowed_models": []string{"fake-echo"}, "allowed_mcp_ids": []string{"acme-stdio"},
	})
	if err := os.WriteFile(filepath.Join(g1tDir(i, "acme-chat"), "CLAUDE.md"), []byte(marker+"\n"), 0o600); err != nil {
		t.Fatalf("writing CLAUDE.md: %v", err)
	}

	resp := cosDo(t, i, "POST", "/api/chief-of-staff/sessions", map[string]any{"projectId": pid, "prompt": "acme hello", "entry": "researcher"})
	if resp.Status != http.StatusCreated {
		t.Fatalf("a chat start answered %d, want 201: %s", resp.Status, resp.Body)
	}
	var s struct {
		SessionID string `json:"sessionId"`
		Entry     string `json:"entry"`
		EntryType string `json:"entryType"`
		Kind      string `json:"kind"`
	}
	resp.JSON(t, &s)
	if s.Entry != "Researcher" || s.EntryType != "chat" || s.Kind != "chat" {
		t.Fatalf("the result is %+v, want chat Researcher of kind chat", s)
	}
	// A chat session sends no turn_done, and only a /ws send_message writes
	// chat.turn. A second message on /ws queues behind the prompt's turn, so
	// its chat.turn proves the prompt's model call is in the host's log.
	ws := i.WebSocket("/ws", i.Credential("cos"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_session", "sessionId": s.SessionID})
	trace := harness.NewTrace(t)
	ws.Send(map[string]any{"type": "send_message", "sessionId": s.SessionID, "text": "acme follow-up", "trace_id": trace})
	i.WaitEvent(harness.EventQuery{Key: "chat.turn", Trace: trace, Fields: map[string]any{"status": "ok", "session_id": s.SessionID}}, g1tEventDeadline)

	var named struct {
		Sessions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"sessions"`
	}
	i.MustCLI("session", "list", "--json").JSON(t, &named)
	found := false
	for _, r := range named.Sessions {
		if r.ID == s.SessionID {
			found = true
			if r.Name != "acme-chat - Researcher" {
				t.Fatalf("the session is named %q, want \"acme-chat - Researcher\"", r.Name)
			}
		}
	}
	if !found {
		t.Fatalf("session %s is not listed", s.SessionID)
	}

	var first *harness.Call
	for _, c := range i.FakeModelHostCalls() {
		if c.Method == "POST /v1/chat/completions" {
			first = &c
			break
		}
	}
	if first == nil {
		t.Fatalf("the model host saw no chat completion")
	}
	for _, want := range []string{prompt, marker} {
		if !strings.Contains(string(first.Params), want) {
			t.Fatalf("the first model call lacks %q: %s", want, first.Params)
		}
	}
	if names := g1tToolNames([]harness.Call{*first}); !hasAll(names, "acme_echo") {
		t.Fatalf("the first model call carried tools %v, want relay's tools including acme_echo", names)
	}
}

func TestCosCannotWriteAgentEntry(t *testing.T) {
	t.Parallel()
	i := cosStart(t)
	pid := agProject(t, i, "acme-noagent", nil)
	agAdd(t, i, pid, agEntry{Name: "keeper", Kind: "claude", Model: "sonnet"})
	before := agList(t, i, pid)
	written := len(g2Rows(i, "config_change", "ok", map[string]any{"credential": "agent_entry"}))

	base := "/api/projects/" + pid + "/agents"
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", base, agEntry{Name: "newcomer", Kind: "claude", Model: "sonnet"}},
		{"PUT", base + "/keeper", agEntry{Name: "keeper", Kind: "claude", Model: "opus"}},
		{"DELETE", base + "/keeper", nil},
	} {
		if r := cosDo(t, i, c.method, c.path, c.body); r.Status != http.StatusForbidden {
			t.Fatalf("a scoped %s %s answered %d with presence approving, want 403: %s", c.method, c.path, r.Status, r.Body)
		}
		if rows := g2RowsWait(t, i, "control_decision", "denied", map[string]any{"method": c.method, "path": c.path}); len(rows) == 0 {
			t.Fatalf("no denied control_decision row for scoped %s %s", c.method, c.path)
		}
	}
	if got := agList(t, i, pid); !g2Equal(got, before) {
		t.Fatalf("scoped writes changed the list to %+v, want %+v", got, before)
	}
	if n := len(g2Rows(i, "config_change", "ok", map[string]any{"credential": "agent_entry"})); n != written {
		t.Fatalf("scoped writes left %d agent_entry rows, want the %d from setup", n, written)
	}
}

func TestCosCannotWriteChatTemplate(t *testing.T) {
	t.Parallel()
	i := cosStart(t)
	pid := agProject(t, i, "acme-nochat", map[string]any{"chat_templates": cosChats("Keeper")})
	before := cosProjectView(t, i, pid)["chat_templates"]

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/projects/" + pid + "/chat-templates", map[string]any{"name": "Newcomer", "model": "fake-echo"}},
		{"PUT", "/api/projects/" + pid, map[string]any{"chat_templates": cosChats("Keeper", "Newcomer")}},
	} {
		if r := cosDo(t, i, c.method, c.path, c.body); r.Status != http.StatusForbidden {
			t.Fatalf("a scoped %s %s answered %d with presence approving, want 403: %s", c.method, c.path, r.Status, r.Body)
		}
		if rows := g2RowsWait(t, i, "control_decision", "denied", map[string]any{"method": c.method, "path": c.path}); len(rows) == 0 {
			t.Fatalf("no denied control_decision row for scoped %s %s", c.method, c.path)
		}
	}
	if after := cosProjectView(t, i, pid)["chat_templates"]; string(after) != string(before) {
		t.Fatalf("scoped writes changed the chat templates to %s, want %s", after, before)
	}
}

var cosUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestChatTemplateAddNeedsPresence(t *testing.T) {
	t.Parallel()
	i := cosStart(t)
	pid := agProject(t, i, "acme-gate", nil)
	cfg := i.HTTP(i.Credential("configurer"))
	path := "/api/projects/" + pid + "/chat-templates"
	input := map[string]any{"name": "Researcher", "model": "fake-echo"}

	i.SetPresence(map[string]harness.Outcome{"project.chat_template": harness.OutcomeDeny})
	denied := cfg.Do("POST", path, input)
	if denied.Status != http.StatusForbidden {
		t.Fatalf("an add with the owner refusing answered %d, want 403: %s", denied.Status, denied.Body)
	}
	g2RequireRefusal(t, i, "chat_template.add", denied.Trace, "project.chat_template", "http")
	if got := cosProjectView(t, i, pid)["chat_templates"]; len(got) != 0 && string(got) != "null" && string(got) != "[]" {
		t.Fatalf("a refused add stored %s", got)
	}
	if rows := g2Rows(i, "config_change", "ok", map[string]any{"credential": "chat_template"}); len(rows) != 0 {
		t.Fatalf("a refused add left %d chat_template rows", len(rows))
	}

	i.SetPresence(cosApprove)
	ok := cfg.Do("POST", path, input)
	if ok.Status != http.StatusCreated {
		t.Fatalf("an approved add answered %d, want 201: %s", ok.Status, ok.Body)
	}
	var added struct {
		ProjectID string `json:"projectId"`
		Template  struct{ ID, Name, Model string }
	}
	ok.JSON(t, &added)
	if added.ProjectID != pid || !cosUUID.MatchString(added.Template.ID) || added.Template.Name != "Researcher" || added.Template.Model != "fake-echo" {
		t.Fatalf("the add answered %+v, want the new template with a UUID id", added)
	}
	requireEvent(t, i, harness.EventQuery{Key: "chat_template.add", Trace: ok.Trace, Fields: map[string]any{"status": "ok", "project_id": pid}})

	var stored []struct{ ID, Name, Model string }
	if err := json.Unmarshal(cosProjectView(t, i, pid)["chat_templates"], &stored); err != nil || len(stored) != 1 || stored[0].ID != added.Template.ID {
		t.Fatalf("the project view holds %+v (%v), want the added template", stored, err)
	}
	if rows := cosEntries(t, cosDo(t, i, "GET", cosListPath(pid), nil)); len(rows) != 1 || rows[0]["type"] != "chat" || rows[0]["name"] != "Researcher" {
		t.Fatalf("the scoped list holds %v, want the added chat", rows)
	}
	rows := g2Rows(i, "config_change", "ok", map[string]any{"credential": "chat_template", "subject": pid, "subject_name": "acme-gate"})
	if len(rows) != 1 {
		t.Fatalf("the add left %d chat_template config_change rows, want 1", len(rows))
	}
	grants := map[string]bool{}
	for _, g := range rows[0]["grants"].([]any) {
		grants[g.(string)] = true
	}
	if id, _ := rows[0]["presence_id"].(string); id == "" || !grants["action=add"] || !grants["template=Researcher"] || !grants["model=fake-echo"] {
		t.Fatalf("the row is %v, want a presence_id and grants action=add, template=Researcher, model=fake-echo", rows[0])
	}

	agAdd(t, i, pid, agEntry{Name: "scribe", Kind: "codex"})
	remote := g1tCreate(t, i, map[string]any{"name": "acme-remote", "kind": "remote"})
	for _, c := range []struct {
		name, project string
		body          any
		status        int
		code          string
	}{
		{"a name that clashes with a chat", pid, map[string]any{"name": "RESEARCHER", "model": "fake-echo"}, 409, "name_taken"},
		{"a name that clashes with an agent", pid, map[string]any{"name": "Scribe", "model": "fake-echo"}, 409, "name_taken"},
		{"a blank name", pid, map[string]any{"name": "  ", "model": "fake-echo"}, 400, "name_invalid"},
		{"a 61-rune name", pid, map[string]any{"name": strings.Repeat("é", 61), "model": "fake-echo"}, 400, "name_invalid"},
		{"a control character in the name", pid, map[string]any{"name": "Bad\nname", "model": "fake-echo"}, 400, "name_invalid"},
		{"no model", pid, map[string]any{"name": "Modelless"}, 400, "model_required"},
		{"an unknown key", pid, map[string]any{"name": "Extra", "model": "fake-echo", "system_prompt": "x"}, 400, "invalid_body"},
		{"an unknown project", "p_missing", input, 404, "project_not_found"},
		{"a remote project", remote.ID, input, 400, "project_not_available"},
	} {
		cosRefusal(t, cfg.Do("POST", "/api/projects/"+c.project+"/chat-templates", c.body), c.status, c.code)
	}
	if rows := g2Rows(i, "config_change", "ok", map[string]any{"credential": "chat_template"}); len(rows) != 1 {
		t.Fatalf("refused adds left %d chat_template rows in all, want the 1 from the approved add", len(rows))
	}
}

func TestCosAgentProposalNeedsGatedWrite(t *testing.T) {
	t.Parallel()
	i := cosStart(t)
	pid := agProject(t, i, "acme-proposal", nil)
	bin := filepath.Join(i.Home, "acmebin")
	agWrapper(t, i, filepath.Join(bin, "mywrapper"), filepath.Join(i.Home, "unused.log"), "claude")
	if err := os.WriteFile(filepath.Join(i.Home, ".zshrc"), []byte("export PATH="+agShQuote(bin)+":\"$PATH\"\n"), 0o600); err != nil {
		t.Fatalf("writing .zshrc: %v", err)
	}
	i.SetPresence(map[string]harness.Outcome{"project.agent": harness.OutcomeDeny})
	preview := cosListPath(pid) + "/preview"
	proposal := agEntry{Name: "wrapped", Kind: "claude", Model: "sonnet", Command: "mywrapper"}

	resp := cosDo(t, i, "POST", preview, proposal)
	if resp.Status != http.StatusOK {
		t.Fatalf("a preview answered %d, want 200: %s", resp.Status, resp.Body)
	}
	var previewed agWrite
	resp.JSON(t, &previewed)
	if !filepath.IsAbs(previewed.Entry.Command) || !agSame(t, previewed.Entry.Command, filepath.Join(bin, "mywrapper")) {
		t.Fatalf("the preview resolved the command to %q, want the absolute path of %s", previewed.Entry.Command, filepath.Join(bin, "mywrapper"))
	}
	requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.agents.preview", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "project_id": pid}})
	// A preview raises no prompt, so the refusing owner was never asked.
	if got := i.Events(harness.EventQuery{Key: "chief_of_staff.agents.preview", Trace: resp.Trace, Fields: map[string]any{"reason": "presence_refused"}}); len(got) != 0 {
		t.Fatalf("the preview raised a presence prompt")
	}
	if rows := g2Rows(i, "control_decision", "", map[string]any{"method": "project.agent"}); len(rows) != 0 {
		t.Fatalf("the preview left %d project.agent decision rows, want none", len(rows))
	}
	if got := agList(t, i, pid); len(got) != 0 {
		t.Fatalf("the preview stored %+v", got)
	}

	// The proposal completes only through the configure door.
	if r := agPost(t, i, pid, previewed.Entry); r.Status != http.StatusForbidden {
		t.Fatalf("the write with the owner refusing answered %d, want 403: %s", r.Status, r.Body)
	}
	i.SetPresence(cosApprove)
	if added := agAdd(t, i, pid, previewed.Entry); !g2Equal(added.Entry, previewed.Entry) {
		t.Fatalf("the stored entry is %+v, want the previewed %+v", added.Entry, previewed.Entry)
	}

	// Refusals of the preview door.
	cosRefusal(t, cosDo(t, i, "POST", preview, map[string]any{"name": "other", "kind": "claude", "model": "sonnet", "args": []string{"--x"}}), 400, "invalid_body")
	marker := filepath.Join(t.TempDir(), "marker")
	cosRefusal(t, cosDo(t, i, "POST", preview, agEntry{Name: "other", Kind: "claude", Model: "sonnet", Command: `/tmp/x\'; touch ` + marker + `; #`}), 400, "command_invalid")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the shell-quoting command ran: marker stat err = %v", err)
	}
	e := cosRefusal(t, cosDo(t, i, "POST", preview, agEntry{Name: "other", Kind: "claude", Model: "sonnet", Command: "nosuchwrapper"}), 422, "command_not_found")
	if e.Command != "nosuchwrapper" || e.Machine != "this Mac" {
		t.Fatalf("command_not_found names command %q on %q, want nosuchwrapper on this Mac", e.Command, e.Machine)
	}
}

func TestAgentEntryWriteAuditRow(t *testing.T) {
	t.Parallel()
	i := cosStart(t)
	pid := agProject(t, i, "acme-rows", nil)
	wrapper := filepath.Join(i.Home, "acmebin", "mywrapper")
	agWrapper(t, i, wrapper, filepath.Join(i.Home, "unused.log"), "claude")
	cfg := i.HTTP(i.Credential("configurer"))
	base := "/api/projects/" + pid + "/agents"

	agAdd(t, i, pid, agEntry{Name: "httpone", Kind: "claude", Model: "sonnet", Command: wrapper})
	if r := cfg.Do("PUT", base+"/httpone", agEntry{Name: "httptwo", Kind: "claude", Model: "sonnet", Command: wrapper}); r.Status != http.StatusOK {
		t.Fatalf("PUT agents answered %d, want 200: %s", r.Status, r.Body)
	}
	if r := cfg.Do("DELETE", base+"/httptwo", nil); r.Status >= 300 {
		t.Fatalf("DELETE agents answered %d: %s", r.Status, r.Body)
	}
	i.MustCLI("project", "agents", "add", "--id", pid, "--name", "clione", "--kind", "claude", "--model", "sonnet", "--command", wrapper)

	rows := g2Rows(i, "config_change", "ok", map[string]any{"credential": "agent_entry", "subject": pid, "subject_name": "acme-rows"})
	for _, w := range []struct{ action, entry string }{{"add", "httpone"}, {"update", "httptwo"}, {"remove", "httptwo"}, {"add", "clione"}} {
		n := 0
		for _, r := range rows {
			var grants []string
			for _, g := range r["grants"].([]any) {
				grants = append(grants, g.(string))
			}
			if slices.Contains(grants, "action="+w.action) && slices.Contains(grants, "entry="+w.entry) {
				n++
				cmd := ""
				for _, g := range grants {
					if v, ok := strings.CutPrefix(g, "command="); ok {
						cmd = v
					}
				}
				if !agSame(t, cmd, wrapper) {
					t.Fatalf("the %s %s row names command %q, want the absolute path of %s: %v", w.action, w.entry, cmd, wrapper, grants)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%d agent_entry rows record action=%s entry=%s, want 1", n, w.action, w.entry)
		}
	}
}
