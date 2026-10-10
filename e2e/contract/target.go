package contract

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"relaye2e/harness"
)

// Kind names the implementation a Target runs.
type Kind string

// The two implementations a scenario runs against.
const (
	Fake Kind = "fakerelay"
	Real Kind = "relay"
)

const (
	bootWait  = 60 * time.Second
	frameWait = 60 * time.Second
	// backoffCeiling is past relay's 30 s reconnect backoff, so one clock step
	// lets a dropped host agent reconnect.
	backoffCeiling = 31 * time.Second
)

// Target is one running implementation of the spec. Remote is the directory
// the spec's "{remote}" stands for.
type Target struct {
	Kind   Kind
	I      *harness.Instance
	Remote string

	t     *testing.T
	spec  Spec
	hooks map[string]*wsConn // real only: host id -> the hook's own /ws/files connection
}

// startTarget boots kind from sp and returns once its services and fake MCPs
// are up. Every wait is an event the instance writes.
func startTarget(t *testing.T, kind Kind, sp Spec) *Target {
	t.Helper()
	if err := sp.validate(); err != nil {
		t.Fatalf("contract spec: %v", err)
	}
	tg := &Target{Kind: kind, t: t, spec: sp, hooks: map[string]*wsConn{}}
	switch kind {
	case Fake:
		world, err := sp.fakeWorld()
		if err != nil {
			t.Fatalf("contract spec: %v", err)
		}
		tg.I = harness.StartFake(t, harness.FakeOptions{
			World: world, Credentials: sp.Credentials, Presence: sp.Presence,
			PrepareDir: tg.prepare("world.json"),
		})
	case Real:
		settings, mcps, err := sp.realSettings()
		if err != nil {
			t.Fatalf("contract spec: %v", err)
		}
		tg.I = harness.Start(t, harness.Options{
			Settings: settings, Credentials: sp.Credentials, Presence: sp.Presence,
			FakeMCPs: mcps, SSHStub: len(sp.Hosts) > 0,
			PrepareConfigDir: tg.prepare("settings.json"),
		})
		tg.I.WaitSessionHost(bootWait)
		for _, m := range sp.MCPs {
			tg.I.WaitEvent(harness.EventQuery{Key: "mcp.state", Fields: map[string]any{"mcp_id": m.ID, "state": "up"}}, bootWait)
		}
	default:
		t.Fatalf("unknown target kind %q", kind)
	}
	if want := tg.I.RemoteRoot(); tg.Remote == "" || !sameDir(want, tg.Remote) {
		t.Fatalf("remote root %q does not match the instance's %q", tg.Remote, want)
	}
	for _, s := range sp.Services {
		q := harness.EventQuery{Key: "service.manifest.register", Fields: map[string]any{"service_id": s.ID}}
		if kind == Real {
			q.Fields["status"] = "ok"
		}
		tg.I.WaitEvent(q, bootWait)
	}
	return tg
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// prepare runs between the harness writing its files and serve starting. The
// instance directory is known here and not earlier, so the placeholder tokens
// are resolved and the disk is seeded now. Both are the same for either kind.
func (tg *Target) prepare(file string) func(configDir string) {
	return func(configDir string) {
		t := tg.t
		t.Helper()
		dir := filepath.Dir(configDir)
		remote := filepath.Join(dir, "remote")
		if err := os.MkdirAll(remote, 0o700); err != nil {
			t.Fatalf("creating %s: %v", remote, err)
		}
		// Resolved, because the run root sits under /tmp and relay may report
		// the resolved form of a path it was given.
		rdir, err1 := filepath.EvalSymlinks(dir)
		rremote, err2 := filepath.EvalSymlinks(remote)
		if err1 != nil || err2 != nil {
			t.Fatalf("resolving %s and %s: %v %v", dir, remote, err1, err2)
		}
		tg.Remote = rremote
		repl := map[string]string{tokRemote: rremote, tokInstance: rdir}
		if err := patchTokens(filepath.Join(configDir, file), repl); err != nil {
			t.Fatalf("resolving placeholders in %s: %v", file, err)
		}
		tg.seedServices(dir, repl)
		tg.seedProjects(dir, repl)
		if tg.Kind == Real {
			tg.seedTmux(remote)
		}
	}
}

func (tg *Target) seedServices(dir string, repl map[string]string) {
	for _, s := range tg.spec.Services {
		if len(s.Config) == 0 {
			continue
		}
		body := string(s.Config)
		for k, v := range repl {
			body = strings.ReplaceAll(body, k, v)
		}
		path := filepath.Join(dir, "fakes", s.ID+".config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			tg.t.Fatalf("writing %s: %v", path, err)
		}
	}
}

func (tg *Target) seedProjects(dir string, repl map[string]string) {
	t := tg.t
	for _, p := range tg.spec.Projects {
		root := p.dir()
		for k, v := range repl {
			root = strings.ReplaceAll(root, k, v)
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("creating project %s at %s: %v", p.ID, root, err)
		}
		if p.Repo != nil {
			tg.seedRepo(filepath.Join(dir, "home"), root, p.Repo)
		}
		names := make([]string, 0, len(p.Files))
		for rel := range p.Files {
			names = append(names, rel)
		}
		sort.Strings(names)
		for _, rel := range names {
			tg.seedFile(root, rel, p.Files[rel])
		}
	}
}

func (tg *Target) seedFile(root, rel string, f File) {
	t := tg.t
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		t.Fatalf("seed path %q must be relative and without '..'", rel)
	}
	path := filepath.Join(root, rel)
	var err error
	switch {
	case f.Dir:
		err = os.MkdirAll(path, 0o755)
	case f.Symlink != "":
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			_ = os.Remove(path)
			err = os.Symlink(f.Symlink, path)
		}
	default:
		data := []byte(f.Text)
		if f.Base64 != "" {
			if data, err = base64.StdEncoding.DecodeString(f.Base64); err != nil {
				t.Fatalf("seed file %s: bad base64: %v", rel, err)
			}
		}
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			err = os.WriteFile(path, data, 0o644)
		}
	}
	if err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}
}

