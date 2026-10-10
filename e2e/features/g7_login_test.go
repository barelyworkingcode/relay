package features

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

var (
	g7Mint        = harness.OutcomeApprove
	g7ApproveEve  = map[string]harness.Outcome{"eve.enrolment.open": harness.OutcomeApprove}
	g7EveReadOnly = harness.CredentialSpec{Name: "eve-reader", Classes: []string{"read"}}
	g7EveWriter   = harness.CredentialSpec{Name: "eve-writer", Classes: []string{"configure"}}
	g7EveCreds    = []harness.CredentialSpec{g7EveReadOnly, g7EveWriter}
)

// g7Presence builds a presence outcome file that approves the login mint and
// sets the other named ops.
func g7Presence(others map[string]harness.Outcome) map[string]harness.Outcome {
	out := map[string]harness.Outcome{"login.bootstrap.mint": g7Mint}
	for k, v := range others {
		out[k] = v
	}
	return out
}

// g7Register registers a fresh authenticator through a freshly minted code.
func g7Register(t *testing.T, i *harness.Instance) (*harness.Authenticator, string) {
	t.Helper()
	a := harness.NewAuthenticator()
	id, resp := i.RegisterPasskey(a, mintLoginCode(t, i))
	if resp.Status != 201 || id == "" {
		t.Fatalf("register answered %d with passkey %q, want 201", resp.Status, id)
	}
	return a, id
}

// g7RequireRefused checks the refusal set of a gated CLI door the owner
// denied: exit 1, nothing on stdout, a denied event with reason
// presence_refused, a denied control_decision audit row naming the gate, and
// the approver's recorded answer.
func g7RequireRefused(t *testing.T, i *harness.Instance, r harness.Result, eventKey, gate string) {
	t.Helper()
	if r.Code != 1 || len(r.Stdout) != 0 {
		t.Fatalf("%s denied: exit %d with %d stdout bytes, want exit 1 and empty stdout", gate, r.Code, len(r.Stdout))
	}
	requireEvent(t, i, harness.EventQuery{Key: eventKey, Trace: r.Trace, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	requireEvent(t, i, harness.EventQuery{Key: "debug.presence.answer", Trace: r.Trace, Fields: map[string]any{"gated_op": gate, "answer": "deny"}})
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		if row["method"] == gate {
			return
		}
	}
	t.Fatalf("no control_decision denied audit row for %s", gate)
}

func g7RequireNoPrompt(t *testing.T, i *harness.Instance, trace string) {
	t.Helper()
	if got := i.Events(harness.EventQuery{Key: "debug.presence.answer", Trace: trace}); len(got) != 0 {
		t.Fatalf("a request refused before the gate still asked the approver %d times", len(got))
	}
}

func g7EvePasskeys(ids ...string) map[string]any {
	list := make([]map[string]string, 0, len(ids))
	for n, id := range ids {
		list = append(list, map[string]string{
			"id": id, "label": "Acme browser " + strconv.Itoa(n),
			"created": "2026-09-07T10:12:31Z", "last_used": "2026-09-07T18:02:11Z",
		})
	}
	return map[string]any{"passkeys": list}
}

// g7ReportEve reports ids as eve's passkey list and returns the pending revocations.
func g7ReportEve(t *testing.T, i *harness.Instance, ids ...string) []string {
	t.Helper()
	resp := i.HTTP(i.Credential("eve-writer")).Do("PUT", "/api/eve/passkeys", g7EvePasskeys(ids...))
	if resp.Status != 200 {
		t.Fatalf("PUT /api/eve/passkeys answered %d, want 200", resp.Status)
	}
	var out struct {
		Revocations []string `json:"revocations"`
	}
	resp.JSON(t, &out)
	return out.Revocations
}

func g7Revocations(t *testing.T, i *harness.Instance) []string {
	t.Helper()
	resp := i.HTTP(i.Credential("eve-reader")).Do("GET", "/api/eve/passkeys/revocations", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/eve/passkeys/revocations answered %d, want 200", resp.Status)
	}
	var out struct {
		Revocations []string `json:"revocations"`
	}
	resp.JSON(t, &out)
	return out.Revocations
}

func g7Contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func g7EveStatus(t *testing.T, i *harness.Instance) (open bool, expires string) {
	t.Helper()
	resp := i.HTTP(i.Credential("eve-reader")).Do("GET", "/api/eve/passkey-enrolment", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/eve/passkey-enrolment answered %d, want 200", resp.Status)
	}
	var out struct {
		Open    bool   `json:"open"`
		Expires string `json:"expires"`
	}
	resp.JSON(t, &out)
	return out.Open, out.Expires
}

func TestLoginPageServed(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{})
	resp := i.Anonymous().Do("GET", "/relay/login", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /relay/login answered %d, want 200", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type %q, want text/html", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control %q, want no-store", cc)
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("the login page carries no Content-Security-Policy header")
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.page", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})
}

func TestLoginCodeMintDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: map[string]harness.Outcome{"login.bootstrap.mint": harness.OutcomeDeny}})
	r := i.CLI("login", "enrol")
	g7RequireRefused(t, i, r, "login.bootstrap.mint", "login.bootstrap.mint")
}

func TestPasskeyRegisterBadCodeRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7Presence(nil)})

	_, none := i.RegisterPasskey(harness.NewAuthenticator(), "")
	if none.Status != 403 {
		t.Fatalf("a register with no code answered %d, want 403", none.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.passkey.register", Trace: none.Trace, Fields: map[string]any{"status": "denied", "reason": "unauthorized"}})
	g7RequireNoPrompt(t, i, none.Trace)

	code := mintLoginCode(t, i)
	_, first := i.RegisterPasskey(harness.NewAuthenticator(), code)
	if first.Status != 201 {
		t.Fatalf("the first register with a fresh code answered %d, want 201", first.Status)
	}
	_, spent := i.RegisterPasskey(harness.NewAuthenticator(), code)
	if spent.Status != 403 {
		t.Fatalf("a register with a spent code answered %d, want 403", spent.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.passkey.register", Trace: spent.Trace, Fields: map[string]any{"status": "denied", "reason": "unauthorized"}})
	g7RequireNoPrompt(t, i, spent.Trace)

	list := i.MustCLI("login", "list")
	requireEvent(t, i, harness.EventQuery{Key: "login.list", Trace: list.Trace, Fields: map[string]any{"status": "ok", "count": 1}})
}

func TestLoginChallengeThrottled(t *testing.T) {
	t.Parallel()
	const tableSize = 64
	i := harness.Start(t, harness.Options{})
	c := i.Anonymous()
	for n := 1; n <= tableSize; n++ {
		if resp := c.Do("POST", "/relay/login/challenge", map[string]string{"ceremony": "register"}); resp.Status != 200 {
			t.Fatalf("challenge %d answered %d, want 200", n, resp.Status)
		}
	}
	over := c.Do("POST", "/relay/login/challenge", map[string]string{"ceremony": "register"})
	if over.Status != 429 {
		t.Fatalf("challenge %d answered %d, want 429", tableSize+1, over.Status)
	}
	if over.Header.Get("Retry-After") == "" {
		t.Fatalf("the 429 carries no Retry-After header")
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.challenge", Trace: over.Trace, Fields: map[string]any{"status": "denied", "reason": "throttled"}})
}

func TestPasskeyList(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7Presence(nil)})
	g7Register(t, i)
	g7Register(t, i)

	r := i.CLI("login", "list")
	if r.Code != 0 {
		t.Fatalf("login list exited %d, want 0", r.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.list", Trace: r.Trace, Fields: map[string]any{"status": "ok", "count": 2}})
}

func TestBrowserSessionsListAndSignOut(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7Presence(nil)})
	a, _ := g7Register(t, i)
	cred, resp := i.SignIn(a)
	if resp.Status != 200 || cred.Token == "" {
		t.Fatalf("sign-in answered %d, want 200 and a token", resp.Status)
	}

	list := i.MustCLI("login", "sessions", "--json")
	var sessions struct {
		Sessions []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Created string `json:"created"`
			Expires string `json:"expires"`
		} `json:"sessions"`
	}
	list.JSON(t, &sessions)
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].ID == "" {
		t.Fatalf("login sessions lists %d sessions, want one with an id: %s", len(sessions.Sessions), list.Stdout)
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.session.list", Trace: list.Trace, Fields: map[string]any{"status": "ok", "count": 1}})
	id := sessions.Sessions[0].ID

	if s := i.HTTP(cred).Do("GET", "/api/projects", nil).Status; s != 200 {
		t.Fatalf("the browser token answered %d before sign-out, want 200", s)
	}
	out := i.MustCLI("login", "sign-out", "--id", id, "--json")
	var ended struct {
		ID string `json:"id"`
	}
	out.JSON(t, &ended)
	if ended.ID != id {
		t.Fatalf("sign-out reports id %q, want %q", ended.ID, id)
	}
	ev := requireEvent(t, i, harness.EventQuery{Key: "login.session.sign_out", Trace: out.Trace, Fields: map[string]any{"status": "ok"}})
	if got := strings.TrimSuffix(ev.Str("credential_id"), "…"); got == "" || !strings.HasPrefix(id, got) {
		t.Fatalf("login.session.sign_out credential_id %q does not name session %q", ev.Str("credential_id"), id)
	}
	if s := i.HTTP(cred).Do("GET", "/api/projects", nil).Status; s != 401 {
		t.Fatalf("the signed-out browser token answered %d, want 401", s)
	}
}

func TestPasskeyRevoke(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7Presence(map[string]harness.Outcome{"login.passkey.revoke": harness.OutcomeApprove})})
	a, id := g7Register(t, i)
	before, resp := i.SignIn(a)
	if resp.Status != 200 {
		t.Fatalf("sign-in before the revoke answered %d, want 200", resp.Status)
	}

	r := i.CLI("login", "revoke", "--id", id)
	if r.Code != 0 {
		t.Fatalf("login revoke exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requirePasskeyID(t, requireEvent(t, i, harness.EventQuery{Key: "login.passkey.revoke", Trace: r.Trace, Fields: map[string]any{"status": "ok"}}), id)

	if _, after := i.SignIn(a); after.Status != 403 {
		t.Fatalf("sign-in with the revoked passkey answered %d, want 403", after.Status)
	}
	if s := i.HTTP(before).Do("GET", "/api/projects", nil).Status; s != 200 {
		t.Fatalf("a session minted before the revoke answered %d, want 200", s)
	}
}

func TestPasskeyRevokeDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7Presence(map[string]harness.Outcome{"login.passkey.revoke": harness.OutcomeDeny})})
	a, id := g7Register(t, i)

	r := i.CLI("login", "revoke", "--id", id)
	g7RequireRefused(t, i, r, "login.passkey.revoke", "login.passkey.revoke")
	if _, resp := i.SignIn(a); resp.Status != 200 {
		t.Fatalf("sign-in after a denied revoke answered %d, want 200", resp.Status)
	}
}

