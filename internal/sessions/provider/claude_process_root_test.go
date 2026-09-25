package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// writePIDReportScript writes a stand-in claude that records "$$ $PPID" to
// pidFile, then stays alive until its stdin closes.
func writePIDReportScript(t *testing.T, dir, pidFile string) string {
	t.Helper()
	path := filepath.Join(dir, "pidreport.sh")
	body := "#!/bin/sh\n" +
		"echo \"$$ $PPID\" > \"" + pidFile + ".tmp\"\n" +
		"mv \"" + pidFile + ".tmp\" \"" + pidFile + "\"\n" +
		"exec cat > /dev/null\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write pid report script: %v", err)
	}
	return path
}

func readSelfAndParent(t *testing.T, path string) (self, parent int) {
	t.Helper()
	waitForFile(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d %d", &self, &parent); err != nil {
		t.Fatalf("parse %s (%q): %v", path, data, err)
	}
	return self, parent
}

func TestClaudeProvider_ProcessRoot_LocalSpawn(t *testing.T) {
	cases := []struct {
		name   string
		config func(t *testing.T, script string) ClaudeConfig
		// wantRoot picks the expected root pid from the stand-in's own pid and its parent's.
		wantRoot func(self, parent int) int
	}{
		{"direct spawn is claude itself",
			func(_ *testing.T, script string) ClaudeConfig { return ClaudeConfig{Binary: script} },
			func(self, _ int) int { return self }},
		{"shim spawn is the shim",
			func(t *testing.T, script string) ClaudeConfig {
				bridgeSock, _ := startFakeBridge(t)
				return ClaudeConfig{
					Binary:       script,
					ShimBinary:   buildRelaySessionsBinary(t),
					BridgeSocket: bridgeSock,
					Identity:     &sessionsmcp.IdentitySpec{Secret: strings.Repeat("4", 64)},
				}
			},
			func(_, parent int) int { return parent }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scratch := shortTempDir(t)
			pidFile := filepath.Join(scratch, "pids")
			script := writePIDReportScript(t, scratch, pidFile)

			session := &sessionstypes.Session{ID: "claude-root-1", Model: "sonnet", Directory: scratch}
			p := NewClaudeProvider(session, func(string, json.RawMessage) {}, tc.config(t, script), nil)
			defer p.Kill()
			if err := p.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			self, parent := readSelfAndParent(t, pidFile)
			wantPID := tc.wantRoot(self, parent)

			root, ok := p.ProcessRoot()
			if !ok {
				t.Fatal("ProcessRoot: no root for a live local spawn")
			}
			want, ok := testutil.ProcessRootOf(wantPID)
			if !ok {
				t.Fatalf("ProcessRootOf(%d): process not readable", wantPID)
			}
			if root != want {
				t.Fatalf("ProcessRoot = %+v, want %+v (claude pid %d, its parent %d)", root, want, self, parent)
			}

			p.Kill()
			if root, ok := p.ProcessRoot(); ok {
				t.Fatalf("ProcessRoot after Kill = %+v, want no root", root)
			}
		})
	}
}

func TestClaudeProvider_ProcessRoot_SSHHostHasNone(t *testing.T) {
	scratch := shortTempDir(t)
	pidFile := filepath.Join(scratch, "pids")
	fakeSSH := writePIDReportScript(t, scratch, pidFile)

	session := &sessionstypes.Session{
		ID:    "claude-root-host-1",
		Model: "sonnet",
		Host: &sessionstypes.HostSpec{
			ID:         "host-1",
			Name:       "devbox",
			SSHArgv:    []string{fakeSSH},
			ClaudePath: "/remote/bin/claude",
		},
	}
	p := NewClaudeProvider(session, func(string, json.RawMessage) {}, ClaudeConfig{}, nil)
	defer p.Kill()
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	readSelfAndParent(t, pidFile)

	if root, ok := p.ProcessRoot(); ok {
		t.Fatalf("ProcessRoot = %+v on an SSH session, want no root", root)
	}
}
