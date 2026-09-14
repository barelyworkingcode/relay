package main

// The composition nothing else in the suite exercises: NewFrontendServer's
// OUTER gate and control.RouteRegistrar's per-route class check running together,
// against real credentials in a real store. The route-level tests
// (capability_test.go, credential_enforcement_test.go) drive a synthetic mux
// with no outer gate at all, and the server-level tests
// (frontend_server_test.go, transport_enforcement_test.go) pass a nil
// control.Authorizer — so a gate that admitted exactly one token while the class
// check expected many could be, and was, green in both.
//
// Also covers the `relay credential` CLI that mints those credentials, since
// a class model with no way to mint is a class model with one credential.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
)

// accLegacyToken is the bearer accNewServer seeds as a read+configure+proxy
// credential.
const accLegacyToken = "acc-bearer"

type accServer struct {
	store    config.SettingsStore
	sockHTTP *http.Client
	tcpBase  string
}

// accNewServer wires the REAL composed stack — real ServiceOps/EnrolmentOps/
// audit.AuditOps/McpOps, a real 0600 socket, a real loopback TCP listener, and a
// real credentialAuthorizer over the same store. bearer is seeded as a
// read+configure+proxy credential; "" seeds none, which is the fail-closed case.
func accNewServer(t *testing.T, store config.SettingsStore, bearer string) *accServer {
	t.Helper()

	ops := &ServiceOps{Store: store, Registry: &svcRecorder{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	enrolOps := &EnrolmentOps{Store: store, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}
	mcpOps := &McpOps{Store: store, Ctx: context.Background(), Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	projOps := &ProjectOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	extMgr := mcpbroker.NewManager(nil)

	dir := mkShortTempDir(t, "acc-fe-")
	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		seededEndpoint(t, store, filepath.Join(dir, "frontend.sock"), bearer),
		NewEnhancedServiceRegistry(nil), nil, nil,
		ops, enrolOps, &audit.AuditOps{}, mcpOps, projOps, nil, nil, nil,
		NewCredentialAuthorizer(store), nil, nil,
	)
	assertNoErr(t, err, "NewFrontendServer")
	go func() { _ = srv.Serve() }()
	assertNoErr(t, srv.ListenLoopback("127.0.0.1:0"), "ListenLoopback")
	go func() { _ = srv.ServeLoopback() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	_ = dialUnixWithTimeout(t, srv.socketPath, 2*time.Second).Close()

	return &accServer{
		store:    store,
		sockHTTP: dialFrontendHTTP(srv.socketPath),
		tcpBase:  "http://" + srv.tcpLn.Addr().String(),
	}
}

func accDo(t *testing.T, client *http.Client, method, url, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		assertNoErr(t, err, "marshal body")
		r = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, url, r)
	assertNoErr(t, err, "new request")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	assertNoErr(t, err, "%s %s", method, url)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	assertNoErr(t, err, "read body")
	return resp, raw
}

func (a *accServer) socket(t *testing.T, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	return accDo(t, a.sockHTTP, method, "http://unix"+path, token, body)
}

func (a *accServer) tcp(t *testing.T, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	return accDo(t, http.DefaultClient, method, a.tcpBase+path, token, body)
}

// accAssertReached checks the request got past BOTH gates. It cannot assert a
// specific success status because the routes under test answer 200, 201 and
// 204; what it asserts is the only thing authorization decides.
func accAssertReached(t *testing.T, resp *http.Response, body []byte, what string) {
	t.Helper()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("%s: status = %d, want the request to reach its handler; body=%s", what, resp.StatusCode, body)
	}
}

func accAssertForbidden(t *testing.T, resp *http.Response, body []byte, what string) {
	t.Helper()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("%s: status = %d, want 403; body=%s", what, resp.StatusCode, body)
	}
}

func accMint(t *testing.T, store config.SettingsStore, name string, classes ...string) string {
	t.Helper()
	_, plaintext, err := mintAPICredential(store, credentialMintRequest{Name: name, Classes: classes})
	assertNoErr(t, err, "mint %q", name)
	return plaintext
}

// ---------------------------------------------------------------------------
// The composed stack
// ---------------------------------------------------------------------------