// seedRepo makes a repository whose SHAs depend only on the Spec: the
// environment is built from scratch, so no GIT_* variable or user config of
// the machine reaches git.
func (tg *Target) seedRepo(home, root string, r *Repo) {
	t := tg.t
	branch := r.Branch
	if branch == "" {
		branch = "main"
	}
	git := func(date string, args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Dir = root
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C",
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=Acme Author", "GIT_AUTHOR_EMAIL=author@acme.test",
			"GIT_COMMITTER_NAME=Acme Author", "GIT_COMMITTER_EMAIL=author@acme.test",
			"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date,
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, root, err, out)
		}
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	git(base.Format(time.RFC3339), "init", "-q", "-b", branch)
	for n, c := range r.Commits {
		names := make([]string, 0, len(c.Files))
		for rel := range c.Files {
			names = append(names, rel)
		}
		sort.Strings(names)
		for _, rel := range names {
			tg.seedFile(root, rel, File{Text: c.Files[rel]})
		}
		date := base.Add(time.Duration(n+1) * time.Minute).Format(time.RFC3339)
		git(date, "add", "-A")
		git(date, "commit", "-q", "--allow-empty", "-m", c.Message)
	}
}

// seedTmux starts the host's tmux sessions in the tmux dir the ssh stub gives
// every remote command. new-session -d exits once the session exists, which is
// the signal.
func (tg *Target) seedTmux(remote string) {
	t := tg.t
	var names []string
	for _, h := range tg.spec.Hosts {
		names = append(names, h.Persistent...)
	}
	if len(names) == 0 {
		return
	}
	tmux, _, err := findTool("tmux")
	if err != nil {
		t.Fatalf("contract spec: %v", err)
	}
	tmuxDir := filepath.Join(remote, "tmux")
	if err := os.MkdirAll(tmuxDir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", tmuxDir, err)
	}
	run := func(args ...string) string {
		cmd := exec.Command(tmux, append([]string{"-f", "/dev/null"}, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + remote, "SHELL=/bin/sh", "TERM=xterm", "TMUX_TMPDIR=" + tmuxDir}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for _, name := range names {
		run("new-session", "-d", "-s", name, "/bin/cat")
	}
	// Ends the one server this target started; it is not found by name.
	if pid, err := strconv.Atoi(run("display-message", "-p", "-t", names[0], "#{pid}")); err == nil {
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
	}
}

// SetPresence replaces the per-op presence outcomes: ctl on fakerelay, the
// seam file on the relay test build.
func (tg *Target) SetPresence(outcomes map[string]harness.Outcome) {
	tg.t.Helper()
	tg.I.SetPresence(outcomes)
}

// ClockAdvance moves the target's clock forward.
func (tg *Target) ClockAdvance(d time.Duration) {
	tg.t.Helper()
	tg.I.ClockAdvance(d)
}

// SetHostStatus moves a host between "connected" and "unreachable". fakerelay
// is told through ctl. On relay the ssh stub drops or restores the link, and
// the call returns when the host_status frame reaches the hook's own
// /ws/files connection, which holds a watch on one of the host's projects.
func (tg *Target) SetHostStatus(hostID, status string) {
	t := tg.t
	t.Helper()
	if status != "connected" && status != "unreachable" {
		t.Fatalf("SetHostStatus %s: status must be connected or unreachable, got %q", hostID, status)
	}
	var host *Host
	for k := range tg.spec.Hosts {
		if tg.spec.Hosts[k].ID == hostID {
			host = &tg.spec.Hosts[k]
		}
	}
	if host == nil {
		t.Fatalf("SetHostStatus: host %q is not in Spec.Hosts", hostID)
	}
	if tg.Kind == Fake {
		if r := tg.I.Ctl("host", "status", "--id", hostID, "--status", status); r.Code != 0 {
			t.Fatalf("fakerelay ctl host status exited %d\nstderr: %s", r.Code, r.Stderr)
		}
		return
	}
	hook := tg.hostHook(*host)
	if status == "unreachable" {
		tg.I.SSHDown(host.Target)
	} else {
		tg.I.SSHUp(host.Target)
		tg.I.ClockAdvance(backoffCeiling)
	}
	hook.waitFor(fmt.Sprintf("host_status %s for %s", status, hostID), func(f map[string]any) bool {
		return f["type"] == "host_status" && f["host_id"] == hostID && f["status"] == status
	})
}

// hostHook returns the hook's /ws/files connection, opened on first use with a
// watch on the host's first project. Frames before watch_ok or watch_error are
// the initial host_status frames.
func (tg *Target) hostHook(h Host) *wsConn {
	t := tg.t
	t.Helper()
	if w := tg.hooks[h.ID]; w != nil {
		return w
	}
	var token, project string
	for _, c := range tg.spec.Credentials {
		for _, class := range c.Classes {
			if class == "execute" && token == "" {
				token = tg.I.Credential(c.Name).Token
			}
		}
	}
	for _, p := range tg.spec.Projects {
		if p.HostID == h.ID && project == "" {
			project = p.ID
		}
	}
	if token == "" || project == "" {
		t.Fatalf("SetHostStatus %s on relay needs a credential with the execute class and a project on the host", h.ID)
	}
	w := dialWS(t, tg.I, "/ws/files", token)
	w.send(map[string]any{"type": "watch", "project_id": project})
	w.waitFor("watch answer for "+project, func(f map[string]any) bool {
		return (f["type"] == "watch_ok" || f["type"] == "watch_error") && f["project_id"] == project
	})
	tg.hooks[h.ID] = w
	return w
}
