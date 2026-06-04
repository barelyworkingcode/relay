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
