package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/logging"
)

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

// captureLogs routes slog.Default into the returned buffer for the test.
func captureLogs(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{DefaultService: "relay"})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

var nineKeys = []string{"ts", "level", "msg", "service", "op", "status", "duration_ms", "error", "trace_id"}

// bridgeLines returns the parsed bridge.request lines and fails if any has
// fewer than the nine standard keys.
func bridgeLines(t *testing.T, buf *lockedBuf) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !strings.HasPrefix(raw, "{") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", raw, err)
		}
		if m["op"] != "bridge.request" {
			continue
		}
		for _, k := range nineKeys {
			if _, ok := m[k]; !ok {
				t.Fatalf("line lacks key %q: %s", k, raw)
			}
		}
		if _, ok := m["duration_ms"].(float64); !ok {
			t.Fatalf("duration_ms is not a number: %s", raw)
		}
		out = append(out, m)
	}
	return out
}

func oneBridgeLine(t *testing.T, buf *lockedBuf) map[string]any {
	t.Helper()
	lines := bridgeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 bridge.request line, got %d:\n%s", len(lines), buf.String())
	}
	return lines[0]
}

// traceRouter records the trace ID each CallTool context carries.
type traceRouter struct {
	*stubRouter
	mu     sync.Mutex
	traces []string
	err    error
}

func (r *traceRouter) CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error) {
	r.mu.Lock()
	r.traces = append(r.traces, logging.TraceFromContext(ctx))
	err := r.err
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return json.RawMessage(`"done"`), nil
}

func (r *traceRouter) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.traces...)
}

// sendLine writes one raw line and waits for the response frame.
func sendLine(t *testing.T, sock, line string) BridgeResponse {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sc := NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("read: %v", sc.Err())
	}
	var resp BridgeResponse
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return resp
}

func sendReq(t *testing.T, sock string, req BridgeRequest) BridgeResponse {
	t.Helper()
	data, _ := json.Marshal(req)
	return sendLine(t, sock, string(data))
}

// sendTraced sends a CallTool carrying trace_id on the wire as raw JSON, so
// the test pins the wire name rather than a Go field.
func sendTraced(t *testing.T, sock, id string) BridgeResponse {
	t.Helper()
	line, _ := json.Marshal(map[string]string{"type": ReqCallTool, "name": "tool_a", "trace_id": id})
	return sendLine(t, sock, string(line))
}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestBridgeTrace_ValidIDReachesRouterAndLine(t *testing.T) {
	buf := captureLogs(t)
	router := &traceRouter{stubRouter: &stubRouter{}}
	sock := startTestBridge(t, router)

	const id = "abcd1234-trace_ID"
	sendTraced(t, sock, id)

	if got := router.seen(); len(got) != 1 || got[0] != id {
		t.Fatalf("router saw trace %v, want [%s]", got, id)
	}
	if line := oneBridgeLine(t, buf); line["trace_id"] != id {
		t.Fatalf("line trace_id = %v, want %s", line["trace_id"], id)
	}
}

func TestBridgeTrace_AbsentIsMintedInvalidIsReplaced(t *testing.T) {
	cases := []struct{ name, id string }{
		{"absent", ""},
		{"too short", "abc1234"},
		{"too long", strings.Repeat("a", 65)},
		{"bad character", "abcd.1234"},
		{"newline", "abcd1234\nINJECTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t)
			router := &traceRouter{stubRouter: &stubRouter{}}
			sock := startTestBridge(t, router)

			sendTraced(t, sock, tc.id)

			seen := router.seen()
			if len(seen) != 1 || !hex32.MatchString(seen[0]) {
				t.Fatalf("router saw %v, want one minted 32-hex ID", seen)
			}
			line := oneBridgeLine(t, buf)
			if line["trace_id"] != seen[0] {
				t.Fatalf("line trace_id = %v, router saw %s", line["trace_id"], seen[0])
			}
			if tc.id != "" && strings.Contains(buf.String(), tc.id) {
				t.Fatalf("rejected trace_id leaked into output:\n%s", buf.String())
			}
		})
	}
}

