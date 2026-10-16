package features

import (
	"regexp"
	"strings"
	"testing"

	"relaye2e/harness"
)

var g8WholeWordEve = regexp.MustCompile(`\bEve\b`)

// g8Rig is a g10 rig whose credentials also include a configure class, so eve's consume can be called.
func g8Rig(t *testing.T, presence map[string]harness.Outcome) *g10Rig {
	t.Helper()
	return g10Start(t, presence, func(o *harness.Options) {
		o.Credentials = append(o.Credentials, harness.CredentialSpec{Name: "configurer", Classes: []string{"configure"}})
	})
}

func g8WaitTargetClosed(i *harness.Instance, target string) harness.Event {
	return i.WaitEvent(harness.EventQuery{Key: "enrolment.window.close", Fields: map[string]any{"status": "ok", "target": target, "close_reason": "cancelled"}}, windowEventDeadline)
}

func TestEnrolmentCloseClosesEveryWindow(t *testing.T) {
	t.Parallel()
	r := g8Rig(t, map[string]harness.Outcome{
		"eve.enrolment.open":   harness.OutcomeApprove,
		"login.bootstrap.mint": harness.OutcomeApprove,
	})
	i := r.i
	consume := func() harness.Response {
		return i.HTTP(i.Credential("configurer")).Do("POST", "/api/eve/passkey-enrolment/consume", map[string]string{"ip": "203.0.113.7", "label": "Acme phone"})
	}

	i.MustCLI("eve", "enrol")
	code := mintLoginCode(t, i)
	profileAddr := i.EnrolOpen(r.profile)

	res := i.MustCLI("enrol", "close", "--json")
	var closed struct {
		Closed []string `json:"closed"`
	}
	res.JSON(t, &closed)
	want := []string{"profile", "home_work", "relay_login"}
	if strings.Join(closed.Closed, ",") != strings.Join(want, ",") {
		t.Fatalf("enrol close --json reports %v, want %v", closed.Closed, want)
	}
	g10WaitClose(i, "cancelled")
	g8WaitTargetClosed(i, "home_work")
	g8WaitTargetClosed(i, "relay_login")

	if resp := consume(); resp.Status != 409 {
		t.Fatalf("eve's consume after enrol close answered %d, want 409", resp.Status)
	}
	if _, resp := i.RegisterPasskey(harness.NewAuthenticator(), code); resp.Status != 403 {
		t.Fatalf("registering a passkey with a cancelled sign-in code answered %d, want 403", resp.Status)
	}
	if !i.EnrolRefused(profileAddr) {
		t.Fatalf("a connection to %s succeeded after enrol close", profileAddr)
	}
	g10RequireWindowClosed(t, i)
	if open := i.HTTP(i.Credential("reader")).Do("GET", "/api/eve/passkey-enrolment", nil); open.Status != 200 || strings.Contains(string(open.Body), `"open":true`) {
		t.Fatalf("the Home|Work window reads %d %s after enrol close, want closed", open.Status, open.Body)
	}
}

func TestEnrolmentCloseReportsOnlyWhatClosed(t *testing.T) {
	t.Parallel()
	r := g8Rig(t, map[string]harness.Outcome{"eve.enrolment.open": harness.OutcomeApprove})
	i := r.i
	var closed struct {
		Closed []string `json:"closed"`
	}

	i.MustCLI("eve", "enrol")
	i.MustCLI("enrol", "close", "--json").JSON(t, &closed)
	if len(closed.Closed) != 1 || closed.Closed[0] != "home_work" {
		t.Fatalf("enrol close --json reports %v, want [home_work]", closed.Closed)
	}
	g8WaitTargetClosed(i, "home_work")

	closed.Closed = nil
	i.MustCLI("enrol", "close", "--json").JSON(t, &closed)
	if len(closed.Closed) != 0 {
		t.Fatalf("enrol close with nothing open reports %v, want none", closed.Closed)
	}
}

func TestEnrolmentTextSaysHomeWork(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, r harness.Result, what string) {
		t.Helper()
		for name, out := range map[string][]byte{"stdout": r.Stdout, "stderr": r.Stderr} {
			if g8WholeWordEve.Match(out) {
				t.Errorf("%s %s says Eve:\n%s", what, name, out)
			}
		}
	}

	ok := g8Rig(t, map[string]harness.Outcome{
		"eve.enrolment.open":   harness.OutcomeApprove,
		"login.bootstrap.mint": harness.OutcomeApprove,
	})
	check(t, ok.i.MustCLI("eve", "enrol"), "eve enrol")
	check(t, ok.i.MustCLI("login", "enrol"), "login enrol")
	check(t, ok.i.MustCLI("enrol", "open", "--profile", ok.profile), "enrol open")
	check(t, ok.i.MustCLI("enrol", "open", "--profile", ok.profile, "--json"), "enrol open --json")
	check(t, ok.i.MustCLI("enrol", "close"), "enrol close")
	check(t, ok.i.MustCLI("enrol", "close"), "enrol close with nothing open")
	check(t, ok.i.CLI("enrol", "open", "--profile", "acme-no-such-id"), "enrol open for an unknown id")

	denied := g8Rig(t, map[string]harness.Outcome{
		"eve.enrolment.open":    harness.OutcomeDeny,
		"login.bootstrap.mint":  harness.OutcomeDeny,
		"enrolment.window.open": harness.OutcomeDeny,
	})
	check(t, denied.i.CLI("eve", "enrol"), "a refused eve enrol")
	check(t, denied.i.CLI("login", "enrol"), "a refused login enrol")
	check(t, denied.i.CLI("enrol", "open", "--profile", denied.profile), "a refused enrol open")
}
