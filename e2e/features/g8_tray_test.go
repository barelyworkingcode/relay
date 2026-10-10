package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"relaye2e/harness"
)

type g8Status struct {
	Version    string `json:"version"`
	SealStatus string `json:"seal_status"`
	Paths      struct {
		Config string `json:"config"`
		Logs   string `json:"logs"`
	} `json:"paths"`
	MCPHealth map[string]struct {
		ID        string `json:"id"`
		Connected bool   `json:"connected"`
	} `json:"mcp_health"`
	ServiceRuntime map[string]struct {
		PID       int    `json:"pid"`
		StartedAt string `json:"started_at"`
	} `json:"service_runtime"`
	ServiceSupervision map[string]struct {
		Phase   string `json:"phase"`
		Attempt int    `json:"attempt"`
	} `json:"service_supervision"`
}

func g8ServiceRecord(id, command string, args ...string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"id": id, "display_name": id, "command": command,
		"args": args, "env": map[string]string{}, "autostart": true,
	})
	return raw
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
		Settings: map[string]json.RawMessage{
			"services": json.RawMessage("[" + string(g8ServiceRecord("acme-sleeper", "/bin/sleep", "600")) + "]"),
		},
		FakeMCPs: acmeStdioMCP(),
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)
	i.WaitEvent(harness.EventQuery{Key: "service.state", Fields: map[string]any{"service_id": "acme-sleeper", "phase": "running"}}, mcpUpDeadline)

	st, r := g8ReadStatus(t, i)
	if st.Version == "" || st.Version != i.ReloadReady().Version {
		t.Fatalf("status version %q, ready.json version %q", st.Version, i.Ready.Version)
	}
	if st.SealStatus != "" {
		t.Fatalf("seal_status %q on a healthy store, want empty", st.SealStatus)
	}
	if h, ok := st.MCPHealth["acme-stdio"]; !ok || !h.Connected {
		t.Fatalf("mcp_health[acme-stdio] is %+v (present %v), want connected", h, ok)
	}
	if rt, ok := st.ServiceRuntime["acme-sleeper"]; !ok || rt.PID <= 0 {
		t.Fatalf("service_runtime[acme-sleeper] is %+v (present %v), want a pid", rt, ok)
	}
	requireEvent(t, i, harness.EventQuery{Key: "status.view", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
}

func TestRecentToolCallsFilter(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: readOnly,
		FakeMCPs:    acmeStdioMCP(),
		Presence: map[string]harness.Outcome{
			"project.grant":        harness.OutcomeApprove,
			"project.reveal_token": harness.OutcomeApprove,
		},
	})
	i.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": "acme-stdio", "state": "up"}}, mcpUpDeadline)

	dir := filepath.Join(i.Home, "work", "acme-calls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body, _ := json.Marshal(map[string]any{"name": "acme-calls", "path": dir, "allowed_mcp_ids": []string{"acme-stdio"}})
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	var tok struct {
		Token string `json:"token"`
	}
	i.MustCLI("project", "token", "--id", p.ID, "--json").JSON(t, &tok)
	if tok.Token == "" {
		t.Fatalf("project token printed no token")
	}

	for _, tool := range []string{"acme_ok", "acme_echo"} {
		reply := i.BridgeSend(map[string]any{"type": "CallTool", "name": tool, "arguments": map[string]any{}, "token": tok.Token})
		if reply.Type != "Result" {
			t.Fatalf("CallTool %s answered %s (code %d), want Result", tool, reply.Type, reply.Code)
		}
	}

	reader := i.HTTP(i.Credential("reader"))
	resp := reader.Do("GET", "/api/audit?event=call_tool&limit=100", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/audit?event=call_tool answered %d, want 200", resp.Status)
	}
	var rows []struct {
		Event string `json:"event"`
		TS    string `json:"ts"`
		Tool  string `json:"tool"`
	}
	resp.JSON(t, &rows)
	if len(rows) == 0 {
		t.Fatalf("no call_tool rows after two tool calls")
	}
	firstIdx := map[string]int{}
	var prev time.Time
	for n, row := range rows {
		if row.Event != "call_tool" {
			t.Fatalf("row %d is a %q row in a call_tool query", n, row.Event)
		}
		at, err := time.Parse(time.RFC3339Nano, row.TS)
		if err != nil {
			t.Fatalf("row %d ts %q: %v", n, row.TS, err)
		}
		if n > 0 && at.After(prev) {
			t.Fatalf("row %d (%s) is newer than row %d: the list is not newest first", n, row.TS, n-1)
		}
		prev = at
		if _, seen := firstIdx[row.Tool]; !seen {
			firstIdx[row.Tool] = n
		}
	}
	okAt, okSeen := firstIdx["acme_ok"]
	echoAt, echoSeen := firstIdx["acme_echo"]
	if !okSeen || !echoSeen || echoAt >= okAt {
		t.Fatalf("call_tool rows by first tool position %v, want acme_echo (called last) before acme_ok", firstIdx)
	}
	requireEvent(t, i, harness.EventQuery{Key: "audit.query", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})

	// The unfiltered log holds other rows, so the filter is what leaves only calls.
	var all []struct {
		Event string `json:"event"`
	}
	reader.Do("GET", "/api/audit?limit=200", nil).JSON(t, &all)
	other := false
	for _, row := range all {
		other = other || row.Event != "call_tool"
	}
	if !other {
		t.Fatalf("the unfiltered audit holds only call_tool rows; the filter proves nothing")
	}
}

