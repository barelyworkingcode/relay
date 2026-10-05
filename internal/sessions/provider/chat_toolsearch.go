package provider

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/barelyworkingcode/relay/internal/sessions/toolsearch"
)

// Chat tool search: when a session's relay MCP tools would cost too much of
// the model's context, Start hides the tools a project skill covers behind
// two fixed tools, tool_search and call_tool. The tool list is decided once
// at Start and never changes for the provider's life, so every PostChat
// sends the same bytes and the model's prompt cache stays valid. What a
// search loads only changes which names call_tool will accept, and what the
// model has read in earlier tool messages.

const (
	toolSearchName = "tool_search"
	callToolName   = "call_tool"

	// toolSearchResultMaxBytes bounds a tool_search result. Whole trailing
	// entries are dropped instead of cutting the JSON, so the model never
	// reads a truncated schema.
	toolSearchResultMaxBytes = 16 * 1024
)

// contextTokensReporter is implemented by a transport that learned the
// model's context window. Optional, so chatTransport itself is unchanged.
type contextTokensReporter interface {
	ContextTokens() int
}

// toolSearchState is the immutable outcome of the Start-time decision.
type toolSearchState struct {
	sessionID string
	cfg       toolsearch.Config
	catalog   *toolsearch.Catalog
	defs      []map[string]any // the fixed tool list sent every request
	index     string           // appended to the system message
	known     map[string]bool  // every live MCP tool name
	hidden    map[string]bool  // covered and not pinned
	pinned    map[string]bool  // pinned and live
	// catalogNames lists every live tool name, for the already-loaded lookup.
	catalogNames []string
}

var toolSearchDef = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        toolSearchName,
		"description": "Load hidden tools. Give a few words describing the task, for example \"tide times for a port\". Returns up to 5 matching tool definitions. Call a returned tool with call_tool.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "A few words describing the task.",
				},
			},
			"required": []string{"query"},
		},
	},
}

var callToolDef = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        callToolName,
		"description": "Call a tool that tool_search returned.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "The tool's name, as tool_search returned it.",
				},
				"arguments": map[string]any{
					"type":        "object",
					"description": "The tool's arguments, matching its parameters.",
				},
			},
			"required": []string{"name", "arguments"},
		},
	},
}

