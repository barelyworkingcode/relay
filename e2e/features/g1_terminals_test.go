package features

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"relaye2e/harness"
)

const g1tWait = 60 * time.Second

var g1tCredentials = []harness.CredentialSpec{
	{Name: "runner", Classes: []string{"execute"}},
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
}

// g1tTemplate builds one terminal template object for Options.Settings.
func g1tTemplate(t *testing.T, id, command string, args ...string) json.RawMessage {
	t.Helper()
	if args == nil {
		args = []string{}
	}
	raw, err := json.Marshal(map[string]any{"id": id, "name": id, "command": command, "args": args})
	if err != nil {
		t.Fatalf("encoding template %s: %v", id, err)
	}
	return raw
}

func g1tTemplates(t *testing.T, templates ...json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(templates)
	if err != nil {
		t.Fatalf("encoding terminal_templates: %v", err)
	}
	return map[string]json.RawMessage{"terminal_templates": raw}
}

// g1tProject creates a local project over a fresh folder under HOME and
// returns it with its folder. body holds extra project fields.
func g1tProject(t *testing.T, i *harness.Instance, name string, body map[string]any) (project, string) {
	t.Helper()
	dir := filepath.Join(i.Home, "work", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving %s: %v", dir, err)
	}
	full := map[string]any{"name": name, "path": real}
	for k, v := range body {
		full[k] = v
	}
	enc, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("encoding the project body: %v", err)
	}
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: enc}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id")
	}
	return p, real
}

func TestSandboxRunsInProject(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence: approveGrant,
		Settings: g1tTemplates(t,
			g1tTemplate(t, "acme-exit", "/bin/sh", "-c", "exit 7"),
			g1tTemplate(t, "acme-unlisted", "/bin/sh", "-c", "exit 0")),
	})
	i.WaitSessionHost(g1tWait)
	_, dir := g1tProject(t, i, "acme-sbx", map[string]any{"allowed_templates": []string{"acme-exit"}})

	trace := harness.NewTrace(t)
	res := i.StartCLITTY(harness.CLIOpts{Trace: trace}, dir, "sandbox", "acme-exit").Wait()
	if res.Code != 7 {
		t.Fatalf("relay sandbox exited %d, want the tool's own status 7\noutput: %s", res.Code, res.Stdout)
	}
	requireEvent(t, i, harness.EventQuery{Key: "sandbox.attach", Trace: trace, Fields: map[string]any{"status": "ok", "template": "acme-exit"}})

	outside := filepath.Join(i.Home, "elsewhere")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("creating %s: %v", outside, err)
	}
	denyTrace := harness.NewTrace(t)
	refused := i.StartCLITTY(harness.CLIOpts{Trace: denyTrace}, outside, "sandbox", "acme-exit").Wait()
	if refused.Code != 1 {
		t.Fatalf("relay sandbox from a folder in no project exited %d, want 1\noutput: %s", refused.Code, refused.Stdout)
	}
	requireEvent(t, i, harness.EventQuery{Key: "sandbox.attach", Trace: denyTrace, Fields: map[string]any{"status": "error", "reason": "not_found"}})

	unlistedTrace := harness.NewTrace(t)
	unlisted := i.StartCLITTY(harness.CLIOpts{Trace: unlistedTrace}, dir, "sandbox", "acme-unlisted").Wait()
	if unlisted.Code != 1 {
		t.Fatalf("relay sandbox of a template outside allowed_templates exited %d, want 1\noutput: %s", unlisted.Code, unlisted.Stdout)
	}
	requireEvent(t, i, harness.EventQuery{Key: "sandbox.attach", Trace: unlistedTrace, Fields: map[string]any{"status": "denied"}})
}

// g1tAddTemplate creates a terminal template through the configure door.
func g1tAddTemplate(t *testing.T, i *harness.Instance, id, command string, args ...string) {
	t.Helper()
	if args == nil {
		args = []string{}
	}
	resp := i.HTTP(i.Credential("configurer")).Do("POST", "/api/terminal/templates", map[string]any{
		"id": id, "name": id, "command": command, "args": args,
	})
	if resp.Status != 201 {
		t.Fatalf("POST /api/terminal/templates for %s answered %d, want 201", id, resp.Status)
	}
}

