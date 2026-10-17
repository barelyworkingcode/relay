package features

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

// The outside-session tests run real neutral processes named claude, with the
// registry and transcript files Claude Code keeps under ~/.claude, against the
// console and one Unix host on the ssh stub. The console reads the instance's
// HOME; the stub host reads the remote root.

const (
	osBound     = 30 * time.Second // upper bound for every poll below; the signal is named at each call
	osHookBound = 5 * time.Second  // the hook-to-snapshot promise
	osProbeStep = 15 * time.Second // the documented probe interval
	osListPath  = "/api/chief-of-staff/outside-sessions"
)

type osRow struct {
	ID        string  `json:"id"`
	HostID    *string `json:"hostId"`
	ProjectID *string `json:"projectId"`
	Machine   string  `json:"machine"`
	Directory string  `json:"directory"`
	Branch    string  `json:"branch"`
	State     string  `json:"state"`
	Source    string  `json:"source"`
	Outside   bool    `json:"outside"`
	ReadOnly  bool    `json:"readOnly"`

	TmuxSession *string `json:"tmuxSession"`
	Viewable    bool    `json:"viewable"`
	ViewReason  string  `json:"viewReason"`
}

type osSnap struct {
	Revision uint64  `json:"revision"`
	Sessions []osRow `json:"sessions"`
	Machines []struct {
		Machine     string `json:"machine"`
		Status      string `json:"status"`
		LastProbeAt string `json:"lastProbeAt"`
	} `json:"machines"`
}

type osRig struct {
	t      *testing.T
	i      *harness.Instance
	bin    string // a copy of sleep named claude, so the probe counts it as Claude
	remote string // the stub host's HOME and project root; "" without a host
}

func osStart(t *testing.T, host bool, mut ...func(*harness.Options)) *osRig {
	t.Helper()
	i := cosStart(t, append([]func(*harness.Options){func(o *harness.Options) { o.SSHStub = host }}, mut...)...)
	src, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatalf("reading /bin/sleep: %v", err)
	}
	r := &osRig{t: t, i: i, bin: filepath.Join(t.TempDir(), "claude")}
	if err := os.WriteFile(r.bin, src, 0o755); err != nil {
		t.Fatalf("writing %s: %v", r.bin, err)
	}
	machines := 1
	if host {
		r.remote = r.hostProject()
		machines = 2
	}
	// Ready signal: every machine has completed one probe, so a later change
	// of lastProbeAt is a probe that started after the files were planted.
	osSubscribe(t, i).until(osBound, func(s osSnap) bool {
		n := 0
		for _, m := range s.Machines {
			if m.LastProbeAt != "" && m.Status == "reachable" {
				n++
			}
		}
		return n == machines
	})
	return r
}

