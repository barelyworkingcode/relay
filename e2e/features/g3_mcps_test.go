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

var g3Creds = []harness.CredentialSpec{
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
	{Name: "runner", Classes: []string{"execute"}},
}

var g3ApproveAll = map[string]harness.Outcome{
	"project.grant":        harness.OutcomeApprove,
	"project.reveal_token": harness.OutcomeApprove,
	"mcp.register":         harness.OutcomeApprove,
}

func g3Catalogue(tools ...string) harness.Catalogue {
	var c harness.Catalogue
	for _, name := range tools {
		c.Tools = append(c.Tools, json.RawMessage(`{"name":"`+name+`","description":"Say ok","inputSchema":{"type":"object","properties":{}}}`))
	}
	return c
}

func g3Stdio(id string, c harness.Catalogue) harness.FakeMCPSpec {
	return harness.FakeMCPSpec{ID: id, Transport: "stdio", Catalogue: c}
}

func g3ListedMCPs(t *testing.T, i *harness.Instance) []string {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/mcps", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/mcps answered %d", resp.Status)
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

func g3Call(i *harness.Instance, token, tool string) harness.Result {
	return i.CLI("mcp", "call", "--token", token, "--tool", tool)
}

// g3Advance moves the server clock by step until an event matching q is
// stored. The restart timer is registered after the "down" event, and nothing
// signals that, so the clock moves again until the timer has fired.
func g3Advance(t *testing.T, i *harness.Instance, step time.Duration, limit int, q harness.EventQuery) harness.Event {
	t.Helper()
	for n := 0; n < limit; n++ {
		i.ClockAdvance(step)
		if got := i.Events(q); len(got) > 0 {
			return got[0]
		}
	}
	t.Fatalf("no %s event matching %v after the clock moved %d times by %s", q.Key, q.Fields, limit, step)
	return nil
}

func TestMCPRegister(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    map[string]harness.Outcome{"mcp.register": harness.OutcomeApprove},
	})
	cmd, args := i.FakeMCPCommand(g3Stdio("acme-reg", echoCatalogue()))
	argv := []string{"mcp", "register", "--id", "acme-reg", "--name", "Acme Reg", "--command", cmd}
	for _, a := range args {
		argv = append(argv, "--args", a)
	}
	r := i.MustCLI(argv...)
	requireEvent(t, i, harness.EventQuery{Key: "mcp.register", Trace: r.Trace, Fields: map[string]any{"status": "ok", "mcp_id": "acme-reg"}})
	g2WaitMCPUp(i, "acme-reg")

	names, resp := toolNames(t, i, "acme-reg")
	if !hasAll(names, "acme_echo", "acme_ok") {
		t.Fatalf("GET tools answered %d with %v, want acme_echo and acme_ok", resp.Status, names)
	}
	if !methods(i.FakeMCPCalls("acme-reg"))["tools/list"] {
		t.Fatalf("the registered fake never saw tools/list")
	}
	if rows := g2Rows(i, "config_change", "ok", nil); len(rows) != 1 {
		t.Fatalf("got %d config_change ok rows, want 1", len(rows))
	}
}

func TestMCPRegisterDeniedCLI(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    map[string]harness.Outcome{"mcp.register": harness.OutcomeDeny},
	})
	cmd, args := i.FakeMCPCommand(g3Stdio("acme-reg", echoCatalogue()))
	argv := []string{"mcp", "register", "--id", "acme-reg", "--name", "Acme Reg", "--command", cmd}
	for _, a := range args {
		argv = append(argv, "--args", a)
	}
	r := i.CLI(argv...)
	if r.Code != 1 {
		t.Fatalf("mcp register exited %d, want 1", r.Code)
	}
	g2RequireRefusal(t, i, "mcp.register", r.Trace, "mcp.register", "cli")
	if ids := g3ListedMCPs(t, i); len(ids) != 0 {
		t.Fatalf("a refused register left MCPs %v", ids)
	}
}

func TestMCPRegisterDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    map[string]harness.Outcome{"mcp.register": harness.OutcomeDeny},
	})
	cmd, args := i.FakeMCPCommand(g3Stdio("acme-reg", echoCatalogue()))
	// The refusal status of this route family is 500 today; the event and the
	// audit row are the contract.
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/mcps", map[string]any{
		"id": "acme-reg", "display_name": "Acme Reg", "transport": "stdio", "command": cmd, "args": args,
	})
	if resp.Status >= 200 && resp.Status < 300 {
		t.Fatalf("a refused register answered %d", resp.Status)
	}
	g2RequireRefusal(t, i, "mcp.register", resp.Trace, "mcp.register", "http")
	if ids := g3ListedMCPs(t, i); len(ids) != 0 {
		t.Fatalf("a refused register left MCPs %v", ids)
	}
}

func TestMCPAuthenticateDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    map[string]harness.Outcome{"mcp.oauth.start": harness.OutcomeDeny},
		FakeMCPs:    []harness.FakeMCPSpec{{ID: "acme-oauth", Transport: "http", OAuth: true, Catalogue: echoCatalogue()}},
	})
	r := i.CLI("mcp", "authenticate", "--id", "acme-oauth", "--json")
	if r.Code != 1 {
		t.Fatalf("mcp authenticate exited %d, want 1", r.Code)
	}
	if strings.Contains(string(r.Stdout), "authorization_url") {
		t.Fatalf("a refused authenticate printed an authorization URL: %s", r.Stdout)
	}
	g2RequireRefusal(t, i, "mcp.oauth.start", r.Trace, "mcp.oauth.start", "cli")
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
	g2WaitMCPUp(i, "acme-stdio")
	g2WaitMCPUp(i, "acme-http")

	r := i.MustCLI("mcp", "list")
	requireEvent(t, i, harness.EventQuery{Key: "mcp.list", Trace: r.Trace, Fields: map[string]any{"status": "ok", "count": 2}})

	var status struct {
		MCPHealth map[string]struct {
			Connected bool `json:"connected"`
		} `json:"mcp_health"`
	}
	i.MustCLI("status", "--json").JSON(t, &status)
	for _, id := range []string{"acme-stdio", "acme-http"} {
		if h, ok := status.MCPHealth[id]; !ok || !h.Connected {
			t.Fatalf("status mcp_health[%s] is %+v (present %v), want connected", id, h, ok)
		}
	}
}

func TestMCPToolsUnknownID(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g3Creds})
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/mcps/nope/tools", nil)
	if resp.Status != http.StatusNotFound {
		t.Fatalf("GET /api/mcps/nope/tools answered %d, want 404", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.tools.list", Trace: resp.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestBridgeToolsScopedToProject(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g3Creds,
		Presence:    g3ApproveAll,
		FakeMCPs: []harness.FakeMCPSpec{
			g3Stdio("acme-alpha", g3Catalogue("acme_alpha_ping")),
			g3Stdio("acme-beta", g3Catalogue("acme_beta_ping")),
		},
	})
	g2WaitMCPUp(i, "acme-alpha")
	g2WaitMCPUp(i, "acme-beta")
	a := g2Create(t, i, "acme-proj-a", map[string]any{"allowed_mcp_ids": []string{"acme-alpha"}})
	b := g2Create(t, i, "acme-proj-b", map[string]any{"allowed_mcp_ids": []string{"acme-beta"}})
	ta, tb := g2Token(t, i, a.ID), g2Token(t, i, b.ID)

	names, listed := g2ToolNames(i, ta)
	if len(names) != 1 || names[0] != "acme_alpha_ping" {
		t.Fatalf("project A's token lists %v (exit %d), want only acme_alpha_ping", names, listed.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.list", Trace: listed.Trace, Fields: map[string]any{"status": "ok", "count": 1}})

	own := g3Call(i, ta, "acme_alpha_ping")
	if own.Code != 0 {
		t.Fatalf("project A calling its own tool exited %d\nstderr: %s", own.Code, own.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Trace: own.Trace, Fields: map[string]any{"status": "ok", "tool": "acme_alpha_ping"}})

	other := g3Call(i, ta, "acme_beta_ping")
	if other.Code != 1 {
		t.Fatalf("project A calling project B's tool exited %d, want 1", other.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "tool.call", Trace: other.Trace, Fields: map[string]any{"status": "denied"}})
	if rows := g2Rows(i, "call_tool", "denied", map[string]any{"tool": "acme_beta_ping"}); len(rows) != 1 {
		t.Fatalf("got %d denied call_tool rows for acme_beta_ping, want 1", len(rows))
	}

	cross := g3Call(i, tb, "acme_alpha_ping")
	if cross.Code != 1 {
		t.Fatalf("project B calling project A's tool exited %d, want 1", cross.Code)
	}
	if rows := g2Rows(i, "call_tool", "denied", map[string]any{"tool": "acme_alpha_ping"}); len(rows) != 1 {
		t.Fatalf("got %d denied call_tool rows for acme_alpha_ping, want 1", len(rows))
	}
	if r := g3Call(i, tb, "acme_beta_ping"); r.Code != 0 {
		t.Fatalf("project B calling its own tool exited %d", r.Code)
	}
	if calls := i.FakeMCPCalls("acme-beta"); len(calls) == 0 || !methods(calls)["tools/call"] {
		t.Fatalf("project B's own call never reached its MCP")
	}
	for _, c := range i.FakeMCPCalls("acme-alpha") {
		if c.Method == "tools/call" && strings.Contains(string(c.Params), "acme_beta_ping") {
			t.Fatalf("a refused call reached the wrong MCP")
		}
	}
}

func TestDisabledToolRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g3Creds, Presence: g3ApproveAll, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-disabled", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})
	token := g2Token(t, i, p.ID)
	if r := g3Call(i, token, "acme_echo"); r.Code != 0 {
		t.Fatalf("the granted tool answered exit %d before it was disabled", r.Code)
	}

	resp := i.HTTP(i.Credential("configurer")).Do("PUT", "/api/projects/"+p.ID, map[string]any{
		"disabled_tools": map[string][]string{"acme-stdio": {"acme_echo"}},
	})
	if resp.Status != 200 {
		t.Fatalf("PUT /api/projects/%s answered %d: %s", p.ID, resp.Status, resp.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "project.update", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "gated": false}})

	refused := g3Call(i, token, "acme_echo")
	if refused.Code != 1 {
		t.Fatalf("calling a disabled tool exited %d, want 1", refused.Code)
	}
	if rows := g2Rows(i, "call_tool", "denied", map[string]any{"tool": "acme_echo"}); len(rows) != 1 {
		t.Fatalf("got %d denied call_tool rows for acme_echo, want 1", len(rows))
	}
	if sibling := g3Call(i, token, "acme_ok"); sibling.Code != 0 {
		t.Fatalf("the sibling tool exited %d, want 0", sibling.Code)
	}
}

