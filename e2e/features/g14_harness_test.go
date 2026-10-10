package features

import (
	"encoding/json"
	"flag"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

var readOnly = []harness.CredentialSpec{{Name: "reader", Classes: []string{"read"}}}

var approveGrant = map[string]harness.Outcome{"project.grant": harness.OutcomeApprove}

type project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// createProject runs `relay project create` and returns the project the CLI
// printed. The instance must approve project.grant.
func createProject(t *testing.T, i *harness.Instance, name string) (project, harness.Result) {
	t.Helper()
	dir := filepath.Join(i.Home, "work", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	r := i.MustCLI("project", "create", "--name", name, "--path", dir, "--json")
	var p project
	r.JSON(t, &p)
	return p, r
}

func listProjects(t *testing.T, i *harness.Instance) []project {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/projects", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/projects answered %d", resp.Status)
	}
	var ps []project
	resp.JSON(t, &ps)
	return ps
}

func requireEvent(t *testing.T, i *harness.Instance, q harness.EventQuery) harness.Event {
	t.Helper()
	got := i.Events(q)
	if len(got) == 0 {
		t.Fatalf("no %s event matched trace %q fields %v", q.Key, q.Trace, q.Fields)
	}
	return got[0]
}

func TestServeWritesReadyFile(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{})

	r := i.ReloadReady()
	if r.Schema != 1 || r.PID <= 0 {
		t.Fatalf("ready.json schema %d pid %d, want schema 1 and a pid", r.Schema, r.PID)
	}
	if r.PID != i.Ready.PID {
		t.Fatalf("ready.json pid %d differs from the ready line's %d", r.PID, i.Ready.PID)
	}
	if r.Listeners["api"] == "" || r.Sockets["bridge"] == "" {
		t.Fatalf("ready.json lacks listeners.api or sockets.bridge: %+v", r)
	}
	ev := requireEvent(t, i, harness.EventQuery{Key: "server.ready", Fields: map[string]any{"status": "ok"}})
	if ev.Str("ready_file") == "" {
		t.Fatalf("server.ready has no ready_file: %v", ev)
	}
	if pid, _ := ev["pid"].(float64); int(pid) != r.PID {
		t.Fatalf("server.ready pid %v, ready.json pid %d", ev["pid"], r.PID)
	}

	second := i.CLI("serve")
	if second.Code != 1 || len(second.Stdout) != 0 {
		t.Fatalf("a second serve on the same dir exited %d with %d stdout bytes, want exit 1 and none", second.Code, len(second.Stdout))
	}

	i.Stop()
	if _, err := os.Stat(filepath.Join(i.ConfigDir, "ready.json")); !os.IsNotExist(err) {
		t.Fatalf("ready.json still exists after SIGTERM (stat error: %v)", err)
	}
}

func TestClientWithoutServerFails(t *testing.T) {
	t.Parallel()
	dir := harness.ScratchDir(t)
	res := harness.RunAt(t, dir, "doors", "--json")
	if res.Code != 1 {
		t.Fatalf("doors against a dir with no server exited %d, want 1", res.Code)
	}
	if len(res.Stdout) != 0 {
		t.Fatalf("a failing --json verb printed %d stdout bytes, want none", len(res.Stdout))
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the client created %s (stat error: %v)", dir, err)
	}
}

func TestVerbReachesOnlyItsInstance(t *testing.T) {
	t.Parallel()
	a := harness.Start(t, harness.Options{})
	b := harness.Start(t, harness.Options{})
	for _, i := range []*harness.Instance{a, b} {
		var st struct {
			Paths struct {
				Config string `json:"config"`
			} `json:"paths"`
		}
		i.MustCLI("status", "--json").JSON(t, &st)
		if st.Paths.Config != i.ConfigDir {
			t.Fatalf("status against %s reported config %q", i.ConfigDir, st.Paths.Config)
		}
	}
}

