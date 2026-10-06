package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

// dropInHost is a fake relay-sessions host for the drop-in routes. Each
// endpoint answers what the test sets; every call is counted.
type dropInHost struct {
	mu          sync.Mutex
	handoff     func(w http.ResponseWriter)
	launchCode  int
	paths       []string
	launchSpecs []hostapi.LaunchRequest
}

func (h *dropInHost) calls(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, p := range h.paths {
		if p == path {
			n++
		}
	}
	return n
}

type dropInFixture struct {
	*sessionRoutesFixture
	host *dropInHost
	logs *lrSyncBuffer
}

const dropInTestSessionID = "agent-1"

func newDropInFixture(t *testing.T, claudeID string) *dropInFixture {
	t.Helper()
	logs := captureTraceLogs(t)
	f := newSessionRoutesFixture(t)
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{
		SessionID: dropInTestSessionID, Kind: KindClaude, ProjectID: f.proj.ID, Directory: f.proj.Path, State: ledger.StateLive,
	}), "seed claude session")

	h := &dropInHost{launchCode: http.StatusCreated}
	h.handoff = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hostapi.HandoffResponse{SessionID: dropInTestSessionID, ClaudeSessionID: claudeID})
	}
	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			h.mu.Lock()
			h.paths = append(h.paths, r.URL.Path)
			h.mu.Unlock()
			switch r.URL.Path {
			case "/handoff":
				h.handoff(w)
			case "/handback":
				w.WriteHeader(http.StatusNoContent)
			case "/launch":
				var spec hostapi.LaunchRequest
				_ = json.Unmarshal(fs.LastRequest().Body, &spec)
				h.mu.Lock()
				h.launchSpecs = append(h.launchSpecs, spec)
				code := h.launchCode
				h.mu.Unlock()
				if code != http.StatusCreated {
					http.Error(w, "boom", code)
					return
				}
				fakeLaunchHandler(&fs, func(id string) string { return `{"terminalId":"` + id + `"}` })(w, r)
			default:
				http.NotFound(w, r)
			}
		},
	})
	f.registerFakeSessionsHost(t, fs, selfPeerToken(t).Process())
	return &dropInFixture{sessionRoutesFixture: f, host: h, logs: logs}
}

func (f *dropInFixture) post(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+id+"/drop-in", strings.NewReader(`{"cols":100,"rows":30}`))
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *dropInFixture) errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct{ Error, Message string }
	assertNoErr(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode refusal "+rec.Body.String())
	if body.Message == "" {
		t.Errorf("refusal %s carries no message", rec.Body.String())
	}
	return body.Error
}

// requireOneLogLine asserts exactly one session.drop_in line, with the given
// status, error code and host, and returns it.
func (f *dropInFixture) requireOneLogLine(t *testing.T, status, code string) map[string]any {
	t.Helper()
	var got []map[string]any
	for _, l := range traceLines(t, f.logs) {
		if l["op"] == "session.drop_in" {
			got = append(got, l)
		}
	}
	if len(got) != 1 {
		t.Fatalf("session.drop_in lines = %d, want 1: %v", len(got), got)
	}
	l := got[0]
	if l["status"] != status || l["error"] != code || l["session_id"] != dropInTestSessionID || l["host"] != "console" {
		t.Fatalf("log line = %v, want status=%s error=%q session_id=%s host=console", l, status, code, dropInTestSessionID)
	}
	return l
}

func profileFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(sessionProfilesDir(), "*.sb"))
	assertNoErr(t, err, "glob profiles")
	return files
}

func TestDropIn_SuccessLaunchesTerminalResumingTheConversation(t *testing.T) {
	claudeID := uuid.NewString()
	f := newDropInFixture(t, claudeID)

	rec := f.post(t, dropInTestSessionID)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", rec.Code, rec.Body.String())
	}
	var body struct {
		SessionID       string          `json:"sessionId"`
		ClaudeSessionID string          `json:"claudeSessionId"`
		Host            string          `json:"host"`
		Terminal        json.RawMessage `json:"terminal"`
	}
	assertNoErr(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode")
	if body.SessionID != dropInTestSessionID || body.ClaudeSessionID != claudeID || body.Host != "" || len(body.Terminal) < 3 {
		t.Fatalf("body = %s, want sessionId, claudeSessionId=%s, no host, a terminal object", rec.Body.String(), claudeID)
	}

	if len(f.host.launchSpecs) != 1 {
		t.Fatalf("host launches = %d, want 1", len(f.host.launchSpecs))
	}
	spec := f.host.launchSpecs[0]
	n := len(spec.Argv)
	if spec.Kind != KindPTY || n < 3 || spec.Argv[n-2] != "--resume" || spec.Argv[n-1] != claudeID {
		t.Fatalf("launched kind=%q argv=%v, want a pty ending --resume %s", spec.Kind, spec.Argv, claudeID)
	}
	if spec.DropInFor != dropInTestSessionID {
		t.Fatalf("drop_in_for = %q, want %q", spec.DropInFor, dropInTestSessionID)
	}
	if files := profileFiles(t); len(files) != 1 {
		t.Fatalf("sandbox profiles after a drop-in = %v, want the terminal's one (guards the refusal tests' profile checks)", files)
	}
	if n := f.host.calls("/handback"); n != 0 {
		t.Fatalf("handback calls = %d on success; the terminal's exit hands back", n)
	}
	l := f.requireOneLogLine(t, "ok", "")
	if l["terminal_id"] != spec.SessionID || spec.SessionID == "" {
		t.Fatalf("log terminal_id = %v, want the launched terminal %q", l["terminal_id"], spec.SessionID)
	}
}

