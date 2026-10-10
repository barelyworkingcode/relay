package features

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

const g2Deadline = 60 * time.Second

var g2Creds = []harness.CredentialSpec{
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
	{Name: "granter", Classes: []string{"grant"}},
	{Name: "cos", Classes: []string{"proxy"}},
}

var g2ApproveAll = map[string]harness.Outcome{
	"project.grant":        harness.OutcomeApprove,
	"project.reveal_token": harness.OutcomeApprove,
	"project.rotate_token": harness.OutcomeApprove,
}

type g2Project struct {
	ID            string                         `json:"id"`
	Name          string                         `json:"name"`
	Kind          string                         `json:"kind"`
	AllowedMCPIDs []string                       `json:"allowed_mcp_ids"`
	DefaultFor    []string                       `json:"default_for"`
	FilesReadOnly bool                           `json:"files_read_only"`
	Context       map[string]map[string][]string `json:"context"`
}

// g2Create creates a project through `relay project create --file -`. The
// instance must approve project.grant.
func g2Create(t *testing.T, i *harness.Instance, name string, extra map[string]any) g2Project {
	t.Helper()
	dir := filepath.Join(i.Home, "work", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body := map[string]any{"name": name, "path": dir}
	for k, v := range extra {
		body[k] = v
	}
	enc, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the project body: %v", err)
	}
	r := i.CLIWith(harness.CLIOpts{Stdin: enc}, "project", "create", "--file", "-", "--json")
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

func g2Get(t *testing.T, i *harness.Instance, id string) g2Project {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects/"+id, nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/projects/%s answered %d", id, resp.Status)
	}
	var p g2Project
	resp.JSON(t, &p)
	return p
}

func g2List(t *testing.T, i *harness.Instance) []g2Project {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/projects answered %d", resp.Status)
	}
	var ps []g2Project
	resp.JSON(t, &ps)
	return ps
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

// g2ToolNames lists the tools a project token reaches. It reads the names
// from `--list --schema` and leaves them nil when the output is not the JSON
// array, which is what an empty list prints.
func g2ToolNames(i *harness.Instance, token string) ([]string, harness.Result) {
	r := i.CLI("mcp", "call", "--token", token, "--list", "--schema")
	var rows []struct {
		Name string `json:"name"`
	}
	var names []string
	if r.Code == 0 && json.Unmarshal(r.Stdout, &rows) == nil {
		for _, row := range rows {
			names = append(names, row.Name)
		}
	}
	return names, r
}

func g2Equal(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func g2Rows(i *harness.Instance, event, outcome string, match map[string]any) []map[string]any {
	var out []map[string]any
	for _, row := range i.Audit(harness.AuditQuery{Event: event, Outcome: outcome}) {
		ok := true
		for k, v := range match {
			if !g2Equal(row[k], v) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, row)
		}
	}
	return out
}

