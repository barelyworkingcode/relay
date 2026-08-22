package main

// F1 and F2: an allowlist entry that matches by shape rather than by name.
//
// ADR-011 decision 2b refuses a wildcard in allowed_tools because registering
// a tool tomorrow must not widen a grant made today. That refusal was a
// literal compare against "*" while the matcher underneath was path.Match, and
// a tool name contains no "/" — so "**", "?*", "*_*", "[a-z]*" and "*e*" each
// match EVERY tool of an MCP and not one of them is the string "*". An
// adversarial review built a read-only "mail" profile with
// allowed_tools {"macmcp": ["**"]}, was served 26 tools across 11 of macMCP's
// domains, and exfiltrated through web_fetch — the outbound channel decision
// 2b claims to remove.
//
// The two halves are tested together on purpose. Refusing at the editor is
// F1; refusing at the matcher is F2, because any route that skips validation
// (a hand-edited settings.json, a restored backup, a migration written before
// the rule) must not be able to widen a grant either. A fix in one place only
// is a fix that holds until the next way into the file.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// everyToolPatternTheReviewFound is the reviewer's list verbatim, plus the
// bare "*" the old rule did catch. Every one of these matched all 47 of
// macMCP's tools through path.Match.
var everyToolPatternTheReviewFound = []string{
	"*",
	"**",
	"?*",
	"*_*",
	"[a-z]*",
	"*e*",
	// Not from the review: the next spellings, which is the point of fixing
	// the matcher rather than blacklisting the six above.
	"***",
	"?",
	"??????*",
	"[a-z]*_*",
	"*[a-z]*",
	`\*` + "*", // an escaped star followed by a real one: literal "*", matches nothing real
}

// The editor refuses every one of them, and the refusal names the pattern and
// the MCP so an operator knows which line to fix.
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
			// The create path is a second route to the same record and must
			// refuse identically — CreateProjectWithTokenKind runs shape only.
			if err := validateProjectShape(proj); err == nil {
				t.Fatalf("validateProjectShape accepted %q", pattern)
			}
		})
	}
}

// The other half of the rule: what an operator actually writes still works.
// A refusal that also refused "mail_*" would be a fix that removes the
// feature.
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

// F2. Validation is not the boundary: the matcher refuses an over-broad
// pattern wherever the record came from, and goes on honouring the real ones
// beside it.
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
			// Beside a real pattern the real one still decides. This is the
			// case a "refuse the whole list" fix would get wrong, and the
			// case a hand-edited file most plausibly holds.
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

// The measurement the reviewer took, through the router this time: a profile
// whose allowlist is "**" is served nothing at all, and web_fetch — the
// outbound channel — is refused by name.
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

	// And the profile the operator meant to write is unaffected: mail_* still
	// serves the mail tools and still holds nothing else.
	r := newProfileRouter(t, profileOpts{
		kind:         ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
		access:       map[string]string{"macmcp": AccessWrite},
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

// The rule itself, stated as the two questions it asks. This is the test that
// says what "too broad" MEANS, so a future edit to the probe list or the
// literal scanner has something to be wrong against.
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

// A context field's applies_to shares the matcher and must NOT share this
// rule: a field that governs everything is a restriction that applies to
// everything, which is the fail-closed reading there. Same pattern, opposite
// meaning, and conflating the two would quietly unscope every tool.
func TestOverBroadRuleDoesNotReachAppliesTo(t *testing.T) {
	f := ContextField{Name: "mail_accounts", Scope: ContextScopeRestrict, AppliesTo: []string{"*"}}
	for _, tool := range []string{"mail_search", "web_fetch", "capture_screenshot"} {
		if !f.Governs(tool) {
			t.Errorf(`applies_to ["*"] stopped governing %q`, tool)
		}
	}
}

// allowed_mcp_ids does NOT have allowed_tools' shape, and this test is what
// says so out loud.
//
// The review asked for the same rule there. It does not apply, because the
// list is not matched with path.Match: every consumer is
// `isWildcard(ids) || slices.Contains(ids, mcpID)`, an exact string compare
// with one special case for the single-entry "*". So "**" is not a wildcard
// there — it is an MCP id nothing is named, and it grants nothing, which is
// the fail-closed direction and needs no refusal.
//
// The reason to pin it is that the property is one edit away from being
// untrue: swap the Contains for a glob and every spelling F1 is about becomes
// live one layer up, with no test failing. This one fails.
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
	// The one special case is still the single "*", which validateProjectShape
	// refuses for a profile and keeps for a local project.
	local := &Project{ID: "p2", Name: "Local", Path: "/tmp/x", AllowedMcpIDs: []string{"*"}}
	if tok := (&Settings{ExternalMcps: mcps}).storedTokenForProject(local, "hash"); len(tok.Permissions) != 0 {
		t.Errorf(`a local project's ["*"] stopped meaning every MCP: %v`, tok.Permissions)
	}
	if err := validateProjectShape(&Project{Kind: ProjectKindRemote, AllowedMcpIDs: []string{"*"}}); err == nil {
		t.Error(`a profile was allowed allowed_mcp_ids: ["*"]`)
	}
}
