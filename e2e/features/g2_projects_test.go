package features

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

const g2MCP = "acme-stdio"

var g2Creds = []harness.CredentialSpec{
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
	{Name: "granter", Classes: []string{"grant"}},
	{Name: "scoped", Classes: []string{"proxy"}},
}

func g2Presence(ops ...string) map[string]harness.Outcome {
	m := map[string]harness.Outcome{}
	for _, op := range ops {
		m[op] = harness.OutcomeApprove
	}
	return m
}

type g2Project struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	AllowedMCPIDs []string `json:"allowed_mcp_ids"`
	DefaultFor    []string `json:"default_for"`
	FilesReadOnly bool     `json:"files_read_only"`
}

// g2Start boots an instance with the G2 credentials and waits until every
// fake MCP is published.
func g2Start(t *testing.T, presence map[string]harness.Outcome, mcps ...harness.FakeMCPSpec) *harness.Instance {
	t.Helper()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: presence, FakeMCPs: mcps})
	for _, m := range mcps {
		i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": m.ID, "state": "up"}}, mcpUpDeadline)
	}
	return i
}

func g2Stdio(cat harness.Catalogue) harness.FakeMCPSpec {
	return harness.FakeMCPSpec{ID: g2MCP, Transport: "stdio", Catalogue: cat}
}

// g2Scoped is the echo catalogue plus a restrict-scope field the fake can
// enumerate.
func g2Scoped() harness.Catalogue {
	cat := echoCatalogue()
	cat.Initialize = json.RawMessage(`{"serverInfo":{"name":"fakemcp","version":"1","contextSchemaVersion":2,"contextSchema":{"acme_scope":{"type":"array","items":{"type":"string"},"description":"Acme folders","scope":"restrict","source":"operator","applies_to":["acme_*"],"enumerable":true}}}}`)
	cat.Enumerate = map[string][]harness.EnumValue{"acme_scope": {{Value: "alpha", Label: "Alpha"}, {Value: "beta", Label: "Beta"}}}
	return cat
}

func g2Marshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding %v: %v", v, err)
	}
	return b
}

// g2Create creates a local project through `relay project create --file -`.
// A nil value in extra drops that key from the body.
func g2Create(t *testing.T, i *harness.Instance, name string, extra map[string]any) g2Project {
	t.Helper()
	dir := filepath.Join(i.Home, "work", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body := map[string]any{"name": name, "path": dir}
	for k, v := range extra {
		if v == nil {
			delete(body, k)
			continue
		}
		body[k] = v
	}
	r := i.CLIWith(harness.CLIOpts{Stdin: g2Marshal(t, body)}, "project", "create", "--file", "-", "--json")
	if r.Code != 0 {
		t.Fatalf("project create exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	var p g2Project
	r.JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id: %s", r.Stdout)
	}
	return p
}

func g2Edit(t *testing.T, i *harness.Instance, id string, body map[string]any) harness.Result {
	t.Helper()
	return i.CLIWith(harness.CLIOpts{Stdin: g2Marshal(t, body)}, "project", "edit", "--id", id, "--file", "-", "--json")
}

func g2Get(t *testing.T, i *harness.Instance, id string) g2Project {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects/"+id, nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/projects/%s answered %d, want 200", id, resp.Status)
	}
	var p g2Project
	resp.JSON(t, &p)
	return p
}

func g2Token(t *testing.T, i *harness.Instance, id string) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	i.MustCLI("project", "token", "--id", id, "--json").JSON(t, &out)
	if out.Token == "" {
		t.Fatalf("project token printed no token for %s", id)
	}
	return out.Token
}

// g2Tools lists the tools a project token reaches over the bridge. It returns
// the names the token sees and the CLI result; a refused token has code 1.
func g2Tools(t *testing.T, i *harness.Instance, token string) ([]string, harness.Result) {
	t.Helper()
	r := i.CLI("mcp", "call", "--token", token, "--list", "--schema")
	if r.Code != 0 || !bytes.HasPrefix(bytes.TrimSpace(r.Stdout), []byte("[")) {
		return nil, r
	}
	var rows []toolRow
	r.JSON(t, &rows)
	var names []string
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return names, r
}

func g2Rows(i *harness.Instance, event, outcome string) []map[string]any {
	return i.Audit(harness.AuditQuery{Event: event, Outcome: outcome})
}

