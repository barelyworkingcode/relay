package harness

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Outcome is what the test build's presence approver answers for one gated op.
type Outcome string

const (
	OutcomeApprove Outcome = "approve"
	OutcomeDeny    Outcome = "deny"
	OutcomeTimeout Outcome = "timeout"
)

// Fault is a failure the test build's keychain provider reproduces.
type Fault string

const (
	FaultNone    Fault = "none"
	FaultLocked  Fault = "locked"
	FaultMissing Fault = "missing"
	FaultCorrupt Fault = "corrupt"
	FaultSlow    Fault = "slow"
)

// CredentialSpec describes a control-plane credential planted in settings.json.
type CredentialSpec struct {
	Name    string
	Classes []string  // read, configure, grant, execute, proxy
	Expires time.Time // zero = no expiry
}

// Catalogue is a fake MCP's tool list.
type Catalogue struct {
	Initialize json.RawMessage   `json:"initialize,omitempty"` // merged into the initialize result
	Tools      []json.RawMessage `json:"tools"`                // MCP Tool objects, plus the control key "x-fake"
}

// FakeMCPSpec attaches a fake MCP server to an instance.
type FakeMCPSpec struct {
	ID        string
	Transport string // "stdio" | "http"
	OAuth     bool   // http only
	Catalogue Catalogue
}

// FakeModelHostSpec attaches a fake model host, standing in for relayLLM.
type FakeModelHostSpec struct {
	ID     string
	Models []json.RawMessage // /v1/models data rows, in docs/model-endpoint.md's upstream shape
}

// Options shape one instance.
type Options struct {
	// Settings replaces top-level settings.json keys. For api_credentials,
	// external_mcps and services the harness appends its own records to any
	// array given.
	Settings         map[string]json.RawMessage
	Credentials      []CredentialSpec
	Presence         map[string]Outcome // nil = no outcomes (every gated op denied)
	NoConsoleSession bool               // writes the console-session fact as false
	KeychainFault    Fault              // "" = no fault file
	FakeMCPs         []FakeMCPSpec
	FakeModelHost    *FakeModelHostSpec
	BootDeadline     time.Duration // default 90s
	// PrepareConfigDir runs after the harness wrote its files and before serve
	// starts, so a test can plant a file the harness has no option for.
	PrepareConfigDir func(configDir string)
}

// Ready is ready.json, schema 1 (docs/cli.md).
type Ready struct {
	Schema    int               `json:"schema"`
	PID       int               `json:"pid"`
	Version   string            `json:"version"`
	ConfigDir string            `json:"config_dir"`
	Sockets   map[string]string `json:"sockets"`
	Listeners map[string]string `json:"listeners"`
}

// Instance is one relay server with its own config dir, HOME, TMPDIR and PATH.
type Instance struct {
	Dir, ConfigDir, Home, Tmp string
	Ready                     Ready

	t     *testing.T
	n     int
	env   []string
	opts  Options
	creds map[string]Credential

	serve   *Proc
	stopped bool

	presence    map[string]Outcome
	consoleOn   bool
	fault       Fault
	fakeMCPs    []*fakeMCP
	modelHost   *FakeModelHostSpec
	hostLogPath string

	mu         sync.Mutex
	raceNotes  []string
	raceInFile map[string]bool
}

const defaultBootDeadline = 90 * time.Second

// Start boots an instance and registers its teardown with t. It fails t if the
// boot fails.
func Start(t *testing.T, o Options) *Instance {
	t.Helper()
	i := newInstance(t, o)
	t.Cleanup(i.cleanup)
	if res, ok := i.boot(); !ok {
		t.Fatalf("serve did not become ready: exit %d\nstderr: %s", res.Code, tailString(res.Stderr, 2000))
	}
	return i
}

// StartFails runs serve and requires it to exit before it is ready, returning
// its exit code and stderr.
func StartFails(t *testing.T, o Options) Result {
	t.Helper()
	i := newInstance(t, o)
	t.Cleanup(i.cleanup)
	res, ok := i.boot()
	if ok {
		t.Fatalf("serve became ready; expected it to refuse to start")
	}
	return res
}

