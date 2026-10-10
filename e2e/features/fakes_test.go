package features

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

func echoCatalogue() harness.Catalogue {
	return harness.Catalogue{Tools: []json.RawMessage{
		json.RawMessage(`{"name":"acme_echo","description":"Echo the arguments","inputSchema":{"type":"object","properties":{}},"x-fake":{"echo":true}}`),
		json.RawMessage(`{"name":"acme_ok","description":"Say ok","inputSchema":{"type":"object","properties":{}}}`),
	}}
}

type toolRow struct {
	Name string `json:"name"`
}

func toolNames(t *testing.T, i *harness.Instance, id string) ([]string, harness.Response) {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/mcps/"+id+"/tools", nil, harness.ReqOpts{Trace: harness.NewTrace(t)})
	if resp.Status != 200 {
		return nil, resp
	}
	var rows []toolRow
	resp.JSON(t, &rows)
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	return names, resp
}

func hasAll(got []string, want ...string) bool {
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func methods(calls []harness.Call) map[string]bool {
	m := map[string]bool{}
	for _, c := range calls {
		m[c.Method] = true
	}
	return m
}

func TestFakeMCPStdioListsTools(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: readOnly,
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: echoCatalogue()}},
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, 60*time.Second)
	names, resp := toolNames(t, i, "acme-stdio")
	if !hasAll(names, "acme_echo", "acme_ok") {
		t.Fatalf("GET tools answered %d with %v, want acme_echo and acme_ok", resp.Status, names)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.tools.list", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "mcp_id": "acme-stdio"}})
	m := methods(i.FakeMCPCalls("acme-stdio"))
	if !m["initialize"] || !m["tools/list"] {
		t.Fatalf("fake call log methods %v lack initialize or tools/list", m)
	}
}

func TestFakeMCPHTTPListsTools(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: readOnly,
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-http", Transport: "http", Catalogue: echoCatalogue()}},
	})
	if u := i.FakeMCPURL("acme-http"); !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Fatalf("fake MCP URL %q is not a loopback http URL", u)
	}
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-http", "state": "up"}}, 60*time.Second)
	names, resp := toolNames(t, i, "acme-http")
	if !hasAll(names, "acme_echo", "acme_ok") {
		t.Fatalf("GET tools answered %d with %v, want acme_echo and acme_ok", resp.Status, names)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.tools.list", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "mcp_id": "acme-http"}})
	calls := i.FakeMCPCalls("acme-http")
	m := methods(calls)
	if !m["initialize"] || !m["tools/list"] {
		t.Fatalf("fake call log methods %v lack initialize or tools/list", m)
	}
	for _, c := range calls {
		if c.Method == "tools/list" && c.Transport != "http" {
			t.Fatalf("tools/list logged transport %q, want http", c.Transport)
		}
	}
}

func TestFakeMCPOAuthAuthenticates(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: readOnly,
		Presence:    map[string]harness.Outcome{"mcp.oauth.start": harness.OutcomeApprove},
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-oauth", Transport: "http", OAuth: true, Catalogue: echoCatalogue()}},
	})
	p := i.StartCLI(harness.CLIOpts{Deadline: 2 * time.Minute}, "mcp", "authenticate", "--id", "acme-oauth", "--json")
	var first struct {
		ID               string `json:"id"`
		AuthorizationURL string `json:"authorization_url"`
		Authenticated    bool   `json:"authenticated"`
	}
	if err := json.Unmarshal(p.FirstLine(60*time.Second), &first); err != nil {
		t.Fatalf("decoding the first line: %v", err)
	}
	if first.AuthorizationURL == "" || first.Authenticated {
		t.Fatalf("first line %+v, want an authorization_url and authenticated unset", first)
	}
	i.CompleteOAuth(first.AuthorizationURL)
	res := p.Wait()
	if res.Code != 0 {
		t.Fatalf("mcp authenticate exited %d", res.Code)
	}
	lines := bufio.NewScanner(bytes.NewReader(res.Stdout))
	var last struct {
		Authenticated bool `json:"authenticated"`
	}
	for lines.Scan() {
		json.Unmarshal(lines.Bytes(), &last)
	}
	if !last.Authenticated {
		t.Fatalf("no line with authenticated:true in %q", res.Stdout)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.oauth.start", Fields: map[string]any{"status": "ok", "mcp_id": "acme-oauth"}})

	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-oauth", "state": "up"}}, 60*time.Second)
	names, resp := toolNames(t, i, "acme-oauth")
	if !hasAll(names, "acme_echo") {
		t.Fatalf("after authentication GET tools answered %d with %v", resp.Status, names)
	}
	if !methods(i.FakeMCPCalls("acme-oauth"))["oauth/token"] {
		t.Fatalf("the fake logged no oauth/token request")
	}
}