// g2Has reports whether a row carries every key/value pair.
func g2Has(rows []map[string]any, kv map[string]string) bool {
	return len(g2Match(rows, kv)) > 0
}

func g2Match(rows []map[string]any, kv map[string]string) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		ok := true
		for k, v := range kv {
			if s, _ := r[k].(string); s != v {
				ok = false
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

// g2RequireRefused checks a refused gated act: the event reads denied with
// presence_refused on the trace, and the audit log holds the denied
// control_decision row for the gate.
func g2RequireRefused(t *testing.T, i *harness.Instance, key, trace, gate, via string, fields map[string]any) {
	t.Helper()
	want := map[string]any{"status": "denied", "reason": "presence_refused"}
	for k, v := range fields {
		want[k] = v
	}
	requireEvent(t, i, harness.EventQuery{Key: key, Trace: trace, Fields: want})
	if !g2Has(g2Rows(i, "control_decision", "denied"), map[string]string{"method": gate, "via": via}) {
		t.Fatalf("no denied control_decision row with method %s via %s", gate, via)
	}
}

func g2RequireNoProjects(t *testing.T, i *harness.Instance) {
	t.Helper()
	if got := listProjects(t, i); len(got) != 0 {
		t.Fatalf("GET /api/projects lists %d projects, want none", len(got))
	}
}

func g2Headers(kv ...string) http.Header {
	h := http.Header{}
	for n := 0; n+1 < len(kv); n += 2 {
		h.Set(kv[n], kv[n+1])
	}
	return h
}

func TestProjectCreateNarrowDefault(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"))
	dir := filepath.Join(i.Home, "work", "acme-narrow")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}

	r := i.MustCLI("project", "create", "--name", "acme-narrow", "--path", dir, "--json")
	var p g2Project
	r.JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id: %s", r.Stdout)
	}
	if len(p.AllowedMCPIDs) != 0 {
		t.Fatalf("a --name/--path create grants MCPs %v, want none", p.AllowedMCPIDs)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.create", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})
	if !g2Has(g2Rows(i, "config_change", "ok"), map[string]string{"subject": p.ID}) {
		t.Fatalf("no config_change ok row for project %s", p.ID)
	}
	approvals := g2Match(g2Rows(i, "control_decision", "ok"), map[string]string{"method": "project.grant"})
	if len(approvals) == 0 {
		t.Fatalf("no control_decision ok row for project.grant")
	}
	if s, _ := approvals[0]["presence_approver"].(string); s == "" {
		t.Fatalf("the approval row has no presence_approver: %v", approvals[0])
	}
}

func TestProjectCreateDeniedCLI(t *testing.T) {
	t.Parallel()
	i := g2Start(t, map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})
	dir := filepath.Join(i.Home, "work", "acme-denied")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}

	r := i.CLI("project", "create", "--name", "acme-denied", "--path", dir, "--json")
	if r.Code != 1 {
		t.Fatalf("a refused project create exited %d, want 1", r.Code)
	}
	g2RequireRefused(t, i, "project.create", r.Trace, "project.grant", "cli", nil)
	if len(g2Rows(i, "config_change", "")) != 0 {
		t.Fatalf("a refused create wrote a config_change row")
	}
	g2RequireNoProjects(t, i)
}

func TestProjectCreateDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := g2Start(t, map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})
	dir := filepath.Join(i.Home, "work", "acme-denied")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}

	resp := i.HTTP(i.Credential("configurer")).Do("POST", "/api/projects", map[string]any{"name": "acme-denied", "path": dir})
	if resp.Status != 403 {
		t.Fatalf("a refused POST /api/projects answered %d, want 403", resp.Status)
	}
	g2RequireRefused(t, i, "project.create", resp.Trace, "project.grant", "http", nil)
	if len(g2Rows(i, "config_change", "")) != 0 {
		t.Fatalf("a refused create wrote a config_change row")
	}
	g2RequireNoProjects(t, i)
}

