package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

func newBlankModelFixture(t *testing.T) (*sessionRoutesFixture, *FakeService) {
	t.Helper()
	f := newSessionRoutesFixture(t, newTestAudit(t, nil))
	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler:   fakeLaunchHandler(&fs, func(id string) string { return `{"sessionId":"` + id + `"}` }),
	})
	f.registerFakeSessionsHost(t, fs, selfPeerToken(t).Process())
	return f, fs
}

func (f *sessionRoutesFixture) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func assertBlankModelRefused(t *testing.T, path, name, modelField, wantMsg string) {
	t.Helper()
	f, fs := newBlankModelFixture(t)
	body := `{"projectId":"` + f.proj.ID + `","name":"` + name + `"` + modelField + `}`
	rec := f.post(t, path, body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rec.Code, rec.Body.String())
	}
	var got struct {
		Error string `json:"error"`
	}
	assertNoErr(t, json.Unmarshal(rec.Body.Bytes(), &got), "decode error body")
	if got.Error != wantMsg {
		t.Fatalf("error = %q, want %q", got.Error, wantMsg)
	}
	if n := len(fs.Requests()); n != 0 {
		t.Fatalf("host received %d request(s) for a refused launch", n)
	}
	if recs := f.deps.sessions.All(); len(recs) != 0 {
		t.Fatalf("ledger holds %d record(s) after a refused launch: %+v", len(recs), recs)
	}
	f.deps.accounting.mu.Lock()
	tracked := len(f.deps.accounting.byID)
	f.deps.accounting.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("accounting tracks %d session(s) after a refused launch", tracked)
	}
	var launches []audit.AuditEvent
	for _, ev := range readLoggedEvents(t, f.deps.auditor) {
		if ev.Event == audit.AuditEventSessionLaunch {
			launches = append(launches, ev)
		}
	}
	if len(launches) != 1 || launches[0].Outcome != audit.AuditOutcomeError || launches[0].Error != wantMsg {
		t.Fatalf("session_launch audit = %+v, want one error record carrying %q", launches, wantMsg)
	}
}

func TestCreateSession_BlankModel_Refused(t *testing.T) {
	const want = `chat session "Daily digest" has no model; choose a model and try again`
	for _, path := range []string{"/api/sessions", "/api/sessions/"} {
		for _, tc := range []struct{ name, modelField string }{
			{"empty", `,"model":""`},
			{"missing", ``},
			{"whitespace", `,"model":"   "`},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				assertBlankModelRefused(t, path, "Daily digest", tc.modelField, want)
			})
		}
	}
}

func TestCreateSession_BlankModel_NoName(t *testing.T) {
	assertBlankModelRefused(t, "/api/sessions", "", `,"model":""`,
		"chat session has no model; choose a model and try again")
}

func TestAuthorizeLaunch_BlankChatModel_RefusedBeforeMint(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)

	mints := 0
	original := mintModelKey
	t.Cleanup(func() { mintModelKey = original })
	mintModelKey = func(*ModelKeyTable, string, string) (string, error) {
		mints++
		return "rmk_test", nil
	}

	const want = `chat session "Daily digest" has no model; choose a model and try again`
	result, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindChat, Name: "Daily digest", Model: "",
	})
	if refusal == nil {
		t.Fatalf("blank chat model was authorized as session %s (model key mints: %d)", result.SessionID, mints)
	}
	if refusal.Status != http.StatusBadRequest || refusal.Code != "model_required" || refusal.Message != want {
		t.Fatalf("refusal = {%d %q %q}, want {400 model_required %q}", refusal.Status, refusal.Code, refusal.Message, want)
	}
	if ev := refusal.Audit; ev.Event != audit.AuditEventSessionLaunch || ev.Outcome != audit.AuditOutcomeError || ev.Error != want {
		t.Fatalf("refusal audit = {%q %q %q}, want a session_launch error carrying the message", ev.Event, ev.Outcome, ev.Error)
	}
	if mints != 0 {
		t.Fatalf("model key minted %d time(s) for a refused launch", mints)
	}
	entries, err := os.ReadDir(sessionProfilesDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read profiles dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused launch left %d profile(s) behind", len(entries))
	}
}

func TestAuthorizeLaunch_BlankChatModel_CallerCheckStillFirst(t *testing.T) {
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)

	_, refusal := AuthorizeLaunch(store, NewModelKeyTable(), newLaunchTestLedger(t), LaunchRequest{
		Caller: bearerCaller(control.ClassRead), ProjectID: proj.ID, Kind: KindChat, Name: "Daily digest",
	})
	if refusal == nil || refusal.Status != http.StatusForbidden || refusal.Code != "caller_not_authorized" {
		t.Fatalf("refusal = %+v, want 403 caller_not_authorized", refusal)
	}
}

func TestResumeSession_BlankStoredModel_NotRefused(t *testing.T) {
	f, fs := newBlankModelFixture(t)
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{
		SessionID: "s1", Kind: KindChat, ProjectID: f.proj.ID, Directory: f.proj.Path, State: ledger.StateDormant,
		SessionRequest: json.RawMessage(`{"projectId":"` + f.proj.ID + `","directory":"` + f.proj.Path + `","model":""}`),
	}), "seed dormant record")

	rec := f.post(t, "/api/sessions/s1/resume", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var body resumeResponseBody
	assertNoErr(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode resume response")
	if body.SessionID != "s1" || !body.Resumed {
		t.Fatalf("resume response = %+v, want {s1 true}", body)
	}
	last := fs.LastRequest()
	if last == nil || last.Path != "/launch" {
		t.Fatalf("host last request = %+v, want /launch", last)
	}
	var spec hostapi.LaunchRequest
	assertNoErr(t, json.Unmarshal(last.Body, &spec), "decode launch spec")
	if !spec.Resume || spec.Kind != KindChat {
		t.Fatalf("launch spec resume=%v kind=%q, want a chat resume", spec.Resume, spec.Kind)
	}
}

func TestCreateSession_NamedModel_Launches(t *testing.T) {
	f, fs := newBlankModelFixture(t)
	rec := f.post(t, "/api/sessions", `{"projectId":"`+f.proj.ID+`","name":"Daily digest","model":"gpt-5"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", rec.Code, rec.Body.String())
	}
	var spec hostapi.LaunchRequest
	assertNoErr(t, json.Unmarshal(fs.LastRequest().Body, &spec), "decode launch spec")
	var sr struct {
		Model string `json:"model"`
	}
	assertNoErr(t, json.Unmarshal(spec.SessionRequest, &sr), "decode session_request")
	if spec.Kind != KindChat || sr.Model != "gpt-5" {
		t.Fatalf("launch spec kind=%q model=%q, want chat gpt-5", spec.Kind, sr.Model)
	}
}
