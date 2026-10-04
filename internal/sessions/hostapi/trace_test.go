package hostapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/logging"
)

type stdBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *stdBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *stdBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureStd routes the default slog logger through the standard handler.
// Callers must not run in parallel: slog.Default is process-wide.
func captureStd(t *testing.T) *stdBuf {
	t.Helper()
	buf := &stdBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler(buf, logging.Options{DefaultService: "testbox"})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func launchLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(out), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", ln, err)
		}
		if m["op"] == "session.launch" {
			lines = append(lines, m)
		}
	}
	return lines
}

func oneLaunchLine(t *testing.T, buf *stdBuf) map[string]any {
	t.Helper()
	lines := launchLines(t, buf.String())
	if len(lines) != 1 {
		t.Fatalf("want exactly one session.launch line, got %d in:\n%s", len(lines), buf.String())
	}
	return lines[0]
}

func launchWithHeader(t *testing.T, sock, bearer, method, traceHeader string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, "http://h/launch", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if traceHeader != "" {
		req.Header.Set(logging.TraceHeader, traceHeader)
	}
	resp, err := unixClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// A spec that fails validation: answered 400 invalid_spec before any spawn.
func badSpec() map[string]any { return map[string]any{"v": 0, "session_id": "p1", "kind": "pty"} }

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestLaunchTrace_HeaderHandling(t *testing.T) {
	const rejected = "bad value with spaces CANARY-REJECTED"
	cases := []struct {
		name   string
		header string
		check  func(t *testing.T, got string)
	}{
		{"valid kept", "trace-abc12345", func(t *testing.T, got string) {
			if got != "trace-abc12345" {
				t.Errorf("trace_id = %q, want the supplied id", got)
			}
		}},
		{"invalid replaced", rejected, func(t *testing.T, got string) {
			if !hex32.MatchString(got) {
				t.Errorf("trace_id = %q, want a fresh 32-hex id", got)
			}
		}},
		{"absent generated", "", func(t *testing.T, got string) {
			if !hex32.MatchString(got) {
				t.Errorf("trace_id = %q, want a fresh 32-hex id", got)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, sock, _, bearer := startServer(t, os.Getpid())
			buf := captureStd(t)
			launchWithHeader(t, sock, bearer, http.MethodPost, tc.header, badSpec())
			line := oneLaunchLine(t, buf)
			id, _ := line["trace_id"].(string)
			tc.check(t, id)
			if strings.Contains(buf.String(), "CANARY-REJECTED") {
				t.Errorf("rejected header value reached the log:\n%s", buf.String())
			}
		})
	}
}

func TestLaunchTraceLine_ByOutcome(t *testing.T) {
	_, target := buildBinaries(t)
	_, sock, _, bearer := startServer(t, os.Getpid())
	okBody := launchBody("p1", []string{target, "-sleep", "200ms", "-exit-code", "0"})

	cases := []struct {
		name       string
		bearer     string
		method     string
		body       any
		wantHTTP   int
		wantLevel  string
		wantStatus string
		wantErr    string
	}{
		{"created", bearer, http.MethodPost, okBody, 201, "info", "ok", ""},
		{"forbidden", "wrong", http.MethodPost, okBody, 403, "warn", "denied", "forbidden"},
		{"method not allowed", bearer, http.MethodGet, okBody, 405, "warn", "error", "method_not_allowed"},
		{"invalid spec", bearer, http.MethodPost, badSpec(), 400, "warn", "error", "invalid_spec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureStd(t)
			resp := launchWithHeader(t, sock, tc.bearer, tc.method, "", tc.body)
			if resp.StatusCode != tc.wantHTTP {
				t.Fatalf("http status = %d, want %d", resp.StatusCode, tc.wantHTTP)
			}
			line := oneLaunchLine(t, buf)
			if line["level"] != tc.wantLevel || line["status"] != tc.wantStatus {
				t.Errorf("level/status = %v/%v, want %s/%s", line["level"], line["status"], tc.wantLevel, tc.wantStatus)
			}
			if got, _ := line["error"].(string); got != tc.wantErr {
				t.Errorf("error = %q, want %q", got, tc.wantErr)
			}
			if _, ok := line["duration_ms"].(float64); !ok {
				t.Errorf("duration_ms missing or not a number: %v", line["duration_ms"])
			}
		})
	}
}

func TestLaunchTraceLine_ErrorCarriesCodeNotMessage(t *testing.T) {
	_, sock, _, bearer := startServer(t, os.Getpid())
	buf := captureStd(t)
	// The validation message names the missing fields; only the code may be logged.
	launchWithHeader(t, sock, bearer, http.MethodPost, "", badSpec())
	line := oneLaunchLine(t, buf)
	if strings.Contains(buf.String(), "are required") {
		t.Errorf("error message text reached the log:\n%s", buf.String())
	}
	if line["error"] != "invalid_spec" {
		t.Errorf("error = %v, want invalid_spec", line["error"])
	}
}

func TestLaunchTraceLine_NeverLogsSecrets(t *testing.T) {
	_, target := buildBinaries(t)
	_, sock, _, bearer := startServer(t, os.Getpid())
	secret := strings.Repeat("c4", 32)
	const modelKey = "sk-CANARY-MODEL-KEY"

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"accepted", launchBody("p1", []string{target, "-sleep", "200ms", "-exit-code", "0"})},
		{"refused", badSpec()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureStd(t)
			tc.body["identity"] = map[string]any{"secret": secret}
			tc.body["model_key"] = modelKey
			launchWithHeader(t, sock, bearer, http.MethodPost, "", tc.body)
			out := buf.String()
			if out == "" {
				t.Fatal("no log output captured; the check would pass vacuously")
			}
			for _, canary := range []string{secret, modelKey, bearer} {
				if strings.Contains(out, canary) {
					t.Errorf("secret %q reached the log:\n%s", canary, out)
				}
			}
		})
	}
}