// g2RequireRefusal asserts the refusal set of a gated door: the event ends
// denied for presence_refused, and a denied control_decision row names the
// gate and the door that asked.
func g2RequireRefusal(t *testing.T, i *harness.Instance, key, trace, gate, via string) {
	t.Helper()
	requireEvent(t, i, harness.EventQuery{Key: key, Trace: trace, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	if rows := g2Rows(i, "control_decision", "denied", map[string]any{"method": gate, "via": via}); len(rows) == 0 {
		t.Fatalf("no denied control_decision row with method %s via %s", gate, via)
	}
}

func g2Body(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding a body: %v", err)
	}
	return b
}

func g2WaitMCPUp(i *harness.Instance, id string) {
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": id, "state": "up"}}, mcpUpDeadline)
}

// g2ScopedCatalogue is a fake MCP whose initialize answer declares one
// operator-supplied scope field that it can enumerate.
func g2ScopedCatalogue() harness.Catalogue {
	return harness.Catalogue{
		Initialize: json.RawMessage(`{"serverInfo":{"name":"acme-fake","version":"1","contextSchemaVersion":2,"contextSchema":{"acme_folders":{"type":"array","items":{"type":"string"},"description":"Folders the client may reach","scope":"restrict","source":"operator","applies_to":["acme_*"],"enumerable":true}}}}`),
		Tools:      echoCatalogue().Tools,
		Enumerate: map[string][]harness.EnumValue{
			"acme_folders": {{Value: "Alpha", Label: "Alpha"}, {Value: "Beta", Label: "Beta"}},
		},
	}
}

func TestProjectCreateNarrowDefault(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: approveGrant})
	dir := filepath.Join(i.Home, "work", "acme-narrow")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	r := i.MustCLI("project", "create", "--name", "Acme Narrow", "--path", dir, "--json")
	var p g2Project
	r.JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id: %s", r.Stdout)
	}
	if len(p.AllowedMCPIDs) != 0 {
		t.Fatalf("a project created with --name and --path allows MCPs %v, want none", p.AllowedMCPIDs)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.create", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})
	if got := g2Get(t, i, p.ID); len(got.AllowedMCPIDs) != 0 {
		t.Fatalf("the stored project allows MCPs %v, want none", got.AllowedMCPIDs)
	}
	if rows := g2Rows(i, "config_change", "ok", nil); len(rows) != 1 {
		t.Fatalf("got %d config_change ok rows, want 1", len(rows))
	}
	rows := g2Rows(i, "control_decision", "ok", map[string]any{"method": "project.grant"})
	if len(rows) != 1 {
		t.Fatalf("got %d control_decision ok rows for project.grant, want 1", len(rows))
	}
	if rows[0]["presence_approver"] != "testapprover" {
		t.Fatalf("the approval row has presence_approver %v, want testapprover", rows[0]["presence_approver"])
	}
}

func TestProjectCreateDeniedCLI(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g2Creds,
		Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeDeny},
	})
	dir := filepath.Join(i.Home, "work", "acme-denied")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	r := i.CLI("project", "create", "--name", "Acme Denied", "--path", dir, "--json")
	if r.Code != 1 {
		t.Fatalf("project create exited %d, want 1", r.Code)
	}
	if len(r.Stdout) != 0 {
		t.Fatalf("a refused create printed %q on stdout", r.Stdout)
	}
	g2RequireRefusal(t, i, "project.create", r.Trace, "project.grant", "cli")
	if got := g2List(t, i); len(got) != 0 {
		t.Fatalf("a refused create left %d projects", len(got))
	}
	if rows := g2Rows(i, "config_change", "", nil); len(rows) != 0 {
		t.Fatalf("a refused create wrote %d config_change rows", len(rows))
	}
}

func TestProjectCreateDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g2Creds,
		Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeDeny},
	})
	dir := filepath.Join(i.Home, "work", "acme-denied")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	resp := i.HTTP(i.Credential("configurer")).Do("POST", "/api/projects", map[string]any{"name": "Acme Denied", "path": dir})
	if resp.Status != http.StatusForbidden {
		t.Fatalf("POST /api/projects answered %d, want 403", resp.Status)
	}
	g2RequireRefusal(t, i, "project.create", resp.Trace, "project.grant", "http")
	if got := g2List(t, i); len(got) != 0 {
		t.Fatalf("a refused create left %d projects", len(got))
	}
}

