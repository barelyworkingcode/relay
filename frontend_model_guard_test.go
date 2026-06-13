package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// restrictedProject creates a project whose allowed_models is an explicit,
// non-wildcard list.
func restrictedProject(t *testing.T, store SettingsStore, models []string) Project {
	t.Helper()
	proj := createTestProject(t, store, "Restricted", t.TempDir(), []string{"fsmcp"})
	if err := store.With(func(s *Settings) {
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

// nextSpy records whether the downstream handler ran and what body it saw —
// the guard must forward the original payload untouched on allow.
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

// CR-7: the allowlist must hold on the trailing-slash variant of the create
// path. Go's ServeMux routes "POST /api/sessions/" to the catch-all, so a guard
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

// Sub-resource POSTs (e.g. sending a message to an existing session) are NOT
// gated: they never name a model and may carry bodies larger than the guard's
// buffer, which must not be read/truncated. They must pass straight through.
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

// Non-POST requests to the sessions path (e.g. listing) must pass through
// without the guard buffering the body.
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
