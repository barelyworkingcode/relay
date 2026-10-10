package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaye2e/harness"
)

var g3Creds = []harness.CredentialSpec{
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
	{Name: "runner", Classes: []string{"execute"}},
}

// g3GrantAndReveal approves the two gates a test needs to create a project and
// read its token.
var g3GrantAndReveal = map[string]harness.Outcome{
	"project.grant":        harness.OutcomeApprove,
	"project.reveal_token": harness.OutcomeApprove,
}

const g3Deadline = 60 * time.Second

func g3Catalogue(tools ...string) harness.Catalogue {
	var raw []json.RawMessage
	for _, name := range tools {
		raw = append(raw, json.RawMessage(`{"name":"`+name+`","description":"Echo","inputSchema":{"type":"object","properties":{}},"x-fake":{"echo":true}}`))
	}
	return harness.Catalogue{Tools: raw}
}

func g3WaitUp(t *testing.T, i *harness.Instance, id string) {
	t.Helper()
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": id, "state": "up"}}, mcpUpDeadline)
}

func g3RegisterArgs(i *harness.Instance, spec harness.FakeMCPSpec) []string {
	command, args := i.FakeMCPCommand(spec)
	out := []string{"mcp", "register", "--name", "Acme " + spec.ID, "--id", spec.ID, "--command", command}
	for _, a := range args {
		out = append(out, "--args", a)
	}
	return out
}

func g3MCPIDs(t *testing.T, i *harness.Instance) []string {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/mcps", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/mcps answered %d, want 200", resp.Status)
	}
	var rows []struct {
		ID string `json:"id"`
	}
	resp.JSON(t, &rows)
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// g3RequireRefusal asserts the shared refusal shape of an owner-gated act: the
// event ends denied with presence_refused, and a control_decision denied row
// names the gate.
func g3RequireRefusal(t *testing.T, i *harness.Instance, key, trace, gate string) {
	t.Helper()
	requireEvent(t, i, harness.EventQuery{Key: key, Trace: trace, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		if row["method"] == gate {
			return
		}
	}
	t.Fatalf("no control_decision denied audit row with method %s", gate)
}

// g3Project creates a project through the CLI with the body's grant and
// returns its id and revealed token. The instance must approve project.grant
// and project.reveal_token.
func g3Project(t *testing.T, i *harness.Instance, name string, body map[string]any) (id, token string) {
	t.Helper()
	dir := filepath.Join(i.Home, "work", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body["name"] = name
	body["path"] = dir
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the project body: %v", err)
	}
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: raw}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create for %s returned no id", name)
	}
	var tok struct {
		Token string `json:"token"`
	}
	i.MustCLI("project", "token", "--id", p.ID, "--json").JSON(t, &tok)
	if tok.Token == "" {
		t.Fatalf("project token for %s printed no token", p.ID)
	}
	return p.ID, tok.Token
}

func g3GrantBody(mcp string) map[string]any {
	return map[string]any{
		"allowed_mcp_ids": []string{mcp},
		"access":          map[string]string{mcp: "write"},
	}
}

// g3ListedTools runs `mcp call --list --schema` and returns the tool names.
func g3ListedTools(t *testing.T, i *harness.Instance, token string) ([]string, harness.Result) {
	t.Helper()
	res := i.MustCLI("mcp", "call", "--token", token, "--list", "--schema")
	var tools []struct {
		Name string `json:"name"`
	}
	res.JSON(t, &tools)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	return names, res
}

func g3CallTool(i *harness.Instance, token, tool string) harness.Result {
	return i.CLI("mcp", "call", "--token", token, "--tool", tool, "--args", "{}")
}

func g3DeniedCallRow(i *harness.Instance, tool string) bool {
	for _, row := range i.Audit(harness.AuditQuery{Event: "call_tool", Outcome: "denied"}) {
		if row["tool"] == tool {
			return true
		}
	}
	return false
}