func TestProjectWidenApproved(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"), g2Stdio(echoCatalogue()))
	p := g2Create(t, i, "acme-widen", nil)
	before := len(g2Rows(i, "config_change", "ok"))

	resp := i.HTTP(i.Credential("configurer")).Do("PUT", "/api/projects/"+p.ID, map[string]any{"allowed_mcp_ids": []string{g2MCP}})
	if resp.Status != 200 {
		t.Fatalf("an approved widening answered %d, want 200", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.update", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "gated": true, "project_id": p.ID}})
	if got := len(g2Rows(i, "config_change", "ok")); got != before+1 {
		t.Fatalf("config_change ok rows went from %d to %d, want one more", before, got)
	}

	var grants []struct {
		ID   string `json:"id"`
		MCPs []struct {
			MCP string `json:"mcp"`
		} `json:"mcps"`
	}
	i.MustCLI("grant", "--project", p.ID, "--json").JSON(t, &grants)
	if len(grants) != 1 || len(grants[0].MCPs) != 1 || grants[0].MCPs[0].MCP != g2MCP {
		t.Fatalf("relay grant shows %+v, want the one MCP %s", grants, g2MCP)
	}
}

// g2RequireUnchanged checks that a project still has no MCP and is still
// local, read back through the get door.
func g2RequireUnchanged(t *testing.T, i *harness.Instance, id string) {
	t.Helper()
	p := g2Get(t, i, id)
	if len(p.AllowedMCPIDs) != 0 || p.Kind == "remote" {
		t.Fatalf("project %s changed despite the refusal: %+v", id, p)
	}
}

func TestProjectWidenDeniedCLI(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"), g2Stdio(echoCatalogue()))
	p := g2Create(t, i, "acme-widen", nil)
	i.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})
	before := len(g2Rows(i, "config_change", ""))

	r := g2Edit(t, i, p.ID, map[string]any{"allowed_mcp_ids": []string{g2MCP}})
	if r.Code != 1 {
		t.Fatalf("a refused project edit exited %d, want 1", r.Code)
	}
	g2RequireRefused(t, i, "project.update", r.Trace, "project.grant", "cli", map[string]any{"project_id": p.ID})
	if got := len(g2Rows(i, "config_change", "")); got != before {
		t.Fatalf("a refused widening wrote a config_change row")
	}
	g2RequireUnchanged(t, i, p.ID)

	var grants []struct {
		MCPs []json.RawMessage `json:"mcps"`
	}
	i.MustCLI("grant", "--project", p.ID, "--json").JSON(t, &grants)
	if len(grants) != 1 || len(grants[0].MCPs) != 0 {
		t.Fatalf("relay grant shows %+v after a refused widening, want no MCPs", grants)
	}
}

func TestProjectWidenDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"), g2Stdio(echoCatalogue()))
	p := g2Create(t, i, "acme-widen", nil)
	i.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})
	before := len(g2Rows(i, "config_change", ""))

	cases := []struct {
		name string
		body map[string]any
	}{
		{"add an MCP", map[string]any{"allowed_mcp_ids": []string{g2MCP}}},
		{"convert to remote", map[string]any{"kind": "remote"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := i.HTTP(i.Credential("configurer")).Do("PUT", "/api/projects/"+p.ID, c.body)
			if resp.Status != 403 {
				t.Fatalf("a refused PUT answered %d, want 403: %s", resp.Status, resp.Body)
			}
			g2RequireRefused(t, i, "project.update", resp.Trace, "project.grant", "http", map[string]any{"project_id": p.ID})
			g2RequireUnchanged(t, i, p.ID)
		})
	}
	if got := len(g2Rows(i, "config_change", "")); got != before {
		t.Fatalf("a refused widening wrote a config_change row")
	}
}

