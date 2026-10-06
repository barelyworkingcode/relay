package hostapi_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// startSendHost brings up a real host over a real Unix socket whose session
// manager hands out fake providers, and returns the providers by session id.
func startSendHost(t *testing.T, relayPID int) (sessions *session.Manager, client *http.Client, bearer string, providers map[string]*testutil.FakeProvider) {
	t.Helper()
	terminals, sessions := buildManagers(t)
	providers = map[string]*testutil.FakeProvider{}
	sessions.SetProviderFactory(func(s *sessionstypes.Session, _ session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		p := testutil.NewFakeProvider(handler)
		providers[s.ID] = p
		return p, nil
	})
	_, sock, _, bearer := startServerWithManagers(t, relayPID, terminals, sessions)
	return sessions, unixClient(sock), bearer, providers
}

func createSendSession(t *testing.T, sessions *session.Manager, id, project, settings string) {
	t.Helper()
	spec := session.CreateSpec{SessionID: id, ProjectID: project, Kind: session.KindClaude, Directory: "/tmp/p1"}
	if settings != "" {
		spec.Settings = json.RawMessage(settings)
	}
	if _, err := sessions.Create(spec); err != nil {
		t.Fatalf("Create %s: %v", id, err)
	}
}

func sendBody(id, text, origin string) map[string]any {
	return map[string]any{"session_id": id, "text": text, "origin": origin}
}

func decodeErr(t *testing.T, resp *http.Response) hostapi.ErrorResponse {
	t.Helper()
	var e hostapi.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return e
}

func TestSend_PeerCheckRunsFirst(t *testing.T) {
	sessions, client, bearer, providers := startSendHost(t, os.Getpid()+999999)
	createSendSession(t, sessions, "s-listed", "p1", "")

	// The body is invalid on purpose: a wrong peer must be refused before
	// anything about the request is judged or revealed.
	for _, body := range []map[string]any{
		sendBody("s-listed", "hello", sessionstypes.OriginChiefOfStaff),
		sendBody("s-listed", "hello", "person"),
	} {
		resp := postJSON(t, client, "http://h/send", bearer, body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 for a wrong peer pid", resp.StatusCode)
		}
	}
	if got := providers["s-listed"].Sent(); len(got) != 0 {
		t.Fatalf("provider received %d message(s) from a refused peer", len(got))
	}
	if sess, _ := sessions.Get("s-listed"); len(sess.Messages) != 0 {
		t.Fatalf("a refused peer left %d message(s) in the transcript", len(sess.Messages))
	}
}

func TestSend_ListedSession_AcceptedAndMarked(t *testing.T) {
	sessions, client, bearer, providers := startSendHost(t, os.Getpid())
	createSendSession(t, sessions, "s-listed", "p1", "")

	resp := postJSON(t, client, "http://h/send", bearer, sendBody("s-listed", "check the build", sessionstypes.OriginChiefOfStaff))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var out hostapi.SendResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SessionID != "s-listed" || out.Origin != sessionstypes.OriginChiefOfStaff || out.At == "" {
		t.Fatalf("response = %+v, want session s-listed, origin chief-of-staff and a time", out)
	}
	if got := providers["s-listed"].Sent(); len(got) != 1 || got[0].Text != "check the build" {
		t.Fatalf("provider received %+v, want exactly the sent text", got)
	}
	sess, _ := sessions.Get("s-listed")
	if len(sess.Messages) != 1 || sess.Messages[0].Role != "user" || sess.Messages[0].Origin != sessionstypes.OriginChiefOfStaff {
		t.Fatalf("transcript = %+v, want one user message marked chief-of-staff", sess.Messages)
	}
}

func TestSend_AgentHeadlessSession_IsListed(t *testing.T) {
	sessions, client, bearer, _ := startSendHost(t, os.Getpid())
	createSendSession(t, sessions, "s-agent", "p1", `{"headless":true,"agent":true}`)

	resp := postJSON(t, client, "http://h/send", bearer, sendBody("s-agent", "status?", sessionstypes.OriginChiefOfStaff))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 for a headless session marked as an agent", resp.StatusCode)
	}
}