func TestProjectWidenApproved(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: approveGrant, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-widen", nil)
	before := len(g2Rows(i, "config_change", "ok", nil))

	resp := i.HTTP(i.Credential("configurer")).Do("PUT", "/api/projects/"+p.ID, map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})
	if resp.Status != 200 {
		t.Fatalf("PUT /api/projects/%s answered %d: %s", p.ID, resp.Status, resp.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.update", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID, "gated": true}})
	if after := len(g2Rows(i, "config_change", "ok", nil)); after != before+1 {
		t.Fatalf("config_change ok rows went from %d to %d, want one more", before, after)
	}

	var grants []struct {
		ID   string `json:"id"`
		MCPs []struct {
			MCP string `json:"mcp"`
		} `json:"mcps"`
	}
	i.MustCLI("grant", "--json").JSON(t, &grants)
	found := false
	for _, g := range grants {
		if g.ID != p.ID {
			continue
		}
		for _, m := range g.MCPs {
			found = found || m.MCP == "acme-stdio"
		}
	}
	if !found {
		t.Fatalf("relay grant does not show acme-stdio for the widened project: %+v", grants)
	}
}

func TestProjectWidenDeniedCLI(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: approveGrant, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-widen-denied", nil)
	i.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})

	r := i.CLIWith(harness.CLIOpts{Stdin: g2Body(t, map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})},
		"project", "edit", "--id", p.ID, "--file", "-", "--json")
	if r.Code != 1 {
		t.Fatalf("project edit exited %d, want 1", r.Code)
	}
	g2RequireRefusal(t, i, "project.update", r.Trace, "project.grant", "cli")
	if got := g2Get(t, i, p.ID); len(got.AllowedMCPIDs) != 0 {
		t.Fatalf("a refused edit left allowed_mcp_ids %v", got.AllowedMCPIDs)
	}
	var grants []struct {
		ID   string            `json:"id"`
		MCPs []json.RawMessage `json:"mcps"`
	}
	i.MustCLI("grant", "--json").JSON(t, &grants)
	for _, g := range grants {
		if g.ID == p.ID && len(g.MCPs) != 0 {
			t.Fatalf("relay grant shows %d MCPs after a refused edit", len(g.MCPs))
		}
	}
}

func TestProjectWidenDeniedHTTP(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body map[string]any
		same func(t *testing.T, before, after g2Project)
	}{
		{
			name: "add_mcp",
			body: map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}},
			same: func(t *testing.T, _, after g2Project) {
				if len(after.AllowedMCPIDs) != 0 {
					t.Fatalf("a refused widening left allowed_mcp_ids %v", after.AllowedMCPIDs)
				}
			},
		},
		{
			name: "remote_conversion",
			body: map[string]any{"kind": "remote"},
			same: func(t *testing.T, before, after g2Project) {
				if after.Kind == "remote" || after.Kind != before.Kind {
					t.Fatalf("a refused conversion changed kind from %q to %q", before.Kind, after.Kind)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: approveGrant, FakeMCPs: acmeStdioMCP()})
			g2WaitMCPUp(i, "acme-stdio")
			p := g2Create(t, i, "acme-widen-http", nil)
			i.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})

			resp := i.HTTP(i.Credential("configurer")).Do("PUT", "/api/projects/"+p.ID, tc.body)
			if resp.Status != http.StatusForbidden {
				t.Fatalf("PUT /api/projects/%s answered %d, want 403: %s", p.ID, resp.Status, resp.Body)
			}
			g2RequireRefusal(t, i, "project.update", resp.Trace, "project.grant", "http")
			tc.same(t, p, g2Get(t, i, p.ID))
		})
	}
}

