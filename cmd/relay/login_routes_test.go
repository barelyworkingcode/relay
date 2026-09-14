package main

// HTTP coverage for ADR-016 decision 5's public route table. Every ceremony
// here runs through the real mux over the real loopback listener, driven by
// the software authenticator in internal/login/loginfake — the negative
// verifier cases live in internal/login's webauthn_test.go and are
// deliberately not repeated.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/login"
	"github.com/barelyworkingcode/relay/internal/login/loginfake"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
)

type lrAuditor struct {
	mu        sync.Mutex
	decisions []control.ControlDecision
}

func (a *lrAuditor) RecordDecision(d control.ControlDecision) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.decisions = append(a.decisions, d)
}

func (a *lrAuditor) forPath(path string) []control.ControlDecision {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []control.ControlDecision
	for _, d := range a.decisions {
		if d.Path == path {
			out = append(out, d)
		}
	}
	return out
}

type lrServer struct {
	t       *testing.T
	store   config.SettingsStore
	dir     string
	base    string
	origin  string
	sock    string
	auditor *lrAuditor
}

// lrNewServer binds the real loopback listener beside the real socket, with
// the real credential authorizer: a test that mounted the login mux by hand
// would prove nothing about which door serves it.
func lrNewServer(t *testing.T) *lrServer {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	sock := filepath.Join(mkShortTempDir(t, "lr-"), "frontend.sock")
	auditor := &lrAuditor{}
	extMgr := mcpbroker.NewManager(nil)
	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		seededEndpoint(t, store, sock, "lr-frontend-token"),
		NewEnhancedServiceRegistry(nil),
		nil, nil, nil, nil, nil,
		&McpOps{Store: store, Ctx: context.Background()},
		nil,
		nil,
		nil,
		nil,
		NewCredentialAuthorizer(store), auditor, nil,
	)
	if err != nil {
		t.Fatalf("NewFrontendServer: %v", err)
	}
	if err := srv.ListenLoopback("127.0.0.1:0"); err != nil {
		t.Fatalf("ListenLoopback: %v", err)
	}
	go func() { _ = srv.Serve() }()
	go func() { _ = srv.ServeLoopback() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	origin := srv.LoginOrigin()
	port := origin[strings.LastIndex(origin, ":")+1:]
	return &lrServer{
		t:       t,
		store:   store,
		dir:     dir,
		base:    "http://127.0.0.1:" + port,
		origin:  origin,
		sock:    sock,
		auditor: auditor,
	}
}

func (s *lrServer) mintCode() string {
	s.t.Helper()
	code, _, err := mintLoginBootstrap(s.store)
	if err != nil {
		s.t.Fatalf("mintLoginBootstrap: %v", err)
	}
	return code
}

type lrChallenge struct {
	Challenge   string   `json:"challenge"`
	RPID        string   `json:"rp_id"`
	Origin      string   `json:"origin"`
	UserHandle  string   `json:"user_handle"`
	UserName    string   `json:"user_name"`
	Credentials []string `json:"credentials"`
}

func (s *lrServer) challenge(ceremony string) lrChallenge {
	s.t.Helper()
	resp, body := doJSON(s.t, "POST", s.base+"/relay/login/challenge", map[string]any{"ceremony": ceremony})
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("challenge %s: status %d, body %s", ceremony, resp.StatusCode, body)
	}
	var out lrChallenge
	if err := json.Unmarshal(body, &out); err != nil {
		s.t.Fatalf("decode challenge: %v (%s)", err, body)
	}
	return out
}

func lrB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func lrDecodeChallenge(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	return raw
}

