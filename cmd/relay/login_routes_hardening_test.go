package main

// The unauthenticated half of ADR-016 decision 5, from the attacker's side:
// what a caller with no code, no credential and no passkey can make relay do.
// The ceremonies themselves are driven by the same software authenticator the
// rest of login_routes_test.go uses.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/ceremonylimit"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/login"
	"github.com/barelyworkingcode/relay/internal/login/loginfake"
)

func lrhStat(t *testing.T, s *lrServer) os.FileInfo {
	t.Helper()
	info, err := os.Stat(filepath.Join(s.dir, "settings.json"))
	if err != nil {
		t.Fatalf("stat settings.json: %v", err)
	}
	return info
}

func lrhRead(t *testing.T, s *lrServer) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	return data
}

// A refused registration must not write settings.json at all. Byte equality is
// not enough on its own — a rewrite of identical bytes is still a write, and it
// is the write, not the content, that gives an unauthenticated caller a lever
// on the file every credential lives in — so the identity of the file is
// asserted too.
func TestLoginRoutes_ARefusedRegistrationWritesNothing(t *testing.T) {
	// Each case sets up whatever it legitimately needs on disk and returns the
	// one refusal that is measured, so the only candidate writer between the
	// two stats is that refusal.
	cases := map[string]func(t *testing.T, s *lrServer) func() (*http.Response, []byte){
		"no code at all": func(t *testing.T, s *lrServer) func() (*http.Response, []byte) {
			return func() (*http.Response, []byte) { return s.register(loginfake.NewSoftAuthenticator(t), "", 1) }
		},
		"a wrong code against a live anchor": func(t *testing.T, s *lrServer) func() (*http.Response, []byte) {
			s.mintCode()
			return func() (*http.Response, []byte) {
				return s.register(loginfake.NewSoftAuthenticator(t), "not-the-code", 1)
			}
		},
		"the passkey cap reached": func(t *testing.T, s *lrServer) func() (*http.Response, []byte) {
			for i := 0; i < login.MaxRegisteredPasskeys; i++ {
				if resp, body := s.register(loginfake.NewSoftAuthenticator(t), s.mintCode(), 1); resp.StatusCode != http.StatusCreated {
					t.Fatalf("passkey %d: status %d, body %s", i+1, resp.StatusCode, body)
				}
			}
			code := s.mintCode()
			return func() (*http.Response, []byte) { return s.register(loginfake.NewSoftAuthenticator(t), code, 1) }
		},
		"a duplicate credential id": func(t *testing.T, s *lrServer) func() (*http.Response, []byte) {
			a := loginfake.NewSoftAuthenticator(t)
			if resp, body := s.register(a, s.mintCode(), 1); resp.StatusCode != http.StatusCreated {
				t.Fatalf("setup registration: status %d, body %s", resp.StatusCode, body)
			}
			code := s.mintCode()
			return func() (*http.Response, []byte) { return s.register(a, code, 1) }
		},
	}

	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			s := lrNewServer(t)
			refuse := setup(t, s)

			before := lrhStat(t, s)
			beforeBytes := lrhRead(t, s)

			resp, body := refuse()
			if resp.StatusCode == http.StatusCreated {
				t.Fatalf("the registration was accepted: %s", body)
			}
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403: %s", resp.StatusCode, body)
			}

			after := lrhStat(t, s)
			if !os.SameFile(before, after) {
				t.Fatal("a refused registration replaced settings.json — an unauthenticated caller drove relay's settings writer")
			}
			if got := lrhRead(t, s); string(got) != string(beforeBytes) {
				t.Fatalf("a refused registration changed settings.json:\n before %s\n after  %s", beforeBytes, got)
			}
			// The anchor a refused registration must not spend: still there,
			// because nothing was written at all.
			if name == "the passkey cap reached" || name == "a duplicate credential id" {
				if s.store.Get().LoginBootstrap == nil {
					t.Fatal("the refused registration spent the operator's code")
				}
			}
		})
	}
}