func newInstance(t *testing.T, o Options) *Instance {
	t.Helper()
	if runRoot == "" {
		t.Fatalf("harness.Main did not run: use it from TestMain")
	}
	n := int(nextN.Add(1))
	dir := filepath.Join(runRoot, fmt.Sprintf("%03d", n))
	i := &Instance{
		t: t, n: n, Dir: dir, opts: o,
		ConfigDir:  filepath.Join(dir, "x"),
		Home:       filepath.Join(dir, "home"),
		Tmp:        filepath.Join(dir, "tmp"),
		creds:      map[string]Credential{},
		presence:   o.Presence,
		consoleOn:  !o.NoConsoleSession,
		fault:      o.KeychainFault,
		raceInFile: map[string]bool{},
	}
	for _, d := range []string{i.ConfigDir, i.Home, i.Tmp, filepath.Join(dir, "fakes"), filepath.Join(i.Home, ".local", "bin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}
	i.env = buildEnv(i.Home, i.Tmp, filepath.Join(i.Home, ".local", "bin"))
	i.linkAgents()
	i.writeSettings()
	i.writeSeams()
	if o.PrepareConfigDir != nil {
		o.PrepareConfigDir(i.ConfigDir)
	}
	return i
}

func buildEnv(home, tmp, binDir string) []string {
	name := "tester"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	// The built environment never inherits the caller's: no RELAY_*, CLAUDE*,
	// ANTHROPIC_* or OPENAI_* reaches serve or a CLI.
	return []string{
		"HOME=" + home,
		"TMPDIR=" + strings.TrimRight(tmp, "/") + "/",
		"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"SHELL=/bin/zsh",
		"USER=" + name,
		"LOGNAME=" + name,
		"LANG=en_US.UTF-8",
		"GORACE=halt_on_error=0",
	}
}

// linkAgents puts the fake agent CLIs on the instance's PATH. argv[0]'s
// basename names the persona, so each is the same binary under another name.
func (i *Instance) linkAgents() {
	for _, name := range []string{"claude", "pi", "codex"} {
		dst := filepath.Join(i.Home, ".local", "bin", name)
		if err := os.Link(bundle.FakeAgent, dst); err != nil {
			data, rerr := os.ReadFile(bundle.FakeAgent)
			if rerr != nil {
				i.t.Fatalf("linking fake %s: %v; reading %s: %v", name, err, bundle.FakeAgent, rerr)
			}
			if werr := os.WriteFile(dst, data, 0o755); werr != nil {
				i.t.Fatalf("copying fake %s: %v", name, werr)
			}
		}
	}
}

func (i *Instance) writeSettings() {
	t := i.t
	t.Helper()
	// No remote block: an absent block opens no remote or enrolment listener.
	settings := map[string]json.RawMessage{
		"api":            json.RawMessage(`{"listen":"127.0.0.1:0"}`),
		"model_endpoint": json.RawMessage(`{"listen":"127.0.0.1:0"}`),
	}
	appended := map[string][]json.RawMessage{}
	for k, v := range i.opts.Settings {
		switch k {
		case "api_credentials", "external_mcps", "services":
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err != nil {
				t.Fatalf("Options.Settings[%q] must be a JSON array: %v", k, err)
			}
			appended[k] = arr
		default:
			settings[k] = v
		}
	}
	for _, c := range i.opts.Credentials {
		appended["api_credentials"] = append(appended["api_credentials"], i.newCredential(c))
	}
	for _, spec := range i.opts.FakeMCPs {
		appended["external_mcps"] = append(appended["external_mcps"], i.startFakeMCP(spec))
	}
	if h := i.opts.FakeModelHost; h != nil {
		appended["services"] = append(appended["services"], i.modelHostRecord(h))
		i.modelHost = h
	}
	for k, arr := range appended {
		raw, err := json.Marshal(arr)
		if err != nil {
			t.Fatalf("encoding %s: %v", k, err)
		}
		settings[k] = raw
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatalf("encoding settings.json: %v", err)
	}
	writeFileAtomic(t, filepath.Join(i.ConfigDir, "settings.json"), data)
}

func (i *Instance) newCredential(c CredentialSpec) json.RawMessage {
	var id [16]byte
	tokBytes := make([]byte, 32)
	if _, err := rand.Read(tokBytes); err != nil {
		i.t.Fatalf("reading random bytes: %v", err)
	}
	rand.Read(id[:])
	token := hex.EncodeToString(tokBytes)
	sum := sha256.Sum256([]byte(token))
	hexID := hex.EncodeToString(id[:])
	credID := hexID[0:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:]
	i.creds[c.Name] = Credential{ID: credID, Name: c.Name, Token: token, Classes: c.Classes}
	rec := map[string]any{
		"id":      credID,
		"name":    c.Name,
		"hash":    hex.EncodeToString(sum[:]),
		"classes": c.Classes,
		"created": time.Now().UTC().Format(time.RFC3339),
	}
	if !c.Expires.IsZero() {
		rec["expires"] = c.Expires.UTC().Format(time.RFC3339)
	}
	raw, _ := json.Marshal(rec)
	return raw
}

func writeFileAtomic(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		t.Fatalf("creating a temp file beside %s: %v", path, err)
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
	} else {
		tmp.Close()
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
		t.Fatalf("writing %s: %v", path, err)
	}
}

// boot starts serve and waits for the ready line. ok is false when serve
// exited first; the returned result then holds its exit and stderr.
func (i *Instance) boot() (res Result, ok bool) {
	t := i.t
	t.Helper()
	deadline := i.opts.BootDeadline
	if deadline <= 0 {
		deadline = defaultBootDeadline
	}
	stdoutFile, err := os.OpenFile(filepath.Join(i.Dir, "serve.stdout"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening serve.stdout: %v", err)
	}
	stderrFile, err := os.OpenFile(filepath.Join(i.Dir, "serve.stderr"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening serve.stderr: %v", err)
	}
	// The readiness line is read through the Proc, then the tee keeps copying
	// the rest of stdout into serve.stdout for the failure dump.
	p := startProc(t, procSpec{
		bin: bundle.Relay, args: []string{"--config-dir", i.ConfigDir, "serve"},
		env: i.env, dir: i.Dir, stdoutTee: stdoutFile, stderrTo: stderrFile,
	})
	stderrFile.Close() // the child holds its own descriptor
	go func() {
		<-p.done
		stdoutFile.Close()
	}()
	i.serve = p
	i.stopped = false
	os.WriteFile(filepath.Join(i.Dir, "serve.pid"), []byte(fmt.Sprint(p.cmd.Process.Pid)), 0o600)

	line, got := p.nextLine(deadline, "waiting for relay serve to be ready")
	if !got {
		stderr, _ := os.ReadFile(filepath.Join(i.Dir, "serve.stderr"))
		out, _ := p.out.snapshot()
		i.stopped = true
		return Result{Code: p.exitCode(), Stdout: out, Stderr: stderr}, false
	}
	want := filepath.Join(i.ConfigDir, "ready.json")
	if !samePath(string(line), want) {
		t.Fatalf("serve printed %q as its ready line, expected the path of %s", line, want)
	}
	i.ReloadReady()
	return Result{}, true
}

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// ReloadReady re-reads ready.json.
func (i *Instance) ReloadReady() Ready {
	i.t.Helper()
	data, err := os.ReadFile(filepath.Join(i.ConfigDir, "ready.json"))
	if err != nil {
		i.t.Fatalf("reading ready.json: %v", err)
	}
	var r Ready
	if err := json.Unmarshal(data, &r); err != nil {
		i.t.Fatalf("decoding ready.json: %v\n%s", err, data)
	}
	i.Ready = r
	return r
}

const stopDeadline = 30 * time.Second

// Stop sends SIGTERM to serve, waits for it to exit and checks the exit code
// and the logs for data races. It is safe to call twice.
func (i *Instance) Stop() {
	t := i.t
	t.Helper()
	if i.stopped || i.serve == nil {
		return
	}
	i.stopped = true
	p := i.serve
	select {
	case <-p.done:
	default:
		p.cmd.Process.Signal(syscall.SIGTERM)
		tm := time.NewTimer(stopDeadline)
		select {
		case <-p.done:
			tm.Stop()
		case <-tm.C:
			p.cmd.Process.Kill()
			<-p.done
			t.Errorf("serve did not exit within %v of SIGTERM and was killed", stopDeadline)
		}
	}
	if code := p.exitCode(); code != 0 {
		t.Errorf("serve exited %d, expected 0", code)
	}
	i.scanRaces()
}

// Restart stops serve and starts it again on the same config dir.
func (i *Instance) Restart() {
	i.t.Helper()
	i.Stop()
	i.writeSeams()
	if res, ok := i.boot(); !ok {
		i.t.Fatalf("serve did not become ready after restart: exit %d\nstderr: %s", res.Code, tailString(res.Stderr, 2000))
	}
}

func (i *Instance) noteStderr(r Result) {
	if strings.Contains(string(r.Stderr), "WARNING: DATA RACE") {
		i.mu.Lock()
		i.raceNotes = append(i.raceNotes, "a CLI process's stderr")
		i.mu.Unlock()
	}
}

func (i *Instance) scanRaces() {
	t := i.t
	t.Helper()
	files := []string{filepath.Join(i.Dir, "serve.stderr")}
	logs, _ := filepath.Glob(filepath.Join(i.ConfigDir, "logs", "relaysessions.log*"))
	files = append(files, logs...)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil || !strings.Contains(string(data), "WARNING: DATA RACE") {
			continue
		}
		i.mu.Lock()
		seen := i.raceInFile[f]
		i.raceInFile[f] = true
		i.mu.Unlock()
		if !seen {
			t.Errorf("data race reported in %s", f)
		}
	}
	i.mu.Lock()
	notes := i.raceNotes
	i.raceNotes = nil
	i.mu.Unlock()
	for _, n := range notes {
		t.Errorf("data race reported in %s", n)
	}
}

func (i *Instance) cleanup() {
	i.Stop()
	i.scanRaces()
	i.stopFakes()
	if i.t.Failed() {
		i.dump()
		i.t.Logf("instance directory kept at %s; the next run removes it", i.Dir)
		return
	}
	os.RemoveAll(i.Dir)
}