// hostProject registers the stub host with one project under the remote root,
// by its unresolved path: the probe reports a process's resolved working
// folder, and the temp root is itself reached through a symlink.
func (r *osRig) hostProject() string {
	t := r.t
	t.Helper()
	root, err := filepath.EvalSymlinks(r.i.RemoteRoot())
	if err != nil {
		t.Fatalf("resolving the remote root: %v", err)
	}
	dir := filepath.Join(r.i.RemoteRoot(), "boxproj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	hr := r.i.HTTP(r.i.Credential("configurer")).Do("POST", "/api/hosts", map[string]any{"name": "testbox", "target": "acme@testbox"})
	if hr.Status != http.StatusCreated {
		t.Fatalf("POST /api/hosts answered %d, want 201: %s", hr.Status, hr.Body)
	}
	var h struct {
		ID string `json:"id"`
	}
	hr.JSON(t, &h)
	body, _ := json.Marshal(map[string]any{"name": "Acme Box", "path": dir, "host_id": h.ID, "allowed_templates": []string{"claude-code"}})
	if res := r.i.CLIWith(harness.CLIOpts{Stdin: body}, "project", "create", "--file", "-", "--json"); res.Code != 0 {
		t.Fatalf("project create for the host project exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	return root
}

type osSub struct {
	t  *testing.T
	ws *harness.WSConn
}

func osSubscribe(t *testing.T, i *harness.Instance) *osSub {
	t.Helper()
	return &osSub{t: t, ws: i.WebSocket("/ws/outside-sessions", i.Credential("cos"), harness.ReqOpts{Header: g1tScope})}
}

// until reads whole snapshots (sent on open and on each change) until f holds;
// it fails t if none arrives before bound.
func (s *osSub) until(bound time.Duration, f func(osSnap) bool) osSnap {
	s.t.Helper()
	end := time.Now().Add(bound)
	for {
		var snap osSnap
		if err := json.Unmarshal(s.ws.Next(time.Until(end)), &snap); err != nil {
			s.t.Fatalf("decoding a snapshot: %v", err)
		}
		if f(snap) {
			return snap
		}
	}
}

func (s osSnap) at(dir string) *osRow {
	for k := range s.Sessions {
		if s.Sessions[k].Directory == dir {
			return &s.Sessions[k]
		}
	}
	return nil
}

// probe moves the clock one probe interval and returns the snapshot once every
// machine has completed a probe after the move.
func (r *osRig) probe() osSnap {
	r.t.Helper()
	sub := osSubscribe(r.t, r.i)
	before := map[string]string{}
	for _, m := range sub.until(osBound, func(osSnap) bool { return true }).Machines { // the snapshot on open, before the move
		before[m.Machine] = m.LastProbeAt
	}
	r.i.ClockAdvance(osProbeStep)
	return sub.until(osBound, func(s osSnap) bool {
		for _, m := range s.Machines {
			if m.LastProbeAt == before[m.Machine] {
				return false
			}
		}
		return true
	})
}

type osProc struct {
	pid  int
	dir  string
	home string
	sid  string
	exit chan struct{} // closed after Wait returns
}

// spawn starts a live claude in a fresh git folder under root and registers it
// in home's session registry, as Claude Code does.
func (r *osRig) spawn(home, root, name string) *osProc {
	t := r.t
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolving %s: %v", dir, err)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "-b", "acme-branch").CombinedOutput(); err != nil {
		t.Fatalf("git init in %s: %v\n%s", dir, err, out)
	}
	cmd := exec.Command(r.bin, "600")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", r.bin, err)
	}
	p := &osProc{pid: cmd.Process.Pid, dir: dir, home: home, sid: "acme-" + name, exit: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.exit) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-p.exit })
	reg, _ := json.Marshal(map[string]any{"pid": p.pid, "sessionId": p.sid, "cwd": dir, "startedAt": time.Now().UnixMilli(), "name": "Acme " + name})
	osWrite(t, filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", p.pid)), reg)
	return p
}

func osWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("renaming %s: %v", tmp, err)
	}
}

// transcript replaces the session transcript with lines.
func (p *osProc) transcript(t *testing.T, lines ...string) {
	t.Helper()
	folder := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(p.dir, "-")
	osWrite(t, filepath.Join(p.home, ".claude", "projects", folder, p.sid+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"))
}

func osLine(kind string, message map[string]any) string {
	b, _ := json.Marshal(map[string]any{"type": kind, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "message": message})
	return string(b)
}

func osPrompt(text string) string {
	return osLine("user", map[string]any{"role": "user", "content": text})
}

func osAnswer(text string) string {
	return osLine("assistant", map[string]any{"role": "assistant", "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": text}}})
}