func fakeEchoModels() *harness.FakeModelHostSpec {
	return &harness.FakeModelHostSpec{
		ID:     "acme-models",
		Models: []json.RawMessage{json.RawMessage(`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":8192}`)},
	}
}

type modelList struct {
	Status string `json:"status"`
	Models []struct {
		ID string `json:"id"`
	} `json:"models"`
}

func modelIDs(t *testing.T, i *harness.Instance) (modelList, []string) {
	t.Helper()
	var ml modelList
	i.MustCLI("model", "list", "--json").JSON(t, &ml)
	var ids []string
	for _, m := range ml.Models {
		ids = append(ids, m.ID)
	}
	return ml, ids
}

func TestFakeModelHostServesCatalogue(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{FakeModelHost: fakeEchoModels()})
	i.WaitModelHost(60 * time.Second)
	i.WaitSessionHost(60 * time.Second)
	ml, ids := modelIDs(t, i)
	if ml.Status != "ok" || !hasAll(ids, "fake-echo") {
		t.Fatalf("model list status %q ids %v, want ok and fake-echo", ml.Status, ids)
	}
	found := false
	for _, c := range i.FakeModelHostCalls() {
		found = found || c.Method == "GET /v1/models"
	}
	if !found {
		t.Fatalf("the fake model host logged no GET /v1/models")
	}
}

// startAgentSession creates a project that allows the template and starts a
// session with the model, returning the session id.
func startAgentSession(t *testing.T, i *harness.Instance, template, model string) string {
	t.Helper()
	dir := filepath.Join(i.Home, "work", "acme")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body, _ := json.Marshal(map[string]any{"name": "Acme " + template, "path": dir, "allowed_templates": []string{template}})
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	r := i.MustCLI("session", "start", "--project", p.ID, "--model", model, "--json")
	var s struct {
		SessionID string `json:"sessionId"`
	}
	r.JSON(t, &s)
	if s.SessionID == "" {
		t.Fatalf("session start printed no sessionId")
	}
	return s.SessionID
}

func askAndCheck(t *testing.T, i *harness.Instance, persona, sessionID string) {
	t.Helper()
	r := i.MustCLI("session", "message", "--id", sessionID, "--text", "hi", "--json")
	var out struct {
		Text string `json:"text"`
	}
	r.JSON(t, &out)
	if out.Text != "echo: hi" {
		t.Fatalf("reply text %q, want %q", out.Text, "echo: hi")
	}
	events := map[string]bool{}
	for _, c := range i.FakeAgentCalls(persona) {
		events[c.Event] = true
	}
	if !events["start"] || !events["input"] {
		t.Fatalf("fake %s log events %v lack start or input", persona, events)
	}
}

func TestFakeClaudeAnswers(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant})
	i.WaitSessionHost(60 * time.Second)
	askAndCheck(t, i, "claude", startAgentSession(t, i, "claude-code", "sonnet"))
}

func TestFakePiAnswers(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant, FakeModelHost: fakeEchoModels()})
	i.WaitModelHost(60 * time.Second)
	i.WaitSessionHost(60 * time.Second)
	_, ids := modelIDs(t, i)
	piModel := ""
	for _, id := range ids {
		if strings.HasPrefix(id, "pi/") && strings.HasSuffix(id, "/fake-echo") {
			piModel = id
		}
	}
	if piModel == "" {
		t.Fatalf("model list %v holds no pi/<provider>/fake-echo", ids)
	}
	askAndCheck(t, i, "pi", startAgentSession(t, i, "pi", piModel))
}

func TestFakeCodexAnswers(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant})
	i.WaitSessionHost(60 * time.Second)
	_, ids := modelIDs(t, i)
	if !hasAll(ids, "codex/fake-echo") {
		t.Fatalf("model list %v holds no codex/fake-echo", ids)
	}
	askAndCheck(t, i, "codex", startAgentSession(t, i, "codex", "codex/fake-echo"))
}
