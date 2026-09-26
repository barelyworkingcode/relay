package provider

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const msgProviderExitedWithError = "provider exited with error"

// exitRecorder records every event kind in arrival order and hands over a
// snapshot of what had arrived when process_exited was delivered.
type exitRecorder struct {
	mu     sync.Mutex
	kinds  []string
	exited chan []string
}

func newExitRecorder() *exitRecorder { return &exitRecorder{exited: make(chan []string, 1)} }

func (r *exitRecorder) handle(kind string, _ json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, kind)
	if kind == "process_exited" {
		select {
		case r.exited <- slices.Clone(r.kinds):
		default:
		}
	}
}

func (r *exitRecorder) waitExited(t *testing.T, within time.Duration) []string {
	t.Helper()
	select {
	case before := <-r.exited:
		return before
	case <-time.After(within):
		t.Fatalf("no process_exited within %s", within)
		return nil
	}
}

// spawnFake starts a claude or pi provider whose binary is the given script
// and returns its Kill.
func spawnFake(t *testing.T, kind, id, script string, handler sessionstypes.EventHandler) (kill func()) {
	t.Helper()
	sess := &sessionstypes.Session{ID: id, Model: "m", Directory: t.TempDir()}
	var start func() error
	switch kind {
	case "claude":
		p := NewClaudeProvider(sess, handler, ClaudeConfig{Binary: writeFakeCLI(t, script)}, nil)
		start, kill = p.Start, p.Kill
	case "pi":
		p := NewPiProvider(sess, handler, PiConfig{Binary: writeFakeCLI(t, script), DataDir: t.TempDir()})
		start, kill = p.Start, p.Kill
	default:
		t.Fatalf("unknown provider kind %q", kind)
	}
	t.Cleanup(kill)
	if err := start(); err != nil {
		t.Fatalf("%s Start: %v", kind, err)
	}
	return kill
}

func printLine(line string) string { return "printf '%s\\n' '" + line + "'\n" }

var finalEvents = map[string]string{
	"claude": `{"type":"result","subtype":"success"}`,
	"pi":     `{"type":"agent_end","messages":[],"willRetry":false}`,
}

func TestProviderExit_FinalStdoutEventPrecedesProcessExited(t *testing.T) {
	const runs = 200
	for _, kind := range []string{"claude", "pi"} {
		t.Run(kind, func(t *testing.T) {
			lost := 0
			for i := 0; i < runs; i++ {
				rec := newExitRecorder()
				spawnFake(t, kind, fmt.Sprintf("final-%s-%d", kind, i), printLine(finalEvents[kind])+"exit 0\n", rec.handle)
				if !slices.Contains(rec.waitExited(t, 5*time.Second), events.HandlerMessageComplete) {
					lost++
				}
			}
			if lost > 0 {
				t.Fatalf("final event missing before process_exited in %d of %d runs", lost, runs)
			}
		})
	}
}

type exitWarnRecord struct {
	Level      string   `json:"level"`
	Msg        string   `json:"msg"`
	Session    string   `json:"session"`
	Kind       string   `json:"kind"`
	ExitCode   int      `json:"exitCode"`
	StderrTail []string `json:"stderr_tail"`
}

func exitWarnRecords(log, session string) []exitWarnRecord {
	var out []exitWarnRecord
	for _, l := range strings.Split(log, "\n") {
		var r exitWarnRecord
		if json.Unmarshal([]byte(l), &r) == nil && r.Msg == msgProviderExitedWithError && r.Session == session {
			out = append(out, r)
		}
	}
	return out
}

func TestProviderExit_NonZeroExitWarnsWithStderrTail(t *testing.T) {
	modelKey := "rmk_" + strings.Repeat("0a", 32)
	var ringLines []string
	for i := 1; i <= 12; i++ {
		ringLines = append(ringLines, fmt.Sprintf("err-e%02d", i))
	}
	cases := []struct {
		name, kind  string
		stderrLines []string
		exitCode    int
		tail        [][]string // per tail entry, oldest first: substrings it must hold
		absent      string
	}{
		{"claude crash", "claude", []string{"fatal: p1 crashed mid-session"}, 3, [][]string{{"fatal: p1 crashed mid-session"}}, ""},
		{"pi crash", "pi", []string{"fatal: p1 crashed mid-session"}, 3, [][]string{{"fatal: p1 crashed mid-session"}}, ""},
		{"tail redacts secrets", "claude", []string{"auth failed " + modelKey + " rejected"}, 3, [][]string{{"auth failed ", " rejected"}}, modelKey},
		{"tail keeps the last ten lines", "claude", ringLines, 3, wholeLines(ringLines[2:]), ""},
		{"zero exit logs nothing", "claude", []string{"note: p1 finished"}, 0, nil, ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureDebugJSON(t)
			id := fmt.Sprintf("exit-warn-%d", i)
			script := printLine(claudeInitLine("[]"))
			for _, l := range tc.stderrLines {
				script += "echo '" + l + "' >&2\n"
			}
			script += fmt.Sprintf("exit %d\n", tc.exitCode)
			rec := newExitRecorder()
			var logAtExit string
			spawnFake(t, tc.kind, id, script, func(kind string, data json.RawMessage) {
				if kind == "process_exited" {
					logAtExit = logs.all()
				}
				rec.handle(kind, data)
			})
			rec.waitExited(t, 5*time.Second)

			got := exitWarnRecords(logAtExit, id)
			if tc.exitCode == 0 {
				if len(got) != 0 {
					t.Fatalf("zero exit logged %q: %+v", msgProviderExitedWithError, got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want exactly one %q record before process_exited, got %d; log:\n%s", msgProviderExitedWithError, len(got), logAtExit)
			}
			r := got[0]
			if r.Level != "WARN" || r.Kind != tc.kind || r.ExitCode != tc.exitCode {
				t.Fatalf("record = %+v, want level WARN kind %s exitCode %d", r, tc.kind, tc.exitCode)
			}
			if len(r.StderrTail) != len(tc.tail) {
				t.Fatalf("stderr_tail = %q, want %d entries", r.StderrTail, len(tc.tail))
			}
			for j, parts := range tc.tail {
				for _, p := range parts {
					if !strings.Contains(r.StderrTail[j], p) {
						t.Fatalf("stderr_tail[%d] = %q, want it to hold %q (tail %q)", j, r.StderrTail[j], p, r.StderrTail)
					}
				}
				if tc.absent != "" && strings.Contains(r.StderrTail[j], tc.absent) {
					t.Fatalf("stderr_tail[%d] = %q holds %q", j, r.StderrTail[j], tc.absent)
				}
			}
		})
	}
}

func wholeLines(lines []string) [][]string {
	out := make([][]string, len(lines))
	for i, l := range lines {
		out[i] = []string{l}
	}
	return out
}

// sleep dies of the SIGINT Kill sends, so the exit is never a clean zero.
func TestProviderExit_KillIsNotACrash(t *testing.T) {
	for _, kind := range []string{"claude", "pi"} {
		t.Run(kind, func(t *testing.T) {
			logs := captureDebugJSON(t)
			id := "kill-" + kind
			rec := newExitRecorder()
			kill := spawnFake(t, kind, id, "exec sleep 30\n", rec.handle)
			kill()
			rec.waitExited(t, 5*time.Second)
			if got := exitWarnRecords(logs.all(), id); len(got) != 0 {
				t.Fatalf("Kill logged %q: %+v", msgProviderExitedWithError, got)
			}
		})
	}
}