func TestEveEnrolmentOpen(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7ApproveEve, Credentials: g7EveCreds})
	r := i.CLI("eve", "enrol")
	if r.Code != 0 {
		t.Fatalf("eve enrol exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "eve.enrolment.open", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
	open, expires := g7EveStatus(t, i)
	if !open || expires == "" {
		t.Fatalf("status after an approved open is open=%v expires=%q, want open with an expiry", open, expires)
	}
	if _, err := time.Parse(time.RFC3339, expires); err != nil {
		t.Fatalf("expires %q is not RFC 3339: %v", expires, err)
	}
}

func TestEveEnrolmentOpenDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    map[string]harness.Outcome{"eve.enrolment.open": harness.OutcomeDeny},
		Credentials: g7EveCreds,
	})
	g7RequireRefused(t, i, i.CLI("eve", "enrol"), "eve.enrolment.open", "eve.enrolment.open")
	if open, _ := g7EveStatus(t, i); open {
		t.Fatalf("status reads open after a denied open")
	}
}

func TestEvePasskeyList(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g7EveCreds})
	g7ReportEve(t, i, "alpha-acme-eve-passkey", "bravo-acme-eve-passkey")

	r := i.CLI("eve", "list")
	if r.Code != 0 {
		t.Fatalf("eve list exited %d, want 0", r.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: "eve.list", Trace: r.Trace, Fields: map[string]any{"status": "ok", "count": 2}})
}

