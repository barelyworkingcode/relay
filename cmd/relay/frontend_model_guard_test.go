package main

import (
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func restrictedProject(t *testing.T, store config.SettingsStore, models []string) config.Project {
	t.Helper()
	proj := createTestProject(t, store, "Restricted", t.TempDir(), []string{"fsmcp"})
	if err := store.With(func(s *config.Settings) {
		s.UpdateProjectModels(proj.ID, models)
	}); err != nil {
		t.Fatalf("UpdateProjectModels: %v", err)
	}
	return proj
}

func TestModelAllowedForProject(t *testing.T) {
	store := newProjectsTestStore(t)
	restricted := restrictedProject(t, store, []string{"haiku", "sonnet"})
	wildcard := createTestProject(t, store, "Wild", t.TempDir(), []string{"fsmcp"}) // models default to ["*"]

	cases := []struct {
		name      string
		projectID string
		model     string
		want      bool
	}{
		{"allowed model on restricted project", restricted.ID, "haiku", true},
		{"disallowed model on restricted project", restricted.ID, "opus", false},
		{"wildcard project allows any model", wildcard.ID, "opus", true},
		{"no project scope", "", "opus", true},
		{"server-default (empty) model", restricted.ID, "", true},
		{"unknown project falls open", "does-not-exist", "opus", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelAllowedForProject(store, tc.projectID, tc.model); got != tc.want {
				t.Errorf("modelAllowedForProject(%q, %q) = %v, want %v",
					tc.projectID, tc.model, got, tc.want)
			}
		})
	}
}

func TestModelAllowedForProject_EmptyAllowlistIsUnrestricted(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{})
	if !modelAllowedForProject(store, proj.ID, "opus") {
		t.Error("empty allowlist should be treated as unrestricted (allow all)")
	}
}

type nextSpy struct {
	called   bool
	gotBody  string
	statusTo int
}

func (n *nextSpy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.called = true
	b, _ := io.ReadAll(r.Body)
	n.gotBody = string(b)
	if n.statusTo == 0 {
		n.statusTo = http.StatusOK
	}
	w.WriteHeader(n.statusTo)
}

func postSessions(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestSessionModelGuard_BlocksDisallowedModel(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"haiku"})

	spy := &nextSpy{}
	guard := newSessionModelGuard(store, spy)
	rec := httptest.NewRecorder()
	guard(rec, postSessions(`{"projectId":"`+proj.ID+`","model":"opus"}`))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if spy.called {
		t.Error("disallowed model must not reach the dispatcher")
	}
}

func TestSessionModelGuard_ForwardsAllowedModelUntouched(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"haiku"})

	spy := &nextSpy{}
	guard := newSessionModelGuard(store, spy)
	rec := httptest.NewRecorder()
	body := `{"projectId":"` + proj.ID + `","model":"haiku","name":"x"}`
	guard(rec, postSessions(body))

	if !spy.called {
		t.Fatal("allowed model must be forwarded to the dispatcher")
	}
	if spy.gotBody != body {
		t.Errorf("forwarded body = %q, want %q (guard must restore the consumed body)", spy.gotBody, body)
	}
}

func TestSessionModelGuard_FailsOpenOnNonJSON(t *testing.T) {
	store := newProjectsTestStore(t)
	spy := &nextSpy{}
	guard := newSessionModelGuard(store, spy)
	rec := httptest.NewRecorder()
	guard(rec, postSessions(`not json`))

	if !spy.called {
		t.Error("unparseable body should be forwarded so relayLLM produces the error")
	}
}

// Go's ServeMux routes "POST /api/sessions/" to the catch-all, so a guard
// bound to the exact "POST /api/sessions" pattern would miss it.
func TestSessionModelGuard_BlocksDisallowedModel_TrailingSlash(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"haiku"})

	spy := &nextSpy{}
	guard := newSessionModelGuard(store, spy)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/", strings.NewReader(`{"projectId":"`+proj.ID+`","model":"opus"}`))
	req.Header.Set("Content-Type", "application/json")
	guard(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 on trailing-slash create path", rec.Code)
	}
	if spy.called {
		t.Error("disallowed model on /api/sessions/ must not reach the dispatcher")
	}
}

