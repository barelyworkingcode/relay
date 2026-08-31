package main

// Subtle: path.Match has no "/" to anchor on in a tool name, so "**", "?*",
// "*_*", "[a-z]*", and "*e*" each match every tool despite not being the
// literal string "*" that ADR-011 decision 2b refuses.
//
// Deliberate: tested at both the editor (validation) and the matcher
// (ToolAllowed) — a route that skips validation (a hand-edited settings.json,
// a restored backup, an old migration) must not be able to widen a grant
// either.

import (
	"context"
	"encoding/json"
	"slices"
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
			proj := &Project{ID: "p1", Kind: ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
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
			// Second route to the same record: CreateProjectWithTokenKind runs
			// shape validation only.
			if err := validateProjectShape(proj); err == nil {
				t.Fatalf("validateProjectShape accepted %q", pattern)
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
			proj := &Project{ID: "p1", Kind: ProjectKindRemote, AllowedMcpIDs: []string{"macmcp"},
				AllowedTools: map[string][]string{"macmcp": {pattern}}}
			if err := validateProjectPermissions(proj, v2Surfaces()); err != nil {
				t.Fatalf("pattern %q was refused: %v", pattern, err)
			}
			if err := validateProjectShape(proj); err != nil {
				t.Fatalf("validateProjectShape refused %q: %v", pattern, err)
			}
		})
	}
}

func TestAllowedTools_MatcherRefusesEveryOverBroadSpelling(t *testing.T) {
	surface := macmcpToolSurface()
	for _, pattern := range everyToolPatternTheReviewFound {
		if pattern == `\**` {
			continue
		}
		t.Run(pattern, func(t *testing.T) {
			tok := &StoredToken{ProjectKind: ProjectKindRemote,
				AllowedTools: map[string][]string{"macmcp": {pattern}},
				Access:       map[string]string{"macmcp": AccessWrite}}
			for _, tool := range surface {
				if tok.ToolAllowed("macmcp", tool.Name) {
					t.Errorf("pattern %q admitted %q", pattern, tool.Name)
				}
			}
			// A naive "refuse the whole list" fix would wrongly block mail_*
			// too — the real pattern must still decide beside the bad one.
			tok.AllowedTools["macmcp"] = []string{"mail_*", pattern}
			if !tok.ToolAllowed("macmcp", "mail_search") {
				t.Errorf(`"mail_*" stopped admitting mail_search beside %q`, pattern)
			}
			for _, forbidden := range []string{"web_fetch", "capture_screenshot", "shortcuts_run", "xmail_send"} {
				if tok.ToolAllowed("macmcp", forbidden) {
					t.Errorf("pattern %q admitted %q beside \"mail_*\"", pattern, forbidden)
				}
			}
		})
	}
}

func TestListTools_AnOverBroadAllowlistIsNotTheWholeMcp(t *testing.T) {
	for _, pattern := range []string{"**", "*_*", "[a-z]*", "*e*"} {
		t.Run(pattern, func(t *testing.T) {
			r := newProfileRouter(t, profileOpts{
				kind:         ProjectKindRemote,
				allowedTools: map[string][]string{"macmcp": {pattern}},
			})
			if got := listedToolNames(t, r); len(got) != 0 {
				t.Fatalf("allowed_tools [%q] listed %v", pattern, got)
			}
			for _, tool := range []string{"web_fetch", "capture_screenshot", "mail_search"} {
				if _, err := r.CallTool(context.Background(), tool, json.RawMessage(`{}`), testToken); err == nil {
					t.Errorf("allowed_tools [%q] called %q", pattern, tool)
				}
			}
		})
	}

	r := newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
		access:       map[string]string{"macmcp": AccessWrite},
		// mail_send needs the outbound grant too (decision 2c) — given here
		// so the test isolates the pattern, not the grant.
		allowExternal: map[string]bool{"macmcp": true},
	})
	got := listedToolNames(t, r)
	if !slices.Contains(got, "mail_search") || !slices.Contains(got, "mail_send") {
		t.Fatalf(`"mail_*" listed %v, want the mail tools`, got)
	}
	if slices.Contains(got, "web_fetch") {
		t.Error(`"mail_*" listed web_fetch`)
	}
	if _, err := r.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), testToken); err != nil {
		t.Fatalf(`"mail_*" could not call mail_search: %v`, err)
	}
}

// Defines what "too broad" means — a future edit to the probe list or literal
// scanner has this to be wrong against.
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
	mcps := []ExternalMcp{{ID: "macmcp"}, {ID: "fsmcp"}}
	for _, pattern := range []string{"**", "?*", "*_*", "mac*", "[a-z]*"} {
		s := &Settings{ExternalMcps: mcps}
		proj := &Project{ID: "p1", Name: "Profile", Kind: ProjectKindRemote,
			AllowedMcpIDs: []string{pattern}}
		tok := s.storedTokenForProject(proj, "hash")
		for _, id := range []string{"macmcp", "fsmcp"} {
			if tok.Permissions[id] != PermOff {
				t.Errorf("allowed_mcp_ids [%q] granted %q — the list is being matched as a pattern", pattern, id)
			}
		}
	}
	// The single "*" is still special: refused for a profile, kept for local.
	local := &Project{ID: "p2", Name: "Local", Path: "/tmp/x", AllowedMcpIDs: []string{"*"}}
	if tok := (&Settings{ExternalMcps: mcps}).storedTokenForProject(local, "hash"); len(tok.Permissions) != 0 {
		t.Errorf(`a local project's ["*"] stopped meaning every MCP: %v`, tok.Permissions)
	}
	if err := validateProjectShape(&Project{Kind: ProjectKindRemote, AllowedMcpIDs: []string{"*"}}); err == nil {
		t.Error(`a profile was allowed allowed_mcp_ids: ["*"]`)
	}
}
