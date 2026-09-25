package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const testRelayBin = "/opt/acme/Relay.app/Contents/MacOS/relay"

func serviceArgs(extra ...string) []string {
	return append([]string{"-internal-socket", "/tmp/i.sock", "-hook-socket", "/tmp/h.sock"}, extra...)
}

func TestParseServiceArgs_RelayMCPCommand(t *testing.T) {
	cases := []struct {
		name    string
		extra   []string
		want    string
		wantErr string
	}{
		{name: "set", extra: []string{"-relay-mcp-command", testRelayBin}, want: testRelayBin},
		{name: "relative", extra: []string{"-relay-mcp-command", "bin/relay"}, wantErr: `-relay-mcp-command "bin/relay" is not an absolute path`},
		{name: "absent", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseServiceArgs(serviceArgs(tc.extra...))
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseServiceArgs: %v", err)
			}
			if got := sessionConfig(cfg, "/opt/acme/relay-sessions").Chat.RelayMCPCommand; got != tc.want {
				t.Fatalf("RelayMCPCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSessionConfig_RelayMCPCommandReachesClaudeAndChat(t *testing.T) {
	cfg, err := parseServiceArgs(serviceArgs("-relay-mcp-command", testRelayBin))
	if err != nil {
		t.Fatalf("parseServiceArgs: %v", err)
	}
	sc := sessionConfig(cfg, "/opt/acme/relay-sessions")
	if sc.Claude.RelayMCPCommand != testRelayBin {
		t.Errorf("Claude.RelayMCPCommand = %q, want %q", sc.Claude.RelayMCPCommand, testRelayBin)
	}
	if sc.Chat.RelayMCPCommand != testRelayBin {
		t.Errorf("Chat.RelayMCPCommand = %q, want %q", sc.Chat.RelayMCPCommand, testRelayBin)
	}
}

// startFakeClaude installs a stand-in claude at $HOME/.local/bin/claude,
// the first place the provider looks, under a temp HOME. It records its argv
// and a copy of any --mcp-config file, then waits on stdin.
func startFakeClaude(t *testing.T, settings json.RawMessage) (argvOut, mcpOut string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	scratch := t.TempDir()
	argvOut = filepath.Join(scratch, "argv")
	mcpOut = filepath.Join(scratch, "mcp.json")

	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"prev=\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = --mcp-config ]; then cp \"$a\" '" + mcpOut + ".tmp'; mv '" + mcpOut + ".tmp' '" + mcpOut + "'; fi\n" +
		"  printf '%s\\n' \"$a\" >> '" + argvOut + ".tmp'\n" +
		"  prev=$a\n" +
		"done\n" +
		"mv '" + argvOut + ".tmp' '" + argvOut + "'\n" +
		"exec cat\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := parseServiceArgs(serviceArgs("-relay-mcp-command", testRelayBin))
	if err != nil {
		t.Fatalf("parseServiceArgs: %v", err)
	}
	sess := &sessionstypes.Session{ID: "acme-claude-1", Model: "sonnet", Directory: t.TempDir(), Settings: settings}
	p := provider.NewClaudeProvider(sess, func(string, json.RawMessage) {}, sessionConfig(cfg, "/opt/acme/relay-sessions").Claude, nil)
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(p.Kill)
	waitForPath(t, argvOut)
	return argvOut, mcpOut
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestSessionConfig_ClaudeMCPConfigCarriesRelayServer(t *testing.T) {
	_, mcpOut := startFakeClaude(t, json.RawMessage(`{"useRelayTools":true}`))
	waitForPath(t, mcpOut)

	data, err := os.ReadFile(mcpOut)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		McpServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode --mcp-config %s: %v", data, err)
	}
	relay, ok := got.McpServers["relay"]
	if !ok {
		t.Fatalf("--mcp-config has no relay server: %s", data)
	}
	if relay.Command != testRelayBin || len(relay.Args) != 1 || relay.Args[0] != "mcp" || relay.Env != nil {
		t.Fatalf("relay server = %+v, want {%s [mcp]} with no env", relay, testRelayBin)
	}
}

func TestSessionConfig_ClaudeWithoutUseRelayToolsGetsNoMCPConfig(t *testing.T) {
	argvOut, _ := startFakeClaude(t, json.RawMessage(`{"useRelayTools":false}`))

	argv, err := os.ReadFile(argvOut)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "--mcp-config") {
		t.Fatalf("claude argv carries --mcp-config for an opted-out session:\n%s", argv)
	}
}
