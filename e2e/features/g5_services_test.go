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

const g5Up = 60 * time.Second

var g5Creds = []harness.CredentialSpec{{Name: "op", Classes: []string{"read", "configure", "execute"}}}

var g5Approve = map[string]harness.Outcome{"service.register": harness.OutcomeApprove}
var g5Deny = map[string]harness.Outcome{"service.register": harness.OutcomeDeny}

// g5Record is a settings.json service record (docs/cli.md, "The record it
// writes") that runs command with args.
func g5Record(id, command string, args []string, autostart bool, caps ...string) map[string]any {
	if args == nil {
		args = []string{}
	}
	if caps == nil {
		caps = []string{}
	}
	return map[string]any{
		"id": id, "display_name": "Acme " + id, "command": command, "args": args,
		"env": map[string]string{}, "autostart": autostart, "capabilities": caps,
	}
}

func g5Settings(t *testing.T, records ...map[string]any) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("encoding services: %v", err)
	}
	return map[string]json.RawMessage{"services": b}
}

func g5Sleeper(id string, autostart bool) map[string]any {
	return g5Record(id, "/bin/sleep", []string{"600"}, autostart)
}

func g5Start(t *testing.T, presence map[string]harness.Outcome, records ...map[string]any) *harness.Instance {
	t.Helper()
	return harness.Start(t, harness.Options{
		Credentials: g5Creds,
		Presence:    presence,
		Settings:    g5Settings(t, records...),
	})
}

type g5Service struct {
	ID           string   `json:"id"`
	DisplayName  string   `json:"display_name"`
	Command      string   `json:"command"`
	Args         []string `json:"args"`
	Autostart    bool     `json:"autostart"`
	HideFromMenu bool     `json:"hide_from_menu"`
	Capabilities []string `json:"capabilities"`
	Running      bool     `json:"running"`
}

func g5Get(t *testing.T, i *harness.Instance, id string) (g5Service, harness.Response) {
	t.Helper()
	resp := i.SocketHTTP(i.Credential("op")).Do("GET", "/api/services/"+id, nil)
	var s g5Service
	if resp.Status == 200 {
		resp.JSON(t, &s)
	}
	return s, resp
}

func g5List(t *testing.T, i *harness.Instance) ([]g5Service, harness.Response) {
	t.Helper()
	resp := i.SocketHTTP(i.Credential("op")).Do("GET", "/api/services", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/services answered %d, want 200", resp.Status)
	}
	var list []g5Service
	resp.JSON(t, &list)
	return list, resp
}

func g5Ids(list []g5Service) []string {
	ids := make([]string, 0, len(list))
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	return ids
}

func g5MustGet(t *testing.T, i *harness.Instance, id string) g5Service {
	t.Helper()
	s, resp := g5Get(t, i, id)
	if resp.Status != 200 {
		t.Fatalf("GET /api/services/%s answered %d, want 200", id, resp.Status)
	}
	return s
}

func g5WaitRunning(i *harness.Instance, id string) harness.Event {
	return i.WaitEvent(harness.EventQuery{Key: "service.state", Fields: map[string]any{"service_id": id, "phase": "running"}}, g5Up)
}

type g5Status struct {
	Paths struct {
		Logs string `json:"logs"`
	} `json:"paths"`
	Runtime map[string]struct {
		PID int `json:"pid"`
	} `json:"service_runtime"`
	Supervision map[string]struct {
		Phase        string `json:"phase"`
		Attempt      int    `json:"attempt"`
		LastExitCode int    `json:"last_exit_code"`
		HasExitCode  bool   `json:"has_exit_code"`
	} `json:"service_supervision"`
}

func g5Stat(t *testing.T, i *harness.Instance) g5Status {
	t.Helper()
	var st g5Status
	i.MustCLI("status", "--json").JSON(t, &st)
	return st
}

