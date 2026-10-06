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
)

const cosRoute = "/api/chief-of-staff/messages"

// cosHost is a fake relay-sessions whose /send answer a test scripts. It
// records whether the intent row was already on disk when /send arrived.
type cosHost struct {
	svc *FakeService

	mu           sync.Mutex
	intentOnDisk bool
}

func (h *cosHost) sendRequests() []*fakeServiceRequest {
	var out []*fakeServiceRequest
	for _, r := range h.svc.Requests() {
		if r.Path == "/send" {
			out = append(out, r)
		}
	}
	return out
}

// newCoSFixture wires the real session routes to a fake host. reply writes
// the host's answer; nil means a 202 with a fixed time.
func newCoSFixture(t *testing.T, rec *audit.AuditRecorder, reply func(w http.ResponseWriter)) (*sessionRoutesFixture, *cosHost) {
	t.Helper()
	f := newSessionRoutesFixture(t, rec)
	h := &cosHost{}
	h.svc = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			if rec != nil {
				if data, err := os.ReadFile(rec.Path()); err == nil {
					h.mu.Lock()
					h.intentOnDisk = strings.Contains(string(data), `"phase":"intent"`)
					h.mu.Unlock()
				}
			}
			if reply != nil {
				reply(w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(hostapi.SendResponse{SessionID: "ignored", Origin: "ignored", At: "2026-10-05T14:03:07.141Z"})
		},
	})
	f.registerFakeSessionsHost(t, h.svc, selfPeerToken(t).Process())
	return f, h
}

func newCoSRecorder(t *testing.T, cfg *config.AuditConfig) *audit.AuditRecorder {
	t.Helper()
	rec, err := audit.NewAuditRecorder(cfg, filepath.Join(t.TempDir(), "audit", "toolcalls.jsonl"), openAuditWriter)
	assertNoErr(t, err, "NewAuditRecorder")
	t.Cleanup(rec.Close)
	return rec
}

func postCoS(f *sessionRoutesFixture, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, cosRoute, strings.NewReader(body))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func sessionMessageRows(t *testing.T, rec *audit.AuditRecorder) []audit.AuditEvent {
	t.Helper()
	var out []audit.AuditEvent
	for _, ev := range readLoggedEvents(t, rec) {
		if ev.Event == audit.AuditEventSessionMessage {
			out = append(out, ev)
		}
	}
	return out
}

func rowArgs(t *testing.T, ev audit.AuditEvent) map[string]any {
	t.Helper()
	var args map[string]any
	assertNoErr(t, json.Unmarshal(ev.Args, &args), "decode row args")
	return args
}

