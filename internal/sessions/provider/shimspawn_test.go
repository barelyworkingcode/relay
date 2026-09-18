package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// TestClaudeStart_ShimArgvShape pins buildShimCmd's exact argv order for a
// claude-shaped invocation: exec, --session-id <id>, --identity,
// --sandbox-profile <abs>, --status-fd 4, --, real binary path, then
// buildClaudeArgs's own output verbatim in order. --pty must never appear —
// pipe mode is this package's whole launch shape.
func TestClaudeStart_ShimArgvShape(t *testing.T) {
	session := &sessionstypes.Session{ID: "claude-argv-1", Model: "sonnet"}
	p := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{}, nil)
	claudeArgs := p.buildClaudeArgs("", "")

	spec := shimSpec{
		Binary:         "/abs/relay-sessions",
		SessionID:      session.ID,
		BridgeSocket:   "/tmp/bridge.sock",
		SandboxProfile: "/abs/profile.sb",
		Identity:       &sessionsmcp.IdentitySpec{Secret: strings.Repeat("a", 64)},
	}
	cmd, statusR, extraFiles, err := buildShimCmd(spec, "/abs/claude", claudeArgs)
	if err != nil {
		t.Fatalf("buildShimCmd: %v", err)
	}
	defer func() {
		_ = statusR.Close()
		for _, f := range extraFiles {
			_ = f.Close()
		}
	}()

	want := append([]string{
		"exec", "--session-id", session.ID, "--identity",
		"--sandbox-profile", "/abs/profile.sb",
		"--status-fd", "4", "--", "/abs/claude",
	}, claudeArgs...)
	assertArgvEqual(t, cmd.Args[1:], want)

	if len(extraFiles) != 2 {
		t.Fatalf("extraFiles = %d, want 2 (secret read end + status write end)", len(extraFiles))
	}
	for _, a := range cmd.Args {
		if a == "--pty" {
			t.Fatal("--pty must never appear in a provider-session shim invocation")
		}
	}
}

// TestPiStart_ShimArgvShape mirrors the claude test against buildPiArgs.
func TestPiStart_ShimArgvShape(t *testing.T) {
	session := &sessionstypes.Session{ID: "pi-argv-1", Model: "gpt"}
	p := NewPiProvider(session, func(string, json.RawMessage) {}, PiConfig{})
	piArgs := p.buildPiArgs("/tmp/pi-sessions", "", "")

	spec := shimSpec{
		Binary:         "/abs/relay-sessions",
		SessionID:      session.ID,
		BridgeSocket:   "/tmp/bridge.sock",
		SandboxProfile: "/abs/profile.sb",
		Identity:       &sessionsmcp.IdentitySpec{Secret: strings.Repeat("b", 64)},
	}
	cmd, statusR, extraFiles, err := buildShimCmd(spec, "/abs/pi", piArgs)
	if err != nil {
		t.Fatalf("buildShimCmd: %v", err)
	}
	defer func() {
		_ = statusR.Close()
		for _, f := range extraFiles {
			_ = f.Close()
		}
	}()

	want := append([]string{
		"exec", "--session-id", session.ID, "--identity",
		"--sandbox-profile", "/abs/profile.sb",
		"--status-fd", "4", "--", "/abs/pi",
	}, piArgs...)
	assertArgvEqual(t, cmd.Args[1:], want)

	for _, a := range cmd.Args {
		if a == "--pty" {
			t.Fatal("--pty must never appear in a provider-session shim invocation")
		}
	}
}

// TestBuildShimCmd_SecretNeverInArgvOrEnv confirms the 64-hex secret appears
// nowhere in argv (it travels on fd 3 only) and that buildShimCmd sets no
// env of its own at all -- Start is what builds env, uniformly across the
// shim-wrapped and direct-spawn branches.
func TestBuildShimCmd_SecretNeverInArgvOrEnv(t *testing.T) {
	secret := strings.Repeat("c", 64)
	spec := shimSpec{
		Binary:       "/abs/relay-sessions",
		SessionID:    "sess-1",
		BridgeSocket: "/tmp/bridge.sock",
		Identity:     &sessionsmcp.IdentitySpec{Secret: secret},
	}
	cmd, statusR, extraFiles, err := buildShimCmd(spec, "/abs/claude", []string{"--print"})
	if err != nil {
		t.Fatalf("buildShimCmd: %v", err)
	}
	defer func() {
		_ = statusR.Close()
		for _, f := range extraFiles {
			_ = f.Close()
		}
	}()

	for _, a := range cmd.Args {
		if strings.Contains(a, secret) {
			t.Fatalf("secret leaked into argv: %q", a)
		}
	}
	if len(cmd.Env) != 0 {
		t.Fatalf("buildShimCmd set env %v, want none (Start builds it)", cmd.Env)
	}
}