// A registration that verifies and is then refused for want of a code is a
// failed login attempt, and must be counted as one: the verifier cannot see
// the code (ADR-016 decision 2), so if the route does not charge the attempt,
// guessing the anchor is free.
func TestLoginRoutes_BootstrapCodeGuessesAreThrottled(t *testing.T) {
	s := lrNewServer(t)

	const guesses = 30
	throttled := 0
	for i := 0; i < guesses; i++ {
		resp, body := s.register(loginfake.NewSoftAuthenticator(t), "guess-the-anchor", 1)
		switch resp.StatusCode {
		case http.StatusTooManyRequests:
			throttled++
		case http.StatusForbidden:
		default:
			t.Fatalf("guess %d: status %d, body %s", i, resp.StatusCode, body)
		}
	}
	if throttled == 0 {
		t.Fatalf("%d bootstrap-code guesses, 0 throttled: a registration refused only by the route costs the caller nothing", guesses)
	}
}

// The interleaving the review demonstrated: a codeless registration is a
// failure to the route and used to be a success to the limiter, so an attacker
// could clear the penalty for their own failed assertions whenever they liked.
func TestLoginRoutes_ACodelessRegistrationDoesNotClearTheCeremonyLimiter(t *testing.T) {
	s := lrNewServer(t)
	a, _ := s.enrolled()

	// A failed assertion that is genuinely a ceremony: the signature verifies
	// and the user handle does not.
	failAssertion := func() int {
		resp, _ := s.assertWith(a, 9, []byte("not-the-owner"))
		return resp.StatusCode
	}

	const rounds = 4
	for round := 0; round < rounds; round++ {
		for i := 0; i < ceremonylimit.FailureGrace; i++ {
			failAssertion()
		}
		// Under the defect this is what wiped `failures` and `nextAllowed`.
		if resp, body := s.register(loginfake.NewSoftAuthenticator(t), "", 1); resp.StatusCode == http.StatusCreated {
			t.Fatalf("round %d: a registration with no code was accepted: %s", round, body)
		}
	}

	resp, body := s.assert(a, 10)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after %d failed ceremonies interleaved with codeless registrations the limiter answered %d, want 429: %s",
			rounds*ceremonylimit.FailureGrace, resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("a throttled ceremony carries no Retry-After")
	}
}

// `relay audit` is ground truth for anything relay gates (CLAUDE.md), and this
// is the one surface where an unauthenticated caller can obtain a
// control-plane credential. Every outcome is recorded, and no record carries
// the plaintext, the stored hash, or a bootstrap code — the caller's guess
// included, since a guess is caller-chosen bytes and one of them could be
// right.
func TestLoginRoutes_LoginOutcomesAreAuditedAndCarryNoSecret(t *testing.T) {
	s := lrNewServer(t)

	const guess = "GUESSED-ANCHOR-7K2P-QX4M"

	a := loginfake.NewSoftAuthenticator(t)
	code := s.mintCode()
	if resp, body := s.register(a, code, 1); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", resp.StatusCode, body)
	}
	passkeyID := lrB64(a.CredID)
	token := s.signIn(a, 2)
	cred := s.loginCredential()

	if resp, body := s.register(loginfake.NewSoftAuthenticator(t), guess, 1); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("guessed code: status %d, want 403, body %s", resp.StatusCode, body)
	}
	unknown := loginfake.NewSoftAuthenticator(t)
	if resp, body := s.assertWith(unknown, 1, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unknown credential id: status %d, want 403, body %s", resp.StatusCode, body)
	}

	decisions := s.auditor.forPath(loginVerifyPath)
	if len(decisions) == 0 {
		t.Fatal("no login outcome reached the auditor at all")
	}

	find := func(match func(control.ControlDecision) bool) *control.ControlDecision {
		for i := range decisions {
			if match(decisions[i]) {
				return &decisions[i]
			}
		}
		return nil
	}

	signedIn := find(func(d control.ControlDecision) bool { return d.Allowed && d.CredID == cred.ID })
	if signedIn == nil {
		t.Fatalf("a successful login wrote no record naming the credential it minted (%s): %+v", cred.ID, decisions)
	}
	if signedIn.Transport != control.TransportTCP || signedIn.Method != http.MethodPost {
		t.Errorf("login record = %s %s on %s, want POST %s on tcp", signedIn.Method, signedIn.Path, signedIn.Transport, loginVerifyPath)
	}
	if signedIn.Reason != "" {
		t.Errorf("an allowed record carries a reason: %q", signedIn.Reason)
	}

	registered := find(func(d control.ControlDecision) bool { return d.Allowed && d.CredID == abbreviatePasskeyID(passkeyID) })
	if registered == nil {
		t.Errorf("a successful registration wrote no record naming the passkey (%s): %+v", abbreviatePasskeyID(passkeyID), decisions)
	}

	badCode := find(func(d control.ControlDecision) bool { return !d.Allowed && d.Reason == errBootstrapCodeInvalid.Error() })
	if badCode == nil {
		t.Errorf("a refused bootstrap code wrote no record: %+v", decisions)
	}

	rejected := find(func(d control.ControlDecision) bool {
		return !d.Allowed && d.Reason == login.ErrWebAuthnAssertionRejected.Error()
	})
	if rejected == nil {
		t.Errorf("a refused assertion wrote no record: %+v", decisions)
	}

	secrets := map[string]string{
		"the minted plaintext":  token,
		"the bootstrap code":    code,
		"the caller's guess":    guess,
		"the credential's hash": cred.Hash,
	}
	encoded, err := json.Marshal(decisions)
	if err != nil {
		t.Fatalf("marshal decisions: %v", err)
	}
	for name, secret := range secrets {
		if secret == "" {
			t.Fatalf("%s is empty, so the leak check below proves nothing", name)
		}
		if strings.Contains(string(encoded), secret) {
			t.Errorf("%s appears in an audit record: %s", name, encoded)
		}
	}

	for _, d := range decisions {
		if len(d.Reason) > loginAuditMaxReasonBytes {
			t.Errorf("a recorded reason is %d bytes, want <= %d", len(d.Reason), loginAuditMaxReasonBytes)
		}
	}
}

