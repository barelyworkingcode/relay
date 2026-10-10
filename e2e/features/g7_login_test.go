package features

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

var g7Creds = []harness.CredentialSpec{
	{Name: "reader", Classes: []string{"read"}},
	{Name: "configurer", Classes: []string{"configure"}},
}

func g7Presence(extra map[string]harness.Outcome) map[string]harness.Outcome {
	out := map[string]harness.Outcome{"login.bootstrap.mint": harness.OutcomeApprove}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// g7Register mints a code and registers a fresh authenticator.
func g7Register(t *testing.T, i *harness.Instance) (*harness.Authenticator, string) {
	t.Helper()
	a := harness.NewAuthenticator()
	id, resp := i.RegisterPasskey(a, mintLoginCode(t, i))
	if resp.Status != 201 || id == "" {
		t.Fatalf("register answered %d with passkey %q, want 201", resp.Status, id)
	}
	return a, id
}

// g7DeniedDecision reports whether a control_decision denied row names the gate.
func g7DeniedDecision(i *harness.Instance, gate string) bool {
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		if row["method"] == gate {
			return true
		}
	}
	return false
}

func g7RequireRefusedCLI(t *testing.T, i *harness.Instance, r harness.Result, key string) {
	t.Helper()
	if r.Code != 1 {
		t.Fatalf("a refused %s exited %d, want 1", key, r.Code)
	}
	requireEvent(t, i, harness.EventQuery{Key: key, Trace: r.Trace, Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
	if !g7DeniedDecision(i, key) {
		t.Fatalf("no control_decision denied row for %s", key)
	}
}

func TestLoginCodeMintDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: map[string]harness.Outcome{"login.bootstrap.mint": harness.OutcomeDeny}})

	r := i.CLI("login", "enrol")
	if len(r.Stdout) != 0 {
		t.Fatalf("a refused mint printed %d stdout bytes, want none", len(r.Stdout))
	}
	g7RequireRefusedCLI(t, i, r, "login.bootstrap.mint")
	if got := i.Events(harness.EventQuery{Key: "login.bootstrap.mint", Fields: map[string]any{"status": "ok"}}); len(got) != 0 {
		t.Fatalf("a refused mint wrote %d ok events", len(got))
	}
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
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "nonce-") {
		t.Fatalf("Content-Security-Policy %q lacks a nonce", csp)
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.page", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})
}

