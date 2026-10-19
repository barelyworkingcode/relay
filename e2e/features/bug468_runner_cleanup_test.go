package features

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// An instance's cleanup ends every process that names its directory, so the
// detached remote-agent runner and its agent child do not outlive the test.
func TestInstanceCleanupEndsHostAgentRunner(t *testing.T) {
	t.Parallel()

	var instDir string
	t.Run("session", func(t *testing.T) {
		r := g11dStart(t)
		instDir = r.i.Dir
		sid := r.startSession(t, "")
		// session start returns only after the generation's run.json exists,
		// so the runner is up. The session is left running on purpose, and
		// r.run is not called because it kills the group at cleanup.
		runner := filepath.Join(r.agentsDir(sid), "g1", "runner.sh")
		if _, err := os.Stat(runner); err != nil {
			t.Fatalf("the started host session has no runner script: %v", err)
		}
	})
	if instDir == "" {
		t.Fatal("the subtest recorded no instance directory")
	}

	// Subtest cleanups have run. Cleanup is synchronous, so ps is read once.
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var left []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, instDir+"/") {
			continue
		}
		left = append(left, line)
		if f := strings.Fields(line); len(f) > 0 {
			if pid, err := strconv.Atoi(f[0]); err == nil && pid > 1 && pid != os.Getpid() {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
	if len(left) > 0 {
		t.Fatalf("%d process(es) outlived the instance directory:\n%s", len(left), strings.Join(left, "\n"))
	}
}
