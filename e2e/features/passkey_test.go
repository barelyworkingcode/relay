package features

import (
	"strings"
	"testing"

	"relaye2e/harness"
)

var approveLoginCode = map[string]harness.Outcome{"login.bootstrap.mint": harness.OutcomeApprove}

// mintLoginCode runs `relay login enrol` and returns the code on its first line.
func mintLoginCode(t *testing.T, i *harness.Instance) string {
	t.Helper()
	r := i.MustCLI("login", "enrol")
	first, _, _ := strings.Cut(string(r.Stdout), "\n")
	code, ok := strings.CutPrefix(first, "login code: ")
	if !ok || len(strings.TrimSpace(code)) != 16 {
		t.Fatalf("first stdout line %q is not 'login code: <16 hex characters>'", first)
	}
	return strings.TrimSpace(code)
}

func TestPasskeyRegisterAndSignIn(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveLoginCode})
	a := harness.NewAuthenticator()

	passkeyID, resp := i.RegisterPasskey(a, mintLoginCode(t, i))
	if resp.Status != 201 || passkeyID == "" {
		t.Fatalf("register answered %d with passkey %q, want 201", resp.Status, passkeyID)
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.passkey.register", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "passkey_id": passkeyID}})

	cred, resp := i.SignIn(a)
	if resp.Status != 200 || cred.Token == "" {
		t.Fatalf("sign-in answered %d, want 200 and a token", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.sign_in", Trace: resp.Trace, Fields: map[string]any{"status": "ok", "passkey_id": passkeyID}})

	if s := i.HTTP(cred).Do("GET", "/api/projects", nil).Status; s != 200 {
		t.Fatalf("GET /api/projects with the session token answered %d, want 200", s)
	}
}

func TestPasskeyUnknownCredentialRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{})
	cred, resp := i.SignIn(harness.NewAuthenticator())
	if resp.Status != 403 || cred.Token != "" {
		t.Fatalf("an unregistered authenticator got %d, want 403 and no token", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.sign_in", Trace: resp.Trace, Fields: map[string]any{"status": "denied", "reason": "unauthorized"}})
	found := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		found = found || row["path"] == "/relay/login/verify"
	}
	if !found {
		t.Fatalf("no control_decision denied row for /relay/login/verify")
	}
}
