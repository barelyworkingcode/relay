package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestChatHTTPTransport_PostChat_SendsBearerModelKey is the required
// "fake model.sock gets Bearer rmk_..." test: the transport's only
// credential is the session's model key, sent as an Authorization bearer.
func TestChatHTTPTransport_PostChat_SendsBearerModelKey(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "model.sock")

	const key = "rmk_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var gotAuth string
	fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	transport := newChatHTTPTransport(ChatConfig{ModelSocket: sock, ModelKey: key}, "sonnet", nil)
	resp, err := transport.PostChat(context.Background(), []map[string]any{{"role": "user", "content": "hi"}}, nil)
	if err != nil {
		t.Fatalf("PostChat: %v", err)
	}
	resp.Body.Close()

	if want := "Bearer " + key; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

// TestChatHTTPTransport_DialsProductionSocket confirms that with no dial
// override, the transport reaches exactly ModelSocket.
func TestChatHTTPTransport_DialsProductionSocket(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "model.sock")

	var hit atomic.Bool
	fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	transport := newChatHTTPTransport(ChatConfig{ModelSocket: sock}, "sonnet", nil)
	if err := transport.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !hit.Load() {
		t.Fatal("request never reached the broker over ModelSocket")
	}
}

// TestChatHTTPTransport_NoDirectEndpointDial is the required "no direct
// endpoint dial (dialer seam)" test. ModelSocket is set to a path nothing
// ever listens on; only the injected dial seam's target is live. If the
// transport dialed anything other than the seam -- ModelSocket directly, or
// some other endpoint -- this request would fail to connect. It succeeds
// and reaches the fake broker, proving every request funnels through the
// one seam and nothing else.
func TestChatHTTPTransport_NoDirectEndpointDial(t *testing.T) {
	dir := shortTempDir(t)
	neverListened := filepath.Join(dir, "unreachable.sock")
	fakeSock := filepath.Join(dir, "fake-broker.sock")

	var hit atomic.Bool
	fakeBroker(t, fakeSock, func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		w.WriteHeader(http.StatusOK)
	})

	cfg := ChatConfig{ModelSocket: neverListened}
	cfg.dial = unixDialer(fakeSock)

	transport := newChatHTTPTransport(cfg, "sonnet", nil)
	if err := transport.Ping(context.Background()); err != nil {
		t.Fatalf("Ping via overridden dial seam: %v", err)
	}
	if !hit.Load() {
		t.Fatal("request did not reach the fake broker through the overridden dial seam")
	}
}

// TestChatHTTPTransport_ManagedAliasPrefixStripped is the required
// "llama/a sends model a" test.
func TestChatHTTPTransport_ManagedAliasPrefixStripped(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"llama/a", "a"},
		{"mlx/a", "a"},
		{"sonnet", "sonnet"},
		{"a", "a"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			dir := shortTempDir(t)
			sock := filepath.Join(dir, "model.sock")

			var gotModel string
			fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				gotModel = body.Model
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			})

			transport := newChatHTTPTransport(ChatConfig{ModelSocket: sock}, tc.model, nil)
			resp, err := transport.PostChat(context.Background(), nil, nil)
			if err != nil {
				t.Fatalf("PostChat: %v", err)
			}
			resp.Body.Close()

			if gotModel != tc.want {
				t.Errorf("model = %q, want %q", gotModel, tc.want)
			}
		})
	}
}

// TestChatHTTPTransport_StreamChunks_TextAndUsage exercises the SSE parser
// against a small, realistic OpenAI-shaped stream.
func TestChatHTTPTransport_StreamChunks_TextAndUsage(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "model.sock")

	fakeBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"hel"}}]}`+"\n\n")
		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"lo"}}]}`+"\n\n")
		fmt.Fprint(w, "data: "+`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	transport := newChatHTTPTransport(ChatConfig{ModelSocket: sock}, "sonnet", nil)
	resp, err := transport.PostChat(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("PostChat: %v", err)
	}

	var got strings.Builder
	result := transport.StreamChunks(resp, time.Now(), func(d ChatDelta) {
		got.WriteString(d.Text)
	})
	if got.String() != "hello" {
		t.Errorf("streamed text = %q, want %q", got.String(), "hello")
	}
	if result.Stats.InputTokens != 3 || result.Stats.OutputTokens != 2 {
		t.Errorf("stats = %+v", result.Stats)
	}
}