type g5CallEntry struct {
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"`
	Code    int    `json:"code"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Auth    string `json:"auth"`
}

// g5CallLog reads the fake service's call log. A missing file reads as empty.
func g5CallLog(t *testing.T, path string) []g5CallEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []g5CallEntry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e g5CallEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("call log line is not JSON: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func g5HasEntry(entries []g5CallEntry, kind, outcome string) bool {
	for _, e := range entries {
		if e.Kind == kind && e.Outcome == outcome {
			return true
		}
	}
	return false
}

// g5Dir makes a scratch directory for a fake service's call log and config.
func g5Dir(t *testing.T) string {
	t.Helper()
	dir := harness.ScratchDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	return dir
}

// g5Fake is a service record that runs the fake enhanced service and logs to
// dir/<id>.calls.jsonl.
func g5Fake(dir, id string, autostart bool, caps []string, extra ...string) (map[string]any, string) {
	logPath := filepath.Join(dir, id+".calls.jsonl")
	args := append([]string{"--call-log", logPath}, extra...)
	return g5Record(id, harness.BundlePaths().FakeService, args, autostart, caps...), logPath
}

func TestServiceRegister(t *testing.T) {
	t.Parallel()
	i := g5Start(t, g5Approve)

	r := i.MustCLI("service", "register", "--name", "Acme Svc", "--id", "acme-svc",
		"--command", "/bin/sleep", "--args", "600", "--autostart")
	requireEvent(t, i, harness.EventQuery{Key: "service.register", Trace: r.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-svc"}})
	g5WaitRunning(i, "acme-svc")
	got := g5MustGet(t, i, "acme-svc")
	if got.Command != "/bin/sleep" || !got.Autostart || !got.Running {
		t.Fatalf("registered service reads command=%q autostart=%v running=%v, want /bin/sleep, true, true", got.Command, got.Autostart, got.Running)
	}

	resp := i.SocketHTTP(i.Credential("op")).Do("POST", "/api/services", map[string]any{
		"id": "acme-http", "display_name": "Acme Http", "command": "/bin/sleep", "args": []string{"600"},
	})
	if resp.Status != 201 {
		t.Fatalf("POST /api/services answered %d, want 201: %s", resp.Status, resp.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.create", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-http"}})
	created := g5MustGet(t, i, "acme-http")
	if created.Running {
		t.Fatalf("a service registered without autostart is running")
	}
}

func TestServiceRegisterDeniedCLI(t *testing.T) {
	t.Parallel()
	i := g5Start(t, g5Deny)

	r := i.CLI("service", "register", "--name", "Acme Svc", "--id", "acme-svc",
		"--command", "/bin/sleep", "--args", "600", "--autostart")
	if r.Code != 1 {
		t.Fatalf("service register exited %d, want 1", r.Code)
	}
	g2RequireRefusal(t, i, "service.register", r.Trace, "service.register", "cli")
	list, _ := g5List(t, i)
	if containsString(g5Ids(list), "acme-svc") {
		t.Fatalf("a refused register added the service: %v", g5Ids(list))
	}
}

func TestServiceRegisterDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := g5Start(t, g5Deny)

	// The refusal status of this route family is not part of the contract; the
	// event and the audit row carry the refusal.
	resp := i.SocketHTTP(i.Credential("op")).Do("POST", "/api/services", map[string]any{
		"id": "acme-svc", "display_name": "Acme Svc", "command": "/bin/sleep", "args": []string{"600"}, "autostart": true,
	})
	if resp.Status >= 200 && resp.Status < 300 {
		t.Fatalf("a refused POST /api/services answered %d", resp.Status)
	}
	g2RequireRefusal(t, i, "service.create", resp.Trace, "service.register", "http")
	if _, got := g5Get(t, i, "acme-svc"); got.Status != 404 {
		t.Fatalf("a refused register left the service: GET answered %d, want 404", got.Status)
	}
}

func TestServiceEditApproved(t *testing.T) {
	t.Parallel()
	i := g5Start(t, g5Approve, g5Sleeper("acme-svc", false))
	c := i.SocketHTTP(i.Credential("op"))

	resp := c.Do("PUT", "/api/services/acme-svc", map[string]any{
		"display_name": "Acme acme-svc", "command": "/bin/sleep", "args": []string{"601"},
	})
	if resp.Status != 200 {
		t.Fatalf("PUT /api/services/acme-svc answered %d, want 200: %s", resp.Status, resp.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.update", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-svc"}})
	if got := g5MustGet(t, i, "acme-svc"); len(got.Args) != 1 || got.Args[0] != "601" {
		t.Fatalf("edited args read %v, want [601]", got.Args)
	}

	resp = c.Do("PUT", "/api/services/acme-svc", map[string]any{
		"display_name": "Acme acme-svc", "command": "/bin/sleep", "args": []string{"601"},
		"capabilities": []string{"manifest"},
	})
	if resp.Status != 200 {
		t.Fatalf("PUT with capabilities answered %d, want 200: %s", resp.Status, resp.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.update", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-svc"}})
	if got := g5MustGet(t, i, "acme-svc"); len(got.Capabilities) != 1 || got.Capabilities[0] != "manifest" {
		t.Fatalf("edited capabilities read %v, want [manifest]", got.Capabilities)
	}
}

func TestServiceEditDenied(t *testing.T) {
	t.Parallel()
	i := g5Start(t, g5Deny, g5Sleeper("acme-svc", false))
	c := i.SocketHTTP(i.Credential("op"))

	rows := 0
	for name, body := range map[string]map[string]any{
		"command change": {"display_name": "Acme acme-svc", "command": "/bin/sleep", "args": []string{"601"}},
		"capability add": {"display_name": "Acme acme-svc", "command": "/bin/sleep", "args": []string{"600"}, "capabilities": []string{"manifest"}},
	} {
		resp := c.Do("PUT", "/api/services/acme-svc", body)
		if resp.Status >= 200 && resp.Status < 300 {
			t.Fatalf("a refused %s answered %d", name, resp.Status)
		}
		g2RequireRefusal(t, i, "service.update", resp.Trace, "service.register", "http")
		// Refusal rows are fail-open and may land after the answer, so the read
		// repeats until this pass's own row is counted, bounded by a deadline.
		deadline := time.Now().Add(g5Up)
		for len(g2Rows(i, "control_decision", "denied", map[string]any{"method": "service.register", "via": "http"})) < rows+1 {
			if time.Now().After(deadline) {
				t.Fatalf("a refused %s wrote no denied control_decision row of its own", name)
			}
		}
		rows++
		got := g5MustGet(t, i, "acme-svc")
		if len(got.Args) != 1 || got.Args[0] != "600" || len(got.Capabilities) != 0 {
			t.Fatalf("after a refused %s the service reads args=%v capabilities=%v, want [600] and none", name, got.Args, got.Capabilities)
		}
	}

	// docs/routes.md: a request that changes no stored value does not prompt.
	resp := c.Do("PUT", "/api/services/acme-svc", map[string]any{
		"display_name": "Acme acme-svc", "command": "/bin/sleep", "args": []string{"600"},
	})
	if resp.Status != 200 {
		t.Fatalf("a PUT that changes nothing answered %d, want 200", resp.Status)
	}
}

func TestServiceStartStop(t *testing.T) {
	t.Parallel()
	i := g5Start(t, nil, g5Sleeper("acme-svc", false))
	c := i.SocketHTTP(i.Credential("op"))

	if g5MustGet(t, i, "acme-svc").Running {
		t.Fatalf("a service with no autostart is running before a start")
	}
	var id struct {
		ID string `json:"id"`
	}
	start := i.MustCLI("service", "start", "--id", "acme-svc", "--json")
	start.JSON(t, &id)
	if id.ID != "acme-svc" {
		t.Fatalf("service start --json printed id %q, want acme-svc", id.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.start", Trace: start.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-svc"}})
	if !g5MustGet(t, i, "acme-svc").Running {
		t.Fatalf("service is not running after service start")
	}

	stop := i.MustCLI("service", "stop", "--id", "acme-svc", "--json")
	requireEvent(t, i, harness.EventQuery{Key: "service.stop", Trace: stop.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-svc"}})
	if g5MustGet(t, i, "acme-svc").Running {
		t.Fatalf("service is running after service stop")
	}

	up := c.Do("POST", "/api/services/acme-svc/start", nil)
	if up.Status != 200 {
		t.Fatalf("POST start answered %d, want 200", up.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.start", Trace: up.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-svc"}})
	if !g5MustGet(t, i, "acme-svc").Running {
		t.Fatalf("service is not running after POST start")
	}
	down := c.Do("POST", "/api/services/acme-svc/stop", nil)
	if down.Status != 200 {
		t.Fatalf("POST stop answered %d, want 200", down.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.stop", Trace: down.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-svc"}})
	if g5MustGet(t, i, "acme-svc").Running {
		t.Fatalf("service is running after POST stop")
	}
}

func TestServiceRestart(t *testing.T) {
	t.Parallel()
	i := g5Start(t, nil, g5Sleeper("acme-svc", true))
	g5WaitRunning(i, "acme-svc")
	i.WaitSessionHost(g5Up)

	for _, id := range []string{"acme-svc", "relaysessions"} {
		before, ok := g5Stat(t, i).Runtime[id]
		if !ok || before.PID == 0 {
			t.Fatalf("status lists no running process for %s", id)
		}
		r := i.MustCLI("service", "restart", "--id", id)
		requireEvent(t, i, harness.EventQuery{Key: "service.restart", Trace: r.Trace, Fields: map[string]any{"status": "ok", "service_id": id}})
		after, ok := g5Stat(t, i).Runtime[id]
		if !ok || after.PID == 0 || after.PID == before.PID {
			t.Fatalf("%s runs as pid %d after a restart from pid %d (running=%v), want a new process", id, after.PID, before.PID, ok)
		}
	}
	i.WaitSessionHost(g5Up)

	if r := i.CLI("service", "restart", "--id", "acme-nosuch"); r.Code != 1 {
		t.Fatalf("restart of an unknown service exited %d, want 1", r.Code)
	}
}

func TestServiceListAndGet(t *testing.T) {
	t.Parallel()
	i := g5Start(t, nil, g5Sleeper("acme-one", true), g5Sleeper("acme-two", false))
	g5WaitRunning(i, "acme-one")

	list, resp := g5List(t, i)
	byID := map[string]g5Service{}
	for _, s := range list {
		byID[s.ID] = s
	}
	if !byID["acme-one"].Running || byID["acme-two"].ID == "" || byID["acme-two"].Running {
		t.Fatalf("list reads %+v, want acme-one running and acme-two stopped", list)
	}
	ev := requireEvent(t, i, harness.EventQuery{Key: "service.list", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})
	if n, _ := ev["count"].(float64); int(n) != len(list) {
		t.Fatalf("service.list count is %v, the list has %d", ev["count"], len(list))
	}

	got, one := g5Get(t, i, "acme-one")
	if one.Status != 200 || got.ID != "acme-one" || !got.Running {
		t.Fatalf("GET acme-one answered %d with %+v", one.Status, got)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.get", Trace: one.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-one"}})

	_, missing := g5Get(t, i, "acme-nosuch")
	if missing.Status != 404 {
		t.Fatalf("GET of an unknown service answered %d, want 404", missing.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.get", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})

	cli := i.MustCLI("service", "list")
	requireEvent(t, i, harness.EventQuery{Key: "service.list", Trace: cli.Trace, Fields: map[string]any{"status": "ok"}})
}

func TestServiceAutostartAtStart(t *testing.T) {
	t.Parallel()
	i := g5Start(t, nil, g5Sleeper("acme-auto", true), g5Sleeper("acme-manual", false))
	g5WaitRunning(i, "acme-auto")
	if !g5MustGet(t, i, "acme-auto").Running {
		t.Fatalf("an autostart service is not running after relay started")
	}
	if g5MustGet(t, i, "acme-manual").Running {
		t.Fatalf("a service without autostart is running after relay started")
	}

	c := i.SocketHTTP(i.Credential("op"))
	resp := c.Do("PUT", "/api/services/acme-manual/autostart", map[string]any{"autostart": true})
	if resp.Status != 200 {
		t.Fatalf("PUT autostart answered %d, want 200", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.autostart.set", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-manual", "autostart": true}})

	i.Restart()
	g5WaitRunning(i, "acme-manual")
	if !g5MustGet(t, i, "acme-manual").Running {
		t.Fatalf("a service switched to autostart is not running after relay restarted")
	}

	off := i.SocketHTTP(i.Credential("op")).Do("PUT", "/api/services/acme-manual/autostart", map[string]any{"autostart": false})
	requireEvent(t, i, harness.EventQuery{Key: "service.autostart.set", Trace: off.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-manual", "autostart": false}})
	if g5MustGet(t, i, "acme-manual").Autostart {
		t.Fatalf("autostart still reads true after switching it off")
	}
}

func TestServiceRestartOnCrashThenFailed(t *testing.T) {
	t.Parallel()
	crash := g5Record("acme-crash", "/bin/sh", []string{"-c", "exit 3"}, true)
	i := g5Start(t, nil, crash)

	// The restart backoff runs on the server clock (docs/testing.md), so the
	// test moves the clock instead of waiting out 1s, 2s, 4s, 8s and 16s. The
	// restarting event with attempt N is the signal that the attempt's timer is
	// armed, so each move waits for it and the clock moves once per attempt,
	// never far enough to reach the 60s stable-run window.
	for attempt := 1; attempt <= 5; attempt++ {
		i.WaitEvent(harness.EventQuery{Key: "service.state", Fields: map[string]any{"service_id": "acme-crash", "phase": "restarting", "attempt": float64(attempt)}}, g5Up)
		i.ClockAdvance(20 * time.Second)
	}
	failed := i.WaitEvent(harness.EventQuery{Key: "service.state", Fields: map[string]any{"service_id": "acme-crash", "phase": "failed"}}, g5Up)
	if a, _ := failed["attempt"].(float64); int(a) != 5 {
		t.Fatalf("failed after attempt %v, want 5", failed["attempt"])
	}

	sup, ok := g5Stat(t, i).Supervision["acme-crash"]
	if !ok || sup.Phase != "failed" || !sup.HasExitCode || sup.LastExitCode != 3 {
		t.Fatalf("status supervision reads %+v (listed=%v), want failed with exit code 3", sup, ok)
	}
	if g5MustGet(t, i, "acme-crash").Running {
		t.Fatalf("a failed service reads running")
	}
}

func TestServiceUnregister(t *testing.T) {
	t.Parallel()
	i := g5Start(t, nil, g5Sleeper("acme-http", true), g5Sleeper("acme-cli", true))
	g5WaitRunning(i, "acme-http")
	g5WaitRunning(i, "acme-cli")
	c := i.SocketHTTP(i.Credential("op"))

	resp := c.Do("DELETE", "/api/services/acme-http", nil)
	if resp.Status != 204 {
		t.Fatalf("DELETE answered %d, want 204", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.unregister", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-http"}})
	if _, got := g5Get(t, i, "acme-http"); got.Status != 404 {
		t.Fatalf("GET of an unregistered service answered %d, want 404", got.Status)
	}

	missing := c.Do("DELETE", "/api/services/acme-nosuch", nil)
	if missing.Status != 404 {
		t.Fatalf("DELETE of an unknown service answered %d, want 404", missing.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.unregister", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})

	r := i.MustCLI("service", "unregister", "--id", "acme-cli")
	requireEvent(t, i, harness.EventQuery{Key: "service.unregister", Trace: r.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-cli"}})
	st := g5Stat(t, i)
	for _, id := range []string{"acme-http", "acme-cli"} {
		if _, running := st.Runtime[id]; running {
			t.Fatalf("%s still runs after it was unregistered", id)
		}
	}
	list, _ := g5List(t, i)
	for _, id := range g5Ids(list) {
		if id == "acme-http" || id == "acme-cli" {
			t.Fatalf("list still carries %s after it was unregistered", id)
		}
	}
	if rows := g2Rows(i, "config_change", "ok", nil); len(rows) < 2 {
		t.Fatalf("got %d config_change ok rows for two unregisters, want at least 2", len(rows))
	}
}

func TestServiceMenuAndPosition(t *testing.T) {
	t.Parallel()
	i := g5Start(t, nil, g5Sleeper("acme-a", false), g5Sleeper("acme-b", false), g5Sleeper("acme-c", false))
	c := i.SocketHTTP(i.Credential("op"))

	hide := c.Do("PUT", "/api/services/acme-b/menu", map[string]any{"hidden": true})
	if hide.Status != 200 {
		t.Fatalf("PUT menu answered %d, want 200", hide.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.menu.set", Trace: hide.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-b", "hidden": true}})
	if !g5MustGet(t, i, "acme-b").HideFromMenu || g5MustGet(t, i, "acme-a").HideFromMenu {
		t.Fatalf("hide_from_menu does not read true for acme-b only")
	}
	show := c.Do("PUT", "/api/services/acme-b/menu", map[string]any{"hidden": false})
	requireEvent(t, i, harness.EventQuery{Key: "service.menu.set", Trace: show.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-b", "hidden": false}})
	if g5MustGet(t, i, "acme-b").HideFromMenu {
		t.Fatalf("acme-b is still hidden after showing it")
	}

	order := func(list []g5Service) []string {
		var ids []string
		for _, id := range g5Ids(list) {
			if strings.HasPrefix(id, "acme-") {
				ids = append(ids, id)
			}
		}
		return ids
	}
	list, _ := g5List(t, i)
	if got := strings.Join(order(list), ","); got != "acme-a,acme-b,acme-c" {
		t.Fatalf("seeded order reads %s, want acme-a,acme-b,acme-c", got)
	}
	idx := 0
	for n, id := range g5Ids(list) {
		if id == "acme-a" {
			idx = n
		}
	}
	mv := c.Do("PUT", "/api/services/acme-c/position", map[string]any{"index": idx})
	if mv.Status != 200 {
		t.Fatalf("PUT position answered %d, want 200: %s", mv.Status, mv.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.move", Trace: mv.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-c"}})
	var moved []g5Service
	mv.JSON(t, &moved)
	if got := strings.Join(order(moved), ","); got != "acme-c,acme-a,acme-b" {
		t.Fatalf("order after the move reads %s, want acme-c,acme-a,acme-b", got)
	}
	again, _ := g5List(t, i)
	if got := strings.Join(order(again), ","); got != "acme-c,acme-a,acme-b" {
		t.Fatalf("list after the move reads %s, want acme-c,acme-a,acme-b", got)
	}
}

func TestServiceManifestActionAndConfig(t *testing.T) {
	t.Parallel()
	dir := g5Dir(t)
	cfg := filepath.Join(dir, "acme-config.json")
	rec, logPath := g5Fake(dir, "acme-fake", true, []string{"manifest"}, "--config", cfg)
	i := g5Start(t, nil, rec)
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"status": "ok", "service_id": "acme-fake"}}, g5Up)

	var act struct {
		ServiceID string `json:"service_id"`
		ActionID  string `json:"action_id"`
		OK        bool   `json:"ok"`
	}
	run := i.MustCLI("service", "action", "--id", "acme-fake", "--action", "ping", "--json")
	run.JSON(t, &act)
	if act.ServiceID != "acme-fake" || act.ActionID != "ping" || !act.OK {
		t.Fatalf("service action printed %+v, want acme-fake ping ok", act)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.action", Trace: run.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-fake", "action_id": "ping"}})
	pinged := false
	for _, e := range g5CallLog(t, logPath) {
		pinged = pinged || (e.Kind == "request" && e.Method == "POST" && e.Path == "/fakesvc/ping" && strings.HasPrefix(e.Auth, "bearer:"))
	}
	if !pinged {
		t.Fatalf("the service saw no authenticated POST /fakesvc/ping")
	}
	if r := i.CLI("service", "action", "--id", "acme-fake", "--action", "no-such-action", "--json"); r.Code != 1 {
		t.Fatalf("an undeclared action exited %d, want 1", r.Code)
	}

	var read struct {
		ServiceID string `json:"service_id"`
		Text      string `json:"text"`
	}
	get := i.MustCLI("service", "config", "--id", "acme-fake", "--json")
	get.JSON(t, &read)
	if read.ServiceID != "acme-fake" || strings.TrimSpace(read.Text) != "{}" {
		t.Fatalf("service config read %+v, want the seeded {}", read)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.config.get", Trace: get.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-fake"}})

	const saved = `{"greeting":"hello"}` + "\n"
	before := g5Stat(t, i).Runtime["acme-fake"].PID
	var save struct {
		ServiceID string `json:"service_id"`
		Restarted bool   `json:"restarted"`
	}
	set := i.CLIWith(harness.CLIOpts{Stdin: []byte(saved)}, "service", "config", "--id", "acme-fake", "--set", "-", "--json")
	if set.Code != 0 {
		t.Fatalf("service config --set exited %d", set.Code)
	}
	set.JSON(t, &save)
	if !save.Restarted {
		t.Fatalf("saving the config of a running service printed restarted=false")
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.config.save", Trace: set.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-fake", "restarted": true}})
	b, err := os.ReadFile(cfg)
	if err != nil || string(b) != saved {
		t.Fatalf("config file reads %q (err %v), want %q", b, err, saved)
	}
	if after := g5Stat(t, i).Runtime["acme-fake"].PID; after == 0 || after == before {
		t.Fatalf("service runs as pid %d after a config save from pid %d, want a new process", after, before)
	}

}

func TestServiceLogPath(t *testing.T) {
	t.Parallel()
	script := g5Record("acme-log", "/bin/sh", []string{"-c", "echo acme-log-marker; exec sleep 600"}, true)
	i := g5Start(t, nil, script)
	g5WaitRunning(i, "acme-log")

	logs := g5Stat(t, i).Paths.Logs
	if logs == "" {
		t.Fatalf("status --json carries no paths.logs")
	}
	// stop returns after the process group has exited and the log is closed,
	// so the file holds everything the service wrote.
	i.MustCLI("service", "stop", "--id", "acme-log")
	b, err := os.ReadFile(filepath.Join(logs, "acme-log.log"))
	if err != nil {
		t.Fatalf("reading paths.logs + acme-log.log: %v", err)
	}
	if !bytes.Contains(b, []byte("acme-log-marker")) {
		t.Fatalf("the service log does not hold what the service wrote: %q", b)
	}
}

func TestBridgeHelloAndManifestIdentity(t *testing.T) {
	t.Parallel()
	dir := g5Dir(t)
	good, goodLog := g5Fake(dir, "acme-good", true, []string{"manifest"})
	// The mute service never reads its launch secret, so its launch stays unspent.
	mute := g5Record("acme-mute", "/bin/sleep", []string{"600"}, true)
	// The impostor says Hello as itself and registers its manifest under
	// another service's id.
	imp, impLog := g5Fake(dir, "acme-imp", true, []string{"manifest"}, "--claim-id", "acme-good")
	i := g5Start(t, nil, good, imp, mute)

	i.WaitEvent(harness.EventQuery{Key: "bridge.hello", Fields: map[string]any{"status": "ok", "service_id": "acme-good"}}, g5Up)
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"status": "ok", "service_id": "acme-good"}}, g5Up)
	i.WaitEvent(harness.EventQuery{Key: "bridge.hello", Fields: map[string]any{"status": "ok", "service_id": "acme-imp"}}, g5Up)
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"status": "denied"}}, g5Up)

	if log := g5CallLog(t, goodLog); !g5HasEntry(log, "hello", "ok") || !g5HasEntry(log, "register", "ok") {
		t.Fatalf("the honest service's call log lacks an ok hello and register: %+v", log)
	}
	log := g5CallLog(t, impLog)
	if !g5HasEntry(log, "hello", "ok") || !g5HasEntry(log, "register", "error") {
		t.Fatalf("the impostor's call log wants an ok hello and a refused register: %+v", log)
	}
	for _, e := range log {
		if e.Kind == "register" && e.Code != -32001 {
			t.Fatalf("the refused manifest answered code %d, want -32001", e.Code)
		}
	}
	if ok := i.Events(harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"status": "ok", "service_id": "acme-imp"}}); len(ok) != 0 {
		t.Fatalf("a manifest was registered for acme-imp")
	}

	g5WaitRunning(i, "acme-mute")
	wrong := i.BridgeSend(map[string]any{"type": "Hello", "name": "acme-mute", "token": strings.Repeat("0", 64)})
	if wrong.Type != "Error" || wrong.Code != -32001 {
		t.Fatalf("Hello with a wrong secret answered %s code %d, want Error -32001", wrong.Type, wrong.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "bridge.hello", Trace: wrong.Trace, Fields: map[string]any{"status": "denied"}})

	bare := i.BridgeSend(map[string]any{"type": "RegisterManifest", "arguments": map[string]any{
		"serviceId": "acme-good", "manifest": map[string]any{"routes": []string{"/x/"}},
		"internalSocket": "/tmp/acme-none.sock", "internalToken": "acme-token",
	}})
	if bare.Type != "Error" || bare.Code != -32001 {
		t.Fatalf("RegisterManifest from a caller with no identity answered %s code %d, want Error -32001", bare.Type, bare.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.manifest.register", Trace: bare.Trace, Fields: map[string]any{"status": "denied"}})
}
