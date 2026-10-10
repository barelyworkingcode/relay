package prooftest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// waitBound is only an upper bound around a signal, never a pause.
const waitBound = 120 * time.Second

const (
	opsTok = "test-token-ops-0001"
	roTok  = "test-token-ro-0002"
)

type J = map[string]any

var buildRoot, fakerelayBin, probeBin string

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	root, err := os.MkdirTemp("/tmp", "frp-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	buildRoot = root
	fakerelayBin, probeBin = filepath.Join(root, "fakerelay"), filepath.Join(root, "probesvc")
	for _, b := range [][2]string{{"./cmd/fakerelay", fakerelayBin}, {"./prooftest/probesvc", probeBin}} {
		args := []string{"build"}
		if raceBuilt {
			args = append(args, "-race")
		}
		cmd := exec.Command("go", append(args, "-o", b[1], b[0])...)
		cmd.Dir = ".."
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", b[0], err, out)
			os.RemoveAll(root)
			return 1
		}
	}
	code := m.Run()
	if code == 0 {
		os.RemoveAll(root)
	} else {
		fmt.Fprintln(os.Stderr, "kept", root)
	}
	return code
}

func eq[T any](t testing.TB, got, want T, what string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func must(t testing.TB, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type readyFile struct {
	PID       int
	ConfigDir string `json:"config_dir"`
	Sockets   struct{ Bridge, Frontend, Control string }
	Listeners struct{ API string }
}

type instance struct {
	idx    int
	dir    string
	cmd    *exec.Cmd
	done   chan struct{}
	stderr *lockedBuf
	extra  chan string // stdout after the first line
	ready  readyFile
}

func newDir() (string, error) { return os.MkdirTemp(buildRoot, "i") }

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "RELAY_") && !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

// launch starts `serve` in dir and returns once the first stdout line arrives.
func launch(dir string, world any, env ...string) (*instance, error) {
	if world != nil {
		b, _ := json.Marshal(world)
		if err := os.WriteFile(filepath.Join(dir, "world.json"), b, 0o600); err != nil {
			return nil, err
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	in := &instance{dir: dir, done: make(chan struct{}), stderr: &lockedBuf{}, extra: make(chan string, 1)}
	in.cmd = exec.Command(fakerelayBin, "--config-dir", dir, "serve")
	in.cmd.Env = append(cleanEnv(), env...)
	in.cmd.Stdout, in.cmd.Stderr = w, in.stderr
	if err := in.cmd.Start(); err != nil {
		return nil, err
	}
	w.Close()
	first := make(chan string, 1)
	go func() {
		br := bufio.NewReader(r)
		l, _ := br.ReadString('\n')
		first <- l
		rest, _ := io.ReadAll(br)
		in.extra <- string(rest)
	}()
	go func() { _ = in.cmd.Wait(); close(in.done) }()
	select {
	case line := <-first:
		if strings.TrimSpace(line) != filepath.Join(dir, "ready.json") {
			return in, fmt.Errorf("first stdout line %q, want the ready.json path\n%s", line, in.stderr)
		}
	case <-in.done:
		return in, fmt.Errorf("serve exited before ready: %v\n%s", in.cmd.ProcessState, in.stderr)
	case <-time.After(waitBound):
		_ = in.cmd.Process.Kill()
		return in, fmt.Errorf("no ready line\n%s", in.stderr)
	}
	b, err := os.ReadFile(filepath.Join(dir, "ready.json"))
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(b, &in.ready); err != nil {
		return in, err
	}
	if st, err := os.Stat(filepath.Join(dir, "ready.json")); err != nil || st.Mode().Perm() != 0o600 {
		return in, fmt.Errorf("ready.json mode: %v %v", st, err)
	}
	if st, err := os.Stat(in.ready.Sockets.Control); err != nil || st.Mode().Perm() != 0o600 {
		return in, fmt.Errorf("control socket mode: %v %v", st, err)
	}
	return in, nil
}

// startInstance launches an instance and registers the shutdown checks.
func startInstance(t *testing.T, mkWorld func(dir string) any, env ...string) *instance {
	t.Helper()
	dir, err := newDir()
	must(t, err, "temp dir")
	var world any
	if mkWorld != nil {
		world = mkWorld(dir)
	}
	in, err := launch(dir, world, env...)
	if in != nil {
		t.Cleanup(func() { in.stop(t) })
	}
	must(t, err, "launch")
	return in
}

// stop sends SIGTERM and checks the clean-exit promises.
func (in *instance) stop(t testing.TB) {
	select {
	case <-in.done:
		t.Errorf("fakerelay died before the test ended: %v\n%s", in.cmd.ProcessState, in.stderr)
		return
	default:
	}
	_ = in.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-in.done:
	case <-time.After(waitBound):
		_ = in.cmd.Process.Kill()
		<-in.done
		t.Errorf("no exit after SIGTERM\n%s", in.stderr)
		return
	}
	if code := in.cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("exit code %d after SIGTERM, want 0\n%s", code, in.stderr)
	}
	for _, p := range []string{filepath.Join(in.dir, "ready.json"), in.ready.Sockets.Bridge, in.ready.Sockets.Frontend, in.ready.Sockets.Control} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after exit (%v)", p, err)
		}
	}
	if rest := <-in.extra; rest != "" {
		t.Errorf("stdout held more than the ready path: %q", rest)
	}
	locks, _ := filepath.Glob(filepath.Join(in.dir, "probe-*.lock"))
	for _, l := range locks {
		in.waitProbeGone(t, l)
	}
}