func TestSandboxContainment(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant, Credentials: g1tCredentials})
	i.WaitSessionHost(g1tWait)
	own, ownDir := g1tProject(t, i, "acme-own", map[string]any{"allowed_templates": []string{"*"}})
	_, otherDir := g1tProject(t, i, "acme-other", map[string]any{"allowed_templates": []string{"*"}})
	ownFile := filepath.Join(ownDir, "marker.txt")
	otherFile := filepath.Join(otherDir, "secret.txt")
	for _, f := range []string{ownFile, otherFile} {
		if err := os.WriteFile(f, []byte("acme"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", f, err)
		}
	}
	g1tAddTemplate(t, i, "acme-read-own", "/bin/cat", ownFile)
	g1tAddTemplate(t, i, "acme-write-own", "/bin/sh", "-c", "echo acme > "+filepath.Join(ownDir, "written.txt"))
	g1tAddTemplate(t, i, "acme-read-other", "/bin/cat", otherFile)
	g1tAddTemplate(t, i, "acme-write-other", "/bin/sh", "-c", "echo acme > "+filepath.Join(otherDir, "written.txt"))

	run := func(template string) (harness.Result, harness.Event) {
		t.Helper()
		trace := harness.NewTrace(t)
		res := i.StartCLITTY(harness.CLIOpts{Trace: trace}, ownDir, "sandbox", template).Wait()
		return res, requireEvent(t, i, harness.EventQuery{Key: "sandbox.attach", Trace: trace, Fields: map[string]any{"status": "ok", "template": template}})
	}

	for _, template := range []string{"acme-read-own", "acme-write-own"} {
		res, ev := run(template)
		if res.Code != 0 {
			t.Fatalf("%s inside its own project exited %d, want 0\noutput: %s", template, res.Code, res.Stdout)
		}
		if ev.Str("project_id") != own.ID {
			t.Fatalf("sandbox.attach for %s names project %q, want %q", template, ev.Str("project_id"), own.ID)
		}
	}
	if _, err := os.Stat(filepath.Join(ownDir, "written.txt")); err != nil {
		t.Fatalf("the write inside the own project left no file: %v", err)
	}
	for _, template := range []string{"acme-read-other", "acme-write-other"} {
		if res, _ := run(template); res.Code == 0 {
			t.Fatalf("%s reached another project's folder and exited 0\noutput: %s", template, res.Stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(otherDir, "written.txt")); !os.IsNotExist(err) {
		t.Fatalf("a sandboxed session wrote into another project (stat error: %v)", err)
	}
}

type g1tSession struct {
	SessionID string `json:"sessionId"`
}

type g1tSessionRow struct {
	ID        string `json:"id"`
	Live      bool   `json:"live"`
	Headless  bool   `json:"headless"`
	Attention *struct {
		State string `json:"state"`
	} `json:"attention"`
	Origin string `json:"origin"`
}

func g1tListSessions(t *testing.T, i *harness.Instance) []g1tSessionRow {
	t.Helper()
	var out struct {
		Sessions []g1tSessionRow `json:"sessions"`
	}
	i.MustCLI("session", "list", "--json").JSON(t, &out)
	return out.Sessions
}

func g1tFindSession(rows []g1tSessionRow, id string) *g1tSessionRow {
	for k := range rows {
		if rows[k].ID == id {
			return &rows[k]
		}
	}
	return nil
}

// g1tStartHeadless starts a headless tracked agent session on the model.
func g1tStartHeadless(t *testing.T, i *harness.Instance, projectID, model string) string {
	t.Helper()
	var s g1tSession
	i.MustCLI("session", "start", "--project", projectID, "--model", model, "--settings", `{"headless":true,"agent":true}`, "--json").JSON(t, &s)
	if s.SessionID == "" {
		t.Fatalf("session start printed no sessionId")
	}
	return s.SessionID
}

func TestDropInHandsOver(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant})
	i.WaitSessionHost(g1tWait)
	p, dir := g1tProject(t, i, "acme-dropin", map[string]any{"allowed_templates": []string{"claude-code", "codex"}})

	claudeID := g1tStartHeadless(t, i, p.ID, "sonnet")
	i.MustCLI("session", "message", "--id", claudeID, "--text", "hi", "--json")

	trace := harness.NewTrace(t)
	tty := i.StartCLITTY(harness.CLIOpts{Trace: trace}, dir, "drop-in", claudeID)
	ev := i.WaitEvent(harness.EventQuery{Key: "session.drop_in", Trace: trace, Fields: map[string]any{"status": "ok", "session_id": claudeID}}, g1tWait)
	if ev.Str("terminal_id") == "" {
		t.Fatalf("session.drop_in ok carries no terminal_id: %v", ev)
	}
	if row := g1tFindSession(g1tListSessions(t, i), claudeID); row == nil || row.Attention == nil || row.Attention.State != "running" {
		t.Fatalf("while the terminal holds the session its row is %+v, want attention running", row)
	}
	handedBack := time.Now()
	tty.Hangup()
	tty.Wait()
	i.WaitEvent(harness.EventQuery{Key: "session.state", Since: handedBack, Fields: map[string]any{"session_id": claudeID, "from": "running", "to": "idle"}}, g1tWait)
	row := g1tFindSession(g1tListSessions(t, i), claudeID)
	if row == nil || row.Attention == nil || row.Attention.State != "idle" {
		t.Fatalf("after the terminal closed the session row is %+v, want attention idle", row)
	}

	codexID := g1tStartHeadless(t, i, p.ID, "codex/fake-echo")
	for name, id := range map[string]string{"not_claude": codexID, "session_not_found": "no-such-session"} {
		refuseTrace := harness.NewTrace(t)
		refused := i.StartCLITTY(harness.CLIOpts{Trace: refuseTrace}, dir, "drop-in", id).Wait()
		if refused.Code != 1 {
			t.Fatalf("drop-in of %s exited %d, want 1\noutput: %s", name, refused.Code, refused.Stdout)
		}
		requireEvent(t, i, harness.EventQuery{Key: "session.drop_in", Trace: refuseTrace, Fields: map[string]any{"status": "denied", "reason": name}})
	}
}

