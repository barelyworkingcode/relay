package hostapi_test

import (
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const blankModelSessionID = "77777777-7777-7777-7777-777777777777"

func startChatHost(t *testing.T, sessions *session.Manager) (client *http.Client, bearer string, providers *atomic.Int32) {
	t.Helper()
	terminals, _ := buildManagers(t)
	providers = &atomic.Int32{}
	sessions.SetProviderFactory(func(_ *sessionstypes.Session, _ session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		providers.Add(1)
		return testutil.NewFakeProvider(handler), nil
	})
	_, internalSock, _, bearer := startServerWithManagers(t, os.Getpid(), terminals, sessions)
	return unixClient(internalSock), bearer, providers
}

func newChatSessions(t *testing.T) (*session.Manager, *session.Store) {
	t.Helper()
	store := session.NewStore(mkShortTempDir(t, "hostapi-chat-"))
	return session.NewManager(session.Config{}, store, nil), store
}

func chatLaunchBody(resume bool, sessionRequest map[string]any) map[string]any {
	body := map[string]any{"v": 1, "session_id": blankModelSessionID, "kind": "chat", "resume": resume}
	if sessionRequest != nil {
		body["session_request"] = sessionRequest
	}
	return body
}

func TestLaunch_ChatBlankModel_RefusedInvalidSpec(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sessionRequest map[string]any
	}{
		{"empty", map[string]any{"projectId": "proj-1", "directory": "/tmp/proj", "model": ""}},
		{"whitespace", map[string]any{"projectId": "proj-1", "directory": "/tmp/proj", "model": "   "}},
		{"no session_request", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, _ := newChatSessions(t)
			client, bearer, providers := startChatHost(t, sessions)

			resp := postJSON(t, client, "http://h/launch", bearer, chatLaunchBody(false, tc.sessionRequest))
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (providers started: %d)", resp.StatusCode, providers.Load())
			}
			var errBody hostapi.ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			want := "hostapi: chat session " + blankModelSessionID + " has no model"
			if errBody.Error != hostapi.ErrInvalidSpec || errBody.Message != want {
				t.Fatalf("error body = %+v, want {%q %q}", errBody, hostapi.ErrInvalidSpec, want)
			}
			if n := providers.Load(); n != 0 {
				t.Fatalf("provider factory called %d time(s) for a refused launch", n)
			}
			if _, ok := sessions.Get(blankModelSessionID); ok {
				t.Fatal("session manager holds a session for a refused launch")
			}
		})
	}
}

func TestLaunch_ChatNamedModel_Launches(t *testing.T) {
	sessions, _ := newChatSessions(t)
	client, bearer, _ := startChatHost(t, sessions)

	resp := postJSON(t, client, "http://h/launch", bearer, chatLaunchBody(false,
		map[string]any{"projectId": "proj-1", "directory": "/tmp/proj", "model": "gpt-5"}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	sess, ok := sessions.Get(blankModelSessionID)
	if !ok || sess.Model != "gpt-5" {
		t.Fatalf("session = %+v, ok = %v, want model gpt-5", sess, ok)
	}
}

func TestLaunch_ChatResume_BlankModel_NotRefused(t *testing.T) {
	sessions, store := newChatSessions(t)
	if err := store.Save(&sessionstypes.Session{
		ID: blankModelSessionID, ProjectID: "proj-1", Directory: "/tmp/proj",
		Model: "gpt-5", ProviderType: session.KindChat, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed saved session: %v", err)
	}
	client, bearer, _ := startChatHost(t, sessions)

	resp := postJSON(t, client, "http://h/launch", bearer, chatLaunchBody(true,
		map[string]any{"projectId": "proj-1", "directory": "/tmp/proj", "model": ""}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var errBody hostapi.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		t.Fatalf("status = %d (%+v), want 201", resp.StatusCode, errBody)
	}
	sess, ok := sessions.Get(blankModelSessionID)
	if !ok || sess.Model != "gpt-5" {
		t.Fatalf("session = %+v, ok = %v, want the saved model gpt-5", sess, ok)
	}
}
