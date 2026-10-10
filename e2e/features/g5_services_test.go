package features

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

const g5Deadline = 60 * time.Second

var g5Cred = []harness.CredentialSpec{{Name: "svc", Classes: []string{"read", "configure", "execute"}}}

func g5Approve() map[string]harness.Outcome {
	return map[string]harness.Outcome{"service.register": harness.OutcomeApprove}
}

func g5Deny() map[string]harness.Outcome {
	return map[string]harness.Outcome{"service.register": harness.OutcomeDeny}
}

// g5Dir returns a short folder under /tmp: a fake's internal socket and call
// log paths must stay under the unix socket path limit.
func g5Dir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "g5-")
	if err != nil {
		t.Fatalf("creating a scratch folder: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// g5Record builds a settings.json service record.
func g5Record(id, command string, autostart bool, args ...string) map[string]any {
	if args == nil {
		args = []string{}
	}
	return map[string]any{
		"id": id, "display_name": "Acme " + id, "command": command, "args": args,
		"env": map[string]string{}, "autostart": autostart, "capabilities": []string{},
	}
}

func g5Settings(t *testing.T, recs ...map[string]any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(recs)
	if err != nil {
		t.Fatalf("encoding services: %v", err)
	}
	return map[string]json.RawMessage{"services": raw}
}

type g5Status struct {
	Paths struct {
		Logs string `json:"logs"`
	} `json:"paths"`
	Runtime map[string]struct {
		PID int `json:"pid"`
	} `json:"service_runtime"`
	Supervision map[string]struct {
		Phase string `json:"phase"`
	} `json:"service_supervision"`
}

func g5ReadStatus(t *testing.T, i *harness.Instance) g5Status {
	t.Helper()
	var s g5Status
	i.MustCLI("status", "--json").JSON(t, &s)
	return s
}

func g5WaitRunning(t *testing.T, i *harness.Instance, id string, since time.Time) {
	t.Helper()
	i.WaitEvent(harness.EventQuery{Key: "service.state", Since: since, Fields: map[string]any{"service_id": id, "phase": "running"}}, g5Deadline)
}

type g5Service struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	Hidden      bool     `json:"hide_from_menu"`
}

func g5Get(t *testing.T, c *harness.Client, id string) g5Service {
	t.Helper()
	resp := c.Do("GET", "/api/services/"+id, nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/services/%s answered %d, want 200", id, resp.Status)
	}
	var s g5Service
	resp.JSON(t, &s)
	return s
}

func g5List(t *testing.T, c *harness.Client) []g5Service {
	t.Helper()
	resp := c.Do("GET", "/api/services", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/services answered %d, want 200", resp.Status)
	}
	var l []g5Service
	resp.JSON(t, &l)
	return l
}

func g5Index(l []g5Service, id string) int {
	for n, s := range l {
		if s.ID == id {
			return n
		}
	}
	return -1
}

// g5RequireDenied checks the refusal set of a gated act: the denied event, the
// control_decision denied row naming the gate, and nothing else.
func g5RequireDenied(t *testing.T, i *harness.Instance, event, trace, method, via string) {
	t.Helper()
	requireEvent(t, i, harness.EventQuery{Key: event, Trace: trace, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		if row["method"] == method && row["via"] == via {
			return
		}
	}
	t.Fatalf("no control_decision denied row with method %s via %s", method, via)
}

func TestServiceRegister(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g5Approve(), Credentials: g5Cred})
	before := time.Now()
	r := i.CLI("service", "register", "--name", "Acme Sleeper", "--id", "acme-sleeper", "--command", "/bin/sleep", "--args", "3600", "--autostart")
	if r.Code != 0 {
		t.Fatalf("an approved service register exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.register", Trace: r.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-sleeper"}})
	g5WaitRunning(t, i, "acme-sleeper", before)
	if _, ok := g5ReadStatus(t, i).Runtime["acme-sleeper"]; !ok {
		t.Fatalf("status service_runtime lacks the registered autostart service")
	}

	// The Settings door: one line per operation, so the HTTP create writes
	// service.create and the CLI above wrote service.register.
	resp := i.SocketHTTP(i.Credential("svc")).Do("POST", "/api/services", map[string]any{"id": "acme-http", "display_name": "Acme Http", "command": "/bin/sleep", "args": []string{"3600"}})
	if resp.Status != 201 {
		t.Fatalf("an approved POST /api/services answered %d, want 201\nbody: %s", resp.Status, resp.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.create", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-http"}})
	if g5Index(g5List(t, i.HTTP(i.Credential("svc"))), "acme-http") < 0 {
		t.Fatalf("GET /api/services lacks the service the POST created")
	}
}

func TestServiceRegisterDeniedCLI(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g5Deny(), Credentials: g5Cred})
	r := i.CLI("service", "register", "--name", "Acme Sleeper", "--id", "acme-sleeper", "--command", "/bin/sleep", "--args", "3600", "--autostart")
	if r.Code != 1 {
		t.Fatalf("a denied service register exited %d, want 1", r.Code)
	}
	g5RequireDenied(t, i, "service.register", r.Trace, "service.register", "cli")
	if g5Index(g5List(t, i.HTTP(i.Credential("svc"))), "acme-sleeper") >= 0 {
		t.Fatalf("a denied register left the service in GET /api/services")
	}
}

