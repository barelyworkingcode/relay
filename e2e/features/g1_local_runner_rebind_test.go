package features

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaye2e/harness"
)

// A local Claude session's agent outlives a relay restart (docs/session-host.md,
// "claude" runner). Its relay tools come back on their own: relay-sessions
// claims the surviving shim after it reattaches, and relay rebinds a launch
// identity to the shim's recorded process (G1.51). The fake agent plays the
// agent's tool child: a `!sh` turn runs `relay mcpExec` with no token, so the
// call is authorized only by its place under the session's shim.

// g1rLedger is the session ledger file under the config dir. It is a JSON array
// of records, each with a session_id and, once the shim has bound, a root.
const g1rLedger = "sessions-ledger.json"

// g1rBoot starts an instance that grants the claude-code template and one fake
// stdio MCP, and waits until the MCP is up.
func g1rBoot(t *testing.T, extra map[string]json.RawMessage) *harness.Instance {
	t.Helper()
	settings := map[string]json.RawMessage{"terminal_templates": json.RawMessage(g1lClaudeTemplate)}
	for k, v := range extra {
		settings[k] = v
	}
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove, Settings: settings, FakeMCPs: g4MCPs(),
	})
	i.WaitSessionHost(g1tHostDeadline)
	g4WaitMCP(i)
	return i
}

// g1rSession creates a project that grants the fake MCP and starts one session
// in it. The returned folder is the project's, where the agent may write.
func g1rSession(t *testing.T, i *harness.Instance, name string) (projectID, sid, dir string) {
	t.Helper()
	p := g1tProject(t, i, name, map[string]any{
		"allowed_templates": []string{"claude-code"},
		"allowed_mcp_ids":   []string{"acme-stdio"},
	})
	hook := map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{
		"matcher": "",
		"hooks":   []any{map[string]any{"type": "command", "command": "'" + harness.BundlePaths().Sessions + "' hook"}},
	}}}}
	raw, err := json.Marshal(hook)
	if err != nil {
		t.Fatalf("encoding the hook settings: %v", err)
	}
	dir = g1tDir(i, name)
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatalf("creating the hook settings folder: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"), raw, 0o600); err != nil {
		t.Fatalf("writing the hook settings: %v", err)
	}
	sid = g1tStartSession(t, i, p.ID, "sonnet", "")
	t.Cleanup(func() {
		gens, err := os.ReadDir(g1lRunnersDir(i, sid))
		if err != nil {
			return
		}
		for _, g := range gens {
			if run, err := g1lReadRun(filepath.Join(g1lRunnersDir(i, sid), g.Name())); err == nil && run.PID > 0 {
				g1lKillTree(run.PID)
			}
		}
	})
	return p.ID, sid, dir
}

// g1rSay sends one turn to the session and returns the reply text. The CLI
// returns after the turn's reply, so a `!sh` exit code needs no wait.
func g1rSay(t *testing.T, i *harness.Instance, sid, text string) string {
	t.Helper()
	var reply struct {
		Text string `json:"text"`
	}
	i.MustCLI("session", "message", "--id", sid, "--text", text, "--json").JSON(t, &reply)
	return reply.Text
}

// g1rMCPExec is a `!sh` turn that runs relay mcpExec with no token, from inside
// the session's tree, with its stdout written to out (when set).
func g1rMCPExec(i *harness.Instance, out string, args ...string) string {
	cmd := "RELAY_CONFIG_DIR=" + agShQuote(i.ConfigDir) + " " + agShQuote(harness.BundlePaths().Relay) + " mcpExec"
	for _, a := range args {
		cmd += " " + agShQuote(a)
	}
	if out != "" {
		cmd += " > " + agShQuote(out)
	}
	return "!sh " + cmd
}

// g1rRestart restarts relay and returns the time before the restart. It waits
// on the session host's manifest registration, the signal that serve is back.
func g1rRestart(t *testing.T, i *harness.Instance) time.Time {
	t.Helper()
	since := time.Now()
	i.Restart()
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Since: since,
		Fields: map[string]any{"service_id": "relaysessions", "status": "ok"}}, g1tHostDeadline)
	return since
}

// g1rRebindEvent waits for the session's session.rebind line since the restart.
func g1rRebindEvent(i *harness.Instance, sid string, since time.Time) harness.Event {
	return i.WaitEvent(harness.EventQuery{Key: "session.rebind", Since: since,
		Fields: map[string]any{"session_id": sid}}, g1tEventDeadline)
}

