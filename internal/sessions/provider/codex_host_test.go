package provider

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

func TestBuildCodexHostExec_ArgvAndEmptyPath(t *testing.T) {
	spec := &sessionstypes.HostSpec{
		Name: "acme-box", SSHArgv: []string{"ssh", "-o", "BatchMode=yes", "acme@testbox"},
		CodexPath: "/opt/acme/bin/codex", OS: "darwin",
	}
	name, argv, err := buildCodexHostExec(spec, "/work/p1", "cx1")
	if err != nil {
		t.Fatal(err)
	}
	remote := sshhost.RemoteCommandForOS("darwin", "/work/p1", []string{"/opt/acme/bin/codex", "app-server"}, map[string]string{"RELAY_SESSION_ID": "cx1"})
	wantArgv := []string{"-o", "BatchMode=yes", "acme@testbox", "-T", "--", remote}
	if name != "ssh" || !reflect.DeepEqual(argv, wantArgv) {
		t.Errorf("exec = %s %q, want ssh %q", name, argv, wantArgv)
	}

	spec.CodexPath = ""
	_, _, err = buildCodexHostExec(spec, "/work/p1", "cx1")
	if want := `host "acme-box" has no codex path: set its codex template's command`; err == nil || err.Error() != want {
		t.Errorf("empty CodexPath error = %v, want %q", err, want)
	}
}

// writeFakeSSH installs an ssh stand-in that runs the remote command (its
// last argument) locally, as sshd would for a login shell.
func writeFakeSSH(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nfor a; do last=\"$a\"; done\nexec /bin/sh -c \"$last\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCodexSpawn_HostSessionRunsRemoteCommandAndAnswers(t *testing.T) {
	warns := captureWarns(t)
	bin := buildTestCodexBinary(t)
	h := newCodexHarness(t, codexFixtureOK, nil, func(s *sessionstypes.Session) {
		s.Settings = optIn
		s.SetHost(&sessionstypes.HostSpec{
			ID: "h1", Name: "acme-box", SSHArgv: []string{writeFakeSSH(t), "-o", "BatchMode=yes"},
			CodexPath: bin, OS: "darwin",
		})
	})
	h.start(t)
	h.turn(t, "go", 1)

	if got := h.rec.text(); got != "There are 2 entries." {
		t.Errorf("assistant text = %q", got)
	}
	start := h.lines(t)[0]
	if argv, _ := start["argv"].([]any); len(argv) != 2 || argv[1] != "app-server" {
		t.Errorf("remote argv = %v, want [codex app-server]", argv)
	}
	if got, _ := start["cwd"].(string); mustEval(t, got) != mustEval(t, h.dir) {
		t.Errorf("remote cwd = %q, want %q", got, h.dir)
	}
	if ts := h.sent(t, "thread/start")["params"].(map[string]any); ts["approvalPolicy"] != "never" || ts["sandbox"] != "danger-full-access" {
		t.Errorf("host thread/start params = %v", ts)
	}
	if ls := warns.lines(); len(ls) != 1 || !strings.Contains(ls[0], "cx1") || !strings.Contains(ls[0], "codex") {
		t.Errorf("relay-tools warning = %v, want exactly one naming cx1 and codex", ls)
	}
}

func TestCodexSpawn_HostWithoutCodexPathFailsStart(t *testing.T) {
	h := newCodexHarness(t, codexFixtureOK, nil, func(s *sessionstypes.Session) {
		s.SetHost(&sessionstypes.HostSpec{Name: "acme-box", SSHArgv: []string{"ssh"}})
	})
	if err := h.p.Start(); err == nil || !strings.Contains(err.Error(), "has no codex path") {
		t.Fatalf("Start error = %v, want the no-codex-path error", err)
	}
}
