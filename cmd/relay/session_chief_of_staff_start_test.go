package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

const cosStartRoute = "/api/chief-of-staff/sessions"

// cosStartFx is the real session routes in front of a fake relay-sessions.
type cosStartFx struct {
	*sessionRoutesFixture
	rec  *audit.AuditRecorder
	host *FakeService

	mu        sync.Mutex
	sendReply int // status the fake /send answers; 0 means 202
}

func newCoSStartFx(t *testing.T) *cosStartFx {
	t.Helper()
	return newCoSStartFxWith(t, newCoSRecorder(t, &config.AuditConfig{}))
}

func newCoSStartFxWith(t *testing.T, rec *audit.AuditRecorder) *cosStartFx {
	t.Helper()
	x := &cosStartFx{sessionRoutesFixture: newSessionRoutesFixture(t, rec), rec: rec}
	launch := fakeLaunchHandler(&x.host, func(id string) string { return `{"sessionId":"` + id + `","terminalId":"` + id + `"}` })
	x.host = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID, Manifest: fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/launch":
				launch(w, r)
			case "/send":
				x.mu.Lock()
				status := x.sendReply
				x.mu.Unlock()
				if status == 0 || status == http.StatusAccepted {
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(hostapi.SendResponse{At: "2026-10-05T14:03:07.141Z"})
					return
				}
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(hostapi.ErrorResponse{Error: hostapi.ErrSendFailed, Message: "m"})
			case "/terminate":
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		},
	})
	x.registerFakeSessionsHost(t, x.host, selfPeerToken(t).Process())
	return x
}

func (x *cosStartFx) calls(path string) []*fakeServiceRequest {
	var out []*fakeServiceRequest
	for _, r := range x.host.Requests() {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (x *cosStartFx) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := x.withExecuteCredential(t, httptest.NewRequest(http.MethodPost, cosStartRoute, strings.NewReader(body)))
	w := httptest.NewRecorder()
	x.mux.ServeHTTP(w, req)
	return w
}

func startBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	b := map[string]any{"projectId": "p1", "prompt": "do the thing", "model": "haiku"}
	for k, v := range fields {
		b[k] = v
	}
	data, err := json.Marshal(b)
	assertNoErr(t, err, "marshal start body")
	return string(data)
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &body), "decode error body: "+w.Body.String())
	if body["message"] == "" {
		t.Fatalf("error body %v has no message", body)
	}
	return body["error"]
}

func launchRows(t *testing.T, rec *audit.AuditRecorder) []audit.AuditEvent {
	t.Helper()
	var out []audit.AuditEvent
	for _, ev := range readLoggedEvents(t, rec) {
		if ev.Event == audit.AuditEventSessionLaunch {
			out = append(out, ev)
		}
	}
	return out
}

