package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// ---- harness ----

type tsReply struct {
	tool, args, text string // a tool call (tool+args) or text; both empty = "done"
	status           int    // non-zero = plain HTTP status, no stream
}

func callReply(tool, args string) tsReply { return tsReply{tool: tool, args: args} }

type tsEnv struct {
	t      *testing.T
	sock   string
	models string
	mu     sync.Mutex
	bodies [][]byte
	script []tsReply
}

func newTSEnv(t *testing.T, models string, script ...tsReply) *tsEnv {
	t.Helper()
	e := &tsEnv{t: t, sock: filepath.Join(shortTempDir(t), "model.sock"), models: models, script: script}
	if e.models == "" {
		e.models = `{"data":[{"id":"sonnet"}]}`
	}
	fakeBroker(t, e.sock, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(e.models))
			return
		}
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.bodies = append(e.bodies, body)
		idx := len(e.bodies) - 1
		var rep tsReply
		if idx < len(e.script) {
			rep = e.script[idx]
		}
		e.mu.Unlock()
		if rep.status != 0 {
			w.WriteHeader(rep.status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if rep.tool != "" {
			args, _ := json.Marshal(rep.args)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_%d\",\"function\":{\"name\":%q,\"arguments\":%s}}]}}]}\n\n", idx, rep.tool, args)
		} else {
			text, _ := json.Marshal(orDefault(rep.text, "done"))
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", text)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	return e
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (e *tsEnv) requests() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]byte(nil), e.bodies...)
}

