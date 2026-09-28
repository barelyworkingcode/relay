package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "devboxverify-home-")
	if err == nil {
		err = os.Setenv("HOME", home)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "isolate HOME:", err)
		os.Exit(2)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// notPass marks a case whose exact non-PASS state the contract leaves open.
const notPass state = ""

func row(kind, projectID, outcome, errMsg, args string) *audit.AuditEvent {
	return &audit.AuditEvent{Actor: audit.AuditActor{Kind: kind, ProjectID: projectID}, Outcome: outcome, Error: errMsg, Args: json.RawMessage(args)}
}

// mutCase edits a known-PASS input so each case breaks exactly one clause.
type mutCase[T any] struct {
	name string
	mut  func(*T)
	want state
}

func results(ss ...state) []result {
	out := make([]result, len(ss))
	for i, s := range ss {
		out[i] = result{ID: fmt.Sprint("j", i), State: s}
	}
	return out
}

func checkState(t *testing.T, got result, want state) {
	t.Helper()
	if want == notPass && got.State == statePass || want != notPass && got.State != want {
		t.Fatalf("got %s (detail %q), want %q (empty: anything but PASS)", got.State, got.Detail, want)
	}
}

func TestBuildMatches(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	type bs = []debug.BuildSetting
	rev := debug.BuildSetting{Key: "vcs.revision", Value: head}
	clean := debug.BuildSetting{Key: "vcs.modified", Value: "false"}
	cases := []struct {
		name     string
		settings bs
		ok       bool
	}{
		{"clean build of head", bs{{Key: "vcs", Value: "git"}, rev, clean}, true},
		{"other revision", bs{{Key: "vcs.revision", Value: "fedcba9876543210fedcba9876543210fedcba98"}, clean}, false},
		{"dirty tree", bs{rev, {Key: "vcs.modified", Value: "true"}}, false},
		{"no modified flag", bs{rev}, false},
		{"no revision", bs{clean}, false},
	}
	for _, c := range cases {
		if err := buildMatches(c.settings, head); (err == nil) != c.ok {
			t.Errorf("%s: buildMatches = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestParseWorldSummary(t *testing.T) {
	out := "CHECK\tacme\tOK\nSUMMARY\tpass=3\tfail=2\nretrying\nSUMMARY\tpass=11\tfail=1\n"
	pass, fail, err := parseWorldSummary(out)
	if err != nil || pass != 11 || fail != 1 {
		t.Fatalf("parseWorldSummary = %d, %d, %v; want 11, 1, nil (the last SUMMARY line)", pass, fail, err)
	}
	for _, bad := range []string{"", "CHECK\tacme\tOK\n", "note: SUMMARY\tpass=1\tfail=0\n"} {
		if _, _, err := parseWorldSummary(bad); err == nil {
			t.Errorf("parseWorldSummary(%q) returned no error", bad)
		}
	}
}

func TestTallyAndStatus(t *testing.T) {
	cases := []struct {
		name   string
		in     []result
		want   map[state]int
		exit   int
		status string
	}{
		{"pass and notrun", results(statePass, stateNotRun, statePass), map[state]int{statePass: 2, stateNotRun: 1}, 0, "success"},
		{"a failure beside a blocked", results(stateBlocked, stateFail, stateNotRun), map[state]int{stateBlocked: 1, stateFail: 1, stateNotRun: 1}, 1, "failure"},
		{"a blocked journey", results(stateBlocked, statePass), map[state]int{statePass: 1, stateBlocked: 1}, 1, "error"},
	}
	for _, c := range cases {
		counts, exit := tally(c.in)
		if got := statusState(c.in); exit != c.exit || got != c.status {
			t.Errorf("%s: exit = %d, status = %q; want %d, %q", c.name, exit, got, c.exit, c.status)
		}
		for _, s := range []state{statePass, stateFail, stateBlocked, stateNotRun} {
			if counts[s] != c.want[s] {
				t.Errorf("%s: counts[%s] = %d, want %d", c.name, s, counts[s], c.want[s])
			}
		}
	}
}

func TestClassifyBlankModel(t *testing.T) {
	const name, acme, notAllowed = "verify-0a1b2c3d", "p1", `template "chat" is not available for this project`
	msg := fmt.Sprintf("chat session %q has no model; choose a model and try again", name)
	body := func(e string) []byte { b, _ := json.Marshal(map[string]string{"error": e}); return b }
	const chatArgs = `{"session_kind":"chat","sandbox":false}`
	const launchedArgs = `{"session_id":"s1","session_kind":"chat","sandbox":false}`
	type in struct {
		status int
		body   []byte
		row    *audit.AuditEvent
	}
	cases := []mutCase[in]{
		{"audited refusal", func(*in) {}, statePass},
		{"credential rejected", func(c *in) { c.status, c.body, c.row = 401, body("unauthorized"), nil }, stateBlocked},
		{"chat template not allowed", func(c *in) {
			c.status, c.body, c.row = 403, body(notAllowed), row("control", acme, "denied", notAllowed, chatArgs)
		}, stateBlocked},
		{"refusal not audited", func(c *in) { c.row = nil }, stateFail},
		{"launch accepted", func(c *in) {
			c.status, c.body, c.row = 201, []byte(`{"id":"s1"}`), row("control", acme, "ok", "", launchedArgs)
		}, stateFail},
		{"launched then failed downstream", func(c *in) {
			c.status, c.body, c.row = 502, body("launch failed"), row("control", acme, "error", "launch failed", launchedArgs)
		}, stateFail},
		{"body error differs", func(c *in) { c.body = body("model required") }, notPass},
		{"row error differs", func(c *in) { c.row.Error = "model required" }, notPass},
		{"row outcome denied", func(c *in) { c.row.Outcome = "denied" }, notPass},
		{"actor not control", func(c *in) { c.row.Actor.Kind = "operator" }, notPass},
		{"row for another project", func(c *in) { c.row.Actor.ProjectID = "p2" }, notPass},
		{"row for another kind", func(c *in) { c.row.Args = json.RawMessage(`{"session_kind":"claude","sandbox":false}`) }, notPass},
		{"row names a session", func(c *in) { c.row.Args = json.RawMessage(launchedArgs) }, notPass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := in{400, body(msg), row("control", acme, "error", msg, chatArgs)}
			c.mut(&v)
			checkState(t, classifyBlankModel(v.status, v.body, v.row, name, acme, "Acme"), c.want)
		})
	}
}

func TestClassifyOversized(t *testing.T) {
	capped := strings.Repeat("A", 256) + "…" // 257 runes, 259 bytes
	type in struct {
		reason   string
		row      *audit.AuditEvent
		rowBytes int
	}
	uncapped := func(c *in) { c.row.Error, c.rowBytes = strings.Repeat("A", 300000), 300212 }
	cases := []mutCase[in]{
		{"capped row at the size limit", func(*in) {}, statePass},
		{"uncapped row", uncapped, stateFail},
		{"error over 257 runes", func(c *in) { c.row.Error = strings.Repeat("A", 257) + "…" }, notPass},
		{"error not marked truncated", func(c *in) { c.row.Error = strings.Repeat("A", 257) }, notPass},
		{"row over 4096 bytes", func(c *in) { c.rowBytes = 4097 }, notPass},
		{"actor not operator", func(c *in) { c.row.Actor.Kind = "control" }, notPass},
		{"outcome not denied", func(c *in) { c.row.Outcome = "error" }, notPass},
		{"other refusal reason", func(c *in) { c.reason = "template_not_allowed" }, notPass},
		{"no audit row", func(c *in) { c.row = nil }, notPass},
	}
	run := func(mut func(*in)) result {
		v := in{"template_unknown", row("operator", "p1", "denied", capped, `{"sandbox":false}`), 4096}
		mut(&v)
		return classifyOversized(v.reason, v.row, v.rowBytes)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { checkState(t, run(c.mut), c.want) })
	}
	if d := run(uncapped).Detail; !strings.Contains(d, "300212") {
		t.Errorf("uncapped row detail %q does not name its 300212 bytes", d)
	}
}

func TestClassifyReach(t *testing.T) {
	// The PTY echoes every byte typed into the probe shell, so the transcript
	// always holds the script itself; a marker must appear only as output.
	script := reachScript("PROJECT.md", "/w/Other/PROJECT.md")
	for _, m := range []string{"ACME_OK", "ACME_DENIED", "GLOBEX_OK", "GLOBEX_DENIED"} {
		if strings.Contains(script, m) {
			t.Fatalf("reachScript contains the literal marker %s, so its echo alone could pass", m)
		}
	}
	echo := strings.ReplaceAll(script, "\n", "\r\n")
	const denied = "cat: Globex/PROJECT.md: Operation not permitted\r\nGLOBEX_DENIED\r\n"
	const missing = "cat: Globex/PROJECT.md: No such file or directory\r\nGLOBEX_DENIED\r\n"
	good := echo + "ACME_OK\r\n" + denied
	type in struct {
		reason, transcript string
		exited             bool
		code               int
		row                *audit.AuditEvent
	}
	cases := []mutCase[in]{
		{"acme read, globex refused", func(*in) {}, statePass},
		{"echo only", func(c *in) { c.transcript = echo }, notPass},
		{"globex readable", func(c *in) { c.transcript = echo + "ACME_OK\r\nGLOBEX_OK\r\n" }, stateFail},
		{"acme not read", func(c *in) { c.transcript = echo + denied }, notPass},
		{"globex missing, not refused", func(c *in) { c.transcript = echo + "ACME_OK\r\n" + missing }, notPass},
		{"shell never exited", func(c *in) { c.exited = false }, notPass},
		{"nonzero exit", func(c *in) { c.code = 1 }, notPass},
		{"no audit row", func(c *in) { c.row = nil }, notPass},
		{"row not sandboxed", func(c *in) { c.row.Args = json.RawMessage(`{"session_id":"s1","sandbox":false}`) }, notPass},
		{"row outcome error", func(c *in) { c.row.Outcome = "error" }, notPass},
	}
	for _, r := range []string{"template_unknown", "template_not_allowed", "inside_session", "peer_confined"} {
		cases = append(cases, mutCase[in]{"refused " + r, func(c *in) { c.reason, c.transcript, c.exited, c.row = r, "", false, nil }, stateBlocked})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := in{"", good, true, 0, row("operator", "p1", "ok", "", `{"session_id":"s1","template_id":"world-probe","sandbox":true}`)}
			c.mut(&v)
			checkState(t, classifyReach("Acme", "Other", v.reason, v.transcript, v.exited, v.code, v.row), c.want)
		})
	}
}