func TestEvePasskeyRevoke(t *testing.T) {
	t.Parallel()
	const a, b = "alpha-acme-eve-passkey", "bravo-acme-eve-passkey"
	i := harness.Start(t, harness.Options{
		Presence:    map[string]harness.Outcome{"eve.passkey.revoke": harness.OutcomeApprove},
		Credentials: g7EveCreds,
	})
	g7ReportEve(t, i, a, b)

	unknown := i.CLI("eve", "revoke", "--id", "charlie-acme-eve-passkey")
	if unknown.Code != 1 {
		t.Fatalf("revoking an unknown id exited %d, want 1", unknown.Code)
	}
	g7RequireNoPrompt(t, i, unknown.Trace)

	r := i.CLI("eve", "revoke", "--id", a)
	if r.Code != 0 {
		t.Fatalf("eve revoke exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requirePasskeyID(t, requireEvent(t, i, harness.EventQuery{Key: "eve.passkey.revoke", Trace: r.Trace, Fields: map[string]any{"status": "ok"}}), a)
	if got := g7Revocations(t, i); !g7Contains(got, a) {
		t.Fatalf("revocations %v do not list %s", got, a)
	}

	last := i.CLI("eve", "revoke", "--id", b)
	if last.Code != 1 {
		t.Fatalf("revoking the last remaining id exited %d, want 1", last.Code)
	}
	g7RequireNoPrompt(t, i, last.Trace)
	if got := g7Revocations(t, i); g7Contains(got, b) {
		t.Fatalf("revocations %v list the last remaining id", got)
	}
}

func TestEvePasskeyRevokeDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence:    map[string]harness.Outcome{"eve.passkey.revoke": harness.OutcomeDeny},
		Credentials: g7EveCreds,
	})
	g7ReportEve(t, i, "alpha-acme-eve-passkey", "bravo-acme-eve-passkey")

	r := i.CLI("eve", "revoke", "--id", "alpha-acme-eve-passkey")
	g7RequireRefused(t, i, r, "eve.passkey.revoke", "eve.passkey.revoke")
	if got := g7Revocations(t, i); len(got) != 0 {
		t.Fatalf("revocations %v after a denied revoke, want none", got)
	}
}

func TestEveEnrolmentWindowConsume(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7ApproveEve, Credentials: g7EveCreds})
	writer := i.HTTP(i.Credential("eve-writer"))
	consume := func() harness.Response {
		return writer.Do("POST", "/api/eve/passkey-enrolment/consume", map[string]string{"ip": "203.0.113.9", "label": "Acme phone"})
	}

	i.MustCLI("eve", "enrol")
	first := consume()
	if first.Status != 200 {
		t.Fatalf("the first consume answered %d, want 200", first.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "eve.enrolment.consume", Trace: first.Trace, Fields: map[string]any{"status": "ok"}})

	second := consume()
	if second.Status != 409 {
		t.Fatalf("the second consume answered %d, want 409", second.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "eve.enrolment.consume", Trace: second.Trace, Fields: map[string]any{"status": "error", "reason": "conflict"}})

	i.MustCLI("eve", "enrol")
	i.ClockAdvance(6 * time.Minute)
	late := consume()
	if late.Status != 409 {
		t.Fatalf("a consume after the window expired answered %d, want 409", late.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "eve.enrolment.consume", Trace: late.Trace, Fields: map[string]any{"status": "error", "reason": "conflict"}})
}

func TestEvePasskeyReport(t *testing.T) {
	t.Parallel()
	const a, b = "alpha-acme-eve-passkey", "bravo-acme-eve-passkey"
	i := harness.Start(t, harness.Options{
		Presence:    map[string]harness.Outcome{"eve.passkey.revoke": harness.OutcomeApprove},
		Credentials: g7EveCreds,
	})
	writer := i.HTTP(i.Credential("eve-writer"))

	report := writer.Do("PUT", "/api/eve/passkeys", g7EvePasskeys(a, b))
	if report.Status != 200 {
		t.Fatalf("PUT /api/eve/passkeys answered %d, want 200", report.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "eve.passkey.report", Trace: report.Trace, Fields: map[string]any{"status": "ok", "count": 2}})

	i.MustCLI("eve", "revoke", "--id", a)
	if got := g7ReportEve(t, i, a, b); !g7Contains(got, a) {
		t.Fatalf("a report that still lists %s was answered with revocations %v", a, got)
	}
	if got := g7ReportEve(t, i, b); len(got) != 0 {
		t.Fatalf("a report without the revoked id was answered with revocations %v, want none", got)
	}

	denied := i.HTTP(i.Credential("eve-reader")).Do("PUT", "/api/eve/passkeys", g7EvePasskeys(a, b))
	if denied.Status != 403 {
		t.Fatalf("a read credential PUT /api/eve/passkeys answered %d, want 403", denied.Status)
	}
	if got := i.Events(harness.EventQuery{Key: "eve.passkey.report", Trace: denied.Trace}); len(got) != 0 {
		t.Fatalf("a class refusal still wrote %d eve.passkey.report events", len(got))
	}
	found := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		found = found || row["path"] == "/api/eve/passkeys"
	}
	if !found {
		t.Fatalf("no control_decision denied row for /api/eve/passkeys")
	}
}
