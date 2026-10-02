package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

func collectUntilComplete(t *testing.T, evCh <-chan string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-evCh:
			if e == events.HandlerMessageComplete {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for message_complete")
		}
	}
}

// TestChatProvider_SendMessage_TextRoundTrip drives a full SendMessage
// through a fake model.sock with no tool calls, and confirms the turn is
// persisted to session.Messages.
func TestChatProvider_SendMessage_TextRoundTrip(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "model.sock")

	fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" { // Start's Ping
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"hello"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	sess := &sessionstypes.Session{ID: "s1", Model: "sonnet"}
	evCh := make(chan string, 32)
	handler := func(eventType string, _ json.RawMessage) { evCh <- eventType }

	p := NewChatProvider(sess, handler, ChatConfig{ModelSocket: sock, ModelKey: "test-key"})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Kill()

	if err := p.SendMessage("hi", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	collectUntilComplete(t, evCh)

	sess.Lock()
	msgs := append([]sessionstypes.Message(nil), sess.Messages...)
	sess.Unlock()

	if len(msgs) != 1 || msgs[0].Role != "assistant" {
		t.Fatalf("session.Messages = %+v, want exactly one assistant message", msgs)
	}
}

// TestChatProvider_ToolCallRoundTrip drives a turn where the model calls an
// MCP tool before producing its final answer, using testutil.FakeMCPClient
// as the test-only substitute for a real MCP connection (SetMCPClient).
func TestChatProvider_ToolCallRoundTrip(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "model.sock")

	turn := 0
	fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			return
		}
		turn++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if turn == 1 {
			fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"echo","arguments":""}}]}}]}`+"\n\n")
			fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"done"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	sess := &sessionstypes.Session{ID: "s1", Model: "sonnet"}
	evCh := make(chan string, 64)
	handler := func(eventType string, _ json.RawMessage) { evCh <- eventType }

	p := NewChatProvider(sess, handler, ChatConfig{ModelSocket: sock, ModelKey: "test-key"})
	fake := testutil.NewFakeMCPClient(testutil.FakeTool{
		Name: "echo",
		Handler: func(args json.RawMessage) (string, error) {
			return "ok:" + string(args), nil
		},
	})
	p.SetMCPClient(fake)

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Kill()

	if err := p.SendMessage("hi", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	collectUntilComplete(t, evCh)

	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Name != "echo" {
		t.Fatalf("MCP calls = %+v, want exactly one call to echo", calls)
	}

	sess.Lock()
	msgs := append([]sessionstypes.Message(nil), sess.Messages...)
	sess.Unlock()

	var sawTool, sawAssistant bool
	for _, m := range msgs {
		switch m.Role {
		case "tool":
			sawTool = true
		case "assistant":
			sawAssistant = true
		}
	}
	if !sawTool || !sawAssistant {
		t.Fatalf("session.Messages = %+v, want at least one tool and one assistant message", msgs)
	}
}

// TestChatProvider_Kill_EmitsProcessExited covers the gap a chat session's
// Kill left open: unlike claude/pi, it has no underlying OS process and
// therefore no waitForExit goroutine of its own to fire process_exited —
// without Kill emitting it directly, ending a chat session this way never
// reaches session.Manager's exit handler at all, so relay never learns to
// revoke the session's launch identity, model key or sandbox profile.
func TestChatProvider_Kill_EmitsProcessExited(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "model.sock")
	fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	sess := &sessionstypes.Session{ID: "s1", Model: "sonnet"}
	evCh := make(chan string, 8)
	handler := func(eventType string, _ json.RawMessage) { evCh <- eventType }

	p := NewChatProvider(sess, handler, ChatConfig{ModelSocket: sock, ModelKey: "test-key"})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !p.Alive() {
		t.Fatal("provider should be alive after Start")
	}

	p.Kill()

	select {
	case ev := <-evCh:
		if ev != "process_exited" {
			t.Fatalf("event = %q, want process_exited", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Kill never emitted process_exited")
	}
	if p.Alive() {
		t.Fatal("provider should not be alive after Kill")
	}

	// A second Kill on an already-dead provider (StopAll and a redundant
	// /terminate can both reach here) must not emit a second event: nothing
	// downstream expects — or waits to consume — more than one exit report
	// per session.
	p.Kill()
	select {
	case ev := <-evCh:
		t.Fatalf("second Kill emitted %q, want no further event", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestChatProvider_ToolResultEvent_Flags checks the emitted tool_result JSON:
// is_error follows the MCP result, scope_violation appears only when true.
func TestChatProvider_ToolResultEvent_Flags(t *testing.T) {
	cases := []struct {
		name           string
		tool           testutil.FakeTool
		wantErr        bool
		wantScope      bool
		wantContentSub string
	}{
		{"success", testutil.FakeTool{}, false, false, "ok"},
		{"isError", testutil.FakeTool{IsError: true}, true, false, "ok"},
		{"scope refusal", testutil.FakeTool{IsError: true, ScopeViolation: true}, true, true, "ok"},
		{"call error", testutil.FakeTool{Handler: func(json.RawMessage) (string, error) { return "", fmt.Errorf("boom") }}, true, false, "Error: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortTempDir(t)
			sock := filepath.Join(dir, "model.sock")
			turn := 0
			fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					w.WriteHeader(http.StatusOK)
					return
				}
				turn++
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				if turn == 1 {
					fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"echo","arguments":"{}"}}]}}]}`+"\n\n")
					fmt.Fprint(w, "data: [DONE]\n\n")
					return
				}
				fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"done"}}]}`+"\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			})

			sess := &sessionstypes.Session{ID: "s1", Model: "sonnet"}
			evCh := make(chan string, 64)
			var mu sync.Mutex
			var toolResult json.RawMessage
			handler := func(eventType string, data json.RawMessage) {
				var probe struct {
					Subtype string `json:"subtype"`
				}
				if json.Unmarshal(data, &probe) == nil && probe.Subtype == events.ResultToolResultSubtype {
					mu.Lock()
					toolResult = append(json.RawMessage(nil), data...)
					mu.Unlock()
				}
				evCh <- eventType
			}
			p := NewChatProvider(sess, handler, ChatConfig{ModelSocket: sock, ModelKey: "test-key"})
			tool := tc.tool
			tool.Name = "echo"
			if tool.Handler == nil {
				tool.Handler = func(json.RawMessage) (string, error) { return "ok", nil }
			}
			p.SetMCPClient(testutil.NewFakeMCPClient(tool))
			if err := p.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer p.Kill()
			if err := p.SendMessage("hi", nil); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			collectUntilComplete(t, evCh)

			mu.Lock()
			raw := toolResult
			mu.Unlock()
			if raw == nil {
				t.Fatal("no tool_result event emitted")
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			if got, _ := fields["is_error"].(bool); got != tc.wantErr {
				t.Errorf("is_error = %v; want %v (%s)", fields["is_error"], tc.wantErr, raw)
			}
			sv, present := fields["scope_violation"]
			if present != tc.wantScope {
				t.Errorf("scope_violation present = %v; want %v (%s)", present, tc.wantScope, raw)
			}
			if present && sv != true {
				t.Errorf("scope_violation = %v; want true", sv)
			}
			if c, _ := fields["content"].(string); c != tc.wantContentSub {
				t.Errorf("content = %q; want %q", c, tc.wantContentSub)
			}
		})
	}
}
