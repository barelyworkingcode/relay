package features

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

const (
	ctDeadline = 60 * time.Second
	ctSetting  = "~/.config/acme-ltt/token"
	ctFD       = "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR"
	ctShell    = "acme-shell"
)

// ctOpts shape one instance for the claude_token_file tests.
type ctOpts struct {
	setting  string         // the claude_token_file value; "" leaves it unset
	override map[string]any // keys set on the claude-code template
	shellCmd string         // when set, adds a sandboxed non-Claude template running it
}

// ctStart boots an instance with a project that is granted the claude-code
// template (and the acme-shell template when shellCmd is set).
func ctStart(t *testing.T, o ctOpts) (*harness.Instance, string) {
	t.Helper()
	opts := g1sOptions(harness.Options{})
	if o.setting != "" {
		opts.Settings = map[string]json.RawMessage{"claude_token_file": ctJSON(t, o.setting)}
	}
	granted := []string{"claude-code"}
	if o.shellCmd != "" {
		granted = append(granted, ctShell)
		shell := map[string]any{"id": ctShell, "name": "Acme shell", "command": "/bin/sh",
			"args": []string{"-c", o.shellCmd}, "sandbox": true, "read_write": []string{"~"}}
		if opts.Settings == nil {
			opts.Settings = map[string]json.RawMessage{}
		}
		opts.Settings["terminal_templates"] = ctJSON(t, []any{shell})
	}
	opts.PrepareConfigDir = func(configDir string) {
		path := filepath.Join(configDir, "settings.json")
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(mustRead(t, path), &settings); err != nil {
			t.Fatalf("decoding settings.json: %v", err)
		}
		var tmpls []map[string]any
		if err := json.Unmarshal(settings["terminal_templates"], &tmpls); err != nil {
			t.Fatalf("decoding terminal_templates: %v", err)
		}
		for _, tm := range tmpls {
			if tm["id"] == "claude-code" {
				for k, v := range o.override {
					tm[k] = v
				}
			}
		}
		settings["terminal_templates"] = ctJSON(t, tmpls)
		raw, _ := json.MarshalIndent(settings, "", "  ")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("writing settings.json: %v", err)
		}
	}
	i := harness.Start(t, opts)
	i.WaitSessionHost(ctDeadline)
	pid := g1sCreateProject(t, i, "acme-token", map[string]any{"allowed_templates": granted})
	return i, pid
}

func ctJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding %v: %v", v, err)
	}
	return b
}

// ctNewToken returns a fresh fake token. It is a placeholder, not a credential.
func ctNewToken(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("reading random bytes: %v", err)
	}
	return "acme-fake-token-" + hex.EncodeToString(b[:])
}

func ctSum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func ctTokenPath(i *harness.Instance) string {
	return filepath.Join(i.Home, ".config", "acme-ltt", "token")
}

// ctWriteToken writes the token with a trailing newline, which relay trims.
func ctWriteToken(t *testing.T, i *harness.Instance, token string, mode os.FileMode) {
	t.Helper()
	p := ctTokenPath(i)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(p), err)
	}
	_ = os.Chmod(p, 0o600)
	if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("chmod %s: %v", p, err)
	}
}

// ctSessionStart launches a claude session, runs one turn (the start line is
// written before the first reply) and returns the session's start line.
func ctSessionStart(t *testing.T, i *harness.Instance, pid string) (string, harness.AgentCall) {
	t.Helper()
	sid := g1sLaunchClaude(t, i, pid)
	g1sSendText(t, i, sid, "hi", "echo: hi")
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "start" && c.Env["RELAY_SESSION_ID"] == sid {
			return sid, c
		}
	}
	t.Fatalf("no start line carries RELAY_SESSION_ID %s", sid)
	return "", harness.AgentCall{}
}

