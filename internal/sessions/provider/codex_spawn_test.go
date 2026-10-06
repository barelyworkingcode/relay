package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const (
	codexFixtureOK     = "testdata/codex/turn_ok.jsonl"
	codexFixtureFailed = "testdata/codex/turn_failed.jsonl"
	// Recorded in turn_ok.jsonl.
	codexFixtureThread = "01a10fd0-f034-7653-ba63-00b35079cca2"
	codexFixtureTurn1  = "01a10fd0-f2a6-75d2-9c39-373d2ff5670d"
)

var (
	buildTestCodexOnce sync.Once
	testCodexBin       string
	buildTestCodexErr  error
)

// buildTestCodexBinary builds cmd/testcodex once per test run into a short
// /tmp dir.
func buildTestCodexBinary(t *testing.T) string {
	t.Helper()
	buildTestCodexOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "rh-testcodex-")
		if err != nil {
			buildTestCodexErr = err
			return
		}
		testCodexBin = filepath.Join(dir, "codex")
		cmd := exec.Command("go", "build", "-o", testCodexBin, "./cmd/testcodex")
		cmd.Dir = repoRoot(t)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			buildTestCodexErr = fmt.Errorf("build testcodex: %w", err)
		}
	})
	if buildTestCodexErr != nil {
		t.Fatalf("build testcodex binary: %v", buildTestCodexErr)
	}
	return testCodexBin
}

// codexRec records every handler call.
type codexRec struct {
	mu    sync.Mutex
	kinds []string
	data  []string
}

func (r *codexRec) handle(kind string, data json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, kind)
	r.data = append(r.data, string(data))
}

func (r *codexRec) count(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, k := range r.kinds {
		if k == kind {
			n++
		}
	}
	return n
}

func (r *codexRec) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.kinds...)
}

// text joins every assistant text delta, in order.
func (r *codexRec) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sb strings.Builder
	for i, k := range r.kinds {
		if k != "llm_event" {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(r.data[i]), &ev) == nil && ev.Type == "assistant" && ev.Delta.Type == "text_delta" {
			sb.WriteString(ev.Delta.Text)
		}
	}
	return sb.String()
}

// codexHarness is one real testcodex child driven through CodexProvider.Start.
type codexHarness struct {
	p   *CodexProvider
	rec *codexRec
	out string
	dir string
}

// newCodexHarness points testcodex at fixture (a path relative to this
// package) and env, and returns a provider that has not started yet.
func newCodexHarness(t *testing.T, fixture string, env map[string]string, mutate func(*sessionstypes.Session)) *codexHarness {
	t.Helper()
	bin := buildTestCodexBinary(t)
	fx, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatal(err)
	}
	h := &codexHarness{rec: &codexRec{}, out: filepath.Join(t.TempDir(), "out.jsonl"), dir: t.TempDir()}
	t.Setenv("TESTCODEX_FIXTURE", fx)
	t.Setenv("TESTCODEX_OUT", h.out)
	for k, v := range env {
		t.Setenv(k, v)
	}
	sess := &sessionstypes.Session{ID: "cx1", Model: "codex/gpt-6-luna", Directory: h.dir}
	if mutate != nil {
		mutate(sess)
	}
	h.p = NewCodexProvider(sess, h.rec.handle, CodexConfig{Binary: bin})
	t.Cleanup(h.p.Kill)
	return h
}

