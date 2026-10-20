package features

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaye2e/harness"
)

// b181Dir is the sandbox Claude folder an instance's launches share.
func b181Dir(i *harness.Instance) string {
	return filepath.Join(i.Home, "Library", "Application Support", "relay", "sandbox-claude")
}

// b181Links are the ~/.claude entries relay links into the folder.
var b181Links = []string{"settings.json", "CLAUDE.md", "skills", "agents", "commands", "projects", "sessions"}

// b181Fixture plants a ~/.claude with the linkable entries and a login that
// must never be linked or copied. The bytes are placeholders, not tokens.
func b181Fixture(t *testing.T, i *harness.Instance) {
	t.Helper()
	claude := filepath.Join(i.Home, ".claude")
	for _, d := range []string{"skills", "agents", "commands"} {
		if err := os.MkdirAll(filepath.Join(claude, d), 0o700); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}
	files := map[string]string{
		filepath.Join(claude, "settings.json"):      "{}",
		filepath.Join(claude, "CLAUDE.md"):          "acme notes",
		filepath.Join(claude, ".credentials.json"):  "acme-placeholder",
		filepath.Join(i.Home, ".claude.json"):       "acme-placeholder",
		filepath.Join(claude, "skills", "note.txt"): "acme-placeholder",
	}
	for p, b := range files {
		if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
}

// b181AssertLinks checks each name in names is a link to ~/.claude/<name>.
func b181AssertLinks(t *testing.T, i *harness.Instance, names []string) {
	t.Helper()
	w := b181Dir(i)
	for _, n := range names {
		got, err := os.Readlink(filepath.Join(w, n))
		if err != nil {
			t.Fatalf("%s in the sandbox Claude folder is not a link: %v", n, err)
		}
		if want := filepath.Join(i.Home, ".claude", n); got != want {
			t.Fatalf("link %s points at %q, want %q", n, got, want)
		}
	}
}

// b181SessionEnv launches a sandboxed claude session, runs one turn and
// returns the environment of its start line.
func b181SessionEnv(t *testing.T, i *harness.Instance, pid string) map[string]string {
	t.Helper()
	sid := g1sLaunchClaude(t, i, pid)
	g1sSendText(t, i, sid, "hi", "echo: hi")
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "start" && len(c.Argv) > 0 && c.Argv[0] != "debug" {
			return c.Env
		}
	}
	t.Fatalf("no session start line found")
	return nil
}

// b181Shell runs a shell command in a session and returns the reply text.
func b181Shell(t *testing.T, i *harness.Instance, sid, cmd string) string {
	t.Helper()
	var reply struct {
		Text string `json:"text"`
	}
	i.MustCLI("session", "message", "--id", sid, "--text", "!sh "+cmd, "--json").JSON(t, &reply)
	return reply.Text
}

// A sandboxed claude session starts with CLAUDE_CONFIG_DIR set to the folder.
func TestBug181SandboxedClaudeSessionConfigDir(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	env := b181SessionEnv(t, i, pid)
	if got, want := env["CLAUDE_CONFIG_DIR"], b181Dir(i); got != want {
		t.Fatalf("sandboxed claude session CLAUDE_CONFIG_DIR = %q (env %v), want %q", got, env, want)
	}
}

// A sandboxed claude-code terminal starts with CLAUDE_CONFIG_DIR set to the
// same folder.
func TestBug181SandboxedClaudeTerminalConfigDir(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	env := b181Terminal(t, i, pid)
	if got, want := env["CLAUDE_CONFIG_DIR"], b181Dir(i); got != want {
		t.Fatalf("sandboxed claude-code terminal CLAUDE_CONFIG_DIR = %q (env %v), want %q", got, env, want)
	}
}

// An unsandboxed launch gets no CLAUDE_CONFIG_DIR and relay makes no folder.
func TestBug181UnsandboxedClaudeNoConfigDir(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, map[string]any{"sandbox": false})
	env := b181Terminal(t, i, pid)
	if v, ok := env["CLAUDE_CONFIG_DIR"]; ok {
		t.Fatalf("unsandboxed claude-code terminal got CLAUDE_CONFIG_DIR=%q, want it unset", v)
	}
	if _, err := os.Lstat(b181Dir(i)); !os.IsNotExist(err) {
		t.Fatalf("the sandbox Claude folder after an unsandboxed launch: Lstat error %v, want not exist", err)
	}
}

// A CLAUDE_CONFIG_DIR in the template's own env reaches the child unchanged.
func TestBug181TemplateConfigDirWins(t *testing.T) {
	t.Parallel()
	const own = "/acme/claude-own"
	i, pid := b181Start(t, map[string]any{"env": map[string]string{"CLAUDE_CONFIG_DIR": own}})
	env := b181Terminal(t, i, pid)
	if got := env["CLAUDE_CONFIG_DIR"]; got != own {
		t.Fatalf("template env CLAUDE_CONFIG_DIR=%q reached the child as %q (env %v)", own, got, env)
	}
}