// ctTerminalStart launches the claude-code template with --print-env, waits
// for session.exited and returns its start line.
func ctTerminalStart(t *testing.T, i *harness.Instance, pid string) harness.AgentCall {
	t.Helper()
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	i.MustCLI("terminal", "start", "--project", pid, "--template", "claude-code", "--extra-arg", "--print-env", "--json").JSON(t, &term)
	if term.TerminalID == "" {
		t.Fatalf("terminal start printed no terminalId")
	}
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, ctDeadline)
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event != "start" {
			continue
		}
		for _, a := range c.Argv {
			if a == "--print-env" {
				return c
			}
		}
	}
	t.Fatalf("the fake claude logged no start line for terminal %s", term.TerminalID)
	return harness.AgentCall{}
}

func ctWantToken(t *testing.T, what string, c harness.AgentCall, token string) {
	t.Helper()
	if got := c.Env[ctFD]; got != "5" {
		t.Fatalf("%s: %s = %q (env %v), want \"5\"", what, ctFD, got, c.Env)
	}
	if c.TokenError != "" {
		t.Fatalf("%s: the child could not read the token descriptor: %s", what, c.TokenError)
	}
	if c.TokenSHA256 != ctSum(token) || c.TokenBytes != len(token) {
		t.Fatalf("%s: the child read %d bytes with sha256 %s, want %d bytes with sha256 %s (the trimmed token)",
			what, c.TokenBytes, c.TokenSHA256, len(token), ctSum(token))
	}
}

func ctWantNoToken(t *testing.T, what string, c harness.AgentCall) {
	t.Helper()
	if v, ok := c.Env[ctFD]; ok {
		t.Fatalf("%s: %s = %q, want it unset", what, ctFD, v)
	}
	if c.TokenSHA256 != "" || c.TokenBytes != 0 {
		t.Fatalf("%s: the child read a token (%d bytes), want none", what, c.TokenBytes)
	}
}

// With the setting empty a session gets no descriptor variable, and a template
// value for it passes through to a terminal unchanged.
func TestClaudeTokenUnsetLaunchUnchanged(t *testing.T) {
	t.Parallel()
	i, pid := ctStart(t, ctOpts{override: map[string]any{"env": map[string]string{ctFD: "9"}}})
	_, s := ctSessionStart(t, i, pid)
	ctWantNoToken(t, "session with the setting empty", s)
	term := ctTerminalStart(t, i, pid)
	if got := term.Env[ctFD]; got != "9" {
		t.Fatalf("terminal with the setting empty: template env %s=9 reached the child as %q", ctFD, got)
	}
}

// A sandboxed claude session gets the token on fd 5.
func TestClaudeTokenSessionDescriptor(t *testing.T) {
	t.Parallel()
	i, pid := ctStart(t, ctOpts{setting: ctSetting})
	token := ctNewToken(t)
	ctWriteToken(t, i, token, 0o600)
	_, s := ctSessionStart(t, i, pid)
	ctWantToken(t, "sandboxed claude session", s, token)
}

// A sandboxed claude-code terminal gets the token on fd 5.
func TestClaudeTokenTerminalDescriptor(t *testing.T) {
	t.Parallel()
	i, pid := ctStart(t, ctOpts{setting: ctSetting})
	token := ctNewToken(t)
	ctWriteToken(t, i, token, 0o600)
	ctWantToken(t, "sandboxed claude-code terminal", ctTerminalStart(t, i, pid), token)
}

// A file rewritten between two launches reaches the second with no restart.
func TestClaudeTokenReadAtEachLaunch(t *testing.T) {
	t.Parallel()
	i, pid := ctStart(t, ctOpts{setting: ctSetting})
	first, second := ctNewToken(t), ctNewToken(t)
	ctWriteToken(t, i, first, 0o600)
	_, a := ctSessionStart(t, i, pid)
	ctWriteToken(t, i, second, 0o600)
	_, b := ctSessionStart(t, i, pid)
	if a.Env["RELAY_SESSION_ID"] == b.Env["RELAY_SESSION_ID"] {
		t.Fatalf("both launches share session id %s", a.Env["RELAY_SESSION_ID"])
	}
	ctWantToken(t, "first launch", a, first)
	ctWantToken(t, "second launch", b, second)
}