// g1rExpectEvent fails t unless the event reads the status and, when set, the reason.
func g1rExpectEvent(t *testing.T, ev harness.Event, status, reason string) {
	t.Helper()
	if ev["status"] != status {
		t.Fatalf("session.rebind line reads status %v reason %v, want status %s", ev["status"], ev["reason"], status)
	}
	if reason != "" && ev["reason"] != reason {
		t.Fatalf("session.rebind line reads status %v reason %v, want reason %s", ev["status"], ev["reason"], reason)
	}
}

// g1rRebindRows returns the session's session_rebind rows with the given phase
// ("" selects the rows that carry no phase).
func g1rRebindRows(t *testing.T, i *harness.Instance, sid, phase string) []g4Row {
	t.Helper()
	return g4Where(g4Audit(t, i, "--event", "session_rebind"), func(r g4Row) bool {
		return r.obj("args").str("session_id") == sid && r.str("phase") == phase
	})
}

// g1rPairs checks that the session has n intent rows and n completion rows
// that share their ids, and that every completion row reads outcome.
func g1rPairs(t *testing.T, i *harness.Instance, sid string, n int, outcome string) []g4Row {
	t.Helper()
	intents := g1rRebindRows(t, i, sid, "intent")
	done := g1rRebindRows(t, i, sid, "completion")
	if len(intents) != n || len(done) != n {
		t.Fatalf("audit holds %d intent and %d completion session_rebind rows for %s, want %d of each", len(intents), len(done), sid, n)
	}
	ids := g4IDs(intents)
	for _, c := range done {
		if !ids[c.str("id")] {
			t.Fatalf("completion row %s has no intent row with the same id", c.str("id"))
		}
		if c.str("outcome") != outcome {
			t.Fatalf("completion row reads outcome %q, want %q (row %v)", c.str("outcome"), outcome, c)
		}
	}
	for _, r := range intents {
		if r.str("outcome") != "pending" {
			t.Fatalf("intent row reads outcome %q, want pending", r.str("outcome"))
		}
	}
	return done
}

// g1rToolRows waits until the log holds at least n call_tool rows by the
// session's project_session identity and returns them.
func g1rToolRows(t *testing.T, i *harness.Instance, sid string, n int) []g4Row {
	t.Helper()
	mine := func(r g4Row) bool {
		a := r.obj("actor")
		return a.str("kind") == "project_session" && a.str("session_id") == sid
	}
	rows := g4Wait(t, i, func(rows []g4Row) bool { return len(g4Where(rows, mine)) >= n }, "--event", "call_tool")
	return g4Where(rows, mine)
}

// g1rToolsWork asserts the agent's tool child lists the MCP's tools (the same
// list as the control's) and calls one, as the session's own identity.
func g1rToolsWork(t *testing.T, i *harness.Instance, sid, projectID, dir, label string, control []byte, calls int) {
	t.Helper()
	list := filepath.Join(dir, "tools-"+label+".txt")
	if got := g1rSay(t, i, sid, g1rMCPExec(i, list, "--list")); got != "exit: 0" {
		t.Fatalf("the list call %s answered %q, want exit: 0", label, got)
	}
	listed, err := os.ReadFile(list)
	if err != nil {
		t.Fatalf("reading the list %s: %v", label, err)
	}
	if !bytes.Equal(listed, control) {
		t.Fatalf("the list %s reads %q, want the control's %q", label, listed, control)
	}
	if got := g1rSay(t, i, sid, g1rMCPExec(i, "", "--tool", "acme_ok", "--args", "{}")); got != "exit: 0" {
		t.Fatalf("the tool call %s answered %q, want exit: 0", label, got)
	}
	rows := g1rToolRows(t, i, sid, calls)
	last := rows[len(rows)-1]
	if a := last.obj("actor"); a.str("project_id") != projectID || a.str("session_id") != sid || a.str("kind") != "project_session" {
		t.Fatalf("call_tool actor reads %v, want project_session %s in project %s", a, sid, projectID)
	}
}