func TestServiceRegisterDeniedHTTP(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g5Deny(), Credentials: g5Cred})
	c := i.SocketHTTP(i.Credential("svc"))
	resp := c.Do("POST", "/api/services", map[string]any{"id": "acme-sleeper", "display_name": "Acme Sleeper", "command": "/bin/sleep", "args": []string{"3600"}})
	if resp.Status == 201 {
		t.Fatalf("a denied POST /api/services answered 201")
	}
	g5RequireDenied(t, i, "service.create", resp.Trace, "service.register", "http")
	if g5Index(g5List(t, i.HTTP(i.Credential("svc"))), "acme-sleeper") >= 0 {
		t.Fatalf("a denied POST left the service in GET /api/services")
	}
}

func TestServiceEditApproved(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence: g5Approve(), Credentials: g5Cred,
		Settings: g5Settings(t, g5Record("acme-sleeper", "/bin/sleep", false, "3600")),
	})
	resp := i.SocketHTTP(i.Credential("svc")).Do("PUT", "/api/services/acme-sleeper", map[string]any{
		"display_name": "Acme acme-sleeper", "command": "/bin/sleep", "args": []string{"7200"},
	})
	if resp.Status != 200 {
		t.Fatalf("an approved PUT /api/services/acme-sleeper answered %d, want 200\nbody: %s", resp.Status, resp.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.update", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-sleeper"}})
	got := g5Get(t, i.HTTP(i.Credential("svc")), "acme-sleeper")
	if len(got.Args) != 1 || got.Args[0] != "7200" {
		t.Fatalf("GET shows args %v, want [7200]", got.Args)
	}
}

func TestServiceEditDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence: g5Deny(), Credentials: g5Cred,
		Settings: g5Settings(t, g5Record("acme-sleeper", "/bin/sleep", false, "3600")),
	})
	resp := i.SocketHTTP(i.Credential("svc")).Do("PUT", "/api/services/acme-sleeper", map[string]any{
		"display_name": "Acme acme-sleeper", "command": "/bin/cat", "args": []string{"7200"},
	})
	if resp.Status == 200 {
		t.Fatalf("a denied PUT answered 200")
	}
	g5RequireDenied(t, i, "service.update", resp.Trace, "service.register", "http")
	got := g5Get(t, i.HTTP(i.Credential("svc")), "acme-sleeper")
	if got.Command != "/bin/sleep" || len(got.Args) != 1 || got.Args[0] != "3600" {
		t.Fatalf("a denied edit changed the service to %+v", got)
	}
}

func TestServiceStartStop(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: g5Settings(t, g5Record("acme-sleeper", "/bin/sleep", true, "3600")),
	})
	g5WaitRunning(t, i, "acme-sleeper", time.Time{})

	stop := i.MustCLI("service", "stop", "--id", "acme-sleeper", "--json")
	var out struct {
		ID string `json:"id"`
	}
	stop.JSON(t, &out)
	if out.ID != "acme-sleeper" {
		t.Fatalf("service stop printed id %q", out.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.stop", Trace: stop.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-sleeper"}})
	if _, ok := g5ReadStatus(t, i).Runtime["acme-sleeper"]; ok {
		t.Fatalf("service_runtime still holds the stopped service")
	}

	before := time.Now()
	start := i.MustCLI("service", "start", "--id", "acme-sleeper", "--json")
	start.JSON(t, &out)
	if out.ID != "acme-sleeper" {
		t.Fatalf("service start printed id %q", out.ID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.start", Trace: start.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-sleeper"}})
	g5WaitRunning(t, i, "acme-sleeper", before)
	if _, ok := g5ReadStatus(t, i).Runtime["acme-sleeper"]; !ok {
		t.Fatalf("service_runtime lacks the started service")
	}
}

