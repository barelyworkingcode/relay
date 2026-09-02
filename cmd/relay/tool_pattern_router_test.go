package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

// everyToolPatternTheReviewFound: each of these matches every one of macMCP's
// tools via path.Match, despite not being the literal "*" string ADR-011
// decision 2b refuses. The same list is pinned beside the validation rule in
// internal/project; these two tests are the ENFORCEMENT half, which needs a
// live router.
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

func TestAllowedTools_MatcherRefusesEveryOverBroadSpelling(t *testing.T) {
	surface := macmcpToolSurface()
	for _, pattern := range everyToolPatternTheReviewFound {
		if pattern == `\**` {
			continue
		}
		t.Run(pattern, func(t *testing.T) {
			tok := &config.StoredToken{ProjectKind: config.ProjectKindRemote,
				AllowedTools: map[string][]string{"macmcp": {pattern}},
				Access:       map[string]string{"macmcp": config.AccessWrite}}
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
				kind:         config.ProjectKindRemote,
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
		kind:         config.ProjectKindRemote,
		allowedTools: map[string][]string{"macmcp": {"mail_*"}},
		access:       map[string]string{"macmcp": config.AccessWrite},
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