// setupToolSearch makes the Start-time decision. It returns nil when tool
// search is off or inactive; the provider then behaves exactly as before.
// It logs the one chat.tool_search line unless no config path is set.
func (p *ChatProvider) setupToolSearch(configPath string) *toolSearchState {
	if configPath == "" {
		return nil
	}
	sid := p.session.ID

	cfg, cfgErr := toolsearch.LoadConfig(configPath)
	var defs []map[string]any
	if p.mcpManager != nil && p.mcpManager.HasTools() {
		defs = p.mcpManager.ChatToolDefs()
	}
	tools := toolsFromDefs(defs)
	total := len(tools)
	toolTokens := toolsearch.EstimateTokens(defs)
	ctxTokens := 0
	if r, ok := p.transport.(contextTokensReporter); ok {
		ctxTokens = r.ContextTokens()
	}

	report := func(d toolsearch.Decision, reason string, skills, sent, hidden, pinned int) {
		ctx := d.ContextTokens
		if ctx <= 0 {
			ctx = ctxTokens
			if ctx <= 0 {
				ctx = toolsearch.UnknownContextTokens
			}
		}
		slog.Info("chat tool search",
			"op", "chat.tool_search", "status", "ok", "session_id", sid,
			"mode", string(cfg.Mode), "active", d.Active, "reason", reason,
			"skills", skills, "tools_total", total, "tools_sent", sent,
			"hidden", hidden, "pinned", pinned,
			"tool_tokens", toolTokens, "context_tokens", ctx)
	}
	inactive := func(reason string) *toolSearchState {
		report(toolsearch.Decision{}, reason, 0, total, 0, 0)
		return nil
	}

	if cfgErr != nil {
		slog.Warn("chat tool search config invalid, tool search off",
			"op", "chat.tool_search", "status", "error", "session_id", sid,
			"error", cfgErr.Error())
		return inactive("config_invalid")
	}
	if cfg.Mode == toolsearch.ModeOff {
		return inactive("off")
	}
	if total == 0 || p.session.GetHost() != nil {
		return inactive("no_tools")
	}
	for _, t := range tools {
		if t.Name == toolSearchName || t.Name == callToolName {
			slog.Warn("chat tool search inactive: an MCP tool is named tool_search or call_tool",
				"op", "chat.tool_search", "status", "ok", "session_id", sid, "tool", t.Name)
			return inactive("name_collision")
		}
	}

	skills, err := toolsearch.ReadSkills(p.session.Directory)
	if err != nil {
		slog.Warn("chat tool search: skills unreadable",
			"op", "chat.tool_search", "status", "ok", "session_id", sid, "error", err.Error())
	}
	cat := toolsearch.NewCatalog(skills, tools)

	known := make(map[string]bool, total)
	for _, t := range tools {
		known[t.Name] = true
	}
	pinned := map[string]bool{}
	for _, n := range cfg.Pinned {
		if !known[n] {
			slog.Warn("chat tool search: pinned tool not found",
				"op", "chat.tool_search", "status", "ok", "session_id", sid, "tool", n)
			continue
		}
		pinned[n] = true
	}
	hidden := map[string]bool{}
	for _, t := range tools {
		if cat.Covered(t.Name) && !pinned[t.Name] {
			hidden[t.Name] = true
		}
	}
	indexLines := len(cat.Skills())

	d := toolsearch.Decide(cfg, toolTokens, ctxTokens, len(hidden))
	if !d.Active {
		report(d, d.Reason, indexLines, total, 0, len(pinned))
		return nil
	}

	sent := make([]map[string]any, 0, total-len(hidden)+2)
	sent = append(sent, toolSearchDef, callToolDef)
	for i, def := range defs {
		if !hidden[tools[i].Name] {
			sent = append(sent, def)
		}
	}
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	st := &toolSearchState{
		sessionID:    sid,
		cfg:          cfg,
		catalog:      cat,
		defs:         sent,
		index:        cat.Index(),
		known:        known,
		hidden:       hidden,
		pinned:       pinned,
		catalogNames: names,
	}
	report(d, d.Reason, indexLines, len(sent), len(hidden), len(pinned))
	return st
}

// toolsFromDefs converts MCP chat tool defs, in order, to catalogue tools.
// The result is index-aligned with defs.
func toolsFromDefs(defs []map[string]any) []toolsearch.Tool {
	out := make([]toolsearch.Tool, 0, len(defs))
	for _, def := range defs {
		fn, _ := def["function"].(map[string]any)
		t := toolsearch.Tool{}
		t.Name, _ = fn["name"].(string)
		t.Description, _ = fn["description"].(string)
		if params, ok := fn["parameters"]; ok && params != nil {
			t.Parameters, _ = json.Marshal(params)
		}
		out = append(out, t)
	}
	return out
}

// systemPrompt returns the system message text: the session's prompt plus,
// when tool search is active, the hidden-tools index. The index is built per
// request and never written back to the session.
func (p *ChatProvider) systemPrompt() string {
	sp := p.session.SystemPrompt
	ts := p.toolSearch()
	if ts == nil || ts.index == "" {
		return sp
	}
	if sp == "" {
		return ts.index
	}
	return sp + "\n\n" + ts.index
}

func (p *ChatProvider) toolSearch() *toolSearchState {
	p.tsMu.Lock()
	defer p.tsMu.Unlock()
	return p.ts
}

// committedLoads returns a copy of the loaded names committed so far.
func (p *ChatProvider) committedLoads() []string {
	p.tsMu.Lock()
	defer p.tsMu.Unlock()
	return append([]string(nil), p.tsLoaded...)
}

// commitLoads records names a finished turn loaded.
func (p *ChatProvider) commitLoads(names []string) {
	if len(names) == 0 {
		return
	}
	p.tsMu.Lock()
	defer p.tsMu.Unlock()
	have := make(map[string]bool, len(p.tsLoaded))
	for _, n := range p.tsLoaded {
		have[n] = true
	}
	for _, n := range names {
		if !have[n] {
			p.tsLoaded = append(p.tsLoaded, n)
			have[n] = true
		}
	}
}

