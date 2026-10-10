package contract

import (
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