func TestLaunchAuditCapped(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant, Credentials: g1tCredentials})
	i.WaitSessionHost(g1tWait)
	runner := i.SocketHTTP(i.Credential("runner"))

	const projectIDCap = 64
	long := strings.Repeat("p", 20*projectIDCap)
	resp := runner.Do("POST", "/api/sessions", map[string]any{"projectId": long, "model": "sonnet"})
	if resp.Status != 403 {
		t.Fatalf("a launch naming an unknown project answered %d, want 403", resp.Status)
	}
	var capped []string
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_launch", Outcome: "denied"}) {
		actor, _ := row["actor"].(map[string]any)
		if id, _ := actor["project_id"].(string); strings.HasPrefix(id, "ppp") {
			capped = append(capped, id)
		}
	}
	if len(capped) != 1 {
		t.Fatalf("found %d denied session_launch rows for the long project id, want 1", len(capped))
	}
	if n := utf8.RuneCountInString(capped[0]); n > projectIDCap+1 || !strings.HasSuffix(capped[0], "…") {
		t.Fatalf("the audited project id holds %d runes (ends in the cut marker: %v), want at most %d and the marker", n, strings.HasSuffix(capped[0], "…"), projectIDCap+1)
	}

	before := len(i.Audit(harness.AuditQuery{Event: "session_launch"}))
	body := []byte(`{"name":"` + strings.Repeat("a", 1<<20+1) + `"}`)
	big := runner.Do("POST", "/api/sessions", body)
	if big.Status != 413 {
		t.Fatalf("a launch body over 1 MiB answered %d, want 413", big.Status)
	}
	if after := len(i.Audit(harness.AuditQuery{Event: "session_launch"})); after != before {
		t.Fatalf("the oversized launch changed the session_launch rows from %d to %d", before, after)
	}
	if got := i.Events(harness.EventQuery{Key: "session.launch", Trace: big.Trace}); len(got) != 0 {
		t.Fatalf("the oversized launch wrote %d session.launch events, want none", len(got))
	}
}

func TestSessionModeChange(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant})
	i.WaitSessionHost(g1tWait)
	id := startAgentSession(t, i, "claude-code", "sonnet")
	i.MustCLI("session", "message", "--id", id, "--text", "hi", "--json")

	// A local claude session cannot change mode in place (docs/cli.md, session
	// mode): the host asks for a resume. A mode change never reports ok, and
	// the session stays live.
	for _, mode := range []string{"plan", "bogus"} {
		trace := harness.NewTrace(t)
		res := i.CLIWith(harness.CLIOpts{Trace: trace}, "session", "mode", "--id", id, "--mode", mode, "--json")
		if res.Code != 1 || len(res.Stdout) != 0 {
			t.Fatalf("session mode %s on a local claude session exited %d with %d stdout bytes, want exit 1 and none", mode, res.Code, len(res.Stdout))
		}
		requireEvent(t, i, harness.EventQuery{Key: "session.mode", Trace: trace, Fields: map[string]any{"status": "error", "session_id": id}})
		if got := i.Events(harness.EventQuery{Key: "session.mode", Trace: trace, Fields: map[string]any{"status": "ok"}}); len(got) != 0 {
			t.Fatalf("session mode %s wrote an ok session.mode event", mode)
		}
	}
	if row := g1tFindSession(g1tListSessions(t, i), id); row == nil || !row.Live {
		t.Fatalf("the session is no longer live after refused mode changes: %+v", row)
	}
}

type g1tTemplateRow struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func g1tListTemplates(t *testing.T, i *harness.Instance, projectID string) ([]g1tTemplateRow, harness.Response) {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/terminal/templates?project="+projectID, nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/terminal/templates answered %d, want 200", resp.Status)
	}
	var rows []g1tTemplateRow
	resp.JSON(t, &rows)
	return rows, resp
}

func g1tHasTemplate(rows []g1tTemplateRow, id string) *g1tTemplateRow {
	for k := range rows {
		if rows[k].ID == id {
			return &rows[k]
		}
	}
	return nil
}