// register drives one full registration ceremony and returns the raw
// response, so a caller can assert on a refusal as easily as on success.
func (s *lrServer) register(a *loginfake.SoftAuthenticator, code string, signCount uint32) (*http.Response, []byte) {
	s.t.Helper()
	ch := s.challenge("register")
	c := &loginfake.RegistrationCeremony{
		ClientData: loginfake.ClientDataOpts{
			Type:        "webauthn.create",
			Origin:      s.origin,
			Challenge:   lrDecodeChallenge(s.t, ch.Challenge),
			CrossOrigin: boolPtr(false),
		},
		AuthData: loginfake.AuthDataOpts{
			RPIDHash:  loginfake.RPIDHash(ch.RPID),
			Flags:     loginfake.FlagUserPresent | loginfake.FlagUserVerified | loginfake.FlagAttestedCredentialData,
			SignCount: signCount,
			Attested:  true,
			AAGUID:    a.AAGUID,
			CredID:    a.CredID,
			COSEKey:   a.CoseKey(),
		},
		Format:  "none",
		AttStmt: loginfake.CBOREncMap(),
	}
	in := c.Input()
	return doJSON(s.t, "POST", s.base+"/relay/login/verify", map[string]any{
		"ceremony":           "register",
		"code":               code,
		"client_data_json":   lrB64(in.ClientDataJSON),
		"attestation_object": lrB64(in.AttestationObject),
	})
}

func (s *lrServer) assert(a *loginfake.SoftAuthenticator, signCount uint32) (*http.Response, []byte) {
	s.t.Helper()
	return s.assertWith(a, signCount, loginOwnerHandle)
}

// assertWith takes the user handle explicitly because a real browser returns
// none for a non-discoverable credential, which is what residentKey
// "discouraged" asks for.
func (s *lrServer) assertWith(a *loginfake.SoftAuthenticator, signCount uint32, userHandle []byte) (*http.Response, []byte) {
	s.t.Helper()
	ch := s.challenge("assert")
	c := &loginfake.AssertionCeremony{
		ClientData: loginfake.ClientDataOpts{
			Type:        "webauthn.get",
			Origin:      s.origin,
			Challenge:   lrDecodeChallenge(s.t, ch.Challenge),
			CrossOrigin: boolPtr(false),
		},
		AuthData: loginfake.AuthDataOpts{
			RPIDHash:  loginfake.RPIDHash(ch.RPID),
			Flags:     loginfake.FlagUserPresent | loginfake.FlagUserVerified,
			SignCount: signCount,
		},
		CredentialID: a.CredID,
		UserHandle:   userHandle,
		SignWith:     a.Key,
	}
	in := c.Input(s.t)
	return doJSON(s.t, "POST", s.base+"/relay/login/verify", map[string]any{
		"ceremony":           "assert",
		"credential_id":      lrB64(in.CredentialID),
		"client_data_json":   lrB64(in.ClientDataJSON),
		"authenticator_data": lrB64(in.AuthenticatorData),
		"signature":          lrB64(in.Signature),
		"user_handle":        lrB64(in.UserHandle),
	})
}

// signIn runs the whole ceremony and returns the plaintext credential.
func (s *lrServer) signIn(a *loginfake.SoftAuthenticator, signCount uint32) string {
	s.t.Helper()
	resp, body := s.assert(a, signCount)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("assert: status %d, body %s", resp.StatusCode, body)
	}
	var out loginSignedInResponse
	if err := json.Unmarshal(body, &out); err != nil {
		s.t.Fatalf("decode signed-in response: %v (%s)", err, body)
	}
	if out.Token == "" {
		s.t.Fatalf("assertion returned no token: %s", body)
	}
	return out.Token
}

// enrolled registers one passkey against a freshly minted code and signs in.
func (s *lrServer) enrolled() (*loginfake.SoftAuthenticator, string) {
	s.t.Helper()
	a := loginfake.NewSoftAuthenticator(s.t)
	resp, body := s.register(a, s.mintCode(), 1)
	if resp.StatusCode != http.StatusCreated {
		s.t.Fatalf("register: status %d, body %s", resp.StatusCode, body)
	}
	return a, s.signIn(a, 2)
}

func (s *lrServer) loginCredential() config.APICredential {
	s.t.Helper()
	for _, c := range s.store.Get().APICredentials {
		if strings.HasPrefix(c.Name, "login ") {
			return c
		}
	}
	s.t.Fatalf("no login credential in %+v", s.store.Get().APICredentials)
	return config.APICredential{}
}