func TestChiefOfStaffSend_AcceptedStampsOriginAndRecordsIntentBeforeDelivery(t *testing.T) {
	rec := newCoSRecorder(t, &config.AuditConfig{})
	f, host := newCoSFixture(t, rec, nil)

	// A body that claims another origin, and carries fields nobody defined.
	w := postCoS(f, `{"sessionId":"s-1","text":"restart the job","origin":"person","extra":{"origin":"person"}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got map[string]string
	assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &got), "decode 202 body")
	if len(got) != 3 || got["sessionId"] != "s-1" || got["origin"] != "chief-of-staff" || got["at"] != "2026-10-05T14:03:07.141Z" {
		t.Fatalf("202 body = %v, want sessionId, origin chief-of-staff and the host's time", got)
	}

	sent := host.sendRequests()
	if len(sent) != 1 {
		t.Fatalf("host got %d /send requests, want 1", len(sent))
	}
	var wire map[string]any
	assertNoErr(t, json.Unmarshal(sent[0].Body, &wire), "decode host body")
	if len(wire) != 3 || wire["session_id"] != "s-1" || wire["text"] != "restart the job" || wire["origin"] != "chief-of-staff" {
		t.Fatalf("host received %v, want exactly {session_id, text, origin:chief-of-staff}", wire)
	}
	host.mu.Lock()
	onDisk := host.intentOnDisk
	host.mu.Unlock()
	if !onDisk {
		t.Fatal("the intent row was not durable when the host was called")
	}

	rows := sessionMessageRows(t, rec)
	if len(rows) != 2 || rows[0].Phase != audit.AuditPhaseIntent || rows[1].Phase != audit.AuditPhaseCompletion {
		t.Fatalf("want one intent then one completion row, got %+v", rows)
	}
	if rows[0].ID == "" || rows[0].ID != rows[1].ID {
		t.Fatalf("intent id %q and completion id %q must match", rows[0].ID, rows[1].ID)
	}
	if rows[0].TS.IsZero() || rows[1].TS.IsZero() {
		t.Fatal("rows carry no time")
	}
	if rows[0].Outcome != audit.AuditOutcomePending || rows[1].Outcome != audit.AuditOutcomeOK || rows[1].Error != "" {
		t.Fatalf("outcomes = %q, %q (error %q), want pending then ok", rows[0].Outcome, rows[1].Outcome, rows[1].Error)
	}
	for _, r := range rows {
		a := rowArgs(t, r)
		if a["session_id"] != "s-1" || a["origin"] != "chief-of-staff" {
			t.Fatalf("%s args = %v, want session s-1 and origin chief-of-staff", r.Phase, a)
		}
	}
}

func TestChiefOfStaffSend_HostRefusalsMapToCodesAndCompletionOutcomes(t *testing.T) {
	hostAnswers := func(status int, code string) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(hostapi.ErrorResponse{Error: code, Message: "m"})
		}
	}
	for _, tc := range []struct {
		name        string
		reply       func(http.ResponseWriter)
		noHost      bool
		wantStatus  int
		wantCode    string
		wantOutcome string
		wantError   string
	}{
		{"unknown or unlisted session", hostAnswers(404, hostapi.ErrSessionNotFound), false, 404, "session_not_found", audit.AuditOutcomeNotFound, ""},
		{"already processing", hostAnswers(409, hostapi.ErrAlreadyProcessing), false, 409, "already_processing", audit.AuditOutcomeError, "already_processing"},
		{"resume required", hostAnswers(409, hostapi.ErrResumeRequired), false, 409, "resume_required", audit.AuditOutcomeError, "resume_required"},
		{"host failed to send", hostAnswers(500, hostapi.ErrSendFailed), false, 502, "session_host_unavailable", audit.AuditOutcomeError, "send_failed"},
		{"host not reachable", nil, true, 502, "session_host_unavailable", audit.AuditOutcomeError, "session_host_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newCoSRecorder(t, &config.AuditConfig{})
			var f *sessionRoutesFixture
			if tc.noHost {
				f = newSessionRoutesFixture(t, rec)
			} else {
				f, _ = newCoSFixture(t, rec, tc.reply)
			}

			w := postCoS(f, `{"sessionId":"s-1","text":"hello"}`)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			var body map[string]string
			assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &body), "decode error body")
			if body["error"] != tc.wantCode || body["message"] == "" {
				t.Fatalf("error body = %v, want error %q with a message", body, tc.wantCode)
			}
			rows := sessionMessageRows(t, rec)
			if len(rows) != 2 {
				t.Fatalf("want intent and completion rows, got %+v", rows)
			}
			if rows[1].Phase != audit.AuditPhaseCompletion || rows[1].Outcome != tc.wantOutcome || rows[1].Error != tc.wantError {
				t.Fatalf("completion = phase %q outcome %q error %q, want outcome %q error %q",
					rows[1].Phase, rows[1].Outcome, rows[1].Error, tc.wantOutcome, tc.wantError)
			}
		})
	}
}

func TestChiefOfStaffSend_BadRequestsNeverReachTheHost(t *testing.T) {
	tooBig := `{"sessionId":"s-1","text":"` + strings.Repeat("a", 64<<10) + `"}`
	for _, tc := range []struct {
		name, body string
		wantStatus int
		wantCode   string
	}{
		{"not json", `{nope`, 400, "invalid_body"},
		{"empty body", ``, 400, "invalid_body"},
		{"no session id", `{"text":"hello"}`, 400, "session_id_required"},
		{"no text", `{"sessionId":"s-1"}`, 400, "text_required"},
		{"empty text", `{"sessionId":"s-1","text":""}`, 400, "text_required"},
		{"over 64 KiB", tooBig, 413, "body_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newCoSRecorder(t, &config.AuditConfig{})
			f, host := newCoSFixture(t, rec, nil)

			w := postCoS(f, tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			var body map[string]string
			assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &body), "decode error body")
			if body["error"] != tc.wantCode {
				t.Fatalf("error = %q, want %q", body["error"], tc.wantCode)
			}
			if n := len(host.sendRequests()); n != 0 {
				t.Fatalf("host got %d request(s) for a refused body", n)
			}
			if rows := sessionMessageRows(t, rec); len(rows) != 0 {
				t.Fatalf("a refused body left audit rows: %+v", rows)
			}
		})
	}
}

// Fail closed: with no durable record there is no message.
func TestChiefOfStaffSend_NoDurableAuditMeansNothingIsSent(t *testing.T) {
	brokenSink := audit.NewAuditRecorderWith(audit.ResolveAuditConfig(&config.AuditConfig{}), "unwritable", failingWriteCloser{})
	t.Cleanup(brokenSink.Close)
	disabled := false

	for _, tc := range []struct {
		name string
		rec  func(t *testing.T) *audit.AuditRecorder
	}{
		{"auditing not wired", func(*testing.T) *audit.AuditRecorder { return nil }},
		{"auditing switched off", func(t *testing.T) *audit.AuditRecorder {
			rec := audit.NewAuditRecorderWith(audit.ResolveAuditConfig(&config.AuditConfig{Enabled: &disabled}), "off", failingWriteCloser{})
			t.Cleanup(rec.Close)
			return rec
		}},
		{"intent write fails", func(*testing.T) *audit.AuditRecorder { return brokenSink }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, host := newCoSFixture(t, tc.rec(t), nil)

			w := postCoS(f, `{"sessionId":"s-1","text":"hello"}`)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			var body map[string]string
			assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &body), "decode error body")
			if body["error"] != "audit_unavailable" {
				t.Fatalf("error = %q, want audit_unavailable", body["error"])
			}
			if n := len(host.sendRequests()); n != 0 {
				t.Fatalf("host got %d request(s) with no audit record", n)
			}
		})
	}
}

func TestChiefOfStaffSend_TextInIntentFollowsLogArgsAndCap(t *testing.T) {
	off, on := false, true
	long := strings.Repeat("é", 10) // 20 bytes, every rune two bytes
	for _, tc := range []struct {
		name          string
		cfg           config.AuditConfig
		text          string
		wantText      bool
		wantTruncated bool
	}{
		{"log_args on", config.AuditConfig{LogArgs: &on}, "restart the job", true, false},
		{"log_args off", config.AuditConfig{LogArgs: &off}, "restart the job", false, false},
		{"cut inside a rune", config.AuditConfig{LogArgs: &on, MaxArgBytes: 5}, long, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newCoSRecorder(t, &tc.cfg)
			f, _ := newCoSFixture(t, rec, nil)

			body, _ := json.Marshal(map[string]string{"sessionId": "s-1", "text": tc.text})
			if w := postCoS(f, string(body)); w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			rows := sessionMessageRows(t, rec)
			if len(rows) != 2 {
				t.Fatalf("want 2 rows, got %+v", rows)
			}
			intent, completion := rowArgs(t, rows[0]), rowArgs(t, rows[1])

			if intent["text_bytes"] != float64(len(tc.text)) || completion["text_bytes"] != float64(len(tc.text)) {
				t.Errorf("text_bytes = %v / %v, want %d on both rows", intent["text_bytes"], completion["text_bytes"], len(tc.text))
			}
			if _, has := completion["text"]; has {
				t.Error("the completion row carries the text")
			}
			text, has := intent["text"].(string)
			if has != tc.wantText {
				t.Fatalf("intent text present = %v, want %v (%v)", has, tc.wantText, intent)
			}
			_, truncated := intent["text_truncated"]
			if truncated != tc.wantTruncated {
				t.Errorf("text_truncated present = %v, want %v", truncated, tc.wantTruncated)
			}
			if tc.wantText {
				if !utf8.ValidString(text) || !strings.HasPrefix(tc.text, text) || (tc.wantTruncated && len(text) > 5) {
					t.Errorf("intent text %q is not a rune-aligned prefix within the cap", text)
				}
				if !tc.wantTruncated && text != tc.text {
					t.Errorf("intent text = %q, want %q", text, tc.text)
				}
				if tc.wantTruncated && (len(text) == 0 || len(text) >= len(tc.text)) {
					t.Errorf("truncated text %q is empty or uncut", text)
				}
			}
		})
	}
}
