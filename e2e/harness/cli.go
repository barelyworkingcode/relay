package harness

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CLIOpts tune one CLI call.
type CLIOpts struct {
	Trace    string // "" = fresh trace; not added when args[0] == "logs"
	Stdin    []byte
	Deadline time.Duration // default 60s; the process is killed and t fails at the deadline
}

const defaultCLIDeadline = 60 * time.Second

func (o CLIOpts) deadline() time.Duration {
	if o.Deadline > 0 {
		return o.Deadline
	}
	return defaultCLIDeadline
}

func (i *Instance) cliArgs(o CLIOpts, args []string) ([]string, string) {
	out := []string{"--config-dir", i.ConfigDir}
	trace := ""
	// For `relay logs` --trace is the filter, so the harness never injects one.
	if len(args) == 0 || args[0] != "logs" {
		trace = o.Trace
		if trace == "" {
			trace = NewTrace(i.t)
		}
		out = append(out, "--trace", trace)
	}
	return append(out, args...), trace
}

// CLI runs the relay CLI against this instance and returns its result whatever
// the exit code.
func (i *Instance) CLI(args ...string) Result { return i.CLIWith(CLIOpts{}, args...) }

// CLIWith is CLI with options.
func (i *Instance) CLIWith(o CLIOpts, args ...string) Result {
	i.t.Helper()
	return i.StartCLI(o, args...).Wait()
}

// MustCLI runs the CLI and fails t unless it exits 0.
func (i *Instance) MustCLI(args ...string) Result {
	i.t.Helper()
	r := i.CLI(args...)
	if r.Code != 0 {
		i.t.Fatalf("relay %v exited %d\nstdout: %s\nstderr: %s", args, r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// StartCLI starts the CLI and returns before it exits.
func (i *Instance) StartCLI(o CLIOpts, args ...string) *Proc {
	i.t.Helper()
	full, trace := i.cliArgs(o, args)
	p := startProc(i.t, procSpec{
		bin: i.bin, args: full, env: i.env, dir: i.Dir,
		stdin: o.Stdin, deadline: o.deadline(), trace: trace,
	})
	p.onResult = i.noteStderr
	return p
}

// RunAt runs the CLI against any config dir, with no instance behind it: the
// cases where no server is expected.
func RunAt(t *testing.T, configDir string, args ...string) Result {
	t.Helper()
	home := t.TempDir()
	env := buildEnv(home, home, filepath.Join(home, ".local", "bin"))
	trace := ""
	full := []string{"--config-dir", configDir}
	if len(args) == 0 || args[0] != "logs" {
		trace = NewTrace(t)
		full = append(full, "--trace", trace)
	}
	p := startProc(t, procSpec{
		bin: bundle.Relay, args: append(full, args...), env: env, dir: home,
		deadline: defaultCLIDeadline, trace: trace,
	})
	return p.Wait()
}

// ScratchDir returns a path under the run root that does not exist yet, for
// tests that need a config dir no instance owns. It is removed at cleanup.
func ScratchDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(runRoot, "s"+NewTrace(t)[3:11])
	t.Cleanup(func() {
		if !t.Failed() {
			_ = os.RemoveAll(d)
		}
	})
	return d
}