func TestChiefOfStaffStart_Refusals(t *testing.T) {
	over := strings.Repeat("a", 64<<10)
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, x *cosStartFx)
		body    func(t *testing.T) string
		status  int
		code    string
		audited bool // a denied session_launch row with origin is written
	}{
		{"not json", nil, func(*testing.T) string { return `{nope` }, 400, "invalid_body", false},
		{"over 64 KiB", nil, func(*testing.T) string { return `{"prompt":"` + over + `"}` }, 413, "body_too_large", false},
		{"no project", nil, func(t *testing.T) string { return startBody(t, map[string]any{"projectId": ""}) }, 400, "project_id_required", false},
		{"blank prompt", nil, func(t *testing.T) string { return startBody(t, map[string]any{"prompt": " \n\t "}) }, 400, "prompt_required", false},
		{"prompt over 8000 runes", nil, func(t *testing.T) string {
			return startBody(t, map[string]any{"prompt": strings.Repeat("é", 8001)})
		}, 400, "prompt_too_long", false},
		{"no model", nil, func(t *testing.T) string { return startBody(t, map[string]any{"model": ""}) }, 400, "model_required", false},
		{"unknown mode", nil, func(t *testing.T) string { return startBody(t, map[string]any{"mode": "tmux"}) }, 400, "mode_invalid", false},
		{"folder with ..", nil, func(t *testing.T) string { return startBody(t, map[string]any{"folder": "a/../b"}) }, 400, "folder_invalid", false},
		{"folder is ..", nil, func(t *testing.T) string { return startBody(t, map[string]any{"folder": ".."}) }, 400, "folder_invalid", false},
		{"absolute folder", nil, func(t *testing.T) string { return startBody(t, map[string]any{"folder": "/etc"}) }, 400, "folder_invalid", false},
		{"folder with NUL", nil, func(t *testing.T) string { return startBody(t, map[string]any{"folder": "a\x00b"}) }, 400, "folder_invalid", false},
		{"unknown project", nil, func(t *testing.T) string { return startBody(t, map[string]any{"projectId": "nope"}) }, 403, "project_not_available", true},
		{"remote project", func(t *testing.T, x *cosStartFx) {
			assertNoErr(t, x.store.With(func(s *config.Settings) {
				s.Projects = append(s.Projects, config.Project{ID: "r1", Name: "Remote", Kind: config.ProjectKindRemote})
			}), "seed remote project")
		}, func(t *testing.T) string { return startBody(t, map[string]any{"projectId": "r1"}) }, 403, "project_not_available", true},
		{"terminal start in a project on an SSH host", func(t *testing.T, x *cosStartFx) {
			seedHostedProject(t, x)
		}, func(t *testing.T) string {
			return startBody(t, map[string]any{"projectId": "h1p", "mode": "terminal"})
		}, 400, "terminal_on_host", true},
		{"missing folder", nil, func(t *testing.T) string { return startBody(t, map[string]any{"folder": "nope"}) }, 400, "folder_not_found", true},
		{"folder is a file", func(t *testing.T, x *cosStartFx) {
			assertNoErr(t, os.WriteFile(filepath.Join(x.proj.Path, "f.txt"), []byte("x"), 0o600), "seed file")
		}, func(t *testing.T) string { return startBody(t, map[string]any{"folder": "f.txt"}) }, 400, "folder_not_found", true},
		{"symlink inside the project pointing outside", func(t *testing.T, x *cosStartFx) {
			assertNoErr(t, os.Symlink(t.TempDir(), filepath.Join(x.proj.Path, "escape")), "seed symlink")
		}, func(t *testing.T) string { return startBody(t, map[string]any{"folder": "escape"}) }, 403, "directory_outside_project", true},
		{"terminal with a non-Claude model", nil, func(t *testing.T) string {
			return startBody(t, map[string]any{"mode": "terminal", "model": "gpt-5"})
		}, 400, "terminal_needs_claude", true},
		{"terminal with a model the project disallows", func(t *testing.T, x *cosStartFx) {
			assertNoErr(t, x.store.With(func(s *config.Settings) { s.Projects[0].AllowedModels = []string{"opus"} }), "narrow models")
		}, func(t *testing.T) string { return startBody(t, map[string]any{"mode": "terminal", "model": "haiku"}) }, 403, "model_not_allowed", true},
		{"headless with a model the project disallows", func(t *testing.T, x *cosStartFx) {
			assertNoErr(t, x.store.With(func(s *config.Settings) { s.Projects[0].AllowedModels = []string{"opus"} }), "narrow models")
		}, func(t *testing.T) string { return startBody(t, map[string]any{"model": "haiku"}) }, 403, "model_not_allowed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newCoSStartFx(t)
			if tc.setup != nil {
				tc.setup(t, x)
			}
			w := x.post(t, tc.body(t))
			if w.Code != tc.status || errorCode(t, w) != tc.code {
				t.Fatalf("got %d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
			if n := len(x.host.Requests()); n != 0 {
				t.Fatalf("a refused start reached the session host %d time(s)", n)
			}
			rows := launchRows(t, x.rec)
			if !tc.audited {
				if len(rows) != 0 {
					t.Fatalf("a body refused before the audit check left rows: %+v", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0].Outcome != audit.AuditOutcomeDenied {
				t.Fatalf("want one denied session_launch row, got %+v", rows)
			}
			if a := rowArgs(t, rows[0]); a["origin"] != "chief-of-staff" {
				t.Fatalf("denied row args = %v, want origin chief-of-staff", a)
			}
		})
	}
}

// Fail closed: with no working audit log nothing starts.
func TestChiefOfStaffStart_NoAuditMeans503AndNoLaunch(t *testing.T) {
	off := false
	for name, rec := range map[string]*audit.AuditRecorder{
		"not wired":    nil,
		"switched off": audit.NewAuditRecorderWith(audit.ResolveAuditConfig(&config.AuditConfig{Enabled: &off}), "off", failingWriteCloser{}),
	} {
		t.Run(name, func(t *testing.T) {
			x := newCoSStartFxWith(t, rec)
			w := x.post(t, startBody(t, nil))
			if w.Code != http.StatusServiceUnavailable || errorCode(t, w) != "audit_unavailable" {
				t.Fatalf("got %d %s, want 503 audit_unavailable", w.Code, w.Body.String())
			}
			if n := len(x.host.Requests()); n != 0 {
				t.Fatalf("host got %d request(s) with no audit log", n)
			}
		})
	}
}

type sentSession struct {
	Name     string                     `json:"name"`
	Model    string                     `json:"model"`
	Settings map[string]json.RawMessage `json:"settings"`
}

func launchSpec(t *testing.T, x *cosStartFx) (hostapi.LaunchRequest, sentSession) {
	t.Helper()
	calls := x.calls("/launch")
	if len(calls) != 1 {
		t.Fatalf("host got %d /launch request(s), want 1", len(calls))
	}
	var spec hostapi.LaunchRequest
	assertNoErr(t, json.Unmarshal(calls[0].Body, &spec), "decode launch spec")
	var sess sentSession
	if len(spec.SessionRequest) > 0 {
		assertNoErr(t, json.Unmarshal(spec.SessionRequest, &sess), "decode session_request")
	}
	return spec, sess
}

func TestChiefOfStaffStart_HeadlessLaunchesDeliversPromptAndMarksOrigin(t *testing.T) {
	x := newCoSStartFx(t)
	sub := filepath.Join(x.proj.Path, "svc", "api")
	assertNoErr(t, os.MkdirAll(sub, 0o755), "mkdir folder")

	// The body claims another origin and its own settings; none is read.
	w := x.post(t, startBody(t, map[string]any{
		"folder": "svc/api", "prompt": "  fix the build\nthen rerun  ",
		"origin": "person", "settings": map[string]any{"headless": false, "permissionMode": "plan"},
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	wantDir, _ := filepath.EvalSymlinks(sub)
	var got map[string]string
	assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &got), "decode 201")
	if got["sessionId"] == "" || got["at"] == "" || got["name"] != "fix the build" || got["projectId"] != "p1" ||
		got["directory"] != wantDir || got["mode"] != "headless" || got["kind"] != "claude" || got["origin"] != "chief-of-staff" {
		t.Fatalf("201 body = %v", got)
	}

	spec, sess := launchSpec(t, x)
	if spec.Origin != "chief-of-staff" || spec.Kind != "claude" || spec.SessionID != got["sessionId"] || spec.Directory != wantDir {
		t.Fatalf("launch spec = %+v, want origin chief-of-staff, kind claude, the response's session and directory", spec)
	}
	if string(sess.Settings["headless"]) != "true" || string(sess.Settings["agent"]) != "true" || sess.Model != "haiku" {
		t.Fatalf("session settings = %v model %q, want headless and agent true and model haiku", sess.Settings, sess.Model)
	}
	if _, ok := sess.Settings["permissionMode"]; ok {
		t.Fatalf("a caller-named setting reached the host: %v", sess.Settings)
	}

	sends := x.calls("/send")
	if len(sends) != 1 {
		t.Fatalf("host got %d /send request(s), want 1", len(sends))
	}
	var wire map[string]any
	assertNoErr(t, json.Unmarshal(sends[0].Body, &wire), "decode send")
	if wire["session_id"] != got["sessionId"] || wire["text"] != "fix the build\nthen rerun" || wire["origin"] != "chief-of-staff" {
		t.Fatalf("send = %v, want the trimmed prompt for the new session with origin chief-of-staff", wire)
	}

	if rec, ok := x.deps.sessions.Get(got["sessionId"]); !ok || rec.Origin != "chief-of-staff" {
		t.Fatalf("ledger record = %+v, ok = %v, want origin chief-of-staff", rec, ok)
	}
	if len(x.calls("/terminate")) != 0 {
		t.Fatal("a delivered start was terminated")
	}
}

func TestChiefOfStaffStart_NameFromFirstLine(t *testing.T) {
	for _, tc := range []struct{ name, prompt, want string }{
		{"first line only", "one\ntwo", "one"},
		{"whitespace collapsed", "  a \t b   c  \nrest", "a b c"},
		{"cut at 60 runes", strings.Repeat("é", 70), strings.Repeat("é", 60)},
		{"60 runes kept whole", strings.Repeat("é", 60), strings.Repeat("é", 60)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newCoSStartFx(t)
			w := x.post(t, startBody(t, map[string]any{"prompt": tc.prompt}))
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			_, sess := launchSpec(t, x)
			var got map[string]string
			assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &got), "decode 201")
			if sess.Name != tc.want || got["name"] != tc.want || utf8.RuneCountInString(sess.Name) > 60 {
				t.Fatalf("host name %q, response name %q, want %q", sess.Name, got["name"], tc.want)
			}
		})
	}
}