type tsRequest struct {
	Tools    json.RawMessage `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func decodeReq(t *testing.T, b []byte) tsRequest {
	t.Helper()
	var r tsRequest
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("decode request: %v\n%s", err, b)
	}
	return r
}

func toolNames(t *testing.T, r tsRequest) []string {
	t.Helper()
	var defs []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if len(r.Tools) > 0 {
		if err := json.Unmarshal(r.Tools, &defs); err != nil {
			t.Fatal(err)
		}
	}
	var out []string
	for _, d := range defs {
		out = append(out, d.Function.Name)
	}
	return out
}

func (r tsRequest) system() string {
	if len(r.Messages) == 0 || r.Messages[0].Role != "system" {
		return ""
	}
	var s string
	_ = json.Unmarshal(r.Messages[0].Content, &s)
	return s
}

// toolResults returns the content strings of role "tool" messages.
func (r tsRequest) toolResults() []string {
	var out []string
	for _, m := range r.Messages {
		if m.Role == "tool" {
			var s string
			_ = json.Unmarshal(m.Content, &s)
			out = append(out, s)
		}
	}
	return out
}

func writeProject(t *testing.T, skills map[string]string) string {
	t.Helper()
	proj := t.TempDir()
	for dir, md := range skills {
		d := filepath.Join(proj, ".claude", "skills", dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return proj
}

func skillDoc(name, desc, keywords string, tools ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\nname: %s\ndescription: %s\nkeywords: [%s]\n---\n# %s\n\n## Tools\n", name, desc, keywords, name)
	for _, tl := range tools {
		fmt.Fprintf(&b, "- **%s** — does %s\n", tl, tl)
	}
	return b.String()
}

func mainSkills() map[string]string {
	return map[string]string{
		"relay-mail":  skillDoc("relay-mail", "Send and read email", "inbox", "mail_send", "mail_read"),
		"relay-tides": skillDoc("relay-tides", "Tide times for ports", "tides, harbour", "tides_lookup"),
		"relay-memo":  "---\nname: relay-memo\ndescription: Instruction only skill\n---\nNo tools here.\n",
	}
}

var tideSchema = map[string]any{
	"type":       "object",
	"properties": map[string]any{"port": map[string]any{"type": "string"}, "date": map[string]any{"type": "string"}},
	"required":   []any{"port"},
}

func okTool(name, desc string, schema map[string]any) testutil.FakeTool {
	return testutil.FakeTool{Name: name, Description: desc, Schema: schema,
		Handler: func(json.RawMessage) (string, error) { return "ok " + name, nil }}
}

func mainTools() []testutil.FakeTool {
	return []testutil.FakeTool{
		okTool("mail_send", "Send an email", map[string]any{"type": "object"}),
		okTool("mail_read", "Read the inbox", map[string]any{"type": "object"}),
		okTool("tides_lookup", "Look up tide times for a port", tideSchema),
		okTool("clock_now", "Current time", map[string]any{"type": "object"}),
	}
}

func chatJSON(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const onCfg = `{"toolSearch":{"mode":"on"}}`

type tsRun struct {
	p    *ChatProvider
	sess *sessionstypes.Session
	fake *testutil.FakeMCPClient
	evCh chan string
}

func (e *tsEnv) start(t *testing.T, sess *sessionstypes.Session, cfgPath string, fake *testutil.FakeMCPClient) *tsRun {
	t.Helper()
	evCh := make(chan string, 4096)
	p := NewChatProvider(sess, func(ev string, _ json.RawMessage) { evCh <- ev },
		ChatConfig{ModelSocket: e.sock, ModelKey: "test-key", ToolSearchConfigPath: cfgPath})
	if fake != nil {
		p.SetMCPClient(fake)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(p.Kill)
	return &tsRun{p: p, sess: sess, fake: fake, evCh: evCh}
}

func (r *tsRun) send(t *testing.T) {
	t.Helper()
	if err := r.p.SendMessage("go", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	collectUntilComplete(t, r.evCh)
}

func newSess(id, dir string) *sessionstypes.Session {
	return &sessionstypes.Session{ID: id, Model: "sonnet", Directory: dir, SystemPrompt: "Be brief."}
}

func lastCallArgs(t *testing.T, f *testutil.FakeMCPClient, name string) (string, bool) {
	t.Helper()
	calls := f.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Name == name {
			return string(calls[i].Args), true
		}
	}
	return "", false
}

const wantSearchDef = `{"type":"function","function":{"name":"tool_search","description":"Load hidden tools. Give a few words describing the task, for example \"tide times for a port\". Returns up to 5 matching tool definitions. Call a returned tool with call_tool.","parameters":{"type":"object","properties":{"query":{"type":"string","description":"A few words describing the task."}},"required":["query"]}}}`

// ---- criteria 1, 2: index and fixed meta tools ----

func TestToolSearch_FirstRequestCarriesIndexAndHidesCoveredDefinitions(t *testing.T) {
	env := newTSEnv(t, "")
	sess := newSess("ts1", writeProject(t, mainSkills()))
	run := env.start(t, sess, chatJSON(t, onCfg), testutil.NewFakeMCPClient(mainTools()...))
	run.send(t)

	req := decodeReq(t, env.requests()[0])
	names := toolNames(t, req)
	if want := []string{"tool_search", "call_tool", "clock_now"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("tools = %v, want %v (meta tools, then uncovered tools in catalogue order)", names, want)
	}
	var defs []json.RawMessage
	_ = json.Unmarshal(req.Tools, &defs)
	var got, want any
	_ = json.Unmarshal(defs[0], &got)
	_ = json.Unmarshal([]byte(wantSearchDef), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tools[0] = %s, want %s", defs[0], wantSearchDef)
	}

	sys := req.system()
	const header = "## Hidden tools\nSome tools are hidden to keep this conversation small."
	wantTail := "- relay-mail: Send and read email\n- relay-tides: Tide times for ports"
	if !strings.HasPrefix(sys, "Be brief.\n\n"+header) || !strings.Contains(sys, wantTail) {
		t.Fatalf("system message = %q", sys)
	}
	if strings.Contains(sys, "relay-memo") {
		t.Fatal("instruction-only skill must not be indexed")
	}
	if strings.Contains(string(env.requests()[0]), "Look up tide times for a port") {
		t.Fatal("hidden tool definition leaked into the first request")
	}
	if sess.SystemPrompt != "Be brief." {
		t.Fatalf("persisted SystemPrompt was changed to %q", sess.SystemPrompt)
	}
}

// ---- criteria 5, 6, 11 and the session record ----

func TestToolSearch_SearchThenCall_DefinitionsPersistedToolsStableArgsVerbatim(t *testing.T) {
	const rawArgs = `{"date": "2026-01-01",  "port":"Acme Bay"}`
	env := newTSEnv(t, "",
		callReply("tool_search", `{"query":"tide times"}`),
		callReply("call_tool", `{"name":"tides_lookup","arguments":`+rawArgs+`}`),
		tsReply{text: "done"},
		tsReply{text: "again"},
	)
	sess := newSess("ts2", writeProject(t, mainSkills()))
	fake := testutil.NewFakeMCPClient(mainTools()...)
	run := env.start(t, sess, chatJSON(t, onCfg), fake)
	run.send(t)
	run.send(t)

	reqs := env.requests()
	if len(reqs) != 4 {
		t.Fatalf("got %d model requests, want 4", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		if a, b := string(decodeReq(t, reqs[0]).Tools), string(decodeReq(t, reqs[i]).Tools); a != b {
			t.Fatalf("tools differ between request 0 and %d:\n%s\n%s", i, a, b)
		}
	}

	res := decodeReq(t, reqs[1]).toolResults()
	if len(res) != 1 {
		t.Fatalf("request 1 has %d tool results, want 1", len(res))
	}
	var sr struct {
		Loaded []struct {
			Name       string         `json:"name"`
			Skill      string         `json:"skill"`
			Parameters map[string]any `json:"parameters"`
		} `json:"loaded"`
		HowToCall string `json:"howToCall"`
	}
	if err := json.Unmarshal([]byte(res[0]), &sr); err != nil {
		t.Fatalf("tool_search result is not JSON: %v\n%s", err, res[0])
	}
	if len(sr.Loaded) == 0 || sr.Loaded[0].Name != "tides_lookup" || sr.Loaded[0].Skill != "relay-tides" || sr.HowToCall == "" {
		t.Fatalf("tool_search result = %s", res[0])
	}
	if !reflect.DeepEqual(sr.Loaded[0].Parameters, tideSchema2()) {
		t.Fatalf("parameters = %v, want the tool's schema verbatim", sr.Loaded[0].Parameters)
	}

	if got, ok := lastCallArgs(t, fake, "tides_lookup"); !ok || got != rawArgs {
		t.Fatalf("CallTool(tides_lookup) args = %q (called=%v), want %q verbatim", got, ok, rawArgs)
	}
	for _, c := range fake.Calls() {
		if c.Name == "call_tool" || c.Name == "tool_search" {
			t.Fatalf("dispatcher %q reached the MCP client", c.Name)
		}
	}

	var sawSearch bool
	sess.Lock()
	for _, m := range sess.Messages {
		if m.Role == "tool" && m.ToolName == "tool_search" {
			sawSearch = true
		}
	}
	sess.Unlock()
	if !sawSearch {
		t.Fatal("tool_search result not persisted as a tool message with ToolName tool_search")
	}
}

func tideSchema2() map[string]any {
	b, _ := json.Marshal(tideSchema)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// ---- call_tool rules ----

func TestToolSearch_CallRules(t *testing.T) {
	cases := []struct {
		name        string
		script      []tsReply // before the final text reply
		wantCalled  string    // MCP tool expected to be called ("" = none)
		wantArgs    string
		wantInError string // substring of the last tool result
	}{
		{"hidden tool not loaded via call_tool", []tsReply{callReply("call_tool", `{"name":"tides_lookup","arguments":{}}`)},
			"", "", `tool "tides_lookup" is not loaded; call tool_search first`},
		{"hidden tool not loaded by real name", []tsReply{callReply("tides_lookup", `{"port":"x"}`)},
			"", "", `tool "tides_lookup" is not loaded; call tool_search first`},
		{"unknown name", []tsReply{callReply("call_tool", `{"name":"no_such","arguments":{}}`)},
			"", "", "unknown tool"},
		{"visible tool through call_tool", []tsReply{callReply("call_tool", `{"name":"clock_now","arguments":{"a":1}}`)},
			"clock_now", `{"a":1}`, ""},
		{"visible tool by real name", []tsReply{callReply("clock_now", `{"b":2}`)},
			"clock_now", `{"b":2}`, ""},
		{"loaded tool by real name", []tsReply{callReply("tool_search", `{"query":"tide"}`), callReply("tides_lookup", `{"port":"p1"}`)},
			"tides_lookup", `{"port":"p1"}`, ""},
		{"arguments as JSON string holding an object", []tsReply{callReply("call_tool", `{"name":"clock_now","arguments":"{\"c\":3}"}`)},
			"clock_now", `{"c":3}`, ""},
		{"arguments absent", []tsReply{callReply("call_tool", `{"name":"clock_now"}`)},
			"clock_now", `{}`, ""},
		{"arguments null", []tsReply{callReply("call_tool", `{"name":"clock_now","arguments":null}`)},
			"clock_now", `{}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTSEnv(t, "", append(tc.script, tsReply{text: "done"})...)
			fake := testutil.NewFakeMCPClient(mainTools()...)
			run := env.start(t, newSess("ts3", writeProject(t, mainSkills())), chatJSON(t, onCfg), fake)
			run.send(t)

			reqs := env.requests()
			results := decodeReq(t, reqs[len(reqs)-1]).toolResults()
			last := results[len(results)-1]
			if tc.wantCalled == "" {
				if n := len(fake.Calls()); n != 0 {
					t.Fatalf("MCP client was called %d times: %+v", n, fake.Calls())
				}
				var e struct {
					Error string `json:"error"`
				}
				_ = json.Unmarshal([]byte(last), &e)
				if !strings.Contains(e.Error, tc.wantInError) {
					t.Fatalf("result = %s, want error containing %q", last, tc.wantInError)
				}
				return
			}
			got, ok := lastCallArgs(t, fake, tc.wantCalled)
			if !ok || got != tc.wantArgs {
				t.Fatalf("CallTool(%s) = %q (called=%v), want args %q", tc.wantCalled, got, ok, tc.wantArgs)
			}
		})
	}
}

