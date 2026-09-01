package project

// Subtle: path.Match has no "/" to anchor on in a tool name, so "**", "?*",
// "*_*", "[a-z]*", and "*e*" each match every tool despite not being the
// literal string "*" that ADR-011 decision 2b refuses.
//
// Deliberate: tested at both the editor (validation) and the matcher
// (ToolAllowed) — a route that skips validation (a hand-edited settings.json,
// a restored backup, an old migration) must not be able to widen a grant
// either.

import (
	"github.com/barelyworkingcode/relay/internal/config"
	"strings"
	"testing"
)

// Each of these matches every one of macMCP's tools via path.Match, despite
// not being the literal "*" string decision 2b refuses.
var everyToolPatternTheReviewFound = []string{
	"*",
	"**",
	"?*",
	"*_*",
	"[a-z]*",
	"*e*",
	// The fix is the matcher, not a blacklist of the spellings above.
	"***",
	"?",
	"??????*",
	"[a-z]*_*",
	"*[a-z]*",
	`\*` + "*", // an escaped star followed by a real one: literal "*", matches nothing real
}

func TestAllowedTools_ValidationRefusesEveryOverBroadSpelling(t *testing.T) {
	for _, pattern := range everyToolPatternTheReviewFound {
		if pattern == `\**` {
			continue // literal "*" — narrow, not broad; asserted below
		}
		t.Run(pattern, func(t *testing.T) {
			proj := &config.Project{ID: "p1", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
				AllowedTools: map[string][]string{"macmcp": {pattern}}}
			err := validateProjectPermissions(proj, v2Surfaces())
			if err == nil {
				t.Fatalf("pattern %q was accepted into an allowlist", pattern)
			}
			for _, want := range []string{pattern, "macmcp", "allowed_tools"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal should say %q; got: %v", want, err)
				}
			}
			// Second route to the same record: CreateWithTokenKind runs
			// shape validation only.
			if err := ValidateShape(proj); err == nil {
				t.Fatalf("ValidateShape accepted %q", pattern)
			}
		})
	}
}

func TestAllowedTools_ValidationKeepsNamePatterns(t *testing.T) {
	for _, pattern := range []string{
		"mail_*",
		"mail_search",
		"capture_screen*",
		"mail_get_*",
		"messages_?end",
		`\**`, // a literal asterisk: a name, not a wildcard
		"[cm]ail_*",
	} {
		t.Run(pattern, func(t *testing.T) {
			proj := &config.Project{ID: "p1", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
				AllowedTools: map[string][]string{"macmcp": {pattern}}}
			if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
				t.Fatalf("pattern %q was refused: %v", pattern, err)
			}
			if err := ValidateShape(proj); err != nil {
				t.Fatalf("ValidateShape refused %q: %v", pattern, err)
			}
		})
	}
}

func TestOverBroadToolPattern_TheRule(t *testing.T) {
	cases := []struct {
		pattern string
		over    bool
		why     string
	}{
		{"*", true, "no literal content"},
		{"**", true, "no literal content"},
		{"?*", true, "no literal content"},
		{"[a-z]*", true, "a class is not a literal"},
		{"*_*", true, "an underscore is in every identifier"},
		{"*e*", true, "a letter is in every identifier"},
		{"mail_*", false, "a real prefix"},
		{"mail_search", false, "an exact name"},
		{"*_search", false, "a real suffix"},
		{"capture_screen*", false, "a real prefix"},
	}
	for _, tc := range cases {
		_, over := overBroadToolPattern(tc.pattern)
		if over != tc.over {
			t.Errorf("overBroadToolPattern(%q) = %v, want %v (%s)", tc.pattern, over, tc.over, tc.why)
		}
	}

	// The literal scanner underneath, which is where the first question is
	// answered. A class and a wildcard contribute nothing; an escape does.
	for _, tc := range []struct{ pattern, literal string }{
		{"*", ""},
		{"?*?", ""},
		{"[a-z][0-9]*", ""},
		{"[]a]*", ""},
		{"[^a]*", ""},
		{"mail_*", "mail_"},
		{`\*`, "*"},
		{`\[abc`, "[abc"},
		{"a[b-c]d", "ad"},
	} {
		if got := toolPatternLiteral(tc.pattern); got != tc.literal {
			t.Errorf("toolPatternLiteral(%q) = %q, want %q", tc.pattern, got, tc.literal)
		}
	}
}

// Deliberate: applies_to shares the matcher but not this rule — "*" there
// means "restriction applies everywhere" (fail-closed), the opposite of
// over-broad. Conflating the two would quietly unscope every tool.
func TestOverBroadRuleDoesNotReachAppliesTo(t *testing.T) {
	f := ContextField{Name: "mail_accounts", Scope: ContextScopeRestrict, AppliesTo: []string{"*"}}
	for _, tool := range []string{"mail_search", "web_fetch", "capture_screenshot"} {
		if !f.Governs(tool) {
			t.Errorf(`applies_to ["*"] stopped governing %q`, tool)
		}
	}
}

// Subtle: unlike allowed_tools, allowed_mcp_ids is matched literally
// (isWildcard(ids) || slices.Contains(ids, mcpID)), not via path.Match — "**"
// here is just an unmatched id, which grants nothing and needs no refusal.
//
// Deliberate: pinned because the property is one edit away from being
// untrue — swap Contains for a glob and this becomes live silently.
func TestAllowedMcpIDs_AreMatchedLiterallyAndNotAsGlobs(t *testing.T) {
	mcps := []config.ExternalMcp{{ID: "macmcp"}, {ID: "fsmcp"}}
	for _, pattern := range []string{"**", "?*", "*_*", "mac*", "[a-z]*"} {
		s := &config.Settings{ExternalMcps: mcps}
		proj := &config.Project{ID: "p1", Name: "Profile", Kind: config.ProjectKindRemote,
			AllowedMcpIDs: []string{pattern}}
		tok := config.StoredTokenForProject(s, proj, "hash")
		for _, id := range []string{"macmcp", "fsmcp"} {
			if tok.Permissions[id] != config.PermOff {
				t.Errorf("allowed_mcp_ids [%q] granted %q — the list is being matched as a pattern", pattern, id)
			}
		}
	}
	// The single "*" is still special: refused for a profile, kept for local.
	local := &config.Project{ID: "p2", Name: "Local", Path: "/tmp/x", AllowedMcpIDs: []string{"*"}}
	if tok := config.StoredTokenForProject(&config.Settings{ExternalMcps: mcps}, local, "hash"); len(tok.Permissions) != 0 {
		t.Errorf(`a local project's ["*"] stopped meaning every MCP: %v`, tok.Permissions)
	}
	if err := ValidateShape(&config.Project{Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"*"}}); err == nil {
		t.Error(`a profile was allowed allowed_mcp_ids: ["*"]`)
	}
}
