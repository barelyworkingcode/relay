package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
)

const (
	traceTestToken       = "canary-bearer-token-9f3a"
	traceTestQueryCanary = "canary-query-secret-77c1"
)

var traceHexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// captureTraceLogs routes slog.Default through the real logging handler into
// a buffer. Tests using it must not run in parallel.
func captureTraceLogs(t *testing.T) *lrSyncBuffer {
	t.Helper()
	buf := &lrSyncBuffer{}
	prev := slog.Default()
	h := logging.NewHandler(buf, logging.Options{
		DefaultService: "relay",
		Getenv:         func(string) string { return "" },
	})
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func traceLines(t *testing.T, buf *lrSyncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(buf.String(), "\n") {
		if !strings.HasPrefix(raw, "{") {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("line is not JSON: %v\n%s", err, raw)
		}
		out = append(out, m)
	}
	return out
}

func requestLines(t *testing.T, buf *lrSyncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range traceLines(t, buf) {
		if m["op"] == "frontend.request" {
			out = append(out, m)
		}
	}
	return out
}

// waitRequestLine waits for a frontend.request line matching pred; the line is
// written as the handler returns, which can trail the client's response.
func waitRequestLine(t *testing.T, buf *lrSyncBuffer, desc string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, m := range requestLines(t, buf) {
			if pred(m) {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no frontend.request line for %s; output:\n%s", desc, buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func linesForPath(t *testing.T, buf *lrSyncBuffer, path string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range requestLines(t, buf) {
		if m["path"] == path {
			out = append(out, m)
		}
	}
	return out
}

var (
	traceOpRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	traceTsRe = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
)

// checkNineKeys is a minimal inline check of the line schema's nine keys.
func checkNineKeys(m map[string]any) error {
	for _, k := range []string{"ts", "level", "msg", "service", "op", "status", "duration_ms", "error", "trace_id"} {
		if _, ok := m[k]; !ok {
			return fmt.Errorf("missing key %q", k)
		}
	}
	str := func(k string) (string, error) {
		s, ok := m[k].(string)
		if !ok {
			return "", fmt.Errorf("%q is %T, want string", k, m[k])
		}
		return s, nil
	}
	for _, k := range []string{"ts", "level", "msg", "service", "op", "status", "error", "trace_id"} {
		if _, err := str(k); err != nil {
			return err
		}
	}
	if !traceTsRe.MatchString(m["ts"].(string)) {
		return fmt.Errorf("ts %q not UTC millisecond RFC 3339", m["ts"])
	}
	switch m["level"] {
	case "error", "warn", "info", "debug":
	default:
		return fmt.Errorf("level %v", m["level"])
	}
	switch m["status"] {
	case "ok", "error", "denied":
	default:
		return fmt.Errorf("status %v", m["status"])
	}
	if m["service"] == "" {
		return fmt.Errorf("service empty")
	}
	if !traceOpRe.MatchString(m["op"].(string)) {
		return fmt.Errorf("op %q", m["op"])
	}
	n, ok := m["duration_ms"].(json.Number)
	if !ok {
		return fmt.Errorf("duration_ms is %T", m["duration_ms"])
	}
	if i, err := n.Int64(); err != nil || i < 0 {
		return fmt.Errorf("duration_ms %v not a non-negative integer", n)
	}
	for _, k := range []string{"msg", "error"} {
		if utf8.RuneCountInString(m[k].(string)) > 500 {
			return fmt.Errorf("%s over 500 runes", k)
		}
	}
	return nil
}

// traceFixture starts the full frontend stack with one proxied service that
// answers 200 on every path, and 503 when the query has fail=1.
func traceFixture(t *testing.T, routes ...string) (*lrSyncBuffer, *FakeService, *http.Client, string) {
	t.Helper()
	buf := captureTraceLogs(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	fake := NewFakeService(t, FakeServiceOptions{
		ServiceID: "svc-trace",
		Manifest:  newManifest(routes...),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				c, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer c.Close()
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			}
			if r.URL.Query().Get("fail") == "1" {
				http.Error(w, "upstream failure", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok"))
		},
	})
	if err := enhanced.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest()); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	sock := startFrontendServerWith(t, traceTestToken, enhanced)
	return buf, fake, dialFrontendHTTP(sock), sock
}

func traceGet(t *testing.T, c *http.Client, method, rawURL string, hdr http.Header) int {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+traceTestToken)
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestTrace_HTTPHeaderHandling(t *testing.T) {
	cases := []struct {
		name    string
		inbound []string
		check   func(t *testing.T, got string)
		want    string // exact expected ID; empty means a fresh 32-hex ID
		reject  string // value that must never appear in output
	}{
		{name: "absent header gets a new id", want: ""},
		{name: "valid id is kept", inbound: []string{"Valid_trace-ID-42"}, want: "Valid_trace-ID-42"},
		{name: "invalid id is replaced", inbound: []string{"bad.value.REJECTED-CANARY"}, reject: "REJECTED-CANARY"},
		{name: "first of several values wins", inbound: []string{"firstvalid01", "secondvalid02"}, want: "firstvalid01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, fake, client, _ := traceFixture(t, "/api/svc/")
			hdr := http.Header{}
			if tc.inbound != nil {
				hdr["X-Trace-Id"] = tc.inbound
			}
			if code := traceGet(t, client, http.MethodGet, "http://unix/api/svc/echo", hdr); code != http.StatusOK {
				t.Fatalf("status %d", code)
			}
			up := fake.LastRequest()
			if up == nil {
				t.Fatal("upstream saw no request")
			}
			got := up.Headers.Get("X-Trace-Id")
			if tc.want != "" && got != tc.want {
				t.Fatalf("upstream X-Trace-Id = %q, want %q", got, tc.want)
			}
			if tc.want == "" && !traceHexID.MatchString(got) {
				t.Fatalf("upstream X-Trace-Id = %q, want 32 lowercase hex", got)
			}
			line := waitRequestLine(t, buf, "/api/svc/echo", func(m map[string]any) bool { return m["path"] == "/api/svc/echo" })
			if line["trace_id"] != got {
				t.Fatalf("log trace_id = %v, upstream got %q; they must be one id", line["trace_id"], got)
			}
			if tc.reject != "" && strings.Contains(buf.String(), tc.reject) {
				t.Fatalf("rejected trace value appears in output:\n%s", buf.String())
			}
		})
	}
}

func TestTrace_WebSocketHandshakeCarriesTraceID(t *testing.T) {
	buf, fake, _, sock := traceFixture(t, "/ws")
	hdr := http.Header{"Authorization": {"Bearer " + traceTestToken}}
	conn, _, err := wsDialerOverUnix(sock).Dial("ws://unix/ws", hdr)
	assertNoErr(t, err, "ws dial")

	var up *fakeServiceRequest
	deadline := time.Now().Add(3 * time.Second)
	for up == nil && time.Now().Before(deadline) {
		for _, r := range fake.Requests() {
			if r.WasWebSocket {
				up = r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if up == nil {
		t.Fatal("upstream saw no websocket handshake")
	}
	id := up.Headers.Get("X-Trace-Id")
	if !traceHexID.MatchString(id) {
		t.Fatalf("handshake X-Trace-Id = %q, want 32 lowercase hex", id)
	}

	_ = conn.Close()
	line := waitRequestLine(t, buf, "websocket close", func(m map[string]any) bool { return m["path"] == "/ws" })
	if line["http_status"] != json.Number("101") || line["trace_id"] != id {
		t.Fatalf("ws line = %v; want http_status 101 and trace_id %q", line, id)
	}
}

func TestTrace_RequestLineShapeAndNoSecrets(t *testing.T) {
	buf, _, client, _ := traceFixture(t, "/api/svc/")
	if code := traceGet(t, client, http.MethodGet, "http://unix/api/svc/echo?k="+traceTestQueryCanary, nil); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	// A rejected credential is the other place a bearer could leak.
	req, _ := http.NewRequest(http.MethodGet, "http://unix/api/svc/denied?k="+traceTestQueryCanary, nil)
	req.Header.Set("Authorization", "Bearer wrong-"+traceTestToken)
	resp, err := client.Do(req)
	assertNoErr(t, err, "denied GET")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: status %d, want 401", resp.StatusCode)
	}

	ok := waitRequestLine(t, buf, "ok request", func(m map[string]any) bool { return m["path"] == "/api/svc/echo" })
	denied := waitRequestLine(t, buf, "denied request", func(m map[string]any) bool { return m["path"] == "/api/svc/denied" })

	if n := len(linesForPath(t, buf, "/api/svc/echo")); n != 1 {
		t.Fatalf("want exactly one frontend.request line for the request, got %d", n)
	}
	if ok["method"] != "GET" || ok["http_status"] != json.Number("200") || ok["status"] != "ok" || ok["level"] != "info" || ok["error"] != "" {
		t.Fatalf("ok line = %v", ok)
	}
	if denied["http_status"] != json.Number("401") || denied["status"] != "denied" || denied["level"] != "warn" || denied["error"] != "http_401" {
		t.Fatalf("denied line = %v", denied)
	}
	for _, canary := range []string{traceTestToken, traceTestQueryCanary} {
		if strings.Contains(buf.String(), canary) {
			t.Fatalf("canary %q leaked into output:\n%s", canary, buf.String())
		}
	}
	for _, m := range traceLines(t, buf) {
		if err := checkNineKeys(m); err != nil {
			t.Fatalf("line breaks the schema: %v\n%v", err, m)
		}
	}
}

func TestTrace_PollsWriteNoLine(t *testing.T) {
	pollPaths := []string{"/api/eve/passkeys/revocations", "/api/eve/passkey-enrolment", "/api/auth/status"}
	buf, _, client, _ := traceFixture(t, "/api/svc/", "/api/auth/status")

	for _, p := range pollPaths {
		if code := traceGet(t, client, http.MethodGet, "http://unix"+p, nil); code != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200 so the poll succeeds", p, code)
		}
	}
	// A request that completes after the polls proves earlier lines would have
	// been written by now.
	traceGet(t, client, http.MethodGet, "http://unix/api/svc/regular", nil)
	waitRequestLine(t, buf, "non-poll GET", func(m map[string]any) bool { return m["path"] == "/api/svc/regular" })

	for _, p := range pollPaths {
		if n := len(linesForPath(t, buf, p)); n != 0 {
			t.Errorf("successful poll GET %s wrote %d line(s); want none", p, n)
		}
	}

	for _, p := range pollPaths {
		traceGet(t, client, http.MethodPost, "http://unix"+p, nil)
		waitRequestLine(t, buf, "POST "+p, func(m map[string]any) bool { return m["path"] == p && m["method"] == "POST" })
		// A rejected credential makes the same GET fail.
		bad := http.Header{"Authorization": {"Bearer wrong-credential"}}
		if code := traceGet(t, client, http.MethodGet, "http://unix"+p, bad); code != http.StatusUnauthorized {
			t.Fatalf("GET %s with a wrong bearer: status %d, want 401", p, code)
		}
		waitRequestLine(t, buf, "failing GET "+p, func(m map[string]any) bool {
			return m["path"] == p && m["method"] == "GET" && m["http_status"] == json.Number("401")
		})
	}
}

// A stalled body on a relay-owned route must be cut by the route read
// deadline even though the status-capturing writer wraps the connection.
func TestTrace_StalledBodyOnRelayRouteIsCut(t *testing.T) {
	orig := frontendRouteReadDeadline
	frontendRouteReadDeadline = 150 * time.Millisecond
	t.Cleanup(func() { frontendRouteReadDeadline = orig })

	buf, _, _, sock := traceFixture(t, "/api/svc/")
	conn := dialUnixWithTimeout(t, sock, 2*time.Second)
	defer conn.Close()

	// Headers promise 1000 bytes; only 10 arrive.
	fmt.Fprintf(conn, "POST /api/projects HTTP/1.1\r\nHost: unix\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"name\":\"a", traceTestToken)

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response to a stalled body within 3s of a 150ms deadline: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("stalled body: status %d, want an error status", resp.StatusCode)
	}
	waitRequestLine(t, buf, "stalled POST", func(m map[string]any) bool { return m["path"] == "/api/projects" })
}

func TestTrace_SessionHostClientForwardsContextTraceID(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want string // empty means the header must be absent
	}{
		{name: "ctx trace id is sent", ctx: logging.ContextWithTrace(context.Background(), "Ctx_trace-ID-7"), want: "Ctx_trace-ID-7"},
		{name: "no ctx trace id sends no header", ctx: context.Background()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sock := filepath.Join(mkShortTempDir(t, "sh-"), "host.sock")
			ln, err := net.Listen("unix", sock)
			assertNoErr(t, err, "listen")
			var mu sync.Mutex
			var seen []http.Header
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Header.Clone())
				mu.Unlock()
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("{}"))
			})}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			launches := service.NewLaunches()
			secret, _, err := launches.Begin(service.Identity{Kind: service.IdentityKindService, Name: config.RelaySessionsServiceID})
			assertNoErr(t, err, "Begin")
			_, err = launches.Bind(config.RelaySessionsServiceID, secret, selfPeerToken(t))
			assertNoErr(t, err, "Bind")
			enhanced := NewEnhancedServiceRegistry(nil)
			assertNoErr(t, enhanced.RegisterManifest(config.RelaySessionsServiceID, sock, "host-token", newManifest("/api/models")), "RegisterManifest")

			client := &sessionHostClient{enhanced: enhanced, launches: launches}
			if _, _, err := client.Launch(tc.ctx, hostapi.LaunchRequest{}); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 {
				t.Fatalf("host saw %d requests, want 1", len(seen))
			}
			vals, present := seen[0]["X-Trace-Id"]
			if tc.want == "" {
				if present {
					t.Fatalf("X-Trace-Id = %v, want the header absent", vals)
				}
				return
			}
			if got := seen[0].Get("X-Trace-Id"); got != tc.want {
				t.Fatalf("X-Trace-Id = %q, want %q", got, tc.want)
			}
		})
	}
}