// adoptToolSearch installs the decision and filters names restored before Start against the live
// catalogue, called once at Start with the decision's outcome (nil = off).
func (p *ChatProvider) adoptToolSearch(ts *toolSearchState) {
	p.tsMu.Lock()
	defer p.tsMu.Unlock()
	p.ts = ts
	restored := p.tsLoaded
	p.tsLoaded = nil
	if ts == nil {
		return
	}
	for _, n := range restored {
		if !ts.hidden[n] {
			slog.Debug("chat tool search: restored tool not in catalogue, dropped",
				"session_id", ts.sessionID, "tool", n)
			continue
		}
		if len(p.tsLoaded) >= ts.cfg.MaxLoaded {
			break
		}
		p.tsLoaded = append(p.tsLoaded, n)
	}
}

type toolSearchProviderState struct {
	ToolSearch *struct {
		Loaded []string `json:"loaded"`
	} `json:"toolSearch"`
}

// GetState returns the provider state persisted with the session. It runs
// on the persist goroutine, hence the lock.
func (p *ChatProvider) GetState() json.RawMessage {
	loaded := p.committedLoads()
	if len(loaded) == 0 {
		return json.RawMessage(`{}`)
	}
	data, err := json.Marshal(map[string]any{"toolSearch": map[string]any{"loaded": loaded}})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return data
}

// RestoreState reads the loaded list before Start; Start filters it.
func (p *ChatProvider) RestoreState(raw json.RawMessage) {
	var st toolSearchProviderState
	if len(raw) == 0 || json.Unmarshal(raw, &st) != nil || st.ToolSearch == nil {
		return
	}
	p.tsMu.Lock()
	defer p.tsMu.Unlock()
	p.tsLoaded = append([]string(nil), st.ToolSearch.Loaded...)
}

// toolSearchTurn is one turn's view of what is loaded: the committed names
// plus the ones this turn's searches add. Pending names reach the provider
// only when the turn commits.
type toolSearchTurn struct {
	loaded  map[string]bool
	pending []string
}

func (ts *toolSearchState) newTurn(committed []string) *toolSearchTurn {
	t := &toolSearchTurn{loaded: make(map[string]bool, len(committed))}
	for _, n := range committed {
		t.loaded[n] = true
	}
	return t
}

// toolSearchOutcome is how runToolLoop must treat one tool call. Final
// results go to the model as they are; otherwise the loop forwards Name and
// Args to the MCP manager.
type toolSearchOutcome struct {
	Final     bool
	Result    string
	IsError   bool
	CutExempt bool
	Name      string
	Args      json.RawMessage
}

func finalJSON(v any, isErr bool) toolSearchOutcome {
	return toolSearchOutcome{Final: true, Result: marshalNoEscape(v), IsError: isErr}
}

func marshalNoEscape(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return `{"error":"internal error"}`
	}
	return strings.TrimRight(buf.String(), "\n")
}

// route applies tool search's rules to one model tool call.
func (ts *toolSearchState) route(tc NormalizedToolCall, turn *toolSearchTurn) toolSearchOutcome {
	switch tc.Name {
	case toolSearchName:
		return ts.search(tc.Arguments, turn)
	case callToolName:
		var in struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(tc.Arguments, &in); err != nil || in.Name == "" {
			return finalJSON(map[string]string{"error": "call_tool needs a name and arguments"}, true)
		}
		return ts.dispatch(in.Name, normalizeCallArguments(in.Arguments), turn)
	default:
		return ts.dispatch(tc.Name, tc.Arguments, turn)
	}
}

// normalizeCallArguments applies call_tool's argument rules: a JSON string
// holding an object is unwrapped, absent or null becomes {}, anything else
// is forwarded byte for byte.
func normalizeCallArguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`)
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			inner := strings.TrimSpace(s)
			if strings.HasPrefix(inner, "{") && json.Valid([]byte(inner)) {
				return json.RawMessage(s)
			}
		}
	}
	return raw
}

// dispatch decides whether a named tool may run: visible and pinned tools
// always, a hidden tool only once loaded.
func (ts *toolSearchState) dispatch(name string, args json.RawMessage, turn *toolSearchTurn) toolSearchOutcome {
	if ts.hidden[name] && !turn.loaded[name] {
		slog.Warn("chat call_tool denied: tool not loaded",
			"op", "chat.call_tool", "status", "denied", "error", "not_loaded",
			"session_id", ts.sessionID, "tool", name)
		return finalJSON(map[string]string{
			"error": "tool \"" + name + "\" is not loaded; call tool_search first",
		}, true)
	}
	if !ts.known[name] {
		return finalJSON(map[string]string{"error": "unknown tool \"" + name + "\""}, true)
	}
	return toolSearchOutcome{Name: name, Args: args}
}

type toolSearchEntry struct {
	Name        string          `json:"name"`
	Skill       string          `json:"skill"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type toolSearchResult struct {
	Loaded        []toolSearchEntry `json:"loaded"`
	AlreadyLoaded []string          `json:"alreadyLoaded"`
	HowToCall     string            `json:"howToCall,omitempty"`
	Error         string            `json:"error,omitempty"`
}