func (h *codexHarness) start(t *testing.T) {
	t.Helper()
	if err := h.p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// turn sends text and waits for the n-th message_complete.
func (h *codexHarness) turn(t *testing.T, text string, n int) {
	t.Helper()
	if err := h.p.SendMessage(text, nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	testutil.WaitFor(t, 10*time.Second, func() bool { return h.rec.count("message_complete") >= n })
}

// lines returns what testcodex received: the start record first, then each
// line relay wrote.
func (h *codexHarness) lines(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(h.out)
	if err != nil {
		t.Fatalf("read testcodex out: %v", err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("testcodex out line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func (h *codexHarness) sent(t *testing.T, method string) map[string]any {
	t.Helper()
	for _, m := range h.lines(t) {
		if m["method"] == method {
			return m
		}
	}
	t.Fatalf("relay never sent %s; got %v", method, h.lines(t))
	return nil
}

func TestCodexSpawn_HandshakeAndTurn(t *testing.T) {
	t.Setenv("RELAY_PROJECT_TOKEN", "leaked-project-token")
	h := newCodexHarness(t, codexFixtureOK, nil, func(s *sessionstypes.Session) { s.SystemPrompt = "Be brief." })
	h.start(t)
	h.turn(t, "How many entries?", 1)

	start := h.lines(t)[0]
	argv, _ := start["argv"].([]any)
	if len(argv) != 2 || argv[1] != "app-server" {
		t.Errorf("child argv = %v, want exactly [codex app-server]", argv)
	}
	var env []string
	for _, e := range start["env"].([]any) {
		env = append(env, e.(string))
	}
	assertNoForbiddenSecrets(t, "codex child env", env)
	if !contains(env, "RELAY_SESSION_ID=cx1") {
		t.Errorf("child env lacks RELAY_SESSION_ID=cx1")
	}
	if got, _ := filepath.EvalSymlinks(start["cwd"].(string)); got != mustEval(t, h.dir) {
		t.Errorf("child cwd = %q, want the session directory %q", got, h.dir)
	}

	init := h.sent(t, "initialize")
	if init["id"] != float64(1) || fmt.Sprint(init["params"]) != "map[clientInfo:map[name:relay version:1]]" {
		t.Errorf("initialize = %v", init)
	}
	if _, has := h.sent(t, "initialized")["id"]; has {
		t.Error("initialized must be a notification, it carries an id")
	}
	ts := h.sent(t, "thread/start")
	want := map[string]any{
		"cwd": h.dir, "model": "gpt-6-luna", "approvalPolicy": "never",
		"sandbox": "danger-full-access", "developerInstructions": "Be brief.",
	}
	if ts["id"] != float64(2) || fmt.Sprint(ts["params"]) != fmt.Sprint(want) {
		t.Errorf("thread/start = %v, want id 2 params %v", ts, want)
	}
	tu := h.sent(t, "turn/start")
	if fmt.Sprint(tu["params"]) != fmt.Sprintf("map[input:[map[text:How many entries? type:text]] threadId:%s]", codexFixtureThread) {
		t.Errorf("turn/start params = %v", tu["params"])
	}

	if got := h.rec.text(); got != "There are 2 entries." {
		t.Errorf("assistant text = %q, want %q", got, "There are 2 entries.")
	}
	if got := string(h.p.GetState()); got != `{"threadId":"`+codexFixtureThread+`"}` {
		t.Errorf("GetState = %s", got)
	}
}

func TestCodexSpawn_ResumeSendsThreadResumeWithBypassedApprovals(t *testing.T) {
	h := newCodexHarness(t, codexFixtureOK, nil, nil)
	h.p.RestoreState(json.RawMessage(`{"threadId":"t-prev"}`))
	h.start(t)

	tr := h.sent(t, "thread/resume")
	want := map[string]any{
		"threadId": "t-prev", "cwd": h.dir, "model": "gpt-6-luna",
		"approvalPolicy": "never", "sandbox": "danger-full-access",
	}
	if fmt.Sprint(tr["params"]) != fmt.Sprint(want) {
		t.Errorf("thread/resume params = %v, want %v (no developerInstructions for an empty prompt)", tr["params"], want)
	}
	for _, m := range h.lines(t) {
		if m["method"] == "thread/start" {
			t.Error("a restored thread id must resume, not start a new thread")
		}
	}
}

func TestCodexSpawn_EmptyModelFailsStart(t *testing.T) {
	h := newCodexHarness(t, codexFixtureOK, nil, func(s *sessionstypes.Session) { s.Model = "codex/" })
	err := h.p.Start()
	if err == nil || !strings.Contains(err.Error(), "codex session has no model") {
		t.Fatalf("Start error = %v, want %q", err, "codex session has no model")
	}
}

func TestCodexSpawn_StopGenerationInterruptsCurrentTurn(t *testing.T) {
	h := newCodexHarness(t, codexFixtureOK, map[string]string{"TESTCODEX_HANG": "1"}, nil)
	h.start(t)
	if err := h.p.SendMessage("go", nil); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, 10*time.Second, func() bool { return h.rec.text() == "There are 2 entries." })

	h.p.StopGeneration()
	testutil.WaitFor(t, 10*time.Second, func() bool {
		for _, m := range h.lines(t) {
			if m["method"] == "turn/interrupt" {
				return true
			}
		}
		return false
	})
	want := fmt.Sprintf("map[threadId:%s turnId:%s]", codexFixtureThread, codexFixtureTurn1)
	if got := fmt.Sprint(h.sent(t, "turn/interrupt")["params"]); got != want {
		t.Errorf("turn/interrupt params = %s, want %s", got, want)
	}
}

// Every server request is answered, whatever it is, and the turn still ends.
func TestCodexSpawn_ServerRequestsAreAnsweredAndTurnCompletes(t *testing.T) {
	const unsupported = `{"code":-32601,"message":"unsupported by relay"}`
	cases := []struct {
		method, want string // want is the reply's result or error, as JSON
		isError      bool
		warns        bool
	}{
		{"item/commandExecution/requestApproval", `{"decision":"decline"}`, false, true},
		{"item/fileChange/requestApproval", `{"decision":"decline"}`, false, true},
		{"applyPatchApproval", `{"decision":"denied"}`, false, true},
		{"execCommandApproval", `{"decision":"denied"}`, false, true},
		{"mcpServer/elicitation/request", `{"action":"decline"}`, false, false},
		{"item/tool/requestUserInput", unsupported, true, true},
		{"item/permissions/requestApproval", unsupported, true, true},
		{"item/tool/call", unsupported, true, true},
		{"account/chatgptAuthTokens/refresh", unsupported, true, true},
		{"attestation/generate", unsupported, true, true},
		{"acme/futureRequest", unsupported, true, true},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			logs := captureDebugJSON(t)
			req := fmt.Sprintf(`{"id":900,"method":%q,"params":{}}`, c.method)
			h := newCodexHarness(t, codexFixtureOK, map[string]string{"TESTCODEX_REQUEST": req}, nil)
			h.start(t)
			h.turn(t, "go", 1)

			var reply map[string]any
			for _, m := range h.lines(t) {
				if m["id"] == float64(900) && m["method"] == nil {
					reply = m
				}
			}
			if reply == nil {
				t.Fatalf("server request %s was never answered", c.method)
			}
			key := "result"
			if c.isError {
				key = "error"
			}
			got, _ := json.Marshal(reply[key])
			if string(got) != c.want {
				t.Errorf("reply %s = %s, want %s", key, got, c.want)
			}
			if c.warns && !warnedAbout(logs, "cx1", c.method) {
				t.Errorf("no WARN naming %s; log:\n%s", c.method, logs.all())
			}
		})
	}
}

func warnedAbout(b *warnBuffer, session, method string) bool {
	for _, l := range strings.Split(b.all(), "\n") {
		if strings.Contains(l, `"level":"WARN"`) && strings.Contains(l, `"`+session+`"`) && strings.Contains(l, method) {
			return true
		}
	}
	return false
}

// An unrecognised line mid-turn warns once, reaches raw_output, and leaves
// the session alive and answering.
func TestCodexSpawn_UnrecognisedLineKeepsSessionAlive(t *testing.T) {
	cases := map[string]string{
		"unknown method":    `{"method":"acme/futureEvent","params":{}}`,
		"unknown item type": `{"method":"item/started","params":{"item":{"type":"acmeHologram","id":"i1"}}}`,
		"not json":          `this is not json`,
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			logs := captureDebugJSON(t)
			h := newCodexHarness(t, codexFixtureOK, map[string]string{"TESTCODEX_INJECT": inject}, nil)
			h.start(t)
			h.turn(t, "first", 1)

			if !h.p.Alive() {
				t.Fatal("an unrecognised line ended the session")
			}
			h.turn(t, "second", 2)
			if got := h.rec.text(); !strings.HasSuffix(got, "pong") {
				t.Errorf("second turn text = %q, want it to end with pong", got)
			}
			if n := h.rec.count("raw_output"); n != 2 {
				t.Errorf("raw_output count = %d, want 2 (one per turn's injected line)", n)
			}
			if n := strings.Count(logs.all(), "codex: unrecognised event"); n != 1 {
				t.Errorf("unrecognised-event warnings = %d, want 1 across both turns; log:\n%s", n, logs.all())
			}
			for _, k := range []string{"error", "process_exited"} {
				if h.rec.count(k) != 0 {
					t.Errorf("%s emitted; kinds = %v", k, h.rec.all())
				}
			}
		})
	}
}

func TestCodexSpawn_ChildDeathReportsProcessExited(t *testing.T) {
	h := newCodexHarness(t, codexFixtureOK, map[string]string{"TESTCODEX_DIE": "1"}, nil)
	h.start(t)
	if err := h.p.SendMessage("go", nil); err != nil {
		t.Fatal(err)
	}
	testutil.WaitFor(t, 10*time.Second, func() bool { return h.rec.count("process_exited") == 1 })
	if h.p.Alive() {
		t.Error("Alive() true after the child was killed")
	}
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