// TestACCMintedCredentialAuthenticatesAndReachesOnlyItsClasses mints AFTER
// the server is already serving, which is the CLI's real situation: the
// credential is written by another process and must authenticate on the very
// next request, with no restart and no poll interval in between.
func TestACCMintedCredentialAuthenticatesAndReachesOnlyItsClasses(t *testing.T) {
	store := newCLISandboxStore(t)
	srv := accNewServer(t, store, accLegacyToken)
	proj := mkStoreProject(t, store, config.ProjectKindLocal, "acc-proj", t.TempDir())

	token := accMint(t, store, "acc-reader", "read")

	resp, body := srv.socket(t, "GET", "/api/services", token, nil)
	accAssertReached(t, resp, body, "read credential on GET /api/services")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read credential on GET /api/services: status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	resp, body = srv.socket(t, "POST", "/api/projects", token, map[string]any{"name": "acc-new", "path": t.TempDir()})
	accAssertForbidden(t, resp, body, "read credential on configure-class POST /api/projects")

	resp, body = srv.socket(t, "POST", "/api/projects/"+proj.ID+"/rotate_token", token, nil)
	accAssertForbidden(t, resp, body, "read credential on grant-class rotate_token")

	resp, body = srv.socket(t, "POST", "/api/services", token, map[string]any{"display_name": "acc-phantom", "command": "/bin/true"})
	accAssertForbidden(t, resp, body, "read credential on execute-class POST /api/services")

	if svc, _ := config.FindServiceByID(store.Get(), "acc-phantom"); svc != nil {
		t.Fatal("a read-only credential reached ServiceOps.Create")
	}
	if len(store.Get().Projects) != 1 {
		t.Fatalf("project count = %d, want 1; a read-only credential created one", len(store.Get().Projects))
	}
}

func TestACCUnknownAndAbsentBearersAreRefusedIdentically(t *testing.T) {
	store := newCLISandboxStore(t)
	srv := accNewServer(t, store, accLegacyToken)

	cases := []struct{ name, token string }{
		{"absent", ""},
		{"unknown", "acc-not-a-credential"},
		{"empty bearer", " "},
	}
	var bodies []string
	for _, tc := range cases {
		resp, body := srv.socket(t, "GET", "/api/services", tc.token, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s bearer: status = %d, want 401; body=%s", tc.name, resp.StatusCode, body)
		}
		bodies = append(bodies, string(body))
	}
	for i := range bodies {
		if bodies[i] != bodies[0] {
			t.Fatalf("refusal bodies differ (%q vs %q); the 401 must not distinguish absent from unknown", bodies[0], bodies[i])
		}
	}
}

func TestACCGrantCredentialReachesRotateTokenAndEnrolments(t *testing.T) {
	store := newCLISandboxStore(t)
	srv := accNewServer(t, store, accLegacyToken)
	proj := mkStoreProject(t, store, config.ProjectKindLocal, "acc-proj", t.TempDir())

	token := accMint(t, store, "acc-granter", "grant")

	resp, body := srv.socket(t, "POST", "/api/projects/"+proj.ID+"/rotate_token", token, nil)
	accAssertReached(t, resp, body, "grant credential on rotate_token")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate_token: status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if rotated, _ := config.FindProjectByID(store.Get(), proj.ID); rotated == nil || rotated.TokenHash == proj.TokenHash {
		t.Fatal("rotate_token answered but the project's token hash did not move")
	}

	resp, body = srv.socket(t, "POST", "/api/enrolments", token, map[string]any{"client_id": "acc-enr"})
	accAssertReached(t, resp, body, "grant credential on POST /api/enrolments")

	// A grant credential is not a superset: it holds exactly what it names.
	resp, body = srv.socket(t, "GET", "/api/projects", token, nil)
	accAssertForbidden(t, resp, body, "grant credential on read-class GET /api/projects")
}