// socketDo reaches the 0600 socket door, which is where the execute- and
// proxy-class routes live at all.
func (s *lrServer) socketDo(method, path, token string) (*http.Response, []byte) {
	s.t.Helper()
	req, err := http.NewRequest(method, "http://unix"+path, nil)
	if err != nil {
		s.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := dialFrontendHTTP(s.sock).Do(req)
	if err != nil {
		s.t.Fatalf("%s %s over the socket: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("read body: %v", err)
	}
	return resp, body
}

// TestLoginRoutes_HappyPath is the whole ceremony end to end: a code minted
// on the host, a passkey registered with it, an assertion, and a credential
// that then authenticates a real classed route.
func TestLoginRoutes_HappyPath(t *testing.T) {
	s := lrNewServer(t)

	a := loginfake.NewSoftAuthenticator(t)
	resp, body := s.register(a, s.mintCode(), 1)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", resp.StatusCode, body)
	}
	if got := s.store.Get().Passkeys; len(got) != 1 {
		t.Fatalf("expected 1 registered passkey, got %+v", got)
	}
	if s.store.Get().LoginBootstrap != nil {
		t.Fatal("the bootstrap code survived the registration that consumed it")
	}

	token := s.signIn(a, 2)

	authed, body := doJSONAuth(t, "GET", s.base+"/api/projects", nil, token)
	if authed.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/projects with the minted credential: status %d, body %s", authed.StatusCode, body)
	}

	// A browser returns no user handle for a non-discoverable credential, so
	// the credential id lookup is the binding on that path (ADR-016 decision
	// 7, point 9).
	if resp, body := s.assertWith(a, 3, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("assertion with no user handle: status %d, body %s", resp.StatusCode, body)
	}
}

// The four ways a registration can lack a valid code must be one answer: an
// absent record, a wrong guess, an expired one and a spent one are the same
// refusal, byte for byte (ADR-016 decision 2).
func TestLoginRoutes_RegistrationWithoutAValidCodeIsRefusedIdentically(t *testing.T) {
	s := lrNewServer(t)

	type refusal struct {
		name   string
		status int
		body   string
	}
	var refusals []refusal
	record := func(name string, resp *http.Response, body []byte) {
		refusals = append(refusals, refusal{name, resp.StatusCode, string(body)})
		if resp.StatusCode == http.StatusCreated {
			t.Fatalf("%s: registration was accepted", name)
		}
		if got := s.store.Get().Passkeys; len(got) != 0 {
			t.Fatalf("%s: a refused registration persisted a passkey: %+v", name, got)
		}
	}

	a := loginfake.NewSoftAuthenticator(t)
	resp, body := s.register(a, "", 1)
	record("no code", resp, body)

	resp, body = s.register(a, "not-the-code", 1)
	record("wrong code", resp, body)

	s.mintCode()
	expired := s.store.Get().LoginBootstrap
	if err := s.store.With(func(st *config.Settings) {
		st.LoginBootstrap = &config.LoginBootstrap{
			Hash:    expired.Hash,
			Expires: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		}
	}); err != nil {
		t.Fatalf("age the bootstrap record: %v", err)
	}
	// The plaintext behind that hash is unknown to this test on purpose: a
	// wrong guess against an expired record and a right one must land in the
	// same place, so the guess is not what is being varied.
	resp, body = s.register(a, "any-guess", 1)
	record("expired code", resp, body)

	code := s.mintCode()
	if resp, body := s.register(a, code, 1); resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup registration: status %d, body %s", resp.StatusCode, body)
	}
	second := loginfake.NewSoftAuthenticator(t)
	resp, body = s.register(second, code, 1)
	refusals = append(refusals, refusal{"spent code", resp.StatusCode, string(body)})
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("spent code: a code was accepted twice")
	}
	if got := s.store.Get().Passkeys; len(got) != 1 {
		t.Fatalf("spent code: a refused registration changed the passkey list: %+v", got)
	}

	for _, r := range refusals[1:] {
		if r.status != refusals[0].status || r.body != refusals[0].body {
			t.Fatalf("%q answers %d %q, but %q answers %d %q — the two are distinguishable",
				r.name, r.status, r.body, refusals[0].name, refusals[0].status, refusals[0].body)
		}
	}
	if refusals[0].status != http.StatusForbidden {
		t.Fatalf("a refused registration answered %d, want 403", refusals[0].status)
	}
}