func TestListenerAddressesFromSettings(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Settings: map[string]json.RawMessage{
		"remote": json.RawMessage(`{"enabled":true,"listen":"127.0.0.1:0","enrolment_requests":true,"enrolment_listen":"127.0.0.1:0"}`),
	}})
	var out struct {
		Listeners map[string]string `json:"listeners"`
	}
	data, err := os.ReadFile(filepath.Join(i.ConfigDir, "ready.json"))
	if err != nil {
		t.Fatalf("reading ready.json: %v", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decoding ready.json: %v", err)
	}
	seen := map[string]string{}
	for _, k := range []string{"api", "model", "remote", "enrolment"} {
		addr := out.Listeners[k]
		if addr == "" {
			t.Fatalf("ready.json has no listeners.%s: %v", k, out.Listeners)
		}
		host, port, err := splitHostPort(addr)
		if err != nil || host != "127.0.0.1" || port == 0 {
			t.Fatalf("listeners.%s = %q, want 127.0.0.1 and a bound port", k, addr)
		}
		if other, dup := seen[addr]; dup {
			t.Fatalf("listeners.%s and listeners.%s share %s", k, other, addr)
		}
		seen[addr] = k
	}
	var show struct {
		Enabled            bool   `json:"enabled"`
		Listen             string `json:"listen"`
		EnrolmentRequests  bool   `json:"enrolment_requests"`
		EnrolmentEffective string `json:"enrolment_effective"`
	}
	i.MustCLI("remote", "show", "--json").JSON(t, &show)
	if !show.Enabled || show.Listen != "127.0.0.1:0" || !show.EnrolmentRequests {
		t.Fatalf("remote show reports %+v, want the configured remote block", show)
	}
}

func TestDoorsListsLiveCatalogue(t *testing.T) {
	t.Parallel()
	type door struct {
		Kind      string   `json:"kind"`
		Name      string   `json:"name"`
		Listeners []string `json:"listeners"`
	}
	type doc struct {
		Schema   int    `json:"schema"`
		Headless bool   `json:"headless"`
		Doors    []door `json:"doors"`
	}
	has := func(d doc, kind, name string) bool {
		for _, x := range d.Doors {
			if x.Kind == kind && x.Name == name {
				return true
			}
		}
		return false
	}

	t.Run("default", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{})
		var d doc
		i.MustCLI("doors", "--json").JSON(t, &d)
		if d.Schema != 1 || !d.Headless {
			t.Fatalf("doors schema %d headless %v, want 1 and true", d.Schema, d.Headless)
		}
		if !has(d, "cli", "relay debug clock") {
			t.Fatalf("the test build's doors lack cli relay debug clock")
		}
		if !has(d, "http", "POST /relay/login/verify") {
			t.Fatalf("an instance with a TCP listener lacks POST /relay/login/verify")
		}
	})

	t.Run("no_tcp_listener", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{Settings: map[string]json.RawMessage{"api": json.RawMessage(`{}`)}})
		var d doc
		i.MustCLI("doors", "--json").JSON(t, &d)
		for _, x := range d.Doors {
			for _, l := range x.Listeners {
				if l == "tcp" {
					t.Fatalf("door %s lists a tcp listener on an instance with none", x.Name)
				}
			}
		}
		if has(d, "http", "POST /relay/login/verify") {
			t.Fatalf("an instance with no TCP listener lists the login verify route")
		}
	})
}

