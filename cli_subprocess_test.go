package main

// Every brokered command's refusal is exitError, which calls os.Exit(1) —
// the established convention in this suite (enrol_cmd_test.go's own
// comment) is to test around that rather than kill the test process, but
// AC-11/AC-12 are specifically about what a real process does: exits
// non-zero, names the command, and never touches settings.json. The only
// way to observe that honestly is a subprocess, so this file builds one
// re-exec helper and uses it for every brokered command.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestHelperProcess is not a real test. go test still discovers and runs
// it, so the GO_WANT_HELPER_PROCESS guard is what keeps an ordinary
// `go test ./...` invocation from doing anything here; runCLISubprocess is
// the only caller that sets it, and does so only in a child process it
// spawned for exactly this purpose.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.Args = append([]string{"relay"}, args...)
	main()
}

// runCLISubprocess re-executes this test binary as `relay --config-dir dir
// <args...>`, so an exitError's os.Exit(1) lands in a child process instead
// of the suite. Returns combined stdout+stderr and the exit code.
func runCLISubprocess(t *testing.T, dir string, args ...string) (output string, exitCode int) {
	t.Helper()
	full := append([]string{"-test.run=TestHelperProcess", "--", "--config-dir", dir}, args...)
	cmd := exec.Command(os.Args[0], full...)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run subprocess %v: %v\noutput:\n%s", args, err, out)
	}
	return string(out), exitErr.ExitCode()
}

// brokeredCLICommand is one row of AC-11/AC-12's table: a command that must
// refuse by its own name when relay is not running, and a minimal legal
// argument list for it (the refusal must fire before argument validation
// would matter, but a well-formed invocation is what proves the refusal
// isn't hiding a flag-parsing error instead).
type brokeredCLICommand struct {
	name string
	args []string
}

// sharedRefusalTestCSRPath writes one valid CSR fixture, once per test
// binary run, for the "relay enrol sign" row below: brokeredCLICommands
// builds its table without a *testing.T, so the CSR has to already exist
// on disk under a stable path before any subtest reads it via --csr.
var (
	sharedRefusalTestCSRPathOnce sync.Once
	sharedRefusalTestCSRPath     string
)

func sharedRefusalTestCSRFile(t *testing.T) string {
	t.Helper()
	sharedRefusalTestCSRPathOnce.Do(func() {
		f, err := os.CreateTemp("", "enrol-sign-refusal-*.csr")
		if err != nil {
			t.Fatalf("create shared CSR fixture: %v", err)
		}
		defer f.Close()
		if _, err := f.Write(genClientCSRPEM(t, "cli-refuse-test")); err != nil {
			t.Fatalf("write shared CSR fixture: %v", err)
		}
		sharedRefusalTestCSRPath = f.Name()
	})
	return sharedRefusalTestCSRPath
}

func brokeredCLICommands(t *testing.T) []brokeredCLICommand {
	return []brokeredCLICommand{
		{"relay credential mint", []string{"credential", "mint", "--name", "x", "--class", "read"}},
		{"relay credential revoke", []string{"credential", "revoke", "--id", "x"}},
		{"relay enrol create", []string{"enrol", "create", "--client-id", "x"}},
		{"relay enrol sign", []string{"enrol", "sign", "--client-id", "x", "--csr", sharedRefusalTestCSRFile(t)}},
		{"relay enrol update", []string{"enrol", "update", "--client-id", "x", "--max-calls", "5"}},
		{"relay enrol revoke", []string{"enrol", "revoke", "--client-id", "x"}},
		{"relay login enrol", []string{"login", "enrol"}},
		{"relay login revoke", []string{"login", "revoke", "--id", "x"}},
		{"relay mcp register", []string{"mcp", "register", "--name", "x", "--command", "/bin/true"}},
		{"relay mcp unregister", []string{"mcp", "unregister", "--id", "x"}},
		{"relay service register", []string{"service", "register", "--name", "x", "--command", "/bin/true"}},
		{"relay service unregister", []string{"service", "unregister", "--id", "x"}},
		{"relay service restart", []string{"service", "restart", "--id", "x"}},
	}
}

// TestBrokeredCommands_RefuseByNameAndTouchNothing is AC-11 and AC-12: with
// no bridge socket, every mutating command exits non-zero, names itself and
// says "requires the service" — never a settings or generic bridge error —
// and leaves the (empty) config dir exactly as it found it.
func TestBrokeredCommands_RefuseByNameAndTouchNothing(t *testing.T) {
	for _, c := range brokeredCLICommands(t) {
		t.Run(c.name, func(t *testing.T) {
			dir := mkShortTempDir(t, "relay-refuse-")
			out, code := runCLISubprocess(t, dir, c.args...)

			if code == 0 {
				t.Fatalf("%s: exited 0 with no service running; output:\n%s", c.name, out)
			}
			if !strings.Contains(out, c.name) {
				t.Errorf("%s: refusal does not name the command: %q", c.name, out)
			}
			if !strings.Contains(out, "requires the service") {
				t.Errorf("%s: refusal is not the ADR-017 §7.3 text: %q", c.name, out)
			}

			if _, err := os.Stat(filepath.Join(dir, "settings.json")); err == nil {
				t.Fatalf("%s: settings.json was created by a refused command", c.name)
			}
		})
	}
}