// A code registers a passkey and does nothing else. Presented as a login it
// must not authenticate anything, and must not even be spent.
func TestLoginRoutes_BootstrapCodeCannotSignIn(t *testing.T) {
	s := lrNewServer(t)
	a := loginfake.NewSoftAuthenticator(t)
	if resp, body := s.register(a, s.mintCode(), 1); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", resp.StatusCode, body)
	}

	code := s.mintCode()
	resp, body := doJSON(t, "POST", s.base+"/relay/login/verify", map[string]any{
		"ceremony": "assert",
		"code":     code,
	})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a bootstrap code was accepted in place of an assertion: %s", body)
	}
	if bytes.Contains(body, []byte("token")) {
		t.Fatalf("a refused sign-in returned something token-shaped: %s", body)
	}
	for _, c := range s.store.Get().APICredentials {
		if strings.HasPrefix(c.Name, "login ") {
			t.Fatalf("a bootstrap code minted a credential: %+v", c)
		}
	}
	if s.store.Get().LoginBootstrap == nil {
		t.Fatal("a refused sign-in consumed the registration anchor")
	}
}

func TestLoginRoutes_FifthPasskeyIsAcceptedAndSixthIsRefused(t *testing.T) {
	s := lrNewServer(t)

	for i := 0; i < login.MaxRegisteredPasskeys; i++ {
		a := loginfake.NewSoftAuthenticator(t)
		resp, body := s.register(a, s.mintCode(), 1)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("passkey %d: status %d, body %s", i+1, resp.StatusCode, body)
		}
	}
	if got := len(s.store.Get().Passkeys); got != login.MaxRegisteredPasskeys {
		t.Fatalf("registered %d passkeys, want %d", got, login.MaxRegisteredPasskeys)
	}

	code := s.mintCode()
	resp, body := s.register(loginfake.NewSoftAuthenticator(t), code, 1)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sixth passkey: status %d, want 403, body %s", resp.StatusCode, body)
	}
	if got := len(s.store.Get().Passkeys); got != login.MaxRegisteredPasskeys {
		t.Fatalf("a refused registration changed the passkey count to %d", got)
	}
	if s.store.Get().LoginBootstrap == nil {
		t.Fatal("a registration refused by the cap still spent the operator's code")
	}
}

// An expired login credential is refused exactly as an unknown one is — same
// status, same body — because a distinguishable answer is an oracle for which
// credentials exist (ADR-016 decision 3).
func TestLoginRoutes_MintedCredentialExpiresAndIsRefusedLikeAnUnknownOne(t *testing.T) {
	s := lrNewServer(t)
	_, token := s.enrolled()

	if resp, body := doJSONAuth(t, "GET", s.base+"/api/projects", nil, token); resp.StatusCode != http.StatusOK {
		t.Fatalf("before expiry: status %d, body %s", resp.StatusCode, body)
	}

	cred := s.loginCredential()
	if cred.Expires == "" {
		t.Fatal("the login credential was minted with no expiry")
	}
	if err := s.store.With(func(st *config.Settings) {
		findAPICredential(st, cred.ID).Expires = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	}); err != nil {
		t.Fatalf("age the credential: %v", err)
	}

	expiredResp, expiredBody := doJSONAuth(t, "GET", s.base+"/api/projects", nil, token)
	unknownResp, unknownBody := doJSONAuth(t, "GET", s.base+"/api/projects", nil, "no-such-credential")
	if expiredResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired credential: status %d, want 401, body %s", expiredResp.StatusCode, expiredBody)
	}
	if expiredResp.StatusCode != unknownResp.StatusCode || !bytes.Equal(expiredBody, unknownBody) {
		t.Fatalf("expired answers %d %q, unknown answers %d %q — the two are distinguishable",
			expiredResp.StatusCode, expiredBody, unknownResp.StatusCode, unknownBody)
	}
}