func TestToolSearch_CallToolResultStillCutAt8192(t *testing.T) {
	env := newTSEnv(t, "", callReply("call_tool", `{"name":"clock_now","arguments":{}}`), tsReply{text: "done"})
	tools := mainTools()
	tools[3].Handler = func(json.RawMessage) (string, error) { return strings.Repeat("z", 20000), nil }
	run := env.start(t, newSess("ts4", writeProject(t, mainSkills())), chatJSON(t, onCfg), testutil.NewFakeMCPClient(tools...))
	run.send(t)
	res := decodeReq(t, env.requests()[1]).toolResults()
	if len(res) != 1 || len(res[0]) > 8192+64 || !strings.Contains(res[0], "truncated") {
		t.Fatalf("call_tool result length %d, want cut near 8192", len(res[0]))
	}
}

// ---- criterion 10: pinned ----

func TestToolSearch_Pinned(t *testing.T) {
	logs := captureAllLogs(t)
	env := newTSEnv(t, "",
		callReply("mail_send", `{"to":"a"}`),
		callReply("tool_search", `{"query":"tide times"}`),
		callReply("tool_search", `{"query":"mail inbox"}`),
		tsReply{text: "done"},
	)
	fake := testutil.NewFakeMCPClient(mainTools()...)
	cfg := chatJSON(t, `{"toolSearch":{"mode":"on","maxLoaded":1,"pinned":["mail_send","mail_nope"]}}`)
	run := env.start(t, newSess("ts5", writeProject(t, mainSkills())), cfg, fake)
	run.send(t)

	reqs := env.requests()
	if names := toolNames(t, decodeReq(t, reqs[0])); !reflect.DeepEqual(names, []string{"tool_search", "call_tool", "mail_send", "clock_now"}) {
		t.Fatalf("tools = %v, want pinned mail_send among the visible tools", names)
	}
	if got, ok := lastCallArgs(t, fake, "mail_send"); !ok || got != `{"to":"a"}` {
		t.Fatalf("pinned tool not directly callable: %q %v", got, ok)
	}
	// maxLoaded is 1 and the pinned tool must not use it up.
	first := decodeReq(t, reqs[2]).toolResults()
	if !strings.Contains(first[len(first)-1], `"tides_lookup"`) || strings.Contains(first[len(first)-1], "limit reached") {
		t.Fatalf("pinned tool counted against the cap: %s", first[len(first)-1])
	}
	second := decodeReq(t, reqs[3]).toolResults()
	if last := second[len(second)-1]; !strings.Contains(last, "mail_send") || !strings.Contains(last, "alreadyLoaded") {
		t.Fatalf("pinned tool not reported as already loaded: %s", last)
	}
	if !strings.Contains(logs.all(), "mail_nope") {
		t.Fatalf("no warning for an unknown pinned tool; logs:\n%s", logs.all())
	}
}