// waitProbeGone blocks until the probe that owns lockPath has exited: the
// probe holds an exclusive flock on it for its whole life.
func (in *instance) waitProbeGone(t testing.TB, lockPath string) {
	f, err := os.Open(lockPath)
	if err != nil {
		t.Errorf("open %s: %v", lockPath, err)
		return
	}
	defer f.Close()
	got := make(chan error, 1)
	go func() { got <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }()
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("flock %s: %v", lockPath, err)
		}
	case <-time.After(waitBound):
		t.Errorf("probe behind %s is still alive", lockPath)
	}
}

func (in *instance) cli(t testing.TB, args ...string) (string, string, int) {
	return in.cliTrace(t, "", args...)
}

func (in *instance) cliTrace(t testing.TB, trace string, args ...string) (string, string, int) {
	t.Helper()
	pre := []string{"--config-dir", in.dir}
	if trace != "" {
		pre = append(pre, "--trace", trace)
	}
	return runBin(t, nil, append(pre, args...)...)
}

func runBin(t testing.TB, env []string, args ...string) (string, string, int) {
	t.Helper()
	return runBinIn(t, "", env, args...)
}

func parseRows(t testing.TB, out string) []J {
	t.Helper()
	out = strings.TrimSpace(out)
	var rows []J
	if strings.HasPrefix(out, "[") {
		must(t, json.Unmarshal([]byte(out), &rows), "json array output: "+out)
		return rows
	}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		var r J
		must(t, json.Unmarshal([]byte(l), &r), "json line: "+l)
		rows = append(rows, r)
	}
	return rows
}

// events returns the matching log lines already written; no waiting.
func (in *instance) events(t testing.TB, trace, key string, extra ...string) []J {
	t.Helper()
	args := append([]string{"logs", "--json", "--event", key}, extra...)
	out, se, code := in.cliTrace(t, trace, args...)
	if code == 1 {
		return nil
	}
	if code != 0 {
		t.Fatalf("logs %s: exit %d: %s", key, code, se)
	}
	return parseRows(t, out)
}

// waitEvent returns the first matching line, waiting on the server's append
// notice when none is written yet.
func (in *instance) waitEvent(t testing.TB, key string, extra ...string) J {
	t.Helper()
	args := append([]string{"logs", "--json", "--follow", "--timeout", waitBound.String(), "--event", key}, extra...)
	out, se, code := in.cli(t, args...)
	if code != 0 {
		t.Fatalf("waiting for %s: exit %d: %s%s", key, code, out, se)
	}
	rows := parseRows(t, out)
	if len(rows) == 0 {
		t.Fatalf("waiting for %s: no line", key)
	}
	return rows[0]
}