func TestChiefOfStaffStart_TerminalRunsClaudeWithModelAndPromptArgs(t *testing.T) {
	x := newCoSStartFx(t)
	w := x.post(t, startBody(t, map[string]any{"mode": "terminal", "prompt": "-rf look at this", "model": "sonnet"}))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got map[string]string
	assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &got), "decode 201")
	if got["mode"] != "terminal" || got["kind"] != "pty" || got["origin"] != "chief-of-staff" {
		t.Fatalf("201 body = %v", got)
	}
	spec, _ := launchSpec(t, x)
	if spec.Kind != "pty" || spec.TemplateID != "claude-code" || spec.Origin != "chief-of-staff" {
		t.Fatalf("launch spec = %+v, want pty, claude-code, origin chief-of-staff", spec)
	}
	want := []string{"--model", "sonnet", "--", "-rf look at this"}
	argv := spec.Argv
	if len(argv) < len(want) {
		t.Fatalf("argv = %q, want it to end with %q", argv, want)
	}
	if tail := argv[len(argv)-len(want):]; strings.Join(tail, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want it to end with %q", argv, want)
	}
	if n := len(x.calls("/send")); n != 0 {
		t.Fatalf("a terminal start sent %d message(s); the prompt rides in argv", n)
	}
}

func TestChiefOfStaffStart_UndeliveredPromptEndsTheSession(t *testing.T) {
	x := newCoSStartFx(t)
	x.sendReply = http.StatusInternalServerError

	w := x.post(t, startBody(t, nil))
	if w.Code != http.StatusBadGateway || errorCode(t, w) != "prompt_not_delivered" {
		t.Fatalf("got %d %s, want 502 prompt_not_delivered", w.Code, w.Body.String())
	}
	spec, _ := launchSpec(t, x)
	ends := x.calls("/terminate")
	if len(ends) != 1 {
		t.Fatalf("host got %d /terminate request(s), want 1", len(ends))
	}
	var term hostapi.TerminateRequest
	assertNoErr(t, json.Unmarshal(ends[0].Body, &term), "decode terminate")
	if term.SessionID != spec.SessionID || term.Reason != "chief_of_staff_start_failed" {
		t.Fatalf("terminate = %+v, want session %s reason chief_of_staff_start_failed", term, spec.SessionID)
	}
}