func TestServiceRestart(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: g5Settings(t, g5Record("acme-sleeper", "/bin/sleep", true, "3600")),
	})
	g5WaitRunning(t, i, "acme-sleeper", time.Time{})
	oldPID := g5ReadStatus(t, i).Runtime["acme-sleeper"].PID
	if oldPID == 0 {
		t.Fatalf("service_runtime holds no pid for the running service")
	}

	before := time.Now()
	r := i.MustCLI("service", "restart", "--id", "acme-sleeper")
	requireEvent(t, i, harness.EventQuery{Key: "service.restart", Trace: r.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-sleeper"}})
	g5WaitRunning(t, i, "acme-sleeper", before)
	if got := g5ReadStatus(t, i).Runtime["acme-sleeper"].PID; got == 0 || got == oldPID {
		t.Fatalf("pid after restart is %d, was %d; want a new pid", got, oldPID)
	}

	// The session host reloads on request: it registers its manifest again.
	before = time.Now()
	host := i.MustCLI("service", "restart", "--id", "relaysessions")
	requireEvent(t, i, harness.EventQuery{Key: "service.restart", Trace: host.Trace, Fields: map[string]any{"status": "ok", "service_id": "relaysessions"}})
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Since: before, Fields: map[string]any{"service_id": "relaysessions", "status": "ok"}}, g5Deadline)
}

func TestServiceListAndGet(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g5Cred,
		Settings:    g5Settings(t, g5Record("acme-sleeper", "/bin/sleep", false, "3600")),
	})
	c := i.HTTP(i.Credential("svc"))

	list := c.Do("GET", "/api/services", nil)
	if list.Status != 200 {
		t.Fatalf("GET /api/services answered %d", list.Status)
	}
	var services []g5Service
	list.JSON(t, &services)
	if g5Index(services, "acme-sleeper") < 0 || g5Index(services, "relaysessions") < 0 {
		t.Fatalf("GET /api/services lacks the seeded service or the session host: %+v", services)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.list", Trace: list.Trace, Fields: map[string]any{"status": "ok", "count": len(services)}})

	cli := i.CLI("service", "list")
	if cli.Code != 0 {
		t.Fatalf("service list exited %d", cli.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.list", Trace: cli.Trace, Fields: map[string]any{"status": "ok"}})

	get := c.Do("GET", "/api/services/acme-sleeper", nil)
	if get.Status != 200 {
		t.Fatalf("GET /api/services/acme-sleeper answered %d", get.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.get", Trace: get.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-sleeper"}})

	missing := c.Do("GET", "/api/services/acme-missing", nil)
	if missing.Status != 404 {
		t.Fatalf("GET of an unknown service answered %d, want 404", missing.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.get", Trace: missing.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestServiceAutostartAtStart(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g5Cred,
		Settings:    g5Settings(t, g5Record("acme-sleeper", "/bin/sleep", false, "3600")),
	})
	resp := i.HTTP(i.Credential("svc")).Do("PUT", "/api/services/acme-sleeper/autostart", map[string]any{"autostart": true})
	if resp.Status != 200 {
		t.Fatalf("PUT autostart answered %d\nbody: %s", resp.Status, resp.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.autostart.set", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-sleeper", "autostart": true}})

	before := time.Now()
	i.Restart()
	g5WaitRunning(t, i, "acme-sleeper", before)
	if got := i.Events(harness.EventQuery{Key: "service.start", Fields: map[string]any{"service_id": "acme-sleeper"}}); len(got) != 0 {
		t.Fatalf("the service came up after a restart with %d service.start calls", len(got))
	}
}

func TestServiceRestartOnCrashThenFailed(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: g5Settings(t, g5Record("acme-crash", "/bin/sh", true, "-c", "exit 3")),
	})
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		i.WaitEvent(harness.EventQuery{Key: "service.state", Fields: map[string]any{"service_id": "acme-crash", "phase": "restarting", "attempt": attempt}}, g5Deadline)
		// 60 s is the backoff cap, so one move passes any delay.
		i.ClockAdvance(60 * time.Second)
	}
	i.WaitEvent(harness.EventQuery{Key: "service.state", Fields: map[string]any{"service_id": "acme-crash", "phase": "failed"}}, g5Deadline)
	if got := g5ReadStatus(t, i).Supervision["acme-crash"].Phase; got != "failed" {
		t.Fatalf("service_supervision phase is %q, want failed", got)
	}
}