// ctScan fails when any text holds the token.
func ctScan(t *testing.T, where, text, token string) {
	t.Helper()
	if strings.Contains(text, token) {
		t.Fatalf("%s holds the token", where)
	}
}

// The token appears in no answer, row, event, file or child environment.
func TestClaudeTokenNeverDisclosed(t *testing.T) {
	t.Parallel()
	i, pid := ctStart(t, ctOpts{setting: ctSetting})
	token := ctNewToken(t)
	ctWriteToken(t, i, token, 0o600)

	runner := i.SocketHTTP(i.Credential("runner"))
	post := runner.Do("POST", "/api/sessions", map[string]any{"projectId": pid, "model": "sonnet"},
		harness.ReqOpts{Trace: harness.NewTrace(t)})
	if post.Status != 201 {
		t.Fatalf("POST /api/sessions answered %d, want 201: %s", post.Status, post.Body)
	}
	ctScan(t, "the POST /api/sessions answer", string(post.Body), token)
	var s struct {
		SessionID string `json:"sessionId"`
	}
	post.JSON(t, &s)
	reply := g1sSendText(t, i, s.SessionID, "hi", "echo: hi")
	ctScan(t, "the session message answer", string(reply.Stdout), token)

	tpost := runner.Do("POST", "/api/terminals", map[string]any{"projectId": pid, "templateId": "claude-code", "extraArgs": []string{"--print-env"}},
		harness.ReqOpts{Trace: harness.NewTrace(t)})
	if tpost.Status != 201 && tpost.Status != 200 {
		t.Fatalf("POST /api/terminals answered %d, want 201: %s", tpost.Status, tpost.Body)
	}
	ctScan(t, "the POST /api/terminals answer", string(tpost.Body), token)
	var tm struct {
		TerminalID string `json:"terminalId"`
	}
	tpost.JSON(t, &tm)
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": tm.TerminalID}}, ctDeadline)

	list := runner.Do("GET", "/api/sessions", nil, harness.ReqOpts{Trace: harness.NewTrace(t)})
	ctScan(t, "GET /api/sessions", string(list.Body), token)
	ctScan(t, "relay terminal list --json", string(i.MustCLI("terminal", "list", "--json").Stdout), token)
	ctScan(t, "relay session list --json", string(i.MustCLI("session", "list", "--json").Stdout), token)
	ctScan(t, "relay terminal log --json", string(i.MustCLI("terminal", "log", "--id", tm.TerminalID, "--json").Stdout), token)
	rows := i.Audit(harness.AuditQuery{Tail: 100000})
	if len(rows) == 0 {
		t.Fatalf("relay audit holds no rows")
	}
	raw, _ := json.Marshal(rows)
	ctScan(t, "the audit rows", string(raw), token)
	evs, _ := json.Marshal(i.Events(harness.EventQuery{}))
	ctScan(t, "the events", string(evs), token)

	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event != "start" {
			continue
		}
		ctScan(t, "a child's argv", strings.Join(c.Argv, "\x00"), token)
		ctScan(t, "a child's environment", strings.Join(c.Environ, "\x00"), token)
		if c.Env[ctFD] != "" && c.TokenSHA256 != ctSum(token) {
			t.Fatalf("a child started with %s but read the wrong token (error %q)", ctFD, c.TokenError)
		}
	}

	i.Stop()
	tokenFile := ctTokenPath(i)
	scanned := 0
	err := filepath.WalkDir(i.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || p == tokenFile {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		scanned++
		if strings.Contains(string(b), token) {
			t.Errorf("%s holds the token", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", i.Dir, err)
	}
	if scanned == 0 {
		t.Fatalf("the file scan read no file under %s", i.Dir)
	}
}

// A template that grants a folder holding the token file still cannot open it.
func TestClaudeTokenFileDeniedToSession(t *testing.T) {
	t.Parallel()
	i, pid := ctStart(t, ctOpts{setting: ctSetting,
		override: map[string]any{"read": []string{"~/.config", "~/.local/bin"}}})
	token := ctNewToken(t)
	ctWriteToken(t, i, token, 0o600)
	sid, _ := ctSessionStart(t, i, pid)
	// The grant is real: a sibling file in the same folder reads back.
	sibling := filepath.Join(i.Home, ".config", "acme-ltt", "sibling")
	if err := os.WriteFile(sibling, []byte("acme-sibling"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", sibling, err)
	}
	if got := b181Shell(t, i, sid, "/bin/cat "+agShQuote(sibling)); got != "exit: 0" {
		t.Fatalf("the granted folder does not read a sibling file; the denial below would prove nothing: %q", got)
	}
	got := b181Shell(t, i, sid, "/bin/cat "+agShQuote(ctTokenPath(i)))
	if got == "exit: 0" || strings.Contains(got, token) {
		t.Fatalf("cat of the token file in a sandboxed session answered %q, want a refusal without the token", got)
	}
}

// A sandboxed shell template that grants the whole home cannot open the file.
func TestClaudeTokenFileDeniedToShellTerminal(t *testing.T) {
	t.Parallel()
	rc, out := "acme-rc", "acme-out"
	// $HOME is the instance home: relay hands it on, and the template grants it.
	// The sibling read is the control: the home is granted, so only the deny
	// can explain a refusal of the token file.
	script := `/bin/cat "$HOME/.config/acme-ltt/sibling" > /dev/null; echo $? > "$HOME/acme-ctl"; ` +
		`/bin/cat "$HOME/.config/acme-ltt/token" > "$HOME/` + out + `"; echo $? > "$HOME/` + rc + `"`
	i, pid := ctStart(t, ctOpts{setting: ctSetting, shellCmd: script})
	token := ctNewToken(t)
	ctWriteToken(t, i, token, 0o600)
	if err := os.WriteFile(filepath.Join(filepath.Dir(ctTokenPath(i)), "sibling"), []byte("acme-sibling"), 0o600); err != nil {
		t.Fatalf("writing the sibling file: %v", err)
	}

	var term struct {
		TerminalID string `json:"terminalId"`
	}
	i.MustCLI("terminal", "start", "--project", pid, "--template", ctShell, "--json").JSON(t, &term)
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, ctDeadline)

	ctl, err := os.ReadFile(filepath.Join(i.Home, "acme-ctl"))
	if err != nil || strings.TrimSpace(string(ctl)) != "0" {
		t.Fatalf("the control read of a sibling file in the granted home gave %q (%v), want 0", ctl, err)
	}
	code, err := os.ReadFile(filepath.Join(i.Home, rc))
	if err != nil {
		t.Fatalf("the shell left no %s: %v", rc, err)
	}
	if strings.TrimSpace(string(code)) == "0" {
		t.Fatalf("cat of the token file from a sandboxed shell with the home granted exited 0, want a refusal")
	}
	if b, _ := os.ReadFile(filepath.Join(i.Home, out)); strings.Contains(string(b), token) {
		t.Fatalf("the shell copied the token into %s", out)
	}
	ctScan(t, "relay terminal log", string(i.MustCLI("terminal", "log", "--id", term.TerminalID, "--json").Stdout), token)
}

// ctAuditRefusals returns the non-ok session_launch rows naming the project.
func ctAuditRefusals(t *testing.T, i *harness.Instance, pid string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, row := range i.Audit(harness.AuditQuery{Event: "session_launch"}) {
		actor, _ := row["actor"].(map[string]any)
		if actor["project_id"] == pid && row["outcome"] != "ok" {
			out = append(out, row)
		}
	}
	return out
}

// A missing file refuses the launch with sandbox_unavailable, naming the path.
func TestClaudeTokenMissingFileRefused(t *testing.T) {
	t.Parallel()
	i, pid := ctStart(t, ctOpts{setting: ctSetting})
	trace := harness.NewTrace(t)
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/sessions",
		map[string]any{"projectId": pid, "model": "sonnet"}, harness.ReqOpts{Trace: trace})
	if resp.Status != 400 {
		t.Fatalf("POST /api/sessions with a missing token file answered %d, want 400: %s", resp.Status, resp.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.launch", Trace: trace,
		Fields: map[string]any{"reason": "sandbox_unavailable"}}, ctDeadline)
	if !strings.Contains(string(resp.Body), ctTokenPath(i)) {
		t.Fatalf("the 400 body does not name %s: %s", ctTokenPath(i), resp.Body)
	}
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "start" && (len(c.Argv) == 0 || c.Argv[0] != "debug") {
			t.Fatalf("a refused launch still started the fake claude: argv %v", c.Argv)
		}
	}
	if len(ctAuditRefusals(t, i, pid)) == 0 {
		t.Fatalf("no refused session_launch audit row names project %s", pid)
	}
}

