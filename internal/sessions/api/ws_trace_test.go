package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const (
	traceCanaryPrompt = "CANARY-PROMPT-7731"
	traceCanaryFile   = "canary-attachment-9915.png"
)

var traceRequiredKeys = []string{"ts", "level", "msg", "service", "op", "status", "duration_ms", "error", "trace_id"}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// turnLines returns every chat.turn line captured so far, failing the test on
// any line that is not valid JSON carrying the nine required keys.
func (l *lockedBuf) turnLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", ln, err)
		}
		for _, k := range traceRequiredKeys {
			if _, ok := m[k]; !ok {
				t.Fatalf("log line missing required key %q: %s", k, ln)
			}
		}
		if m["op"] == "chat.turn" {
			out = append(out, m)
		}
	}
	return out
}

func captureLogs(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{DefaultService: "testsessions"})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// traceProvider reports its own exit from Kill through the manager's event
// handler, as the production claude and pi providers do.
type traceProvider struct {
	wsFakeProvider
	handler sessionstypes.EventHandler
	killed  chan struct{}
	once    sync.Once
}

func (p *traceProvider) Kill() {
	p.wsFakeProvider.Kill()
	p.handler("process_exited", json.RawMessage(`{"exitCode":0}`))
	p.once.Do(func() { close(p.killed) })
}

type traceEnv struct {
	conn interface {
		WriteJSON(any) error
	}
	sh   *SessionHandlers
	fp   *traceProvider
	id   string
	buf  *lockedBuf
	read func() map[string]any
}

