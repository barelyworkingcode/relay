package service

import (
	"os/exec"
	"strings"
	"testing"
)

func traceEntries(cmd *exec.Cmd) []string {
	var out []string
	for _, kv := range cmd.Environ() {
		if strings.HasPrefix(kv, "RELAY_TRACE_ID=") {
			out = append(out, kv)
		}
	}
	return out
}

func TestSetTraceEnv(t *testing.T) {
	const good = "abcd1234efgh5678"
	cases := []struct {
		name   string
		inCmd  []string
		id     string
		want   []string
		others string
	}{
		{"sets a valid id", []string{"KEEP=1"}, good, []string{"RELAY_TRACE_ID=" + good}, "KEEP=1"},
		{"collapses duplicates to one", []string{"RELAY_TRACE_ID=stale-one-1", "KEEP=1", "RELAY_TRACE_ID=stale-two-2"}, good, []string{"RELAY_TRACE_ID=" + good}, "KEEP=1"},
		{"empty id scrubs a configured value", []string{"RELAY_TRACE_ID=stale-one-1", "KEEP=1"}, "", nil, "KEEP=1"},
		{"invalid id scrubs and sets nothing", []string{"RELAY_TRACE_ID=stale-one-1", "KEEP=1"}, "bad id\nX=1", nil, "KEEP=1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command("true")
			cmd.Env = c.inCmd
			SetTraceEnv(cmd, c.id)
			got := traceEntries(cmd)
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("RELAY_TRACE_ID entries = %v, want %v", got, c.want)
			}
			if !strings.Contains(strings.Join(cmd.Env, "|"), c.others) {
				t.Errorf("unrelated entry %q lost: %v", c.others, cmd.Env)
			}
			if strings.Contains(strings.Join(cmd.Env, "\n"), "X=1") {
				t.Errorf("invalid id leaked into the environment: %v", cmd.Env)
			}
		})
	}
}

func TestSetTraceEnvScrubsInheritedValue(t *testing.T) {
	t.Setenv("RELAY_TRACE_ID", "inherited-trace-id")
	cmd := exec.Command("true")
	SetTraceEnv(cmd, "")
	if got := traceEntries(cmd); len(got) != 0 {
		t.Errorf("inherited value reached the child: %v", got)
	}
}