func TestProjectNarrowUngated(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant", "project.reveal_token"), g2Stdio(echoCatalogue()))
	p := g2Create(t, i, "acme-narrow", map[string]any{"allowed_mcp_ids": []string{g2MCP}})
	token := g2Token(t, i, p.ID)
	names, r := g2Tools(t, i, token)
	if r.Code != 0 || !hasAll(names, "acme_echo", "acme_ok") {
		t.Fatalf("before the narrowing the token lists %v (exit %d), want both tools", names, r.Code)
	}

	i.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny, "project.reveal_token": harness.OutcomeApprove})
	grantRows := len(g2Match(g2Rows(i, "control_decision", ""), map[string]string{"method": "project.grant"}))
	changeRows := len(g2Rows(i, "config_change", ""))

	edit := g2Edit(t, i, p.ID, map[string]any{"allowed_mcp_ids": []string{}})
	if edit.Code != 0 {
		t.Fatalf("a narrowing project edit exited %d, want 0\nstderr: %s", edit.Code, edit.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.update", Trace: edit.Trace, Fields: map[string]any{"status": "ok", "gated": false, "project_id": p.ID}})
	if got := i.Events(harness.EventQuery{Key: "debug.presence.answer", Trace: edit.Trace}); len(got) != 0 {
		t.Fatalf("a narrowing edit asked the presence gate %d times, want none", len(got))
	}
	if got := len(g2Match(g2Rows(i, "control_decision", ""), map[string]string{"method": "project.grant"})); got != grantRows {
		t.Fatalf("project.grant control_decision rows went from %d to %d on a narrowing", grantRows, got)
	}
	if got := len(g2Rows(i, "config_change", "")); got != changeRows {
		t.Fatalf("config_change rows went from %d to %d on a narrowing", changeRows, got)
	}

	names, r = g2Tools(t, i, token)
	if hasAll(names, "acme_echo") || hasAll(names, "acme_ok") {
		t.Fatalf("after the narrowing the token still lists %v (exit %d)", names, r.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.list", Trace: r.Trace, Fields: map[string]any{"project_id": p.ID, "count": 0}})

	// Power path: the flag verb narrows what eve may do and never prompts.
	upd := i.CLI("project", "update", "--id", p.ID, "--files-read-only=true")
	if upd.Code != 0 {
		t.Fatalf("project update --files-read-only exited %d, want 0\nstderr: %s", upd.Code, upd.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.update", Trace: upd.Trace, Fields: map[string]any{"status": "ok", "gated": false, "project_id": p.ID}})
	if !g2Get(t, i, p.ID).FilesReadOnly {
		t.Fatalf("project %s is not files_read_only after project update", p.ID)
	}
}

func TestProjectRemove(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"))
	i.WaitSessionHost(60 * time.Second)
	p := g2Create(t, i, "acme-remove", map[string]any{"allowed_templates": []string{"shell"}})

	var term struct {
		TerminalID string `json:"terminalId"`
	}
	i.MustCLI("terminal", "start", "--project", p.ID, "--template", "shell", "--json").JSON(t, &term)
	if term.TerminalID == "" {
		t.Fatalf("terminal start printed no terminalId")
	}

	r := i.MustCLI("project", "remove", "--id", p.ID, "--json")
	var removed struct {
		ID string `json:"id"`
	}
	r.JSON(t, &removed)
	if removed.ID != p.ID {
		t.Fatalf("project remove printed id %q, want %s", removed.ID, p.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.remove", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})
	if got := listProjects(t, i); len(got) != 0 {
		t.Fatalf("GET /api/projects lists %d projects after the removal, want none", len(got))
	}
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, 60*time.Second)

	unknown := i.CLI("project", "remove", "--id", "no-such-project", "--json")
	if unknown.Code != 1 {
		t.Fatalf("removing an unknown project exited %d, want 1", unknown.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.remove", Trace: unknown.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})

	if resp := i.HTTP(i.Credential("configurer")).Do("DELETE", "/api/projects/no-such-project", nil); resp.Status != 404 {
		t.Fatalf("DELETE of an unknown project answered %d, want 404", resp.Status)
	}
}

func TestProjectListAndGet(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"))
	a, _ := createProject(t, i, "acme-one")
	createProject(t, i, "acme-two")
	reader := i.HTTP(i.Credential("reader"))

	list := reader.Do("GET", "/api/projects", nil)
	if list.Status != 200 {
		t.Fatalf("GET /api/projects answered %d, want 200", list.Status)
	}
	var got []project
	list.JSON(t, &got)
	if len(got) != 2 {
		t.Fatalf("GET /api/projects lists %d projects, want 2", len(got))
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.list", Trace: list.Trace, Fields: map[string]any{"status": "ok", "count": 2}})

	one := reader.Do("GET", "/api/projects/"+a.ID, nil)
	if one.Status != 200 {
		t.Fatalf("GET /api/projects/%s answered %d, want 200", a.ID, one.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.get", Trace: one.Trace, Fields: map[string]any{"status": "ok", "project_id": a.ID}})

	if resp := i.Anonymous().Do("GET", "/api/projects", nil); resp.Status != 401 {
		t.Fatalf("GET /api/projects with no credential answered %d, want 401", resp.Status)
	}
}

func TestGrantShowsExactGrant(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"), g2Stdio(g2Scoped()))
	p := g2Create(t, i, "acme-grant", map[string]any{
		"kind":            "remote",
		"path":            nil,
		"allowed_mcp_ids": []string{g2MCP},
		"allowed_tools":   map[string][]string{g2MCP: {"acme_*"}},
		"access":          map[string]string{g2MCP: "read"},
		"allow_external":  map[string]bool{g2MCP: true},
		"context":         map[string]any{g2MCP: map[string]any{"acme_scope": []string{"alpha"}}},
	})

	r := i.MustCLI("grant", "--project", p.ID, "--json")
	var grants []struct {
		ID   string `json:"id"`
		MCPs []struct {
			MCP      string            `json:"mcp"`
			Access   string            `json:"access"`
			Outbound string            `json:"outbound"`
			Tools    string            `json:"tools"`
			Scope    map[string]string `json:"scope"`
		} `json:"mcps"`
	}
	r.JSON(t, &grants)
	if len(grants) != 1 || grants[0].ID != p.ID || len(grants[0].MCPs) != 1 {
		t.Fatalf("relay grant shows %s, want one record with one MCP", r.Stdout)
	}
	m := grants[0].MCPs[0]
	if m.MCP != g2MCP || m.Access != "read" || m.Outbound != "allowed" || m.Tools != "acme_*" {
		t.Fatalf("relay grant shows %+v, want %s read allowed acme_*", m, g2MCP)
	}
	var scope []string
	if err := json.Unmarshal([]byte(m.Scope["acme_scope"]), &scope); err != nil || len(scope) != 1 || scope[0] != "alpha" {
		t.Fatalf("relay grant scope is %v, want acme_scope [alpha]", m.Scope)
	}
	requireEvent(t, i, harness.EventQuery{Key: "grant.view", Trace: r.Trace, Fields: map[string]any{"status": "ok", "count": 1}})
}

func TestScopeFieldsAndGatedWidening(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"), g2Stdio(g2Scoped()))

	r := i.MustCLI("mcp", "scope-fields", "--id", g2MCP, "--json")
	var fields []struct {
		Name       string `json:"name"`
		Enumerable bool   `json:"enumerable"`
	}
	r.JSON(t, &fields)
	if len(fields) != 1 || fields[0].Name != "acme_scope" || !fields[0].Enumerable {
		t.Fatalf("mcp scope-fields lists %+v, want the enumerable acme_scope", fields)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.scope_fields.get", Trace: r.Trace, Fields: map[string]any{"status": "ok", "mcp_id": g2MCP}})

	resp := i.HTTP(i.Credential("reader")).Do("POST", "/api/mcps/"+g2MCP+"/enumerate", map[string]any{"field": "acme_scope"})
	if resp.Status != 200 {
		t.Fatalf("enumerate answered %d, want 200: %s", resp.Status, resp.Body)
	}
	var enum struct {
		Status string `json:"status"`
		Values []struct {
			Value string `json:"value"`
		} `json:"values"`
	}
	resp.JSON(t, &enum)
	if enum.Status != "ok" || len(enum.Values) != 2 || enum.Values[0].Value != "alpha" || enum.Values[1].Value != "beta" {
		t.Fatalf("enumerate answered %+v, want ok with alpha and beta", enum)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.scope_field.enumerate", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "mcp_id": g2MCP, "field": "acme_scope"}})

	p := g2Create(t, i, "acme-scope", map[string]any{
		"allowed_mcp_ids": []string{g2MCP},
		"context":         map[string]any{g2MCP: map[string]any{"acme_scope": []string{"alpha"}}},
	})
	i.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})
	edit := g2Edit(t, i, p.ID, map[string]any{"context": map[string]any{g2MCP: map[string]any{"acme_scope": []string{"alpha", "beta"}}}})
	if edit.Code != 1 {
		t.Fatalf("a refused scope widening exited %d, want 1", edit.Code)
	}
	g2RequireRefused(t, i, "project.update", edit.Trace, "project.grant", "cli", map[string]any{"project_id": p.ID})

	var grants []struct {
		MCPs []struct {
			Scope map[string]string `json:"scope"`
		} `json:"mcps"`
	}
	i.MustCLI("grant", "--project", p.ID, "--json").JSON(t, &grants)
	var scope []string
	if len(grants) != 1 || len(grants[0].MCPs) != 1 {
		t.Fatalf("relay grant shows %+v, want one record with one MCP", grants)
	}
	if err := json.Unmarshal([]byte(grants[0].MCPs[0].Scope["acme_scope"]), &scope); err != nil || len(scope) != 1 || scope[0] != "alpha" {
		t.Fatalf("scope is %v after a refused widening, want [alpha]", grants[0].MCPs[0].Scope)
	}
}

func TestDefaultProjectSet(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"))
	p, _ := createProject(t, i, "acme-home")
	configure := i.HTTP(i.Credential("configurer"))

	resp := configure.Do("PUT", "/api/default_project/home", map[string]any{"project_id": p.ID})
	if resp.Status != 200 {
		t.Fatalf("PUT /api/default_project/home answered %d, want 200: %s", resp.Status, resp.Body)
	}
	var defaults struct {
		Home string `json:"home"`
		Work string `json:"work"`
	}
	resp.JSON(t, &defaults)
	if defaults.Home != p.ID || defaults.Work != "" {
		t.Fatalf("the default projects are %+v, want home %s and no work", defaults, p.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.default.set", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "mode": "home", "project_id": p.ID}})
	if got := g2Get(t, i, p.ID).DefaultFor; len(got) != 1 || got[0] != "home" {
		t.Fatalf("project default_for is %v, want [home]", got)
	}

	bogus := configure.Do("PUT", "/api/default_project/bogus", map[string]any{"project_id": p.ID})
	if bogus.Status != 400 {
		t.Fatalf("PUT /api/default_project/bogus answered %d, want 400", bogus.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.default.set", Trace: bogus.Trace, Fields: map[string]any{"status": "error", "reason": "invalid"}})
}

func TestChiefOfStaffConfig(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"))
	p := g2Create(t, i, "acme-cos", map[string]any{"allowed_templates": []string{"claude-code"}})
	reader, configure := i.HTTP(i.Credential("reader")), i.HTTP(i.Credential("configurer"))

	type view struct {
		Configured bool   `json:"configured"`
		ProjectID  string `json:"projectId"`
		Model      string `json:"model"`
	}
	get := reader.Do("GET", "/api/chief-of-staff/config", nil)
	var v view
	get.JSON(t, &v)
	if get.Status != 200 || v.Configured {
		t.Fatalf("GET before any config answered %d %+v, want 200 and not configured", get.Status, v)
	}
	requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.config.get", Trace: get.Trace, Fields: map[string]any{"status": "ok"}})

	put := configure.Do("PUT", "/api/chief-of-staff/config", map[string]any{"projectId": p.ID, "model": "sonnet", "dailyModelCalls": 20})
	if put.Status != 200 {
		t.Fatalf("PUT /api/chief-of-staff/config answered %d, want 200: %s", put.Status, put.Body)
	}
	put.JSON(t, &v)
	if !v.Configured || v.ProjectID != p.ID {
		t.Fatalf("PUT answered %+v, want configured for %s", v, p.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.config.set", Trace: put.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})

	// Inside the chief-of-staff scope all three routes are closed, and nothing changes.
	scoped := i.SocketHTTP(i.Credential("scoped"))
	scope := harness.ReqOpts{Header: g2Headers("X-Relay-Scope", "chief-of-staff")}
	attempts := []struct {
		method string
		body   any
	}{
		{"GET", nil},
		{"PUT", map[string]any{"projectId": p.ID, "model": "opus", "dailyModelCalls": 5}},
		{"DELETE", nil},
	}
	for _, a := range attempts {
		if resp := scoped.Do(a.method, "/api/chief-of-staff/config", a.body, scope); resp.Status != 403 {
			t.Fatalf("%s /api/chief-of-staff/config in the scope answered %d, want 403", a.method, resp.Status)
		}
		if !g2Has(g2Rows(i, "control_decision", "denied"), map[string]string{"method": a.method, "path": "/api/chief-of-staff/config"}) {
			t.Fatalf("no denied control_decision row for %s /api/chief-of-staff/config", a.method)
		}
	}
	after := reader.Do("GET", "/api/chief-of-staff/config", nil)
	after.JSON(t, &v)
	if !v.Configured || v.Model != "sonnet" {
		t.Fatalf("the config is %+v after the refused scoped calls, want it untouched", v)
	}

	del := configure.Do("DELETE", "/api/chief-of-staff/config", nil)
	if del.Status != 200 {
		t.Fatalf("DELETE /api/chief-of-staff/config answered %d, want 200", del.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.config.clear", Trace: del.Trace, Fields: map[string]any{"status": "ok"}})
	reader.Do("GET", "/api/chief-of-staff/config", nil).JSON(t, &v)
	if v.Configured {
		t.Fatalf("the config is still set after DELETE")
	}
}

func TestProjectRotateToken(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant", "project.reveal_token", "project.rotate_token"), g2Stdio(echoCatalogue()))
	p := g2Create(t, i, "acme-rotate", map[string]any{"allowed_mcp_ids": []string{g2MCP}})
	old := g2Token(t, i, p.ID)
	if _, r := g2Tools(t, i, old); r.Code != 0 {
		t.Fatalf("the old token is refused before the rotation (exit %d)", r.Code)
	}

	r := i.MustCLI("project", "rotate-token", "--id", p.ID, "--json")
	var out struct {
		Token string `json:"token"`
	}
	r.JSON(t, &out)
	if out.Token == "" || out.Token == old {
		t.Fatalf("rotate-token printed no new token")
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.rotate_token", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})

	names, list := g2Tools(t, i, out.Token)
	if list.Code != 0 || !hasAll(names, "acme_echo", "acme_ok") {
		t.Fatalf("the new token lists %v (exit %d), want both tools", names, list.Code)
	}
	_, refused := g2Tools(t, i, old)
	if refused.Code != 1 {
		t.Fatalf("the old token listed tools after the rotation (exit %d), want 1", refused.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.list", Trace: refused.Trace, Fields: map[string]any{"status": "denied"}})
	if !g2Has(g2Rows(i, "credential_issued", "ok"), map[string]string{"credential": "project_token"}) {
		t.Fatalf("no credential_issued ok row for project_token")
	}
}

func TestProjectRotateTokenDeniedCLI(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant", "project.reveal_token"), g2Stdio(echoCatalogue()))
	p := g2Create(t, i, "acme-rotate", map[string]any{"allowed_mcp_ids": []string{g2MCP}})
	old := g2Token(t, i, p.ID)
	i.SetPresence(map[string]harness.Outcome{"project.rotate_token": harness.OutcomeDeny})
	issued := len(g2Rows(i, "credential_issued", ""))

	r := i.CLI("project", "rotate-token", "--id", p.ID, "--json")
	if r.Code != 1 || len(bytes.TrimSpace(r.Stdout)) != 0 {
		t.Fatalf("a refused rotate-token exited %d with stdout %q, want 1 and nothing", r.Code, r.Stdout)
	}
	g2RequireRefused(t, i, "project.rotate_token", r.Trace, "project.rotate_token", "cli", map[string]any{"project_id": p.ID})
	if got := len(g2Rows(i, "credential_issued", "")); got != issued {
		t.Fatalf("a refused rotation wrote a credential_issued row")
	}
	if names, list := g2Tools(t, i, old); list.Code != 0 || !hasAll(names, "acme_echo") {
		t.Fatalf("the old token lists %v (exit %d) after a refused rotation, want it to work", names, list.Code)
	}
}

func TestProjectRotateTokenDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant", "project.reveal_token"), g2Stdio(echoCatalogue()))
	p := g2Create(t, i, "acme-rotate", map[string]any{"allowed_mcp_ids": []string{g2MCP}})
	old := g2Token(t, i, p.ID)
	i.SetPresence(map[string]harness.Outcome{"project.rotate_token": harness.OutcomeDeny})
	issued := len(g2Rows(i, "credential_issued", ""))

	resp := i.HTTP(i.Credential("granter")).Do("POST", "/api/projects/"+p.ID+"/rotate_token", nil)
	if resp.Status != 403 {
		t.Fatalf("a refused rotate_token answered %d, want 403", resp.Status)
	}
	if bytes.Contains(resp.Body, []byte(old)) {
		t.Fatalf("the refusal body carries a token")
	}
	g2RequireRefused(t, i, "project.rotate_token", resp.Trace, "project.rotate_token", "http", map[string]any{"project_id": p.ID})
	if got := len(g2Rows(i, "credential_issued", "")); got != issued {
		t.Fatalf("a refused rotation wrote a credential_issued row")
	}
	if names, list := g2Tools(t, i, old); list.Code != 0 || !hasAll(names, "acme_echo") {
		t.Fatalf("the old token lists %v (exit %d) after a refused rotation, want it to work", names, list.Code)
	}
}

func TestProjectTokenReveal(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant", "project.reveal_token"), g2Stdio(echoCatalogue()))
	p := g2Create(t, i, "acme-reveal", map[string]any{"allowed_mcp_ids": []string{g2MCP}})

	token := g2Token(t, i, p.ID)
	if names, r := g2Tools(t, i, token); r.Code != 0 || !hasAll(names, "acme_echo", "acme_ok") {
		t.Fatalf("the revealed token lists %v (exit %d), want both tools", names, r.Code)
	}
	if !g2Has(g2Rows(i, "credential_disclosed", "ok"), map[string]string{"subject": p.ID}) {
		t.Fatalf("no credential_disclosed ok row with subject %s after the reveal", p.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.reveal_token", Fields: map[string]any{"status": "ok", "project_id": p.ID}})
}

func TestProjectTokenRevealFromSession(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g2Creds,
		Presence:    g2Presence("project.grant", "project.reveal_token"),
		Settings: map[string]json.RawMessage{
			"terminal_templates": json.RawMessage(`[{"id":"relaycli","name":"Relay CLI","command":` + string(g2Marshal(t, harness.BundlePaths().Relay)) + `,"sandbox":true}]`),
		},
	})
	i.WaitSessionHost(60 * time.Second)
	p := g2Create(t, i, "acme-session-reveal", map[string]any{"allowed_templates": []string{"relaycli"}})
	token := g2Token(t, i, p.ID)
	disclosed := len(g2Rows(i, "credential_disclosed", ""))
	denied := len(g2Rows(i, "control_decision", "denied"))

	body := g2Marshal(t, map[string]any{
		"projectId":  p.ID,
		"templateId": "relaycli",
		"extraArgs":  []string{"--config-dir=" + i.ConfigDir, "project", "token", "--id", p.ID},
	})
	r := i.CLIWith(harness.CLIOpts{Stdin: body}, "terminal", "start", "--file", "-", "--json")
	if r.Code != 0 {
		t.Fatalf("terminal start exited %d: %s", r.Code, r.Stderr)
	}
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	r.JSON(t, &term)
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, 60*time.Second)

	var logged struct {
		Log string `json:"log"`
	}
	i.MustCLI("terminal", "log", "--id", term.TerminalID, "--json").JSON(t, &logged)
	if strings.Contains(logged.Log, token) {
		t.Fatalf("a reveal from inside a session printed the token")
	}
	if got := len(g2Rows(i, "credential_disclosed", "")); got != disclosed {
		t.Fatalf("a reveal from inside a session wrote a credential_disclosed row")
	}
	if got := len(g2Rows(i, "control_decision", "denied")); got <= denied {
		t.Fatalf("a reveal from inside a session wrote no denied control_decision row")
	}
}

func TestProjectTokenRevealDenied(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"))
	p, _ := createProject(t, i, "acme-reveal")
	i.SetPresence(map[string]harness.Outcome{"project.reveal_token": harness.OutcomeDeny})

	r := i.CLI("project", "token", "--id", p.ID, "--json")
	if r.Code != 1 || len(bytes.TrimSpace(r.Stdout)) != 0 {
		t.Fatalf("a refused reveal exited %d with stdout %q, want 1 and nothing", r.Code, r.Stdout)
	}
	g2RequireRefused(t, i, "project.reveal_token", r.Trace, "project.reveal_token", "cli", map[string]any{"project_id": p.ID})
	if len(g2Rows(i, "credential_disclosed", "")) != 0 {
		t.Fatalf("a refused reveal wrote a credential_disclosed row")
	}
}

func TestProjectRegenSkill(t *testing.T) {
	t.Parallel()
	i := g2Start(t, g2Presence("project.grant"), g2Stdio(echoCatalogue()))
	p := g2Create(t, i, "acme-skill", map[string]any{"allowed_mcp_ids": []string{g2MCP}, "generate_skill": true})

	r := i.MustCLI("project", "regen-skill", "--id", p.ID, "--json")
	var out struct {
		Path string `json:"path"`
	}
	r.JSON(t, &out)
	if out.Path == "" {
		t.Fatalf("regen-skill printed no path: %s", r.Stdout)
	}
	// The answered path is the skills directory; each granted MCP's SKILL.md sits in a folder beneath it.
	if found, err := filepath.Glob(filepath.Join(out.Path, "*", "SKILL.md")); err != nil || len(found) == 0 {
		t.Fatalf("the path regen-skill answered holds no SKILL.md beneath it: %v", err)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.regen_skill", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})
}
