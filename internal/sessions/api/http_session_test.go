package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const httpTestSessionID = "11111111-1111-1111-1111-111111111111"

func newHTTPTestManager(t *testing.T) *session.Manager {
	t.Helper()
	store := session.NewStore(t.TempDir())
	return session.NewManager(session.Config{}, store, nil)
}

func TestHandleListSessions(t *testing.T) {
	mgr := newHTTPTestManager(t)
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return &wsFakeProvider{}, nil
	})
	if _, err := mgr.Create(session.CreateSpec{SessionID: httpTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude, Name: "one"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	w := httptest.NewRecorder()
	HandleListSessions(mgr, w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Sessions []session.Summary `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 1 || body.Sessions[0].ID != httpTestSessionID {
		t.Fatalf("sessions = %+v", body.Sessions)
	}
	if !body.Sessions[0].Live {
		t.Fatal("live session should list as live")
	}
}

func TestHandleListSessions_WrongMethod(t *testing.T) {
	mgr := newHTTPTestManager(t)
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", nil)
	w := httptest.NewRecorder()
	HandleListSessions(mgr, w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestHandleDeleteSession(t *testing.T) {
	mgr := newHTTPTestManager(t)
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return &wsFakeProvider{}, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: httpTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+httpTestSessionID, nil)
	w := httptest.NewRecorder()
	HandleDeleteSession(mgr, sess.ID, w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if _, ok := mgr.Get(sess.ID); ok {
		t.Fatal("session should be gone after delete")
	}
}

func TestHandleSessionMessageSync_ResumeRequired(t *testing.T) {
	mgr := newHTTPTestManager(t)
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return &wsFakeProvider{}, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: httpTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess.Provider().(*wsFakeProvider).Kill()

	body, _ := json.Marshal(map[string]any{"text": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+httpTestSessionID+"/message", bytes.NewReader(body))
	w := httptest.NewRecorder()
	HandleSessionMessageSync(mgr, sess.ID, w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	var errBody map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody["error"] != "resume_required" {
		t.Fatalf("error body = %+v, want resume_required", errBody)
	}
}

// newLazyAdHocManager returns a manager whose only session is a project-less
// one on disk that this manager never launched.
func newLazyAdHocManager(t *testing.T) *session.Manager {
	t.Helper()
	store := session.NewStore(t.TempDir())
	if err := store.Save(&sessionstypes.Session{ID: httpTestSessionID, ProviderType: session.KindClaude}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	mgr := session.NewManager(session.Config{}, store, nil)
	mgr.SetProviderFactory(func(*sessionstypes.Session, session.CreateSpec, sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		return &wsFakeProvider{}, nil
	})
	return mgr
}

func assertResumeGuidance(t *testing.T, message any) {
	t.Helper()
	text, _ := message.(string)
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "new session") || !strings.Contains(lower, "project") {
		t.Errorf("message = %v, want guidance to start a new session in a project", message)
	}
}

func TestHandleSessionMessageSync_LazyLoadedAdHoc_ResumeRequiredWithGuidance(t *testing.T) {
	mgr := newLazyAdHocManager(t)

	body, _ := json.Marshal(map[string]any{"text": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+httpTestSessionID+"/message", bytes.NewReader(body))
	w := httptest.NewRecorder()
	HandleSessionMessageSync(mgr, httpTestSessionID, w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	var errBody map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody["error"] != "resume_required" {
		t.Errorf("error = %v, want resume_required", errBody["error"])
	}
	assertResumeGuidance(t, errBody["message"])
}

func TestHandleSessionMessageSync_NotFound(t *testing.T) {
	mgr := newHTTPTestManager(t)
	body, _ := json.Marshal(map[string]any{"text": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/ghost/message", bytes.NewReader(body))
	w := httptest.NewRecorder()
	HandleSessionMessageSync(mgr, "ghost", w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestHandleSessionMessageSync_HappyPath uses testutil.FakeProvider (not
// this package's own wsFakeProvider) because ResponseCollector.Wait needs a
// real HandlerLLMEvent/HandlerStatsUpdate/HandlerMessageComplete sequence
// to unblock, and FakeProvider's Script* helpers are exactly relayLLM's own
// wire shape for that.
func TestHandleSessionMessageSync_HappyPath(t *testing.T) {
	mgr := newHTTPTestManager(t)
	var fp *testutil.FakeProvider
	mgr.SetProviderFactory(func(_ *sessionstypes.Session, _ session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		fp = testutil.NewFakeProvider(handler)
		fp.ScriptText("hello back")
		fp.ScriptResult("end_turn", sessionstypes.SessionStats{OutputTokens: 3})
		return fp, nil
	})
	sess, err := mgr.Create(session.CreateSpec{SessionID: httpTestSessionID, ProjectID: "proj-1", Kind: session.KindClaude})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"text": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+httpTestSessionID+"/message", bytes.NewReader(body))
	w := httptest.NewRecorder()
	HandleSessionMessageSync(mgr, sess.ID, w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp sendMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Text != "hello back" {
		t.Fatalf("text = %q, want %q", resp.Text, "hello back")
	}
	if resp.Stats.OutputTokens != 3 {
		t.Fatalf("stats.outputTokens = %d, want 3", resp.Stats.OutputTokens)
	}
}