func TestTemplateListAndGet(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    approveGrant,
		Credentials: g1tCredentials,
		Settings:    g1tTemplates(t, g1tTemplate(t, "acme-tool", "/bin/echo", "acme")),
	})
	p, _ := g1tProject(t, i, "acme-templates", map[string]any{"allowed_templates": []string{"*"}})
	reader := i.HTTP(i.Credential("reader"))

	rows, listed := g1tListTemplates(t, i, p.ID)
	if g1tHasTemplate(rows, "acme-tool") == nil {
		t.Fatalf("the template list holds %d rows and not acme-tool", len(rows))
	}
	requireEvent(t, i, harness.EventQuery{Key: "template.list", Trace: listed.Trace, Fields: map[string]any{"status": "ok", "count": len(rows)}})

	got := reader.Do("GET", "/api/terminal/templates/acme-tool", nil)
	if got.Status != 200 {
		t.Fatalf("GET of a listed template answered %d, want 200", got.Status)
	}
	var one g1tTemplateRow
	got.JSON(t, &one)
	if one.ID != "acme-tool" || one.Command != "/bin/echo" {
		t.Fatalf("GET returned template %+v, want acme-tool running /bin/echo", one)
	}
	requireEvent(t, i, harness.EventQuery{Key: "template.get", Trace: got.Trace, Fields: map[string]any{"status": "ok", "template_id": "acme-tool"}})

	missing := reader.Do("GET", "/api/terminal/templates/acme-missing", nil)
	if missing.Status != 404 {
		t.Fatalf("GET of an unknown template answered %d, want 404", missing.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "template.get", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestTemplateCRUD(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant, Credentials: g1tCredentials})
	p, _ := g1tProject(t, i, "acme-crud", map[string]any{"allowed_templates": []string{"*"}})
	cfg := i.HTTP(i.Credential("configurer"))

	created := cfg.Do("POST", "/api/terminal/templates", map[string]any{"id": "acme-crud-tool", "name": "Acme tool", "command": "/bin/echo"})
	if created.Status != 201 {
		t.Fatalf("POST a template answered %d, want 201", created.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "template.create", Trace: created.Trace, Fields: map[string]any{"status": "ok", "template_id": "acme-crud-tool"}})
	rows, _ := g1tListTemplates(t, i, p.ID)
	if row := g1tHasTemplate(rows, "acme-crud-tool"); row == nil || row.Name != "Acme tool" {
		t.Fatalf("after create the list holds %+v for acme-crud-tool", row)
	}

	updated := cfg.Do("PUT", "/api/terminal/templates/acme-crud-tool", map[string]any{"name": "Acme renamed", "command": "/bin/echo"})
	if updated.Status != 200 {
		t.Fatalf("PUT a template answered %d, want 200", updated.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "template.update", Trace: updated.Trace, Fields: map[string]any{"status": "ok", "template_id": "acme-crud-tool"}})
	rows, _ = g1tListTemplates(t, i, p.ID)
	if row := g1tHasTemplate(rows, "acme-crud-tool"); row == nil || row.Name != "Acme renamed" {
		t.Fatalf("after update the list holds %+v for acme-crud-tool", row)
	}

	removed := cfg.Do("DELETE", "/api/terminal/templates/acme-crud-tool", nil)
	if removed.Status != 204 {
		t.Fatalf("DELETE a template answered %d, want 204", removed.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "template.remove", Trace: removed.Trace, Fields: map[string]any{"status": "ok", "template_id": "acme-crud-tool"}})
	rows, _ = g1tListTemplates(t, i, p.ID)
	if g1tHasTemplate(rows, "acme-crud-tool") != nil {
		t.Fatalf("after remove the list still holds acme-crud-tool")
	}

	nameless := cfg.Do("POST", "/api/terminal/templates", map[string]any{"id": "acme-nameless", "command": "/bin/echo"})
	if nameless.Status != 400 {
		t.Fatalf("POST a template with no name answered %d, want 400", nameless.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "template.create", Trace: nameless.Trace, Fields: map[string]any{"status": "error", "reason": "invalid"}})
	rows, _ = g1tListTemplates(t, i, p.ID)
	if g1tHasTemplate(rows, "acme-nameless") != nil {
		t.Fatalf("a refused template is in the list")
	}
}