// A system-only model is refused for a headless start, and the denied row
// carries the same origin and prompt size as every other start refusal.
func TestChiefOfStaffStart_SystemModelRefusalRowCarriesOrigin(t *testing.T) {
	x := newCoSStartFx(t)
	sys := &fakeSystemModel{system: true}
	req := x.withExecuteCredential(t, httptest.NewRequest(http.MethodPost, cosStartRoute,
		strings.NewReader(startBody(t, map[string]any{"model": "acme-sys", "prompt": "  héllo  "}))))
	w := httptest.NewRecorder()
	x.muxWithSystemModel(sys.lookup).ServeHTTP(w, req)

	if w.Code != http.StatusForbidden || errorCode(t, w) != "model_system_only" {
		t.Fatalf("got %d %s, want 403 model_system_only", w.Code, w.Body.String())
	}
	rows := launchRows(t, x.rec)
	if len(rows) != 1 || rows[0].Outcome != "denied" {
		t.Fatalf("want one denied session_launch row, got %+v", rows)
	}
	a := rowArgs(t, rows[0])
	if a["origin"] != "chief-of-staff" || a["prompt_bytes"] != float64(len("héllo")) {
		t.Fatalf("denied row args = %v, want origin chief-of-staff and prompt_bytes %d", a, len("héllo"))
	}
	if got := x.calls("/launch"); len(got) != 0 {
		t.Fatalf("a refused start reached the host: %d launch requests", len(got))
	}
}

// hookedRecorder runs onHeader the moment the handler starts its response.
type hookedRecorder struct {
	*httptest.ResponseRecorder
	onHeader func()
	once     sync.Once
}

