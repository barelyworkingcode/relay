package toolsearch

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func tool(name, desc string) Tool {
	return Tool{Name: name, Description: desc, Parameters: json.RawMessage(`{"type":"object"}`)}
}

func firstNames(ms []Match) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Tool.Name)
	}
	return out
}

func fixture() *Catalog {
	skills := []Skill{
		{Dir: "relay-mail", Name: "relay-mail", Description: "Send and read messages", Keywords: []string{"inbox", "correspondence"}, Tools: []string{"mail_send", "mail_read"}},
		{Dir: "relay-tides", Name: "relay-tides", Description: "Zeppelin harbour schedules", Keywords: []string{"tidal"}, Tools: []string{"tides_lookup"}},
		{Dir: "relay-notes", Name: "relay-notes", Description: "Personal notebook", Tools: []string{"note_add"}},
	}
	tools := []Tool{
		tool("mail_send", "Deliver a message"),
		tool("mail_read", "Fetch a message"),
		tool("tides_lookup", "Fetch ledger values"),
		tool("note_add", "Append an entry"),
		tool("unrelated", "Not in any skill"),
	}
	return NewCatalog(skills, tools)
}

func TestSearch_RanksBySkillNameKeywordsAndDescriptions(t *testing.T) {
	c := fixture()
	cases := []struct{ name, query, wantFirst string }{
		{"skill name only", "notes", "note_add"},
		{"keyword only", "correspondence", "mail_read"},
		{"tool name only", "lookup", "tides_lookup"},
		{"skill description only", "zeppelin", "tides_lookup"},
		{"tool description only", "ledger", "tides_lookup"},
		{"prefix, both 4+ chars", "notebooks", "note_add"},
		{"case and punctuation", "INBOX!!", "mail_read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.Search(tc.query, nil, 5)
			if len(got) == 0 || got[0].Tool.Name != tc.wantFirst {
				t.Fatalf("Search(%q) = %v, want %s first", tc.query, firstNames(got), tc.wantFirst)
			}
			if got[0].Score <= 0 {
				t.Fatalf("score = %d, want > 0", got[0].Score)
			}
		})
	}
	if m := c.Search("tidal", nil, 5); len(m) == 0 || m[0].Skill != "relay-tides" {
		t.Fatalf("Match.Skill = %+v, want relay-tides", m)
	}
}

func TestSearch_WeightsKeywordAboveDescription(t *testing.T) {
	c := NewCatalog([]Skill{
		{Dir: "a", Name: "a", Description: "harbour", Tools: []string{"t_desc"}},
		{Dir: "b", Name: "b", Keywords: []string{"harbour"}, Tools: []string{"t_kw"}},
	}, []Tool{tool("t_desc", "x"), tool("t_kw", "x")})
	got := c.Search("harbour", nil, 5)
	// Keyword (weight 3) beats description (weight 1) even though the
	// description skill comes first in directory order.
	if len(got) != 2 || got[0].Tool.Name != "t_kw" {
		t.Fatalf("got %v, want t_kw first", firstNames(got))
	}
}

func TestSearch_NoMatchStopwordsAndShortTokens(t *testing.T) {
	c := fixture()
	for _, q := range []string{"", "   ", "qqqqq", "the a of to", "please use the tool for me", "x y z"} {
		if got := c.Search(q, nil, 5); len(got) != 0 {
			t.Errorf("Search(%q) = %v, want none", q, firstNames(got))
		}
	}
}

