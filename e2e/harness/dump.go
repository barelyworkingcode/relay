package harness

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// dump logs the evidence of a failed instance: the serve exit, every stored
// log line, the audit file, serve's stderr and each fake's call log. Each part
// is cut to its last lines.
func (i *Instance) dump() {
	t := i.t
	if i.serve != nil && i.stopped {
		t.Logf("[%s] serve exit code: %d", filepath.Base(i.Dir), i.serve.exitCode())
	}
	logs := execQuiet(i, "logs", "--json")
	t.Logf("[%s] relay logs (relay and relay-sessions merged):\n%s", filepath.Base(i.Dir), tailLines(logs, 300))

	if path := strings.TrimSpace(string(execQuiet(i, "audit", "--path"))); path != "" {
		data, _ := os.ReadFile(path)
		t.Logf("[%s] audit file %s:\n%s", filepath.Base(i.Dir), path, tailLines(data, 200))
	}
	if data, err := os.ReadFile(filepath.Join(i.Dir, "serve.stderr")); err == nil {
		t.Logf("[%s] serve.stderr:\n%s", filepath.Base(i.Dir), tailLines(data, 100))
	}
	calls, _ := filepath.Glob(filepath.Join(i.Dir, "fakes", "*.jsonl"))
	for _, f := range calls {
		data, _ := os.ReadFile(f)
		t.Logf("[%s] fake call log %s:\n%s", filepath.Base(i.Dir), filepath.Base(f), tailLines(data, 100))
	}
	for _, f := range i.fakeMCPs {
		if f.proc != nil {
			t.Logf("[%s] fake MCP %s stderr:\n%s", filepath.Base(i.Dir), f.spec.ID, tailLines(f.proc.result().Stderr, 100))
		}
	}
}

// execQuiet runs a CLI for the dump: it never fails the test and returns
// whatever stdout the command produced.
func execQuiet(i *Instance, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := newQuietCmd(ctx, i, args).Output()
	return out
}

func newQuietCmd(ctx context.Context, i *Instance, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, i.bin, append([]string{"--config-dir", i.ConfigDir}, args...)...)
	cmd.Env = i.env
	cmd.Dir = i.Dir
	cmd.WaitDelay = 5 * time.Second
	return cmd
}