// The class set is a ceiling ADR-016 decision 3 fixes. The negative half is
// the load-bearing half: grant, execute and proxy must be out of reach, and
// the last two live on the socket, so they are asked for there.
func TestLoginRoutes_MintedCredentialHoldsOnlyReadAndConfigure(t *testing.T) {
	s := lrNewServer(t)
	_, token := s.enrolled()

	cred := s.loginCredential()
	want := []control.CapabilityClass{control.ClassRead, control.ClassConfigure}
	if !slices.Equal(cred.Classes, want) {
		t.Fatalf("classes = %v, want exactly %v", cred.Classes, want)
	}
	for _, class := range []control.CapabilityClass{control.ClassGrant, control.ClassExecute, control.ClassProxy} {
		if cred.Grants(class) {
			t.Fatalf("the login credential holds %s", class)
		}
	}

	if resp, body := doJSONAuth(t, "POST", s.base+"/api/projects/anything/rotate_token", nil, token); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("grant-class route: status %d, want 403, body %s", resp.StatusCode, body)
	}
	if resp, body := s.socketDo("POST", "/api/mcps", token); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("execute-class route on the socket: status %d, want 403, body %s", resp.StatusCode, body)
	}
	if resp, body := s.socketDo("GET", "/some/proxied/path", token); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("proxy-class catch-all on the socket: status %d, want 403, body %s", resp.StatusCode, body)
	}
	// The same catch-all with a credential that does hold proxy reaches the
	// dispatcher instead, so the 403 above is the class check and not the
	// route simply being absent.
	if resp, body := s.socketDo("GET", "/some/proxied/path", "lr-frontend-token"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("proxy-class catch-all with the legacy credential: status %d, want 404, body %s", resp.StatusCode, body)
	}
}

// A stale counter is a cloned-authenticator signal: refuse the assertion,
// write the record, and leave the passkey working (ADR-016 decision 7,
// point 10).
func TestLoginRoutes_StaleCounterIsRefusedAuditedAndLeavesThePasskeyUsable(t *testing.T) {
	s := lrNewServer(t)
	a := loginfake.NewSoftAuthenticator(t)
	if resp, body := s.register(a, s.mintCode(), 1); resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", resp.StatusCode, body)
	}
	s.signIn(a, 2)

	resp, body := s.assert(a, 2)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("replayed counter: status %d, want 403, body %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), login.ErrWebAuthnAssertionRejected.Error()) {
		t.Fatalf("a counter refusal must answer in the words of a rejected assertion: %s", body)
	}

	credID := lrB64(a.CredID)
	var recorded *control.ControlDecision
	for _, d := range s.auditor.forPath("/relay/login/verify") {
		if !d.Allowed && strings.Contains(d.Reason, credID) {
			recorded = &d
			break
		}
	}
	if recorded == nil {
		t.Fatalf("no audit record names the credential: %+v", s.auditor.forPath("/relay/login/verify"))
	}
	if !strings.Contains(recorded.Reason, "stored 2") || !strings.Contains(recorded.Reason, "received 2") {
		t.Fatalf("audit record does not carry both counter values: %q", recorded.Reason)
	}

	stored := s.store.Get().Passkeys
	if len(stored) != 1 {
		t.Fatalf("the refused assertion changed the passkey list: %+v", stored)
	}
	if stored[0].SignCount != 2 {
		t.Fatalf("sign count = %d, want the last accepted value 2", stored[0].SignCount)
	}
	// The passkey is refused, never disabled: one replayed stale assertion
	// must not lock the owner out of their own machine.
	s.signIn(a, 3)
	if got := s.store.Get().Passkeys[0].SignCount; got != 3 {
		t.Fatalf("sign count = %d after the next good assertion, want 3", got)
	}
}