func TestSearch_ResultCountsAndCap(t *testing.T) {
	var tools []Tool
	var names []string
	for _, n := range []string{"inv_a", "inv_b", "inv_c", "inv_d", "inv_e", "inv_f", "inv_g", "inv_h"} {
		tools = append(tools, tool(n, "handles invoice"))
		names = append(names, n)
	}
	tools = append(tools, tool("other_a", "something else"))
	c := NewCatalog([]Skill{
		{Dir: "inv", Name: "inv", Tools: names},
		{Dir: "oth", Name: "oth", Tools: []string{"other_a"}},
	}, tools)

	if got := c.Search("invoice", nil, 50); len(got) != MaxResults {
		t.Fatalf("8 matches, big limit: got %d results, want %d", len(got), MaxResults)
	}
	if got := c.Search("invoice", nil, 2); len(got) != 2 {
		t.Fatalf("limit 2: got %d results", len(got))
	}
	if got := c.Search("invoice", nil, 0); len(got) != 0 {
		t.Fatalf("limit 0: got %d results", len(got))
	}
	if got := c.Search("something", nil, 5); len(got) != 1 {
		t.Fatalf("one match: got %v", firstNames(got))
	}

	three := NewCatalog([]Skill{{Dir: "s", Name: "s", Tools: []string{"x_1", "x_2", "x_3", "y_1", "y_2"}}},
		[]Tool{tool("x_1", "apple"), tool("x_2", "apple"), tool("x_3", "apple"), tool("y_1", "pear"), tool("y_2", "pear")})
	if got := three.Search("apple", nil, 5); len(got) != 3 {
		t.Fatalf("three matches: got %v", firstNames(got))
	}
}

func TestSearch_SkipsLoadedAndBreaksTiesByDirThenName(t *testing.T) {
	c := NewCatalog([]Skill{
		{Dir: "a", Name: "a", Tools: []string{"b_tool", "a_tool"}},
		{Dir: "b", Name: "b", Tools: []string{"c_tool"}},
	}, []Tool{tool("a_tool", "widget"), tool("b_tool", "widget"), tool("c_tool", "widget")})
	got := firstNames(c.Search("widget", nil, 5))
	if strings.Join(got, ",") != "a_tool,b_tool,c_tool" {
		t.Fatalf("tie order = %v, want a_tool,b_tool,c_tool", got)
	}
	got = firstNames(c.Search("widget", map[string]bool{"a_tool": true}, 5))
	if strings.Join(got, ",") != "b_tool,c_tool" {
		t.Fatalf("with a_tool skipped = %v", got)
	}
}

func TestCatalog_ToolBelongsToFirstSkillAndCoverage(t *testing.T) {
	c := NewCatalog([]Skill{
		{Dir: "a", Name: "first", Tools: []string{"shared"}},
		{Dir: "b", Name: "second", Tools: []string{"shared", "own"}},
	}, []Tool{tool("shared", "s"), tool("own", "o"), tool("loose", "l")})
	if got := c.Search("shared", nil, 5); len(got) != 1 || got[0].Skill != "first" {
		t.Fatalf("shared tool owner = %+v, want first", got)
	}
	if !c.Covered("shared") || !c.Covered("own") || c.Covered("loose") || c.Covered("ghost") {
		t.Fatal("Covered must be true only for listed tools present in the live catalogue")
	}
}