func TestChatToolSearch(t *testing.T) {
	t.Parallel()
	var tools []json.RawMessage
	var names []string
	for k := 0; k < 12; k++ {
		name := fmt.Sprintf("acme_tool_%02d", k)
		names = append(names, name)
		raw, _ := json.Marshal(map[string]any{
			"name": name, "description": strings.Repeat("Acme tool that does a long documented thing. ", 12),
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		})
		tools = append(tools, raw)
	}
	i := harness.Start(t, harness.Options{
		Presence: approveGrant,
		FakeMCPs: []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: harness.Catalogue{Tools: tools}}},
		FakeModelHost: &harness.FakeModelHostSpec{ID: "acme-models", Models: []json.RawMessage{
			json.RawMessage(`{"id":"fake-echo","object":"model","owned_by":"fake","context_length":512}`),
		}},
	})
	i.WaitModelHost(g1tWait)
	i.WaitSessionHost(g1tWait)
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, g1tWait)
	p, _ := g1tProject(t, i, "acme-search", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}, "allowed_models": []string{"fake-echo"}, "allowed_templates": []string{"chat"}})

	// toolsOfTurn runs one chat turn and returns the tool names the model host
	// received with it.
	toolsOfTurn := func() []string {
		t.Helper()
		before := len(g1tChatRequests(t, i))
		var s g1tSession
		i.MustCLI("session", "start", "--project", p.ID, "--model", "fake-echo", "--settings", `{"useRelayTools":true}`, "--json").JSON(t, &s)
		i.MustCLI("session", "message", "--id", s.SessionID, "--text", "hi", "--json")
		reqs := g1tChatRequests(t, i)
		if len(reqs) != before+1 {
			t.Fatalf("one chat turn made %d model requests, want 1", len(reqs)-before)
		}
		return reqs[before]
	}

	// With no project skill nothing can be hidden: every tool goes to the model.
	if all := toolsOfTurn(); !hasAll(all, names...) {
		t.Fatalf("before the skill exists the model received %v, want all of %v", all, names)
	}
	// relay writes the project skill from the grant; the tools it lists are
	// then hidden behind the two fixed tools because they outweigh a tenth of
	// the model's context.
	i.MustCLI("project", "regen-skill", "--id", p.ID, "--json")
	got := toolsOfTurn()
	if !hasAll(got, "tool_search", "call_tool") {
		t.Fatalf("with tool search on the model received %v, want tool_search and call_tool", got)
	}
	for _, n := range names {
		if hasAll(got, n) {
			t.Fatalf("with tool search on the model still received the hidden tool %s: %v", n, got)
		}
	}
}

// g1tChatRequests returns, per chat-completions request the fake model host
// logged, the names of the tools the request offered.
func g1tChatRequests(t *testing.T, i *harness.Instance) [][]string {
	t.Helper()
	var out [][]string
	for _, c := range i.FakeModelHostCalls() {
		if c.Method != "POST /v1/chat/completions" {
			continue
		}
		var body struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(c.Params, &body); err != nil {
			t.Fatalf("decoding a logged chat request: %v", err)
		}
		names := []string{}
		for _, tool := range body.Tools {
			names = append(names, tool.Function.Name)
		}
		out = append(out, names)
	}
	return out
}

func TestCodexModelOutsideAllowedRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant, Credentials: g1tCredentials})
	i.WaitSessionHost(g1tWait)
	p, _ := g1tProject(t, i, "acme-codex", map[string]any{"allowed_templates": []string{"codex"}, "allowed_models": []string{"codex/other"}})
	before := len(g1tListSessions(t, i))

	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": "codex/fake-echo"})
	if resp.Status != 403 {
		t.Fatalf("a codex launch outside allowed_models answered %d, want 403", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: resp.Trace, Fields: map[string]any{"status": "denied"}})
	if got := len(g1tListSessions(t, i)); got != before {
		t.Fatalf("the refused launch changed the session list from %d to %d", before, got)
	}
}

const g1tFrameWait = 30 * time.Second

type g1tFrame struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	State     string `json:"state"`
	Excerpt   string `json:"excerpt"`
	Since     string `json:"since"`
	At        string `json:"at"`
}

// g1tFramesUntil reads frames until stop accepts one and returns every frame
// read, the accepted one last. Next fails t at its deadline, so a frame that
// never comes ends the test.
func g1tFramesUntil(t *testing.T, ws *harness.WSConn, stop func(g1tFrame) bool) []g1tFrame {
	t.Helper()
	var out []g1tFrame
	for {
		var f g1tFrame
		raw := ws.Next(g1tFrameWait)
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("decoding a /ws frame: %v\n%s", err, raw)
		}
		out = append(out, f)
		if stop(f) {
			return out
		}
	}
}

func TestAgentStateFrames(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    approveGrant,
		Credentials: append([]harness.CredentialSpec{{Name: "proxier", Classes: []string{"proxy"}}}, g1tCredentials...),
	})
	i.WaitSessionHost(g1tWait)
	p, _ := g1tProject(t, i, "acme-state", map[string]any{"allowed_templates": []string{"claude-code", "codex"}})
	ws := i.WebSocket("/ws", i.Credential("proxier"))

	for _, model := range []string{"sonnet", "codex/fake-echo"} {
		var s g1tSession
		i.MustCLI("session", "start", "--project", p.ID, "--model", model, "--json").JSON(t, &s)
		ws.Send(map[string]any{"type": "join_session", "sessionId": s.SessionID})
		g1tFramesUntil(t, ws, func(f g1tFrame) bool { return f.Type == "session_joined" })

		since := time.Now()
		i.MustCLI("session", "message", "--id", s.SessionID, "--text", "hi", "--json")
		frames := g1tFramesUntil(t, ws, func(f g1tFrame) bool {
			return f.Type == "session_state" && f.SessionID == s.SessionID && f.State == "idle"
		})
		turnDone := -1
		for k, f := range frames {
			if f.Type == "turn_done" && f.SessionID == s.SessionID {
				turnDone = k
				if f.Excerpt != "echo: hi" || f.At == "" {
					t.Fatalf("%s: turn_done carries excerpt %q at %q, want the reply and a time", model, f.Excerpt, f.At)
				}
			}
			if f.Type == "session_state" && f.SessionID == s.SessionID && f.Since == "" {
				t.Fatalf("%s: session_state %q has no since", model, f.State)
			}
			if f.Type == "session_state" && f.State == "asking" && model == "codex/fake-echo" {
				t.Fatalf("a codex session reported the asking state")
			}
		}
		if turnDone < 0 {
			t.Fatalf("%s: no turn_done frame before the session went idle: %+v", model, frames)
		}
		last := frames[len(frames)-1]
		if turnDone > len(frames)-2 || last.Type != "session_state" {
			t.Fatalf("%s: turn_done must come before the idle session_state: %+v", model, frames)
		}
		i.WaitEvent(harness.EventQuery{Key: "session.state", Since: since, Fields: map[string]any{"session_id": s.SessionID, "to": "idle"}}, g1tWait)
	}
}