func TestDropIn_RefusesBeforeTouchingTheHost(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(t *testing.T, f *dropInFixture) string
		status   int
		code     string
		logState string
	}{
		{"unknown session", func(*testing.T, *dropInFixture) string { return "nope" }, 404, "session_not_found", "denied"},
		{"not a claude session", func(t *testing.T, f *dropInFixture) string {
			assertNoErr(t, f.deps.sessions.Put(ledger.Record{
				SessionID: dropInTestSessionID, Kind: KindChat, ProjectID: f.proj.ID, Directory: f.proj.Path, State: ledger.StateLive,
			}), "reseed as chat")
			return dropInTestSessionID
		}, 409, "not_claude", "denied"},
		{"project does not allow the claude-code template", func(t *testing.T, f *dropInFixture) string {
			assertNoErr(t, f.store.With(func(s *config.Settings) { s.Projects[0].AllowedTemplates = []string{"shell"} }), "narrow templates")
			return dropInTestSessionID
		}, 403, "", "denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newDropInFixture(t, uuid.NewString())
			id := c.setup(t, f)

			rec := f.post(t, id)
			if rec.Code != c.status {
				t.Fatalf("status = %d, body = %s, want %d", rec.Code, rec.Body.String(), c.status)
			}
			code := f.errorCode(t, rec)
			if c.code != "" && code != c.code {
				t.Fatalf("error = %q, want %q", code, c.code)
			}
			if got := f.host.paths; len(got) != 0 {
				t.Fatalf("host was contacted (%v) although the request was refused; nothing may be stopped", got)
			}
			if files := profileFiles(t); len(files) != 0 {
				t.Fatalf("refused drop-in left sandbox profiles %v", files)
			}
			var lines int
			for _, l := range traceLines(t, f.logs) {
				if l["op"] == "session.drop_in" {
					lines++
					if l["status"] != c.logState || l["error"] != code || l["session_id"] != id {
						t.Fatalf("log line = %v, want status=%s error=%q session_id=%s", l, c.logState, code, id)
					}
				}
			}
			if lines != 1 {
				t.Fatalf("session.drop_in lines = %d, want 1", lines)
			}
		})
	}
}

func TestDropIn_HostRefusalIsA409AndLeavesNoProfile(t *testing.T) {
	f := newDropInFixture(t, uuid.NewString())
	f.host.handoff = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(hostapi.ErrorResponse{Error: "tool_running", Message: "a tool is running (Bash)"})
	}

	rec := f.post(t, dropInTestSessionID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s, want 409", rec.Code, rec.Body.String())
	}
	var body map[string]string
	assertNoErr(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode")
	if body["error"] != "tool_running" || !strings.Contains(body["message"], "Bash") {
		t.Fatalf("body = %v, want error tool_running with the host's message", body)
	}
	if files := profileFiles(t); len(files) != 0 {
		t.Fatalf("refused handoff left sandbox profiles %v", files)
	}
	if n := f.host.calls("/launch"); n != 0 {
		t.Fatalf("launched %d terminal(s) after a refused handoff", n)
	}
	f.requireOneLogLine(t, "denied", "tool_running")
}

func TestDropIn_FailureAfterHandoffHandsTheSessionBack(t *testing.T) {
	cases := []struct {
		name     string
		claudeID string
		tweak    func(h *dropInHost)
		code     string
	}{
		{"conversation id is not a UUID", "--dangerous-flag", nil, "launch_failed"},
		{"terminal launch fails", "", func(h *dropInHost) { h.launchCode = http.StatusInternalServerError }, "launch_failed"},
		{"handoff transport error", "", func(h *dropInHost) {
			h.handoff = func(w http.ResponseWriter) { http.Error(w, "boom", http.StatusInternalServerError) }
		}, "unavailable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := c.claudeID
			if id == "" {
				id = uuid.NewString()
			}
			f := newDropInFixture(t, id)
			if c.tweak != nil {
				c.tweak(f.host)
			}

			rec := f.post(t, dropInTestSessionID)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, body = %s, want 502", rec.Code, rec.Body.String())
			}
			if got := f.errorCode(t, rec); got != c.code {
				t.Fatalf("error = %q, want %q", got, c.code)
			}
			if n := f.host.calls("/handback"); n != 1 {
				t.Fatalf("handback calls = %d, want 1: a held session must not be left held", n)
			}
			if c.claudeID != "" && f.host.calls("/launch") != 0 {
				t.Fatalf("launched a terminal with the invalid conversation id %q", c.claudeID)
			}
			f.requireOneLogLine(t, "error", c.code)
		})
	}
}

func TestDropIn_RequestWithoutASessionHostFailsClosed(t *testing.T) {
	f := newSessionRoutesFixture(t)
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{
		SessionID: dropInTestSessionID, Kind: KindClaude, ProjectID: f.proj.ID, Directory: f.proj.Path, State: ledger.StateLive,
	}), "seed")
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+dropInTestSessionID+"/drop-in", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, f.withExecuteCredential(t, req))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", rec.Code, rec.Body.String())
	}
	if files := profileFiles(t); len(files) != 0 {
		t.Fatalf("left sandbox profiles %v", files)
	}
	var body map[string]string
	assertNoErr(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode")
	if body["error"] != "unavailable" {
		t.Fatalf("error = %q, want unavailable", body["error"])
	}
}