// TestBuildShimCmd_StatusFDIs3WithoutIdentity confirms a spec with no
// Identity gets --status-fd 3 and exactly one ExtraFiles entry (the status
// pipe alone -- no identity secret pipe).
func TestBuildShimCmd_StatusFDIs3WithoutIdentity(t *testing.T) {
	spec := shimSpec{
		Binary:         "/abs/relay-sessions",
		SessionID:      "sess-2",
		SandboxProfile: "/abs/profile.sb",
	}
	cmd, statusR, extraFiles, err := buildShimCmd(spec, "/abs/claude", []string{"--print"})
	if err != nil {
		t.Fatalf("buildShimCmd: %v", err)
	}
	defer func() {
		_ = statusR.Close()
		for _, f := range extraFiles {
			_ = f.Close()
		}
	}()

	if len(extraFiles) != 1 {
		t.Fatalf("extraFiles = %d, want 1 (status write end only)", len(extraFiles))
	}
	want := []string{"exec", "--session-id", "sess-2", "--sandbox-profile", "/abs/profile.sb", "--status-fd", "3", "--", "/abs/claude", "--print"}
	assertArgvEqual(t, cmd.Args[1:], want)
}

func assertArgvEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv = %v (%d elements), want %v (%d elements)", got, len(got), want, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q\nfull got:  %v\nfull want: %v", i, got[i], want[i], got, want)
		}
	}
}

// TestClaudeStart_SandboxWithoutShimBinary_FailsClosed: a sandbox profile
// requested with no shim binary configured must refuse outright, never spawn
// unconfined.
func TestClaudeStart_SandboxWithoutShimBinary_FailsClosed(t *testing.T) {
	session := &sessionstypes.Session{ID: "claude-failclosed-1", Model: "sonnet", Directory: t.TempDir()}
	p := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{
		Binary:         "/bin/echo", // would be spawned if this test failed to fail closed
		SandboxProfile: "/abs/profile.sb",
	}, nil)

	err := p.Start()
	if !errors.Is(err, ErrShimRequired) {
		t.Fatalf("Start() error = %v, want ErrShimRequired", err)
	}
	if p.cmd != nil {
		t.Fatal("a process was recorded as started despite the fail-closed refusal")
	}
	if p.Alive() {
		t.Fatal("provider reports alive after a fail-closed refusal")
	}
}

// TestClaudeStart_IdentityWithoutBridgeSocket_Refused: an identity with no
// bridge socket configured has nothing legitimate to Hello against.
func TestClaudeStart_IdentityWithoutBridgeSocket_Refused(t *testing.T) {
	session := &sessionstypes.Session{ID: "claude-nobridge-1", Model: "sonnet", Directory: t.TempDir()}
	p := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{
		Binary:     "/bin/echo",
		ShimBinary: "/bin/echo",
		Identity:   &sessionsmcp.IdentitySpec{Secret: strings.Repeat("d", 64)},
	}, nil)

	err := p.Start()
	if !errors.Is(err, ErrNoBridgeSocket) {
		t.Fatalf("Start() error = %v, want ErrNoBridgeSocket", err)
	}
	if p.cmd != nil {
		t.Fatal("a process was recorded as started despite the fail-closed refusal")
	}
}

// TestClaudeSetPermissionMode_SandboxedLaunch_RequiresResume proves the one
// deliberate behavior change: SetPermissionMode refuses to Kill-then-Start a
// launch whose identity/sandbox profile are real, since both are single-use
// and torn down on Kill. The provider must be left exactly as it was.
func TestClaudeSetPermissionMode_SandboxedLaunch_RequiresResume(t *testing.T) {
	session := &sessionstypes.Session{ID: "claude-restart-1", Model: "sonnet", PermissionMode: "default"}
	p := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{
		SandboxProfile: "/abs/profile.sb",
	}, nil)

	err := p.SetPermissionMode("plan")
	if !errors.Is(err, ErrRestartNeedsResume) {
		t.Fatalf("SetPermissionMode error = %v, want ErrRestartNeedsResume", err)
	}
	session.Lock()
	mode := session.PermissionMode
	session.Unlock()
	if mode != "default" {
		t.Fatalf("session.PermissionMode = %q, want unchanged %q -- the guard must fire before any state mutation", mode, "default")
	}
	if p.cmd != nil {
		t.Fatal("SetPermissionMode's refusal must never attempt a Kill/Start cycle")
	}

	// Same guard, Identity instead of SandboxProfile.
	session2 := &sessionstypes.Session{ID: "claude-restart-2", Model: "sonnet", PermissionMode: "default"}
	p2 := NewClaudeProvider(session2, func(string, json.RawMessage) {}, ClaudeConfig{
		Identity: &sessionsmcp.IdentitySpec{Secret: strings.Repeat("e", 64)},
	}, nil)
	if err := p2.SetPermissionMode("acceptEdits"); !errors.Is(err, ErrRestartNeedsResume) {
		t.Fatalf("SetPermissionMode (identity) error = %v, want ErrRestartNeedsResume", err)
	}
}