// TestACCExecuteCredentialIsSocketOnly holds ADR-015 decisions 2 and 3
// together. The credential carries configure and proxy alongside execute
// deliberately: with the widest class set relay will hand anything, the
// caller clears every gate on the TCP mux, so what answers on TCP can only be
// the mux itself and not an authorization refusal wearing a routing costume.
//
// That answer is a 405, not a 404, and the difference is the point: GET
// /api/services IS registered on TCP, and the "/" catch-all is socket-only
// (ADR-016 decision 4), so nothing there absorbs the POST. The Allow header
// is http.ServeMux's signature and names only what TCP actually serves.
func TestACCExecuteCredentialIsSocketOnly(t *testing.T) {
	store := newCLISandboxStore(t)
	srv := accNewServer(t, store, accLegacyToken)

	token := accMint(t, store, "acc-executor", "execute", "configure", "proxy")

	resp, body := srv.socket(t, "POST", "/api/services", token,
		map[string]any{"display_name": "acc-worker", "command": "/bin/true"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("execute credential on the socket: status = %d, want 201; body=%s", resp.StatusCode, body)
	}

	resp, body = srv.tcp(t, "POST", "/api/services", token,
		map[string]any{"display_name": "acc-phantom", "command": "/bin/true"})
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("execute credential on TCP: status = %d, want 405 from http.ServeMux; body=%s", resp.StatusCode, body)
	}
	if allow := resp.Header.Get("Allow"); allow == "" || strings.Contains(allow, "POST") {
		t.Fatalf("Allow = %q; want http.ServeMux's own 405 naming only the methods TCP serves", allow)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q says a handler answered; the route must be absent from the TCP mux", resp.Header.Get("Content-Type"))
	}
	if svc, _ := config.FindServiceByID(store.Get(), "acc-phantom"); svc != nil {
		t.Fatal("POST /api/services reached ServiceOps.Create over TCP")
	}
}

// TestACCReadOnlyCredentialCannotReachTheProxiedSurface is the widening this
// work had to avoid: once the outer gate admits more than one credential, an
// unclassed catch-all would hand every proxied service route to a read-only
// caller. The legacy credential's socket answer is the control — it proves
// the path really does route to the dispatcher, so the read-only 403 beside
// it is a refusal and not a missing mount.
//
// Over TCP the mount is gone for EVERYONE (ADR-016 decision 4), so the same
// two credentials must both get a 404 there — and the legacy credential's is
// the one that matters, since it holds proxy and is refused by routing alone.
func TestACCReadOnlyCredentialCannotReachTheProxiedSurface(t *testing.T) {
	store := newCLISandboxStore(t)
	srv := accNewServer(t, store, accLegacyToken)

	readOnly := accMint(t, store, "acc-reader", "read")

	const dispatcherAnswer = "no service registered for this path"
	for _, path := range []string{"/api/sessions", "/api/terminals/1/input", "/ws"} {
		t.Run(path, func(t *testing.T) {
			resp, body := srv.socket(t, "POST", path, readOnly, map[string]any{})
			accAssertForbidden(t, resp, body, "read-only credential on the proxied catch-all")

			resp, body = srv.socket(t, "POST", path, accLegacyToken, map[string]any{})
			accAssertReached(t, resp, body, "legacy credential on the proxied catch-all")
			if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), dispatcherAnswer) {
				t.Fatalf("legacy credential on %s: status = %d body=%s; want the dispatcher's own 404 (no service registered)", path, resp.StatusCode, body)
			}

			for _, tc := range []struct{ name, token string }{
				{"read-only", readOnly},
				{"legacy (holds proxy)", accLegacyToken},
			} {
				resp, body = srv.tcp(t, "POST", path, tc.token, map[string]any{})
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("%s credential on %s over TCP: status = %d, want 404; body=%s", tc.name, path, resp.StatusCode, body)
				}
				if strings.Contains(string(body), dispatcherAnswer) {
					t.Fatalf("%s credential on %s over TCP reached the dispatcher: body=%s", tc.name, path, body)
				}
			}
		})
	}
}

// TestACCNoCredentialsAtAllRejectsEverything is the replacement for the empty
// configured token: the same misconfiguration, one layer down. Serving open
// here would expose every proxied service.
func TestACCNoCredentialsAtAllRejectsEverything(t *testing.T) {
	store := newCLISandboxStore(t)
	srv := accNewServer(t, store, "")

	if creds := store.Get().APICredentials; len(creds) != 0 {
		t.Fatalf("fixture is not the case under test: %d credentials present", len(creds))
	}

	for _, path := range []string{"/api/services", "/api/projects", "/api/audit", "/anything"} {
		resp, body := srv.socket(t, "GET", path, "acc-any-token", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s with no credentials configured: status = %d, want 401; body=%s", path, resp.StatusCode, body)
		}
		resp, body = srv.tcp(t, "GET", path, "acc-any-token", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s over TCP with no credentials configured: status = %d, want 401; body=%s", path, resp.StatusCode, body)
		}
	}
}