var g1tScope = harness.ReqOpts{Header: http.Header{"X-Relay-Scope": {"chief-of-staff"}}}

func TestChiefOfStaffScopeDoors(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    approveGrant,
		Credentials: append([]harness.CredentialSpec{{Name: "proxier", Classes: []string{"proxy"}}}, g1tCredentials...),
	})
	i.WaitSessionHost(g1tWait)
	p, _ := g1tProject(t, i, "acme-cos", map[string]any{"allowed_templates": []string{"claude-code"}})
	var s g1tSession
	i.MustCLI("session", "start", "--project", p.ID, "--model", "sonnet", "--json").JSON(t, &s)
	cos := i.SocketHTTP(i.Credential("proxier"))

	scoped := func(trace string) harness.ReqOpts {
		return harness.ReqOpts{Trace: trace, Header: g1tScope.Header}
	}

	list := cos.Do("GET", "/api/sessions", nil, g1tScope)
	if list.Status != 200 {
		t.Fatalf("GET /api/sessions in the scope answered %d, want 200", list.Status)
	}
	var listed struct {
		Sessions []g1tSessionRow `json:"sessions"`
	}
	list.JSON(t, &listed)
	if g1tFindSession(listed.Sessions, s.SessionID) == nil {
		t.Fatalf("the scoped session list lacks %s", s.SessionID)
	}

	wsTrace := harness.NewTrace(t)
	ws := i.WebSocket("/ws", i.Credential("proxier"), scoped(wsTrace))

	send := cos.Do("POST", "/api/chief-of-staff/messages", map[string]any{"sessionId": s.SessionID, "text": "hi"}, scoped(harness.NewTrace(t)))
	if send.Status != 202 {
		t.Fatalf("POST /api/chief-of-staff/messages in the scope answered %d, want 202", send.Status)
	}
	var sent struct {
		SessionID string `json:"sessionId"`
		Origin    string `json:"origin"`
	}
	send.JSON(t, &sent)
	if sent.SessionID != s.SessionID || sent.Origin != "chief-of-staff" {
		t.Fatalf("the send answered %+v, want session %s and origin chief-of-staff", sent, s.SessionID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.send", Trace: send.Trace, Fields: map[string]any{"status": "ok", "session_id": s.SessionID}})

	// The scoped /ws receives the hub's broadcasts, and its first frame from
	// the client closes it.
	g1tFramesUntil(t, ws, func(f g1tFrame) bool { return f.Type == "turn_done" && f.SessionID == s.SessionID })
	ws.Send(map[string]any{"type": "join_session", "sessionId": s.SessionID})
	i.WaitEvent(harness.EventQuery{Key: "session.ws.close", Trace: wsTrace}, g1tWait)

	launchTrace := harness.NewTrace(t)
	launch := cos.Do("POST", "/api/sessions", map[string]any{"projectId": p.ID, "model": "sonnet"}, scoped(launchTrace))
	if launch.Status != 403 {
		t.Fatalf("POST /api/sessions in the scope answered %d, want 403", launch.Status)
	}
	denied := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		denied = denied || (row["method"] == "POST" && row["path"] == "/api/sessions")
	}
	if !denied {
		t.Fatalf("no denied control_decision row for the scoped POST /api/sessions")
	}
	if rows := g1tListSessions(t, i); len(rows) != 1 {
		t.Fatalf("the refused scoped launch left %d sessions, want 1", len(rows))
	}
}