func TestPasskeyRegisterBadCodeRefused(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7Presence(nil)})

	requireRefused := func(what string, resp harness.Response) {
		t.Helper()
		if resp.Status != 403 {
			t.Fatalf("a register with %s answered %d, want 403", what, resp.Status)
		}
		requireEvent(t, i, harness.EventQuery{Key: "login.passkey.register", Trace: resp.Trace, Fields: map[string]any{"status": "denied", "reason": "unauthorized"}})
	}

	_, resp := i.RegisterPasskey(harness.NewAuthenticator(), "")
	requireRefused("no code", resp)

	code := mintLoginCode(t, i)
	good := harness.NewAuthenticator()
	if id, resp := i.RegisterPasskey(good, code); resp.Status != 201 || id == "" {
		t.Fatalf("the first register with a fresh code answered %d, want 201", resp.Status)
	}
	_, resp = i.RegisterPasskey(harness.NewAuthenticator(), code)
	requireRefused("a spent code", resp)

	found := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		found = found || row["path"] == "/relay/login/verify"
	}
	if !found {
		t.Fatalf("no control_decision denied row for /relay/login/verify")
	}

	r := i.MustCLI("login", "list")
	ev := requireEvent(t, i, harness.EventQuery{Key: "login.list", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
	if n, _ := ev["count"].(float64); n != 1 {
		t.Fatalf("login.list count %v after two refused registers and one good one, want 1", ev["count"])
	}
}

func TestLoginChallengeThrottled(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{})
	c := i.Anonymous()

	// The challenge table holds 64 outstanding challenges, so a 429 comes by the 65th.
	var throttled harness.Response
	for n := 1; n <= 65; n++ {
		resp := c.Do("POST", "/relay/login/challenge", map[string]string{"ceremony": "register"})
		if resp.Status == http.StatusTooManyRequests {
			throttled = resp
			break
		}
		if resp.Status != 200 {
			t.Fatalf("challenge %d answered %d, want 200 or 429", n, resp.Status)
		}
	}
	if throttled.Status != http.StatusTooManyRequests {
		t.Fatalf("65 open challenges and none was throttled")
	}
	if throttled.Header.Get("Retry-After") == "" {
		t.Fatalf("the 429 carries no Retry-After header")
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.challenge", Trace: throttled.Trace, Fields: map[string]any{"status": "denied", "reason": "throttled"}})
}

func TestPasskeyList(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7Presence(nil)})
	g7Register(t, i)
	g7Register(t, i)

	r := i.CLI("login", "list")
	if r.Code != 0 {
		t.Fatalf("login list exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	ev := requireEvent(t, i, harness.EventQuery{Key: "login.list", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
	if n, _ := ev["count"].(float64); n != 2 {
		t.Fatalf("login.list count %v after two registrations, want 2", ev["count"])
	}
}

func TestBrowserSessionsListAndSignOut(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7Presence(nil)})
	a, _ := g7Register(t, i)
	cred, resp := i.SignIn(a)
	if resp.Status != 200 || cred.Token == "" {
		t.Fatalf("sign-in answered %d, want 200 and a token", resp.Status)
	}

	type sessions struct {
		Sessions []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Created string `json:"created"`
			Expires string `json:"expires"`
		} `json:"sessions"`
	}
	list := i.MustCLI("login", "sessions", "--json")
	var got sessions
	list.JSON(t, &got)
	if len(got.Sessions) != 1 || got.Sessions[0].ID == "" || got.Sessions[0].Created == "" || got.Sessions[0].Expires == "" {
		t.Fatalf("login sessions listed %+v, want one complete session", got.Sessions)
	}
	ev := requireEvent(t, i, harness.EventQuery{Key: "login.session.list", Trace: list.Trace, Fields: map[string]any{"status": "ok"}})
	if n, _ := ev["count"].(float64); n != 1 {
		t.Fatalf("login.session.list count %v, want 1", ev["count"])
	}
	if s := i.HTTP(cred).Do("GET", "/api/projects", nil).Status; s != 200 {
		t.Fatalf("the session token answered %d before sign-out, want 200", s)
	}

	if r := i.CLI("login", "sign-out", "--id", "no-such-session", "--json"); r.Code != 1 {
		t.Fatalf("sign-out of an unknown id exited %d, want 1", r.Code)
	}

	id := got.Sessions[0].ID
	out := i.MustCLI("login", "sign-out", "--id", id, "--json")
	var ended struct {
		ID string `json:"id"`
	}
	out.JSON(t, &ended)
	if ended.ID != id {
		t.Fatalf("sign-out printed id %q, want %q", ended.ID, id)
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.session.sign_out", Trace: out.Trace, Fields: map[string]any{"status": "ok"}})

	if s := i.HTTP(cred).Do("GET", "/api/projects", nil).Status; s != 401 {
		t.Fatalf("the signed-out token answered %d, want 401", s)
	}
	var after sessions
	i.MustCLI("login", "sessions", "--json").JSON(t, &after)
	if len(after.Sessions) != 0 {
		t.Fatalf("login sessions still lists %d sessions after sign-out", len(after.Sessions))
	}
}

func TestPasskeyRevoke(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7Presence(map[string]harness.Outcome{"login.passkey.revoke": harness.OutcomeApprove})})
	a, passkeyID := g7Register(t, i)
	session, resp := i.SignIn(a)
	if resp.Status != 200 {
		t.Fatalf("sign-in before the revoke answered %d, want 200", resp.Status)
	}

	r := i.CLI("login", "revoke", "--id", passkeyID)
	if r.Code != 0 {
		t.Fatalf("login revoke exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requirePasskeyID(t, requireEvent(t, i, harness.EventQuery{Key: "login.passkey.revoke", Trace: r.Trace, Fields: map[string]any{"status": "ok"}}), passkeyID)

	_, resp = i.SignIn(a)
	if resp.Status != 403 {
		t.Fatalf("sign-in with the revoked passkey answered %d, want 403", resp.Status)
	}
	requireEvent(t, i, harness.EventQuery{Key: "login.sign_in", Trace: resp.Trace, Fields: map[string]any{"status": "denied"}})
	// A revoke does not end a session the passkey already minted.
	if s := i.HTTP(session).Do("GET", "/api/projects", nil).Status; s != 200 {
		t.Fatalf("a session minted before the revoke answered %d, want 200", s)
	}
}

func TestPasskeyRevokeDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Presence: g7Presence(map[string]harness.Outcome{"login.passkey.revoke": harness.OutcomeDeny})})
	a, passkeyID := g7Register(t, i)

	r := i.CLI("login", "revoke", "--id", passkeyID)
	g7RequireRefusedCLI(t, i, r, "login.passkey.revoke")

	if cred, resp := i.SignIn(a); resp.Status != 200 || cred.Token == "" {
		t.Fatalf("sign-in after a refused revoke answered %d, want 200", resp.Status)
	}
}