// TestACCCredentialMintedByASeparateProcessAuthenticatesImmediately is the
// cross-process claim, and the only test here that can make it: two
// FileSettingsStore values over one config dir have INDEPENDENT caches, so
// the server's store cannot learn about the CLI's write except by re-reading
// the file. Minting through the server's own store would prove nothing --
// store.With writes that cache through. This is issue #21's window, on the
// control plane.
func TestACCCredentialMintedByASeparateProcessAuthenticatesImmediately(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)

	trayStore := sealedSettingsStoreAt(dir)
	assertNoErr(t, trayStore.EnsureInitialized(), "EnsureInitialized")
	srv := accNewServer(t, trayStore, accLegacyToken)

	// A second store over the same dir stands in for a second process
	// minting a credential — sealed, like the tray, since a CLI-shaped
	// store (no sealer) now refuses every write by design (§5.4); brokering
	// `relay credential mint` itself over admin_op is a later step.
	cliStore := sealedSettingsStoreAt(dir)
	assertNoErr(t, cliStore.EnsureInitialized(), "EnsureInitialized (cli)")
	cred, plaintext, err := mintAPICredential(cliStore, credentialMintRequest{Name: "acc-cli", Classes: []string{"read"}})
	assertNoErr(t, err, "mint from the CLI process")

	resp, body := srv.socket(t, "GET", "/api/services", plaintext, nil)
	accAssertReached(t, resp, body, "credential minted by another process, on the very next request")

	// Revocation closes just as fast, and from the same direction.
	_, err = revokeAPICredential(cliStore, cred.ID)
	assertNoErr(t, err, "revoke from the CLI process")

	resp, body = srv.socket(t, "GET", "/api/services", plaintext, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after a cross-process revoke: status = %d, want 401; body=%s", resp.StatusCode, body)
	}
}

func TestACCRevokedCredentialStopsAuthenticatingOnTheNextRequest(t *testing.T) {
	store := newCLISandboxStore(t)
	srv := accNewServer(t, store, accLegacyToken)

	cred, plaintext, err := mintAPICredential(store, credentialMintRequest{Name: "acc-temp", Classes: []string{"read"}})
	assertNoErr(t, err, "mint")

	resp, body := srv.socket(t, "GET", "/api/services", plaintext, nil)
	accAssertReached(t, resp, body, "minted credential before revocation")

	_, err = revokeAPICredential(store, cred.ID)
	assertNoErr(t, err, "revoke")

	resp, body = srv.socket(t, "GET", "/api/services", plaintext, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after revocation: status = %d, want 401; body=%s", resp.StatusCode, body)
	}
}

// ---------------------------------------------------------------------------
// The CLI
// ---------------------------------------------------------------------------

func TestACCMintRefusesAnUnknownClass(t *testing.T) {
	store := newCLISandboxStore(t)

	for _, bad := range []string{"admin", "Read", "read,configure", "", "exec"} {
		t.Run(bad, func(t *testing.T) {
			_, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-bad", Classes: []string{bad}})
			if err == nil {
				t.Fatalf("class %q was accepted; a class Grants can never match must be refused at the point of entry", bad)
			}
			for _, want := range []string{"read", "configure", "grant", "execute"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name the valid class %q", err, want)
				}
			}
			if len(store.Get().APICredentials) != 0 {
				t.Fatal("a refused mint still wrote a credential")
			}
		})
	}
}

// Surrounding whitespace is normalized rather than refused: a stray space
// from a shell quoting slip would otherwise mint a credential carrying a
// class Grants can never match, which is the inert-credential failure this
// validation exists to prevent, not an example of it.
func TestACCMintNormalizesSurroundingWhitespaceInAClass(t *testing.T) {
	store := newCLISandboxStore(t)
	cred, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-spacey", Classes: []string{" execute "}})
	assertNoErr(t, err, "mint")
	if !cred.Grants(control.ClassExecute) {
		t.Fatalf("classes = %v; the credential is inert despite being accepted", cred.Classes)
	}
}

func TestACCMintRefusesAnEmptyClassSet(t *testing.T) {
	store := newCLISandboxStore(t)

	for _, classes := range [][]string{nil, {}} {
		_, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-inert", Classes: classes})
		if err == nil {
			t.Fatal("an empty class set was accepted; the credential would be inert with nothing to show why")
		}
		if len(store.Get().APICredentials) != 0 {
			t.Fatal("a refused mint still wrote a credential")
		}
	}
}

