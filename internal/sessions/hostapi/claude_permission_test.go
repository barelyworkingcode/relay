package hostapi_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
)

// testClaudeRecord is one line of testclaude's $TESTCLAUDE_OUT.
type testClaudeRecord struct {
	Tool       string `json:"tool"`
	HookStdout string `json:"hookStdout"`
	HookExit   int    `json:"hookExit"`
	Decision   string `json:"decision"`
	Ran        bool   `json:"ran"`
}

func waitTestClaudeRecord(t *testing.T, path string, timeout time.Duration) testClaudeRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f, err := os.Open(path); err == nil {
			sc := bufio.NewScanner(f)
			var rec testClaudeRecord
			ok := sc.Scan() && json.Unmarshal(sc.Bytes(), &rec) == nil
			f.Close()
			if ok {
				return rec
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("testclaude wrote no record to %s within %s", path, timeout)
	return testClaudeRecord{}
}

// TestPermissionFlow_RealClaudeProvider runs a real ClaudeProvider over
// testclaude, which runs the real relay-sessions hook from the
// settings.local.json the provider wrote.
func TestPermissionFlow_RealClaudeProvider(t *testing.T) {
	relaySessions, _ := buildBinaries(t)
	testClaude, testMCP := buildTestClaude(t)
	t.Setenv("HOME", mkShortTempDir(t, "hostapi-home-"))

	for _, tc := range []struct {
		name         string
		hostUp       bool
		approve      bool
		wantDecision string
		wantRan      bool
	}{
		{"approve", true, true, "allow", true},
		{"deny", true, false, "deny", false},
		{"host_unreachable", false, false, "deny", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostDir := mkShortTempDir(t, "hostapi-claude-")
			hookSock := filepath.Join(hostDir, "hook.sock")
			providerHookSock := hookSock
			if !tc.hostUp {
				providerHookSock = filepath.Join(hostDir, "nobody-listens.sock")
			}
			out := filepath.Join(hostDir, "testclaude.jsonl")
			t.Setenv("TESTCLAUDE_OUT", out)

			perms := permission.NewPermissionManager()
			terminals := terminal.NewManager(terminal.Config{ShimBinary: relaySessions, LogDir: filepath.Join(hostDir, "terminal_logs")})
			sessions := session.NewManager(session.Config{Claude: provider.ClaudeConfig{
				Binary:          testClaude,
				HookSocket:      providerHookSock,
				HookCommandPath: relaySessions,
				RelayMCPCommand: testMCP,
			}}, session.NewStore(filepath.Join(hostDir, "sessions")), perms)
			h := &flowHost{
				mountedHost: startMountedHost(t, os.Getpid(), hookSock, terminals, sessions, perms),
				sessions:    sessions,
			}

			const id = "bbbbbbbb-0000-0000-0000-000000000001"
			if _, err := sessions.Create(session.CreateSpec{
				SessionID: id, ProjectID: "p1", Kind: session.KindClaude, Directory: mkShortTempDir(t, "hostapi-proj-"),
				Model: "sonnet", Settings: json.RawMessage(`{"useRelayTools":true}`),
			}); err != nil {
				t.Fatalf("create claude session: %v", err)
			}
			t.Cleanup(sessions.StopAll)

			if tc.hostUp {
				conn := h.joinViewer(t, id)
				go func() { _ = sessions.SendMessage(id, "use the tool", nil) }()
				frame, ok := readFrame(conn, "permission_request", 20*time.Second)
				if !ok {
					t.Fatal("no permission_request frame reached the viewer")
				}
				if frame["toolName"] != "mcp__relay__testmcp_ping" {
					t.Fatalf("toolName = %v, want mcp__relay__testmcp_ping", frame["toolName"])
				}
				if err := conn.WriteJSON(map[string]any{"type": "permission_response", "permissionId": frame["permissionId"], "approved": tc.approve}); err != nil {
					t.Fatalf("write permission_response: %v", err)
				}
			} else {
				go func() { _ = sessions.SendMessage(id, "use the tool", nil) }()
			}

			rec := waitTestClaudeRecord(t, out, 20*time.Second)
			var hookOut struct {
				HookSpecificOutput struct {
					PermissionDecision string `json:"permissionDecision"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(rec.HookStdout), &hookOut); err != nil || rec.HookExit != 0 {
				t.Fatalf("hook exit %d stdout %q, want exit 0 with one decision line", rec.HookExit, rec.HookStdout)
			}
			if got := hookOut.HookSpecificOutput.PermissionDecision; got != tc.wantDecision {
				t.Fatalf("hook decision = %q, want %q (record %+v)", got, tc.wantDecision, rec)
			}
			if rec.Decision != tc.wantDecision || rec.Ran != tc.wantRan {
				t.Fatalf("record = %+v, want decision %q ran %v", rec, tc.wantDecision, tc.wantRan)
			}
		})
	}
}