func TestEveEnrolmentOpen(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g7Creds,
		Presence:    map[string]harness.Outcome{"eve.enrolment.open": harness.OutcomeApprove},
	})
	reader := i.HTTP(i.Credential("reader"))
	status := func() (open bool, expires string) {
		resp := reader.Do("GET", "/api/eve/passkey-enrolment", nil)
		if resp.Status != 200 {
			t.Fatalf("GET /api/eve/passkey-enrolment answered %d, want 200", resp.Status)
		}
		var body struct {
			Open    bool   `json:"open"`
			Expires string `json:"expires"`
		}
		resp.JSON(t, &body)
		return body.Open, body.Expires
	}
	if open, _ := status(); open {
		t.Fatalf("a fresh instance reports an open eve enrolment window")
	}

	r := i.CLI("eve", "enrol")
	if r.Code != 0 {
		t.Fatalf("eve enrol exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requireEvent(t, i, harness.EventQuery{Key: "eve.enrolment.open", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
	open, expires := status()
	if !open {
		t.Fatalf("the window is closed after an approved open")
	}
	at, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		t.Fatalf("expires %q is not RFC 3339: %v", expires, err)
	}
	if left := at.Sub(i.Clock().Now); left <= 4*time.Minute || left > 5*time.Minute+time.Second {
		t.Fatalf("the window has %v left, want a five-minute window", left)
	}
}

func TestEveEnrolmentOpenDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g7Creds,
		Presence:    map[string]harness.Outcome{"eve.enrolment.open": harness.OutcomeDeny},
	})

	r := i.CLI("eve", "enrol")
	g7RequireRefusedCLI(t, i, r, "eve.enrolment.open")

	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/eve/passkey-enrolment", nil)
	var body struct {
		Open bool `json:"open"`
	}
	resp.JSON(t, &body)
	if resp.Status != 200 || body.Open {
		t.Fatalf("after a refused open the status is %d open=%v, want 200 and closed", resp.Status, body.Open)
	}
}

// g7Report sends eve's passkey list to relay and returns the response.
func g7Report(t *testing.T, i *harness.Instance, ids ...string) harness.Response {
	t.Helper()
	var list []map[string]string
	for _, id := range ids {
		list = append(list, map[string]string{
			"id": id, "label": "Acme browser " + id,
			"created": "2026-10-01T09:00:00Z", "last_used": "2026-10-02T09:00:00Z",
		})
	}
	return i.HTTP(i.Credential("configurer")).Do("PUT", "/api/eve/passkeys", map[string]any{"passkeys": list})
}

func g7Revocations(t *testing.T, i *harness.Instance) []string {
	t.Helper()
	resp := i.HTTP(i.Credential("reader")).Do("GET", "/api/eve/passkeys/revocations", nil)
	if resp.Status != 200 {
		t.Fatalf("GET /api/eve/passkeys/revocations answered %d, want 200", resp.Status)
	}
	var body struct {
		Revocations []string `json:"revocations"`
	}
	resp.JSON(t, &body)
	return body.Revocations
}

func TestEvePasskeyList(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{Credentials: g7Creds})
	if resp := g7Report(t, i, "acme-eve-one", "acme-eve-two"); resp.Status != 200 {
		t.Fatalf("PUT /api/eve/passkeys answered %d, want 200", resp.Status)
	}

	r := i.CLI("eve", "list")
	if r.Code != 0 {
		t.Fatalf("eve list exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	ev := requireEvent(t, i, harness.EventQuery{Key: "eve.list", Trace: r.Trace, Fields: map[string]any{"status": "ok"}})
	if n, _ := ev["count"].(float64); n != 2 {
		t.Fatalf("eve.list count %v, want 2", ev["count"])
	}
}

func TestEvePasskeyRevoke(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g7Creds,
		Presence:    map[string]harness.Outcome{"eve.passkey.revoke": harness.OutcomeApprove},
	})

	// Refused before the gate: an unknown id, and the only passkey eve reported.
	g7Report(t, i, "acme-eve-only")
	for _, id := range []string{"acme-eve-unknown", "acme-eve-only"} {
		r := i.CLI("eve", "revoke", "--id", id)
		if r.Code != 1 {
			t.Fatalf("eve revoke --id %s exited %d, want 1", id, r.Code)
		}
		if got := i.Events(harness.EventQuery{Key: "debug.presence.answer", Trace: r.Trace}); len(got) != 0 {
			t.Fatalf("eve revoke --id %s reached the presence gate (%d answers)", id, len(got))
		}
	}
	if got := g7Revocations(t, i); len(got) != 0 {
		t.Fatalf("refused revokes left pending revocations %v", got)
	}

	g7Report(t, i, "acme-eve-a", "acme-eve-b")
	r := i.CLI("eve", "revoke", "--id", "acme-eve-a")
	if r.Code != 0 {
		t.Fatalf("eve revoke exited %d\nstderr: %s", r.Code, r.Stderr)
	}
	requirePasskeyID(t, requireEvent(t, i, harness.EventQuery{Key: "eve.passkey.revoke", Trace: r.Trace, Fields: map[string]any{"status": "ok"}}), "acme-eve-a")
	if got := g7Revocations(t, i); len(got) != 1 || got[0] != "acme-eve-a" {
		t.Fatalf("revocations %v, want [acme-eve-a]", got)
	}
}