func TestACCMintRefusesAnEmptyName(t *testing.T) {
	store := newCLISandboxStore(t)
	if _, _, err := mintAPICredential(store, credentialMintRequest{Name: "  ", Classes: []string{"read"}}); err == nil {
		t.Fatal("an unnamed credential was accepted; `credential list` would have nothing to identify it by")
	}
}

func TestACCMintAndRevokeRefuseTheReservedLegacyName(t *testing.T) {
	store := newCLISandboxStore(t)

	_, _, err := mintAPICredential(store, credentialMintRequest{Name: legacyFrontendCredentialName, Classes: []string{"read", "execute"}})
	if !errors.Is(err, errReservedCredentialName) {
		t.Fatalf("mint under the reserved name: err = %v, want it refused — relay deletes that record on every start", err)
	}
	if len(store.Get().APICredentials) != 0 {
		t.Fatal("a refused mint still wrote a credential")
	}

	assertNoErr(t, store.With(func(s *config.Settings) {
		addAPICredential(s, config.APICredential{ID: "acc-legacy-id", Name: legacyFrontendCredentialName, Hash: config.HashToken(accLegacyToken), Classes: frontendConsumerClasses})
	}), "seed the legacy credential")
	legacyID := store.Get().APICredentials[0].ID

	_, err = revokeAPICredential(store, legacyID)
	if !errors.Is(err, errReservedCredentialName) {
		t.Fatalf("revoke of the legacy credential: err = %v, want it refused", err)
	}
	if len(store.Get().APICredentials) != 1 {
		t.Fatal("the legacy credential was revoked despite the refusal")
	}
}

func TestACCMintDeduplicatesClassesAndPreservesOrder(t *testing.T) {
	store := newCLISandboxStore(t)
	cred, _, err := mintAPICredential(store, credentialMintRequest{
		Name:    "acc-dupes",
		Classes: []string{"configure", "read", "configure"},
	})
	assertNoErr(t, err, "mint")
	if got := formatClasses(cred.Classes); got != "configure,read" {
		t.Fatalf("classes = %q, want %q", got, "configure,read")
	}
}

func TestACCRevokeUnknownIDIsAnError(t *testing.T) {
	store := newCLISandboxStore(t)
	if _, err := revokeAPICredential(store, "acc-no-such-id"); err == nil {
		t.Fatal("revoking an id that does not exist reported success")
	}
	if _, err := revokeAPICredential(store, ""); err == nil {
		t.Fatal("revoking an empty id reported success")
	}
}

// TestACCMintReturnsAPlaintextThatIsNeverStored pins the one-shot property
// the CLI's output promises: only the SHA-256 lands in settings.json, so an
// operator who loses the printed token cannot recover it from the record.
func TestACCMintReturnsAPlaintextThatIsNeverStored(t *testing.T) {
	store := newCLISandboxStore(t)
	cred, plaintext, err := mintAPICredential(store, credentialMintRequest{Name: "acc-once", Classes: []string{"read"}})
	assertNoErr(t, err, "mint")

	if plaintext == "" || plaintext == cred.Hash {
		t.Fatalf("plaintext %q is not a distinct secret from the stored hash", plaintext)
	}
	if cred.Hash != config.HashToken(plaintext) {
		t.Fatal("the stored hash is not the plaintext's")
	}
	raw, err := json.Marshal(store.Get())
	assertNoErr(t, err, "marshal settings")
	if bytes.Contains(raw, []byte(plaintext)) {
		t.Fatal("the plaintext token was persisted into settings.json")
	}
}

// ---------------------------------------------------------------------------
// The proxy class and credential expiry (ADR-016 decisions 4 and 3)
// ---------------------------------------------------------------------------

func TestACCMintAcceptsTheProxyClass(t *testing.T) {
	store := newCLISandboxStore(t)
	cred, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-proxier", Classes: []string{"proxy"}})
	assertNoErr(t, err, "mint --class proxy")
	if !cred.Grants(control.ClassProxy) {
		t.Fatalf("classes = %v; the credential is inert despite being accepted", cred.Classes)
	}
	if cred.Grants(control.ClassConfigure) || cred.Grants(control.ClassExecute) {
		t.Fatalf("classes = %v; proxy must not imply any other class", cred.Classes)
	}

	stored := store.Get().APICredentials
	if len(stored) != 1 || !stored[0].Grants(control.ClassProxy) {
		t.Fatalf("the proxy class did not survive the write: %+v", stored)
	}
}