func TestPresenceOutcomeFile(t *testing.T) {
	t.Parallel()
	create := func(i *harness.Instance, trace string) harness.Result {
		return i.CLIWith(harness.CLIOpts{Trace: trace}, "project", "create", "--name", "Acme", "--path", i.Home, "--json")
	}

	t.Run("approve", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{Presence: approveGrant, Credentials: readOnly})
		tr := harness.NewTrace(t)
		if r := create(i, tr); r.Code != 0 {
			t.Fatalf("approved create exited %d", r.Code)
		}
		requireEvent(t, i, harness.EventQuery{Key: "project.create", Trace: tr, Fields: map[string]any{"status": "ok"}})
		requireEvent(t, i, harness.EventQuery{Key: "debug.presence.answer", Trace: tr, Fields: map[string]any{"gated_op": "project.grant", "answer": "approve", "source": "file"}})
		found := false
		for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "ok"}) {
			if row["method"] == "project.grant" && row["presence_approver"] == "testapprover" {
				found = true
			}
		}
		if !found {
			t.Fatalf("no control_decision ok row with presence_approver testapprover for project.grant")
		}
	})

	t.Run("deny", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{
			Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeDeny},
			Credentials: readOnly,
		})
		tr := harness.NewTrace(t)
		if r := create(i, tr); r.Code == 0 {
			t.Fatalf("a denied create exited 0")
		}
		requireEvent(t, i, harness.EventQuery{Key: "project.create", Trace: tr, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
		if n := len(listProjects(t, i)); n != 0 {
			t.Fatalf("a denied create left %d projects", n)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{Presence: map[string]harness.Outcome{"project.grant": harness.OutcomeTimeout}})
		tr := harness.NewTrace(t)
		p := i.StartCLI(harness.CLIOpts{Trace: tr, Deadline: 2 * time.Minute}, "project", "create", "--name", "Acme", "--path", i.Home, "--json")
		i.WaitEvent(harness.EventQuery{Key: "debug.presence.answer", Trace: tr, Fields: map[string]any{"gated_op": "project.grant", "answer": "timeout"}}, 60*time.Second)
		p.Signal(syscall.SIGTERM)
		if r := p.Wait(); r.Code == 0 {
			t.Fatalf("a killed create exited 0")
		}
		i.WaitEvent(harness.EventQuery{Key: "project.create", Trace: tr, Fields: map[string]any{"status": "denied", "reason": "presence_timeout"}}, 60*time.Second)
	})

	t.Run("no_console_session", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{Presence: approveGrant, NoConsoleSession: true})
		tr := harness.NewTrace(t)
		if r := create(i, tr); r.Code == 0 {
			t.Fatalf("a create with no console session exited 0")
		}
		requireEvent(t, i, harness.EventQuery{Key: "project.create", Trace: tr, Fields: map[string]any{"status": "denied", "reason": "presence_no_session"}})
	})
}

func TestPresenceRefusesUnlistedOp(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: map[string]harness.Outcome{"acme.unknown": harness.OutcomeApprove}, Credentials: readOnly})
	tr := harness.NewTrace(t)
	r := i.CLIWith(harness.CLIOpts{Trace: tr}, "project", "create", "--name", "Acme", "--path", i.Home, "--json")
	if r.Code == 0 {
		t.Fatalf("create exited 0 under an invalid outcome file")
	}
	// Whether the approver is asked depends on the caller's console session: a
	// caller with one gets presence_refused and an error/invalid answer event;
	// one without gets presence_no_session and no answer event.
	ev := requireAnyEvent(t, i, tr, "project.create", "presence_refused", "presence_no_session")
	if ev.Str("status") != "denied" {
		t.Fatalf("project.create status %q, want denied", ev.Str("status"))
	}
	for _, a := range i.Events(harness.EventQuery{Key: "debug.presence.answer", Trace: tr}) {
		if a.Str("status") != "error" || a.Str("reason") != "invalid" {
			t.Fatalf("debug.presence.answer is %s/%s, want error/invalid", a.Str("status"), a.Str("reason"))
		}
	}
	if n := len(listProjects(t, i)); n != 0 {
		t.Fatalf("the refused create left %d projects", n)
	}
}

func TestPresenceRefusesOpsNotListed(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveGrant})
	tr := harness.NewTrace(t)
	r := i.CLIWith(harness.CLIOpts{Trace: tr}, "credential", "mint", "--name", "acme", "--class", "read")
	if r.Code == 0 {
		t.Fatalf("credential mint exited 0 though only project.grant is listed")
	}
	requireEvent(t, i, harness.EventQuery{Key: "credential.mint", Trace: tr, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	found := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		if row["method"] == "credential.mint" && row["presence_approver"] == "testapprover" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no control_decision denied row with presence_approver testapprover for credential.mint")
	}
}

