package session_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

type resumeBroker struct {
	sock   string
	mu     sync.Mutex
	bodies []string
	script []string // per chat request: a tool name to call (args fixed), or "" for text
}

func newResumeBroker(t *testing.T, script ...string) *resumeBroker {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rh-resume-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	b := &resumeBroker{sock: filepath.Join(dir, "m.sock"), script: script}
	ln, err := net.Listen("unix", b.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"sonnet"}]}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, string(body))
		idx := len(b.bodies) - 1
		b.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx < len(b.script) && b.script[idx] != "" {
			args := `{"query":"tide"}`
			if b.script[idx] != "tool_search" {
				args = `{"port":"p1"}`
			}
			a, _ := json.Marshal(args)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c%d\",\"function\":{\"name\":%q,\"arguments\":%s}}]}}]}\n\n", idx, b.script[idx], a)
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return b
}

func (b *resumeBroker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.bodies)
}

func (b *resumeBroker) body(i int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bodies[i]
}

func resumeFixture(t *testing.T) (proj, cfg string, tools []testutil.FakeTool) {
	proj = t.TempDir()
	d := filepath.Join(proj, ".claude", "skills", "relay-tides")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: relay-tides\ndescription: Tide times\nkeywords: [tides]\n---\n## Tools\n- **tides_lookup** — tide\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg = filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(cfg, []byte(`{"toolSearch":{"mode":"on"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tools = []testutil.FakeTool{
		{Name: "tides_lookup", Description: "Look up tides", Schema: map[string]any{"type": "object"},
			Handler: func(json.RawMessage) (string, error) { return "TIDE-1", nil }},
		{Name: "clock_now", Description: "Time", Schema: map[string]any{"type": "object"},
			Handler: func(json.RawMessage) (string, error) { return "now", nil }},
	}
	return
}

func startChat(t *testing.T, b *resumeBroker, sess *sessionstypes.Session, cfg string, fake *testutil.FakeMCPClient, restore json.RawMessage) (*provider.ChatProvider, chan string) {
	t.Helper()
	evCh := make(chan string, 1024)
	p := provider.NewChatProvider(sess, func(ev string, _ json.RawMessage) { evCh <- ev },
		provider.ChatConfig{ModelSocket: b.sock, ModelKey: "test-key", ToolSearchConfigPath: cfg})
	p.SetMCPClient(fake)
	if restore != nil {
		p.RestoreState(restore)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(p.Kill)
	return p, evCh
}

func sendAndWait(t *testing.T, p *provider.ChatProvider, evCh chan string) {
	t.Helper()
	if err := p.SendMessage("go", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-evCh:
			if ev == events.HandlerMessageComplete {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for message_complete")
		}
	}
}

// Criterion 7: a load survives the real Store round trip, replays in history,
// and the resumed provider can call the loaded tool without a new search.
func TestToolSearch_LoadsSurviveStoreResume(t *testing.T) {
	proj, cfg, tools := resumeFixture(t)
	store := session.NewStore(t.TempDir())
	const id = "22222222-2222-2222-2222-222222222222"

	b1 := newResumeBroker(t, "tool_search", "")
	sess := &sessionstypes.Session{ID: id, Model: "sonnet", Directory: proj, ProviderType: "chat"}
	p, ev := startChat(t, b1, sess, cfg, testutil.NewFakeMCPClient(tools...), nil)
	sendAndWait(t, p, ev)

	sess.Lock()
	sess.ProviderState = p.GetState()
	sess.Unlock()
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var ps struct {
		ToolSearch struct {
			Loaded []string `json:"loaded"`
		} `json:"toolSearch"`
	}
	if err := json.Unmarshal(loaded.ProviderState, &ps); err != nil || len(ps.ToolSearch.Loaded) != 1 || ps.ToolSearch.Loaded[0] != "tides_lookup" {
		t.Fatalf("persisted providerState = %s (err=%v)", loaded.ProviderState, err)
	}
	var sawSearch bool
	for _, m := range loaded.Messages {
		if m.Role == "tool" && m.ToolName == "tool_search" {
			sawSearch = true
		}
	}
	if !sawSearch {
		t.Fatalf("tool_search result not in persisted messages: %+v", loaded.Messages)
	}

	// Resume: new provider, restored state, replayed history.
	b2 := newResumeBroker(t, "tides_lookup", "")
	fake2 := testutil.NewFakeMCPClient(tools...)
	p2, ev2 := startChat(t, b2, loaded, cfg, fake2, loaded.ProviderState)
	if got := string(p2.GetState()); got != `{"toolSearch":{"loaded":["tides_lookup"]}}` {
		t.Fatalf("GetState after resume = %s", got)
	}
	sendAndWait(t, p2, ev2)
	if calls := fake2.Calls(); len(calls) != 1 || calls[0].Name != "tides_lookup" {
		t.Fatalf("resumed MCP calls = %+v, want one tides_lookup with no new search", calls)
	}
	if first := b2.body(0); !strings.Contains(first, `"role":"tool"`) || !strings.Contains(first, `alreadyLoaded`) {
		t.Fatalf("resumed request lacks the replayed tool_search result: %.600s", first)
	}
}
