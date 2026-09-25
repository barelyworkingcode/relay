package permission

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/barelyworkingcode/relay/internal/sessions/types"
)

// PreflightInput is everything Preflight needs to decide a tool call without
// asking a person.
type PreflightInput struct {
	ToolName  string
	ToolInput string // raw JSON text of the tool's input
	Mode      string
	Directory string
	Policy    *types.PermissionPolicy
}

// Preflight decides a tool call from policy and mode alone. decided is false
// when a person has to answer. The rules are checked in order and the first
// match wins: a denied tool, bypassPermissions, a tool allowed by bare name,
// then an acceptEdits edit inside the session directory.
func Preflight(in PreflightInput) (d PermissionDecision, decided bool) {
	if in.Policy != nil && MatchToolRule(in.ToolName, in.ToolInput, in.Policy.DeniedTools) {
		return PermissionDecision{Decision: "deny", Reason: "denied by project policy"}, true
	}
	if in.Mode == "bypassPermissions" {
		return PermissionDecision{Decision: "allow", Reason: "bypassPermissions mode"}, true
	}
	if in.Policy != nil && allowedByName(in.ToolName, in.Policy.AllowedTools) {
		return PermissionDecision{Decision: "allow", Reason: "allowed by project policy"}, true
	}
	if in.Mode == "acceptEdits" && editsInsideDirectory(in.ToolName, in.ToolInput, in.Directory) {
		return PermissionDecision{Decision: "allow", Reason: "acceptEdits mode: edit inside the session directory"}, true
	}
	return PermissionDecision{}, false
}

// allowedByName reports whether patterns lists toolName as a bare name.
//
// Deliberate: Tool:arg allow entries never decide here. MatchToolRule matches
// the argument by substring on the serialized input, which cannot bound a
// chained shell command (Bash:"command":"git also matches
// "git status; curl … | sh"), so those calls fall through to a person.
// Deny rules keep the substring match, where broader is safer.
func allowedByName(toolName string, patterns []string) bool {
	for _, pat := range patterns {
		if !strings.Contains(pat, ":") && pat == toolName {
			return true
		}
	}
	return false
}

// editsInsideDirectory reports whether toolName is a file-editing tool whose
// target path lies strictly below dir. The check is lexical only; symlinks
// are not resolved.
func editsInsideDirectory(toolName, toolInput, dir string) bool {
	var key string
	switch toolName {
	case "Edit", "MultiEdit", "Write":
		key = "file_path"
	case "NotebookEdit":
		key = "notebook_path"
	default:
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(toolInput), &fields); err != nil {
		return false
	}
	var target string
	if err := json.Unmarshal(fields[key], &target); err != nil {
		return false
	}
	if !filepath.IsAbs(target) || !filepath.IsAbs(dir) {
		return false
	}
	target = filepath.Clean(target)
	dir = filepath.Clean(dir)
	// The separator suffix keeps a sibling like /work/acme2 from matching
	// /work/acme, and excludes dir itself.
	prefix := dir
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(target, prefix)
}