// ---- search result limits ----

func bigTools(n, size int) []testutil.FakeTool {
	var out []testutil.FakeTool
	for i := 0; i < n; i++ {
		out = append(out, okTool(fmt.Sprintf("big_%d", i), "handles widget", map[string]any{
			"type": "object", "description": strings.Repeat("p", size)}))
	}
	return out
}

func bigSkillProject(t *testing.T, n int) string {
	var names []string
	for i := 0; i < n; i++ {
		names = append(names, fmt.Sprintf("big_%d", i))
	}
	return writeProject(t, map[string]string{"relay-big": skillDoc("relay-big", "Widget tools", "widget", names...)})
}

func TestToolSearch_ResultSizeAndLoadedLimit(t *testing.T) {
	t.Run("over 16KiB drops whole trailing entries and stays JSON", func(t *testing.T) {
		env := newTSEnv(t, "", callReply("tool_search", `{"query":"widget"}`), tsReply{text: "done"})
		run := env.start(t, newSess("ts6", bigSkillProject(t, 5)), chatJSON(t, onCfg), testutil.NewFakeMCPClient(bigTools(5, 5000)...))
		run.send(t)
		res := decodeReq(t, env.requests()[1]).toolResults()[0]
		var out struct {
			Loaded []struct{ Name string } `json:"loaded"`
		}
		if err := json.Unmarshal([]byte(res), &out); err != nil {
			t.Fatalf("result cut mid-JSON: %v", err)
		}
		if len(res) > 16<<10 || len(out.Loaded) < 1 || len(out.Loaded) >= 5 {
			t.Fatalf("result %d bytes with %d entries, want <=16384 bytes and 1..4 entries", len(res), len(out.Loaded))
		}
	})
	t.Run("one oversize entry is kept", func(t *testing.T) {
		env := newTSEnv(t, "", callReply("tool_search", `{"query":"widget"}`), tsReply{text: "done"})
		run := env.start(t, newSess("ts7", bigSkillProject(t, 1)), chatJSON(t, onCfg), testutil.NewFakeMCPClient(bigTools(1, 20000)...))
		run.send(t)
		res := decodeReq(t, env.requests()[1]).toolResults()[0]
		var out struct {
			Loaded []struct{ Name string } `json:"loaded"`
		}
		if err := json.Unmarshal([]byte(res), &out); err != nil || len(out.Loaded) != 1 {
			t.Fatalf("want one valid entry, err=%v result=%.200s", err, res)
		}
	})
	t.Run("at most 5 per search", func(t *testing.T) {
		env := newTSEnv(t, "", callReply("tool_search", `{"query":"widget"}`), tsReply{text: "done"})
		run := env.start(t, newSess("ts8", bigSkillProject(t, 8)), chatJSON(t, onCfg), testutil.NewFakeMCPClient(bigTools(8, 10)...))
		run.send(t)
		var out struct {
			Loaded []struct{ Name string } `json:"loaded"`
		}
		_ = json.Unmarshal([]byte(decodeReq(t, env.requests()[1]).toolResults()[0]), &out)
		if len(out.Loaded) != 5 {
			t.Fatalf("loaded %d, want 5", len(out.Loaded))
		}
	})
	t.Run("loaded limit, no match and missing query are errors", func(t *testing.T) {
		env := newTSEnv(t, "",
			callReply("tool_search", `{"query":"qqqqqq"}`),
			callReply("tool_search", `{"query":""}`),
			callReply("tool_search", `{"query":"tide"}`),
			callReply("tool_search", `{"query":"mail"}`),
			tsReply{text: "done"})
		cfg := chatJSON(t, `{"toolSearch":{"mode":"on","maxLoaded":1}}`)
		run := env.start(t, newSess("ts9", writeProject(t, mainSkills())), cfg, testutil.NewFakeMCPClient(mainTools()...))
		run.send(t)
		res := decodeReq(t, env.requests()[4]).toolResults()
		want := []string{"no hidden tool matches", "query is required", `"tides_lookup"`, "loaded-tool limit reached (1); use a loaded tool"}
		if len(res) != 4 {
			t.Fatalf("got %d results", len(res))
		}
		for i, w := range want {
			if !strings.Contains(res[i], w) {
				t.Errorf("result %d = %s, want it to contain %q", i, res[i], w)
			}
		}
	})
}