func TestMCPRegister(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    map[string]harness.Outcome{"mcp.register": harness.OutcomeApprove},
	})
	res := i.MustCLI(g3RegisterArgs(i, harness.FakeMCPSpec{ID: "acme-reg", Catalogue: echoCatalogue()})...)
	requireEvent(t, i, harness.EventQuery{Key: "mcp.register", Trace: res.Trace, Fields: map[string]any{"status": "ok", "mcp_id": "acme-reg"}})
	g3WaitUp(t, i, "acme-reg")
	names, resp := toolNames(t, i, "acme-reg")
	if !hasAll(names, "acme_echo", "acme_ok") {
		t.Fatalf("GET tools answered %d with %v, want acme_echo and acme_ok", resp.Status, names)
	}
}

func TestMCPRegisterDeniedCLI(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    map[string]harness.Outcome{"mcp.register": harness.OutcomeDeny},
	})
	res := i.CLI(g3RegisterArgs(i, harness.FakeMCPSpec{ID: "acme-reg", Catalogue: echoCatalogue()})...)
	if res.Code != 1 {
		t.Fatalf("a refused mcp register exited %d, want 1", res.Code)
	}
	g3RequireRefusal(t, i, "mcp.register", res.Trace, "mcp.register")
	if ids := g3MCPIDs(t, i); hasAll(ids, "acme-reg") {
		t.Fatalf("GET /api/mcps lists %v after a refused register", ids)
	}
}

func TestMCPRegisterDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    map[string]harness.Outcome{"mcp.register": harness.OutcomeDeny},
	})
	command, args := i.FakeMCPCommand(harness.FakeMCPSpec{ID: "acme-reg", Catalogue: echoCatalogue()})
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/mcps", map[string]any{
		"display_name": "Acme reg", "id": "acme-reg", "transport": "stdio", "command": command, "args": args,
	})
	if resp.Status == 201 {
		t.Fatalf("a refused POST /api/mcps answered 201")
	}
	g3RequireRefusal(t, i, "mcp.register", resp.Trace, "mcp.register")
	if ids := g3MCPIDs(t, i); hasAll(ids, "acme-reg") {
		t.Fatalf("GET /api/mcps lists %v after a refused register", ids)
	}
}

func TestMCPAuthenticateDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    map[string]harness.Outcome{"mcp.oauth.start": harness.OutcomeDeny},
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-oauth", Transport: "http", OAuth: true, Catalogue: echoCatalogue()}},
	})
	res := i.StartCLI(harness.CLIOpts{Deadline: 2 * time.Minute}, "mcp", "authenticate", "--id", "acme-oauth", "--json").Wait()
	if res.Code != 1 {
		t.Fatalf("a refused mcp authenticate exited %d, want 1", res.Code)
	}
	if len(res.Stdout) != 0 {
		var line struct {
			AuthorizationURL string `json:"authorization_url"`
		}
		if json.Unmarshal(res.Stdout, &line) == nil && line.AuthorizationURL != "" {
			t.Fatalf("a refused authenticate printed an authorization_url: %s", res.Stdout)
		}
	}
	g3RequireRefusal(t, i, "mcp.oauth.start", res.Trace, "mcp.oauth.start")
	if methods(i.FakeMCPCalls("acme-oauth"))["oauth/token"] {
		t.Fatalf("the fake logged an oauth/token request after a refused authenticate")
	}
}

func TestMCPListShowsState(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		FakeMCPs: []harness.FakeMCPSpec{
			{ID: "acme-stdio", Transport: "stdio", Catalogue: echoCatalogue()},
			{ID: "acme-http", Transport: "http", Catalogue: echoCatalogue()},
		},
	})
	g3WaitUp(t, i, "acme-stdio")
	g3WaitUp(t, i, "acme-http")

	res := i.MustCLI("mcp", "list")
	requireEvent(t, i, harness.EventQuery{Key: "mcp.list", Trace: res.Trace, Fields: map[string]any{"status": "ok", "count": float64(2)}})

	var status struct {
		MCPHealth map[string]struct {
			Connected bool `json:"connected"`
		} `json:"mcp_health"`
	}
	i.MustCLI("status", "--json").JSON(t, &status)
	for _, id := range []string{"acme-stdio", "acme-http"} {
		if !status.MCPHealth[id].Connected {
			t.Fatalf("status mcp_health[%s].connected is false, want true", id)
		}
	}
}

