package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"relaye2e/harness"
)

const b181Deadline = 60 * time.Second

// b181Start boots an instance with the claude-code template granted to a
// project. Each key of override is set on the harness's claude-code template
// before relay starts.
func b181Start(t *testing.T, override map[string]any) (*harness.Instance, string) {
	t.Helper()
	o := g1sOptions(harness.Options{})
	o.PrepareConfigDir = func(configDir string) {
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
				for k, v := range override {
					tm[k] = v
				}
			}
		}
		settings["terminal_templates"], _ = json.Marshal(tmpls)
		raw, _ := json.MarshalIndent(settings, "", "  ")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("writing settings.json: %v", err)
		}
	}
	i := harness.Start(t, o)
	i.WaitSessionHost(b181Deadline)
	pid := g1sCreateProject(t, i, "acme-updater", map[string]any{"allowed_templates": []string{"claude-code"}})
	return i, pid
}

// b181Terminal launches the claude-code template with the fake's --print-env,
// waits for it to exit and returns the environment its start line reported.
func b181Terminal(t *testing.T, i *harness.Instance, pid string) map[string]string {
	t.Helper()
	var term struct {
		TerminalID string `json:"terminalId"`
	}
	i.MustCLI("terminal", "start", "--project", pid, "--template", "claude-code", "--extra-arg", "--print-env", "--json").JSON(t, &term)
	if term.TerminalID == "" {
		t.Fatalf("terminal start printed no terminalId")
	}
	i.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": term.TerminalID}}, b181Deadline)
	for _, c := range i.FakeAgentCalls("claude") {
		for _, a := range c.Argv {
			if c.Event == "start" && a == "--print-env" {
				return c.Env
			}
		}
	}
	t.Fatalf("the fake claude logged no start line for the terminal launch")
	return nil
}

// A sandboxed claude session starts with the updater off.
func TestBug181SandboxedClaudeSessionUpdaterOff(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	sid := g1sLaunchClaude(t, i, pid)
	g1sSendText(t, i, sid, "hi", "echo: hi")
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "start" && len(c.Argv) > 0 && c.Argv[0] != "debug" {
			if got := c.Env["DISABLE_AUTOUPDATER"]; got != "1" {
				t.Fatalf("sandboxed claude session env DISABLE_AUTOUPDATER = %q (env %v), want \"1\"", got, c.Env)
			}
			return
		}
	}
	t.Fatalf("no session start line found")
}

// A sandboxed terminal launch of the claude-code template starts with the
// updater off.
func TestBug181SandboxedClaudeTerminalUpdaterOff(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	env := b181Terminal(t, i, pid)
	if got := env["DISABLE_AUTOUPDATER"]; got != "1" {
		t.Fatalf("sandboxed claude-code terminal env DISABLE_AUTOUPDATER = %q (env %v), want \"1\"", got, env)
	}
}

// A key the template's own env sets wins over the updater default.
func TestBug181TemplateEnvWins(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, map[string]any{"env": map[string]string{"DISABLE_AUTOUPDATER": "0"}})
	env := b181Terminal(t, i, pid)
	if got := env["DISABLE_AUTOUPDATER"]; got != "0" {
		t.Fatalf("template env DISABLE_AUTOUPDATER=0 reached the child as %q (env %v), want \"0\"", got, env)
	}
}

// An unsandboxed launch is left as it was.
func TestBug181UnsandboxedLaunchUnchanged(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, map[string]any{"sandbox": false})
	env := b181Terminal(t, i, pid)
	if v, ok := env["DISABLE_AUTOUPDATER"]; ok {
		t.Fatalf("unsandboxed claude-code terminal got DISABLE_AUTOUPDATER=%q, want it unset", v)
	}
}