func TestEvePasskeyRevokeDenied(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g7Creds,
		Presence:    map[string]harness.Outcome{"eve.passkey.revoke": harness.OutcomeDeny},
	})
	g7Report(t, i, "acme-eve-a", "acme-eve-b")

	r := i.CLI("eve", "revoke", "--id", "acme-eve-a")
	g7RequireRefusedCLI(t, i, r, "eve.passkey.revoke")
	if got := g7Revocations(t, i); len(got) != 0 {
		t.Fatalf("a refused revoke queued %v", got)
	}
}

func TestEveEnrolmentWindowConsume(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g7Creds,
		Presence:    map[string]harness.Outcome{"eve.enrolment.open": harness.OutcomeApprove},
	})
	configure := i.HTTP(i.Credential("configurer"))
	consume := func() harness.Response {
		return configure.Do("POST", "/api/eve/passkey-enrolment/consume", map[string]string{"ip": "203.0.113.7", "label": "Acme phone"})
	}
	requireConflict := func(what string, resp harness.Response) {
		t.Helper()
		if resp.Status != 409 {
			t.Fatalf("%s answered %d, want 409", what, resp.Status)
		}
		requireEvent(t, i, harness.EventQuery{Key: "eve.enrolment.consume", Trace: resp.Trace, Fields: map[string]any{"status": "error", "reason": "conflict"}})
	}

	i.MustCLI("eve", "enrol")
	resp := consume()
	if resp.Status != 200 {
		t.Fatalf("the first consume answered %d, want 200", resp.Status)
	}
	var body struct {
		Expires string `json:"expires"`
	}
	resp.JSON(t, &body)
	if _, err := time.Parse(time.RFC3339, body.Expires); err != nil {
		t.Fatalf("consume expires %q is not RFC 3339: %v", body.Expires, err)
	}
	requireEvent(t, i, harness.EventQuery{Key: "eve.enrolment.consume", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})

	requireConflict("a second consume", consume())

	i.MustCLI("eve", "enrol")
	i.ClockAdvance(6 * time.Minute)
	requireConflict("a consume after the window expired", consume())
}

func TestEvePasskeyReport(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Credentials: g7Creds,
		Presence:    map[string]harness.Outcome{"eve.passkey.revoke": harness.OutcomeApprove},
	})

	resp := g7Report(t, i, "acme-eve-a", "acme-eve-b")
	if resp.Status != 200 {
		t.Fatalf("PUT /api/eve/passkeys answered %d, want 200", resp.Status)
	}
	ev := requireEvent(t, i, harness.EventQuery{Key: "eve.passkey.report", Trace: resp.Trace, Fields: map[string]any{"status": "ok"}})
	if n, _ := ev["count"].(float64); n != 2 {
		t.Fatalf("eve.passkey.report count %v, want 2", ev["count"])
	}
	var first struct {
		Revocations []string `json:"revocations"`
	}
	resp.JSON(t, &first)
	if len(first.Revocations) != 0 {
		t.Fatalf("a first report was answered with revocations %v, want none", first.Revocations)
	}

	i.MustCLI("eve", "revoke", "--id", "acme-eve-a")
	var second struct {
		Revocations []string `json:"revocations"`
	}
	g7Report(t, i, "acme-eve-a", "acme-eve-b").JSON(t, &second)
	if len(second.Revocations) != 1 || second.Revocations[0] != "acme-eve-a" {
		t.Fatalf("the report after a revoke was answered with %v, want [acme-eve-a]", second.Revocations)
	}

	// A read credential is refused by the bearer middleware, before any handler.
	denied := i.HTTP(i.Credential("reader")).Do("PUT", "/api/eve/passkeys", map[string]any{"passkeys": []any{}})
	if denied.Status != 403 {
		t.Fatalf("PUT /api/eve/passkeys with a read credential answered %d, want 403", denied.Status)
	}
	if got := i.Events(harness.EventQuery{Key: "eve.passkey.report", Trace: denied.Trace}); len(got) != 0 {
		t.Fatalf("a refused report wrote %d eve.passkey.report events", len(got))
	}
	found := false
	for _, row := range i.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"}) {
		found = found || (row["path"] == "/api/eve/passkeys" && row["method"] == "PUT")
	}
	if !found {
		t.Fatalf("no control_decision denied row for PUT /api/eve/passkeys")
	}
}