// An unreadable file refuses the launch naming the path, never the content.
func TestClaudeTokenUnreadableFileRefused(t *testing.T) {
	t.Parallel()
	i, pid := ctStart(t, ctOpts{setting: ctSetting})
	token := ctNewToken(t)
	ctWriteToken(t, i, token, 0o000)
	t.Cleanup(func() { _ = os.Chmod(ctTokenPath(i), 0o600) })
	if f, err := os.Open(ctTokenPath(i)); err == nil {
		_ = f.Close()
		t.Skip("the test process can read a mode 000 file (running as root); the refusal cannot be provoked")
	}
	resp := i.SocketHTTP(i.Credential("runner")).Do("POST", "/api/terminals",
		map[string]any{"projectId": pid, "templateId": "claude-code", "extraArgs": []string{"--print-env"}},
		harness.ReqOpts{Trace: harness.NewTrace(t)})
	if resp.Trace == "" {
		t.Fatalf("the answer carries no trace id")
	}
	if resp.Status != 400 {
		t.Fatalf("POST /api/terminals with an unreadable token file answered %d, want 400: %s", resp.Status, resp.Body)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.launch", Trace: resp.Trace,
		Fields: map[string]any{"reason": "sandbox_unavailable"}}, ctDeadline)
	if !strings.Contains(string(resp.Body), ctTokenPath(i)) {
		t.Fatalf("the 400 body does not name %s: %s", ctTokenPath(i), resp.Body)
	}
	ctScan(t, "the 400 body", string(resp.Body), token)
	rows := ctAuditRefusals(t, i, pid)
	if len(rows) == 0 {
		t.Fatalf("no refused session_launch audit row names project %s", pid)
	}
	raw, _ := json.Marshal(rows)
	ctScan(t, "the refusal audit row", string(raw), token)
	if !strings.Contains(string(raw), ctTokenPath(i)) {
		t.Fatalf("the refusal audit row does not name %s: %s", ctTokenPath(i), raw)
	}
}

// While the setting is set, relay's descriptor value wins over the template's.
func TestClaudeTokenRelayDescriptorWins(t *testing.T) {
	t.Parallel()
	i, pid := ctStart(t, ctOpts{setting: ctSetting, override: map[string]any{"env": map[string]string{ctFD: "9"}}})
	token := ctNewToken(t)
	ctWriteToken(t, i, token, 0o600)
	ctWantToken(t, "terminal with a template value of 9", ctTerminalStart(t, i, pid), token)
}

// An unsandboxed launch keeps the person's own login: no descriptor.
func TestClaudeTokenUnsandboxedNoDescriptor(t *testing.T) {
	t.Parallel()
	i, pid := ctStart(t, ctOpts{setting: ctSetting, override: map[string]any{"sandbox": false}})
	ctWriteToken(t, i, ctNewToken(t), 0o600)
	ctWantNoToken(t, "unsandboxed claude-code terminal", ctTerminalStart(t, i, pid))
}
