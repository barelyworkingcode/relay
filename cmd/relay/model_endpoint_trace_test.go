package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/logging"
)

type modelLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *modelLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *modelLogBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureModelLogs routes the default logger into a buffer for the test.
func captureModelLogs(t *testing.T) *modelLogBuf {
	t.Helper()
	buf := &modelLogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{DefaultService: "relay"})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

var modelStandardLogKeys = []string{"ts", "level", "msg", "service", "op", "status", "duration_ms", "error", "trace_id"}

// modelRequestLines returns the parsed model.request lines, asserting each
// carries the nine standard keys.
func modelRequestLines(t *testing.T, buf *modelLogBuf) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !strings.HasPrefix(raw, "{") {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		if line["op"] != "model.request" {
			continue
		}
		for _, k := range modelStandardLogKeys {
			if _, ok := line[k]; !ok {
				t.Fatalf("line missing key %q: %s", k, raw)
			}
		}
		out = append(out, line)
	}
	return out
}

type modelTraceFixture struct {
	handler http.Handler
	tcp     http.Handler
	token   string
	remote  string
	relayK  string
	seen    func() (trace []string, relayKey []string)
}

func newModelTraceFixture(t *testing.T, chatStatus int, withHost bool) modelTraceFixture {
	t.Helper()
	mkSandboxRelayHome(t)
	m, store, launches, hosts := newModelEndpointTestServer(t)
	var mu sync.Mutex
	var trace, key []string
	record := func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		trace = append(trace, r.Header.Values(logging.TraceHeader)...)
		key = append(key, r.Header.Values("X-Relay-Key")...)
	}
	mux := fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(chatStatus)
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	mux.HandleFunc("/openai/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_, _ = w.Write([]byte(`{}`))
	})
	if withHost {
		registerFakeHost(t, hosts, launches, "relayllm", newFakeRouterSocket(t, mux), selfPeerToken(t).Process())
	}
	tok := addModelProject(t, store, "p1", []string{"vCode"}, false)
	remote := addModelProject(t, store, "r1", nil, true)
	relayKey, err := m.modelKeys.Mint("p1", "session:x")
	assertNoErr(t, err, "Mint")
	return modelTraceFixture{
		handler: m.Handler(transportSocket),
		tcp:     m.Handler(transportTCP),
		token:   tok,
		remote:  remote,
		relayK:  relayKey,
		seen: func() ([]string, []string) {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), trace...), append([]string(nil), key...)
		},
	}
}

func (f modelTraceFixture) chat(token string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?key=CANARY-QUERY", strings.NewReader(`{"model":"vCode","messages":[{"role":"user","content":"CANARY-PROMPT"}]}`))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

var traceHex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestModelEndpointTrace_MintsIDWhenAbsent(t *testing.T) {
	buf := captureModelLogs(t)
	f := newModelTraceFixture(t, http.StatusOK, true)
	if w := f.chat(f.token, nil); w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	trace, _ := f.seen()
	if len(trace) != 1 || !traceHex32.MatchString(trace[0]) {
		t.Fatalf("host saw X-Trace-Id %q, want exactly one 32-hex value", trace)
	}
	lines := modelRequestLines(t, buf)
	if len(lines) != 1 || lines[0]["trace_id"] != trace[0] {
		t.Fatalf("log lines = %v, want one carrying trace_id %s", lines, trace[0])
	}
}

func TestModelEndpointTrace_ValidInboundReachesHostUnchanged(t *testing.T) {
	buf := captureModelLogs(t)
	f := newModelTraceFixture(t, http.StatusOK, true)
	f.chat(f.token, func(r *http.Request) { r.Header.Set(logging.TraceHeader, "Abc_123-xyz") })
	trace, _ := f.seen()
	if len(trace) != 1 || trace[0] != "Abc_123-xyz" {
		t.Fatalf("host saw %q, want [Abc_123-xyz]", trace)
	}
	if lines := modelRequestLines(t, buf); len(lines) != 1 || lines[0]["trace_id"] != "Abc_123-xyz" {
		t.Fatalf("log lines = %v", lines)
	}
}

func TestModelEndpointTrace_InvalidInboundIsReplaced(t *testing.T) {
	for name, bad := range map[string]string{
		"short":   "abc1234",
		"long":    strings.Repeat("a", 65),
		"dot":     "abcd.efgh1234",
		"newline": "abcdefgh\nCANARY-INJECT",
	} {
		t.Run(name, func(t *testing.T) {
			buf := captureModelLogs(t)
			f := newModelTraceFixture(t, http.StatusOK, true)
			f.chat(f.token, func(r *http.Request) { r.Header.Set(logging.TraceHeader, bad) })
			trace, _ := f.seen()
			if len(trace) != 1 || trace[0] == bad || !traceHex32.MatchString(trace[0]) {
				t.Fatalf("host saw %q, want one fresh 32-hex ID", trace)
			}
			if out := buf.String(); strings.Contains(out, bad) || strings.Contains(out, "CANARY-INJECT") {
				t.Fatalf("rejected value reached the log: %s", out)
			}
		})
	}
}

func TestModelEndpointTrace_FirstOfTwoInboundValuesWins(t *testing.T) {
	captureModelLogs(t)
	f := newModelTraceFixture(t, http.StatusOK, true)
	f.chat(f.token, func(r *http.Request) {
		r.Header.Add(logging.TraceHeader, "first-id-0001")
		r.Header.Add(logging.TraceHeader, "second-id-0002")
	})
	trace, _ := f.seen()
	if len(trace) != 1 || trace[0] != "first-id-0001" {
		t.Fatalf("host saw %q, want exactly [first-id-0001]", trace)
	}
}

func TestModelEndpointTrace_PassthroughForwardsIDAndStripsRelayKey(t *testing.T) {
	captureModelLogs(t)
	f := newModelTraceFixture(t, http.StatusOK, true)
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer client-own-credential")
	r.Header.Set("X-Relay-Key", f.relayK)
	r.Header.Set(logging.TraceHeader, "pass-trace-0001")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	trace, key := f.seen()
	if len(trace) != 1 || trace[0] != "pass-trace-0001" {
		t.Fatalf("host saw trace %q, want [pass-trace-0001]", trace)
	}
	if len(key) != 0 {
		t.Fatalf("X-Relay-Key reached the host: %q", key)
	}
}

func TestModelEndpointTrace_OneLinePerCall(t *testing.T) {
	buf := captureModelLogs(t)
	f := newModelTraceFixture(t, http.StatusOK, true)
	f.chat(f.token, nil)
	lines := modelRequestLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d model.request lines, want 1: %s", len(lines), buf.String())
	}
	l := lines[0]
	if l["status"] != "ok" || l["transport"] != transportSocket || l["http_status"] != float64(200) {
		t.Fatalf("line = %v", l)
	}
	if d, ok := l["duration_ms"].(float64); !ok || d != float64(int64(d)) || d < 0 {
		t.Fatalf("duration_ms = %v, want a non-negative integer", l["duration_ms"])
	}
}

