package provider

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/events"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const wantStallText = "model stream stalled: no data for 1s"

// shortenStreamIdle drops the idle bound to 1s for one test.
func shortenStreamIdle(t *testing.T) {
	t.Helper()
	prev := chatStreamIdleTimeout
	chatStreamIdleTimeout = 1 * time.Second
	t.Cleanup(func() { chatStreamIdleTimeout = prev })
}

type stallRecorder struct {
	types chan string // every event type, in order
	errs  chan string // data of each "error" event
}

// startStallChat starts a chat provider against a fake model.sock serving
// stream for every chat completion request.
func startStallChat(t *testing.T, stream http.HandlerFunc, withEchoTool bool) (*ChatProvider, *sessionstypes.Session, *stallRecorder) {
	t.Helper()
	sock := filepath.Join(shortTempDir(t), "model.sock")
	closingBroker(t, sock, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			return
		}
		stream(w, r)
	})

	rec := &stallRecorder{types: make(chan string, 256), errs: make(chan string, 16)}
	sess := &sessionstypes.Session{ID: "s1", Model: "sonnet"}
	p := NewChatProvider(sess, func(eventType string, data json.RawMessage) {
		if eventType == "error" {
			rec.errs <- string(data)
		}
		rec.types <- eventType
	}, ChatConfig{ModelSocket: sock, ModelKey: "test-key"})
	if withEchoTool {
		p.SetMCPClient(testutil.NewFakeMCPClient(testutil.FakeTool{
			Name:    "echo",
			Handler: func(args json.RawMessage) (string, error) { return "ok:" + string(args), nil },
		}))
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(p.Kill)
	if err := p.SendMessage("hi", nil); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	return p, sess, rec
}

// closingBroker serves handler on sock like fakeBroker, but its cleanup
// returns only once every connection is closed on both ends and every
// handler has returned. Server.Close alone closes the server end, and the
// client transport closes its pooled end later on its own goroutine; that
// late close lands inside whichever test runs next (the fd-leak tests count
// open fds). Deliberate order: stop accepting, half-close each connection so
// the client sees EOF and closes its end, wait for the server to see that
// close, then close the server.
func closingBroker(t *testing.T, sock string, handler http.HandlerFunc) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
		open  sync.WaitGroup
	)
	srv := &http.Server{Handler: handler, ConnState: func(c net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Done()
		}
	}}
	served := make(chan struct{})
	go func() { _ = srv.Serve(ln); close(served) }()
	t.Cleanup(func() {
		_ = ln.Close()
		<-served // no StateNew after this, so open's count is final
		mu.Lock()
		for _, c := range conns {
			if hc, ok := c.(interface{ CloseWrite() error }); ok {
				_ = hc.CloseWrite()
			}
		}
		mu.Unlock()
		closed := make(chan struct{})
		go func() { open.Wait(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Errorf("fake broker: connections still open 5s after the provider was killed")
		}
		_ = srv.Close()
	})
}

func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
}

func sseChunk(w http.ResponseWriter, payload string) {
	fmt.Fprint(w, "data: "+payload+"\n\n")
	w.(http.Flusher).Flush()
}

func requireStallError(t *testing.T, rec *stallRecorder) {
	t.Helper()
	select {
	case data := <-rec.errs:
		if !strings.Contains(data, wantStallText) {
			t.Fatalf("error event data = %s, want it to contain %q", data, wantStallText)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no error event within 3s of a stalled stream, want one containing %q", wantStallText)
	}
}

func TestChatStreamIdle_StallAfterToolCallEndsTurnWithError(t *testing.T) {
	shortenStreamIdle(t)
	var turn atomic.Int32
	reqDone := make(chan struct{})
	_, _, rec := startStallChat(t, func(w http.ResponseWriter, r *http.Request) {
		sseHeaders(w)
		if turn.Add(1) == 1 {
			sseChunk(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"echo","arguments":"{}"}}]}}]}`)
			sseChunk(w, "[DONE]")
			return
		}
		sseChunk(w, `{"choices":[{"delta":{"content":"partial"}}]}`)
		<-r.Context().Done()
		close(reqDone)
	}, true)

	requireStallError(t, rec)

	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stalled upstream request was not aborted within 2s of the error")
	}
	if turn.Load() != 2 {
		t.Fatalf("model requests = %d, want 2 (tool turn, then stalled turn)", turn.Load())
	}
	for len(rec.types) > 0 {
		if ev := <-rec.types; ev == events.HandlerMessageComplete {
			t.Fatal("stalled turn emitted message_complete, want only the error")
		}
	}
}

func TestChatStreamIdle_HeadersThenSilenceEndsTurnWithError(t *testing.T) {
	shortenStreamIdle(t)
	_, _, rec := startStallChat(t, func(w http.ResponseWriter, r *http.Request) {
		sseHeaders(w)
		<-r.Context().Done()
	}, false)

	requireStallError(t, rec)
}

func TestChatStreamIdle_SlowSteadyStreamCompletesIntact(t *testing.T) {
	shortenStreamIdle(t)
	var want strings.Builder
	parts := []string{}
	for i := 0; i*400 < 3000; i++ {
		parts = append(parts, fmt.Sprintf("p%d ", i))
		want.WriteString(parts[i])
	}
	_, sess, rec := startStallChat(t, func(w http.ResponseWriter, r *http.Request) {
		sseHeaders(w)
		for i, part := range parts {
			if i > 0 {
				time.Sleep(400 * time.Millisecond)
			}
			sseChunk(w, fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, part))
		}
		sseChunk(w, "[DONE]")
	}, false)

	collectUntilComplete(t, rec.types)
	select {
	case data := <-rec.errs:
		t.Fatalf("steady stream emitted error %s, want none", data)
	default:
	}

	sess.Lock()
	msgs := append([]sessionstypes.Message(nil), sess.Messages...)
	sess.Unlock()
	if len(msgs) != 1 {
		t.Fatalf("session.Messages = %+v, want one assistant message", msgs)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msgs[0].Content, &blocks); err != nil {
		t.Fatalf("decode assistant content %s: %v", msgs[0].Content, err)
	}
	if len(blocks) != 1 || blocks[0].Text != want.String() {
		t.Fatalf("assistant blocks = %+v, want one text block %q", blocks, want.String())
	}
}

func TestChatStreamIdle_StopDuringStallEmitsNoError(t *testing.T) {
	shortenStreamIdle(t)
	stalled := make(chan struct{}, 1)
	p, _, rec := startStallChat(t, func(w http.ResponseWriter, r *http.Request) {
		sseHeaders(w)
		stalled <- struct{}{}
		<-r.Context().Done()
	}, false)

	select {
	case <-stalled:
	case <-time.After(3 * time.Second):
		t.Fatal("model request never reached the fake broker")
	}
	time.Sleep(300 * time.Millisecond)
	p.StopGeneration()

	select {
	case data := <-rec.errs:
		t.Fatalf("stopped turn emitted error %s, want none", data)
	case <-time.After(2 * time.Second):
	}
}