func TestServiceUnregister(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Settings: g5Settings(t, g5Record("acme-sleeper", "/bin/sleep", true, "3600")),
	})
	g5WaitRunning(t, i, "acme-sleeper", time.Time{})

	r := i.MustCLI("service", "unregister", "--id", "acme-sleeper")
	requireEvent(t, i, harness.EventQuery{Key: "service.unregister", Trace: r.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-sleeper"}})
	if _, ok := g5ReadStatus(t, i).Runtime["acme-sleeper"]; ok {
		t.Fatalf("service_runtime still holds the unregistered service")
	}

	again := i.CLI("service", "unregister", "--id", "acme-sleeper")
	if again.Code != 1 {
		t.Fatalf("unregistering an unknown service exited %d, want 1", again.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.unregister", Trace: again.Trace, Fields: map[string]any{"status": "error", "reason": "not_found"}})
}

func TestServiceMenuAndPosition(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g5Cred,
		Settings: g5Settings(t,
			g5Record("acme-first", "/bin/sleep", false, "3600"),
			g5Record("acme-second", "/bin/sleep", false, "3600")),
	})
	c := i.HTTP(i.Credential("svc"))

	menu := c.Do("PUT", "/api/services/acme-second/menu", map[string]any{"hidden": true})
	if menu.Status != 200 {
		t.Fatalf("PUT menu answered %d\nbody: %s", menu.Status, menu.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.menu.set", Trace: menu.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-second", "hidden": true}})
	if !g5Get(t, c, "acme-second").Hidden {
		t.Fatalf("GET shows hide_from_menu false after hiding")
	}

	if l := g5List(t, c); g5Index(l, "acme-first") > g5Index(l, "acme-second") {
		t.Fatalf("the seeded order is not first, second: %+v", l)
	}
	move := c.Do("PUT", "/api/services/acme-second/position", map[string]any{"index": 0})
	if move.Status != 200 {
		t.Fatalf("PUT position answered %d\nbody: %s", move.Status, move.Body)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.move", Trace: move.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-second", "index": 0}})
	var moved []g5Service
	move.JSON(t, &moved)
	if g5Index(moved, "acme-second") != 0 {
		t.Fatalf("the move response lists acme-second at %d, want 0", g5Index(moved, "acme-second"))
	}
	if l := g5List(t, c); g5Index(l, "acme-second") != 0 {
		t.Fatalf("GET lists acme-second at %d after the move, want 0", g5Index(l, "acme-second"))
	}
}

// g5FakeRecord places the fake service with the manifest capability. dir is
// its working folder, which holds the config file and the call log.
func g5FakeRecord(id, dir string, extra ...string) map[string]any {
	args := append([]string{"--call-log", filepath.Join(dir, id+".log")}, extra...)
	rec := g5Record(id, harness.BundlePaths().FakeService, true, args...)
	rec["capabilities"] = []string{"manifest"}
	rec["working_dir"] = dir
	return rec
}

type g5LogEntry struct {
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"`
	Code    int    `json:"code"`
	Method  string `json:"method"`
	Path    string `json:"path"`
}

func g5ReadCallLog(t *testing.T, path string) []g5LogEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening the call log: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []g5LogEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e g5LogEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("call log line is not JSON: %v\n%s", err, sc.Text())
		}
		out = append(out, e)
	}
	return out
}