func TestKeychainProviderFaults(t *testing.T) {
	t.Parallel()
	sealStatus := func(t *testing.T, i *harness.Instance) string {
		var st struct {
			SealStatus string `json:"seal_status"`
		}
		i.MustCLI("status", "--json").JSON(t, &st)
		return st.SealStatus
	}

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{KeychainFault: harness.FaultNone})
		if s := sealStatus(t, i); s != "" {
			t.Fatalf("seal_status %q with no fault, want empty", s)
		}
	})
	for _, f := range []harness.Fault{harness.FaultLocked, harness.FaultSlow} {
		t.Run(string(f), func(t *testing.T) {
			t.Parallel()
			i := harness.Start(t, harness.Options{KeychainFault: f, BootDeadline: 90 * time.Second})
			i.WaitEvent(harness.EventQuery{Key: "debug.keychain.fault", Fields: map[string]any{"fault": string(f)}}, 60*time.Second)
			if sealStatus(t, i) == "" {
				t.Fatalf("seal_status is empty under the %s fault", f)
			}
		})
	}
	for _, f := range []harness.Fault{harness.FaultMissing, harness.FaultCorrupt} {
		t.Run(string(f), func(t *testing.T) {
			t.Parallel()
			i := harness.Start(t, harness.Options{})
			i.SetKeychainFault(f)
			i.Restart()
			i.WaitEvent(harness.EventQuery{Key: "debug.keychain.fault", Fields: map[string]any{"fault": string(f)}}, 60*time.Second)
			if sealStatus(t, i) == "" {
				t.Fatalf("seal_status is empty under the %s fault after a healthy start", f)
			}
		})
	}
}

func TestKeychainOpenRefusal(t *testing.T) {
	t.Parallel()

	// A valid store is planted by a healthy first start, so only the file's
	// mode or kind can be the reason for the refusal.
	storeCases := map[string]struct {
		break_  func(t *testing.T, store string)
		restore func(t *testing.T, store string)
	}{
		"store_group_readable": {
			break_:  func(t *testing.T, store string) { mustChmod(t, store, 0o644) },
			restore: func(t *testing.T, store string) { mustChmod(t, store, 0o600) },
		},
		"store_symlink": {
			break_: func(t *testing.T, store string) {
				mustRename(t, store, store+".real")
				if err := os.Symlink(store+".real", store); err != nil {
					t.Fatalf("symlinking %s: %v", store, err)
				}
			},
			restore: func(t *testing.T, store string) {
				if err := os.Remove(store); err != nil {
					t.Fatalf("removing the symlink %s: %v", store, err)
				}
				mustRename(t, store+".real", store)
			},
		},
	}
	for name, c := range storeCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			i := harness.Start(t, harness.Options{})
			i.Stop()
			store := filepath.Join(i.ConfigDir, "test-keychain.json")
			if _, err := os.Lstat(store); err != nil {
				t.Fatalf("a healthy start left no keychain store: %v", err)
			}
			c.break_(t, store)
			if res := i.RestartFails(); res.Code != 1 {
				t.Fatalf("serve exited %d, want 1", res.Code)
			}
			if _, err := os.Stat(filepath.Join(i.ConfigDir, "ready.json")); !os.IsNotExist(err) {
				t.Fatalf("ready.json exists after a refused start (stat error: %v)", err)
			}
			c.restore(t, store)
			i.Restart()
			if _, err := os.Stat(filepath.Join(i.ConfigDir, "ready.json")); err != nil {
				t.Fatalf("ready.json missing after the store was fixed: %v", err)
			}
		})
	}

	t.Run("fault_file_invalid", func(t *testing.T) {
		t.Parallel()
		dir := ""
		res := harness.StartFails(t, harness.Options{PrepareConfigDir: func(d string) {
			dir = d
			if err := os.WriteFile(filepath.Join(d, "test-keychain-fault.json"), []byte(`{"unknown":true}`), 0o600); err != nil {
				t.Fatalf("planting the fault file: %v", err)
			}
		}})
		if res.Code != 1 {
			t.Fatalf("serve exited %d, want 1", res.Code)
		}
		if _, err := os.Stat(filepath.Join(dir, "ready.json")); !os.IsNotExist(err) {
			t.Fatalf("ready.json exists after a refused start (stat error: %v)", err)
		}
	})
}

func mustChmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func mustRename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatalf("renaming %s to %s: %v", from, to, err)
	}
}

func TestClockMovesCredentialExpiry(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: []harness.CredentialSpec{
		{Name: "reader", Classes: []string{"read"}, Expires: time.Now().Add(time.Hour)},
	}})
	get := func() int {
		return i.HTTP(i.Credential("reader")).Do("GET", "/api/projects", nil).Status
	}
	if s := get(); s != 200 {
		t.Fatalf("before the clock moves GET answered %d, want 200", s)
	}
	i.ClockAdvance(2 * time.Hour)
	if s := get(); s != 401 {
		t.Fatalf("after advancing 2h GET answered %d, want 401", s)
	}
	i.ClockSet(time.Now())
	if s := get(); s != 200 {
		t.Fatalf("after setting the clock back GET answered %d, want 200", s)
	}
	requireEvent(t, i, harness.EventQuery{Key: "debug.clock.advance", Fields: map[string]any{"status": "ok"}})
	requireEvent(t, i, harness.EventQuery{Key: "debug.clock.set", Fields: map[string]any{"status": "ok"}})
}

// TestInstancesIsolated boots 32 instances. When the run allows 32 parallel
// tests, the subtests meet at a barrier after boot, so 32 instances are live
// at once; otherwise they run as many at a time as the run allows.
func TestInstancesIsolated(t *testing.T) {
	t.Parallel()
	const n = 32
	barrierOn := parallelism() >= n

	type seen struct {
		dir, config, api string
		pid              int
	}
	var (
		mu      sync.Mutex
		all     []seen
		arrived atomic.Int32
		release = make(chan struct{})
	)
	arrive := func() {
		if arrived.Add(1) == n {
			close(release)
		}
	}

	t.Run("instances", func(t *testing.T) {
		for k := 0; k < n; k++ {
			t.Run("i"+strconv.Itoa(k), func(t *testing.T) {
				t.Parallel()
				var once sync.Once
				if barrierOn {
					defer once.Do(arrive)
				}
				i := harness.Start(t, harness.Options{Presence: approveGrant, Credentials: readOnly})
				mu.Lock()
				all = append(all, seen{i.Dir, i.ConfigDir, i.Ready.Listeners["api"], i.Ready.PID})
				mu.Unlock()
				if barrierOn {
					once.Do(arrive)
					select {
					case <-release:
					case <-time.After(5 * time.Minute):
						t.Fatalf("barrier: only %d of %d instances arrived within 5 minutes", arrived.Load(), n)
					}
				}

				name := "Acme " + strconv.Itoa(k)
				p, r := createProject(t, i, name)
				ev := requireEvent(t, i, harness.EventQuery{Key: "project.create", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
				if ev.Str("project_id") != p.ID || p.ID == "" {
					t.Fatalf("project.create project_id %q, CLI printed id %q", ev.Str("project_id"), p.ID)
				}
				ps := listProjects(t, i)
				if len(ps) != 1 || ps[0].ID != p.ID || ps[0].Name != name {
					t.Fatalf("GET /api/projects lists %+v, want exactly %s", ps, name)
				}
			})
		}
	})

	if len(all) != n {
		t.Fatalf("%d of %d instances booted", len(all), n)
	}
	dirs, apis, pids, configs := map[string]bool{}, map[string]bool{}, map[int]bool{}, map[string]bool{}
	for _, s := range all {
		if dirs[s.dir] || apis[s.api] || pids[s.pid] || configs[s.config] {
			t.Errorf("instance %+v shares a dir, config dir, API address or pid with another", s)
		}
		dirs[s.dir], apis[s.api], pids[s.pid], configs[s.config] = true, true, true, true
	}
	if barrierOn && !t.Failed() {
		t.Logf("barrier: %d instances live", n)
	}
}

func parallelism() int {
	if f := flag.Lookup("test.parallel"); f != nil {
		if v, err := strconv.Atoi(f.Value.String()); err == nil {
			return v
		}
	}
	return runtime.GOMAXPROCS(0)
}

func splitHostPort(addr string) (string, int, error) {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(p)
	return host, port, err
}

// loopbackListener accepts connections on a free loopback port until the test
// ends, so a connect to it succeeds unless something blocks it.
func loopbackListener(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on loopback: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// probeSandbox runs a sandboxed terminal whose command tries one loopback
// connect and returns the terminal's exit code.
func probeSandbox(t *testing.T, i *harness.Instance, projectID string, port int) int {
	t.Helper()
	r := i.MustCLI("terminal", "start", "--project", projectID, "--template", "probe",
		"--extra-arg", "-z", "--extra-arg", "-w", "--extra-arg", "5", "--extra-arg", "127.0.0.1", "--extra-arg", strconv.Itoa(port), "--json")
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	r.JSON(t, &term)
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, 60*time.Second)
	var list struct {
		Terminals []struct {
			ID       string `json:"id"`
			ExitCode *int   `json:"exitCode"`
		} `json:"terminals"`
	}
	i.MustCLI("terminal", "list", "--json").JSON(t, &list)
	for _, x := range list.Terminals {
		if x.ID == term.TerminalID {
			if x.ExitCode == nil {
				// session.exited was seen, and docs/cli.md makes exitCode optional:
				// an ended terminal that reports none exited 0.
				return 0
			}
			return *x.ExitCode
		}
	}
	t.Fatalf("terminal %s is not in terminal list", term.TerminalID)
	return -1
}

func sandboxInstance(t *testing.T, denied string) (*harness.Instance, string) {
	t.Helper()
	i := harness.Start(t, harness.Options{
		Presence: approveGrant,
		Settings: map[string]json.RawMessage{
			"session_sandbox":    json.RawMessage(`{"denied_loopback_ports":` + denied + `}`),
			"terminal_templates": json.RawMessage(`[{"id":"probe","name":"Probe","command":"/usr/bin/nc","sandbox":true}]`),
		},
	})
	i.WaitSessionHost(60 * time.Second)
	dir := filepath.Join(i.Home, "work", "acme")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	body, _ := json.Marshal(map[string]any{"name": "Acme probe", "path": dir, "allowed_templates": []string{"probe"}})
	var p project
	i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json").JSON(t, &p)
	return i, p.ID
}

func TestSandboxDeniedLoopbackPorts(t *testing.T) {
	t.Parallel()
	denied, allowed := loopbackListener(t), loopbackListener(t)

	t.Run("list_replaces_default", func(t *testing.T) {
		t.Parallel()
		i, pid := sandboxInstance(t, "["+strconv.Itoa(denied)+"]")
		if code := probeSandbox(t, i, pid, allowed); code != 0 {
			t.Fatalf("connect to a port not on the list exited %d, want 0", code)
		}
		if code := probeSandbox(t, i, pid, denied); code == 0 {
			t.Fatalf("connect to a listed port exited 0, want a refusal")
		}
	})

	t.Run("own_api_port_always_denied", func(t *testing.T) {
		t.Parallel()
		i, pid := sandboxInstance(t, "[]")
		_, port, _ := splitHostPort(i.Ready.Listeners["api"])
		if code := probeSandbox(t, i, pid, port); code == 0 {
			t.Fatalf("connect to the instance's own API port exited 0, want a refusal")
		}
	})

	t.Run("bad_entry_refuses_launch", func(t *testing.T) {
		t.Parallel()
		i, pid := sandboxInstance(t, "[70000]")
		tr := harness.NewTrace(t)
		r := i.CLIWith(harness.CLIOpts{Trace: tr}, "terminal", "start", "--project", pid, "--template", "probe", "--json")
		if r.Code == 0 {
			t.Fatalf("terminal start exited 0 with an out-of-range denied port")
		}
		requireEvent(t, i, harness.EventQuery{Key: "session.launch", Trace: tr, Fields: map[string]any{"status": "denied"}})
	})
}

// requireAnyEvent returns the trace's event of the key whose reason is one of reasons.
func requireAnyEvent(t *testing.T, i *harness.Instance, trace, key string, reasons ...string) harness.Event {
	t.Helper()
	for _, ev := range i.Events(harness.EventQuery{Key: key, Trace: trace}) {
		for _, r := range reasons {
			if ev.Str("reason") == r {
				return ev
			}
		}
	}
	t.Fatalf("no %s event on trace %s with reason in %v", key, trace, reasons)
	return nil
}

func TestSSHStubFile(t *testing.T) {
	t.Parallel()

	invalid := map[string]struct {
		write func(t *testing.T, path string)
	}{
		"unknown_key": {
			write: func(t *testing.T, path string) {
				writeSeamFile(t, path, `{"command":"/bin/sh","unknown":true}`, 0o600)
			},
		},
		"relative_command": {
			write: func(t *testing.T, path string) {
				writeSeamFile(t, path, `{"command":"ssh"}`, 0o600)
			},
		},
		"not_executable": {
			write: func(t *testing.T, path string) {
				plain := filepath.Join(filepath.Dir(path), "plain-file")
				writeSeamFile(t, plain, "x", 0o600)
				writeSeamFile(t, path, `{"command":"`+plain+`"}`, 0o600)
			},
		},
		"group_readable": {
			write: func(t *testing.T, path string) {
				writeSeamFile(t, path, `{"command":"/bin/sh"}`, 0o644)
			},
		},
		"symlink": {
			write: func(t *testing.T, path string) {
				real := path + ".real"
				writeSeamFile(t, real, `{"command":"/bin/sh"}`, 0o600)
				if err := os.Symlink(real, path); err != nil {
					t.Fatalf("symlinking %s: %v", path, err)
				}
			},
		},
	}
	for name, c := range invalid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := ""
			res := harness.StartFails(t, harness.Options{PrepareConfigDir: func(d string) {
				dir = d
				c.write(t, filepath.Join(d, "test-ssh.json"))
			}})
			if res.Code != 1 {
				t.Fatalf("serve exited %d, want 1\nstderr: %s", res.Code, res.Stderr)
			}
			if !strings.Contains(string(res.Stderr), filepath.Join(dir, "test-ssh.json")) {
				t.Fatalf("stderr does not name test-ssh.json:\n%s", res.Stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, "ready.json")); !os.IsNotExist(err) {
				t.Fatalf("ready.json exists after a refused start (stat error: %v)", err)
			}
		})
	}

	t.Run("valid_file_installs_stub", func(t *testing.T) {
		t.Parallel()
		stub := harness.BundlePaths().FakeSSH
		i := harness.Start(t, harness.Options{PrepareConfigDir: func(d string) {
			writeSeamFile(t, filepath.Join(d, "test-ssh.json"), `{"command":"`+stub+`"}`, 0o600)
		}})
		i.WaitEvent(harness.EventQuery{Key: "debug.ssh.stub", Fields: map[string]any{"command": stub}}, 60*time.Second)
	})

	t.Run("absent_file_is_real_ssh", func(t *testing.T) {
		t.Parallel()
		i := harness.Start(t, harness.Options{})
		if got := i.Events(harness.EventQuery{Key: "debug.ssh.stub"}); len(got) != 0 {
			t.Fatalf("debug.ssh.stub written with no test-ssh.json: %v", got)
		}
	})
}

func writeSeamFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}