// lrPublicPatterns is the pin. A fourth unauthenticated route has to be added
// here on purpose, which is the whole point of keeping the table in one
// place (ADR-016 decision 5).
var lrPublicPatterns = []string{
	"GET /relay/login",
	"POST /relay/login/challenge",
	"POST /relay/login/verify",
}

func TestLoginRoutes_PublicMuxCarriesExactlyTheEnumeratedPatterns(t *testing.T) {
	verifier, err := login.NewWebAuthnVerifier("http://localhost:1", webauthnRPID)
	if err != nil {
		t.Fatalf("NewWebAuthnVerifier: %v", err)
	}
	var got []string
	for pattern := range newLoginRoutes(nil, verifier, nil).loginHandlers() {
		got = append(got, pattern)
	}
	sort.Strings(got)
	want := append([]string(nil), lrPublicPatterns...)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the unauthenticated route table is %v, want exactly %v", got, want)
	}
}

// Everything the table does not name still needs a credential, including the
// same paths under a different method and every neighbour of /relay/login.
func TestLoginRoutes_EveryOtherPathStillRequiresACredential(t *testing.T) {
	s := lrNewServer(t)

	for _, pattern := range lrPublicPatterns {
		method, path, _ := strings.Cut(pattern, " ")
		resp, body := doJSON(t, method, s.base+path, map[string]any{"ceremony": "assert"})
		if resp.StatusCode == http.StatusUnauthorized {
			t.Fatalf("%s is in the public table but answered 401: %s", pattern, body)
		}
	}

	gated := []string{
		"GET /relay/",
		"GET /relay/login/",
		"GET /relay/login/challenge",
		"GET /relay/login/verify",
		"POST /relay/login",
		"GET /relay/login/document",
		"GET /relay/logout",
		"GET /api/projects",
		"GET /",
	}
	for _, pattern := range gated {
		method, path, _ := strings.Cut(pattern, " ")
		resp, body := doJSON(t, method, s.base+path, map[string]any{"ceremony": "assert"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s answered %d without a credential, want 401: %s", pattern, resp.StatusCode, body)
		}
	}
}

// With no TCP listener there is no origin, so there are no login routes at
// all rather than login routes verifying against a placeholder. On the
// socket the paths are behind the credential gate like any other.
func TestLoginRoutes_AbsentWithoutALoopbackListener(t *testing.T) {
	s := lrNewServer(t)

	if resp, body := s.socketDo("GET", "/relay/login", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("socket /relay/login without a credential: status %d, want 401, body %s", resp.StatusCode, body)
	}
	// Authenticated, the socket has no login document to serve: the
	// dispatcher's catch-all owns the path and knows no service for it.
	if resp, body := s.socketDo("GET", "/relay/login", "lr-frontend-token"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("socket /relay/login with a credential: status %d, want 404, body %s", resp.StatusCode, body)
	}
}

func TestLoginRoutes_DocumentIsSelfContainedUnderAStrictCSP(t *testing.T) {
	s := lrNewServer(t)

	resp, body := doJSON(t, "GET", s.base+"/relay/login", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /relay/login: status %d, body %s", resp.StatusCode, body)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP %q does not carry %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP falls back to unsafe-inline: %q", csp)
	}
	doc := string(body)
	if strings.Contains(doc, loginNoncePlaceholder) {
		t.Fatal("the document was served with its nonce placeholder unreplaced")
	}
	for _, forbidden := range []string{"http://", "https://", "//cdn", "<link"} {
		if strings.Contains(doc, forbidden) {
			t.Fatalf("the login document reaches outside itself: %q", forbidden)
		}
	}
	for _, want := range []string{`alg: -7`, `userVerification: "required"`, `residentKey: "discouraged"`, "allowCredentials"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("the login document does not pin %q", want)
		}
	}
	if strings.Contains(doc, "localStorage") || strings.Contains(doc, "document.cookie") {
		t.Fatal("the login document persists the credential")
	}
}