func newTraceEnv(t *testing.T) *traceEnv {
	t.Helper()
	buf := captureLogs(t)
	hub, mgr, sh := newTestSessionSetup(t)
	fp := &traceProvider{killed: make(chan struct{})}
	mgr.SetProviderFactory(func(_ *sessionstypes.Session, _ session.CreateSpec, h sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		fp.handler = h
		return fp, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: wsTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	conn := dialHub(t, hub)
	return &traceEnv{
		conn: conn, sh: sh, fp: fp, id: sess.ID, buf: buf,
		read: func() map[string]any { return readJSONWithTimeout(t, conn, 2*time.Second) },
	}
}

func (e *traceEnv) send(t *testing.T, msg map[string]any) {
	t.Helper()
	if err := e.conn.WriteJSON(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// sendTurn starts a turn and waits until the provider has received it.
func (e *traceEnv) sendTurn(t *testing.T, traceID any) {
	t.Helper()
	msg := map[string]any{
		"type": "send_message", "sessionId": e.id, "text": traceCanaryPrompt,
		"files": []map[string]any{{"name": traceCanaryFile, "mimeType": "image/png", "data": "AAAA"}},
	}
	if traceID != nil {
		msg["trace_id"] = traceID
	}
	e.send(t, msg)
	testutil.WaitFor(t, 2*time.Second, func() bool {
		e.fp.mu.Lock()
		defer e.fp.mu.Unlock()
		return len(e.fp.sent) > 0
	})
}

func (e *traceEnv) waitTurnLines(t *testing.T, n int) []map[string]any {
	t.Helper()
	testutil.WaitFor(t, 2*time.Second, func() bool { return len(e.buf.turnLines(t)) >= n })
	return e.buf.turnLines(t)
}

func TestTrace_MessageComplete_LogsOneTurnWithCallerTrace(t *testing.T) {
	e := newTraceEnv(t)
	e.sendTurn(t, "caller-trace-0001")
	e.sh.SendToSession(e.id, map[string]any{"type": "message_complete", "sessionId": e.id})

	lines := e.waitTurnLines(t, 1)
	if len(lines) != 1 {
		t.Fatalf("chat.turn lines = %d, want 1: %s", len(lines), e.buf.String())
	}
	l := lines[0]
	if l["trace_id"] != "caller-trace-0001" || l["session_id"] != e.id {
		t.Fatalf("trace/session = %v/%v", l["trace_id"], l["session_id"])
	}
	if l["level"] != "info" || l["status"] != "ok" || l["error"] != "" {
		t.Fatalf("level/status/error = %v/%v/%v", l["level"], l["status"], l["error"])
	}
	if d, ok := l["duration_ms"].(float64); !ok || d < 0 {
		t.Fatalf("duration_ms = %v", l["duration_ms"])
	}

	e.sh.SendToSession(e.id, map[string]any{"type": "message_complete", "sessionId": e.id})
	if n := len(e.buf.turnLines(t)); n != 1 {
		t.Fatalf("a second message_complete logged another turn: %d lines", n)
	}
}

func TestTrace_InvalidOrMissingTraceID_IsReplaced(t *testing.T) {
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for name, in := range map[string]any{
		"invalid": "bad id\nINJECTED-LINE",
		"short":   "abc",
		"absent":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			e := newTraceEnv(t)
			e.sendTurn(t, in)
			e.sh.SendToSession(e.id, map[string]any{"type": "message_complete", "sessionId": e.id})
			lines := e.waitTurnLines(t, 1)
			got, _ := lines[0]["trace_id"].(string)
			if !hex32.MatchString(got) {
				t.Fatalf("trace_id = %q, want fresh 32-hex", got)
			}
			if s, ok := in.(string); ok && strings.Contains(e.buf.String(), s) {
				t.Fatalf("rejected trace_id %q appears in output: %s", s, e.buf.String())
			}
		})
	}
}

func TestTrace_ErrorEnds_LogAtErrorLevel(t *testing.T) {
	for _, tc := range []struct{ evType, code string }{
		{"error", "provider_error"},
		{"process_exited", "process_exited"},
	} {
		t.Run(tc.evType, func(t *testing.T) {
			e := newTraceEnv(t)
			e.sendTurn(t, "caller-trace-0002")
			e.sh.SendToSession(e.id, map[string]any{"type": tc.evType, "sessionId": e.id, "message": traceCanaryPrompt})
			lines := e.waitTurnLines(t, 1)
			l := lines[0]
			if len(lines) != 1 || l["level"] != "error" || l["status"] != "error" || l["error"] != tc.code ||
				l["trace_id"] != "caller-trace-0002" || l["session_id"] != e.id {
				t.Fatalf("unexpected turn lines: %s", e.buf.String())
			}
		})
	}
}

func TestTrace_RefusedSend_LogsOneWarn(t *testing.T) {
	cases := map[string]struct {
		sessionID func(e *traceEnv) string
		prime     bool
		code      string
	}{
		"unknown session":    {func(*traceEnv) string { return "no-such-session" }, false, "session_not_found"},
		"already processing": {func(e *traceEnv) string { return e.id }, true, "already_processing"},
		"missing sessionId":  {func(*traceEnv) string { return "" }, false, "session_id_required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newTraceEnv(t)
			if tc.prime {
				e.sendTurn(t, "caller-trace-0003")
			}
			e.send(t, map[string]any{
				"type": "send_message", "sessionId": tc.sessionID(e), "text": traceCanaryPrompt,
				"trace_id": "refused-trace-01",
			})
			if got := e.read(); got["type"] != "error" {
				t.Fatalf("reply = %v, want error frame", got)
			}
			lines := e.waitTurnLines(t, 1)
			if len(lines) != 1 {
				t.Fatalf("chat.turn lines = %d, want 1: %s", len(lines), e.buf.String())
			}
			l := lines[0]
			if l["level"] != "warn" || l["status"] != "error" || l["error"] != tc.code || l["trace_id"] != "refused-trace-01" {
				t.Fatalf("refusal line = %v", l)
			}
			if strings.Contains(e.buf.String(), traceCanaryPrompt) {
				t.Fatalf("prompt canary in output: %s", e.buf.String())
			}
		})
	}
}

func TestTrace_TurnDroppedSilently(t *testing.T) {
	drops := map[string]func(t *testing.T, e *traceEnv){
		"clear_messages": func(_ *testing.T, e *traceEnv) {
			e.sh.SendToSession(e.id, map[string]any{"type": "clear_messages", "sessionId": e.id})
		},
		"delete_session": func(t *testing.T, e *traceEnv) {
			e.send(t, map[string]any{"type": "delete_session", "sessionId": e.id})
			if got := e.read(); got["type"] != "session_ended" {
				t.Fatalf("reply = %v, want session_ended", got)
			}
			<-e.fp.killed
		},
		"end_session": func(t *testing.T, e *traceEnv) {
			e.send(t, map[string]any{"type": "end_session", "sessionId": e.id})
			select {
			case <-e.fp.killed:
			case <-time.After(2 * time.Second):
				t.Fatal("provider was not killed by end_session")
			}
		},
	}
	for name, drop := range drops {
		t.Run(name, func(t *testing.T) {
			e := newTraceEnv(t)
			e.sendTurn(t, "caller-trace-0004")
			drop(t, e)
			e.sh.SendToSession(e.id, map[string]any{"type": "message_complete", "sessionId": e.id})
			e.sh.SendToSession(e.id, map[string]any{"type": "error", "sessionId": e.id})
			if lines := e.buf.turnLines(t); len(lines) != 0 {
				t.Fatalf("dropped turn logged: %s", e.buf.String())
			}
		})
	}
}

func TestTrace_CanariesNeverLogged(t *testing.T) {
	e := newTraceEnv(t)
	e.sendTurn(t, "caller-trace-0005")
	e.sh.SendToSession(e.id, map[string]any{"type": "message_complete", "sessionId": e.id})
	e.waitTurnLines(t, 1)
	out := e.buf.String()
	for _, c := range []string{traceCanaryPrompt, traceCanaryFile} {
		if strings.Contains(out, c) {
			t.Fatalf("canary %q in output: %s", c, out)
		}
	}
}
