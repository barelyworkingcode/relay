//go:build live

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// execUnderProfile runs argv under sandbox-exec and returns its combined
// output and whether it exited 0. A binary that never started is a failure of
// the test, not a refusal by the profile.
func execUnderProfile(t *testing.T, profile string, argv ...string) (string, bool) {
	t.Helper()
	out, err := exec.Command(sandboxExecPath, append([]string{"-f", profile, "--"}, argv...)...).CombinedOutput()
	if err == nil {
		return string(out), true
	}
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("%v did not run: %v", argv, err)
	}
	return string(out), false
}

// A read-only-projects profile: every project root readable, none writable,
// and nothing readable beyond the roots, the template's folders and temp.
// The fixtures sit under a directory in $HOME, not in temp, because temp is
// granted read-write and would make every probe pass for the wrong reason.
func TestLive_ReadOnlyProjectsProfile(t *testing.T) {
	if err := Available(); err != nil {
		t.Skipf("sandbox-exec unavailable: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(home, "relay-ro-live-")
	if err != nil {
		t.Fatalf("mkdir under home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base = evalSymlinks(t, base)

	own, other, outside := filepath.Join(base, "own"), filepath.Join(base, "other"), filepath.Join(base, "outside")
	mkdirs(t, own, other, outside)
	ownFile, otherFile, outsideFile := filepath.Join(own, "a.txt"), filepath.Join(other, "b.txt"), filepath.Join(outside, "c.txt")
	for _, f := range []string{ownFile, otherFile, outsideFile} {
		if err := os.WriteFile(f, []byte("fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := Write(filepath.Join(evalSymlinks(t, t.TempDir()), "profiles"), "read-only-session", Spec{
		Read:      []string{own, other},
		ReadWrite: []string{evalSymlinks(t, os.TempDir()), "/dev"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if out, ok := execUnderProfile(t, profile, "/bin/cat", otherFile); !ok {
		t.Errorf("a file in another project root was unreadable: %s", out)
	}
	if out, ok := execUnderProfile(t, profile, "/bin/sh", "-c", "echo x > "+filepath.Join(own, "new.txt")); ok || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("a write in the session's own root: ok=%v, want EPERM: %s", ok, out)
	}
	if _, err := os.Stat(filepath.Join(own, "new.txt")); err == nil {
		t.Error("the refused write left a file on disk")
	}
	if out, ok := execUnderProfile(t, profile, "/bin/cat", outsideFile); ok || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("a read outside both roots: ok=%v, want EPERM: %s", ok, out)
	}
}
