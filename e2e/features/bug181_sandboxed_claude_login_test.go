package features

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

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

// b181AwaitStart polls for the fake claude's start line; the line is the
// signal that the launch happened.
func b181AwaitStart(t *testing.T, i *harness.Instance) map[string]string {
	t.Helper()
	deadline := time.Now().Add(g1sEventDeadline)
	for {
		if env, ok := b181Start(i); ok {
			return env
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake claude wrote no start line within %v", g1sEventDeadline)
		}
		time.Sleep(50 * time.Millisecond)
	}
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
	i.MustCLI("terminal", "start", "--project", p, "--template", "claude-code", "--json")
	b181WantSandboxedEnv(t, i, b181AwaitStart(t, i))
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
	i.MustCLI("terminal", "start", "--project", p, "--template", "claude-code", "--json")
	env := b181AwaitStart(t, i)
	for _, k := range []string{b181StoreKey, b181UpdateKey} {
		if v, set := env[k]; set {
			t.Errorf("unsandboxed claude env has %s = %q, want it unset", k, v)
		}
	}
}