func TestStatusPaths(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{})

	st, _ := g8ReadStatus(t, i)
	same := func(a, b string) bool {
		ra, err1 := filepath.EvalSymlinks(a)
		rb, err2 := filepath.EvalSymlinks(b)
		return a == b || (err1 == nil && err2 == nil && ra == rb)
	}
	if !same(st.Paths.Config, i.ConfigDir) {
		t.Fatalf("paths.config %q, instance config dir %q", st.Paths.Config, i.ConfigDir)
	}
	if info, err := os.Stat(st.Paths.Logs); err != nil || !info.IsDir() {
		t.Fatalf("paths.logs %q is not a directory (%v)", st.Paths.Logs, err)
	}
	if _, err := os.Stat(filepath.Join(st.Paths.Logs, "relay.log")); err != nil {
		t.Fatalf("paths.logs %q holds no relay.log: %v", st.Paths.Logs, err)
	}
}

func TestServiceStateInStatus(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{
			"services": json.RawMessage("[" + string(g8ServiceRecord("acme-crasher", "/bin/sh", "-c", "exit 3")) + "]"),
		},
	})
	// Each restart waits twice as long as the last: 1s, 2s, 4s. The fourth
	// restart is scheduled 8s out, which leaves the status read a wide window.
	i.WaitEvent(harness.EventQuery{Key: "service.state", Fields: map[string]any{"service_id": "acme-crasher", "phase": "restarting", "attempt": 4}}, mcpUpDeadline)

	st, _ := g8ReadStatus(t, i)
	sup, ok := st.ServiceSupervision["acme-crasher"]
	if !ok || sup.Phase != "restarting" || sup.Attempt != 4 {
		t.Fatalf("service_supervision[acme-crasher] is %+v (present %v), want restarting attempt 4", sup, ok)
	}
	if _, running := st.ServiceRuntime["acme-crasher"]; running {
		t.Fatalf("service_runtime lists a service that is waiting to restart")
	}
}

func TestPendingEnrolmentCount(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: map[string]json.RawMessage{"remote": json.RawMessage(remoteBlock)},
	})

	type lodged struct{ requestID, label, spki string }
	var want []lodged
	for n, label := range []string{"acme-laptop", "acme-tablet"} {
		if n > 0 {
			// One lodge per source in any ten seconds.
			i.ClockAdvance(11 * time.Second)
		}
		id := harness.NewRemoteIdentity(t, label)
		replies := i.EnrolSend(map[string]any{"type": "EnrolmentRequest", "csr_pem": string(id.CSRPEM()), "label": label})
		if len(replies) != 1 || replies[0]["type"] != "Result" {
			t.Fatalf("lodging %s answered %v, want one Result", label, replies)
		}
		result, _ := replies[0]["result"].(map[string]any)
		reqID, _ := result["request_id"].(string)
		spki, _ := result["spki_sha256"].(string)
		if reqID == "" || spki == "" {
			t.Fatalf("the Result for %s lacks request_id or spki_sha256: %v", label, result)
		}
		want = append(want, lodged{reqID, label, spki})
	}

	r := i.MustCLI("enrol", "requests", "--json")
	var pending []struct {
		RequestID string `json:"request_id"`
		Label     string `json:"label"`
		SPKI      string `json:"spki_sha256"`
	}
	r.JSON(t, &pending)
	if len(pending) != len(want) {
		t.Fatalf("enrol requests lists %d requests, want %d", len(pending), len(want))
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, w := range want {
		found := false
		for _, p := range pending {
			if p.RequestID != w.requestID {
				continue
			}
			found = true
			if p.Label != w.label || p.SPKI != w.spki || !hex64.MatchString(p.SPKI) {
				t.Fatalf("request %s lists label %q key %q, want %q and %q", w.requestID, p.Label, p.SPKI, w.label, w.spki)
			}
		}
		if !found {
			t.Fatalf("enrol requests does not list %s (%s)", w.requestID, w.label)
		}
	}
}