// The throttle is the only refusal an unauthenticated caller can provoke at
// line rate, so recording it would hand that caller the audit-log
// amplification the control-plane caps exist to prevent.
func TestLoginRoutes_AThrottledCeremonyIsNotRecorded(t *testing.T) {
	s := lrNewServer(t)

	throttled := 0
	for i := 0; i < 30; i++ {
		if resp, _ := s.register(loginfake.NewSoftAuthenticator(t), "guess", 1); resp.StatusCode == http.StatusTooManyRequests {
			throttled++
		}
	}
	if throttled == 0 {
		t.Fatal("nothing was throttled, so this test proves nothing")
	}

	limited := 0
	for _, d := range s.auditor.forPath(loginVerifyPath) {
		if strings.Contains(d.Reason, login.ErrWebAuthnRateLimited.Error()) {
			limited++
		}
	}
	if limited != 0 {
		t.Errorf("%d throttled refusals were recorded; %d requests were throttled", limited, throttled)
	}
}

// A decode failure quotes the body it choked on, and the body of a
// registration is where a guessed code lives.
func TestLoginAuditReason_CarriesNoCallerBytes(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"a wrapped decode failure": {
			err:  fmt.Errorf("%w: %w", errLoginBadRequest, errors.New(`json: cannot unmarshal, near "code":"SECRET-CODE"`)),
			want: errLoginBadRequest.Error(),
		},
		"a bootstrap refusal": {
			err:  errBootstrapCodeInvalid,
			want: errBootstrapCodeInvalid.Error(),
		},
		"a throttle": {
			err:  &login.WebAuthnRateLimitedError{RetryAfter: 30},
			want: login.ErrWebAuthnRateLimited.Error(),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := loginAuditReason(tc.err); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}

	// The counter refusal is the exception, and it is relay's own words about
	// its own stored values (ADR-016 decision 7, point 10).
	counter := &login.WebAuthnCounterError{CredentialID: []byte("abc"), Stored: 7, Received: 7}
	if got := loginAuditReason(counter); !strings.Contains(got, "stored 7") || !strings.Contains(got, "received 7") {
		t.Fatalf("counter reason = %q, want both counter values", got)
	}
	if loginAuditReason(nil) != "" {
		t.Fatal("a nil error produced a reason")
	}
}