// The session's profile lets it write inside the folder and refuses a write
// beside it.
func TestBug181SandboxClaudeDirWritable(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	sid := g1sLaunchClaude(t, i, pid)
	inside := filepath.Join(b181Dir(i), "acme-probe")
	if got := b181Shell(t, i, sid, "touch "+agShQuote(inside)); got != "exit: 0" {
		t.Fatalf("a write inside the sandbox Claude folder answered %q, want exit: 0", got)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("the write inside the folder left no file: %v", err)
	}
	outside := filepath.Join(i.Home, "Library", "Application Support", "relay", "acme-outside")
	if got := b181Shell(t, i, sid, "touch "+agShQuote(outside)); got == "exit: 0" {
		t.Fatalf("a write beside the sandbox Claude folder answered exit: 0, want a refusal")
	}
	if _, err := os.Lstat(outside); err == nil {
		t.Fatalf("the refused write left %s", outside)
	}
}

// A sandboxed claude-code terminal can write inside the folder it is given.
func TestBug181SandboxClaudeTerminalDirWritable(t *testing.T) {
	t.Parallel()
	i, _ := b181Start(t, map[string]any{
		"command": "/bin/sh",
		"args":    []string{"-c", `touch "$CLAUDE_CONFIG_DIR/acme-probe"`},
	})
	dir := g1tDir(i, "acme-updater")
	if res := i.StartCLITTY(harness.CLIOpts{}, dir, "sandbox", "claude-code").Wait(); res.Code != 0 {
		t.Fatalf("relay sandbox claude-code exited %d, want 0\nstderr: %s", res.Code, res.Stderr)
	}
	if _, err := os.Stat(filepath.Join(b181Dir(i), "acme-probe")); err != nil {
		t.Fatalf("the terminal's write left no file in the sandbox Claude folder: %v", err)
	}
}

// The folder is mode 0700, holds links to the listed entries and never a
// login or a copy of a file.
func TestBug181SandboxClaudeDirSeeded(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	b181Fixture(t, i)
	b181SessionEnv(t, i, pid)

	w := b181Dir(i)
	st, err := os.Lstat(w)
	if err != nil {
		t.Fatalf("the sandbox Claude folder: %v", err)
	}
	if !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("the sandbox Claude folder is %v, want a directory of mode 0700", st.Mode())
	}
	b181AssertLinks(t, i, b181Links)
	for _, n := range []string{"projects", "sessions"} {
		if s, err := os.Stat(filepath.Join(i.Home, ".claude", n)); err != nil || !s.IsDir() {
			t.Fatalf("link target %s is not a directory: %v", n, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(w, ".credentials.json")); err == nil {
		t.Fatalf("the sandbox Claude folder holds .credentials.json, want it absent")
	}
	entries, err := os.ReadDir(w)
	if err != nil {
		t.Fatalf("reading the folder: %v", err)
	}
	allowed := map[string]bool{}
	for _, n := range b181Links {
		allowed[n] = true
	}
	allowed[".claude.json"] = true
	for _, e := range entries {
		if !allowed[e.Name()] {
			t.Fatalf("unexpected entry %q in the sandbox Claude folder", e.Name())
		}
		if e.Type().IsRegular() {
			b, _ := os.ReadFile(filepath.Join(w, e.Name()))
			if bytes.Contains(b, []byte("acme-placeholder")) {
				t.Fatalf("%s in the folder is a copy of a ~/.claude file", e.Name())
			}
		}
	}
}

// Seeding keeps an entry that already exists, and a second launch changes
// nothing.
func TestBug181SandboxClaudeDirKeepsOwnEntries(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	b181Fixture(t, i)
	w := b181Dir(i)
	if err := os.MkdirAll(w, 0o700); err != nil {
		t.Fatalf("creating the folder: %v", err)
	}
	own := filepath.Join(w, "settings.json")
	if err := os.WriteFile(own, []byte("acme-own"), 0o600); err != nil {
		t.Fatalf("planting settings.json: %v", err)
	}
	others := b181Links[1:]
	for round := 1; round <= 2; round++ {
		g1sLaunchClaude(t, i, pid)
		st, err := os.Lstat(own)
		if err != nil || !st.Mode().IsRegular() {
			t.Fatalf("launch %d: the planted settings.json is no longer a regular file (%v, %v)", round, st, err)
		}
		if b, _ := os.ReadFile(own); string(b) != "acme-own" {
			t.Fatalf("launch %d: settings.json holds %q, want acme-own", round, b)
		}
		b181AssertLinks(t, i, others)
	}
}

// A folder path that is a link is refused with sandbox_unavailable.
func TestBug181SandboxClaudeDirNotADirRefused(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	elsewhere := filepath.Join(i.Home, "acme-elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatalf("creating %s: %v", elsewhere, err)
	}
	w := b181Dir(i)
	if err := os.MkdirAll(filepath.Dir(w), 0o700); err != nil {
		t.Fatalf("creating the parent: %v", err)
	}
	if err := os.Symlink(elsewhere, w); err != nil {
		t.Fatalf("planting the link: %v", err)
	}
	trace := harness.NewTrace(t)
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions",
		map[string]any{"projectId": pid, "model": "sonnet"}, harness.ReqOpts{Trace: trace})
	if resp.Status != 400 {
		t.Fatalf("POST /api/sessions with a linked folder answered %d, want 400: %s", resp.Status, resp.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.launch", Trace: trace,
		Fields: map[string]any{"reason": "sandbox_unavailable"}}, b181Deadline)
	if !strings.Contains(string(resp.Body), w) {
		t.Fatalf("the 400 body does not name %s: %s", w, resp.Body)
	}
	if es, _ := os.ReadDir(elsewhere); len(es) != 0 {
		t.Fatalf("the link target holds %d entries, want it left empty", len(es))
	}
}