// Criteria 1 and 2: a surviving session's relay tools work after each of two
// restarts, with no stop and no resume, and each rebind is audited as an intent
// and a completion row.
func TestLocalRunnerRelayToolsAfterRestart(t *testing.T) {
	t.Parallel()
	i := g1rBoot(t, nil)
	projectID, sid, dir := g1rSession(t, i, "acme-rebind")

	// The control: before any restart the tool child works.
	control := filepath.Join(dir, "tools-0.txt")
	if got := g1rSay(t, i, sid, g1rMCPExec(i, control, "--list")); got != "exit: 0" {
		t.Fatalf("the list call before the restart answered %q, want exit: 0", got)
	}
	listed, err := os.ReadFile(control)
	if err != nil || len(listed) == 0 {
		t.Fatalf("the control list is %q (%v), want the MCP's tools", listed, err)
	}

	for n, label := range []string{"1", "2"} {
		since := g1rRestart(t, i)
		g1rExpectEvent(t, g1rRebindEvent(i, sid, since), "ok", "")
		g1rToolsWork(t, i, sid, projectID, dir, label, listed, n+1)
	}

	done := g1rPairs(t, i, sid, 2, "ok")
	for _, c := range done {
		if c.obj("actor").str("service_id") != "relaysessions" {
			t.Fatalf("completion row actor reads %v, want service relaysessions", c.obj("actor"))
		}
		if c.obj("args").str("project_id") != projectID {
			t.Fatalf("completion row args read %v, want project %s", c.obj("args"), projectID)
		}
	}
	if rows := i.Audit(harness.AuditQuery{Event: "session_launch", Outcome: "ok"}); len(rows) != 1 {
		t.Fatalf("audit holds %d ok session_launch rows, want 1", len(rows))
	}
	if rows := i.Audit(harness.AuditQuery{Event: "session_resume"}); len(rows) != 0 {
		t.Fatalf("audit holds %d session_resume rows, want none", len(rows))
	}
	if got := i.Events(harness.EventQuery{Key: "session.resume"}); len(got) != 0 {
		t.Fatalf("the restarts caused %d session.resume lines, want none", len(got))
	}
}

// g1rLedgerRecords reads the session ledger.
func g1rLedgerRecords(t *testing.T, i *harness.Instance) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(i.ConfigDir, g1rLedger))
	if err != nil {
		t.Fatalf("reading the session ledger: %v", err)
	}
	var recs []map[string]any
	if err := json.Unmarshal(raw, &recs); err != nil {
		t.Fatalf("the session ledger does not decode: %v", err)
	}
	return recs
}

func g1rRecord(t *testing.T, recs []map[string]any, sid string) map[string]any {
	t.Helper()
	for _, r := range recs {
		if r["session_id"] == sid {
			return r
		}
	}
	t.Fatalf("the session ledger holds no record for %s", sid)
	return nil
}

// Criterion 3: a root that is not in the claimed runner's process tree is not
// bound. While relay is down, project B's recorded root is copied into project
// A's record; A's tools stay closed and B's work.
func TestLocalRunnerRebindRefusesOutsideRoot(t *testing.T) {
	t.Parallel()
	i := g1rBoot(t, nil)
	projA, sidA, dirA := g1rSession(t, i, "acme-root-a")
	projB, sidB, dirB := g1rSession(t, i, "acme-root-b")

	// Both sessions' tools work before the restart.
	for _, c := range []struct{ sid, dir string }{{sidA, dirA}, {sidB, dirB}} {
		if got := g1rSay(t, i, c.sid, g1rMCPExec(i, filepath.Join(c.dir, "tools-0.txt"), "--list")); got != "exit: 0" {
			t.Fatalf("the control list call answered %q, want exit: 0", got)
		}
	}

	i.Stop()
	recs := g1rLedgerRecords(t, i)
	rootB, ok := g1rRecord(t, recs, sidB)["root"]
	if !ok {
		t.Fatalf("the ledger record of %s holds no root", sidB)
	}
	g1rRecord(t, recs, sidA)["root"] = rootB
	raw, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		t.Fatalf("encoding the ledger: %v", err)
	}
	if err := os.WriteFile(filepath.Join(i.ConfigDir, g1rLedger), raw, 0o600); err != nil {
		t.Fatalf("writing the ledger: %v", err)
	}

	since := g1rRestart(t, i)
	g1rExpectEvent(t, g1rRebindEvent(i, sidA, since), "denied", "root_not_in_tree")
	g1rExpectEvent(t, g1rRebindEvent(i, sidB, since), "ok", "")

	doneA := g1rRebindRows(t, i, sidA, "completion")
	if len(doneA) != 1 || doneA[0].str("outcome") != "denied" || doneA[0].str("error") != "root_not_in_tree" {
		t.Fatalf("A's completion rows read %v, want one denied with error root_not_in_tree", doneA)
	}
	if doneB := g1rRebindRows(t, i, sidB, "completion"); len(doneB) != 1 || doneB[0].str("outcome") != "ok" {
		t.Fatalf("B's completion rows read %v, want one ok", doneB)
	}

	if got := g1rSay(t, i, sidA, g1rMCPExec(i, filepath.Join(dirA, "tools-1.txt"), "--list")); got == "exit: 0" {
		t.Fatalf("A's list call answered exit: 0 with a root outside its tree, want a refusal")
	}
	if got := g1rSay(t, i, sidA, g1rMCPExec(i, "", "--tool", "acme_ok", "--args", "{}")); got == "exit: 0" {
		t.Fatalf("A's tool call answered exit: 0 with a root outside its tree, want a refusal")
	}
	if got := g1rSay(t, i, sidB, g1rMCPExec(i, filepath.Join(dirB, "tools-1.txt"), "--tool", "acme_ok", "--args", "{}")); got != "exit: 0" {
		t.Fatalf("B's tool call answered %q, want exit: 0", got)
	}
	rows := g1rToolRows(t, i, sidB, 1)
	if rows[0].obj("actor").str("project_id") != projB {
		t.Fatalf("B's call_tool row names project %v, want %s", rows[0].obj("actor"), projB)
	}
	for _, r := range g4Audit(t, i, "--event", "call_tool") {
		if r.actorProject() == projA {
			t.Fatalf("a call_tool row names project A after the refused rebind: %v", r)
		}
	}
}

