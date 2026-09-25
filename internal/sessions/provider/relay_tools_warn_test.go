package provider

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const relayToolsWarnMsg = "relay tools requested but unavailable"

var optIn = json.RawMessage(`{"useRelayTools":true}`)

type warnBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *warnBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *warnBuffer) all() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *warnBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(b.buf.String(), "\n") {
		if strings.Contains(l, relayToolsWarnMsg) {
			out = append(out, l)
		}
	}
	return out
}

// captureWarns swaps the process-wide default logger, so no test in this
// file may run in parallel.
func captureWarns(t *testing.T) *warnBuffer {
	t.Helper()
	b := &warnBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return b
}

// requireOneWarn waits for exactly one relay-tools warning carrying every
// key=value in want.
func requireOneWarn(t *testing.T, b *warnBuffer, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(b.lines()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	lines := b.lines()
	if len(lines) != 1 {
		t.Fatalf("want exactly one %q warning, got %d; all warnings:\n%s", relayToolsWarnMsg, len(lines), b.all())
	}
	for _, w := range want {
		if !strings.Contains(lines[0], w) {
			t.Fatalf("warning lacks %s: %s", w, lines[0])
		}
	}
}

func writeFakeCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testHost(sshBin string) *sessionstypes.HostSpec {
	return &sessionstypes.HostSpec{ID: "h1", Name: "devbox", SSHArgv: []string{sshBin}, ClaudePath: "/opt/acme/bin/claude"}
}

func TestRelayToolsWarn_ChatCommandUnset(t *testing.T) {
	logs := captureWarns(t)
	NewChatProvider(&sessionstypes.Session{ID: "chat-w1", Settings: optIn}, func(string, json.RawMessage) {}, ChatConfig{})
	requireOneWarn(t, logs, "session=chat-w1", "kind=chat", "reason=relay_mcp_command_unset")
}

func TestRelayToolsWarn_ChatHostSession(t *testing.T) {
	logs := captureWarns(t)
	sess := &sessionstypes.Session{ID: "chat-w2", Settings: optIn, Host: testHost("/usr/bin/true")}
	NewChatProvider(sess, func(string, json.RawMessage) {}, ChatConfig{RelayMCPCommand: "/opt/acme/relay"})
	requireOneWarn(t, logs, "session=chat-w2", "kind=chat", "reason=host_session")
}

func TestRelayToolsWarn_ChatMCPStartFailed(t *testing.T) {
	logs := captureWarns(t)
	sock := filepath.Join(shortTempDir(t), "model.sock")
	fakeBroker(t, sock, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	p := NewChatProvider(&sessionstypes.Session{ID: "chat-w3", Settings: optIn}, func(string, json.RawMessage) {}, ChatConfig{
		ModelSocket:     sock,
		ModelKey:        "test-key",
		RelayMCPCommand: "/opt/acme/relay",
		ShimBinary:      filepath.Join(t.TempDir(), "no-such-relay-sessions"),
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Kill()
	requireOneWarn(t, logs, "session=chat-w3", "kind=chat", "reason=mcp_start_failed", "error=")
}

func TestRelayToolsWarn_ClaudeCommandUnset(t *testing.T) {
	logs := captureWarns(t)
	sess := &sessionstypes.Session{ID: "claude-w1", Model: "sonnet", Directory: t.TempDir(), Settings: optIn}
	p := NewClaudeProvider(sess, func(string, json.RawMessage) {}, ClaudeConfig{Binary: writeFakeCLI(t, "exec cat\n")}, nil)
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Kill()
	requireOneWarn(t, logs, "session=claude-w1", "kind=claude", "reason=relay_mcp_command_unset")
}

func TestRelayToolsWarn_ClaudeHostSession(t *testing.T) {
	logs := captureWarns(t)
	sess := &sessionstypes.Session{ID: "claude-w2", Model: "sonnet", Settings: optIn, Host: testHost(writeFakeCLI(t, "exec cat\n"))}
	p := NewClaudeProvider(sess, func(string, json.RawMessage) {}, ClaudeConfig{RelayMCPCommand: "/opt/acme/relay"}, nil)
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Kill()
	requireOneWarn(t, logs, "session=claude-w2", "kind=claude", "reason=host_session")
}

// initCounter counts system-init events the provider emits, so a no-warn
// assertion runs only after init handling has finished.
type initCounter struct {
	mu sync.Mutex
	n  int
}

func (c *initCounter) handle(_ string, data json.RawMessage) {
	if bytes.Contains(data, []byte(`"subtype":"init"`)) {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
}

func (c *initCounter) wait(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := c.n
		c.mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("provider emitted fewer than %d init events", want)
}

func claudeInitLine(servers string) string {
	return `{"type":"system","subtype":"init","session_id":"cli-w3","model":"sonnet","cwd":"/tmp","tools":[],"mcp_servers":` + servers + `}`
}

// startClaudeWithInit spawns a fake Claude that prints each init line, then
// waits until the provider has handled all of them.
func startClaudeWithInit(t *testing.T, id string, initLines ...string) {
	t.Helper()
	script := ""
	for _, l := range initLines {
		script += "printf '%s\\n' '" + l + "'\n"
	}
	cli := writeFakeCLI(t, script+"exec cat\n")
	sess := &sessionstypes.Session{ID: id, Model: "sonnet", Directory: t.TempDir(), Settings: optIn}
	inits := &initCounter{}
	p := NewClaudeProvider(sess, inits.handle, ClaudeConfig{Binary: cli, RelayMCPCommand: "/opt/acme/relay"}, nil)
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(p.Kill)
	inits.wait(t, len(initLines))
}

func TestRelayToolsWarn_ClaudeRelayServerFailedAtInit(t *testing.T) {
	cases := []struct {
		name    string
		servers string
		status  string // "" means no warning
	}{
		{"failed object", `[{"name":"relay","status":"failed","source":"dynamic"}]`, "failed"},
		{"pending object", `[{"name":"relay","status":"pending","source":"dynamic"}]`, ""},
		{"bare string", `["relay"]`, ""},
		{"only other servers", `[{"name":"acme","status":"connected"},"other"]`, "absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureWarns(t)
			startClaudeWithInit(t, "claude-w3", claudeInitLine(tc.servers))
			if tc.status == "" {
				time.Sleep(100 * time.Millisecond)
				if lines := logs.lines(); len(lines) != 0 {
					t.Fatalf("want no relay-tools warning, got %q", lines)
				}
				return
			}
			requireOneWarn(t, logs, "session=claude-w3", "kind=claude", "reason=relay_server_failed", "status="+tc.status)
		})
	}
}

func TestRelayToolsWarn_ClaudeRelayServerFailedOncePerSpawn(t *testing.T) {
	logs := captureWarns(t)
	failed := claudeInitLine(`[{"name":"relay","status":"failed","source":"dynamic"}]`)
	startClaudeWithInit(t, "claude-w4", failed, failed)
	requireOneWarn(t, logs, "session=claude-w4", "reason=relay_server_failed", "status=failed")
}

func TestRelayToolsWarn_SilentWhenNotRequested(t *testing.T) {
	for _, settings := range []json.RawMessage{nil, json.RawMessage(`{"useRelayTools":false}`)} {
		logs := captureWarns(t)
		NewChatProvider(&sessionstypes.Session{ID: "chat-quiet", Settings: settings}, func(string, json.RawMessage) {}, ChatConfig{})

		sess := &sessionstypes.Session{ID: "claude-quiet", Model: "sonnet", Directory: t.TempDir(), Settings: settings}
		p := NewClaudeProvider(sess, func(string, json.RawMessage) {}, ClaudeConfig{Binary: writeFakeCLI(t, "exec cat\n")}, nil)
		if err := p.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		hostSess := &sessionstypes.Session{ID: "claude-quiet-host", Model: "sonnet", Settings: settings, Host: testHost(writeFakeCLI(t, "exec cat\n"))}
		hp := NewClaudeProvider(hostSess, func(string, json.RawMessage) {}, ClaudeConfig{RelayMCPCommand: "/opt/acme/relay"}, nil)
		if err := hp.Start(); err != nil {
			t.Fatalf("host Start: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		p.Kill()
		hp.Kill()
		if lines := logs.lines(); len(lines) != 0 {
			t.Fatalf("settings %s: want no relay-tools warning, got %q", settings, lines)
		}
	}
}
