//go:build live

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

// The profile relay writes for a read-only-projects launch, run under
// sandbox-exec: the kernel, not the tool list, keeps every project unwritable
// and everything else unreadable.
func TestLive_ReadOnlyProjectsProfileEnforces(t *testing.T) {
	const sandboxExec = "/usr/bin/sandbox-exec"
	if _, err := os.Stat(sandboxExec); err != nil {
		t.Fatalf("sandbox-exec unavailable: %v", err)
	}
	// The fixtures sit under the real home directory, not temp: temp is
	// readable and writable to every session, so a probe there proves nothing.
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	base, err := os.MkdirTemp(realHome, "relay-ro-live-")
	if err != nil {
		t.Fatalf("mkdir under home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base = sandboxRealPath(t, base)

	own, other, outside := filepath.Join(base, "own"), filepath.Join(base, "other"), filepath.Join(base, "outside")
	for _, d := range []string{own, other, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	otherFile, outsideFile := filepath.Join(other, "b.txt"), filepath.Join(outside, "c.txt")
	for _, f := range []string{otherFile, outsideFile} {
		if err := os.WriteFile(f, []byte("fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	store := newLaunchTestStore(t)
	ownProj := addLaunchTestProject(t, store, func(p *config.Project) { p.Path = own })
	addLaunchTestProject(t, store, func(p *config.Project) { p.ID = "p2"; p.Name = "Beta"; p.Path = other })
	_, body := launchWithSandbox(t, readOnlyLaunchRequest(ownProj, `{"readOnlyProjects":true}`), store)
	profile := filepath.Join(t.TempDir(), "profile.sb")
	if err := os.WriteFile(profile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(argv ...string) (string, bool) {
		// Run from "/": the sandbox cannot read the test's working directory,
		// and the shell's complaint about that would also say EPERM.
		cmd := exec.Command(sandboxExec, append([]string{"-f", profile, "--"}, argv...)...)
		cmd.Dir = "/"
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), true
		}
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("%v did not run: %v", argv, err)
		}
		return string(out), false
	}
	if out, ok := run("/bin/cat", otherFile); !ok {
		t.Errorf("a file in another project root was unreadable: %s", out)
	}
	newFile := filepath.Join(own, "new.txt")
	if out, ok := run("/bin/sh", "-c", `echo x > "$1"`, "sh", newFile); ok || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("a write in the session's own root: ok=%v, want EPERM: %s", ok, out)
	}
	if _, err := os.Stat(newFile); err == nil {
		t.Error("the refused write left a file on disk")
	}
	if out, ok := run("/bin/cat", outsideFile); ok || !strings.Contains(out, "Operation not permitted") {
		t.Errorf("a read outside every root: ok=%v, want EPERM: %s", ok, out)
	}
}
