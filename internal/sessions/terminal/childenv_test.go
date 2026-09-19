package terminal

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A relay launched from inside a Claude Code session inherits that session's
// variables. The terminal it spawns must not: Claude Code reads
// CLAUDE_CODE_CHILD_SESSION and turns transcript saving off, and the messaging
// token is the parent session's.
func TestBuildShimEnv_DropsTheLaunchingClaudeSession(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "parent-secret")
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/me/.claude-alt")

	// A template names what it wants: this is what env_passthrough produces.
	env := buildShimEnv(CreateSpec{Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "template-supplied"}}, Config{})

	have := map[string]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		have[name] = value
	}
	for _, gone := range []string{"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_MESSAGING_TOKEN"} {
		if _, ok := have[gone]; ok {
			t.Errorf("the shim's environment carried the launching session's %s", gone)
		}
	}
	if have["CLAUDE_CONFIG_DIR"] == "" {
		t.Error("CLAUDE_CONFIG_DIR was dropped; it configures a Claude Code the operator means to run")
	}
	if have["CLAUDE_CODE_OAUTH_TOKEN"] != "template-supplied" {
		t.Errorf("a variable the template supplied was dropped: %q", have["CLAUDE_CODE_OAUTH_TOKEN"])
	}
}

// TestManager_LocalPTY_DoesNotSeeTheLaunchingClaudeSession runs the real shim
// and reads what the target process actually has in its environment.
func TestManager_LocalPTY_DoesNotSeeTheLaunchingClaudeSession(t *testing.T) {
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("HARMLESS_MARKER", "still-here")

	cfg := testConfig(t)
	mgr := NewManager(cfg)
	exitCh := make(chan int, 1)
	mgr.SetExitHandler(func(_ string, code int) { exitCh <- code })

	sess, err := mgr.Create(CreateSpec{
		SessionID: "33333333-4444-5555-6666-777777777777",
		Name:      "env",
		Directory: t.TempDir(),
		Argv:      []string{"/usr/bin/env"},
		Cols:      120,
		Rows:      24,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	select {
	case <-exitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for exit")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !bytes.Contains(sess.ScrollbackBytes(), []byte("HARMLESS_MARKER")) {
		time.Sleep(20 * time.Millisecond)
	}
	out := sess.ScrollbackBytes()
	if !bytes.Contains(out, []byte("HARMLESS_MARKER=still-here")) {
		t.Fatalf("the target's environment lost an ordinary variable: %q", out)
	}
	for _, gone := range []string{"CLAUDE_CODE_CHILD_SESSION", "CLAUDECODE="} {
		if bytes.Contains(out, []byte(gone)) {
			t.Errorf("the target process saw the launching session's %s", gone)
		}
	}
}