// The plaintext is returned once. Anywhere else — another response, a log
// line, settings.json — is a leak of a credential relay cannot re-issue.
func TestLoginRoutes_MintedPlaintextAppearsOnlyInTheOneResponse(t *testing.T) {
	s := lrNewServer(t)

	logs := &lrSyncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	a := loginfake.NewSoftAuthenticator(t)
	registerResp, registerBody := s.register(a, s.mintCode(), 1)
	if registerResp.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", registerResp.StatusCode, registerBody)
	}
	token := s.signIn(a, 2)

	_, listBody := doJSONAuth(t, "GET", s.base+"/api/projects", nil, token)
	_, challengeBody := doJSON(t, "POST", s.base+"/relay/login/challenge", map[string]any{"ceremony": "assert"})
	_, documentBody := doJSON(t, "GET", s.base+"/relay/login", nil)

	others := map[string][]byte{
		"the registration response": registerBody,
		"a project listing":         listBody,
		"a challenge":               challengeBody,
		"the login document":        documentBody,
		"the log":                   []byte(logs.String()),
	}
	for name, body := range others {
		if bytes.Contains(body, []byte(token)) {
			t.Fatalf("the minted plaintext appears in %s", name)
		}
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if bytes.Contains(raw, []byte(token)) {
		t.Fatal("the minted plaintext is present in settings.json")
	}
}

type lrSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lrSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lrSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// /relay/ is relay's own path space. A manifest claiming it would put a
// service in front of the one door relay serves with no credential.
func TestEnhancedServices_ManifestCannotClaimTheRelayPrefix(t *testing.T) {
	reg := NewEnhancedServiceRegistry(nil)

	for _, route := range []string{"/relay/", "/relay/login", "/relay/login/verify", "/relay"} {
		err := reg.RegisterManifest("squatter", "/tmp/squatter.sock", "tok", newManifest(route))
		if err == nil {
			t.Fatalf("a manifest claiming %q was accepted", route)
		}
		if !strings.Contains(err.Error(), route) {
			t.Fatalf("the refusal for %q does not name the route: %v", route, err)
		}
		if reg.Get("squatter") != nil {
			t.Fatalf("a refused manifest claiming %q was registered anyway", route)
		}
	}

	if err := reg.RegisterManifest("neighbour", "/tmp/neighbour.sock", "tok", newManifest("/relayllm/", "/api/tasks/")); err != nil {
		t.Fatalf("a route merely starting with the same letters was refused: %v", err)
	}
}

// TestLoginRoutes_SlowBodyIsCutOff drives a raw connection that sends
// headers declaring a body, then a few bytes of it, and then nothing else —
// the shape of a caller trickling a request to hold a handler goroutine
// open. decodeLoginBody's read deadline must cut the read off and produce a
// refusal well before the connection would otherwise idle out.
func TestLoginRoutes_SlowBodyIsCutOff(t *testing.T) {
	old := loginBodyReadDeadline
	loginBodyReadDeadline = 300 * time.Millisecond
	t.Cleanup(func() { loginBodyReadDeadline = old })

	s := lrNewServer(t)
	addr := strings.TrimPrefix(s.base, "http://")

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	full := `{"ceremony":"register"}`
	partial := full[:5] // well short of Content-Length, and never completed

	req := fmt.Sprintf(
		"POST /relay/login/challenge HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		addr, len(full), partial,
	)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	// Generous relative to the shortened deadline, tight relative to the
	// server's real ReadHeaderTimeout/IdleTimeout (30s/5m) — if those fired
	// instead of the read deadline this reads as a hang, not a fast 400.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("read response: %v (after %s)", err, elapsed)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (malformed body from the cut-off read)", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Errorf("response took %s, want well under 2s given a %s read deadline", elapsed, loginBodyReadDeadline)
	}
}