func TestChiefOfStaffStart(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    approveGrant,
		Credentials: append([]harness.CredentialSpec{{Name: "proxier", Classes: []string{"proxy", "execute"}}}, g1tCredentials...),
	})
	i.WaitSessionHost(g1tWait)
	p, dir := g1tProject(t, i, "acme-cos-start", map[string]any{"allowed_templates": []string{"claude-code"}})
	sub := filepath.Join(dir, "pkg")
	outside := filepath.Join(i.Home, "elsewhere")
	for _, d := range []string{sub, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatalf("creating the escaping link: %v", err)
	}
	cos := i.SocketHTTP(i.Credential("proxier"))
	start := func(body any) harness.Response {
		t.Helper()
		return cos.Do("POST", "/api/chief-of-staff/sessions", body, harness.ReqOpts{Header: g1tScope.Header})
	}
	remote := g1tRemoteProfile(t, i)
	hostID := g1tHostRecord(t, i)
	hosted := g1tHostedProject(t, i, hostID)

	expect := func(what string, body any, status int, event, reason string) harness.Response {
		t.Helper()
		r := start(body)
		if r.Status != status {
			t.Fatalf("%s: the start answered %d, want %d\nbody: %s", what, r.Status, status, r.Body)
		}
		q := harness.EventQuery{Key: "chief_of_staff.start", Trace: r.Trace, Fields: map[string]any{"status": event}}
		if reason != "" {
			q.Fields["reason"] = reason
		}
		requireEvent(t, i, q)
		return r
	}
	started := func(what string, body any) string {
		t.Helper()
		r := expect(what, body, 201, "ok", "")
		var out struct {
			SessionID string `json:"sessionId"`
			Origin    string `json:"origin"`
		}
		r.JSON(t, &out)
		if out.SessionID == "" || out.Origin != "chief-of-staff" {
			t.Fatalf("%s: the start answered %+v, want a session id and origin chief-of-staff", what, out)
		}
		return out.SessionID
	}

	rootID := started("a start in the project root", map[string]any{"projectId": p.ID, "prompt": "hi", "model": "sonnet"})
	folderID := started("a start in a folder of the project", map[string]any{"projectId": p.ID, "prompt": "hi", "model": "sonnet", "folder": "pkg"})

	listed := cos.Do("GET", "/api/sessions", nil, g1tScope)
	var rows struct {
		Sessions []g1tSessionRow `json:"sessions"`
	}
	listed.JSON(t, &rows)
	for _, id := range []string{rootID, folderID} {
		if row := g1tFindSession(rows.Sessions, id); row == nil || row.Origin != "chief-of-staff" {
			t.Fatalf("the session list holds %+v for started session %s, want origin chief-of-staff", row, id)
		}
	}
	launched := 0
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_launch", Outcome: "ok"}) {
		if args, _ := row["args"].(map[string]any); args["origin"] == "chief-of-staff" {
			launched++
		}
	}
	if launched != 2 {
		t.Fatalf("found %d ok session_launch rows with origin chief-of-staff, want 2", launched)
	}

	before := len(rows.Sessions)
	expect("a folder that links out of the project", map[string]any{"projectId": p.ID, "prompt": "hi", "model": "sonnet", "folder": "escape"}, 403, "denied", "")
	expect("a remote project", map[string]any{"projectId": remote, "prompt": "hi", "model": "sonnet"}, 403, "denied", "")
	expect("a terminal start on a host", map[string]any{"projectId": hosted, "prompt": "hi", "model": "sonnet", "mode": "terminal"}, 400, "error", "invalid")
	expect("a folder with a .. segment", map[string]any{"projectId": p.ID, "prompt": "hi", "model": "sonnet", "folder": "../elsewhere"}, 400, "error", "invalid")
	expect("a malformed body", []byte(`{"projectId":`), 400, "error", "invalid")
	after := cos.Do("GET", "/api/sessions", nil, g1tScope)
	after.JSON(t, &rows)
	if len(rows.Sessions) != before {
		t.Fatalf("the refused starts changed the session list from %d to %d", before, len(rows.Sessions))
	}
}

func g1tRemoteProfile(t *testing.T, i *harness.Instance) string {
	t.Helper()
	var p project
	body, _ := json.Marshal(map[string]any{"name": "acme-profile", "kind": "remote"})
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	return p.ID
}

// g1tHostRecord adds an SSH host record that nothing listens behind: the
// refusals that need a hosted project never reach the host.
func g1tHostRecord(t *testing.T, i *harness.Instance) string {
	t.Helper()
	resp := i.HTTP(i.Credential("configurer")).Do("POST", "/api/hosts", map[string]any{"name": "acme-box", "target": "acme@127.0.0.1", "port": 1})
	if resp.Status != 201 {
		t.Fatalf("POST /api/hosts answered %d, want 201\nbody: %s", resp.Status, resp.Body)
	}
	var h struct {
		ID string `json:"id"`
	}
	resp.JSON(t, &h)
	return h.ID
}

func g1tHostedProject(t *testing.T, i *harness.Instance, hostID string) string {
	t.Helper()
	var p project
	body, _ := json.Marshal(map[string]any{"name": "acme-hosted", "path": "/home/acme/work", "host_id": hostID})
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	return p.ID
}

