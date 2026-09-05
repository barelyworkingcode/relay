//go:build !windows

package service

import (
	"os/exec"
	"testing"
)

// spawnGroupLeader starts command as its own process group leader (the same
// shape a managed service's shell wrapper has) and reaps it in the
// background, so a signal ReclaimOrphan sends never leaves a zombie behind
// to confuse a later processGroupAlive check within the same test process.
func spawnGroupLeader(t *testing.T, command string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(command, args...)
	SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", command, err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	return cmd
}

func TestCommandIdentityMatches(t *testing.T) {
	cases := []struct {
		name          string
		cmdLine       string
		expectCommand string
		want          bool
	}{
		{"exact literal match", "/usr/bin/foo --flag", "/usr/bin/foo", true},
		{"basename match: full path in ps, bare name configured", "/usr/local/bin/mytool arg1 arg2", "mytool", true},
		{"basename match: bare name in ps, full path configured", "mytool arg1", "/usr/local/bin/mytool", true},
		{"mismatch: different tool entirely", "/usr/bin/foo", "/usr/bin/bar", false},
		{"substring in an argument is not a match", "/bin/sh -c '/usr/bin/foo bar'", "/usr/bin/foo", false},
		{"substring inside an unrelated word is not a match", "/usr/bin/pythonic --run", "python", false},
		{"empty cmdLine", "", "/usr/bin/foo", false},
		{"empty expectCommand", "/usr/bin/foo", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandIdentityMatches(tc.cmdLine, tc.expectCommand); got != tc.want {
				t.Errorf("commandIdentityMatches(%q, %q) = %v, want %v", tc.cmdLine, tc.expectCommand, got, tc.want)
			}
		})
	}
}

// An empty expectCommand means the pidfile's own config carried no command
// to verify identity against — "cannot verify" must refuse to reclaim, not
// silently skip the check the way the substring-based predecessor did.
func TestReclaimOrphan_EmptyExpectCommandDoesNotReclaim(t *testing.T) {
	cmd := spawnGroupLeader(t, "sleep", "5")
	pid := cmd.Process.Pid

	if ReclaimOrphan(pid, "") {
		t.Fatal("expected ReclaimOrphan to refuse with an empty expectCommand")
	}
	if !processGroupAlive(pid) {
		t.Fatal("the process must not have been killed when identity could not be verified")
	}
}

func TestReclaimOrphan_MismatchedCommandDoesNotReclaim(t *testing.T) {
	cmd := spawnGroupLeader(t, "sleep", "5")
	pid := cmd.Process.Pid

	if ReclaimOrphan(pid, "/usr/bin/totally-different-tool") {
		t.Fatal("expected ReclaimOrphan to refuse on a command mismatch")
	}
	if !processGroupAlive(pid) {
		t.Fatal("the process must not have been killed on a command mismatch")
	}
}

func TestReclaimOrphan_MatchingCommandReclaims(t *testing.T) {
	cmd := spawnGroupLeader(t, "sleep", "5")
	pid := cmd.Process.Pid

	if !ReclaimOrphan(pid, "sleep") {
		t.Fatal("expected ReclaimOrphan to reclaim a process whose command matches")
	}
	if processGroupAlive(pid) {
		t.Fatal("expected the process group to be gone after a successful reclaim")
	}
}