// An unlisted session must answer exactly like a missing one, so a caller
// cannot probe for hidden routine runs.
func TestSend_UnlistedAndUnknownSessions_AreIndistinguishable(t *testing.T) {
	sessions, client, bearer, providers := startSendHost(t, os.Getpid())
	createSendSession(t, sessions, "s-routine", "p1", `{"headless":true}`)

	var bodies []hostapi.ErrorResponse
	for _, id := range []string{"s-routine", "s-missing"} {
		resp := postJSON(t, client, "http://h/send", bearer, sendBody(id, "hello", sessionstypes.OriginChiefOfStaff))
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", id, resp.StatusCode)
		}
		e := decodeErr(t, resp)
		_ = resp.Body.Close()
		if e.Error != hostapi.ErrSessionNotFound {
			t.Fatalf("%s: error = %q, want %q", id, e.Error, hostapi.ErrSessionNotFound)
		}
		bodies = append(bodies, e)
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("unlisted answer %+v differs from unknown answer %+v", bodies[0], bodies[1])
	}
	if got := providers["s-routine"].Sent(); len(got) != 0 {
		t.Fatalf("a hidden session received %d message(s)", len(got))
	}
}

func TestSend_InvalidSpec(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"person origin", sendBody("s-listed", "hello", "person")},
		{"empty origin", sendBody("s-listed", "hello", "")},
		{"invented origin", sendBody("s-listed", "hello", "ops-bot")},
		{"blank text", sendBody("s-listed", "   \n\t", sessionstypes.OriginChiefOfStaff)},
		{"empty text", sendBody("s-listed", "", sessionstypes.OriginChiefOfStaff)},
		{"no session id", sendBody("", "hello", sessionstypes.OriginChiefOfStaff)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions, client, bearer, providers := startSendHost(t, os.Getpid())
			createSendSession(t, sessions, "s-listed", "p1", "")

			resp := postJSON(t, client, "http://h/send", bearer, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if e := decodeErr(t, resp); e.Error != hostapi.ErrInvalidSpec {
				t.Fatalf("error = %q, want %q", e.Error, hostapi.ErrInvalidSpec)
			}
			if got := providers["s-listed"].Sent(); len(got) != 0 {
				t.Fatalf("a refused send reached the provider: %+v", got)
			}
		})
	}
}

func TestSend_MalformedBody_InvalidSpec(t *testing.T) {
	_, client, bearer, _ := startSendHost(t, os.Getpid())
	req, _ := http.NewRequest(http.MethodPost, "http://h/send", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSend_Conflicts(t *testing.T) {
	t.Run("already processing", func(t *testing.T) {
		sessions, client, bearer, providers := startSendHost(t, os.Getpid())
		createSendSession(t, sessions, "s-listed", "p1", "")
		// The fake provider emits no turn_done, so the first send leaves the
		// session mid-turn.
		first := postJSON(t, client, "http://h/send", bearer, sendBody("s-listed", "one", sessionstypes.OriginChiefOfStaff))
		_ = first.Body.Close()
		if first.StatusCode != http.StatusAccepted {
			t.Fatalf("first send status = %d, want 202", first.StatusCode)
		}

		resp := postJSON(t, client, "http://h/send", bearer, sendBody("s-listed", "two", sessionstypes.OriginChiefOfStaff))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
		if e := decodeErr(t, resp); e.Error != hostapi.ErrAlreadyProcessing {
			t.Fatalf("error = %q, want %q", e.Error, hostapi.ErrAlreadyProcessing)
		}
		if got := providers["s-listed"].Sent(); len(got) != 1 {
			t.Fatalf("provider received %d messages, want only the first", len(got))
		}
	})
	t.Run("resume required", func(t *testing.T) {
		sessions, client, bearer, providers := startSendHost(t, os.Getpid())
		createSendSession(t, sessions, "s-listed", "p1", "")
		providers["s-listed"].Kill()

		resp := postJSON(t, client, "http://h/send", bearer, sendBody("s-listed", "hello", sessionstypes.OriginChiefOfStaff))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
		if e := decodeErr(t, resp); e.Error != hostapi.ErrResumeRequired {
			t.Fatalf("error = %q, want %q", e.Error, hostapi.ErrResumeRequired)
		}
	})
	t.Run("dropped in", func(t *testing.T) {
		f := newDropInFixture(t, map[string]any{"headless": true, "agent": true})
		if code, out := f.post(t, "/handoff", map[string]any{"session_id": agentID}); code != http.StatusOK {
			t.Fatalf("handoff = %d %v, want 200", code, out)
		}

		code, out := f.post(t, "/send", sendBody(agentID, "hello", sessionstypes.OriginChiefOfStaff))
		if code != http.StatusConflict || out["error"] != hostapi.ErrDroppedIn || out["message"] == "" {
			t.Fatalf("send while held = %d %v, want 409 %s with a message", code, out, hostapi.ErrDroppedIn)
		}
		if got := f.provider.Sent(); len(got) != 0 {
			t.Fatalf("provider received %d messages while a terminal holds the session, want 0", len(got))
		}
	})
}
