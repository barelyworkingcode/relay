package contract

import (
	"encoding/json"
	"testing"

	"relaye2e/harness"
)

// credsAllClasses is one bearer holding every class a session or terminal
// scenario needs. Its name is the credential the Body passes to HTTP and WS.
var credsAllClasses = []harness.CredentialSpec{
	{Name: "ops", Classes: []string{"read", "configure", "execute", "proxy"}},
}

// sessionsWorld is a console project that allows every model and template.
func sessionsWorld() Spec {
	return Spec{
		Credentials: credsAllClasses,
		Projects: []Project{{
			ID: "p_acme", Name: "Acme", Mode: "work",
			Files: map[string]File{"README.md": {Text: "# Acme\n"}},
		}},
	}
}

// createSession posts a session on a path the transcript does not record and
// returns its id. TestSessionsLifecycle records the create response, so the
// scenarios that only need a session do not repeat its body.
func createSession(r *Run, name string) string {
	r.T.Helper()
	i := r.Target.I
	resp := i.SocketHTTP(i.Credential("ops")).Do("POST", "/api/sessions", map[string]any{
		"projectId": "p_acme", "name": name, "model": "haiku",
	})
	var out struct {
		SessionID string `json:"sessionId"`
	}
	resp.JSON(r.T, &out)
	if resp.Status != 201 || out.SessionID == "" {
		r.T.Fatalf("create session answered %d with sessionId %q", resp.Status, out.SessionID)
	}
	r.Learn(out.SessionID)
	return out.SessionID
}

func TestSessionsModels(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Sessions,
		Spec:    sessionsWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/models", nil)
		},
	})
}

func TestSessionsLifecycle(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Sessions,
		Spec:    sessionsWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/sessions", nil)
			created := r.HTTP("ops", "POST", "/api/sessions", map[string]any{
				"projectId": "p_acme", "name": "Fix the build", "model": "haiku",
			})
			var out struct {
				SessionID string `json:"sessionId"`
			}
			created.JSON(r.T, &out)
			id := out.SessionID
			r.HTTP("ops", "GET", "/api/sessions", nil)
			r.HTTP("ops", "POST", "/api/sessions", map[string]any{"projectId": "p_missing", "model": "haiku"})
			r.HTTP("ops", "DELETE", "/api/sessions/"+id, nil)
			r.HTTP("ops", "GET", "/api/sessions", nil)
		},
	})
}

// wsMessage is a decoded frame for the scenarios that match on one field.
type wsMessage = map[string]any

func frameField(f wsMessage, key string) string {
	s, _ := f[key].(string)
	return s
}

// claudeCodeTemplate is the console template a drop-in launches. relay's
// harness writes one of its own; supplying it keeps fakerelay's world the same.
var claudeCodeTemplate = json.RawMessage(`{"id":"claude-code","name":"Claude Code","command":"claude"}`)

func dropInWorld() Spec {
	s := sessionsWorld()
	s.ConsoleTemplates = []json.RawMessage{claudeCodeTemplate}
	return s
}

// createHeadlessAgent posts a tracked headless session, unrecorded.
func createHeadlessAgent(r *Run, name string) string {
	r.T.Helper()
	i := r.Target.I
	resp := i.SocketHTTP(i.Credential("ops")).Do("POST", "/api/sessions", map[string]any{
		"projectId": "p_acme", "name": name, "model": "haiku",
		"settings": map[string]any{"headless": true, "agent": true},
	})
	var out struct {
		SessionID string `json:"sessionId"`
	}
	resp.JSON(r.T, &out)
	if resp.Status != 201 || out.SessionID == "" {
		r.T.Fatalf("create headless session answered %d with sessionId %q", resp.Status, out.SessionID)
	}
	r.Learn(out.SessionID)
	return out.SessionID
}

func TestSessionsHostPaths(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Sessions,
		Spec:    sessionsWorld(),
		Body: func(r *Run) {
			id := createSession(r, "Fix the build")
			r.HTTP("ops", "POST", "/api/sessions/"+id+"/stop", nil)
			r.HTTP("ops", "POST", "/api/sessions/"+id+"/delete", nil)
			r.HTTP("ops", "GET", "/api/sessions", nil)
			r.HTTP("ops", "DELETE", "/api/sessions/"+id, nil)
			r.HTTP("ops", "GET", "/api/sessions", nil)
		},
	})
}

func TestSessionsDropIn(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Sessions,
		Spec:    dropInWorld(),
		Body: func(r *Run) {
			id := createHeadlessAgent(r, "Fix the build")
			i := r.Target.I
			raw := i.WebSocket("/ws", i.Credential("ops"))
			defer raw.Close()
			raw.Send(wsMessage{"type": "join_session", "sessionId": id})
			nextTerminalFrame(r, raw, "session_joined")
			raw.Send(wsMessage{"type": "send_message", "sessionId": id, "text": "hello acme"})
			nextTerminalFrame(r, raw, "message_complete")

			ws := r.WS("/ws", "ops")

			tr := harness.NewTrace(r.T)
			resp := r.HTTP("ops", "POST", "/api/sessions/"+id+"/drop-in", map[string]any{}, harness.ReqOpts{Trace: tr})
			var out struct {
				Terminal struct {
					TerminalID string `json:"terminalId"`
				} `json:"terminal"`
			}
			resp.JSON(r.T, &out)
			tid := out.Terminal.TerminalID
			if resp.Status != 201 || tid == "" {
				r.T.Fatalf("drop-in answered %d with terminal %q", resp.Status, tid)
			}
			r.Learn(tid)
			r.Event(harness.EventQuery{Key: "session.drop_in", Trace: tr})
			r.HTTP("ops", "GET", "/api/sessions", nil)
			r.HTTP("ops", "GET", "/api/terminals", nil)

			ws.Send(wsMessage{"type": "send_message", "sessionId": id, "text": "while held"})
			ws.Expect("error", func(f wsMessage) bool { return frameField(f, "code") == "dropped_in" })
			r.HTTP("ops", "POST", "/api/sessions/"+id+"/drop-in", map[string]any{})
			r.HTTP("ops", "DELETE", "/api/terminals/"+tid, nil)
			ws.Expect("session_state", func(f wsMessage) bool { return frameField(f, "state") == "idle" })
			r.HTTP("ops", "GET", "/api/sessions", nil)
		},
	})
}

func TestSessionsDropInRefusals(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Sessions,
		Spec:    dropInWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "POST", "/api/sessions/"+absentTerminalID+"/drop-in", map[string]any{})
			plain := createSession(r, "Plain")
			r.HTTP("ops", "POST", "/api/sessions/"+plain+"/drop-in", map[string]any{})
			fresh := createHeadlessAgent(r, "No turn")
			r.HTTP("ops", "POST", "/api/sessions/"+fresh+"/drop-in", map[string]any{})
		},
	})
}