func TestMCPUnregister(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g3Creds, Presence: g3ApproveAll, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	p := g2Create(t, i, "acme-unreg", map[string]any{"allowed_mcp_ids": []string{"acme-stdio"}})
	token := g2Token(t, i, p.ID)

	r := i.MustCLI("mcp", "unregister", "--id", "acme-stdio")
	requireEvent(t, i, harness.EventQuery{Key: "mcp.unregister", Trace: r.Trace, Fields: map[string]any{"status": "ok", "mcp_id": "acme-stdio"}})
	if ids := g3ListedMCPs(t, i); len(ids) != 0 {
		t.Fatalf("GET /api/mcps lists %v after the unregister", ids)
	}

	var grants []struct {
		ID   string `json:"id"`
		MCPs []struct {
			MCP string `json:"mcp"`
		} `json:"mcps"`
	}
	i.MustCLI("grant", "--json").JSON(t, &grants)
	kept := false
	for _, g := range grants {
		for _, m := range g.MCPs {
			kept = kept || (g.ID == p.ID && m.MCP == "acme-stdio")
		}
	}
	if !kept {
		t.Fatalf("relay grant no longer shows the authored grant of acme-stdio: %+v", grants)
	}

	names, listed := g2ToolNames(i, token)
	if len(names) != 0 || strings.Contains(string(listed.Stdout), "acme_") {
		t.Fatalf("after the unregister the token lists %v: %s", names, listed.Stdout)
	}

	missing := i.CLI("mcp", "unregister", "--id", "no-such-mcp")
	if missing.Code != 1 {
		t.Fatalf("unregistering an unknown MCP exited %d, want 1", missing.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.unregister", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestMCPResetPermissionsRefusesWithoutTCC(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g3Creds, FakeMCPs: acmeStdioMCP()})
	g2WaitMCPUp(i, "acme-stdio")
	r := i.CLI("mcp", "reset-permissions", "--id", "acme-stdio", "--json")
	if r.Code != 1 {
		t.Fatalf("reset-permissions on an MCP with no TCC services exited %d, want 1", r.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "mcp.permissions.reset", Trace: r.Trace, Fields: map[string]any{"status": "error", "reason": "internal"}})
}

func TestReloadExternalMCPAdminOnly(t *testing.T) {
	t.Parallel()
	const secret = "acme-planted-admin-secret"
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{"admin_secret": json.RawMessage(`"` + secret + `"`)},
		FakeMCPs: acmeStdioMCP(),
	})
	g2WaitMCPUp(i, "acme-stdio")

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

// g3DieCatalogue has one tool whose call is answered and then ends the MCP.
func g3DieCatalogue() harness.Catalogue {
	c := g3Catalogue("acme_ok")
	c.Tools = append(c.Tools, json.RawMessage(`{"name":"acme_die","description":"Answer, then exit","inputSchema":{"type":"object","properties":{}},"x-fake":{"exit":3}}`))
	return c
}

func TestMCPRestartThenAbandon(t *testing.T) {
	t.Parallel()

	start := func(t *testing.T) (*harness.Instance, string) {
		i := harness.Start(t, harness.Options{
			Credentials: g3Creds,
			Presence:    g3ApproveAll,
			FakeMCPs:    []harness.FakeMCPSpec{g3Stdio("acme-flaky", g3DieCatalogue())},
		})
		g2WaitMCPUp(i, "acme-flaky")
		p := g2Create(t, i, "acme-flaky-proj", map[string]any{"allowed_mcp_ids": []string{"acme-flaky"}})
		return i, g2Token(t, i, p.ID)
	}

	t.Run("restart", func(t *testing.T) {
		t.Parallel()
		i, token := start(t)
		since := time.Now()
		if r := g3Call(i, token, "acme_die"); r.Code != 0 {
			t.Fatalf("the call that ends the MCP exited %d\nstderr: %s", r.Code, r.Stderr)
		}
		i.WaitEvent(harness.EventQuery{Key: "mcp.state", Since: since, Fields: map[string]any{"mcp_id": "acme-flaky", "state": "down"}}, mcpUpDeadline)
		g3Advance(t, i, time.Minute, 20, harness.EventQuery{Key: "mcp.state", Since: since, Fields: map[string]any{"mcp_id": "acme-flaky", "state": "restarted"}})

		if rows := g2Rows(i, "mcp_down", "", map[string]any{"mcp_id": "acme-flaky", "supervision": "down"}); len(rows) != 1 {
			t.Fatalf("got %d mcp_down rows for the outage, want 1", len(rows))
		}
		if r := g3Call(i, token, "acme_ok"); r.Code != 0 {
			t.Fatalf("a call after the restart exited %d\nstderr: %s", r.Code, r.Stderr)
		}
	})

	t.Run("abandon", func(t *testing.T) {
		t.Parallel()
		i, token := start(t)
		// The fake reads its catalogue when it starts, so a catalogue that does
		// not parse makes every restart fail while the running process is unaffected.
		cat := filepath.Join(i.Dir, "fakes", "acme-flaky.catalogue.json")
		if err := os.WriteFile(cat, []byte("not json"), 0o600); err != nil {
			t.Fatalf("breaking %s: %v", cat, err)
		}
		since := time.Now()
		if r := g3Call(i, token, "acme_die"); r.Code != 0 {
			t.Fatalf("the call that ends the MCP exited %d\nstderr: %s", r.Code, r.Stderr)
		}
		i.WaitEvent(harness.EventQuery{Key: "mcp.state", Since: since, Fields: map[string]any{"mcp_id": "acme-flaky", "state": "down"}}, mcpUpDeadline)
		g3Advance(t, i, time.Minute, 60, harness.EventQuery{Key: "mcp.state", Since: since, Fields: map[string]any{"mcp_id": "acme-flaky", "state": "abandoned"}})

		var status struct {
			MCPHealth map[string]struct {
				State string `json:"state"`
			} `json:"mcp_health"`
		}
		i.MustCLI("status", "--json").JSON(t, &status)
		if got := status.MCPHealth["acme-flaky"].State; got != "abandoned" {
			t.Fatalf("status mcp_health state is %q, want abandoned", got)
		}
		if rows := g2Rows(i, "mcp_down", "", map[string]any{"mcp_id": "acme-flaky", "supervision": "abandoned"}); len(rows) != 1 {
			t.Fatalf("got %d mcp_down rows with supervision abandoned, want 1", len(rows))
		}
	})
}