// g1tShellTurn sends "!sh CMD" to the fake claude session and returns the exit
// status it reports.
func g1tShellTurn(t *testing.T, i *harness.Instance, sessionID, cmd string) int {
	t.Helper()
	var out struct {
		Text string `json:"text"`
	}
	i.MustCLI("session", "message", "--id", sessionID, "--text", "!sh "+cmd, "--json").JSON(t, &out)
	code, ok := strings.CutPrefix(out.Text, "exit: ")
	if !ok {
		t.Fatalf("the shell turn %q answered %q, want exit: N", cmd, out.Text)
	}
	n, err := strconv.Atoi(code)
	if err != nil {
		t.Fatalf("the shell turn %q answered %q: %v", cmd, out.Text, err)
	}
	return n
}

func TestReadOnlyProjectsSession(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant, Credentials: g1tCredentials})
	i.WaitSessionHost(g1tWait)
	allow := map[string]any{"allowed_templates": []string{"claude-code", "codex"}}
	a, aDir := g1tProject(t, i, "acme-ro-a", allow)
	_, bDir := g1tProject(t, i, "acme-ro-b", allow)
	elsewhere := filepath.Join(i.Home, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatalf("creating %s: %v", elsewhere, err)
	}
	bFile, strayFile := filepath.Join(bDir, "secret.txt"), filepath.Join(elsewhere, "stray.txt")
	for _, f := range []string{bFile, strayFile} {
		if err := os.WriteFile(f, []byte("acme"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", f, err)
		}
	}
	runner := i.SocketHTTP(i.Credential("runner"))
	launch := func(model string, settings map[string]any) harness.Response {
		t.Helper()
		return runner.Do("POST", "/api/sessions", map[string]any{"projectId": a.ID, "model": model, "settings": settings})
	}
	idOf := func(r harness.Response) string {
		t.Helper()
		if r.Status != 201 {
			t.Fatalf("a launch answered %d, want 201\nbody: %s", r.Status, r.Body)
		}
		var s g1tSession
		r.JSON(t, &s)
		return s.SessionID
	}

	// Control: an ordinary session writes its own project, so the refusals
	// below come from the option.
	plain := idOf(launch("sonnet", map[string]any{}))
	if code := g1tShellTurn(t, i, plain, "echo acme > "+filepath.Join(aDir, "plain.txt")); code != 0 {
		t.Fatalf("an ordinary session could not write its own project: exit %d", code)
	}

	ro := launch("sonnet", map[string]any{"readOnlyProjects": true})
	roID := idOf(ro)
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: ro.Trace, Fields: map[string]any{"status": "ok", "kind": "claude"}})
	if code := g1tShellTurn(t, i, roID, "/bin/cat "+bFile); code != 0 {
		t.Fatalf("a read-only-projects session could not read another project: exit %d", code)
	}
	for _, target := range []string{filepath.Join(aDir, "ro.txt"), filepath.Join(bDir, "ro.txt")} {
		if code := g1tShellTurn(t, i, roID, "echo acme > "+target); code == 0 {
			t.Fatalf("a read-only-projects session wrote %s", target)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("%s exists after a refused write (stat error: %v)", target, err)
		}
	}
	if code := g1tShellTurn(t, i, roID, "/bin/cat "+strayFile); code == 0 {
		t.Fatalf("a read-only-projects session read a file outside every project")
	}

	notClaude := launch("codex/fake-echo", map[string]any{"readOnlyProjects": true})
	if notClaude.Status != 400 {
		t.Fatalf("the option on a codex launch answered %d, want 400", notClaude.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: notClaude.Trace, Fields: map[string]any{"status": "denied"}})

	since := time.Now()
	g1tProject(t, i, "acme-ro-c", nil)
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Since: since, Fields: map[string]any{"session_id": roID}}, g1tWait)
	if row := g1tFindSession(g1tListSessions(t, i), roID); row != nil && row.Live {
		t.Fatalf("the read-only-projects session is still live after a project was added")
	}
	if row := g1tFindSession(g1tListSessions(t, i), plain); row == nil || !row.Live {
		t.Fatalf("the ordinary session ended with the project change: %+v", row)
	}
}

func TestSessionExitRecorded(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant})
	i.WaitSessionHost(g1tWait)
	id := startAgentSession(t, i, "claude-code", "sonnet")
	if g1tFindSession(g1tListSessions(t, i), id) == nil {
		t.Fatalf("the new session %s is not in the list", id)
	}

	since := time.Now()
	i.MustCLI("session", "stop", "--id", id)
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Since: since, Fields: map[string]any{"status": "ok", "session_id": id}}, g1tWait)

	ended := 0
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_end", Outcome: "ok"}) {
		if args, _ := row["args"].(map[string]any); args["session_id"] == id {
			ended++
		}
	}
	if ended != 1 {
		t.Fatalf("found %d ok session_end rows for %s, want 1", ended, id)
	}
	if g1tFindSession(g1tListSessions(t, i), id) != nil {
		t.Fatalf("the stopped session %s is still in the list", id)
	}
}