// Sub-resource POSTs never name a model and may carry bodies larger than the
// guard's buffer, which must not be read or truncated.
func TestSessionModelGuard_IgnoresSubResourcePath(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"haiku"})

	spy := &nextSpy{}
	guard := newSessionModelGuard(store, spy)
	rec := httptest.NewRecorder()
	body := `{"projectId":"` + proj.ID + `","model":"opus","text":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/abc123/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	guard(rec, req)

	if !spy.called {
		t.Fatal("sub-resource POST must be forwarded, not gated")
	}
	if spy.gotBody != body {
		t.Errorf("sub-resource body must pass through untouched; got %q want %q", spy.gotBody, body)
	}
}

// An oversized create body can't be fully inspected for its model field, so
// the guard must fail closed (413) rather than truncate-and-forward — padding
// a disallowed-model body past the cap must not slip it past the allowlist.
func TestSessionModelGuard_OversizedBodyFailsClosed(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"haiku"})

	spy := &nextSpy{}
	guard := newSessionModelGuard(store, spy)
	rec := httptest.NewRecorder()

	pad := strings.Repeat("A", maxSessionBodyBytes) // pushes total past the cap
	body := `{"projectId":"` + proj.ID + `","model":"opus","pad":"` + pad + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	guard(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 for oversized create body", rec.Code)
	}
	if spy.called {
		t.Error("oversized create body must not reach the dispatcher")
	}
}

// A create body exactly at the cap must still be inspected normally (the +1
// read is only to detect overflow, not to reject at the boundary).
func TestSessionModelGuard_BodyAtCapIsInspected(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"haiku"})

	spy := &nextSpy{}
	guard := newSessionModelGuard(store, spy)
	rec := httptest.NewRecorder()

	prefix := `{"projectId":"` + proj.ID + `","model":"opus","pad":"`
	suffix := `"}`
	pad := strings.Repeat("A", maxSessionBodyBytes-len(prefix)-len(suffix))
	body := prefix + pad + suffix
	if len(body) != maxSessionBodyBytes {
		t.Fatalf("test setup: body is %d bytes, want exactly %d", len(body), maxSessionBodyBytes)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	guard(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (disallowed model in a body at the cap)", rec.Code)
	}
	if spy.called {
		t.Error("disallowed model must not reach the dispatcher")
	}
}

func TestSessionModelGuard_IgnoresNonPost(t *testing.T) {
	store := newProjectsTestStore(t)
	spy := &nextSpy{}
	guard := newSessionModelGuard(store, spy)
	rec := httptest.NewRecorder()
	guard(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))

	if !spy.called {
		t.Error("GET /api/sessions must be forwarded to the dispatcher")
	}
}

// Built by hand rather than through the create path, so a refusal proves the
// guard itself acted — not that validation happened to run upstream.
func remoteProject(t *testing.T, store config.SettingsStore) config.Project {
	t.Helper()
	var out config.Project
	if err := store.With(func(s *config.Settings) {
		out = config.Project{
			ID:            "remote-session-proj",
			Name:          "remote",
			Kind:          config.ProjectKindRemote,
			AllowedMcpIDs: []string{},
			Token:         config.NewSecret("tok-remote-session"),
			TokenHash:     config.HashToken("tok-remote-session"),
		}
		s.Projects = append(s.Projects, out)
	}); err != nil {
		t.Fatalf("seed remote project: %v", err)
	}
	return out
}