func TestModelEndpointTrace_FailureStatuses(t *testing.T) {
	cases := []struct {
		name       string
		chatStatus int
		withHost   bool
		token      bool
		transport  string
		level      string
		status     string
		errText    string
	}{
		{"tokenless tcp", 200, true, false, transportTCP, "warn", "denied", "unauthorized"},
		{"no host registered", 200, false, true, transportSocket, "error", "error", "host_unavailable"},
		{"upstream 500", 500, true, true, transportSocket, "error", "error", "http_500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureModelLogs(t)
			f := newModelTraceFixture(t, tc.chatStatus, tc.withHost)
			tok := ""
			if tc.token {
				tok = f.token
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
			if tok != "" {
				r.Header.Set("Authorization", "Bearer "+tok)
			}
			h := f.handler
			if tc.transport == transportTCP {
				h = f.tcp
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			lines := modelRequestLines(t, buf)
			if len(lines) != 1 {
				t.Fatalf("got %d lines: %s", len(lines), buf.String())
			}
			l := lines[0]
			if l["level"] != tc.level || l["status"] != tc.status || l["error"] != tc.errText {
				t.Fatalf("line = %v, want %s/%s/%s", l, tc.level, tc.status, tc.errText)
			}
		})
	}
}

func TestModelEndpointTrace_SuccessfulPollsAreSilentRefusedListLogs(t *testing.T) {
	buf := captureModelLogs(t)
	f := newModelTraceFixture(t, http.StatusOK, true)
	for _, path := range []string{"/v1/models", "/health"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+f.token)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d; body=%s", path, w.Code, w.Body.String())
		}
	}
	if lines := modelRequestLines(t, buf); len(lines) != 0 {
		t.Fatalf("successful polls wrote %d lines: %v", len(lines), lines)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+f.remote)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("remote list status = %d, want 403", w.Code)
	}
	if lines := modelRequestLines(t, buf); len(lines) != 1 {
		t.Fatalf("refused list wrote %d lines, want 1", len(lines))
	}
}

func TestModelEndpointTrace_NoSecretsInOutput(t *testing.T) {
	buf := captureModelLogs(t)
	f := newModelTraceFixture(t, http.StatusOK, true)
	f.chat(f.token, func(r *http.Request) {
		r.Header.Set("X-Relay-Key", "CANARY-RELAY-KEY")
	})
	f.chat("CANARY-BEARER", nil)
	if len(modelRequestLines(t, buf)) == 0 {
		t.Fatal("no model.request lines captured; the canary check would pass vacuously")
	}
	out := buf.String()
	for _, canary := range []string{"CANARY-BEARER", "CANARY-RELAY-KEY", "CANARY-PROMPT", "CANARY-QUERY", f.token} {
		if strings.Contains(out, canary) {
			t.Fatalf("log output contains %q: %s", canary, out)
		}
	}
}