// Criteria 2 and 3: a process outside every agent tree cannot take the binding.
// The raw SessionRebind request is refused as unauthorized and writes one row
// with no phase; the tokenless tool call from here fails; the agent's own call
// still works.
func TestLocalRunnerRebindRefusesStranger(t *testing.T) {
	t.Parallel()
	i := g1rBoot(t, nil)
	_, sid, dir := g1rSession(t, i, "acme-stranger")

	since := g1rRestart(t, i)
	g1rExpectEvent(t, g1rRebindEvent(i, sid, since), "ok", "")

	_, run := g1lLatest(t, i, sid)
	reply := i.BridgeSend(map[string]any{"type": "SessionRebind", "arguments": map[string]any{
		"session_id": sid, "runner_pid": run.PID, "runner_start_sec": 1, "runner_start_usec": 0,
	}})
	if reply.Type != "Error" {
		t.Fatalf("a SessionRebind from outside the services answered %q, want Error: %s", reply.Type, reply.Raw)
	}

	rows := g4Wait(t, i, g4Has(func(r g4Row) bool {
		return r.str("outcome") == "unauthorized" && r.obj("args").str("session_id") == sid
	}), "--event", "session_rebind")
	strangers := g4Where(rows, func(r g4Row) bool { return r.str("outcome") == "unauthorized" })
	if len(strangers) != 1 || strangers[0].str("phase") != "" || strangers[0].obj("actor").str("kind") != "unknown" {
		t.Fatalf("session_rebind rows read %v, want one unauthorized row with no phase and an unknown actor", strangers)
	}
	g1rPairs(t, i, sid, 1, "ok")

	if res := i.CLI("mcpExec", "--list"); res.Code == 0 {
		t.Fatalf("a tokenless mcpExec --list from outside the agent's tree exited 0, want non-zero")
	}
	if got := g1rSay(t, i, sid, g1rMCPExec(i, filepath.Join(dir, "tools-1.txt"), "--list")); got != "exit: 0" {
		t.Fatalf("the agent's own list call answered %q after the stranger's request, want exit: 0", got)
	}
}

// Criterion 2: with auditing off the intent row cannot be written, so relay
// binds nothing and the tools stay closed.
func TestLocalRunnerRebindNeedsAudit(t *testing.T) {
	t.Parallel()
	i := g1rBoot(t, nil)
	_, sid, dir := g1rSession(t, i, "acme-noaudit")

	if got := g1rSay(t, i, sid, g1rMCPExec(i, filepath.Join(dir, "tools-0.txt"), "--list")); got != "exit: 0" {
		t.Fatalf("the list call before the restart answered %q, want exit: 0", got)
	}

	// project.create is refused with auditing off, so the session is made with
	// auditing on and the setting changes while relay is down.
	i.Stop()
	path := filepath.Join(i.ConfigDir, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("settings.json does not decode: %v", err)
	}
	settings["audit"] = json.RawMessage(`{"enabled":false}`)
	if raw, err = json.Marshal(settings); err != nil {
		t.Fatalf("encoding settings.json: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing settings.json: %v", err)
	}

	since := g1rRestart(t, i)
	g1rExpectEvent(t, g1rRebindEvent(i, sid, since), "error", "audit_unavailable")

	if got := g1rSay(t, i, sid, g1rMCPExec(i, filepath.Join(dir, "tools-1.txt"), "--list")); got == "exit: 0" {
		t.Fatalf("the list call answered exit: 0 after a rebind refused for want of audit, want a refusal")
	}
}