// The refusal message is the only place an operator learns the vocabulary,
// so it is asserted against capabilityClasses itself rather than a literal
// list -- a sixth class added without touching the message would fail here.
func TestACCMintRefusalNamesEveryValidClass(t *testing.T) {
	store := newCLISandboxStore(t)

	_, _, unknownErr := mintAPICredential(store, credentialMintRequest{Name: "acc-bad", Classes: []string{"terminal"}})
	if unknownErr == nil {
		t.Fatal("an unknown class was accepted")
	}
	_, _, emptyErr := mintAPICredential(store, credentialMintRequest{Name: "acc-bad", Classes: nil})
	if emptyErr == nil {
		t.Fatal("an empty class set was accepted")
	}

	for _, err := range []error{unknownErr, emptyErr} {
		for _, class := range capabilityClasses {
			if !strings.Contains(err.Error(), string(class)) {
				t.Fatalf("error %q does not name the valid class %q", err, class)
			}
		}
	}
	if len(store.Get().APICredentials) != 0 {
		t.Fatal("a refused mint still wrote a credential")
	}
}

func TestACCMintTTLSetsAnExpiry(t *testing.T) {
	store := newCLISandboxStore(t)

	forever, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-forever", Classes: []string{"read"}})
	assertNoErr(t, err, "mint with no ttl")
	if forever.Expires != "" {
		t.Fatalf("Expires = %q with no --ttl; absent must mean never", forever.Expires)
	}

	before := time.Now()
	short, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-short", Classes: []string{"read"}, TTL: 12 * time.Hour})
	assertNoErr(t, err, "mint with a ttl")
	at, err := time.Parse(time.RFC3339, short.Expires)
	assertNoErr(t, err, "parse Expires %q", short.Expires)
	if at.Before(before.Add(12*time.Hour-time.Minute)) || at.After(time.Now().Add(12*time.Hour+time.Minute)) {
		t.Fatalf("Expires = %q is not ~12h from now", short.Expires)
	}
	if short.Expired(time.Now()) {
		t.Fatal("a credential minted for 12h is already expired")
	}
}

func TestACCMintRefusesANegativeTTL(t *testing.T) {
	store := newCLISandboxStore(t)
	_, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-negative", Classes: []string{"read"}, TTL: -time.Hour})
	if err == nil {
		t.Fatal("a negative ttl was accepted; it would silently mint a credential that never expires")
	}
	if len(store.Get().APICredentials) != 0 {
		t.Fatal("a refused mint still wrote a credential")
	}
}

// TestACCExpiredCredentialIs401ExactlyLikeAnUnknownOne is the oracle check
// through the real composed stack: frontendCredentialAuth resolves the
// bearer before any handler, so an expired credential must be refused there
// with a byte-identical answer to one that was never minted.
func TestACCExpiredCredentialIs401ExactlyLikeAnUnknownOne(t *testing.T) {
	store := newCLISandboxStore(t)
	srv := accNewServer(t, store, accLegacyToken)

	cred, plaintext, err := mintAPICredential(store, credentialMintRequest{Name: "acc-expiring", Classes: []string{"read"}, TTL: time.Hour})
	assertNoErr(t, err, "mint")

	resp, body := srv.socket(t, "GET", "/api/services", plaintext, nil)
	accAssertReached(t, resp, body, "a live credential before its expiry")

	// Backdate rather than sleep: the stored record is what the gate reads.
	assertNoErr(t, store.With(func(s *config.Settings) {
		findAPICredential(s, cred.ID).Expires = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	}), "backdate the credential")

	expiredResp, expiredBody := srv.socket(t, "GET", "/api/services", plaintext, nil)
	unknownResp, unknownBody := srv.socket(t, "GET", "/api/services", "acc-never-minted", nil)
	if expiredResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an expired credential: status = %d, want 401; body=%s", expiredResp.StatusCode, expiredBody)
	}
	if expiredResp.StatusCode != unknownResp.StatusCode || string(expiredBody) != string(unknownBody) {
		t.Fatalf("expired (%d %q) and unknown (%d %q) are distinguishable; that is an oracle for which credentials exist",
			expiredResp.StatusCode, expiredBody, unknownResp.StatusCode, unknownBody)
	}

	if findAPICredential(store.Get(), cred.ID) == nil {
		t.Fatal("the refusal deleted the record; reaping is lazy and happens on the next mint")
	}
}

