package features

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"relaye2e/harness"
)

const (
	b181StoreKey  = "CLAUDE_SECURESTORAGE_CONFIG_DIR"
	b181UpdateKey = "DISABLE_AUTOUPDATER"
)

// b181Start returns the fake claude's start line env, or nil when no start
// line has been written yet.
func b181Start(i *harness.Instance) (map[string]string, bool) {
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "start" {
			return c.Env, true
		}
	}
	return nil, false
}

// b181TerminalStartEnv starts a claude-code terminal and returns the fake
// claude's start line env. The fake logs its start line, then exits on a
// tty; the host's session.exited event for the terminal is the signal that
// the line is written.
func b181TerminalStartEnv(t *testing.T, i *harness.Instance, projectID string) map[string]string {
	t.Helper()
	res := i.MustCLI("terminal", "start", "--project", projectID, "--template", "claude-code", "--json")
	var out struct {
		TerminalID string `json:"terminalId"`
	}
	res.JSON(t, &out)
	if out.TerminalID == "" {
		t.Fatalf("terminal start printed no terminalId: %s", res.Stdout)
	}
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": out.TerminalID}}, g1sEventDeadline)
	env, ok := b181Start(i)
	if !ok {
		t.Fatalf("the fake claude wrote no start line before its terminal exited")
	}
	return env
}

func b181WantSandboxedEnv(t *testing.T, i *harness.Instance, env map[string]string) {
	t.Helper()
	if want := filepath.Join(i.Home, ".claude"); env[b181StoreKey] != want {
		t.Errorf("sandboxed claude env %s = %q, want %q", b181StoreKey, env[b181StoreKey], want)
	}
	if env[b181UpdateKey] != "1" {
		t.Errorf("sandboxed claude env %s = %q, want %q", b181UpdateKey, env[b181UpdateKey], "1")
	}
}

func TestBug181SandboxedClaudeOwnLoginStore(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)
	p := g1sCreateProject(t, i, "acme-login", map[string]any{"allowed_templates": []string{"claude-code"}})
	id := g1sLaunchClaude(t, i, p)
	g1sSendText(t, i, id, "hi", "echo: hi")
	env, ok := b181Start(i)
	if !ok {
		t.Fatalf("the fake claude wrote no start line")
	}
	b181WantSandboxedEnv(t, i, env)
}

func TestBug181SandboxedClaudeTerminalOwnLoginStore(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, g1sOptions(harness.Options{}))
	i.WaitSessionHost(g1sHostDeadline)
	p := g1sCreateProject(t, i, "acme-login-term", map[string]any{"allowed_templates": []string{"claude-code"}})
	b181WantSandboxedEnv(t, i, b181TerminalStartEnv(t, i, p))
}

// An unsandboxed claude-code template keeps the login store the operator's
// own claude uses.
func TestBug181UnsandboxedClaudeTerminalKeepsEnv(t *testing.T) {
	t.Parallel()
	tmpl := json.RawMessage(`[{"id":"claude-code","name":"Claude Code","command":"claude","sandbox":false}]`)
	i := harness.Start(t, g1sOptions(harness.Options{
		Settings: map[string]json.RawMessage{"terminal_templates": tmpl},
	}))
	i.WaitSessionHost(g1sHostDeadline)
	p := g1sCreateProject(t, i, "acme-login-open", map[string]any{"allowed_templates": []string{"claude-code"}})
	env := b181TerminalStartEnv(t, i, p)
	for _, k := range []string{b181StoreKey, b181UpdateKey} {
		if v, set := env[k]; set {
			t.Errorf("unsandboxed claude env has %s = %q, want it unset", k, v)
		}
	}
}

// A template env value wins over the sandboxed-claude defaults, which still
// fill the keys the template leaves alone.
func TestBug181TemplateEnvWins(t *testing.T) {
	t.Parallel()
	tmpl := json.RawMessage(`[{"id":"claude-code","name":"Claude Code","command":"claude","env":{"DISABLE_AUTOUPDATER":"0"}}]`)
	i := harness.Start(t, g1sOptions(harness.Options{
		Settings: map[string]json.RawMessage{"terminal_templates": tmpl},
	}))
	i.WaitSessionHost(g1sHostDeadline)
	p := g1sCreateProject(t, i, "acme-login-env", map[string]any{"allowed_templates": []string{"claude-code"}})
	env := b181TerminalStartEnv(t, i, p)
	if env[b181UpdateKey] != "0" {
		t.Errorf("template env %s = %q, want the template's %q", b181UpdateKey, env[b181UpdateKey], "0")
	}
	if want := filepath.Join(i.Home, ".claude"); env[b181StoreKey] != want {
		t.Errorf("sandboxed claude env %s = %q, want the default %q", b181StoreKey, env[b181StoreKey], want)
	}
}
