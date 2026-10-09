package features

import (
	"encoding/hex"
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
	code = strings.TrimSpace(code)
	if _, err := hex.DecodeString(code); !ok || code == "" || err != nil {
		t.Fatalf("first stdout line %q is not 'login code: <hex characters>'", first)
	}
	return code
}

func TestPasskeyRegisterAndSignIn(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: approveLoginCode})
	a := harness.NewAuthenticator()

	passkeyID, resp := i.RegisterPasskey(a, mintLoginCode(t, i))
	if resp.Status != 201 || passkeyID == "" {
		t.Fatalf("register answered %d with passkey %q, want 201", resp.Status, passkeyID)
	}
	requirePasskeyID(t, requireEvent(t, i, harness.EventQuery{Key: "login.passkey.register", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}}), passkeyID)

	cred, resp := i.SignIn(a)
	if resp.Status != 200 || cred.Token == "" {
		t.Fatalf("sign-in answered %d, want 200 and a token", resp.Status)
	}
	requirePasskeyID(t, requireEvent(t, i, harness.EventQuery{Key: "login.sign_in", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}}), passkeyID)

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

// requirePasskeyID checks the event's passkey_id names the passkey. Events cut
// long ids to a prefix and an ellipsis, so the prefix is what is compared.
func requirePasskeyID(t *testing.T, ev harness.Event, passkeyID string) {
	t.Helper()
	got := strings.TrimSuffix(ev.Str("passkey_id"), "…")
	if got == "" || !strings.HasPrefix(passkeyID, got) {
		t.Fatalf("%s passkey_id %q is not a prefix of the registered id %q", ev.Str("event"), ev.Str("passkey_id"), passkeyID)
	}
}
