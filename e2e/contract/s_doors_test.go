package contract

import (
	"net/http"
	"testing"

	"relaye2e/harness"
)

// acmeOpsCreds is one credential holding every class a bearer can hold on the
// frontend socket, plus a read-only one and one that holds nothing.
func acmeOpsCreds() []harness.CredentialSpec {
	return []harness.CredentialSpec{
		{Name: "ops", Classes: []string{"read", "configure", "execute", "proxy"}},
		{Name: "reader", Classes: []string{"read"}},
		{Name: "nobody", Classes: []string{}},
	}
}

func cosScope() harness.ReqOpts {
	return harness.ReqOpts{Header: http.Header{"X-Relay-Scope": {"chief-of-staff"}}}
}

func TestDoorsReadyFile(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Doors,
		Spec:    Spec{Credentials: acmeOpsCreds()},
		Body: func(r *Run) {
			rd := r.Target.I.Ready
			r.Note("ready", map[string]any{
				"schema":    rd.Schema,
				"config":    rd.ConfigDir,
				"sockets":   rd.Sockets,
				"listeners": rd.Listeners,
			})
		},
	})
}

func TestDoorsRefusals(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Doors,
		Spec: Spec{
			Credentials: acmeOpsCreds(),
			Projects:    []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
		},
		Body: func(r *Run) {
			r.TCP("", "GET", "/api/projects", nil)
			r.TCP("reader", "POST", "/api/projects", map[string]any{"name": "Acme"})
			r.TCP("nobody", "GET", "/api/projects", nil)
			r.TCP("ops", "GET", "/api/no-such-route", nil)
			// execute and proxy routes are absent from the TCP mux.
			r.TCP("ops", "POST", "/api/projects/p_acme/files/list", map[string]any{"path": ""})
			r.TCP("ops", "POST", "/api/terminals", map[string]any{"projectId": "p_acme", "templateId": "shell"})
			r.TCP("ops", "GET", "/api/sessions", nil)
			// The frontend socket without a header resolves the peer's launch identity.
			r.HTTP("", "GET", "/api/projects", nil)
			r.HTTP("reader", "POST", "/api/projects", map[string]any{"name": "Acme"})
		},
	})
}

func TestDoorsScope(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Doors,
		Spec: Spec{
			Credentials: acmeOpsCreds(),
			Projects:    []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
		},
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/sessions", nil, cosScope())
			r.HTTP("ops", "GET", "/api/projects", nil, cosScope())
			r.HTTP("reader", "GET", "/api/sessions", nil, cosScope())
			r.HTTP("ops", "GET", "/api/sessions", nil,
				harness.ReqOpts{Header: http.Header{"X-Relay-Scope": {"unknown-scope"}}})
			r.HTTP("ops", "POST", "/api/chief-of-staff/messages",
				map[string]any{"sessionId": "none", "text": "hello"})
		},
	})
}

func TestDoorsScopedWSReadOnly(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Doors,
		Spec:    Spec{Credentials: acmeOpsCreds()},
		Body: func(r *Run) {
			w := r.WS("/ws", "ops", cosScope())
			w.Send(map[string]any{"type": "join_session", "sessionId": "none"})
			w.Closed()
		},
	})
}