func after(ev J) string {
	ts, err := time.Parse(time.RFC3339Nano, fmt.Sprint(ev["ts"]))
	if err != nil {
		return "bad-ts"
	}
	return ts.Add(time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
}

// followFirst runs a followed `logs` for one key until a line matches (and,
// for service.state, the match accepts it) or ctx ends.
func (in *instance) followFirst(ctx context.Context, key string, accept func(J) bool, extra []string) (J, error) {
	since := extra
	for {
		args := append([]string{"--config-dir", in.dir, "logs", "--json", "--follow", "--timeout", waitBound.String(), "--event", key}, since...)
		cmd := exec.CommandContext(ctx, fakerelayBin, args...)
		cmd.Env = cleanEnv()
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("following %s: %v: %s", key, err, se.String())
		}
		var first J
		line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(so.String()), "\n", 2)[0])
		if err := json.Unmarshal([]byte(line), &first); err != nil {
			return nil, fmt.Errorf("following %s: bad line %q", key, line)
		}
		if accept == nil || accept(first) {
			return first, nil
		}
		since = append(append([]string(nil), extra...), "--since", after(first))
	}
}

// waitRegistered returns the service.manifest.register line, and fails at once
// when a service.state failed line arrives first, so a service that dies
// before registering is reported where it died.
func (in *instance) waitRegistered(t testing.TB, extra ...string) J {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type out struct {
		ev     J
		failed bool
		err    error
	}
	ch := make(chan out, 2)
	go func() {
		ev, err := in.followFirst(ctx, "service.manifest.register", nil, extra)
		ch <- out{ev: ev, err: err}
	}()
	go func() {
		ev, err := in.followFirst(ctx, "service.state", func(e J) bool { return e["phase"] == "failed" }, extra)
		ch <- out{ev: ev, failed: true, err: err}
	}()
	r := <-ch
	if r.err != nil {
		t.Fatalf("waiting for service.manifest.register: %v", r.err)
	}
	if r.failed {
		t.Fatalf("service failed before registering its manifest: %v", r.ev)
	}
	return r.ev
}

// waitServiceFailed waits for the service.state line with phase failed.
func (in *instance) waitServiceFailed(t testing.TB) J {
	ev := in.waitEvent(t, "service.state")
	for ev["phase"] != "failed" {
		ev = in.waitEvent(t, "service.state", "--since", after(ev))
	}
	return ev
}

type res struct {
	Status int
	Body   []byte
	Header http.Header
}

func (r res) is(t testing.TB, status int) res {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status %d, want %d: %s", r.Status, status, r.Body)
	}
	return r
}

func (r res) obj(t testing.TB) J {
	t.Helper()
	var o J
	must(t, json.Unmarshal(r.Body, &o), "object body "+string(r.Body))
	return o
}

func (r res) arr(t testing.TB) []J {
	t.Helper()
	var a []J
	must(t, json.Unmarshal(r.Body, &a), "array body "+string(r.Body))
	return a
}

func unixClient(path string) *http.Client {
	return &http.Client{Timeout: waitBound, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}}
}

func (in *instance) sockClient() *http.Client { return unixClient(in.ready.Sockets.Frontend) }
func (in *instance) ctlClient() *http.Client  { return unixClient(in.ready.Sockets.Control) }
func (in *instance) tcpClient() *http.Client {
	return &http.Client{Timeout: waitBound, Transport: &http.Transport{DisableKeepAlives: true}}
}

func send(ctx context.Context, c *http.Client, base, method, path, tok string, body any, hdr ...string) (res, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return res{}, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		return res{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return res{resp.StatusCode, b, resp.Header}, nil
}

// api calls the frontend socket with the given bearer.
func (in *instance) apiAs(t testing.TB, tok, method, path string, body any, hdr ...string) res {
	t.Helper()
	r, err := send(context.Background(), in.sockClient(), "http://relay", method, path, tok, body, hdr...)
	must(t, err, method+" "+path)
	return r
}

func (in *instance) api(t testing.TB, method, path string, body any, hdr ...string) res {
	t.Helper()
	return in.apiAs(t, opsTok, method, path, body, hdr...)
}

func (in *instance) ctl(t testing.TB, method, path string, body any) res {
	t.Helper()
	r, err := send(context.Background(), in.ctlClient(), "http://ctl", method, path, "", body)
	must(t, err, "control "+method+" "+path)
	return r
}

func (in *instance) dialWS(path, tok string, hdr ...string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		h.Set(hdr[i], hdr[i+1])
	}
	sock := in.ready.Sockets.Frontend
	d := websocket.Dialer{HandshakeTimeout: waitBound, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}
	return d.Dial("ws://relay"+path, h)
}

