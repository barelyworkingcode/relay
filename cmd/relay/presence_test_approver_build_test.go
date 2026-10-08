package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// taCleanEnv drops git's repository variables, which a hook-run suite
// inherits and which would redirect the child go and script runs.
func taCleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

func taRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	assertNoErr(t, err, "repo root")
	return root
}

// taCheck runs the absence script and returns its exit code and output. The
// wait is the process exit.
func taCheck(t *testing.T, mode, binary string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(taRepoRoot(t), "scripts", "check-test-approver.sh"), mode, binary)
	cmd.Env = taCleanEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("check-test-approver.sh %s: %v", mode, err)
	}
	return ee.ExitCode(), string(out)
}

func TestTestApproverCheck_ReleaseBinaryHasNoApprover(t *testing.T) {
	if code, out := taCheck(t, "absent", relayBinary(t)); code != 0 {
		t.Fatalf("absent on the tagless binary: exit %d\n%s", code, out)
	}
}

func TestTestApproverCheck_TaggedBuildHasTheApprover(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "relay-testapprover")
	build := exec.Command("go", "build", "-tags", "testapprover", "-o", bin, "./cmd/relay")
	build.Dir = taRepoRoot(t)
	build.Env = taCleanEnv()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build -tags testapprover: %v\n%s", err, out)
	}
	if code, out := taCheck(t, "present", bin); code != 0 {
		t.Fatalf("present on the tagged binary: exit %d\n%s", code, out)
	}
	if code, out := taCheck(t, "absent", bin); code != 1 || strings.TrimSpace(out) == "" {
		t.Fatalf("absent on the tagged binary: exit %d, output %q; want exit 1 with a reason", code, out)
	}
}
