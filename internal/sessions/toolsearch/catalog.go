package toolsearch

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// Tool is one MCP tool in the OpenAI function shape. Parameters is the MCP
// inputSchema verbatim.
type Tool struct {
	Name, Description string
	Parameters        json.RawMessage
}

// Match is one search hit.
type Match struct {
	Tool  Tool
	Skill string
	Score int
}

type entry struct {
	tool      Tool
	skillName string
	skillIdx  int
	fields    []field
}

// Catalog maps hidden-able tools to the skills that cover them.
type Catalog struct {
	skills  []Skill // indexed skills only, Tools narrowed to owned live tools
	entries []entry
	covered map[string]bool
}

// NewCatalog joins skills (in directory order) with the live MCP tools. A
// tool listed by several skills belongs to the first. A skill left with no
// live tool is not indexed, which also leaves instruction-only skills out.
func NewCatalog(skills []Skill, tools []Tool) *Catalog {
	live := make(map[string]Tool, len(tools))
	for _, t := range tools {
		if _, dup := live[t.Name]; !dup {
			live[t.Name] = t
		}
	}
	c := &Catalog{covered: map[string]bool{}}
	for _, s := range skills {
		var owned []Tool
		var names []string
		for _, n := range s.Tools {
			t, ok := live[n]
			if !ok || c.covered[n] {
				continue
			}
			c.covered[n] = true
			owned = append(owned, t)
			names = append(names, n)
		}
		if len(owned) == 0 {
			continue
		}
		s.Tools = names
		idx := len(c.skills)
		c.skills = append(c.skills, s)
		skillFields := []field{
			{weightName, tokenize(s.Name)},
			{weightKeywords, tokenize(strings.Join(s.Keywords, " "))},
			{weightDesc, tokenize(s.Description)},
		}
		for _, t := range owned {
			f := append(append([]field(nil), skillFields...),
				field{weightToolName, tokenize(t.Name)},
				field{weightDesc, tokenize(t.Description)})
			c.entries = append(c.entries, entry{tool: t, skillName: s.Name, skillIdx: idx, fields: f})
		}
	}
	return c
}

// Skills returns the indexed skills in directory order.
func (c *Catalog) Skills() []Skill { return append([]Skill(nil), c.skills...) }

// Covered reports whether an indexed skill owns the live tool name.
func (c *Catalog) Covered(name string) bool { return c.covered[name] }

const indexHeader = `## Hidden tools
Some tools are hidden to keep this conversation small. Each line below is a skill: a group of hidden tools.
To use one, call tool_search with a few words describing the task. It returns the matching tool definitions.
Then call call_tool with {"name": "<tool name>", "arguments": {<arguments>}}.`

// Index renders the system-prompt block, or "" when no skill is indexed.
func (c *Catalog) Index() string {
	if len(c.skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(indexHeader)
	for _, s := range c.skills {
		b.WriteString("\n- ")
		b.WriteString(s.Name)
		if d := indexDescription(s.Description); d != "" {
			b.WriteString(": ")
			b.WriteString(d)
		}
	}
	return b.String()
}

func indexDescription(d string) string {
	d = strings.Join(strings.Fields(d), " ")
	if len(d) <= IndexLineMaxBytes {
		return d
	}
	cut := IndexLineMaxBytes
	for cut > 0 && !utf8.RuneStart(d[cut]) {
		cut--
	}
	return d[:cut] + "…"
}

// Search returns up to min(limit, MaxResults) tools scoring above zero,
// best first, ties by skill order then tool name. Names in skip are left
// out.
func (c *Catalog) Search(query string, skip map[string]bool, limit int) []Match {
	if limit > MaxResults {
		limit = MaxResults
	}
	q := queryTokens(query)
	if limit <= 0 || len(q) == 0 {
		return nil
	}
	var hits []Match
	var idx []int
	for _, e := range c.entries {
		if skip[e.tool.Name] {
			continue
		}
		if sc := score(q, e.fields); sc > 0 {
			hits = append(hits, Match{Tool: e.tool, Skill: e.skillName, Score: sc})
			idx = append(idx, e.skillIdx)
		}
	}
	order := make([]int, len(hits))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := order[a], order[b]
		if hits[x].Score != hits[y].Score {
			return hits[x].Score > hits[y].Score
		}
		if idx[x] != idx[y] {
			return idx[x] < idx[y]
		}
		return hits[x].Tool.Name < hits[y].Tool.Name
	})
	if len(order) > limit {
		order = order[:limit]
	}
	out := make([]Match, len(order))
	for i, o := range order {
		out[i] = hits[o]
	}
	return out
}

// Decision is the activation verdict made once at provider Start.
type Decision struct {
	Active                    bool
	Reason                    string
	ToolTokens, ContextTokens int
}

// EstimateTokens is len(JSON)/4. No tokenizer is available for an arbitrary
// local model, and the figure only has to compare tool schemas with a
// context window, where a consistent rough ratio is enough.
func EstimateTokens(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

// Decide applies the mode to the estimated tool cost. hideable is the number
// of tools that would be hidden; zero means there is nothing to gain. An
// unknown context (<=0) falls back to UnknownContextTokens.
func Decide(cfg Config, toolTokens, contextTokens, hideable int) Decision {
	if contextTokens <= 0 {
		contextTokens = UnknownContextTokens
	}
	d := Decision{ToolTokens: toolTokens, ContextTokens: contextTokens}
	switch {
	case cfg.Mode == ModeOff:
		d.Reason = "off"
	case hideable <= 0:
		d.Reason = "no_hideable"
	case cfg.Mode == ModeOn:
		d.Active, d.Reason = true, "on"
	case float64(toolTokens) > cfg.Threshold*float64(contextTokens):
		d.Active, d.Reason = true, "auto_threshold"
	default:
		d.Reason = "auto_below_threshold"
	}
	return d
}