func TestRenderCommentScrubsHomeAndListsEveryJourney(t *testing.T) {
	ev := evidence{
		PR: 7, Commit: "0123456789abcdef0123456789abcdef01234567", ToolCommit: "89abcdef0123456789abcdef0123456789abcdef",
		WorldSummary: "pass=11 fail=0", Home: "/Users/someone",
		Results: []result{
			{ID: "blank-model-refused", State: statePass, Detail: "refused and audited"},
			{ID: "permission-mode-restart", State: stateNotRun, Detail: "no host projects"},
			{ID: "oversized-launch-audit-capped", State: stateFail, Detail: "row is 300212 bytes"},
			{ID: "acme-sandbox-reach", State: stateBlocked, Detail: "credential at /Users/someone/.config/verify is group-readable; move it under /Users/someone/.private"},
		},
	}
	out := renderComment(ev)
	for _, want := range []string{ev.Commit, ev.ToolCommit, ev.WorldSummary, "~/.config/verify"} {
		if !strings.Contains(out, want) {
			t.Errorf("comment lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ev.Home) {
		t.Errorf("comment leaks the home directory:\n%s", out)
	}
	for _, r := range ev.Results {
		if !slices.ContainsFunc(strings.Split(out, "\n"), func(l string) bool {
			return strings.Contains(l, r.ID) && strings.Contains(l, string(r.State))
		}) {
			t.Errorf("no line carries %s with %s:\n%s", r.ID, r.State, out)
		}
	}
}

func TestFormatLineIsOneTabSeparatedLineWithoutHome(t *testing.T) {
	got := formatLine("/Users/someone", "JOURNEY", "acme-sandbox-reach", "FAIL", "read /Users/someone/a \n then\t\t/Users/someone/b")
	if want := "JOURNEY\tacme-sandbox-reach\tFAIL\tread ~/a then ~/b"; got != want {
		t.Fatalf("formatLine = %q, want %q", got, want)
	}
}

// runIsolated runs the harness in-process against the marker at marker.
// Every door a run could open before its journeys leaves a trace: gh and git
// on PATH and the world checkout's scripts log to calls, and the browser
// lock is a scratch path.
func runIsolated(t *testing.T, marker string, isVM bool, args ...string) (code int, stdout []string, calls string, lockTaken bool) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeBin(t, bin, "gh", "echo gh >> '"+log+"'\nexit 1\n")
	fakeBin(t, bin, "git", "echo git >> '"+log+"'\nexec '"+realGit+"' \"$@\"\n")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("DEVBOXWORLD_MARKER", marker)
	t.Setenv("EVE_BROWSER_LOCK", filepath.Join(dir, "browser.lock"))
	// Deliberate: should the world checks pass, the first existing check
	// stops the run before it reaches the app.
	t.Setenv("RELAY_SESSION_ID", "s1")
	if m, err := readMarker(marker, vmIs(true, nil)); err == nil {
		for _, s := range []string{"bootstrap.sh", "verify.sh", "reset.sh"} {
			fakeBin(t, m.WorldCheckout, s, "echo "+s+" >> '"+log+"'\n")
		}
	}
	savedVM, savedArgs, savedOut := vmCheck, os.Args, os.Stdout
	t.Cleanup(func() { vmCheck, os.Args, os.Stdout = savedVM, savedArgs, savedOut })
	out, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	vmCheck = func() (bool, error) { return isVM, nil }
	os.Args, os.Stdout = append([]string{"devboxverify"}, args...), out
	code = run()
	vmCheck, os.Args, os.Stdout = savedVM, savedArgs, savedOut
	_ = out.Close()
	raw, _ := os.ReadFile(out.Name())
	if s := strings.TrimSuffix(string(raw), "\n"); s != "" {
		stdout = strings.Split(s, "\n")
	}
	c, _ := os.ReadFile(log)
	_, statErr := os.Stat(filepath.Join(dir, "browser.lock"))
	return code, stdout, string(c), statErr == nil
}

func scratchMarker(t *testing.T, checkout string, version int) string {
	t.Helper()
	return writeMode(t, filepath.Join(t.TempDir(), "machine.json"),
		markerDoc(checkout, func(m map[string]any) { m["world_version"] = version }), 0o600)
}

func TestPreflightRefusesBeforeAnyLockScriptOrNetwork(t *testing.T) {
	const machineOK = "PREFLIGHT\tmachine\tOK\tvm; world v1"
	cases := []struct {
		name   string
		marker func(t *testing.T) string
		isVM   bool
		want   []string
	}{
		{"absent marker", func(t *testing.T) string { return filepath.Join(t.TempDir(), "machine.json") }, true,
			[]string{"PREFLIGHT\tmachine\tFAIL\tnot a test machine: run devboxWorld bootstrap on a VM"}},
		{"not a VM", func(t *testing.T) string { return scratchMarker(t, writeWorld(t, nil), 1) }, false,
			[]string{"PREFLIGHT\tmachine\tFAIL\tnot a test machine: not a VM: kern.hv_vmm_present is not 1; run devboxWorld bootstrap on a VM"}},
		{"pin mismatch", func(t *testing.T) string { return scratchMarker(t, writeWorld(t, nil), 2) }, true,
			[]string{"PREFLIGHT\tmachine\tOK\tvm; world v2", "PREFLIGHT\tpin\tFAIL\tBLOCKED fixture: this machine's world is v2; relay needs v1"}},
		{"missing fixture", func(t *testing.T) string { return scratchMarker(t, writeWorld(t, dropFixtures("project:globex")), 1) }, true,
			[]string{machineOK, "PREFLIGHT\tpin\tOK\tv1", "PREFLIGHT\tfixtures\tFAIL\tBLOCKED fixture: acme-sandbox-reach needs project:globex"}},
		{"unreadable world data", func(t *testing.T) string { return scratchMarker(t, t.TempDir(), 1) }, true,
			[]string{machineOK, "PREFLIGHT\tpin\tOK\tv1", "PREFLIGHT\tfixtures\tFAIL\tBLOCKED fixture: world data is not readable"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, stdout, calls, locked := runIsolated(t, c.marker(t), c.isVM)
			if code != 2 || !slices.Equal(stdout, c.want) {
				t.Errorf("exit %d, stdout:\n%s\nwant exit 2, stdout:\n%s", code, strings.Join(stdout, "\n"), strings.Join(c.want, "\n"))
			}
			if calls != "" || locked {
				t.Errorf("preflight refusal still called %q, lock taken %v", calls, locked)
			}
		})
	}
}

func TestWorldChecksPassThenTheExistingChecksRun(t *testing.T) {
	code, stdout, calls, locked := runIsolated(t, scratchMarker(t, writeWorld(t, nil), 1), true)
	want := []string{"PREFLIGHT\tmachine\tOK\tvm; world v1", "PREFLIGHT\tpin\tOK\tv1",
		fmt.Sprintf("PREFLIGHT\tfixtures\tOK\t4 fixtures for %d journeys", len(contractNeeds()))}
	if code != 2 || len(stdout) != 4 || !slices.Equal(stdout[:3], want) || !strings.HasPrefix(stdout[3], "PREFLIGHT\tsession\tFAIL\t") {
		t.Errorf("exit %d, stdout:\n%s\nwant the world checks OK, then session FAIL", code, strings.Join(stdout, "\n"))
	}
	if !strings.Contains(calls, "git") {
		t.Errorf("calls = %q; the sentinel saw no git call, so it cannot prove the refusals ran none", calls)
	}
	if strings.Contains(calls, "gh") || strings.Contains(calls, ".sh") || locked {
		t.Errorf("stopped at session, yet called %q, lock taken %v", calls, locked)
	}
}

func TestWorldFlagIsRefused(t *testing.T) {
	w := writeWorld(t, nil)
	code, stdout, calls, locked := runIsolated(t, scratchMarker(t, w, 1), true, "--world", w)
	if code != 2 || len(stdout) != 0 || calls != "" || locked {
		t.Errorf("--world: exit %d, stdout %q, calls %q, lock %v; want a usage refusal, exit 2, nothing run", code, stdout, calls, locked)
	}
}

func TestRenderCommentRunTimeFollowsToolCommit(t *testing.T) {
	ev := evidence{PR: 7, Commit: "c1", ToolCommit: "t1", WorldSummary: "pass=1 fail=0", RunTime: 125 * time.Second}
	lines := strings.Split(renderComment(ev), "\n")
	i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "| Tool commit |") })
	if i < 0 || i+1 >= len(lines) || lines[i+1] != "| Run time | 125 s |" {
		t.Errorf("no `| Run time | 125 s |` row after Tool commit:\n%s", strings.Join(lines, "\n"))
	}
}