func TestIndex_LinesForIndexedSkillsOnly(t *testing.T) {
	c := NewCatalog([]Skill{
		{Dir: "b-second", Name: "second", Description: "Two", Tools: []string{"t2"}},
		{Dir: "a-first", Name: "first", Description: "One", Tools: []string{"t1"}},
		{Dir: "c-instr", Name: "instruction-only", Description: "No tools", Tools: nil},
		{Dir: "d-gone", Name: "gone", Description: "Tool not live", Tools: []string{"missing"}},
		{Dir: "e-bare", Name: "bare", Description: "", Tools: []string{"t3"}},
	}, []Tool{tool("t1", ""), tool("t2", ""), tool("t3", "")})

	// Directory order is the order of the input slice (ReadSkills sorts).
	want := "## Hidden tools\n" +
		"Some tools are hidden to keep this conversation small. Each line below is a skill: a group of hidden tools.\n" +
		"To use one, call tool_search with a few words describing the task. It returns the matching tool definitions.\n" +
		"Then call call_tool with {\"name\": \"<tool name>\", \"arguments\": {<arguments>}}.\n" +
		"- second: Two\n- first: One\n- bare"
	got := strings.TrimRight(c.Index(), "\n")
	if got != want {
		t.Fatalf("Index() =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(c.Index(), "instruction-only") || strings.Contains(c.Index(), "gone") {
		t.Fatal("skills without a live tool must not be indexed")
	}
	if len(c.Skills()) != 3 {
		t.Fatalf("Skills() = %d, want 3 indexed", len(c.Skills()))
	}
}

func TestIndex_EmptyWhenNoSkills(t *testing.T) {
	if got := NewCatalog(nil, []Tool{tool("t", "")}).Index(); got != "" {
		t.Fatalf("Index() = %q, want empty", got)
	}
}

func TestIndex_DescriptionCollapsedAndCut(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"whitespace collapsed", "a  b\n\tc", "a b c"},
		{"exactly 160 kept", strings.Repeat("a", 160), strings.Repeat("a", 160)},
		{"161 cut with ellipsis", strings.Repeat("a", 161), strings.Repeat("a", 160) + "…"},
		{"rune boundary", strings.Repeat("é", 100), strings.Repeat("é", 80) + "…"},
		{"rune boundary mid-rune", "a" + strings.Repeat("é", 100), "a" + strings.Repeat("é", 79) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCatalog([]Skill{{Dir: "s", Name: "s", Description: tc.in, Tools: []string{"t"}}}, []Tool{tool("t", "")})
			lines := strings.Split(strings.TrimRight(c.Index(), "\n"), "\n")
			last := lines[len(lines)-1]
			if !utf8.ValidString(last) {
				t.Fatalf("invalid UTF-8 in %q", last)
			}
			if last != "- s: "+tc.want {
				t.Fatalf("line = %q, want %q", last, "- s: "+tc.want)
			}
		})
	}
}

func TestEstimateTokens_BytesOverFour(t *testing.T) {
	// JSON of a 98-char string is 100 bytes with quotes.
	if got := EstimateTokens(strings.Repeat("a", 98)); got != 25 {
		t.Fatalf("EstimateTokens = %d, want 25", got)
	}
}

func TestDecide(t *testing.T) {
	auto := DefaultConfig()
	on := Config{Mode: ModeOn, Threshold: 0.1, MaxLoaded: 20}
	off := Config{Mode: ModeOff, Threshold: 0.1, MaxLoaded: 20}
	cases := []struct {
		name      string
		cfg       Config
		tool, ctx int
		hideable  int
		active    bool
		reason    string
		wantCtx   int
	}{
		{"auto above threshold", auto, 1001, 10000, 3, true, "auto_threshold", 10000},
		{"auto exactly at threshold", auto, 1000, 10000, 3, false, "auto_below_threshold", 10000},
		{"auto below threshold", auto, 999, 10000, 3, false, "auto_below_threshold", 10000},
		{"auto unknown context uses 32768 (above)", auto, 3277, 0, 3, true, "auto_threshold", 32768},
		{"auto unknown context uses 32768 (below)", auto, 3276, 0, 3, false, "auto_below_threshold", 32768},
		{"on ignores size", on, 1, 10000, 3, true, "on", 10000},
		{"off", off, 99999, 10000, 3, false, "off", 10000},
		{"on but nothing hideable", on, 5000, 10000, 0, false, "no_hideable", 10000},
		{"auto but nothing hideable", auto, 5000, 10000, 0, false, "no_hideable", 10000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.cfg, tc.tool, tc.ctx, tc.hideable)
			if d.Active != tc.active || d.Reason != tc.reason {
				t.Fatalf("Decide = %+v, want active=%v reason=%s", d, tc.active, tc.reason)
			}
			if d.ContextTokens != tc.wantCtx || d.ToolTokens != tc.tool {
				t.Fatalf("Decide tokens = %d/%d, want tool %d ctx %d", d.ToolTokens, d.ContextTokens, tc.tool, tc.wantCtx)
			}
		})
	}
	if UnknownContextTokens != 32768 || MaxResults != 5 || IndexLineMaxBytes != 160 {
		t.Fatal("exported constants differ from the contract")
	}
}