func (h *hookedRecorder) WriteHeader(code int) {
	h.once.Do(h.onHeader)
	h.ResponseRecorder.WriteHeader(code)
}

func (h *hookedRecorder) Write(b []byte) (int, error) {
	h.once.Do(h.onHeader)
	return h.ResponseRecorder.Write(b)
}

// Both outcomes are on the log before the caller hears of them.
func TestChiefOfStaffStart_AuditRowsAreWrittenBeforeTheResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   map[string]any
		status int
		rows   []string // event/phase/outcome of every row expected at response time
	}{
		{"started", nil, 201, []string{
			"session_launch//ok", "session_message/intent/pending", "session_message/completion/ok",
		}},
		{"refused", map[string]any{"projectId": "nope"}, 403, []string{"session_launch//denied"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newCoSStartFx(t)
			req := x.withExecuteCredential(t, httptest.NewRequest(http.MethodPost, cosStartRoute, strings.NewReader(startBody(t, tc.body))))
			var atResponse []string
			var origins []any
			w := &hookedRecorder{ResponseRecorder: httptest.NewRecorder(), onHeader: func() {
				for _, ev := range readLoggedEvents(t, x.rec) {
					atResponse = append(atResponse, string(ev.Event)+"/"+string(ev.Phase)+"/"+string(ev.Outcome))
					origins = append(origins, rowArgs(t, ev)["origin"])
				}
			}}
			x.mux.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			if strings.Join(atResponse, ",") != strings.Join(tc.rows, ",") {
				t.Fatalf("rows on the log when the response began = %v, want %v", atResponse, tc.rows)
			}
			for i, o := range origins {
				if o != "chief-of-staff" {
					t.Fatalf("row %d (%s) origin = %v, want chief-of-staff", i, atResponse[i], o)
				}
			}
		})
	}
}

func TestChiefOfStaffStart_LaunchRowRecordsOriginAndPromptSize(t *testing.T) {
	x := newCoSStartFx(t)
	w := x.post(t, startBody(t, map[string]any{"prompt": "  héllo  "}))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	rows := launchRows(t, x.rec)
	if len(rows) != 1 {
		t.Fatalf("want one session_launch row, got %+v", rows)
	}
	a := rowArgs(t, rows[0])
	if a["origin"] != "chief-of-staff" || a["prompt_bytes"] != float64(len("héllo")) {
		t.Fatalf("launch row args = %v, want origin chief-of-staff and prompt_bytes %d", a, len("héllo"))
	}
}

// A resume carries the origin the ledger recorded, to the host and back into
// the record.
func TestChiefOfStaffStart_ResumeKeepsTheLedgerOrigin(t *testing.T) {
	x := newCoSStartFx(t)
	w := x.post(t, startBody(t, nil))
	if w.Code != http.StatusCreated {
		t.Fatalf("start: status = %d, body = %s", w.Code, w.Body.String())
	}
	spec, _ := launchSpec(t, x)
	id := spec.SessionID
	rec, _ := x.deps.sessions.Get(id)
	rec.State = ledger.StateDormant
	assertNoErr(t, x.deps.sessions.Put(rec), "make the record dormant")

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+id+"/resume", nil)
	req.SetPathValue("id", id)
	rw := httptest.NewRecorder()
	x.mux.ServeHTTP(rw, x.withExecuteCredential(t, req))
	if rw.Code != http.StatusOK {
		t.Fatalf("resume: status = %d, body = %s", rw.Code, rw.Body.String())
	}
	launches := x.calls("/launch")
	if len(launches) != 2 {
		t.Fatalf("host got %d /launch request(s), want start and resume", len(launches))
	}
	var resumed hostapi.LaunchRequest
	assertNoErr(t, json.Unmarshal(launches[1].Body, &resumed), "decode resume launch")
	if !resumed.Resume || resumed.Origin != "chief-of-staff" {
		t.Fatalf("resume launch = resume %v origin %q, want origin chief-of-staff", resumed.Resume, resumed.Origin)
	}
	if got, _ := x.deps.sessions.Get(id); got.Origin != "chief-of-staff" {
		t.Fatalf("ledger origin after resume = %q", got.Origin)
	}
}