func (in *instance) ws(t testing.TB, path, tok string, hdr ...string) *websocket.Conn {
	t.Helper()
	c, _, err := in.dialWS(path, tok, hdr...)
	must(t, err, "dial "+path)
	t.Cleanup(func() { c.Close() })
	return c
}

// until reads frames until pred matches; the frames are the signal and the
// read deadline is only a bound. It returns every frame read, the match last.
func until(t testing.TB, c *websocket.Conn, pred func(J) bool) []J {
	t.Helper()
	var seen []J
	for {
		_ = c.SetReadDeadline(time.Now().Add(waitBound))
		var f J
		if err := c.ReadJSON(&f); err != nil {
			t.Fatalf("waiting for a frame: %v (read so far: %v)", err, seen)
		}
		seen = append(seen, f)
		if pred(f) {
			return seen
		}
	}
}

func isType(typ string, kv ...any) func(J) bool {
	return func(f J) bool {
		if f["type"] != typ {
			return false
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if f[kv[i].(string)] != kv[i+1] {
				return false
			}
		}
		return true
	}
}

// bridgeCall sends one request line to the bridge socket and returns the reply.
func (in *instance) bridgeCall(t testing.TB, req J) J {
	t.Helper()
	c, err := net.Dial("unix", in.ready.Sockets.Bridge)
	must(t, err, "dial bridge")
	defer c.Close()
	b, _ := json.Marshal(req)
	_, err = c.Write(append(b, '\n'))
	must(t, err, "bridge write")
	line, err := bufio.NewReader(c).ReadBytes('\n')
	must(t, err, "bridge read")
	var out J
	must(t, json.Unmarshal(line, &out), "bridge reply "+string(line))
	return out
}

func service(dir, id, mode, prefix string, caps ...string) J {
	return J{"id": id, "name": id, "command": probeBin, "args": []string{}, "working_dir": dir,
		"capabilities": caps, "autostart": true, "env": J{"PROBE_MODE": mode, "PROBE_PREFIX": prefix}}
}

func creds() []J {
	return []J{
		{"id": "c_ops", "name": "acme-ops", "classes": []string{"read", "configure", "execute", "proxy"}, "token": opsTok},
		{"id": "c_ro", "name": "acme-ro", "classes": []string{"read"}, "token": roTok},
	}
}

type probeReport struct {
	PID                int      `json:"pid"`
	ServiceID          string   `json:"service_id"`
	SecretHex64        bool     `json:"secret_hex64"`
	FD3EOF             bool     `json:"fd3_eof"`
	LaunchFD           string   `json:"launch_fd"`
	BridgeSocket       string   `json:"bridge_socket"`
	ConfigDir          string   `json:"config_dir"`
	FrontendEnvSet     bool     `json:"frontend_env_set"`
	FrontendSocket     string   `json:"frontend_socket"`
	Leaked             []string `json:"leaked"`
	HelloOK            bool     `json:"hello_ok"`
	WrongSecretRefused bool     `json:"wrong_secret_refused"`
	SecondHelloRefused bool     `json:"second_hello_refused"`
	OtherConnRefused   bool     `json:"other_conn_refused"`
	ProjectsStatus     int      `json:"projects_status"`
	ProjectNames       []string `json:"project_names"`
	AuthOK             bool     `json:"auth_ok"`
	Trace              string   `json:"trace"`
}

func (in *instance) report(t testing.TB, prefix string, hdr ...string) probeReport {
	t.Helper()
	var rep probeReport
	must(t, json.Unmarshal(in.api(t, "GET", "/api/"+prefix+"/report", nil, hdr...).is(t, 200).Body, &rep), "probe report")
	return rep
}

func websocketClosed(err error, code int, reason string) bool {
	var ce *websocket.CloseError
	return errors.As(err, &ce) && ce.Code == code && ce.Text == reason
}

func itoa(n int) string { return fmt.Sprint(n) }

// runBinIn is runBin with a working directory.
func runBinIn(t testing.TB, cwd string, env []string, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitBound)
	defer cancel()
	cmd := exec.CommandContext(ctx, fakerelayBin, args...)
	cmd.Dir, cmd.Env = cwd, append(cleanEnv(), env...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return so.String(), se.String(), ee.ExitCode()
	}
	must(t, err, "run")
	return so.String(), se.String(), 0
}
