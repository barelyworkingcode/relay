package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// Tool search off (no path, mode off, invalid file) must put exactly the same
// bytes on the wire as a build without the feature: the full tool list in
// catalogue order and the session's own system prompt.
func TestToolSearch_OffRequestsAreByteIdentical(t *testing.T) {
	proj := writeProject(t, mainSkills())
	configs := map[string]string{
		"no config path":  "",
		"mode off":        chatJSON(t, `{"toolSearch":{"mode":"off"}}`),
		"invalid config":  chatJSON(t, `{"toolSearch":{"mode":"nope"}}`),
		"other keys only": chatJSON(t, `{"theme":"dark"}`), // auto, far below threshold for these small tools
	}
	var base []byte
	for name, path := range configs {
		env := newTSEnv(t, "")
		run := env.start(t, newSess("off1", proj), path, testutil.NewFakeMCPClient(mainTools()...))
		run.send(t)
		body := env.requests()[0]
		if base == nil {
			base = body
		}
		if string(body) != string(base) {
			t.Errorf("%s: request differs from the others:\n%s\n---\n%s", name, body, base)
		}
		if string(run.p.GetState()) != `{}` {
			t.Errorf("%s: GetState = %s, want {}", name, run.p.GetState())
		}
	}
	req := decodeReq(t, base)
	want := []string{"mail_send", "mail_read", "tides_lookup", "clock_now"}
	if got := toolNames(t, req); !reflect.DeepEqual(got, want) {
		t.Fatalf("tools = %v, want all %v in catalogue order", got, want)
	}
	if req.system() != "Be brief." {
		t.Fatalf("system = %q, want the bare session prompt", req.system())
	}
}

// claude and pi never read chat.json: with an `on` file in the data dir,
// their argv, env and MCP config are the same as without it.
func TestToolSearch_ClaudeAndPiIgnoreChatJSON(t *testing.T) {
	dataDir := t.TempDir()
	settings := json.RawMessage(`{"useRelayTools":true}`)
	build := func() (claudeArgs, claudeEnv []string, mcp map[string]any, piArgs []string) {
		cp := newTestClaudeProvider(&sessionstypes.Session{ID: "u1", Model: "sonnet", Settings: settings}, ClaudeConfig{
			HookSocket: "/tmp/h.sock", HookCommandPath: "/opt/acme/relay-sessions",
			BridgeSocket: "/tmp/b.sock", ModelSocket: "/tmp/m.sock", RelayMCPCommand: "/opt/acme/relay",
		})
		pp := newTestPiProvider(&sessionstypes.Session{ID: "u2", Model: "claude-sonnet-4"}, PiConfig{
			DataDir: dataDir, BridgeSocket: "/tmp/b.sock", ModelSocket: "/tmp/m.sock",
		})
		env := cp.buildClaudeEnv([]string{"PATH=/usr/bin"})
		sort.Strings(env) // order comes from a map
		return cp.buildClaudeArgs("/tmp/mcp.json", "/tmp/sys.txt"), env,
			cp.relayMCPConfig(), pp.buildPiArgs("/tmp/sessdir", "", "")
	}
	a1, e1, m1, p1 := build()
	if err := os.WriteFile(filepath.Join(dataDir, "chat.json"), []byte(onCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a2, e2, m2, p2 := build()
	if !reflect.DeepEqual(a1, a2) || !reflect.DeepEqual(e1, e2) || !reflect.DeepEqual(m1, m2) || !reflect.DeepEqual(p1, p2) {
		t.Fatalf("claude/pi spawn inputs changed with chat.json present:\nclaude args %v -> %v\nenv %v -> %v\nmcp %v -> %v\npi %v -> %v",
			a1, a2, e1, e2, m1, m2, p1, p2)
	}
	if m1 == nil {
		t.Fatal("fixture error: claude relay MCP config unexpectedly absent")
	}
}

func TestProviderSettings_NoToolSearchField(t *testing.T) {
	ps := ProviderSettings()
	if len(ps["claude"]) != 0 {
		t.Fatalf("claude settings = %+v, want none", ps["claude"])
	}
	if len(ps["pi"]) != 1 || ps["pi"][0].Key != "thinkingLevel" {
		t.Fatalf("pi settings = %+v, want only thinkingLevel", ps["pi"])
	}
	if len(ps["chat"]) != 1 || ps["chat"][0].Key != "useRelayTools" {
		t.Fatalf("chat settings = %+v, want only useRelayTools", ps["chat"])
	}
}