const toolSearchHowToCall = `call_tool {"name": "<name>", "arguments": {…}}`

func (ts *toolSearchState) search(raw json.RawMessage, turn *toolSearchTurn) toolSearchOutcome {
	var in struct {
		Query string `json:"query"`
	}
	if json.Unmarshal(raw, &in) != nil || strings.TrimSpace(in.Query) == "" {
		return finalJSON(map[string]string{"error": "query is required"}, true)
	}

	// Names this query matches that are already usable.
	usable := map[string]bool{}
	skip := map[string]bool{}
	for _, n := range ts.catalogNames {
		if turn.loaded[n] || ts.pinned[n] {
			usable[n] = true
		} else {
			skip[n] = true
		}
	}
	already := []string{}
	for _, m := range ts.catalog.Search(in.Query, skip, toolsearch.MaxResults) {
		already = append(already, m.Tool.Name)
	}

	remaining := ts.cfg.MaxLoaded - len(turn.loaded)
	if remaining <= 0 {
		return ts.searchError(already, "loaded-tool limit reached ("+strconv.Itoa(ts.cfg.MaxLoaded)+"); use a loaded tool", 0, turn)
	}
	limit := toolsearch.MaxResults
	if remaining < limit {
		limit = remaining
	}
	skipLoaded := map[string]bool{}
	for n := range usable {
		skipLoaded[n] = true
	}
	matches := ts.catalog.Search(in.Query, skipLoaded, limit)
	if len(matches) == 0 {
		return ts.searchError(already, "no hidden tool matches; try other words from the skill list", 0, turn)
	}

	entries := make([]toolSearchEntry, 0, len(matches))
	for _, m := range matches {
		params := m.Tool.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{}`)
		}
		entries = append(entries, toolSearchEntry{
			Name: m.Tool.Name, Skill: m.Skill, Description: m.Tool.Description, Parameters: params,
		})
	}
	sort.Strings(already)
	res := toolSearchResult{Loaded: entries, AlreadyLoaded: already, HowToCall: toolSearchHowToCall}
	out := marshalNoEscape(res)
	for len(out) > toolSearchResultMaxBytes && len(res.Loaded) > 1 {
		res.Loaded = res.Loaded[:len(res.Loaded)-1]
		out = marshalNoEscape(res)
	}
	names := make([]string, 0, len(res.Loaded))
	for _, e := range res.Loaded {
		if !turn.loaded[e.Name] {
			turn.loaded[e.Name] = true
			turn.pending = append(turn.pending, e.Name)
		}
		names = append(names, e.Name)
	}
	logToolSearchQuery(ts.sessionID, len(names), len(turn.loaded), names)
	return toolSearchOutcome{Final: true, Result: out, CutExempt: true}
}

func (ts *toolSearchState) searchError(already []string, msg string, returned int, turn *toolSearchTurn) toolSearchOutcome {
	logToolSearchQuery(ts.sessionID, returned, len(turn.loaded), nil)
	sort.Strings(already)
	return finalJSON(toolSearchResult{Loaded: []toolSearchEntry{}, AlreadyLoaded: already, Error: msg}, true)
}

func logToolSearchQuery(sessionID string, returned, loadedTotal int, names []string) {
	joined := strings.Join(names, ",")
	if len(joined) > 500 {
		joined = joined[:500]
	}
	slog.Info("chat tool search query",
		"op", "chat.tool_search.query", "status", "ok", "session_id", sessionID,
		"returned", returned, "loaded_total", loadedTotal, "tools", joined)
}