func TestMCPToolsUnknownID(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g3Creds})
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/mcps/nope/tools", nil)
	if resp.Status != 404 {
		t.Fatalf("GET /api/mcps/nope/tools answered %d, want 404", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.tools.list", Trace: resp.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestBridgeToolsScopedToProject(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    g3GrantAndReveal,
		FakeMCPs: []harness.FakeMCPSpec{
			{ID: "acme-a", Transport: "stdio", Catalogue: g3Catalogue("acme_a_echo")},
			{ID: "acme-b", Transport: "stdio", Catalogue: g3Catalogue("acme_b_echo")},
		},
	})
	g3WaitUp(t, i, "acme-a")
	g3WaitUp(t, i, "acme-b")
	idA, tokenA := g3Project(t, i, "acme-proj-a", g3GrantBody("acme-a"))
	_, tokenB := g3Project(t, i, "acme-proj-b", g3GrantBody("acme-b"))

	names, list := g3ListedTools(t, i, tokenA)
	if len(names) != 1 || names[0] != "acme_a_echo" {
		t.Fatalf("project A lists %v, want only acme_a_echo", names)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.list", Trace: list.Trace, Fields: map[string]any{"status": "ok", "project_id": idA, "count": float64(1)}})

	ok := g3CallTool(i, tokenA, "acme_a_echo")
	if ok.Code != 0 {
		t.Fatalf("project A calling its own tool exited %d, want 0", ok.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Trace: ok.Trace, Fields: map[string]any{"status": "ok", "tool": "acme_a_echo"}})

	cross := g3CallTool(i, tokenA, "acme_b_echo")
	if cross.Code != 1 {
		t.Fatalf("project A calling B's tool exited %d, want 1", cross.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Trace: cross.Trace, Fields: map[string]any{"status": "denied"}})
	if !g3DeniedCallRow(i, "acme_b_echo") {
		t.Fatalf("no call_tool denied audit row for acme_b_echo")
	}

	reverse := g3CallTool(i, tokenB, "acme_a_echo")
	if reverse.Code != 1 {
		t.Fatalf("project B calling A's tool exited %d, want 1", reverse.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Trace: reverse.Trace, Fields: map[string]any{"status": "denied"}})
	if calls := i.FakeMCPCalls("acme-b"); len(calls) > 0 {
		for _, c := range calls {
			if c.Method == "tools/call" {
				t.Fatalf("a refused cross-project call reached the fake MCP B")
			}
		}
	}
}

func TestDisabledToolRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    g3GrantAndReveal,
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: echoCatalogue()}},
	})
	g3WaitUp(t, i, "acme-stdio")
	id, token := g3Project(t, i, "acme-proj", g3GrantBody("acme-stdio"))

	if before := g3CallTool(i, token, "acme_echo"); before.Code != 0 {
		t.Fatalf("a call before disabling exited %d, want 0", before.Code)
	}
	put := i.HTTP(i.Credential("configurer")).Do("PUT", "/api/projects/"+id, map[string]any{
		"disabled_tools": map[string][]string{"acme-stdio": {"acme_echo"}},
	})
	if put.Status != 200 {
		t.Fatalf("PUT /api/projects/%s answered %d, want 200", id, put.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.update", Trace: put.Trace, Fields: map[string]any{"status": "ok", "gated": false}})

	refused := g3CallTool(i, token, "acme_echo")
	if refused.Code != 1 {
		t.Fatalf("a call to a disabled tool exited %d, want 1", refused.Code)
	}
	if !g3DeniedCallRow(i, "acme_echo") {
		t.Fatalf("no call_tool denied audit row for the disabled tool")
	}
	if sibling := g3CallTool(i, token, "acme_ok"); sibling.Code != 0 {
		t.Fatalf("the sibling tool exited %d, want 0", sibling.Code)
	}
}

func TestMCPUnregister(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    g3GrantAndReveal,
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: echoCatalogue()}},
	})
	g3WaitUp(t, i, "acme-stdio")
	_, token := g3Project(t, i, "acme-proj", g3GrantBody("acme-stdio"))

	res := i.MustCLI("mcp", "unregister", "--id", "acme-stdio")
	requireEvent(t, i, harness.EventQuery{Key: "mcp.unregister", Trace: res.Trace, Fields: map[string]any{"status": "ok", "mcp_id": "acme-stdio"}})
	if ids := g3MCPIDs(t, i); hasAll(ids, "acme-stdio") {
		t.Fatalf("GET /api/mcps still lists %v", ids)
	}
	// relay grant shows the grant as authored, so the claim "removed from every
	// project" is read as what the project's token can still reach.
	if names, _ := g3ListedTools(t, i, token); len(names) != 0 {
		t.Fatalf("the project's token still lists %v after the MCP was unregistered", names)
	}

	unknown := i.CLI("mcp", "unregister", "--id", "nope")
	if unknown.Code != 1 {
		t.Fatalf("unregistering an unknown id exited %d, want 1", unknown.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.unregister", Trace: unknown.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

// The positive path resets real macOS privacy grants; it needs a test-machine
// pass. This test proves only the refusal.
func TestMCPResetPermissionsRefusesWithoutTCC(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: echoCatalogue()}},
	})
	g3WaitUp(t, i, "acme-stdio")
	res := i.CLI("mcp", "reset-permissions", "--id", "acme-stdio", "--json")
	if res.Code != 1 {
		t.Fatalf("reset-permissions on an MCP with no tcc_services exited %d, want 1", res.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.permissions.reset", Trace: res.Trace, Fields: map[string]any{"status": "error", "reason": "internal"}})
}

func TestReloadExternalMCPAdminOnly(t *testing.T) {
	t.Parallel()
	const secret = "acme-planted-admin-secret"
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{"admin_secret": json.RawMessage(`"` + secret + `"`)},
		FakeMCPs: []harness.FakeMCPSpec{{ID: "acme-stdio", Transport: "stdio", Catalogue: echoCatalogue()}},
	})
	g3WaitUp(t, i, "acme-stdio")

	since := time.Now()
	ok := i.BridgeSend(map[string]any{"type": "ReloadExternalMcp", "name": "acme-stdio", "token": secret})
	if ok.Type != "OK" {
		t.Fatalf("a reload with the admin secret answered %s (code %d), want OK", ok.Type, ok.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.reload", Trace: ok.Trace, Fields: map[string]any{"status": "ok", "mcp_id": "acme-stdio"}})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Since: since, Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)

	bad := i.BridgeSend(map[string]any{"type": "ReloadExternalMcp", "name": "acme-stdio", "token": "not-the-secret"})
	if bad.Type != "Error" || bad.Code != -32001 {
		t.Fatalf("a reload with a wrong token answered %s code %d, want Error -32001", bad.Type, bad.Code)
	}
	if got := i.Events(harness.EventQuery{Key: "mcp.reload", Trace: bad.Trace}); len(got) != 0 {
		t.Fatalf("a refused reload still wrote %d mcp.reload lines", len(got))
	}

	rec := i.BridgeSend(map[string]any{"type": "ReconcileExternalMcps", "token": secret})
	if rec.Type != "OK" {
		t.Fatalf("a reconcile with the admin secret answered %s (code %d), want OK", rec.Type, rec.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.reconcile", Trace: rec.Trace, Fields: map[string]any{"status": "ok"}})
}

// g3AdvanceUntil moves the server clock by step until the event is stored. A
// timer is registered only after the failure is observed, so one advance can
// land before it; the loop keeps moving the clock until the event exists.
func g3AdvanceUntil(t *testing.T, i *harness.Instance, q harness.EventQuery, step time.Duration, limit int) {
	t.Helper()
	for n := 0; n < limit; n++ {
		if len(i.Events(q)) > 0 {
			return
		}
		i.ClockAdvance(step)
	}
	if len(i.Events(q)) == 0 {
		t.Fatalf("no %s event matching %v after advancing the clock %d times by %s", q.Key, q.Fields, limit, step)
	}
}

func TestMCPRestartThenAbandon(t *testing.T) {
	t.Parallel()

	t.Run("restart", func(t *testing.T) {
		t.Parallel()
		crash := harness.Catalogue{Tools: []json.RawMessage{
			json.RawMessage(`{"name":"acme_crash","description":"Replies then exits","inputSchema":{"type":"object","properties":{}},"x-fake":{"echo":true,"exit":3}}`),
		}}
		i := harness.Start(t, harness.Options{
			Credentials: g3Creds,
			Presence:    g3GrantAndReveal,
			FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-crash", Transport: "stdio", Catalogue: crash}},
		})
		g3WaitUp(t, i, "acme-crash")
		_, token := g3Project(t, i, "acme-proj", g3GrantBody("acme-crash"))

		since := time.Now()
		if res := g3CallTool(i, token, "acme_crash"); res.Code != 0 {
			t.Fatalf("the call that makes the fake exit returned %d, want 0 (it replies first)", res.Code)
		}
		i.WaitEvent(harness.EventQuery{Key: "mcp.state", Since: since, Fields: map[string]any{"mcp_id": "acme-crash", "state": "down"}}, g3Deadline)
		g3AdvanceUntil(t, i, harness.EventQuery{Key: "mcp.state", Since: since, Fields: map[string]any{"mcp_id": "acme-crash", "state": "up"}}, 30*time.Second, 40)
		found := false
		for _, row := range i.Audit(harness.AuditQuery{Event: "mcp_down"}) {
			found = found || row["mcp_id"] == "acme-crash"
		}
		if !found {
			t.Fatalf("no mcp_down audit row for acme-crash")
		}
	})

	t.Run("abandon", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{
			Credentials: g3Creds,
			Presence:    map[string]harness.Outcome{"mcp.register": harness.OutcomeApprove, "project.grant": harness.OutcomeApprove, "project.reveal_token": harness.OutcomeApprove},
		})
		crash := harness.Catalogue{Tools: []json.RawMessage{
			json.RawMessage(`{"name":"acme_crash","description":"Replies then exits","inputSchema":{"type":"object","properties":{}},"x-fake":{"echo":true,"exit":3}}`),
		}}
		// The wrapper runs the fake until the flag file exists, then fails every
		// spawn, so the first start works and every restart does not.
		flag := filepath.Join(i.Dir, "fail-spawns")
		command, args := i.FakeMCPCommand(harness.FakeMCPSpec{ID: "acme-broken", Catalogue: crash})
		reg := []string{"mcp", "register", "--name", "Acme broken", "--id", "acme-broken", "--command", "/bin/sh",
			"--args", "-c", "--args", `if [ -e "` + flag + `" ]; then exit 1; fi; exec "$0" "$@"`, "--args", command}
		for _, a := range args {
			reg = append(reg, "--args", a)
		}
		i.MustCLI(reg...)
		g3WaitUp(t, i, "acme-broken")
		_, token := g3Project(t, i, "acme-proj", g3GrantBody("acme-broken"))
		if err := os.WriteFile(flag, nil, 0o600); err != nil {
			t.Fatalf("writing %s: %v", flag, err)
		}
		g3CallTool(i, token, "acme_crash")

		g3AdvanceUntil(t, i, harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-broken", "state": "abandoned"}}, 60*time.Second, 60)
		var status struct {
			MCPHealth map[string]struct {
				State string `json:"state"`
			} `json:"mcp_health"`
		}
		i.MustCLI("status", "--json").JSON(t, &status)
		if got := status.MCPHealth["acme-broken"].State; got != "abandoned" {
			t.Fatalf("status mcp_health[acme-broken].state is %q, want abandoned", got)
		}
	})
}
