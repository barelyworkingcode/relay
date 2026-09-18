package provider

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// TestClaudeStart_HostSession_NeverUsesShim is a regression guard for the
// out-of-scope path: an SSH-hosted session must ignore ShimBinary,
// SandboxProfile and Identity entirely and spawn via buildHostExec's own ssh
// invocation, never through the shim -- relay's own launch authorization
// already guarantees both are empty for a hosted project, but this pins the
// behavior even if that guarantee ever drifted.
func TestClaudeStart_HostSession_NeverUsesShim(t *testing.T) {
	scratch := t.TempDir()
	script := writeEnvArgvDumpScript(t, scratch)
	argvOut := filepath.Join(scratch, "argv.out")
	t.Setenv("RH_TEST_OUT_ENV", filepath.Join(scratch, "env.out"))
	t.Setenv("RH_TEST_OUT_ARGV", argvOut)

	session := &sessionstypes.Session{
		ID:    "claude-host-1",
		Model: "sonnet",
		Host: &sessionstypes.HostSpec{
			ID:         "host-1",
			Name:       "test-host",
			SSHArgv:    []string{script, "-o", "BatchMode=yes"},
			ClaudePath: "/remote/bin/claude",
		},
	}
	p := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{
		// Every field that would trigger the shim on a console session, set
		// anyway -- the host branch must ignore all three.
		ShimBinary:     "/abs/relay-sessions",
		SandboxProfile: "/abs/profile.sb",
		Identity:       &sessionsmcp.IdentitySpec{Secret: strings.Repeat("f", 64)},
	}, nil)
	defer p.Kill()

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFile(t, argvOut)
	argv := readLines(t, argvOut)

	for _, forbidden := range []string{"exec", "--identity", "--session-id", "--sandbox-profile", "--status-fd"} {
		for _, a := range argv {
			if a == forbidden {
				t.Fatalf("host session argv used the shim shape (saw %q): %v", forbidden, argv)
			}
		}
	}
	var sawDashT bool
	for _, a := range argv {
		if a == "-T" {
			sawDashT = true
		}
	}
	if !sawDashT {
		t.Fatalf("host session argv missing ssh's -T flag, want buildHostExec's own shape: %v", argv)
	}
	if p.targetPID != 0 {
		t.Fatalf("targetPID = %d, want 0 on a host session (no shim ever ran)", p.targetPID)
	}
}