// A remote project is a grant to another machine, not a place a session runs.
//
// This cannot be left to the model allowlist: project.ValidateShape requires a
// remote project's AllowedModels to be EMPTY, and modelAllowedForProject reads
// an empty allowlist as "unrestricted". So without an explicit refusal the most
// restrictive configuration produces the most permissive outcome — every model
// allowed, on a project that should host no session at all.
func TestSessionModelGuard_RefusesRemoteProject(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := remoteProject(t, store)

	spy := &nextSpy{}
	guard := newSessionModelGuard(store, spy)
	rec := httptest.NewRecorder()
	guard(rec, postSessions(`{"projectId":"`+proj.ID+`","model":"opus"}`))

	if spy.called {
		t.Fatal("a session on a remote project reached the dispatcher")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestModelAllowedForProject_WouldPermitRemoteProject(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := remoteProject(t, store)

	if !modelAllowedForProject(store, proj.ID, "opus") {
		t.Fatal("expected the model allowlist to permit a remote project's empty allowlist; " +
			"if this now fails, refuseRemoteSession may be redundant and this pair should be revisited")
	}
}

func TestSessionModelGuard_LocalProjectStillCreatesSessions(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"opus"})

	spy := &nextSpy{}
	guard := newSessionModelGuard(store, spy)
	rec := httptest.NewRecorder()
	guard(rec, postSessions(`{"projectId":"`+proj.ID+`","model":"opus"}`))

	if !spy.called {
		t.Fatalf("local project session was blocked, status = %d", rec.Code)
	}
}

// --- PUT /api/sessions/{id}/model ---
//
// relayLLM's PUT /api/sessions/{id}/model (SetPiModel) is the other route
// that can change a live session's model. relay keeps no session table, so
// the guard resolves projectId by calling GET /api/sessions through next —
// sessionLookupStub plays that role here, standing in for the real
// dispatcher's proxy to relayLLM.

// sessionLookupStub answers the guard's internal GET /api/sessions lookup
// and records whatever else is forwarded to it (the PUT itself, once the
// guard is satisfied), so a test can assert both the lookup shape and
// whether the original request ever reached "relayLLM".
type sessionLookupStub struct {
	sessions   []map[string]string
	lookupCode int // 0 defaults to 200

	forwardedCalled bool
	forwardedMethod string
	forwardedPath   string
	forwardedBody   string
}

func (s *sessionLookupStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/api/sessions" {
		code := s.lookupCode
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
		if code == http.StatusOK {
			_ = json.NewEncoder(w).Encode(s.sessions)
		}
		return
	}
	s.forwardedCalled = true
	s.forwardedMethod = r.Method
	s.forwardedPath = r.URL.Path
	b, _ := io.ReadAll(r.Body)
	s.forwardedBody = string(b)
	w.WriteHeader(http.StatusOK)
}

func putSessionModel(id, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/api/sessions/"+id+"/model", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestSessionModelGuard_PUT_AllowedModelForwards(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"pi/anthropic/claude-sonnet-4"})
	next := &sessionLookupStub{sessions: []map[string]string{{"id": "sess1", "projectId": proj.ID}}}

	guard := newSessionModelGuard(store, next)
	rec := httptest.NewRecorder()
	guard(rec, putSessionModel("sess1", `{"provider":"anthropic","modelId":"claude-sonnet-4"}`))

	if !next.forwardedCalled {
		t.Fatalf("allowed model update was not forwarded, status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if next.forwardedMethod != http.MethodPut || next.forwardedPath != "/api/sessions/sess1/model" {
		t.Errorf("forwarded request = %s %s, want PUT /api/sessions/sess1/model", next.forwardedMethod, next.forwardedPath)
	}
}

func TestSessionModelGuard_PUT_DisallowedModelRefused(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"pi/anthropic/claude-haiku"})
	next := &sessionLookupStub{sessions: []map[string]string{{"id": "sess1", "projectId": proj.ID}}}

	guard := newSessionModelGuard(store, next)
	rec := httptest.NewRecorder()
	guard(rec, putSessionModel("sess1", `{"provider":"anthropic","modelId":"claude-opus"}`))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if next.forwardedCalled {
		t.Error("disallowed model update must not reach relayLLM")
	}
}

