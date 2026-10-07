package provider

import (
	"encoding/json"
	"slices"
	"testing"

	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func TestBuildClaudeArgs_ReadOnlyProjects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		wantOn   bool
	}{
		{"on", `{"readOnlyProjects":true}`, true},
		{"on with relay tools", `{"readOnlyProjects":true,"useRelayTools":true}`, true},
		{"off", `{"readOnlyProjects":false}`, false},
		{"absent", `{"useRelayTools":true}`, false},
		{"no settings", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &sessionstypes.Session{Model: "haiku"}
			if tc.settings != "" {
				s.Settings = json.RawMessage(tc.settings)
			}
			args := newTestClaudeProvider(s, ClaudeConfig{}).buildClaudeArgs("/tmp/mcp-config.json", "")

			at := slices.Index(args, "--tools")
			if tc.wantOn {
				if at < 0 || at+1 >= len(args) || args[at+1] != "Read,Grep,Glob" {
					t.Errorf("argv lacks --tools Read,Grep,Glob: %v", args)
				}
				if !slices.Contains(args, "--strict-mcp-config") {
					t.Errorf("argv lacks --strict-mcp-config: %v", args)
				}
			} else {
				if at >= 0 || slices.Contains(args, "--strict-mcp-config") {
					t.Errorf("argv carries read-only flags it was not asked for: %v", args)
				}
			}
			mcp := slices.Index(args, "--mcp-config")
			if mcp < 0 || args[mcp+1] != "/tmp/mcp-config.json" {
				t.Errorf("--mcp-config dropped: %v", args)
			}
		})
	}
}