// ---- criterion 9: activation ----

func TestToolSearch_AutoActivationFollowsContextLength(t *testing.T) {
	cases := []struct {
		name      string
		toolBytes int
		models    string
		active    bool
	}{
		{"large window, big tools: below threshold", 40_000, `{"data":[{"id":"sonnet","context_length":1000000}]}`, false},
		{"small window, big tools", 40_000, `{"data":[{"id":"sonnet","context_length":20000}]}`, true},
		{"small tools, row gives small window", 4_000, `{"data":[{"id":"sonnet","context_length":5000}]}`, true},
		{"small tools, no context_length falls back to 32768", 4_000, `{"data":[{"id":"sonnet"}]}`, false},
		{"big tools, no context_length falls back to 32768", 40_000, `{"data":[{"id":"sonnet"}]}`, true},
		{"other model's row ignored", 4_000, `{"data":[{"id":"other","context_length":5000},{"id":"sonnet"}]}`, false},
		{"garbled body falls back and Start still succeeds", 4_000, `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTSEnv(t, tc.models)
			tools := mainTools()
			tools[3] = okTool("clock_now", strings.Repeat("d", tc.toolBytes), map[string]any{"type": "object"})
			run := env.start(t, newSess("ts10", writeProject(t, mainSkills())), chatJSON(t, `{}`), testutil.NewFakeMCPClient(tools...))
			run.send(t)
			names := toolNames(t, decodeReq(t, env.requests()[0]))
			if active := len(names) > 0 && names[0] == "tool_search"; active != tc.active {
				t.Fatalf("active = %v, want %v (tools %v)", active, tc.active, names)
			}
		})
	}
}

func TestToolSearch_InactiveCases(t *testing.T) {
	collide := func(name string) []testutil.FakeTool { return append(mainTools(), okTool(name, "x", nil)) }
	cases := []struct {
		name  string
		tools []testutil.FakeTool
		proj  map[string]string
		host  bool
		warn  string
	}{
		{"tool named tool_search", collide("tool_search"), mainSkills(), false, "tool_search"},
		{"tool named call_tool", collide("call_tool"), mainSkills(), false, "call_tool"},
		{"no MCP tools", nil, mainSkills(), false, ""},
		{"host session", mainTools(), mainSkills(), true, ""},
		{"no skill covers a live tool", mainTools(), map[string]string{"s": skillDoc("s", "d", "", "ghost_tool")}, false, ""},
		{"no skills at all", mainTools(), nil, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureAllLogs(t)
			env := newTSEnv(t, "")
			sess := newSess("ts11", writeProject(t, tc.proj))
			if tc.host {
				sess.Host = &sessionstypes.HostSpec{ID: "h1", Name: "testbox", SSHArgv: []string{"/usr/bin/true"}}
			}
			fake := testutil.NewFakeMCPClient(tc.tools...)
			run := env.start(t, sess, chatJSON(t, onCfg), fake)
			run.send(t)
			req := decodeReq(t, env.requests()[0])
			if names := toolNames(t, req); len(names) > 0 && names[0] == "tool_search" {
				t.Fatalf("tool search active, tools=%v", names)
			}
			if got, want := len(toolNames(t, req)), len(tc.tools); got != want {
				t.Fatalf("sent %d tools, want all %d", got, want)
			}
			if strings.Contains(req.system(), "## Hidden tools") {
				t.Fatal("index block present while inactive")
			}
			if tc.warn != "" && !strings.Contains(logs.all(), tc.warn) {
				t.Fatalf("no warning naming %s:\n%s", tc.warn, logs.all())
			}
		})
	}
}

// ---- config and logging ----

func TestToolSearch_InvalidConfigTurnsOffWithOneWarning(t *testing.T) {
	for _, body := range []string{`{"toolSearch":{"mode":"sometimes"}}`, `{"toolSearch":`} {
		logs := captureAllLogs(t)
		env := newTSEnv(t, "")
		cfg := chatJSON(t, body)
		run := env.start(t, newSess("ts12", writeProject(t, mainSkills())), cfg, testutil.NewFakeMCPClient(mainTools()...))
		run.send(t)
		if n := len(toolNames(t, decodeReq(t, env.requests()[0]))); n != 4 {
			t.Fatalf("%s: sent %d tools, want all 4", body, n)
		}
		var warns []string
		for _, l := range strings.Split(logs.all(), "\n") {
			if strings.Contains(l, "level=WARN") && strings.Contains(l, cfg) {
				warns = append(warns, l)
			}
		}
		if len(warns) != 1 {
			t.Fatalf("%s: %d warnings naming the file, want 1:\n%s", body, len(warns), logs.all())
		}
		if strings.Contains(body, "mode") && !strings.Contains(warns[0], "mode") {
			t.Fatalf("warning does not name the field: %s", warns[0])
		}
	}
}

func TestToolSearch_LogsStartAndQueryWithoutQueryText(t *testing.T) {
	logs := captureAllLogs(t)
	env := newTSEnv(t, "", callReply("tool_search", `{"query":"secret-lighthouse tide times"}`), tsReply{text: "done"})
	run := env.start(t, newSess("ts13", writeProject(t, mainSkills())), chatJSON(t, onCfg), testutil.NewFakeMCPClient(mainTools()...))
	run.send(t)

	all := logs.all()
	var start, query string
	for _, l := range strings.Split(all, "\n") {
		switch {
		case strings.Contains(l, "op=chat.tool_search.query"):
			query = l
		case strings.Contains(l, "op=chat.tool_search"):
			start = l
		}
	}
	for _, w := range []string{"session_id=ts13", "mode=on", "active=true", "reason=on", "skills=2", "tools_total=4", "tools_sent=3", "hidden=3", "pinned=0", "tool_tokens=", "context_tokens="} {
		if !strings.Contains(start, w) {
			t.Errorf("start line lacks %s: %s", w, start)
		}
	}
	for _, w := range []string{"session_id=ts13", "returned=", "loaded_total=", "tides_lookup"} {
		if !strings.Contains(query, w) {
			t.Errorf("query line lacks %s: %s", w, query)
		}
	}
	if strings.Contains(all, "secret-lighthouse") {
		t.Fatal("query text appears in the logs")
	}
}

func TestToolSearch_DeniedCallLogged(t *testing.T) {
	logs := captureAllLogs(t)
	env := newTSEnv(t, "", callReply("call_tool", `{"name":"tides_lookup","arguments":{}}`), tsReply{text: "done"})
	run := env.start(t, newSess("ts14", writeProject(t, mainSkills())), chatJSON(t, onCfg), testutil.NewFakeMCPClient(mainTools()...))
	run.send(t)
	var line string
	for _, l := range strings.Split(logs.all(), "\n") {
		if strings.Contains(l, "op=chat.call_tool") {
			line = l
		}
	}
	for _, w := range []string{"level=WARN", "status=denied", "not_loaded"} {
		if !strings.Contains(line, w) {
			t.Fatalf("denied line lacks %s: %q", w, line)
		}
	}
}

func captureAllLogs(t *testing.T) *warnBuffer {
	t.Helper()
	b := &warnBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return b
}

// ---- criterion 8: per-directory index ----

func TestToolSearch_EachSessionGetsOnlyItsOwnDirectorysSkills(t *testing.T) {
	dirA := writeProject(t, map[string]string{"relay-mail": skillDoc("relay-mail", "Mail things", "", "mail_send")})
	dirB := writeProject(t, map[string]string{"relay-tides": skillDoc("relay-tides", "Tide things", "", "tides_lookup")})
	cfg := chatJSON(t, onCfg)
	var systems [2]string
	for i, dir := range []string{dirA, dirB} {
		env := newTSEnv(t, "")
		run := env.start(t, newSess(fmt.Sprintf("ts-dir%d", i), dir), cfg, testutil.NewFakeMCPClient(mainTools()...))
		run.send(t)
		systems[i] = decodeReq(t, env.requests()[0]).system()
	}
	if !strings.Contains(systems[0], "- relay-mail: Mail things") || strings.Contains(systems[0], "relay-tides") {
		t.Fatalf("session A index wrong: %q", systems[0])
	}
	if !strings.Contains(systems[1], "- relay-tides: Tide things") || strings.Contains(systems[1], "relay-mail") {
		t.Fatalf("session B index wrong: %q", systems[1])
	}
}

// ---- criterion 7 (provider side): record, commit rules, restore ----

func TestToolSearch_StateCommitRestoreAndDrop(t *testing.T) {
	cfg := chatJSON(t, onCfg)
	proj := writeProject(t, mainSkills())

	t.Run("nothing loaded stays empty object", func(t *testing.T) {
		env := newTSEnv(t, "")
		run := env.start(t, newSess("st1", proj), cfg, testutil.NewFakeMCPClient(mainTools()...))
		run.send(t)
		if got := string(run.p.GetState()); got != `{}` {
			t.Fatalf("GetState = %s, want {}", got)
		}
	})
	t.Run("committed turn records the load", func(t *testing.T) {
		env := newTSEnv(t, "", callReply("tool_search", `{"query":"tide"}`), tsReply{text: "done"})
		run := env.start(t, newSess("st2", proj), cfg, testutil.NewFakeMCPClient(mainTools()...))
		run.send(t)
		if got := string(run.p.GetState()); got != `{"toolSearch":{"loaded":["tides_lookup"]}}` {
			t.Fatalf("GetState = %s", got)
		}
	})
	t.Run("errored turn commits nothing", func(t *testing.T) {
		env := newTSEnv(t, "", callReply("tool_search", `{"query":"tide"}`), tsReply{status: 500})
		run := env.start(t, newSess("st3", proj), cfg, testutil.NewFakeMCPClient(mainTools()...))
		if err := run.p.SendMessage("go", nil); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(5 * time.Second)
		for done := false; !done; {
			select {
			case ev := <-run.evCh:
				done = ev == "error"
			case <-deadline:
				t.Fatal("no error event")
			}
		}
		if got := string(run.p.GetState()); got != `{}` {
			t.Fatalf("GetState after errored turn = %s, want {}", got)
		}
	})
	t.Run("restored loads are callable without a search; missing names dropped", func(t *testing.T) {
		env := newTSEnv(t, "", callReply("tides_lookup", `{"port":"p1"}`), tsReply{text: "done"})
		fake := testutil.NewFakeMCPClient(mainTools()...)
		evCh := make(chan string, 1024)
		p := NewChatProvider(newSess("st4", proj), func(ev string, _ json.RawMessage) { evCh <- ev },
			ChatConfig{ModelSocket: env.sock, ModelKey: "test-key", ToolSearchConfigPath: cfg})
		p.SetMCPClient(fake)
		p.RestoreState(json.RawMessage(`{"toolSearch":{"loaded":["tides_lookup","ghost_tool"]}}`))
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		defer p.Kill()
		if got := string(p.GetState()); got != `{"toolSearch":{"loaded":["tides_lookup"]}}` {
			t.Fatalf("GetState after Start = %s, want the ghost dropped", got)
		}
		(&tsRun{p: p, evCh: evCh}).send(t)
		if _, ok := lastCallArgs(t, fake, "tides_lookup"); !ok {
			t.Fatal("restored tool was not callable without a new search")
		}
	})
}

// ---- criterion 12: 48-tool fixture ----

func TestToolSearch_FortyEightToolsOnRequestIsSmallerThanOff(t *testing.T) {
	var tools []testutil.FakeTool
	skills := map[string]string{}
	for i := 0; i < 48; i++ {
		name := fmt.Sprintf("dom%02d_act", i)
		tools = append(tools, okTool(name, strings.Repeat(fmt.Sprintf("Use when the user asks for domain %02d things. ", i), 5), map[string]any{
			"type": "object", "properties": map[string]any{
				"a": map[string]any{"type": "string", "description": "first described property for " + name},
				"b": map[string]any{"type": "string", "description": "second described property for " + name},
				"c": map[string]any{"type": "string", "description": "third described property for " + name},
			}}))
		skills[fmt.Sprintf("relay-dom%02d", i)] = skillDoc(fmt.Sprintf("relay-dom%02d", i), fmt.Sprintf("Domain %02d helper", i), "", name)
	}
	proj := writeProject(t, skills)

	size := func(cfgBody string) int {
		env := newTSEnv(t, "")
		run := env.start(t, newSess("ts48", proj), chatJSON(t, cfgBody), testutil.NewFakeMCPClient(tools...))
		run.send(t)
		return len(env.requests()[0])
	}
	off, on := size(`{"toolSearch":{"mode":"off"}}`), size(onCfg)
	if on >= off {
		t.Fatalf("request body with tool search on = %d bytes, off = %d; want on < off", on, off)
	}
}
