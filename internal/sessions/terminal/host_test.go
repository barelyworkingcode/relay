package terminal

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sshhost"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func hostSpec() *sessionstypes.HostSpec {
	return &sessionstypes.HostSpec{
		ID:      "h1",
		Name:    "devbox",
		SSHArgv: []string{"ssh", "-o", "BatchMode=yes", "admin@devbox"},
	}
}

// decodeLauncher extracts and decodes RemoteCommand's base64+eval wrapper,
// matching internal/sshhost's own test helper of the same purpose (that one
// is unexported to its package, so this is a narrow, local duplicate rather
// than a shared dependency across two independent test suites).
func decodeLauncher(t *testing.T, cmd string) string {
	t.Helper()
	const prefix = `sh -c 'eval "$(printf %s `
	const suffix = ` | base64 -d)"'`
	if !strings.HasPrefix(cmd, prefix) || !strings.HasSuffix(cmd, suffix) {
		t.Fatalf("command %q does not match the fixed launcher shape", cmd)
	}
	b64 := strings.TrimSuffix(strings.TrimPrefix(cmd, prefix), suffix)
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("launcher payload is not valid base64: %v", err)
	}
	return string(decoded)
}

func TestBuildHostTargetArgv_ArgvPrefixAndFlag(t *testing.T) {
	name, args, err := buildHostTargetArgv(hostSpec(), "/proj", []string{"/bin/zsh", "-l"}, nil)
	if err != nil {
		t.Fatalf("buildHostTargetArgv: %v", err)
	}
	if name != "ssh" {
		t.Fatalf("name = %q, want ssh", name)
	}
	want := []string{"-o", "BatchMode=yes", "admin@devbox", "-tt", "--"}
	if len(args) != len(want)+1 {
		t.Fatalf("args = %v, want prefix %v plus one remote-command token", args, want)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, args[i], w)
		}
	}
}

func TestBuildHostTargetArgv_RemoteCommandShape(t *testing.T) {
	_, args, err := buildHostTargetArgv(hostSpec(), "/proj", []string{"/bin/zsh", "-l"}, map[string]string{"TERM": "xterm-256color"})
	if err != nil {
		t.Fatalf("buildHostTargetArgv: %v", err)
	}
	decoded := decodeLauncher(t, args[len(args)-1])
	want := `cd '/proj' && exec env 'TERM'='xterm-256color' '/bin/zsh' '-l'`
	if decoded != want {
		t.Fatalf("decoded = %q, want %q", decoded, want)
	}
}

func TestBuildHostTargetArgv_NoDirectoryOmitsCd(t *testing.T) {
	_, args, err := buildHostTargetArgv(hostSpec(), "", []string{"/bin/zsh", "-l"}, nil)
	if err != nil {
		t.Fatalf("buildHostTargetArgv: %v", err)
	}
	decoded := decodeLauncher(t, args[len(args)-1])
	if strings.HasPrefix(decoded, "cd ") {
		t.Errorf("empty directory must omit cd (lands in the host's login home), got %q", decoded)
	}
}

func TestBuildHostTargetArgv_NoSSHArgv_Errors(t *testing.T) {
	_, _, err := buildHostTargetArgv(&sessionstypes.HostSpec{Name: "bare"}, "/proj", []string{"/bin/sh"}, nil)
	if err == nil {
		t.Fatal("want an error for a host with no ssh_argv")
	}
}

// TestSec_HostTerminalArgv_NeverContainsRelaySecrets mirrors relayLLM's own
// TestSec_HostTerminalExec_ArgvNeverContainsRelaySecrets: v1 carries no
// relay MCP/token onto a host session (ssh-hosts.md decision 6), so nothing
// this package builds for a host session may ever mention one — env comes
// from spec.Env only, never this package's own RELAY_* additions (host.go
// never calls buildShimEnv).
func TestSec_HostTerminalArgv_NeverContainsRelaySecrets(t *testing.T) {
	_, args, err := buildHostTargetArgv(hostSpec(), "/proj", []string{"npm", "test"}, map[string]string{"TERM": "xterm-256color"})
	if err != nil {
		t.Fatalf("buildHostTargetArgv: %v", err)
	}
	decoded := decodeLauncher(t, args[len(args)-1])
	for _, secret := range []string{"RELAY_PROJECT_TOKEN", "RELAY_TOKEN", "RELAY_SERVICE_TOKEN", "RELAY_LLM_HOOK"} {
		if strings.Contains(decoded, secret) {
			t.Errorf("host terminal argv leaked %q: %s", secret, decoded)
		}
	}
}

// TestBuildHostTargetArgv_NeverShellsOut is the hermeticity proof judgment
// call #3 asks for: sshhost.SetRunnerForTest overrides the exec seam every
// Probe/Check/Disconnect call goes through, but buildHostTargetArgv never
// calls any of those — it only calls sshhost.RemoteCommand, a pure string
// builder. Installing a runner that fails the test if invoked proves that,
// loudly, rather than by absence of evidence: if a future edit to host.go
// ever grows a real exec call (e.g. to probe the host before building
// argv), this test fails immediately instead of silently becoming a live
// network test.
func TestBuildHostTargetArgv_NeverShellsOut(t *testing.T) {
	restore := sshhost.SetRunnerForTest(func(_ context.Context, name string, args []string) ([]byte, []byte, error) {
		t.Fatalf("buildHostTargetArgv must never shell out, but the runner seam was invoked: %s %v", name, args)
		return nil, nil, nil
	})
	defer restore()

	_, _, err := buildHostTargetArgv(hostSpec(), "/proj", []string{"/bin/zsh", "-l"}, nil)
	if err != nil {
		t.Fatalf("buildHostTargetArgv: %v", err)
	}
}

func TestBuildHostTargetArgv_WindowsHostUsesWindowsLauncher(t *testing.T) {
	h := hostSpec()
	h.OS = "MINGW64_NT-10.0-26200"
	_, args, err := buildHostTargetArgv(h, "C:/proj", []string{"claude"}, nil)
	if err != nil {
		t.Fatalf("buildHostTargetArgv: %v", err)
	}
	if remote := args[len(args)-1]; !strings.HasPrefix(remote, `sh -c "set -f; IFS=; eval `) {
		t.Fatalf("remote = %q, want the Windows launcher", remote)
	}
}
