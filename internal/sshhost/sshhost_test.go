package sshhost

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
)

// TestMain redirects bridge.ConfigDir() at a per-run temp directory for
// every test in this package: ControlDir (and everything that calls it —
// Probe, Check, Disconnect) creates a real directory as a side effect, and
// the suite must never touch ~/Library/Application Support/relay (the
// headline sandbox rule; see cmd/relay's support_test.go for the same
// discipline).
func TestMain(m *testing.M) {
	// /tmp directly, not os.MkdirTemp(""): macOS's per-user TMPDIR
	// (/var/folders/.../T) is itself long enough that ControlDir's own
	// dir+%C-hash budget would trip on the TEST fixture rather than on
	// anything this package does (mirrors cmd/relay's mkShortTempDir).
	dir, err := os.MkdirTemp("/tmp", "relay-sshhost-test-")
	if err != nil {
		panic(err)
	}
	bridge.SetConfigDirForTest(dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// decodeLauncher extracts the base64 payload RemoteCommand wraps and
// decodes it, so a test can compare against docs/ssh-hosts.md's Fixtures
// section, which pins the DECODED script.
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

// TestRemoteCommand_Fixtures pins docs/ssh-hosts.md's Fixtures section
// byte-for-byte, shared with relayLLM's vendored sshhost.go and eve's
// ssh-command.js so all three cannot drift apart.
func TestRemoteCommand_Fixtures(t *testing.T) {
	cases := []struct {
		name string
		cwd  string
		argv []string
		env  map[string]string
		want string
	}{
		{
			name: "cwd with a space, apostrophe in an arg, no env",
			cwd:  "/home/a b",
			argv: []string{"/usr/bin/claude", "--print", "it's"},
			env:  map[string]string{},
			want: `cd '/home/a b' && exec env '/usr/bin/claude' '--print' 'it'\''s'`,
		},
		{
			name: "no cwd, one env var",
			cwd:  "",
			argv: []string{"cat", "/x/y.jsonl"},
			env:  map[string]string{"TERM": "xterm-256color"},
			want: `exec env 'TERM'='xterm-256color' 'cat' '/x/y.jsonl'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeLauncher(t, RemoteCommand(tc.cwd, tc.argv, tc.env))
			if got != tc.want {
				t.Fatalf("decoded script mismatch\n got:  %q\n want: %q", got, tc.want)
			}
		})
	}
}

// TestRemoteCommand_DeterministicEnvOrder guards the "sort keys" rule
// directly: an unordered map must never produce two different command
// strings for the same logical input.
func TestRemoteCommand_DeterministicEnvOrder(t *testing.T) {
	env := map[string]string{"ZETA": "1", "ALPHA": "2", "MID": "3"}
	first := RemoteCommand("", []string{"true"}, env)
	for i := 0; i < 20; i++ {
		if got := RemoteCommand("", []string{"true"}, env); got != first {
			t.Fatalf("RemoteCommand is not deterministic across calls:\n%q\n%q", first, got)
		}
	}
	decoded := decodeLauncher(t, first)
	wantOrder := "exec env 'ALPHA'='2' 'MID'='3' 'ZETA'='1' 'true'"
	if decoded != wantOrder {
		t.Fatalf("expected sorted env keys, got %q", decoded)
	}
}

func TestSSHArgv_Order(t *testing.T) {
	h := config.Host{Target: "admin@devbox.local"}
	got := SSHArgv(h, "/tmp/ctl")
	want := []string{
		"ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=/tmp/ctl/%C",
		"-o", "ControlPersist=600",
		"admin@devbox.local",
	}
	assertStringSlicesEqual(t, got, want)
}

func TestSSHArgv_PortAndIdentityFile(t *testing.T) {
	h := config.Host{Target: "devbox", Port: 2222, IdentityFile: "/Users/admin/.ssh/id_ed25519"}
	got := SSHArgv(h, "/tmp/ctl")
	want := []string{
		"ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=/tmp/ctl/%C",
		"-o", "ControlPersist=600",
		"-p", "2222",
		"-i", "/Users/admin/.ssh/id_ed25519",
		"devbox",
	}
	assertStringSlicesEqual(t, got, want)
}

func TestSSHArgv_PortZeroOmitted(t *testing.T) {
	h := config.Host{Target: "devbox", Port: 0}
	got := SSHArgv(h, "/tmp/ctl")
	for _, a := range got {
		if a == "-p" {
			t.Fatalf("expected no -p flag when port is 0, got %v", got)
		}
	}
}

func assertStringSlicesEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length mismatch\n got:  %v\n want: %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("mismatch at index %d\n got:  %v\n want: %v", i, got, want)
		}
	}
}

// canned output a real probe script would produce, used to test the parser
// without shelling out.
const cannedProbeOutput = `Last login: Thu Sep  4 12:00:00 on ttys000
@@RELAY_PROBE_OS@@
Darwin
@@RELAY_PROBE_ARCH@@
arm64
@@RELAY_PROBE_HOME@@
/Users/admin
@@RELAY_PROBE_SHELL@@
/bin/zsh
@@RELAY_PROBE_LOGIN_NODE@@
/opt/homebrew/bin/node
@@RELAY_PROBE_LOGIN_CLAUDE@@
/opt/homebrew/bin/claude
@@RELAY_PROBE_PLAIN_NODE@@
/opt/homebrew/bin/node
@@RELAY_PROBE_PLAIN_CLAUDE@@
/opt/homebrew/bin/claude
@@RELAY_PROBE_END@@
`

func TestParseProbeSections(t *testing.T) {
	sections := parseProbeSections(cannedProbeOutput)
	cases := map[string]string{
		sentinelOS:          "Darwin",
		sentinelArch:        "arm64",
		sentinelHome:        "/Users/admin",
		sentinelShell:       "/bin/zsh",
		sentinelLoginNode:   "/opt/homebrew/bin/node",
		sentinelLoginClaude: "/opt/homebrew/bin/claude",
		sentinelPlainNode:   "/opt/homebrew/bin/node",
		sentinelPlainClaude: "/opt/homebrew/bin/claude",
	}
	for sentinel, want := range cases {
		if got := firstLine(sections[sentinel]); got != want {
			t.Fatalf("section %q: got %q, want %q", sentinel, got, want)
		}
	}
}

// TestParseProbeSections_MissingLoginPaths matches the doc's fallback rule:
// a tool absent from the login shell's PATH prints nothing at all (not even
// a blank line), and the plain fallback must still be readable.
func TestParseProbeSections_MissingLoginPaths(t *testing.T) {
	output := `@@RELAY_PROBE_OS@@
Linux
@@RELAY_PROBE_ARCH@@
x86_64
@@RELAY_PROBE_HOME@@
/home/admin
@@RELAY_PROBE_SHELL@@
/bin/bash
@@RELAY_PROBE_LOGIN_NODE@@
@@RELAY_PROBE_LOGIN_CLAUDE@@
@@RELAY_PROBE_PLAIN_NODE@@
/usr/local/bin/node
@@RELAY_PROBE_PLAIN_CLAUDE@@
/usr/local/bin/claude
@@RELAY_PROBE_END@@
`
	sections := parseProbeSections(output)
	if got := firstLine(sections[sentinelLoginNode]); got != "" {
		t.Fatalf("expected empty login node path, got %q", got)
	}
	if got := firstLine(sections[sentinelPlainNode]); got != "/usr/local/bin/node" {
		t.Fatalf("expected fallback node path, got %q", got)
	}
}

// TestProbe_ParsesCannedTransport stubs runner so Probe never shells out —
// the hermetic tier must not touch a real ssh process (the live path is
// covered separately, tagged //go:build live).
func TestProbe_ParsesCannedTransport(t *testing.T) {
	origRunner := runner
	defer func() { runner = origRunner }()

	calls := 0
	runner = func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		calls++
		switch calls {
		case 1:
			return []byte(cannedProbeOutput), nil, nil
		case 2:
			return []byte("v24.7.0\n"), nil, nil
		case 3:
			return []byte("2.1.258 (Claude Code)\n"), nil, nil
		}
		t.Fatalf("unexpected extra runner call %d", calls)
		return nil, nil, nil
	}

	h := config.Host{Name: "devbox", Target: "admin@devbox.local"}
	probe, err := Probe(context.Background(), h)
	if err != nil {
		t.Fatalf("Probe returned an error: %v", err)
	}
	if !probe.OK {
		t.Fatalf("expected ok=true, got probe=%+v", probe)
	}
	if probe.OS != "Darwin" || probe.Arch != "arm64" {
		t.Fatalf("unexpected os/arch: %+v", probe)
	}
	if probe.NodePath != "/opt/homebrew/bin/node" || probe.NodeVersion != "v24.7.0" {
		t.Fatalf("unexpected node result: %+v", probe)
	}
	if probe.ClaudePath != "/opt/homebrew/bin/claude" || probe.ClaudeVersion != "2.1.258 (Claude Code)" {
		t.Fatalf("unexpected claude result: %+v", probe)
	}
}

func TestProbe_TransportFailureYieldsOkFalseNotAnError(t *testing.T) {
	origRunner := runner
	defer func() { runner = origRunner }()
	runner = func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		return nil, []byte("ssh: connect to host devbox.local port 22: Connection refused\n"), errors.New("exit status 255")
	}

	probe, err := Probe(context.Background(), config.Host{Name: "devbox", Target: "admin@devbox.local"})
	if err != nil {
		t.Fatalf("expected no Go error, got %v", err)
	}
	if probe.OK {
		t.Fatal("expected ok=false")
	}
	if !strings.Contains(probe.Error, "Connection refused") {
		t.Fatalf("expected ssh's stderr to surface, got %q", probe.Error)
	}
}

func TestCheck_UsesControlOptCheck(t *testing.T) {
	origRunner := runner
	defer func() { runner = origRunner }()
	var gotArgs []string
	runner = func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		gotArgs = args
		return nil, nil, nil
	}
	if !Check(config.Host{Target: "devbox"}) {
		t.Fatal("expected Check to report true when runner succeeds")
	}
	found := false
	for i, a := range gotArgs {
		if a == "-O" && i+1 < len(gotArgs) && gotArgs[i+1] == "check" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected -O check in argv, got %v", gotArgs)
	}
}

func TestDisconnect_UsesControlOptExit(t *testing.T) {
	origRunner := runner
	defer func() { runner = origRunner }()
	var gotArgs []string
	runner = func(ctx context.Context, name string, args []string) ([]byte, []byte, error) {
		gotArgs = args
		return nil, nil, nil
	}
	if err := Disconnect(config.Host{Target: "devbox"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for i, a := range gotArgs {
		if a == "-O" && i+1 < len(gotArgs) && gotArgs[i+1] == "exit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected -O exit in argv, got %v", gotArgs)
	}
}

func TestControlDir_CreatesShortPath(t *testing.T) {
	dir, err := ControlDir()
	if err != nil {
		t.Fatalf("ControlDir: %v", err)
	}
	if len(dir) >= 104 {
		t.Fatalf("control dir too long for sun_path: %q (%d bytes)", dir, len(dir))
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("expected control dir to exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %q to be a directory", dir)
	}
}

func TestControlDirFor_AvoidsWhitespaceAndLongPaths(t *testing.T) {
	if got := controlDirFor("/Users/x/Library/Application Support/relay", 501); got != "/tmp/relay-ssh-501" {
		t.Fatalf("whitespace dir: got %q", got)
	}
	if got := controlDirFor("/short/relay", 501); got != "/short/relay/run/ssh" {
		t.Fatalf("short dir: got %q", got)
	}
	long := "/" + strings.Repeat("a", 100)
	if got := controlDirFor(long, 7); got != "/tmp/relay-ssh-7" {
		t.Fatalf("long dir: got %q", got)
	}
}

// TestProbeScript_SkipsWindowsExeLoginShell runs the real probe script under
// the local sh with $SHELL pointing at a stand-in "cmd.exe" that prints a
// banner, as Windows OpenSSH's cmd.exe does when handed -lic. The banner
// must never land in the login-shell sections, where it would be read back
// as node's path.
func TestProbeScript_SkipsWindowsExeLoginShell(t *testing.T) {
	dir := t.TempDir()
	fakeCmd := dir + "/cmd.exe"
	if err := os.WriteFile(fakeCmd, []byte("#!/bin/sh\necho 'Microsoft Windows [Version 10.0]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", probeScript())
	cmd.Env = append(os.Environ(), "SHELL="+fakeCmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe script failed: %v", err)
	}
	sections := parseProbeSections(string(out))
	for _, s := range []string{sentinelLoginNode, sentinelLoginClaude} {
		if got := sections[s]; got != "" {
			t.Fatalf("section %s = %q, want empty for an .exe login shell", s, got)
		}
	}
}

func TestRemoteCommandForOS_PicksLauncherByOS(t *testing.T) {
	argv := []string{"echo", "hi"}
	for _, os := range []string{"", "Linux", "Darwin"} {
		if got, want := RemoteCommandForOS(os, "", argv, nil), RemoteCommand("", argv, nil); got != want {
			t.Errorf("os %q: got %q, want decision 8's launcher %q", os, got, want)
		}
	}
	for _, os := range []string{"MINGW64_NT-10.0-26200", "MSYS_NT-10.0", "CYGWIN_NT-10.0"} {
		got := RemoteCommandForOS(os, "", argv, nil)
		if !strings.HasPrefix(got, `sh -c "set -f; IFS=; eval $(printf %s `) {
			t.Errorf("os %q: got %q, want the Windows launcher", os, got)
		}
	}
}

// TestWindowsLauncher_EvalPreservesScript runs the Windows launcher's sh -c
// argument under the local sh: the unquoted $(…) must reach eval unsplit and
// unglobbed, so a double space and a literal * in a value survive.
func TestWindowsLauncher_EvalPreservesScript(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/match-me", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := windowsLauncherFor(buildScript(dir, []string{"printf", "[%s]", "a  b", "*"}, nil))
	arg := strings.TrimSuffix(strings.TrimPrefix(launcher, `sh -c "`), `"`)
	out, err := exec.Command("/bin/sh", "-c", arg).Output()
	if err != nil {
		t.Fatalf("launcher failed: %v", err)
	}
	if got, want := string(out), "[a  b][*]"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// decodeWindowsLauncher is decodeLauncher for windowsLauncherFor's shape.
func decodeWindowsLauncher(t *testing.T, cmd string) string {
	t.Helper()
	const prefix = `sh -c "set -f; IFS=; eval $(printf %s `
	const suffix = ` | base64 -d)"`
	if !strings.HasPrefix(cmd, prefix) || !strings.HasSuffix(cmd, suffix) {
		t.Fatalf("command %q does not match the Windows launcher shape", cmd)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(cmd, prefix), suffix))
	if err != nil {
		t.Fatalf("launcher payload is not valid base64: %v", err)
	}
	return string(decoded)
}

// The login-shell script's tail is a literal "$SHELL" -l, not shQuote'd, so
// the host's sh expands it; the cd/env prefix is buildScript's.
func TestLoginShellCommandForOS_Script(t *testing.T) {
	env := map[string]string{"TERM": "xterm-256color"}
	for _, c := range []struct {
		name, os, cwd, want string
		decode              func(*testing.T, string) string
	}{
		{"posix with cwd", "Linux", "/home/u/proj", `cd '/home/u/proj' && exec env 'TERM'='xterm-256color' "$SHELL" -l`, decodeLauncher},
		{"posix no cwd", "", "", `exec env 'TERM'='xterm-256color' "$SHELL" -l`, decodeLauncher},
		{"windows with cwd", "MINGW64_NT-10.0-26200", "/c/proj", `cd '/c/proj' && exec env 'TERM'='xterm-256color' "$SHELL" -l`, decodeWindowsLauncher},
		{"windows no cwd", "MSYS_NT-10.0", "", `exec env 'TERM'='xterm-256color' "$SHELL" -l`, decodeWindowsLauncher},
	} {
		if got := c.decode(t, LoginShellCommandForOS(c.os, c.cwd, env)); got != c.want {
			t.Errorf("%s: script = %q, want %q", c.name, got, c.want)
		}
	}
}

// Runs the POSIX launcher under the local sh with SHELL pointing at a stub:
// "$SHELL" must expand on the far side, in the cwd, with -l and the env set.
func TestLoginShellCommand_ExpandsShellOnTheFarSide(t *testing.T) {
	dir := t.TempDir()
	stub := dir + "/fake-shell"
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s|%s|%s' \"$(pwd -P)\" \"$TERM\" \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", LoginShellCommandForOS("Linux", cwd, map[string]string{"TERM": "xterm-256color"}))
	cmd.Env = append(os.Environ(), "SHELL="+stub)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("launcher failed: %v", err)
	}
	if got, want := string(out), cwd+"|xterm-256color|-l"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
