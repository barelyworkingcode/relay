package provider

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const (
	stderrMsg      = "provider stderr"
	stderrLimitMsg = "provider stderr: warn limit reached, later lines at debug"
)

type stderrRecord struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	Session   string `json:"session"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// captureDebugJSON swaps the process-wide default logger, like captureWarns.
func captureDebugJSON(t *testing.T) *warnBuffer {
	t.Helper()
	b := &warnBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return b
}

func stderrRecords(b *warnBuffer, session string) []stderrRecord {
	var out []stderrRecord
	for _, l := range strings.Split(b.all(), "\n") {
		var r stderrRecord
		if json.Unmarshal([]byte(l), &r) == nil && r.Session == session && (r.Msg == stderrMsg || r.Msg == stderrLimitMsg) {
			out = append(out, r)
		}
	}
	return out
}

// waitStderr waits until n provider-stderr records whose text contains substr
// have been logged for session, and returns them.
func waitStderr(t *testing.T, b *warnBuffer, session, substr string, n int, timeout time.Duration) []stderrRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var got []stderrRecord
		for _, r := range stderrRecords(b, session) {
			if r.Msg == stderrMsg && strings.Contains(r.Text, substr) {
				got = append(got, r)
			}
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s: want %d %q records containing %q, got %d; log:\n%s", session, n, stderrMsg, substr, len(got), b.all())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startClaude(t *testing.T, id, script string, handler sessionstypes.EventHandler) *ClaudeProvider {
	t.Helper()
	sess := &sessionstypes.Session{ID: id, Model: "sonnet", Directory: t.TempDir()}
	p := NewClaudeProvider(sess, handler, ClaudeConfig{Binary: writeFakeCLI(t, script)}, nil)
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(p.Kill)
	return p
}

func TestProviderStderr_LaunchFailureLineIsWarn(t *testing.T) {
	const modelKey = "acme-model-key-p1"
	failing := "echo 'launch failed: " + modelKey + " rejected' >&2\nexit 71\n"
	noop := func(string, json.RawMessage) {}
	cases := []struct {
		name, id, kind string
		start          func(t *testing.T, id string) error
	}{
		{"claude", "stderr-claude-1", "claude", func(t *testing.T, id string) error {
			sess := &sessionstypes.Session{ID: id, Model: "sonnet", Directory: t.TempDir()}
			p := NewClaudeProvider(sess, noop, ClaudeConfig{Binary: writeFakeCLI(t, failing)}, nil)
			t.Cleanup(p.Kill)
			return p.Start()
		}},
		{"claude host", "stderr-claude-host-1", "claude", func(t *testing.T, id string) error {
			sess := &sessionstypes.Session{ID: id, Model: "sonnet", Host: testHost(writeFakeCLI(t, failing))}
			p := NewClaudeProvider(sess, noop, ClaudeConfig{}, nil)
			t.Cleanup(p.Kill)
			return p.Start()
		}},
		{"pi redacts its model key", "stderr-pi-1", "pi", func(t *testing.T, id string) error {
			sess := &sessionstypes.Session{ID: id, Model: "m", Directory: t.TempDir()}
			p := NewPiProvider(sess, noop, PiConfig{Binary: writeFakeCLI(t, failing), DataDir: t.TempDir(), ModelKey: modelKey})
			t.Cleanup(p.Kill)
			return p.Start()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureDebugJSON(t)
			if err := tc.start(t, tc.id); err != nil {
				t.Fatalf("Start: %v", err)
			}
			r := waitStderr(t, logs, tc.id, "launch failed", 1, 5*time.Second)[0]
			if r.Level != "WARN" || r.Kind != tc.kind {
				t.Fatalf("record = %+v, want level WARN kind %s", r, tc.kind)
			}
			if strings.Contains(logs.all(), modelKey) {
				t.Fatalf("model key reached the log:\n%s", logs.all())
			}
		})
	}
}

func TestProviderStderr_LastLineBeforeExitIsNotLost(t *testing.T) {
	logs := captureDebugJSON(t)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("stderr-last-%d", i)
		startClaude(t, id, "echo 'fatal: p1 cannot start' >&2\nexit 1\n", func(string, json.RawMessage) {})
		if r := waitStderr(t, logs, id, "fatal: p1 cannot start", 1, 2*time.Second)[0]; r.Level != "WARN" {
			t.Fatalf("run %d: level = %s, want WARN", i, r.Level)
		}
	}
}

// The child writes one stderr line per phase and moves on only when the
// provider writes to its stdin, so each line's order against the first
// stdout line is fixed without a sleep.
func TestProviderStderr_AfterFirstStdoutIsDebug(t *testing.T) {
	logs := captureDebugJSON(t)
	const id = "stderr-seq-1"
	script := "echo early-p1 >&2\nread a\nprintf '%s\\n' '" + claudeInitLine("[]") + "'\nread b\necho late-p1 >&2\nexec cat\n"
	inits := &initCounter{}
	p := startClaude(t, id, script, inits.handle)

	if r := waitStderr(t, logs, id, "early-p1", 1, 5*time.Second)[0]; r.Level != "WARN" {
		t.Fatalf("stderr before first stdout: level = %s, want WARN", r.Level)
	}
	if err := p.SendMessage("go", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	inits.wait(t, 1)
	if err := p.SendMessage("go", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if r := waitStderr(t, logs, id, "late-p1", 1, 5*time.Second)[0]; r.Level != "DEBUG" {
		t.Fatalf("stderr after first stdout: level = %s, want DEBUG", r.Level)
	}

	p.Kill()
	if err := p.Start(); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if r := waitStderr(t, logs, id, "early-p1", 2, 5*time.Second)[1]; r.Level != "WARN" {
		t.Fatalf("second spawn's pre-stdout stderr: level = %s, want WARN", r.Level)
	}
}

// 25 non-empty lines per spawn, interleaved with blank lines, one ending in
// CRLF; the provider is started twice to show the cap resets per spawn.
func TestProviderStderr_WarnLimitPerSpawn(t *testing.T) {
	logs := captureDebugJSON(t)
	const id = "stderr-limit-1"
	script := "i=1\nwhile [ $i -le 23 ]; do echo \"line-$i\"; echo ''; i=$((i+1)); done >&2\n" +
		"printf 'crlf-line\\r\\n' >&2\necho done-p1 >&2\nexit 1\n"
	p := startClaude(t, id, script, func(string, json.RawMessage) {})
	for spawn := 1; spawn <= 2; spawn++ {
		if spawn == 2 {
			if err := p.Start(); err != nil {
				t.Fatalf("second Start: %v", err)
			}
		}
		waitStderr(t, logs, id, "done-p1", spawn, 5*time.Second)
		counts := map[string]int{}
		for _, r := range stderrRecords(logs, id) {
			counts[r.Msg+"/"+r.Level]++
			if r.Msg == stderrMsg && (r.Text == "" || strings.HasSuffix(r.Text, "\r")) {
				t.Fatalf("blank line or trailing CR logged: %+v", r)
			}
		}
		want := map[string]int{stderrMsg + "/WARN": 20 * spawn, stderrLimitMsg + "/WARN": spawn, stderrMsg + "/DEBUG": 5 * spawn}
		for k, n := range want {
			if counts[k] != n {
				t.Fatalf("after spawn %d: %s = %d, want %d (all: %v)", spawn, k, counts[k], n, counts)
			}
		}
	}
}

func TestProviderStderr_LongLineIsTruncatedAndDoesNotBlockChild(t *testing.T) {
	logs := captureDebugJSON(t)
	const id = "stderr-long-1"
	// Byte 1024 of the first line falls inside a two-byte rune; the second
	// line puts a key across the 1024-byte cut, so truncating before
	// redacting would leave a short, unredacted key prefix behind.
	fixture := filepath.Join(t.TempDir(), "stderr.txt")
	body := "a" + strings.Repeat("é", 200000) + "\n" + strings.Repeat("x", 1010) + " sk-" + strings.Repeat("A", 30) + "\n"
	if err := os.WriteFile(fixture, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{}, 1)
	startClaude(t, id, "cat '"+fixture+"' >&2\necho tail-p1 >&2\nexit 0\n", func(ev string, _ json.RawMessage) {
		if ev == "process_exited" {
			exited <- struct{}{}
		}
	})

	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("child blocked writing a long stderr line")
	}
	waitStderr(t, logs, id, "tail-p1", 1, 5*time.Second)
	long := waitStderr(t, logs, id, "aé", 1, time.Second)[0]
	if len(long.Text) > 1024 || strings.ContainsRune(long.Text, '�') || !long.Truncated {
		t.Fatalf("long line: %d bytes, split rune %v, truncated %v; want <= 1024, no split rune, truncated",
			len(long.Text), strings.ContainsRune(long.Text, '�'), long.Truncated)
	}
	if strings.Contains(logs.all(), "sk-AAAA") {
		t.Fatal("key prefix survived: line was truncated before it was redacted")
	}
}

func TestRedactStderrLine(t *testing.T) {
	rmk := "rmk_" + strings.Repeat("0a", 32)
	cases := []struct {
		name, in string
		secrets  []string
		gone     string
		keep     []string
	}{
		{"exact secret", "hello acme-identity-p1 bye", []string{"acme-identity-p1"}, "acme-identity-p1", []string{"hello", "bye"}},
		{"model key", "key " + rmk + " end", nil, rmk, []string{"key", "end"}},
		{"sk token", "using sk-Abc_def-0123456789xyz now", nil, "sk-Abc_def-0123456789xyz", []string{"using", "now"}},
		{"bearer", "Authorization: Bearer abc.DEF-123_xyz", nil, "abc.DEF-123_xyz", []string{"Authorization: Bearer "}},
		{"bearer any case", "got bearer abc.DEF-123_xyz", nil, "abc.DEF-123_xyz", []string{"got bearer "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactStderrLine(tc.in, tc.secrets)
			if strings.Contains(got, tc.gone) {
				t.Fatalf("redactStderrLine(%q) = %q, still holds %q", tc.in, got, tc.gone)
			}
			for _, k := range tc.keep {
				if !strings.Contains(got, k) {
					t.Fatalf("redactStderrLine(%q) = %q, lost %q", tc.in, got, k)
				}
			}
		})
	}
	for _, in := range []string{"sk-short", "rmk_" + strings.Repeat("a", 63), "plain line"} {
		if got := redactStderrLine(in, nil); got != in {
			t.Fatalf("redactStderrLine(%q) = %q, want unchanged", in, got)
		}
	}
}