func osToolRequest() string {
	return osLine("assistant", map[string]any{"role": "assistant", "stop_reason": "tool_use", "content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{}}}})
}

// AC1: discovery needs no hooks, on the console and on a host project.
func TestOutsideDiscoveryWithoutHooks(t *testing.T) {
	t.Parallel()
	r := osStart(t, true)
	root := t.TempDir()
	c := r.spawn(r.i.Home, root, "console")
	h := r.spawn(r.remote, filepath.Join(r.remote, "boxproj"), "host")
	c.transcript(t, osPrompt("acme hello"), osAnswer("acme hi"))
	h.transcript(t, osPrompt("acme hello"), osAnswer("acme hi"))

	s := r.probe()
	cr, hr := s.at(c.dir), s.at(h.dir)
	if cr == nil || cr.HostID != nil || cr.Machine != "Console" {
		t.Fatalf("console row = %+v, want a row for %s on machine Console with no hostId; snapshot %+v", cr, c.dir, s)
	}
	if hr == nil || hr.HostID == nil || hr.Machine != "testbox" {
		t.Fatalf("host row = %+v, want a row for %s on machine testbox with a hostId; snapshot %+v", hr, h.dir, s)
	}
	for _, home := range []string{r.i.Home, r.remote} {
		if _, err := os.Stat(filepath.Join(home, ".claude", "relay-outside-reports")); err == nil {
			t.Fatalf("%s holds a report spool, so the discovery was not hookless", home)
		}
	}
}

// AC2: a row names machine, folder and branch, and shows the three states.
func TestOutsideRowFieldsAndStates(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	// A pathless remote project contains no folder, so no row may name it.
	pathless, _ := json.Marshal(map[string]any{"name": "acme-profile", "kind": "remote"})
	if res := r.i.CLIWith(harness.CLIOpts{Stdin: pathless}, "project", "create", "--file", "-", "--json"); res.Code != 0 {
		t.Fatalf("project create for the pathless remote project exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	root := t.TempDir()
	want := map[string]string{"busy": "running", "waiting": "asking", "done": "idle"}
	procs := map[string]*osProc{}
	for name := range want {
		procs[name] = r.spawn(r.i.Home, root, name)
	}
	procs["busy"].transcript(t, osPrompt("acme work"))
	procs["waiting"].transcript(t, osPrompt("acme work"), osToolRequest())
	procs["done"].transcript(t, osPrompt("acme work"), osAnswer("acme done"))

	s := r.probe()
	for name, state := range want {
		row := s.at(procs[name].dir)
		if row == nil {
			t.Fatalf("no row for %s in %+v", procs[name].dir, s)
		}
		if row.State != state || row.Branch != "acme-branch" || row.Machine != "Console" || !row.Outside || !row.ReadOnly {
			t.Fatalf("row for %s = %+v, want state %s, branch acme-branch, machine Console, outside and readOnly", name, row, state)
		}
		if row.ProjectID != nil {
			t.Fatalf("row for %s has projectId %q, want null: its folder is under no project", name, *row.ProjectID)
		}
	}
}

func (r *osRig) detail(id string) harness.Response {
	return cosDo(r.t, r.i, "GET", osListPath+"/"+id, nil)
}

// AC3: scoped status reads the live transcript at request time.
func TestOutsideDetailReadsLatestTurn(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	p := r.spawn(r.i.Home, t.TempDir(), "turns")
	p.transcript(t, osPrompt("acme one"), osAnswer("acme first answer"))
	row := r.probe().at(p.dir)
	if row == nil {
		t.Fatalf("no row for %s", p.dir)
	}
	turn := func() string {
		resp := r.detail(row.ID)
		if resp.Status != http.StatusOK {
			t.Fatalf("detail answered %d, want 200: %s", resp.Status, resp.Body)
		}
		var d struct {
			LastTurn struct {
				Text     string `json:"text"`
				Complete bool   `json:"complete"`
			} `json:"lastTurn"`
			LastExcerpt string `json:"lastExcerpt"`
		}
		resp.JSON(t, &d)
		if d.LastExcerpt != d.LastTurn.Text || !d.LastTurn.Complete {
			t.Fatalf("detail = %s, want a complete turn whose excerpt is its text", resp.Body)
		}
		return d.LastTurn.Text
	}
	if got := turn(); got != "acme first answer" {
		t.Fatalf("latest turn = %q, want the first answer", got)
	}
	// The write is renamed into place before the read; no probe runs between.
	p.transcript(t, osPrompt("acme one"), osAnswer("acme first answer"), osPrompt("acme two"), osAnswer("acme second answer"))
	if got := turn(); got != "acme second answer" {
		t.Fatalf("latest turn after a transcript write = %q, want the second answer", got)
	}
	resp := r.detail("outside:unknown")
	if resp.Status != http.StatusNotFound || !strings.Contains(string(resp.Body), "outside_session_not_found") {
		t.Fatalf("detail for an unknown id answered %d %s, want 404 outside_session_not_found", resp.Status, resp.Body)
	}
}

// AC4: a hook report reaches the snapshot within five seconds.
func TestOutsideHookReportUpdatesSnapshot(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	// The spool must exist at the probe that finds the row: only then is it drained.
	if err := os.MkdirAll(filepath.Join(r.i.Home, ".claude", "relay-outside-reports"), 0o700); err != nil {
		t.Fatalf("creating the spool: %v", err)
	}
	p := r.spawn(r.i.Home, t.TempDir(), "hooked")
	p.transcript(t, osPrompt("acme one"), osAnswer("acme done"))
	if row := r.probe().at(p.dir); row == nil || row.State != "idle" {
		t.Fatalf("row before the report = %+v, want idle", row)
	}

	sub := osSubscribe(t, r.i)
	script, err := filepath.Abs("../../scripts/outside-session-report.sh")
	if err != nil {
		t.Fatal(err)
	}
	event, _ := json.Marshal(map[string]any{"hook_event_name": "PermissionRequest", "session_id": p.sid, "cwd": p.dir})
	hook := exec.Command("sh", script)
	hook.Env = append(os.Environ(), "HOME="+r.i.Home, "RELAY_OUTSIDE_SPOOL=")
	hook.Stdin = strings.NewReader(string(event))
	if out, err := hook.CombinedOutput(); err != nil {
		t.Fatalf("the report hook failed: %v\n%s", err, out)
	}
	sub.until(osHookBound, func(s osSnap) bool {
		row := s.at(p.dir)
		return row != nil && row.State == "asking" && row.Source == "hook"
	})
}

// osReport runs the report hook with env added to HOME=home.
func osReport(t *testing.T, home string, event map[string]any, env ...string) {
	t.Helper()
	script, err := filepath.Abs("../../scripts/outside-session-report.sh")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(event)
	hook := exec.Command("sh", script)
	hook.Env = append(append(os.Environ(), "HOME="+home), env...)
	hook.Stdin = strings.NewReader(string(body))
	if out, err := hook.CombinedOutput(); err != nil {
		t.Fatalf("the report hook failed: %v\n%s", err, out)
	}
}

// Each relay instance drains only its own spool: a session started with an
// instance's RELAY_OUTSIDE_SPOOL reports to that instance, and a report in
// one instance's spool never reaches another's list.
func TestOutsideReportSpoolPerInstance(t *testing.T) {
	t.Parallel()
	spools := t.TempDir()
	// One machine, one user: both instances share a HOME, so only
	// RELAY_OUTSIDE_SPOOL keeps their spools apart.
	home := t.TempDir()
	type side struct {
		r     *osRig
		spool string
		p     *osProc
	}
	sides := map[string]*side{}
	for _, name := range []string{"a", "b"} {
		spool := filepath.Join(spools, name)
		// The spool must exist at the probe that finds the row: only then is it drained.
		if err := os.Mkdir(spool, 0o700); err != nil {
			t.Fatalf("creating the spool: %v", err)
		}
		r := osStart(t, false, func(o *harness.Options) { o.Env = append(o.Env, "HOME="+home, "RELAY_OUTSIDE_SPOOL="+spool) })
		p := r.spawn(home, t.TempDir(), "spool-"+name)
		p.transcript(t, osPrompt("acme one"), osAnswer("acme done"))
		if row := r.probe().at(p.dir); row == nil || row.State != "idle" {
			t.Fatalf("instance %s row before any report = %+v, want idle", name, row)
		}
		sides[name] = &side{r, spool, p}
	}
	a, b := sides["a"], sides["b"]

	subA, subB := osSubscribe(t, a.r.i), osSubscribe(t, b.r.i)
	// A report naming b's session lands in a's spool, then a's own report.
	osReport(t, home, map[string]any{"hook_event_name": "PermissionRequest", "session_id": b.p.sid, "cwd": b.p.dir}, "RELAY_OUTSIDE_SPOOL="+a.spool)
	osReport(t, home, map[string]any{"hook_event_name": "PermissionRequest", "session_id": a.p.sid, "cwd": a.p.dir}, "RELAY_OUTSIDE_SPOOL="+a.spool)
	subA.until(osHookBound, func(s osSnap) bool {
		row := s.at(a.p.dir)
		return row != nil && row.State == "asking" && row.Source == "hook"
	})
	// One drain pass reads every report present, so a's spool is empty once
	// its own report has shown.
	if left, _ := filepath.Glob(filepath.Join(a.spool, "*.json")); len(left) != 0 {
		t.Fatalf("instance a left %d reports in its spool after its own report showed: %v", len(left), left)
	}

	osReport(t, home, map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": b.p.sid, "cwd": b.p.dir}, "RELAY_OUTSIDE_SPOOL="+b.spool)
	subB.until(osHookBound, func(s osSnap) bool {
		row := s.at(b.p.dir)
		if row != nil && row.State == "asking" {
			t.Fatalf("instance b shows %s asking: the report in instance a's spool reached it", b.p.dir)
		}
		return row != nil && row.State == "running" && row.Source == "hook"
	})
	if _, err := os.Stat(filepath.Join(home, ".claude", "relay-outside-reports")); err == nil {
		t.Fatalf("%s holds the default spool, so a report went past RELAY_OUTSIDE_SPOOL", home)
	}
}

// AC5: a hookless state change shows at the next completed probe.
func TestOutsideHooklessChangeAtNextProbe(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	p := r.spawn(r.i.Home, t.TempDir(), "plain")
	p.transcript(t, osPrompt("acme one"), osAnswer("acme done"))
	if row := r.probe().at(p.dir); row == nil || row.State != "idle" {
		t.Fatalf("row before the change = %+v, want idle", row)
	}
	p.transcript(t, osPrompt("acme one"), osAnswer("acme done"), osPrompt("acme two"))
	if row := r.probe().at(p.dir); row == nil || row.State != "running" || row.Source != "probe" {
		t.Fatalf("row after one probe interval = %+v, want running from source probe", row)
	}
}

// AC6: an ended session leaves the list.
func TestOutsideEndedSessionLeavesList(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	p := r.spawn(r.i.Home, t.TempDir(), "ending")
	p.transcript(t, osPrompt("acme one"), osAnswer("acme done"))
	if r.probe().at(p.dir) == nil {
		t.Fatalf("no row for %s while it runs", p.dir)
	}
	if err := syscall.Kill(p.pid, syscall.SIGKILL); err != nil {
		t.Fatalf("ending the process: %v", err)
	}
	<-p.exit
	if row := r.probe().at(p.dir); row != nil {
		t.Fatalf("row after the process ended = %+v, want none", row)
	}
}

// AC7: an outside id grants no send, kill or start, and ends nothing.
func TestOutsideIDGrantsNoAuthority(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	p := r.spawn(r.i.Home, t.TempDir(), "readonly")
	p.transcript(t, osPrompt("acme one"), osAnswer("acme done"))
	row := r.probe().at(p.dir)
	if row == nil {
		t.Fatalf("no row for %s", p.dir)
	}
	cos := r.i.SocketHTTP(r.i.Credential("cos"))
	if resp := cos.Do("POST", "/api/chief-of-staff/messages", map[string]any{"sessionId": row.ID, "text": "acme refused"}, harness.ReqOpts{Header: g1tScope}); resp.Status < 400 || resp.Status >= 500 {
		t.Fatalf("POST /api/chief-of-staff/messages answered %d, want a 4xx refusal: %s", resp.Status, resp.Body)
	}
	// The unscoped instance credential holds proxy and execute, so these reach
	// the session lookup; the outside id is unknown to it.
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"DELETE", "/api/sessions/" + row.ID, nil, http.StatusNoContent},
		{"POST", "/api/sessions/" + row.ID + "/resume", nil, http.StatusNotFound},
		{"POST", "/api/sessions/" + row.ID + "/message", map[string]any{"text": "acme refused"}, http.StatusNotFound},
	} {
		if resp := cos.Do(c.method, c.path, c.body); resp.Status != c.want {
			t.Fatalf("%s %s answered %d, want %d: %s", c.method, c.path, resp.Status, c.want, resp.Body)
		}
	}
	select {
	case <-p.exit:
		t.Fatalf("the outside process ended after the refused requests")
	default:
	}
	if err := syscall.Kill(p.pid, 0); err != nil {
		t.Fatalf("the outside process is gone after the refused requests: %v", err)
	}
}

// AC8: a relay-managed session is not listed as outside, while an outside one is.
func TestOutsideExcludesManagedSessions(t *testing.T) {
	t.Parallel()
	r := osStart(t, false)
	i := r.i
	pid := agProject(t, i, "acme-managed", nil)
	agAdd(t, i, pid, agEntry{Name: "Researcher", Kind: "claude", Model: "sonnet"})
	ws := i.WebSocket("/ws", i.Credential("cos"), harness.ReqOpts{Header: g1tScope})
	resp := cosDo(t, i, "POST", "/api/chief-of-staff/sessions", map[string]any{"projectId": pid, "prompt": "acme hello", "entry": "Researcher"})
	if resp.Status != http.StatusCreated {
		t.Fatalf("starting a managed session answered %d, want 201: %s", resp.Status, resp.Body)
	}
	var started struct {
		Directory string `json:"directory"`
		SessionID string `json:"sessionId"`
	}
	resp.JSON(t, &started)
	dir, err := filepath.EvalSymlinks(started.Directory)
	if err != nil {
		t.Fatalf("resolving %s: %v", started.Directory, err)
	}
	// Signal: the first turn is done, so the managed process runs.
	g1tAwaitTurnDone(t, ws, started.SessionID)
	managed := &osProc{dir: dir, home: i.Home, sid: "acme-managed"}
	out, err := exec.Command("lsof", "-a", "-d", "cwd", "-Fpn").Output()
	if err != nil {
		t.Fatalf("lsof: %v", err)
	}
	cur := 0
	for _, l := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(l, "p"):
			_, _ = fmt.Sscanf(l, "p%d", &cur)
		case l == "n"+dir && cur > 0:
			if name, _ := exec.Command("ps", "-o", "comm=", "-p", fmt.Sprint(cur)).Output(); filepath.Base(strings.TrimSpace(string(name))) == "claude" {
				managed.pid = cur
			}
		}
	}
	if managed.pid == 0 {
		t.Fatalf("no live claude process has cwd %s", dir)
	}
	// Give the managed process the same registry and transcript evidence an
	// outside one has, so only its managed identity keeps it off the list.
	reg, _ := json.Marshal(map[string]any{"pid": managed.pid, "sessionId": managed.sid, "cwd": dir, "startedAt": time.Now().UnixMilli()})
	osWrite(t, filepath.Join(i.Home, ".claude", "sessions", fmt.Sprintf("%d.json", managed.pid)), reg)
	managed.transcript(t, osPrompt("acme hello"), osAnswer("acme hi"))
	control := r.spawn(i.Home, t.TempDir(), "outside")
	control.transcript(t, osPrompt("acme one"), osAnswer("acme done"))

	s := r.probe()
	if s.at(control.dir) == nil {
		t.Fatalf("the outside control has no row; snapshot %+v", s)
	}
	if row := s.at(dir); row != nil {
		t.Fatalf("the managed session is listed as outside: %+v", row)
	}
}

// Viewability: a claude in a tmux pane is viewable on the console, not on a
// host without show_all_tmux_sessions, and a claude outside tmux says so.
func TestOutsideRowViewability(t *testing.T) {
	t.Parallel()
	r := osStart(t, true)
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("real tmux is required: %v", err)
	}
	// A private socket is deliberate: the default tmux server is shared with
	// everything else on the machine, and its environment is not this test's.
	// The console finds tmux in the instance's ~/.local/bin; the stub host runs
	// tmux with its own TMUX_TMPDIR.
	socket := filepath.Join(t.TempDir(), "tmux.sock")
	t.Cleanup(func() { _ = exec.Command(tmux, "-S", socket, "kill-server").Run() })
	osWrite(t, filepath.Join(r.i.Home, ".local", "bin", "tmux"), []byte("#!/bin/sh\nexec "+tmux+" -S "+socket+" \"$@\"\n"))
	if err := os.Chmod(filepath.Join(r.i.Home, ".local", "bin", "tmux"), 0o755); err != nil {
		t.Fatal(err)
	}
	inTmux := func(home, root, name, tmuxDir string) *osProc {
		p := r.spawn(home, root, name)
		_ = exec.Command("kill", fmt.Sprint(p.pid)).Run() // spawn's process is outside tmux; the pane gets its own
		<-p.exit
		session := fmt.Sprintf("acme-%s-%d", name, os.Getpid())
		run := func(args ...string) ([]byte, error) {
			if tmuxDir == "" {
				args = append([]string{"-S", socket}, args...)
			}
			cmd := exec.Command(tmux, args...)
			cmd.Env = append(os.Environ(), "TMUX=")
			if tmuxDir != "" {
				cmd.Env = append(cmd.Env, "TMUX_TMPDIR="+tmuxDir)
			}
			return cmd.CombinedOutput()
		}
		t.Cleanup(func() { _, _ = run("kill-session", "-t", "="+session) })
		out, err := run("-f", "/dev/null", "new-session", "-d", "-P", "-F", "#{pane_pid}", "-s", session, "-c", p.dir, "exec "+r.bin+" 600")
		if err != nil {
			t.Fatalf("tmux new-session: %v\n%s", err, out)
		}
		p.pid, _ = strconv.Atoi(strings.TrimSpace(string(out)))
		reg, _ := json.Marshal(map[string]any{"pid": p.pid, "sessionId": p.sid, "cwd": p.dir, "startedAt": time.Now().UnixMilli(), "name": "Acme " + name})
		osWrite(t, filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", p.pid)), reg)
		return p
	}
	c := inTmux(r.i.Home, t.TempDir(), "console", "")
	h := inTmux(r.remote, filepath.Join(r.remote, "boxproj"), "host", filepath.Join(r.remote, "tmux"))
	// Orphaned from the test run, which may itself sit inside a tmux pane.
	n := r.spawn(r.i.Home, t.TempDir(), "plain")
	_ = exec.Command("kill", fmt.Sprint(n.pid)).Run()
	<-n.exit
	cmd := exec.Command("sh", "-c", `"$0" 600 >/dev/null 2>&1 & echo $!`, r.bin)
	cmd.Dir = n.dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	n.pid, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	t.Cleanup(func() { _ = syscall.Kill(n.pid, syscall.SIGKILL) })
	reg, _ := json.Marshal(map[string]any{"pid": n.pid, "sessionId": n.sid, "cwd": n.dir, "startedAt": time.Now().UnixMilli(), "name": "Acme plain"})
	osWrite(t, filepath.Join(r.i.Home, ".claude", "sessions", fmt.Sprintf("%d.json", n.pid)), reg)

	s := r.probe()
	for _, w := range []struct {
		p      *osProc
		tmux   bool
		viewOK bool
		reason string
	}{{c, true, true, ""}, {h, true, false, "outside_view_disabled"}, {n, false, false, "not_in_tmux"}} {
		row := s.at(w.p.dir)
		if row == nil || (row.TmuxSession != nil) != w.tmux || row.Viewable != w.viewOK || row.ViewReason != w.reason {
			t.Fatalf("row for %s = %+v, want tmuxSession set %v, viewable %v, viewReason %q; snapshot %+v", w.p.dir, row, w.tmux, w.viewOK, w.reason, s)
		}
	}
}

// Contract: the optional native report spool is bounded and drops events over 64 KiB.
func TestOutsideReportSpoolIsBounded(t *testing.T) {
	t.Parallel()
	script, err := filepath.Abs("../../scripts/outside-session-report.sh")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	run := func(payload string) error {
		hook := exec.Command("sh", script)
		hook.Env = append(os.Environ(), "HOME="+home)
		hook.Stdin = strings.NewReader(payload)
		return hook.Run()
	}
	reports := func() []string {
		names, _ := filepath.Glob(filepath.Join(home, ".claude", "relay-outside-reports", "report.*.json"))
		return names
	}
	const total = 300
	last := fmt.Sprintf("acme-session-%d", total-1)
	for n := 0; n < total; n++ {
		event, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": fmt.Sprintf("acme-session-%d", n), "cwd": "/acme"})
		if err := run(string(event)); err != nil {
			t.Fatalf("report %d failed: %v", n, err)
		}
	}
	files := reports()
	if len(files) > 256 {
		t.Fatalf("spool holds %d report files after %d events, want at most 256", len(files), total)
	}
	found := false
	for _, f := range files {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), `"`+last+`"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("newest event %s is not among the %d kept reports", last, len(files))
	}
	before := len(files)
	big, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": "acme-big", "cwd": "/acme", "pad": strings.Repeat("x", 64*1024+1)})
	if err := run(string(big)); err != nil {
		t.Fatalf("an event over 64 KiB must exit 0, got %v", err)
	}
	if after := len(reports()); after != before {
		t.Fatalf("an event over 64 KiB changed the spool from %d to %d files", before, after)
	}
	for _, f := range reports() {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), `"acme-big"`) {
			t.Fatalf("report %s holds the oversized event acme-big", f)
		}
		if len(b) > 65536 {
			t.Fatalf("report %s is %d bytes, want at most 65536", f, len(b))
		}
	}
}
