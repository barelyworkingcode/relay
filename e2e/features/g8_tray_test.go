package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaye2e/harness"
)

// g8Services seeds one autostart service record.
func g8Services(id, command string, args ...string) map[string]json.RawMessage {
	rec, err := json.Marshal([]map[string]any{{
		"id": id, "display_name": id, "command": command, "args": args,
		"env": map[string]string{}, "autostart": true, "capabilities": []string{},
	}})
	if err != nil {
		panic(err)
	}
	return map[string]json.RawMessage{"services": rec}
}

type g8Status struct {
	Version    string `json:"version"`
	SealStatus string `json:"seal_status"`
	Paths      struct {
		Config string `json:"config"`
		Logs   string `json:"logs"`
	} `json:"paths"`
	MCPHealth map[string]struct {
		Connected bool `json:"connected"`
	} `json:"mcp_health"`
	ServiceRuntime map[string]struct {
		PID int `json:"pid"`
	} `json:"service_runtime"`
	ServiceSupervision map[string]struct {
		Phase   string `json:"phase"`
		Attempt int    `json:"attempt"`
	} `json:"service_supervision"`
}

func g8ReadStatus(t *testing.T, i *harness.Instance) (g8Status, harness.Result) {
	t.Helper()
	r := i.MustCLI("status", "--json")
	var st g8Status
	r.JSON(t, &st)
	return st, r
}

func TestStatusMatchesState(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: g8Services("acme-sleep", "/bin/sleep", "3600"),
		FakeMCPs: acmeStdioMCP(),
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)
	i.WaitEvent(harness.EventQuery{Key: "service.state", Fields: map[string]any{"service_id": "acme-sleep", "phase": "running"}}, mcpUpDeadline)

	st, r := g8ReadStatus(t, i)
	if st.Version == "" || st.Version != i.ReloadReady().Version {
		t.Fatalf("status version %q, ready.json version %q, want them equal and not empty", st.Version, i.ReloadReady().Version)
	}
	if st.SealStatus != "" {
		t.Fatalf("seal_status is %q on a healthy store, want empty", st.SealStatus)
	}
	if !st.MCPHealth["acme-stdio"].Connected {
		t.Fatalf("mcp_health[acme-stdio].connected is false: %+v", st.MCPHealth)
	}
	if st.ServiceRuntime["acme-sleep"].PID <= 0 {
		t.Fatalf("service_runtime[acme-sleep].pid is not set: %+v", st.ServiceRuntime)
	}
	requireEvent(t, i, harness.EventQuery{Key: "status.view", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
}

func TestRecentToolCallsFilter(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		FakeMCPs:    acmeStdioMCP(),
		Credentials: readOnly,
		Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeApprove, "project.reveal_token": harness.OutcomeApprove},
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)

	dir := filepath.Join(i.Home, "work", "acme-tools")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body, err := json.Marshal(map[string]any{"name": "acme-tools", "path": dir, "allowed_mcp_ids": []string{"acme-stdio"}})
	if err != nil {
		t.Fatalf("encoding the project: %v", err)
	}
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	var tok struct {
		Token string `json:"token"`
	}
	i.MustCLI("project", "token", "--id", p.ID, "--json").JSON(t, &tok)
	if tok.Token == "" {
		t.Fatalf("project token printed no token")
	}
	for n := 0; n < 2; n++ {
		i.MustCLI("mcpExec", "--token", tok.Token, "--tool", "acme_ok")
	}
	if len(i.Audit(harness.AuditQuery{Event: "config_change"})) == 0 {
		t.Fatalf("the project create left no config_change row, so the filter has nothing to exclude")
	}

	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/audit?event=call_tool", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/audit?event=call_tool answered %d, want 200", resp.Status)
	}
	var rows []struct {
		Event string `json:"event"`
		TS    string `json:"ts"`
	}
	resp.JSON(t, &rows)
	if len(rows) < 2 {
		t.Fatalf("the call_tool filter returned %d rows after two calls", len(rows))
	}
	var prev time.Time
	for n, row := range rows {
		if row.Event != "call_tool" {
			t.Fatalf("row %d has event %q, want only call_tool", n, row.Event)
		}
		ts, err := time.Parse(time.RFC3339Nano, row.TS)
		if err != nil {
			t.Fatalf("row %d ts %q: %v", n, row.TS, err)
		}
		// routes.md: the route answers newest first.
		if n > 0 && ts.After(prev) {
			t.Fatalf("row %d (%s) is newer than row %d (%s), want newest first", n, ts, n-1, prev)
		}
		prev = ts
	}
}

func TestStatusPaths(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{})

	st, _ := g8ReadStatus(t, i)
	want, err := filepath.EvalSymlinks(i.ConfigDir)
	if err != nil {
		t.Fatalf("resolving %s: %v", i.ConfigDir, err)
	}
	got, err := filepath.EvalSymlinks(st.Paths.Config)
	if err != nil || got != want {
		t.Fatalf("paths.config %q (resolved %q, err %v), want the instance config dir %q", st.Paths.Config, got, err, want)
	}
	if info, err := os.Stat(st.Paths.Logs); err != nil || !info.IsDir() {
		t.Fatalf("paths.logs %q is not a directory: %v", st.Paths.Logs, err)
	}
	if _, err := os.Stat(filepath.Join(st.Paths.Logs, "relay.log")); err != nil {
		t.Fatalf("paths.logs holds no relay.log: %v", err)
	}
}

func TestServiceStateInStatus(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Settings: g8Services("acme-crash", "/bin/sh", "-c", "exit 3")})

	// Attempt 3 waits 4 s before the next start, a window wide enough for the
	// status call to land inside it.
	ev := i.WaitEvent(harness.EventQuery{Key: "service.state", Fields: map[string]any{"service_id": "acme-crash", "phase": "restarting", "attempt": 3}}, mcpUpDeadline)
	st, _ := g8ReadStatus(t, i)
	sup, ok := st.ServiceSupervision["acme-crash"]
	if !ok || sup.Phase != "restarting" || float64(sup.Attempt) != ev["attempt"] {
		t.Fatalf("service_supervision[acme-crash] is %+v (present %v), want restarting at attempt %v", sup, ok, ev["attempt"])
	}
}

func TestPendingEnrolmentCount(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Settings: map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)}})

	// A source may lodge one request per rate window; the clock moves past it.
	first := g10Lodge(t, i, "acme-a")
	i.ClockAdvance(g10RateWindow)
	second := g10Lodge(t, i, "acme-b")

	r := i.MustCLI("enrol", "requests", "--json")
	var reqs []g10Request
	r.JSON(t, &reqs)
	if len(reqs) != 2 {
		t.Fatalf("enrol requests lists %d requests, want 2: %+v", len(reqs), reqs)
	}
	for _, want := range []struct {
		lodged g10Lodged
		label  string
	}{{first, "acme-a"}, {second, "acme-b"}} {
		found := false
		for _, got := range reqs {
			found = found || (got.RequestID == want.lodged.requestID && got.Label == want.label && got.Spki == want.lodged.spki && got.Spki != "")
		}
		if !found {
			t.Fatalf("enrol requests does not list %s with label %s and fingerprint %s: %+v", want.lodged.requestID, want.label, want.lodged.spki, reqs)
		}
	}
}