func TestSessionModelGuard_PUT_UnrestrictedProjectPasses(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := createTestProject(t, store, "Wild", t.TempDir(), []string{"fsmcp"}) // allowed_models defaults to ["*"]
	next := &sessionLookupStub{sessions: []map[string]string{{"id": "sess1", "projectId": proj.ID}}}

	guard := newSessionModelGuard(store, next)
	rec := httptest.NewRecorder()
	guard(rec, putSessionModel("sess1", `{"provider":"anthropic","modelId":"claude-opus"}`))

	if !next.forwardedCalled {
		t.Fatalf("unrestricted project's model update was blocked, status = %d", rec.Code)
	}
}

func TestSessionModelGuard_PUT_LookupFailureRefused(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"pi/anthropic/claude-haiku"})
	_ = proj
	next := &sessionLookupStub{lookupCode: http.StatusInternalServerError}

	guard := newSessionModelGuard(store, next)
	rec := httptest.NewRecorder()
	guard(rec, putSessionModel("sess1", `{"provider":"anthropic","modelId":"claude-opus"}`))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when the session lookup fails", rec.Code)
	}
	if next.forwardedCalled {
		t.Error("a model update must not be forwarded when the session lookup fails")
	}
}

func TestSessionModelGuard_PUT_SessionNotFoundRefused(t *testing.T) {
	store := newProjectsTestStore(t)
	next := &sessionLookupStub{sessions: []map[string]string{{"id": "some-other-session", "projectId": "whatever"}}}

	guard := newSessionModelGuard(store, next)
	rec := httptest.NewRecorder()
	guard(rec, putSessionModel("sess1", `{"provider":"anthropic","modelId":"claude-opus"}`))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 when the session isn't in the lookup response", rec.Code)
	}
	if next.forwardedCalled {
		t.Error("a model update for an unknown session must not be forwarded")
	}
}

func TestSessionModelGuard_PUT_IncompleteBodyForwardsUnchecked(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"pi/anthropic/claude-haiku"})
	next := &sessionLookupStub{sessions: []map[string]string{{"id": "sess1", "projectId": proj.ID}}}

	guard := newSessionModelGuard(store, next)
	rec := httptest.NewRecorder()
	// Missing modelId: SetModel itself refuses this shape, so the guard has
	// no candidate model to check and forwards it for relayLLM to reject.
	guard(rec, putSessionModel("sess1", `{"provider":"anthropic"}`))

	if !next.forwardedCalled {
		t.Errorf("incomplete model-update body should be forwarded so relayLLM produces the error, status = %d", rec.Code)
	}
}

func TestSessionModelGuard_PUT_TrailingSlash(t *testing.T) {
	store := newProjectsTestStore(t)
	proj := restrictedProject(t, store, []string{"pi/anthropic/claude-haiku"})
	next := &sessionLookupStub{sessions: []map[string]string{{"id": "sess1", "projectId": proj.ID}}}

	guard := newSessionModelGuard(store, next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/sessions/sess1/model/", strings.NewReader(`{"provider":"anthropic","modelId":"claude-opus"}`))
	req.Header.Set("Content-Type", "application/json")
	guard(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 on trailing-slash model-update path", rec.Code)
	}
	if next.forwardedCalled {
		t.Error("disallowed model update on the trailing-slash path must not reach relayLLM")
	}
}

func TestIsSessionModelUpdatePath(t *testing.T) {
	cases := []struct {
		path   string
		wantID string
		wantOK bool
	}{
		{"/api/sessions/abc123/model", "abc123", true},
		{"/api/sessions/abc123/model/", "abc123", true},
		{"/api/sessions/abc123/models", "", false},
		{"/api/sessions//model", "", false},
		{"/api/sessions/abc123/message", "", false},
		{"/api/sessions/abc/def/model", "", false},
		{"/api/sessions", "", false},
		{"/api/sessions/", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			id, ok := isSessionModelUpdatePath(tc.path)
			if id != tc.wantID || ok != tc.wantOK {
				t.Errorf("isSessionModelUpdatePath(%q) = (%q, %v), want (%q, %v)", tc.path, id, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}