func TestACCMintReapsExpiredCredentials(t *testing.T) {
	store := newCLISandboxStore(t)

	live, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-live", Classes: []string{"read"}, TTL: time.Hour})
	assertNoErr(t, err, "mint live")
	forever, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-forever", Classes: []string{"read"}})
	assertNoErr(t, err, "mint forever")
	dead, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-dead", Classes: []string{"read"}, TTL: time.Hour})
	assertNoErr(t, err, "mint dead")

	assertNoErr(t, store.With(func(s *config.Settings) {
		findAPICredential(s, dead.ID).Expires = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	}), "backdate")

	if findAPICredential(store.Get(), dead.ID) == nil {
		t.Fatal("nothing may sweep the expired record before the next mint")
	}

	_, _, err = mintAPICredential(store, credentialMintRequest{Name: "acc-trigger", Classes: []string{"read"}})
	assertNoErr(t, err, "mint trigger")

	s := store.Get()
	if findAPICredential(s, dead.ID) != nil {
		t.Fatal("the mint did not reap the expired credential")
	}
	for _, keep := range []string{live.ID, forever.ID} {
		if findAPICredential(s, keep) == nil {
			t.Fatalf("the reap swept %q, which has not expired", keep)
		}
	}
}

// accCapture runs fn with os.Stdout redirected and returns what it printed.
// The redirect must be in place before fn runs: newTabWriter resolves
// os.Stdout at call time, not at package init.
func accCapture(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	assertNoErr(t, err, "os.Pipe")
	saved := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = saved }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	assertNoErr(t, w.Close(), "close pipe writer")
	out := <-done
	_ = r.Close()
	return out
}

func TestACCListHidesExpiredUnlessAsked(t *testing.T) {
	store := newCLISandboxStore(t)

	forever, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-forever", Classes: []string{"read"}})
	assertNoErr(t, err, "mint forever")
	dead, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-dead", Classes: []string{"read"}, TTL: time.Hour})
	assertNoErr(t, err, "mint dead")
	assertNoErr(t, store.With(func(s *config.Settings) {
		findAPICredential(s, dead.ID).Expires = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	}), "backdate")

	plain := accCapture(t, func() { credentialList(store, nil) })
	if !strings.Contains(plain, "EXPIRES") {
		t.Fatalf("`credential list` has no EXPIRES column:\n%s", plain)
	}
	if !strings.Contains(plain, forever.ID) {
		t.Fatalf("`credential list` hid a live credential:\n%s", plain)
	}
	if !strings.Contains(plain, "never") {
		t.Fatalf("a credential with no expiry must read as never, not as a blank cell:\n%s", plain)
	}
	if strings.Contains(plain, dead.ID) {
		t.Fatalf("`credential list` showed an expired credential without --include-expired:\n%s", plain)
	}

	withExpired := accCapture(t, func() { credentialList(store, []string{"--include-expired"}) })
	if !strings.Contains(withExpired, dead.ID) {
		t.Fatalf("--include-expired did not show the expired credential:\n%s", withExpired)
	}
	if !strings.Contains(withExpired, "(expired)") {
		t.Fatalf("--include-expired did not mark the expired credential as expired:\n%s", withExpired)
	}
	if !strings.Contains(withExpired, forever.ID) {
		t.Fatalf("--include-expired dropped the live credentials:\n%s", withExpired)
	}
}

func TestACCListSaysSoWhenEveryCredentialHasExpired(t *testing.T) {
	store := newCLISandboxStore(t)
	dead, _, err := mintAPICredential(store, credentialMintRequest{Name: "acc-dead", Classes: []string{"read"}, TTL: time.Hour})
	assertNoErr(t, err, "mint")
	assertNoErr(t, store.With(func(s *config.Settings) {
		findAPICredential(s, dead.ID).Expires = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	}), "backdate")

	out := accCapture(t, func() { credentialList(store, nil) })
	if strings.Contains(out, dead.ID) {
		t.Fatalf("an expired credential was listed by default:\n%s", out)
	}
	if !strings.Contains(out, "--include-expired") {
		t.Fatalf("a listing emptied by expiry must say where the records went:\n%s", out)
	}
}
