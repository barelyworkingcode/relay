package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
)

func newTestManager(t *testing.T) *terminal.Manager {
	t.Helper()
	return terminal.NewManager(terminal.Config{ShimBinary: buildRelaySessionsBin(t), LogDir: t.TempDir()})
}

func TestHandleListTerminals(t *testing.T) {
	mgr := newTestManager(t)
	sess, err := mgr.Create(terminal.CreateSpec{
		SessionID: "33333333-3333-3333-3333-333333333333",
		Name:      "list-me",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	req := httptest.NewRequest(http.MethodGet, "/api/terminals", nil)
	w := httptest.NewRecorder()
	HandleListTerminals(mgr, w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Terminals []terminal.Summary `json:"terminals"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, s := range body.Terminals {
		if s.ID == sess.ID {
			found = true
			if s.Name != "list-me" {
				t.Errorf("name = %q, want list-me", s.Name)
			}
		}
	}
	if !found {
		t.Fatalf("session %s missing from list: %+v", sess.ID, body.Terminals)
	}
}

func TestHandleListTerminals_WrongMethod(t *testing.T) {
	mgr := newTestManager(t)
	req := httptest.NewRequest(http.MethodPost, "/api/terminals", nil)
	w := httptest.NewRecorder()
	HandleListTerminals(mgr, w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestHandleDeleteTerminal(t *testing.T) {
	mgr := newTestManager(t)
	sess, err := mgr.Create(terminal.CreateSpec{
		SessionID: "44444444-4444-4444-4444-444444444444",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/terminals/"+sess.ID, nil)
	w := httptest.NewRecorder()
	HandleDeleteTerminal(mgr, sess.ID, w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if _, ok := mgr.Get(sess.ID); ok {
		t.Fatal("session still present after delete")
	}
}

func TestHandleDeleteTerminal_NotFound(t *testing.T) {
	mgr := newTestManager(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/terminals/nope", nil)
	w := httptest.NewRecorder()
	HandleDeleteTerminal(mgr, "nope", w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleTerminalLog(t *testing.T) {
	mgr := newTestManager(t)
	sess, err := mgr.Create(terminal.CreateSpec{
		SessionID: "55555555-5555-5555-5555-555555555555",
		Directory: t.TempDir(),
		Argv:      []string{"/bin/sh", "-c", "echo log-line-content; exit 0"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { mgr.Close(sess.ID) })

	// Poll: readLoop's log flush races the process exit, same as
	// internal/sessions/terminal's own TestManager_LocalPTY_ExitAndLog.
	deadline := time.Now().Add(2 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/api/terminals/"+sess.ID+"/log", nil)
		w := httptest.NewRecorder()
		HandleTerminalLog(mgr, sess.ID, w, req)
		if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "log-line-content") {
			body = w.Body.String()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, "log-line-content") {
		t.Fatalf("log body = %q, want to contain log-line-content", body)
	}
}

func TestHandleTerminalLog_NotFound(t *testing.T) {
	mgr := newTestManager(t)
	req := httptest.NewRequest(http.MethodGet, "/api/terminals/66666666-6666-6666-6666-666666666666/log", nil)
	w := httptest.NewRecorder()
	HandleTerminalLog(mgr, "66666666-6666-6666-6666-666666666666", w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestHandleTerminalLog_InvalidID(t *testing.T) {
	mgr := newTestManager(t)
	req := httptest.NewRequest(http.MethodGet, "/api/terminals/../../etc/passwd/log", nil)
	w := httptest.NewRecorder()
	HandleTerminalLog(mgr, "../../etc/passwd", w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