func TestProjectNarrowUngated(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: g2ApproveAll, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-narrow-edit", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})
	token := g2Token(t, i, p.ID)
	if names, r := g2ToolNames(i, token); !hasAll(names, "acme_echo", "acme_ok") {
		t.Fatalf("the granted token lists %v (exit %d), want acme_echo and acme_ok", names, r.Code)
	}

	i.SetPresence(map[string]harness.Outcome{
		"project.grant":        harness.OutcomeDeny,
		"project.reveal_token": harness.OutcomeApprove,
	})
	grantRows := len(g2Rows(i, "control_decision", "", map[string]any{"method": "project.grant"}))
	changes := len(g2Rows(i, "config_change", "", nil))

	r := i.CLIWith(harness.CLIOpts{Stdin: g2Body(t, map[string]any{"allowed_mcp_ids": []string{}})},
		"project", "edit", "--id", p.ID, "--file", "-", "--json")
	if r.Code != 0 {
		t.Fatalf("a narrowing project edit exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.update", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID, "gated": false}})
	if got := len(g2Rows(i, "control_decision", "", map[string]any{"method": "project.grant"})); got != grantRows {
		t.Fatalf("a narrowing edit added %d control_decision rows for project.grant", got-grantRows)
	}
	if got := len(g2Rows(i, "config_change", "", nil)); got != changes {
		t.Fatalf("a narrowing edit added %d config_change rows", got-changes)
	}

	names, listed := g2ToolNames(i, token)
	if listed.Code != 0 || len(names) != 0 || strings.Contains(string(listed.Stdout), "acme_") {
		t.Fatalf("after narrowing the token lists %v (exit %d): %s", names, listed.Code, listed.Stdout)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.list", Trace: listed.Trace, Fields: map[string]any{"status": "ok", "count": 0}})

	power := i.CLI("project", "update", "--id", p.ID, "--files-read-only=true")
	if power.Code != 0 {
		t.Fatalf("project update exited %d\nstderr: %s", power.Code, power.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.update", Trace: power.Trace, Fields: map[string]any{"status": "ok", "gated": false}})
	if !g2Get(t, i, p.ID).FilesReadOnly {
		t.Fatalf("project update --files-read-only=true left files_read_only unset")
	}
}

func TestProjectRemove(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: approveGrant})
	i.WaitSessionHost(g2Deadline)
	p := g2Create(t, i, "acme-remove", map[string]any{"allowed_templates": []string{"shell"}})

	var term struct {
		TerminalID string `json:"terminalId"`
	}
	i.MustCLI("terminal", "start", "--project", p.ID, "--template", "shell", "--json").JSON(t, &term)
	if term.TerminalID == "" {
		t.Fatalf("terminal start printed no terminalId")
	}
	var live struct {
		Terminals []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"terminals"`
	}
	i.MustCLI("terminal", "list", "--json").JSON(t, &live)
	running := false
	for _, x := range live.Terminals {
		running = running || (x.ID == term.TerminalID && x.State != "stopped")
	}
	if !running {
		t.Fatalf("terminal %s is not live before the removal: %+v", term.TerminalID, live.Terminals)
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
	if got := g2List(t, i); len(got) != 0 {
		t.Fatalf("%d projects remain after the removal", len(got))
	}
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, g2Deadline)

	missing := i.CLI("project", "remove", "--id", "no-such-project", "--json")
	if missing.Code != 1 {
		t.Fatalf("removing an unknown project exited %d, want 1", missing.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.remove", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestProjectListAndGet(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: approveGrant})
	p := g2Create(t, i, "acme-list", nil)
	reader := i.HTTP(i.Credential("reader"))

	list := reader.Do("GET", "/api/projects", nil)
	if list.Status != 200 {
		t.Fatalf("GET /api/projects answered %d", list.Status)
	}
	var all []g2Project
	list.JSON(t, &all)
	if len(all) != 1 || all[0].ID != p.ID {
		t.Fatalf("the list is %+v, want the one project %s", all, p.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.list", Trace: list.Trace, Fields: map[string]any{"status": "ok", "count": 1}})

	get := reader.Do("GET", "/api/projects/"+p.ID, nil)
	if get.Status != 200 {
		t.Fatalf("GET /api/projects/%s answered %d", p.ID, get.Status)
	}
	var one g2Project
	get.JSON(t, &one)
	if one.ID != p.ID {
		t.Fatalf("GET returned project %q, want %s", one.ID, p.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.get", Trace: get.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})

	if resp := i.Anonymous().Do("GET", "/api/projects", nil); resp.Status != http.StatusUnauthorized {
		t.Fatalf("GET /api/projects with no credential answered %d, want 401", resp.Status)
	}
}

func TestGrantShowsExactGrant(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g2Creds,
		Presence:    approveGrant,
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: g2ScopedCatalogue()}},
	})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-exact", map[string]any{
		"allowed_mcp_ids": []string{"acme-stdio"},
		"allowed_tools":   map[string][]string{"acme-stdio": {"acme_*"}},
		"access":          map[string]string{"acme-stdio": "read"},
		"context":         map[string]any{"acme-stdio": map[string][]string{"acme_folders": {"Alpha", "Beta"}}},
	})

	r := i.MustCLI("grant", "--json")
	var grants []struct {
		ID   string `json:"id"`
		MCPs []struct {
			MCP    string            `json:"mcp"`
			Access string            `json:"access"`
			Tools  string            `json:"tools"`
			Scope  map[string]string `json:"scope"`
		} `json:"mcps"`
	}
	r.JSON(t, &grants)
	if len(grants) != 1 || grants[0].ID != p.ID || len(grants[0].MCPs) != 1 {
		t.Fatalf("relay grant lists %+v, want the one project with one MCP", grants)
	}
	m := grants[0].MCPs[0]
	if m.MCP != "acme-stdio" || m.Access != "read" || m.Tools != "acme_*" {
		t.Fatalf("the grant shows mcp %q access %q tools %q, want acme-stdio, read, acme_*", m.MCP, m.Access, m.Tools)
	}
	var folders []string
	if err := json.Unmarshal([]byte(m.Scope["acme_folders"]), &folders); err != nil || len(folders) != 2 || folders[0] != "Alpha" || folders[1] != "Beta" {
		t.Fatalf("the grant shows scope %v, want acme_folders [Alpha Beta]", m.Scope)
	}
	requireEvent(t, i, harness.EventQuery{Key: "grant.view", Trace: r.Trace, Fields: map[string]any{"status": "ok", "count": 1}})
}

func TestScopeFieldsAndGatedWidening(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g2Creds,
		Presence:    approveGrant,
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: g2ScopedCatalogue()}},
	})
	g2WaitMCPUp(i, "acme-stdio")

	fields := i.MustCLI("mcp", "scope-fields", "--id", "acme-stdio", "--json")
	var listed []struct {
		Name       string `json:"name"`
		Source     string `json:"source"`
		Enumerable bool   `json:"enumerable"`
	}
	fields.JSON(t, &listed)
	if len(listed) != 1 || listed[0].Name != "acme_folders" || listed[0].Source != "operator" || !listed[0].Enumerable {
		t.Fatalf("scope-fields lists %+v, want the one enumerable operator field acme_folders", listed)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.scope_fields.get", Trace: fields.Trace, Fields: map[string]any{"status": "ok", "mcp_id": "acme-stdio"}})

	enum := i.HTTP(i.Credential("reader")).Do("POST", "/api/mcps/acme-stdio/enumerate", map[string]any{"field": "acme_folders"})
	if enum.Status != 200 {
		t.Fatalf("enumerate answered %d: %s", enum.Status, enum.Body)
	}
	var answer struct {
		Status string `json:"status"`
		Values []struct {
			Value string `json:"value"`
		} `json:"values"`
	}
	enum.JSON(t, &answer)
	if answer.Status != "ok" || len(answer.Values) != 2 || answer.Values[0].Value != "Alpha" || answer.Values[1].Value != "Beta" {
		t.Fatalf("enumerate answered %+v, want ok with Alpha and Beta", answer)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.scope_field.enumerate", Trace: enum.Trace, Fields: map[string]any{"status": "ok", "mcp_id": "acme-stdio", "field": "acme_folders"}})

	p := g2Create(t, i, "acme-scoped", map[string]any{
		"allowed_mcp_ids": []string{"acme-stdio"},
		"context":         map[string]any{"acme-stdio": map[string][]string{"acme_folders": {"Alpha"}}},
	})
	i.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeDeny})
	resp := i.HTTP(i.Credential("configurer")).Do("PUT", "/api/projects/"+p.ID, map[string]any{
		"context": map[string]any{"acme-stdio": map[string][]string{"acme_folders": {"Alpha", "Beta"}}},
	})
	if resp.Status != http.StatusForbidden {
		t.Fatalf("a save adding a scope value answered %d, want 403: %s", resp.Status, resp.Body)
	}
	g2RequireRefusal(t, i, "project.update", resp.Trace, "project.grant", "http")
	got := g2Get(t, i, p.ID).Context["acme-stdio"]["acme_folders"]
	if len(got) != 1 || got[0] != "Alpha" {
		t.Fatalf("a refused save left the scope %v, want [Alpha]", got)
	}
}

func TestDefaultProjectSet(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: approveGrant})
	p := g2Create(t, i, "acme-default", nil)
	cfg := i.HTTP(i.Credential("configurer"))

	set := cfg.Do("PUT", "/api/default_project/home", map[string]string{"project_id": p.ID})
	if set.Status != 200 {
		t.Fatalf("PUT /api/default_project/home answered %d: %s", set.Status, set.Body)
	}
	var defaults map[string]string
	set.JSON(t, &defaults)
	if defaults["home"] != p.ID || defaults["work"] != "" {
		t.Fatalf("the answer is %v, want home %s and no work default", defaults, p.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.default.set", Trace: set.Trace, Fields: map[string]any{"status": "ok", "mode": "home", "project_id": p.ID}})
	found := false
	for _, d := range g2Get(t, i, p.ID).DefaultFor {
		found = found || d == "home"
	}
	if !found {
		t.Fatalf("the project does not list home in default_for")
	}

	bad := cfg.Do("PUT", "/api/default_project/bogus", map[string]string{"project_id": p.ID})
	if bad.Status != http.StatusBadRequest {
		t.Fatalf("PUT /api/default_project/bogus answered %d, want 400", bad.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.default.set", Trace: bad.Trace, Fields: map[string]any{"status": "error", "reason": "invalid"}})
}

func TestChiefOfStaffConfig(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: approveGrant})
	p := g2Create(t, i, "acme-cos", map[string]any{"allowed_templates": []string{"claude-code"}})
	reader, cfg := i.HTTP(i.Credential("reader")), i.HTTP(i.Credential("configurer"))

	type view struct {
		Configured bool   `json:"configured"`
		ProjectID  string `json:"projectId"`
	}
	read := func() (view, harness.Response) {
		resp := reader.Do("GET", "/api/chief-of-staff/config", nil)
		if resp.Status != 200 {
			t.Fatalf("GET /api/chief-of-staff/config answered %d", resp.Status)
		}
		var v view
		resp.JSON(t, &v)
		return v, resp
	}
	v, resp := read()
	if v.Configured {
		t.Fatalf("a fresh instance reports a configured Chief of Staff")
	}
	requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.config.get", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})

	set := cfg.Do("PUT", "/api/chief-of-staff/config", map[string]any{"projectId": p.ID, "model": "haiku", "dailyModelCalls": 50})
	if set.Status != 200 {
		t.Fatalf("PUT /api/chief-of-staff/config answered %d: %s", set.Status, set.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.config.set", Trace: set.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})
	if v, _ := read(); !v.Configured || v.ProjectID != p.ID {
		t.Fatalf("after the PUT the config reads %+v, want configured for %s", v, p.ID)
	}

	del := cfg.Do("DELETE", "/api/chief-of-staff/config", nil)
	if del.Status != 200 {
		t.Fatalf("DELETE /api/chief-of-staff/config answered %d: %s", del.Status, del.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "chief_of_staff.config.clear", Trace: del.Trace, Fields: map[string]any{"status": "ok"}})
	if v, _ := read(); v.Configured {
		t.Fatalf("after the DELETE the config still reads configured")
	}

	scoped := harness.ReqOpts{Header: http.Header{"X-Relay-Scope": {"chief-of-staff"}}}
	cos := i.SocketHTTP(i.Credential("cos"))
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		var body any
		if method == "PUT" {
			body = map[string]any{"projectId": p.ID, "model": "haiku", "dailyModelCalls": 50}
		}
		if resp := cos.Do(method, "/api/chief-of-staff/config", body, scoped); resp.Status != http.StatusForbidden {
			t.Fatalf("%s /api/chief-of-staff/config in the chief-of-staff scope answered %d, want 403", method, resp.Status)
		}
		if rows := g2Rows(i, "control_decision", "denied", map[string]any{"method": method, "path": "/api/chief-of-staff/config"}); len(rows) != 1 {
			t.Fatalf("got %d denied control_decision rows for %s, want 1", len(rows), method)
		}
	}
	if v, _ := read(); v.Configured {
		t.Fatalf("a refused scoped request changed the config")
	}
}

func TestProjectRotateToken(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: g2ApproveAll, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-rotate", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})
	old := g2Token(t, i, p.ID)
	if names, _ := g2ToolNames(i, old); !hasAll(names, "acme_echo") {
		t.Fatalf("the original token lists %v, want acme_echo", names)
	}

	r := i.MustCLI("project", "rotate-token", "--id", p.ID, "--json")
	var out struct {
		Token string `json:"token"`
	}
	r.JSON(t, &out)
	if out.Token == "" || out.Token == old {
		t.Fatalf("rotate-token printed token %q, want a new one", out.Token)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.rotate_token", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})

	if names, res := g2ToolNames(i, out.Token); !hasAll(names, "acme_echo") {
		t.Fatalf("the new token lists %v (exit %d), want acme_echo", names, res.Code)
	}
	refused := i.CLI("mcp", "call", "--token", old, "--list")
	if refused.Code != 1 {
		t.Fatalf("the old token listed tools with exit %d, want 1", refused.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.list", Trace: refused.Trace, Fields: map[string]any{"status": "denied"}})

	issued := g2Rows(i, "credential_issued", "ok", map[string]any{"credential": "project_token"})
	if len(issued) != 1 {
		t.Fatalf("got %d credential_issued rows for a project token, want 1", len(issued))
	}
	if strings.Contains(string(g2Body(t, issued)), out.Token) {
		t.Fatalf("the credential_issued row carries the new token")
	}
}

func TestProjectRotateTokenDeniedCLI(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: g2ApproveAll, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-rotate-denied", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})
	old := g2Token(t, i, p.ID)
	i.SetPresence(map[string]harness.Outcome{"project.rotate_token": harness.OutcomeDeny, "project.reveal_token": harness.OutcomeApprove})

	r := i.CLI("project", "rotate-token", "--id", p.ID, "--json")
	if r.Code != 1 {
		t.Fatalf("rotate-token exited %d, want 1", r.Code)
	}
	if len(r.Stdout) != 0 {
		t.Fatalf("a refused rotation printed %q", r.Stdout)
	}
	g2RequireRefusal(t, i, "project.rotate_token", r.Trace, "project.rotate_token", "cli")
	if names, res := g2ToolNames(i, old); !hasAll(names, "acme_echo") {
		t.Fatalf("after a refused rotation the old token lists %v (exit %d)", names, res.Code)
	}
}

func TestProjectRotateTokenDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: g2ApproveAll, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-rotate-denied-http", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})
	old := g2Token(t, i, p.ID)
	i.SetPresence(map[string]harness.Outcome{"project.rotate_token": harness.OutcomeDeny, "project.reveal_token": harness.OutcomeApprove})

	resp := i.HTTP(i.Credential("granter")).Do("POST", "/api/projects/"+p.ID+"/rotate_token", nil)
	if resp.Status != http.StatusForbidden {
		t.Fatalf("POST rotate_token answered %d, want 403: %s", resp.Status, resp.Body)
	}
	if strings.Contains(string(resp.Body), "token\"") {
		t.Fatalf("a refused rotation answered a token field: %s", resp.Body)
	}
	g2RequireRefusal(t, i, "project.rotate_token", resp.Trace, "project.rotate_token", "http")
	if names, res := g2ToolNames(i, old); !hasAll(names, "acme_echo") {
		t.Fatalf("after a refused rotation the old token lists %v (exit %d)", names, res.Code)
	}
}

func TestProjectTokenReveal(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: g2ApproveAll, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-reveal", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})

	r := i.MustCLI("project", "token", "--id", p.ID, "--json")
	var out struct {
		Token string `json:"token"`
	}
	r.JSON(t, &out)
	if out.Token == "" {
		t.Fatalf("project token printed no token")
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.reveal_token", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})
	if names, res := g2ToolNames(i, out.Token); !hasAll(names, "acme_echo", "acme_ok") {
		t.Fatalf("the revealed token lists %v (exit %d), want the project's tools", names, res.Code)
	}
	disclosed := g2Rows(i, "credential_disclosed", "ok", nil)
	if len(disclosed) != 1 {
		t.Fatalf("got %d credential_disclosed rows, want 1", len(disclosed))
	}
	if strings.Contains(string(g2Body(t, disclosed)), out.Token) {
		t.Fatalf("the credential_disclosed row carries the token")
	}
}

func TestProjectTokenRevealDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g2Creds,
		Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeApprove, "project.reveal_token": harness.OutcomeDeny},
	})
	p := g2Create(t, i, "acme-reveal-denied", nil)

	r := i.CLI("project", "token", "--id", p.ID, "--json")
	if r.Code != 1 {
		t.Fatalf("project token exited %d, want 1", r.Code)
	}
	if len(r.Stdout) != 0 {
		t.Fatalf("a refused reveal printed %q on stdout", r.Stdout)
	}
	g2RequireRefusal(t, i, "project.reveal_token", r.Trace, "project.reveal_token", "cli")
	if rows := g2Rows(i, "credential_disclosed", "", nil); len(rows) != 0 {
		t.Fatalf("a refused reveal wrote %d credential_disclosed rows", len(rows))
	}
}

func TestProjectRegenSkill(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g2Creds, Presence: approveGrant, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-skill", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})

	// Clear any skills the create wrote, so the file the verb reports is its own.
	var first struct {
		Path string `json:"path"`
	}
	i.MustCLI("project", "regen-skill", "--id", p.ID, "--json").JSON(t, &first)
	if first.Path == "" {
		t.Fatalf("regen-skill printed no path")
	}
	if err := os.RemoveAll(first.Path); err != nil {
		t.Fatalf("clearing %s: %v", first.Path, err)
	}

	r := i.MustCLI("project", "regen-skill", "--id", p.ID, "--json")
	var out struct {
		Path string `json:"path"`
	}
	r.JSON(t, &out)
	if filepath.Base(out.Path) != "skills" {
		t.Fatalf("regen-skill path %q is not a skills directory", out.Path)
	}
	files, err := filepath.Glob(filepath.Join(out.Path, "relay-*", "SKILL.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no relay-*/SKILL.md under %s after regen-skill (glob error %v)", out.Path, err)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.regen_skill", Trace: r.Trace, Fields: map[string]any{"status": "ok", "project_id": p.ID}})
}

func TestProjectTokenRevealFromSession(t *testing.T) {
	t.Parallel()
	tmpl := `[{"id":"acme-relay","name":"Acme relay","command":` + g2Quote(harness.BundlePaths().Relay) + `,"sandbox":true}]`
	i := harness.Start(t, harness.Options{
		Credentials: g2Creds,
		Presence:    g2ApproveAll,
		Settings:    map[string]json.RawMessage{"terminal_templates": json.RawMessage(tmpl)},
	})
	i.WaitSessionHost(g2Deadline)
	p := g2Create(t, i, "acme-session-reveal", map[string]any{"allowed_templates": []string{"acme-relay"}})
	// The operator's own reveal is approved, so only the caller can explain a refusal below.
	token := g2Token(t, i, p.ID)
	disclosed := len(g2Rows(i, "credential_disclosed", "", nil))
	denied := len(g2Rows(i, "control_decision", "denied", nil))

	body := g2Body(t, map[string]any{
		"projectId": p.ID, "templateId": "acme-relay",
		"extraArgs": []string{"--config-dir=" + i.ConfigDir, "project", "token", "--id", p.ID},
	})
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	i.CLIWith(harness.CLIOpts{Stdin: body}, "terminal", "start", "--file", "-", "--json").JSON(t, &term)
	if term.TerminalID == "" {
		t.Fatalf("terminal start printed no terminalId")
	}
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, g2Deadline)

	var logOut struct {
		Log string `json:"log"`
	}
	i.MustCLI("terminal", "log", "--id", term.TerminalID, "--json").JSON(t, &logOut)
	if strings.Contains(logOut.Log, token) {
		t.Fatalf("the session's terminal log holds the project token")
	}
	if got := len(g2Rows(i, "credential_disclosed", "", nil)); got != disclosed {
		t.Fatalf("a reveal from a session added %d credential_disclosed rows", got-disclosed)
	}
	if got := len(g2Rows(i, "control_decision", "denied", nil)); got <= denied {
		t.Fatalf("a reveal from a session wrote no denied control_decision row")
	}
}

func g2Quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