func seedHostedProject(t *testing.T, x *cosStartFx) {
	t.Helper()
	assertNoErr(t, x.store.With(func(s *config.Settings) {
		s.Hosts = append(s.Hosts, config.Host{ID: "h1", Name: "testbox", Target: "testbox.example",
			Probe:             &config.HostProbe{OK: true, ClaudePath: "/usr/bin/claude"},
			TerminalTemplates: []config.TerminalTemplate{{ID: "claude-code", Name: "Claude Code", Command: "claude"}}})
		s.Projects = append(s.Projects, config.Project{ID: "h1p", Name: "Hosted", Path: "/srv/acme", HostID: "h1", AllowedModels: []string{"*"}})
	}), "seed hosted project")
}

func TestChiefOfStaffStart_HostedHeadlessStartsOnTheHost(t *testing.T) {
	x := newCoSStartFx(t)
	seedHostedProject(t, x)

	w := x.post(t, startBody(t, map[string]any{"projectId": "h1p", "folder": "svc/api"}))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got map[string]string
	assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &got), "decode 201")
	if got["directory"] != "/srv/acme/svc/api" || got["mode"] != "headless" || got["origin"] != "chief-of-staff" {
		t.Fatalf("201 body = %v", got)
	}

	calls := x.calls("/launch")
	if len(calls) != 1 {
		t.Fatalf("host got %d /launch request(s), want 1", len(calls))
	}
	var spec struct {
		Directory string `json:"directory"`
		Host      *struct {
			ID string `json:"id"`
		} `json:"host"`
	}
	assertNoErr(t, json.Unmarshal(calls[0].Body, &spec), "decode launch spec")
	if spec.Host == nil || spec.Host.ID != "h1" || spec.Directory != "/srv/acme/svc/api" {
		t.Fatalf("launch spec = %s, want host h1 and directory /srv/acme/svc/api", calls[0].Body)
	}

	sends := x.calls("/send")
	if len(sends) != 1 {
		t.Fatalf("host got %d /send request(s), want 1", len(sends))
	}
	var wire map[string]any
	assertNoErr(t, json.Unmarshal(sends[0].Body, &wire), "decode send")
	if wire["session_id"] != got["sessionId"] || wire["origin"] != "chief-of-staff" {
		t.Fatalf("send = %v, want origin chief-of-staff for the new session", wire)
	}

	rows := launchRows(t, x.rec)
	if len(rows) != 1 || rows[0].Outcome != audit.AuditOutcomeOK {
		t.Fatalf("want one ok session_launch row, got %+v", rows)
	}
	if a := rowArgs(t, rows[0]); a["origin"] != "chief-of-staff" || a["host_id"] != "h1" {
		t.Fatalf("launch row args = %v, want origin chief-of-staff and host_id h1", a)
	}
}

// A hosted folder is checked as text. A folder that climbs out of the project
// path never reaches the host. The syntax check answers before the containment
// check can, so the code is folder_invalid, not directory_outside_project.
func TestChiefOfStaffStart_HostedFolderOutsideProjectIsRefused(t *testing.T) {
	x := newCoSStartFx(t)
	seedHostedProject(t, x)

	w := x.post(t, startBody(t, map[string]any{"projectId": "h1p", "folder": "../etc"}))
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "folder_invalid" {
		t.Fatalf("got %d %s, want 400 folder_invalid", w.Code, w.Body.String())
	}
	if n := len(x.host.Requests()); n != 0 {
		t.Fatalf("a refused start reached the session host %d time(s)", n)
	}
}

// A terminal start in a hosted project is refused before the model is looked
// at, so a non-Claude model gets terminal_on_host too, and the route's own
// denied row names the host.
func TestChiefOfStaffStart_HostedTerminalRefusalNamesTheHost(t *testing.T) {
	x := newCoSStartFx(t)
	seedHostedProject(t, x)

	w := x.post(t, startBody(t, map[string]any{"projectId": "h1p", "mode": "terminal", "model": "gpt-5"}))
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "terminal_on_host" {
		t.Fatalf("got %d %s, want 400 terminal_on_host", w.Code, w.Body.String())
	}
	rows := launchRows(t, x.rec)
	if len(rows) != 1 || rows[0].Outcome != audit.AuditOutcomeDenied {
		t.Fatalf("want one denied session_launch row, got %+v", rows)
	}
	if a := rowArgs(t, rows[0]); a["origin"] != "chief-of-staff" || a["host_id"] != "h1" {
		t.Fatalf("denied row args = %v, want origin chief-of-staff and host_id h1", a)
	}
}
