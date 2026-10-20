package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"relaye2e/harness"
)

const trustVar = "CLAUDE_CODE_SANDBOXED"

// trustPlantOwn puts a .claude.json of the person's own in the sandbox Claude
// folder, mode 0644, so a launch finds a folder that is no longer fresh.
func trustPlantOwn(t *testing.T, i *harness.Instance) string {
	t.Helper()
	b506Plant(t, b181Dir(i))
	p := filepath.Join(b181Dir(i), ".claude.json")
	if err := os.WriteFile(p, []byte("acme-own"), 0o644); err != nil {
		t.Fatalf("planting .claude.json: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	return p
}

// trustEnviron reads the variable from the whole environment of a fake
// claude start line, because the fake's watched list does not name it. set
// is false when the variable is absent.
func trustEnviron(t *testing.T, c harness.AgentCall) (value string, set bool) {
	t.Helper()
	if len(c.Environ) == 0 {
		t.Fatalf("the start line reported no environment: %+v", c)
	}
	for _, kv := range c.Environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == trustVar {
			return v, true
		}
	}
	return "", false
}

// trustTerminal launches the claude-code template with --print-env, waits for
// the terminal's session.exited event and returns the variable's state in the
// start line.
func trustTerminal(t *testing.T, i *harness.Instance, pid string) (string, bool) {
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
				return trustEnviron(t, c)
			}
		}
	}
	t.Fatalf("the fake claude logged no start line for the terminal launch")
	return "", false
}

// trustSession starts a claude session, runs one turn and returns the
// variable's state in the session's start line.
func trustSession(t *testing.T, i *harness.Instance, pid string) (string, bool) {
	t.Helper()
	sid := g1sLaunchClaude(t, i, pid)
	g1sSendText(t, i, sid, "hi", "echo: hi")
	for _, c := range i.FakeAgentCalls("claude") {
		if c.Event == "start" && len(c.Argv) > 0 && c.Argv[0] != "debug" {
			return trustEnviron(t, c)
		}
	}
	t.Fatalf("no session start line found")
	return "", false
}

// A sandboxed claude-code terminal in a project folder gets
// CLAUDE_CODE_SANDBOXED=1 even when the sandbox Claude folder already holds a
// .claude.json, and that file keeps its bytes and mode.
func TestSandboxClaudeTrustTerminalEnv(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	p := trustPlantOwn(t, i)
	if got, set := trustTerminal(t, i, pid); !set || got != "1" {
		t.Fatalf("sandboxed claude-code terminal env %s = %q (set %v), want \"1\"", trustVar, got, set)
	}
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o644 {
		t.Fatalf(".claude.json is %v (%v), want a regular file of mode 0644", st, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "acme-own" {
		t.Fatalf(".claude.json holds %q, want acme-own", b)
	}
	b506NoSeedTemp(t, b181Dir(i))
}

// A sandboxed terminal in a project at the home folder gets no variable, so
// Claude asks about trust.
func TestSandboxClaudeTrustHomeNotTrusted(t *testing.T) {
	t.Parallel()
	i, _ := b181Start(t, nil)
	trustPlantOwn(t, i)
	raw, err := json.Marshal(map[string]any{"name": "acme-home", "path": i.Home, "allowed_templates": []string{"claude-code"}})
	if err != nil {
		t.Fatalf("encoding the project: %v", err)
	}
	res := i.CLIWith(harness.CLIOpts{Stdin: raw}, "project", "create", "--file", "-", "--json")
	if res.Code != 0 {
		t.Fatalf("project create exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var p struct {
		ID string `json:"id"`
	}
	res.JSON(t, &p)
	if p.ID == "" {
		t.Fatalf("project create printed no id: %s", res.Stdout)
	}
	if v, ok := trustTerminal(t, i, p.ID); ok {
		t.Fatalf("home-folder terminal got %s=%q, want it unset", trustVar, v)
	}
}

// A value the claude-code template's env sets reaches the child unchanged.
func TestSandboxClaudeTrustTemplateEnvWins(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, map[string]any{"env": map[string]string{trustVar: "0"}})
	trustPlantOwn(t, i)
	if got, set := trustTerminal(t, i, pid); !set || got != "0" {
		t.Fatalf("template env %s=0 reached the child as %q (set %v), want \"0\"", trustVar, got, set)
	}
}

// An unsandboxed claude-code terminal starts without the variable.
func TestSandboxClaudeTrustUnsandboxedUnset(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, map[string]any{"sandbox": false})
	if v, ok := trustTerminal(t, i, pid); ok {
		t.Fatalf("unsandboxed claude-code terminal got %s=%q, want it unset", trustVar, v)
	}
}

// A sandboxed claude session starts without the variable.
func TestSandboxClaudeTrustSessionUnchanged(t *testing.T) {
	t.Parallel()
	i, pid := b181Start(t, nil)
	trustPlantOwn(t, i)
	if v, ok := trustSession(t, i, pid); ok {
		t.Fatalf("sandboxed claude session got %s=%q, want it unset", trustVar, v)
	}
}