func TestServiceManifestActionAndConfig(t *testing.T) {
	t.Parallel()
	dir := g5Dir(t)
	cfg := filepath.Join(dir, "config.json")
	i := harness.Start(t, harness.Options{
		Settings: g5Settings(t, g5FakeRecord("acme-fake", dir, "--config", cfg)),
	})
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"service_id": "acme-fake", "status": "ok"}}, g5Deadline)

	act := i.MustCLI("service", "action", "--id", "acme-fake", "--action", "ping", "--json")
	var actOut struct {
		ServiceID string `json:"service_id"`
		ActionID  string `json:"action_id"`
		OK        bool   `json:"ok"`
	}
	act.JSON(t, &actOut)
	if !actOut.OK || actOut.ActionID != "ping" || actOut.ServiceID != "acme-fake" {
		t.Fatalf("service action printed %+v", actOut)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.action", Trace: act.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-fake", "action_id": "ping"}})
	pinged := false
	for _, e := range g5ReadCallLog(t, filepath.Join(dir, "acme-fake.log")) {
		pinged = pinged || (e.Kind == "request" && e.Method == http.MethodPost && e.Path == "/fakesvc/ping")
	}
	if !pinged {
		t.Fatalf("the fake's call log holds no POST /fakesvc/ping")
	}

	read := i.MustCLI("service", "config", "--id", "acme-fake", "--json")
	var readOut struct {
		ServiceID string `json:"service_id"`
		Text      string `json:"text"`
	}
	read.JSON(t, &readOut)
	disk, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("reading the config file: %v", err)
	}
	if readOut.ServiceID != "acme-fake" || readOut.Text != string(disk) {
		t.Fatalf("service config read %+v, the file holds %q", readOut, disk)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.config.get", Trace: read.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-fake"}})

	const next = "{\"greeting\": \"hello\"}\n"
	save := i.CLIWith(harness.CLIOpts{Stdin: []byte(next)}, "service", "config", "--id", "acme-fake", "--set", "-", "--json")
	if save.Code != 0 {
		t.Fatalf("service config --set exited %d\nstderr: %s", save.Code, save.Stderr)
	}
	var saveOut struct {
		Restarted bool `json:"restarted"`
	}
	save.JSON(t, &saveOut)
	if !saveOut.Restarted {
		t.Fatalf("service config --set printed restarted false for a running service")
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.config.save", Trace: save.Trace, Fields: map[string]any{"status": "ok", "service_id": "acme-fake", "restarted": true}})
	if disk, err = os.ReadFile(cfg); err != nil || string(disk) != next {
		t.Fatalf("config file holds %q (err %v), want %q", disk, err, next)
	}
}

func TestServiceLogPath(t *testing.T) {
	t.Parallel()
	const marker = "acme-log-marker"
	i := harness.Start(t, harness.Options{
		Settings: g5Settings(t, g5Record("acme-logger", "/bin/sh", true, "-c", "echo "+marker+"; exec /bin/sleep 3600")),
	})
	g5WaitRunning(t, i, "acme-logger", time.Time{})
	// A stop returns after the process is reaped and its output is drained.
	i.MustCLI("service", "stop", "--id", "acme-logger")
	logs := g5ReadStatus(t, i).Paths.Logs
	data, err := os.ReadFile(filepath.Join(logs, "acme-logger.log"))
	if err != nil {
		t.Fatalf("reading the service log under paths.logs: %v", err)
	}
	if !strings.Contains(string(data), marker) {
		t.Fatalf("the service log lacks the marker the service printed: %q", data)
	}
}

func TestBridgeHelloAndManifestIdentity(t *testing.T) {
	t.Parallel()
	dir := g5Dir(t)
	i := harness.Start(t, harness.Options{
		Settings: g5Settings(t,
			g5FakeRecord("acme-fake", dir),
			g5FakeRecord("acme-claim", dir, "--claim-id", "relaysessions")),
	})

	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"service_id": "acme-fake", "status": "ok"}}, g5Deadline)
	requireEvent(t, i, harness.EventQuery{Key: "bridge.hello", Fields: map[string]any{"status": "ok", "service_id": "acme-fake"}})

	// The service claiming another's id says Hello as itself and is refused
	// the manifest.
	i.WaitEvent(harness.EventQuery{Key: "bridge.hello", Fields: map[string]any{"status": "ok", "service_id": "acme-claim"}}, g5Deadline)
	i.WaitEvent(harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"status": "denied"}}, g5Deadline)
	if got := i.Events(harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"status": "ok", "service_id": "acme-claim"}}); len(got) != 0 {
		t.Fatalf("a manifest under the claiming service's own id registered")
	}

	wrong := i.BridgeSend(map[string]any{"type": "Hello", "name": "acme-fake", "token": strings.Repeat("0", 64)})
	if wrong.Type != "Error" || wrong.Code != -32001 {
		t.Fatalf("a Hello with a wrong secret answered %s code %d, want Error -32001", wrong.Type, wrong.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "bridge.hello", Trace: wrong.Trace, Fields: map[string]any{"status": "denied"}})

	forged := i.BridgeSend(map[string]any{"type": "RegisterManifest", "arguments": map[string]any{
		"serviceId":      "relaysessions",
		"manifest":       map[string]any{"routes": []string{"/acme-forged/"}},
		"internalSocket": filepath.Join(dir, "forged.sock"),
		"internalToken":  "acme-forged-token",
	}})
	if forged.Type != "Error" || forged.Code != -32001 {
		t.Fatalf("a RegisterManifest from a process with no identity answered %s code %d, want Error -32001", forged.Type, forged.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "service.manifest.register", Trace: forged.Trace, Fields: map[string]any{"status": "denied"}})
}