func TestBridgeTrace_OneLinePerRequestMapsOutcome(t *testing.T) {
	denied := jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, errors.New("nope"))
	cases := []struct {
		name       string
		raw        string
		req        BridgeRequest
		adminErr   error
		callErr    error
		wantLevel  string
		wantStatus string
		wantError  string
		wantType   string
		wantName   string
	}{
		{name: "call tool ok", req: BridgeRequest{Type: ReqCallTool, Name: "tool_a"},
			wantLevel: "info", wantStatus: "ok", wantType: ReqCallTool, wantName: "tool_a"},
		{name: "admin_op ok", req: BridgeRequest{Type: ReqAdminOp, Name: "settings.get"},
			wantLevel: "info", wantStatus: "ok", wantType: ReqAdminOp, wantName: "settings.get"},
		{name: "bad admin token", req: BridgeRequest{Type: ReqReconcileExternalMcps, Token: "t"}, adminErr: denied,
			wantLevel: "warn", wantStatus: "denied", wantError: "unauthorized", wantType: ReqReconcileExternalMcps},
		{name: "unknown type", req: BridgeRequest{Type: "Bogus"},
			wantLevel: "warn", wantStatus: "error", wantError: "method_not_found", wantType: "unknown"},
		{name: "malformed json", raw: `{"type":`,
			wantLevel: "warn", wantStatus: "error", wantError: "parse_error", wantType: "unknown"},
		{name: "router internal error", req: BridgeRequest{Type: ReqCallTool, Name: "tool_a"}, callErr: errors.New("boom"),
			wantLevel: "error", wantStatus: "error", wantError: "internal_error", wantType: ReqCallTool, wantName: "tool_a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t)
			router := &traceRouter{stubRouter: &stubRouter{validateAdminErr: tc.adminErr}, err: tc.callErr}
			sock := startTestBridge(t, router)

			if tc.raw != "" {
				sendLine(t, sock, tc.raw)
			} else {
				sendReq(t, sock, tc.req)
			}

			line := oneBridgeLine(t, buf)
			if line["level"] != tc.wantLevel || line["status"] != tc.wantStatus || line["error"] != tc.wantError {
				t.Fatalf("level/status/error = %v/%v/%v, want %s/%s/%s", line["level"], line["status"], line["error"], tc.wantLevel, tc.wantStatus, tc.wantError)
			}
			if line["request_type"] != tc.wantType {
				t.Fatalf("request_type = %v, want %s", line["request_type"], tc.wantType)
			}
			gotName, hasName := line["name"]
			if tc.wantName == "" && hasName && gotName != "" {
				t.Fatalf("name = %v, want absent or empty", gotName)
			}
			if tc.wantName != "" && gotName != tc.wantName {
				t.Fatalf("name = %v, want %s", gotName, tc.wantName)
			}
			if !hex32.MatchString(line["trace_id"].(string)) {
				t.Fatalf("trace_id = %v, want a minted ID even on this path", line["trace_id"])
			}
		})
	}
}

func TestBridgeTrace_NeverLogsSecretsOrContent(t *testing.T) {
	buf := captureLogs(t)
	router := &traceRouter{
		stubRouter: &stubRouter{},
		err:        errors.New("upstream said CANARY-ERRTEXT"),
	}
	sock := startTestBridge(t, router)

	sendReq(t, sock, BridgeRequest{
		Type: ReqCallTool, Name: "tool_a",
		Token:     "CANARY-TOKEN",
		Arguments: json.RawMessage(`{"prompt":"CANARY-ARGS"}`),
	})
	sendReq(t, sock, BridgeRequest{Type: ReqHello, Name: "svc", Token: "CANARY-SECRET"})

	out := buf.String()
	for _, canary := range []string{"CANARY-TOKEN", "CANARY-ARGS", "CANARY-SECRET", "CANARY-ERRTEXT"} {
		if strings.Contains(out, canary) {
			t.Errorf("%s leaked into log output:\n%s", canary, out)
		}
	}
	if n := len(bridgeLines(t, buf)); n != 2 {
		t.Fatalf("want 2 bridge.request lines, got %d", n)
	}
}
