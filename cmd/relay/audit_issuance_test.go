package main

// Every act that issues or revokes a credential must leave a record, from
// whichever door it was initiated, and no record may carry the secret the act
// produced. These tests drive the real doors — the CLI subcommand functions,
// the LoginOps core the tray and the Passkeys tab share, the registered HTTP
// handlers, and a full WebAuthn ceremony — rather than calling the recorder by
// hand, because a record written only by a test proves nothing about the path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/sealed"
)

// aiHome sandboxes the config dir and returns an initialised store rooted in
// it, so auditLogPath() and NewSettingsStore() both resolve inside the sandbox
// the way they do for a real CLI process.
func aiHome(t *testing.T) (string, config.SettingsStore) {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	return dir, store
}

// aiQuiet runs fn with stdout redirected and returns what it printed. The CLI
// subcommands print a freshly minted plaintext, which is exactly the value the
// leak assertions need and exactly the value that must not end up in the test
// log.
func aiQuiet(t *testing.T, fn func()) string {
	t.Helper()
	saved := os.Stdout
	r, w, err := os.Pipe()
	assertNoErr(t, err, "os.Pipe")
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(r)
		done <- string(out)
	}()
	func() {
		defer func() {
			os.Stdout = saved
			_ = w.Close()
		}()
		fn()
	}()
	return <-done
}

// aiLogText reads the whole audit log as bytes, which is what a leak check
// wants: it does not matter which field a secret rode out on, only that it
// reached the file.
func aiLogText(t *testing.T) string {
	t.Helper()
	path, err := auditLogPath()
	assertNoErr(t, err, "auditLogPath")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read audit log: %v", err)
	}
	return string(data)
}

func aiParse(t *testing.T, text string) []audit.AuditEvent {
	t.Helper()
	var out []audit.AuditEvent
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		var ev audit.AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("audit log line is not valid JSON: %v\nline: %s", err, line)
		}
		out = append(out, ev)
	}
	return out
}

// aiOnly finds the single record of the given event kind naming subject, and
// fails with the whole set when there is not exactly one.
func aiOnly(t *testing.T, events []audit.AuditEvent, event, subject string) audit.AuditEvent {
	t.Helper()
	var found []audit.AuditEvent
	for _, ev := range events {
		if ev.Event == event && ev.Subject == subject {
			found = append(found, ev)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s record for subject %q, got %d in %s", event, subject, len(found), aiSummarise(events))
	}
	return found[0]
}

func aiSummarise(events []audit.AuditEvent) string {
	var b strings.Builder
	for _, ev := range events {
		fmt.Fprintf(&b, "\n  %s credential=%q subject=%q grants=%v via=%q", ev.Event, ev.Credential, ev.Subject, ev.Grants, ev.Via)
	}
	if b.Len() == 0 {
		return " (no events)"
	}
	return b.String()
}

// aiRefuseSecrets is the leak check every new record type is put through. Each
// needle is a value that exists somewhere in relay's state for this act — a
// plaintext, a stored hash, a public key coordinate — and none of them may
// appear anywhere in the log's bytes.
func aiRefuseSecrets(t *testing.T, logText string, needles map[string]string) {
	t.Helper()
	for label, needle := range needles {
		if needle == "" {
			t.Fatalf("leak check %q was handed an empty needle, which would pass vacuously", label)
		}
		if strings.Contains(logText, needle) {
			t.Errorf("the audit log contains the %s (%q)", label, needle)
		}
	}
}

// aiGrants renders a record's grant list for an assertion message.
func aiGrants(ev audit.AuditEvent) string { return strings.Join(ev.Grants, ",") }

// ---------------------------------------------------------------------------
// The CLI doors
// ---------------------------------------------------------------------------

// credential mint and revoke are brokered (ADR-017 decision 2): this
// process holds no sealer, so mintAPICredential/revokeAPICredentialIf run
// inside CredentialOps on the other end of a real bridge connection, not in
// this test's own process.
func TestIssuance_CLICredentialMintAndRevokeAreRecorded(t *testing.T) {
	_, store := aiHome(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	printed := aiQuiet(t, func() {
		credentialMint(store, []string{"--name", "ci-deploy", "--class", "read", "--class", "grant"})
	})

	creds := store.Reload().APICredentials
	if len(creds) != 1 {
		t.Fatalf("want 1 credential after mint, got %d", len(creds))
	}
	cred := creds[0]

	issued := aiOnly(t, aiParse(t, aiLogText(t)), audit.AuditEventCredentialIssued, cred.ID)
	if issued.Credential != auditCredentialAPI {
		t.Errorf("credential = %q, want %q", issued.Credential, auditCredentialAPI)
	}
	if issued.SubjectName != "ci-deploy" {
		t.Errorf("subject_name = %q, want %q", issued.SubjectName, "ci-deploy")
	}
	if got := aiGrants(issued); got != "read,grant" {
		t.Errorf("grants = %q, want %q — the class set is the whole point of this record", got, "read,grant")
	}
	if issued.Via != auditViaCLI {
		t.Errorf("via = %q, want %q", issued.Via, auditViaCLI)
	}
	if issued.Actor.Kind != audit.AuditActorOperator {
		t.Errorf("actor.kind = %q, want %q", issued.Actor.Kind, audit.AuditActorOperator)
	}

	aiQuiet(t, func() { credentialRevoke(store, []string{"--id", cred.ID}) })

	revoked := aiOnly(t, aiParse(t, aiLogText(t)), audit.AuditEventCredentialRevoked, cred.ID)
	if revoked.Credential != auditCredentialAPI || revoked.Via != auditViaCLI {
		t.Errorf("revocation record = %+v, want an api_credential revoked via cli", revoked)
	}

	plaintext := aiPrintedToken(t, printed)
	aiRefuseSecrets(t, aiLogText(t), map[string]string{
		"minted plaintext": plaintext,
		"stored hash":      cred.Hash,
	})
}

// aiPrintedToken pulls the plaintext out of `relay credential mint`'s output,
// which is the only place it ever exists.
func aiPrintedToken(t *testing.T, printed string) string {
	t.Helper()
	for _, line := range strings.Split(printed, "\n") {
		if _, rest, ok := strings.Cut(line, "token:"); ok {
			if token := strings.TrimSpace(rest); token != "" {
				return token
			}
		}
	}
	t.Fatalf("mint printed no token: %s", printed)
	return ""
}

// TestIssuance_EnrolCreateAndCLIRevokeAreRecorded exercises create through
// the CLI path — `relay enrol create` itself, brokered over admin_op
// (ADR-017 decision 2) into the exact same EnrolmentOps a real tray would
// run: this process holds no sealer and never reaches relay's CA key
// directly (§5.3.3, §5.4, AC-29), it only ever builds the request and
// prints what comes back over the bridge. Revoke is the same shape.
func TestIssuance_EnrolCreateAndCLIRevokeAreRecorded(t *testing.T) {
	dir, store := aiHome(t)
	profile := mkStoreProject(t, store, config.ProjectKindRemote, "Mail", "")
	serveBroker(t, newBrokerRouter(t, store, nil))

	aiQuiet(t, func() {
		enrolCreate(store, []string{"--client-id", "hermes-mail", "--grant", profile.ID})
	})

	issued := aiOnly(t, aiParse(t, aiLogText(t)), audit.AuditEventCredentialIssued, "hermes-mail")
	if issued.Credential != auditCredentialEnrolment {
		t.Errorf("credential = %q, want %q", issued.Credential, auditCredentialEnrolment)
	}
	if got := aiGrants(issued); got != profile.ID {
		t.Errorf("grants = %q, want %q — an enrolment's record must name what it reaches", got, profile.ID)
	}
	if issued.Via != auditViaCLI {
		t.Errorf("via = %q, want %q", issued.Via, auditViaCLI)
	}

	enrolments := store.Reload().Enrolments
	if len(enrolments) != 1 {
		t.Fatalf("want 1 enrolment, got %d", len(enrolments))
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, enrolment.BundleDir, "hermes-mail", "client.key"))
	assertNoErr(t, err, "read client key")

	aiQuiet(t, func() { enrolRevoke(store, []string{"--client-id", "hermes-mail"}) })
	revoked := aiOnly(t, aiParse(t, aiLogText(t)), audit.AuditEventCredentialRevoked, "hermes-mail")
	if revoked.Credential != auditCredentialEnrolment {
		t.Errorf("credential = %q, want %q", revoked.Credential, auditCredentialEnrolment)
	}

	aiRefuseSecrets(t, aiLogText(t), map[string]string{
		"client private key": aiPEMBody(t, keyPEM),
		"CA private key":     aiPEMBody(t, aiUnsealCAKeyPEM(t, dir, store)),
	})
}

// aiUnsealCAKeyPEM reads and opens ca.key.sealed directly — relay never
// writes a plaintext ca.key (§5.7), so this is what a test now has to do to
// get the CA's own private key material to check it never appears in the
// audit log.
func aiUnsealCAKeyPEM(t *testing.T, dir string, store config.SettingsStore) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, enrolment.CAKeySealedFile))
	assertNoErr(t, err, "read ca.key.sealed")
	var env sealed.Envelope
	assertNoErr(t, json.Unmarshal(data, &env), "parse ca.key.sealed")
	keyPEM, err := store.Sealer().Unseal(env, []byte(config.CAAADPrefix+"ca.key"))
	assertNoErr(t, err, "unseal ca.key.sealed")
	return keyPEM
}

// login enrol and login revoke are brokered too (ADR-017 decision 2 — the
// ADR-016 decision 2 SSH affordance is withdrawn on purpose, §3.2), so this
// needs the same real bridge server the enrolment test above does.
func TestIssuance_CLILoginEnrolAndPasskeyRevokeAreRecorded(t *testing.T) {
	_, store := aiHome(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	printed := aiQuiet(t, func() { loginEnrol(store) })

	anchor := store.Reload().LoginBootstrap
	if anchor == nil {
		t.Fatal("login enrol stored no bootstrap anchor")
	}
	issued := aiOnly(t, aiParse(t, aiLogText(t)), audit.AuditEventCredentialIssued, anchor.Expires)
	if issued.Credential != auditCredentialBootstrap {
		t.Errorf("credential = %q, want %q", issued.Credential, auditCredentialBootstrap)
	}
	if issued.Via != auditViaCLI {
		t.Errorf("via = %q, want %q", issued.Via, auditViaCLI)
	}

	passkey := aiStorePasskey(t, store, "pk-cli")
	aiQuiet(t, func() { loginRevoke(store, []string{"--id", passkey.ID}) })
	revoked := aiOnly(t, aiParse(t, aiLogText(t)), audit.AuditEventCredentialRevoked, passkey.ID)
	if revoked.Credential != auditCredentialPasskey || revoked.SubjectName != passkey.Name {
		t.Errorf("revocation record = %+v, want the passkey and its name", revoked)
	}

	aiRefuseSecrets(t, aiLogText(t), map[string]string{
		"printed login code": aiPrintedCode(t, printed),
		"bootstrap hash":     anchor.Hash,
		"passkey public X":   string(passkey.X),
	})
}

func aiPrintedCode(t *testing.T, printed string) string {
	t.Helper()
	for _, line := range strings.Split(printed, "\n") {
		if _, rest, ok := strings.Cut(line, "login code:"); ok {
			if code := strings.TrimSpace(rest); code != "" {
				return code
			}
		}
	}
	t.Fatalf("login enrol printed no code: %s", printed)
	return ""
}

// aiStorePasskey writes a passkey whose public key coordinates are a
// recognisable byte string, so a leak check has something unambiguous to look
// for.
func aiStorePasskey(t *testing.T, store config.SettingsStore, id string) config.Passkey {
	t.Helper()
	p := config.Passkey{
		ID:         id,
		Name:       "browser passkey " + id,
		X:          []byte("AI-PUBLIC-KEY-X-COORDINATE"),
		Y:          []byte("AI-PUBLIC-KEY-Y-COORDINATE"),
		UserHandle: "handle",
		Created:    time.Now().UTC().Format(time.RFC3339),
	}
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.Passkeys = append(s.Passkeys, p)
	}), "store passkey")
	return p
}

func aiRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	assertNoErr(t, err, "read %s", path)
	return data
}

// aiPEMBody returns the first base64 line of a PEM block: a needle that is
// unmistakably key material and cannot collide with anything else in the log.
func aiPEMBody(t *testing.T, pem []byte) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(pem)), "\n")
	if len(lines) < 2 {
		t.Fatalf("not a PEM block: %s", pem)
	}
	return strings.TrimSpace(lines[1])
}

// ---------------------------------------------------------------------------
// The tray menu item and the Settings window
// ---------------------------------------------------------------------------

func TestIssuance_TrayAndSettingsWindowActsAreRecorded(t *testing.T) {
	dir, store := aiHome(t)
	rec := aiRecorderAt(t, filepath.Join(dir, "rec.jsonl"), nil)
	ops := &LoginOps{Store: store, Audit: rec, Gate: allowGate(t)}

	view, err := ops.MintBootstrap(context.Background(), auditViaTray)
	assertNoErr(t, err, "MintBootstrap")

	passkey := aiStorePasskey(t, store, "pk-ipc")
	_, err = ops.RevokePasskey(context.Background(), passkey.ID)
	assertNoErr(t, err, "RevokePasskey")

	session, sessionToken, err := aiMintLoginSession(store)
	assertNoErr(t, err, "mint login session")
	_, err = ops.SignOut(session.ID)
	assertNoErr(t, err, "SignOut")

	events := readLoggedEvents(t, rec)

	anchor := aiOnly(t, events, audit.AuditEventCredentialIssued, view.Expires)
	if anchor.Credential != auditCredentialBootstrap || anchor.Via != auditViaTray {
		t.Errorf("bootstrap record = %+v, want a bootstrap_code issued via tray", anchor)
	}
	revokedKey := aiOnly(t, events, audit.AuditEventCredentialRevoked, passkey.ID)
	if revokedKey.Credential != auditCredentialPasskey || revokedKey.Via != auditViaIPC {
		t.Errorf("passkey record = %+v, want a passkey revoked via ipc", revokedKey)
	}
	signedOut := aiOnly(t, events, audit.AuditEventCredentialRevoked, session.ID)
	if signedOut.Credential != auditCredentialAPI || signedOut.Via != auditViaIPC {
		t.Errorf("sign-out record = %+v, want an api_credential revoked via ipc", signedOut)
	}

	rec.Flush()
	logged, err := os.ReadFile(rec.Path())
	assertNoErr(t, err, "read recorder log")
	aiRefuseSecrets(t, string(logged), map[string]string{
		"minted login code":       view.Code,
		"login session plaintext": sessionToken,
		"login session hash":      session.Hash,
		"passkey public X":        string(passkey.X),
	})
}

func aiMintLoginSession(store config.SettingsStore) (config.APICredential, string, error) {
	var cred config.APICredential
	var plaintext string
	var mintErr error
	err := store.With(func(s *config.Settings) {
		cred, plaintext, mintErr = mintAPICredentialFor(s, loginCredentialPrefix+"aitest", loginCredentialClasses, loginCredentialTTL)
	})
	if err != nil {
		return config.APICredential{}, "", err
	}
	return cred, plaintext, mintErr
}

// aiRecorderAt builds a real rotating recorder at path, the way the tray's
// does.
func aiRecorderAt(t *testing.T, path string, cfg *config.AuditConfig) *audit.AuditRecorder {
	t.Helper()
	rec, err := audit.NewAuditRecorder(cfg, path, openAuditWriter)
	assertNoErr(t, err, "NewAuditRecorder")
	if rec == nil {
		t.Fatal("NewAuditRecorder returned nil for an enabled config")
	}
	t.Cleanup(rec.Close)
	return rec
}

// ---------------------------------------------------------------------------
// The HTTP doors
// ---------------------------------------------------------------------------

// aiHTTP mounts the enrolment and project routes through the real
// control.RouteRegistrar with a real credential authorizer, so the credential a
// request resolves to is the one attributed in the record.
type aiHTTPFixture struct {
	srv    *httptest.Server
	store  config.SettingsStore
	rec    *audit.AuditRecorder
	bearer string
	credID string
	hash   string
}

func aiNewHTTP(t *testing.T, issuance IssuanceAuditor, rec *audit.AuditRecorder) *aiHTTPFixture {
	t.Helper()
	dir, store := aiHome(t)
	if rec == nil {
		rec = aiRecorderAt(t, filepath.Join(dir, "rec.jsonl"), nil)
	}
	if issuance == nil {
		issuance = issuanceAuditorOrNil(rec)
	}

	var bearer string
	var cred config.APICredential
	assertNoErr(t, store.With(func(s *config.Settings) {
		var err error
		cred, bearer, err = mintAPICredentialForever(s, "ai-operator", []control.CapabilityClass{control.ClassRead, control.ClassConfigure, control.ClassGrant})
		assertNoErr(t, err, "Mint")
	}), "store.With mint")

	mux := http.NewServeMux()
	rr := &control.RouteRegistrar{CredentialID: APICredentialIDFromContext,
		Mux:       mux,
		Transport: control.TransportSocket,
		Authz:     NewCredentialAuthorizer(store),
		Auditor:   audit.ControlAuditorOrNil(rec),
	}
	extMgr := NewExternalMcpManager(nil)
	RegisterEnrolmentRoutes(rr, &EnrolmentOps{Store: store, Gate: allowGate(t), Audit: rec, Issuance: issuance})
	projOps := &ProjectOps{Store: store, Gate: allowGate(t), Issuance: issuance}
	RegisterProjectRoutes(rr, store, projOps, extMgr, nil, nil, nil, nil)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &aiHTTPFixture{srv: srv, store: store, rec: rec, bearer: bearer, credID: cred.ID, hash: cred.Hash}
}

func (f *aiHTTPFixture) do(t *testing.T, method, path string, body any) (*http.Response, []byte) {
	t.Helper()
	return doJSONAuth(t, method, f.srv.URL+path, body, f.bearer)
}

func TestIssuance_HTTPEnrolmentAndRotateTokenAreRecorded(t *testing.T) {
	f := aiNewHTTP(t, nil, nil)
	profile := mkStoreProject(t, f.store, config.ProjectKindRemote, "Mail", "")
	local := mkStoreProject(t, f.store, config.ProjectKindLocal, "Local", t.TempDir())

	resp, body := f.do(t, "POST", "/api/enrolments", map[string]any{
		"client_id":   "hermes-http",
		"project_ids": []string{profile.ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/enrolments: status %d, body %s", resp.StatusCode, body)
	}

	resp, body = f.do(t, "POST", "/api/projects/"+local.ID+"/rotate_token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate_token: status %d, body %s", resp.StatusCode, body)
	}
	var rotated struct {
		Token string `json:"token"`
	}
	assertNoErr(t, json.Unmarshal(body, &rotated), "decode rotate body")

	resp, body = f.do(t, "DELETE", "/api/enrolments/hermes-http", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /api/enrolments: status %d, body %s", resp.StatusCode, body)
	}

	events := readLoggedEvents(t, f.rec)
	credID := f.credID

	enrolled := aiOnly(t, events, audit.AuditEventCredentialIssued, "hermes-http")
	if enrolled.Credential != auditCredentialEnrolment || enrolled.Via != auditViaHTTP {
		t.Errorf("enrolment record = %+v, want an enrolment issued via http", enrolled)
	}
	if enrolled.Actor.CredID != credID {
		t.Errorf("actor.cred_id = %q, want %q — an HTTP issuance must name the credential that asked", enrolled.Actor.CredID, credID)
	}

	rotation := aiOnly(t, events, audit.AuditEventCredentialIssued, local.ID)
	if rotation.Credential != auditCredentialProject || rotation.Via != auditViaHTTP {
		t.Errorf("rotation record = %+v, want a project_token issued via http", rotation)
	}

	revoked := aiOnly(t, events, audit.AuditEventCredentialRevoked, "hermes-http")
	if revoked.Credential != auditCredentialEnrolment {
		t.Errorf("revocation record = %+v, want an enrolment revoked", revoked)
	}

	// A control_decision names the route a caller was allowed to reach; it
	// does not name the act. Both must be present, and the issuance record is
	// the one that says a token was rotated.
	if !aiHasControlDecision(events, "/api/projects/"+local.ID+"/rotate_token") {
		t.Error("no control_decision for rotate_token; the two records are meant to coexist, not replace each other")
	}

	f.rec.Flush()
	logged, err := os.ReadFile(f.rec.Path())
	assertNoErr(t, err, "read recorder log")
	stored, _ := config.FindProjectByID(f.store.Reload(), local.ID)
	storedToken, _ := stored.Token.Reveal()
	aiRefuseSecrets(t, string(logged), map[string]string{
		"rotated project token":  rotated.Token,
		"stored project token":   storedToken,
		"project token hash":     stored.TokenHash,
		"operator bearer":        f.bearer,
		"operator bearer's hash": f.hash,
	})
}

func aiHasControlDecision(events []audit.AuditEvent, path string) bool {
	for _, ev := range events {
		if ev.Event == audit.AuditEventControlDecision && ev.Path == path {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The login ceremony
// ---------------------------------------------------------------------------

func TestIssuance_LoginCeremonyRecordsThePasskeyAndTheCredential(t *testing.T) {
	rec := aiRecorderAt(t, filepath.Join(mkShortTempDir(t, "ai-lr-"), "audit.jsonl"), nil)
	s := aiNewLoginServer(t, rec)

	auth, token := s.enrolled()

	events := readLoggedEvents(t, rec)
	stored := s.store.Reload().Passkeys
	if len(stored) != 1 {
		t.Fatalf("want 1 registered passkey, got %d", len(stored))
	}

	registered := aiOnly(t, events, audit.AuditEventCredentialIssued, stored[0].ID)
	if registered.Credential != auditCredentialPasskey || registered.Via != auditViaHTTP {
		t.Errorf("registration record = %+v, want a passkey issued via http", registered)
	}

	cred := s.loginCredential()
	minted := aiOnly(t, events, audit.AuditEventCredentialIssued, cred.ID)
	if minted.Credential != auditCredentialAPI {
		t.Errorf("credential = %q, want %q", minted.Credential, auditCredentialAPI)
	}
	if got := aiGrants(minted); got != "read,configure" {
		t.Errorf("grants = %q, want %q — the class set a login session reaches is the point of the record", got, "read,configure")
	}

	rec.Flush()
	logged, err := os.ReadFile(rec.Path())
	assertNoErr(t, err, "read recorder log")
	aiRefuseSecrets(t, string(logged), map[string]string{
		"login session plaintext": token,
		"login session hash":      cred.Hash,
		"passkey public X":        aiHexOf(stored[0].X),
		"authenticator cred id":   aiHexOf(auth.credID),
	})
}

// aiHexOf renders bytes as hex so a leak check has a stable needle even for a
// value that is not printable.
func aiHexOf(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		sb.WriteString(strconv.FormatInt(int64(c), 16))
	}
	return sb.String()
}

// aiNewLoginServer is lrNewServer with an audit.AuditOps carrying a real recorder,
// which is the one ingredient the login routes take their issuance auditor
// from. It builds the same lrServer so the ceremony helpers there drive it.
func aiNewLoginServer(t *testing.T, rec *audit.AuditRecorder) *lrServer {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	sock := filepath.Join(mkShortTempDir(t, "ai-lr-sock-"), "frontend.sock")
	auditor := &lrAuditor{}
	extMgr := NewExternalMcpManager(nil)
	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		Endpoint{Socket: sock, Token: "ai-frontend-token"},
		NewEnhancedServiceRegistry(nil),
		nil, nil, nil, nil, &audit.AuditOps{Audit: rec},
		&McpOps{Store: store, Ctx: context.Background()},
		&ProjectOps{Store: store, Gate: allowGate(t), Issuance: issuanceAuditorOrNil(rec)},
		NewCredentialAuthorizer(store), auditor,
	)
	assertNoErr(t, err, "NewFrontendServer")
	assertNoErr(t, srv.ListenLoopback("127.0.0.1:0"), "ListenLoopback")
	go func() { _ = srv.Serve() }()
	go func() { _ = srv.ServeLoopback() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	origin := srv.LoginOrigin()
	return &lrServer{
		t:       t,
		store:   store,
		dir:     dir,
		base:    "http://127.0.0.1:" + origin[strings.LastIndex(origin, ":")+1:],
		origin:  origin,
		sock:    sock,
		auditor: auditor,
	}
}

// ---------------------------------------------------------------------------
// Fail-closed: nothing anyone can use is created without a record
// ---------------------------------------------------------------------------

// aiBrokenAuditor is a sink that exists and fails, which is the state issuance
// is fail-closed over — distinct from auditing being off, which is not.
type aiBrokenAuditor struct{ calls int }

func (a *aiBrokenAuditor) RecordIssuance(audit.CredentialIssuance) error {
	a.calls++
	return errors.New("audit write: disk is full")
}

func TestIssuance_HTTPRotateWithholdsTheTokenWhenTheRecordFails(t *testing.T) {
	broken := &aiBrokenAuditor{}
	f := aiNewHTTP(t, broken, nil)
	local := mkStoreProject(t, f.store, config.ProjectKindLocal, "Local", t.TempDir())

	resp, body := f.do(t, "POST", "/api/projects/"+local.ID+"/rotate_token", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("rotate_token: status %d, want 500 when the act cannot be recorded; body %s", resp.StatusCode, body)
	}
	stored, _ := config.FindProjectByID(f.store.Reload(), local.ID)
	storedToken, _ := stored.Token.Reveal()
	if strings.Contains(string(body), storedToken) {
		t.Fatalf("the response carried the new project token even though the act was not recorded: %s", body)
	}
	if broken.calls != 1 {
		t.Errorf("issuance auditor called %d times, want 1", broken.calls)
	}
}

func TestIssuance_HTTPEnrolmentIsRevokedWhenTheRecordFails(t *testing.T) {
	broken := &aiBrokenAuditor{}
	f := aiNewHTTP(t, broken, nil)
	profile := mkStoreProject(t, f.store, config.ProjectKindRemote, "Mail", "")

	resp, body := f.do(t, "POST", "/api/enrolments", map[string]any{
		"client_id":   "hermes-unrecorded",
		"project_ids": []string{profile.ID},
	})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("POST /api/enrolments: status %d, want 500 when the act cannot be recorded; body %s", resp.StatusCode, body)
	}
	if e := enrolment.Find(f.store.Reload(), "hermes-unrecorded"); e != nil {
		t.Fatal("an enrolment that could not be recorded survived; the client certificate is live with nothing in the log")
	}
	if strings.Contains(string(body), "dir") {
		t.Errorf("the refusal handed back a bundle directory: %s", body)
	}
}

func TestIssuance_TrayWithholdsTheLoginCodeWhenTheRecordFails(t *testing.T) {
	_, store := aiHome(t)
	// A recorder over a writer that refuses every write is the "sink exists
	// and fails" state; a nil recorder would be "auditing is off", which is
	// deliberately not a refusal.
	rec := audit.NewAuditRecorderWith(audit.ResolveAuditConfig(&config.AuditConfig{}), "ai-broken", failingWriteCloser{})
	t.Cleanup(rec.Close)
	ops := &LoginOps{Store: store, Audit: rec, Gate: allowGate(t)}

	view, err := ops.MintBootstrap(context.Background(), auditViaTray)
	if err == nil {
		t.Fatalf("MintBootstrap succeeded with an unwritable audit log and returned code %q", view.Code)
	}
	if view.Code != "" {
		t.Errorf("a refused mint still carried a code: %q", view.Code)
	}
}

type failingWriteCloser struct{}

func (failingWriteCloser) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }
func (failingWriteCloser) Close() error              { return nil }

// TestIssuance_RecordIssuanceIsANoOpForUngatedCallersWhenAuditingIsOff pins
// what ADR-017's Consequences still lean on: recordIssuance(nil, ...)
// returns nil rather than refusing, for the callers that are not one of the
// six gated cores.
//
// This test used to also mint a credential via the CLI to show issuance
// "must keep working" with auditing off — that is no longer true, and not a
// weakening of this test but a correction of it: `credential.mint` is a
// gated operation (presence.GatedOps), and §7.4's hard dependency
// (requireIssuanceAuditor, called before Gate.Require) now lives INSIDE
// CredentialOps.Mint itself, so every door that reaches it — CLI over
// admin_op, HTTP, IPC alike — refuses with auditing off. That refusal is
// covered once, generically, for every gated op by
// TestGate_IssuanceAuditingOffRefusesBeforeThePrompt in
// presence_gate_wiring_test.go; duplicating it here via a CLI subprocess
// would test the same core twice through a heavier harness.
func TestIssuance_RecordIssuanceIsANoOpForUngatedCallersWhenAuditingIsOff(t *testing.T) {
	_, store := aiHome(t)
	off := false
	assertNoErr(t, store.With(func(s *config.Settings) { s.Audit = &config.AuditConfig{Enabled: &off} }), "disable auditing")

	rec, err := openCLIIssuanceRecorder(store)
	assertNoErr(t, err, "openCLIIssuanceRecorder with auditing off")
	if rec != nil {
		t.Fatal("openCLIIssuanceRecorder built a recorder while auditing is off")
	}
	if err := recordIssuance(issuanceAuditorOrNil(rec), audit.CredentialIssuance{Credential: auditCredentialAPI, Subject: "x"}); err != nil {
		t.Fatalf("recordIssuance refused while auditing is off: %v", err)
	}
}

// TestIssuance_CLIRefusesWhenTheSinkCannotBeOpened is the CLI's fail-closed
// half. cliIssuanceAuditor exits the process on this error, so what is
// asserted here is the error it exits on — a sink that should exist and
// cannot be opened.
func TestIssuance_CLIRefusesWhenTheSinkCannotBeOpened(t *testing.T) {
	dir, store := aiHome(t)

	// A regular file where the audit directory belongs: MkdirAll and OpenFile
	// both fail, which is what a broken or hostile log location looks like.
	logs := filepath.Join(dir, "logs")
	assertNoErr(t, os.MkdirAll(logs, 0o700), "mkdir logs")
	assertNoErr(t, os.WriteFile(filepath.Join(logs, "audit"), []byte("not a directory"), 0o600), "write blocker")

	rec, err := openCLIIssuanceRecorder(store)
	if err == nil {
		if rec != nil {
			rec.Close()
		}
		t.Fatal("openCLIIssuanceRecorder succeeded with an unopenable log; a CLI mint would proceed unrecorded")
	}
}

// ---------------------------------------------------------------------------
// Cross-process: a CLI process appends, and never rotates
// ---------------------------------------------------------------------------

// TestIssuance_CLIAppendsBesideTheTrayAndNeverRotatesItsLog is the
// cross-process guarantee. Rotation renames the log out from under every other
// open descriptor, so a CLI process must never do it: the tray holds one for
// the life of the app.
func TestIssuance_CLIAppendsBesideTheTrayAndNeverRotatesItsLog(t *testing.T) {
	_, store := aiHome(t)
	path, err := auditLogPath()
	assertNoErr(t, err, "auditLogPath")

	const maxFileBytes = 2048
	cfg := &config.AuditConfig{MaxFileBytes: maxFileBytes, Generations: 3}
	assertNoErr(t, store.With(func(s *config.Settings) { s.Audit = cfg }), "set audit config")

	tray := aiRecorderAt(t, path, cfg)
	const canary = "ai-tray-canary"
	assertNoErr(t, tray.RecordIssuance(audit.CredentialIssuance{
		Credential: auditCredentialAPI, Subject: canary, Via: auditViaTray,
	}), "tray record")

	cli, err := openCLIIssuanceRecorder(store)
	assertNoErr(t, err, "openCLIIssuanceRecorder")
	for i := 0; i < 40; i++ {
		assertNoErr(t, cli.RecordIssuance(audit.CredentialIssuance{
			Credential: auditCredentialAPI,
			Subject:    fmt.Sprintf("ai-cli-%02d", i),
			Via:        auditViaCLI,
		}), "cli record %d", i)
	}
	cli.Close()

	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("a CLI process rotated the log (stat .1 err %v); the tray's open descriptor now points at a file that has been renamed out from under it", err)
	}
	info, err := os.Stat(path)
	assertNoErr(t, err, "stat log")
	if info.Size() <= maxFileBytes {
		t.Fatalf("the log is %d bytes, at or under the %d-byte cap — this test proves nothing unless the CLI pushed it over",
			info.Size(), maxFileBytes)
	}

	// Every record, from both processes, is a whole line in the one file.
	events := aiParse(t, aiLogText(t))
	aiOnly(t, events, audit.AuditEventCredentialIssued, canary)
	for i := 0; i < 40; i++ {
		aiOnly(t, events, audit.AuditEventCredentialIssued, fmt.Sprintf("ai-cli-%02d", i))
	}

	// The cost of never rotating from the CLI, stated as an assertion: the
	// tray's writer counts only the bytes it has itself written since it
	// opened the file, so it rotates on its own accounting and the CLI's
	// contribution is what the file overshoots the cap by. When that rotation
	// finally comes, the whole file goes with it — CLI records included — so
	// the overshoot costs a soft cap and never a lost record.
	for i := 0; i < 200; i++ {
		assertNoErr(t, tray.RecordIssuance(audit.CredentialIssuance{
			Credential: auditCredentialAPI,
			Subject:    fmt.Sprintf("ai-tray-after-%03d", i),
			Via:        auditViaTray,
		}), "tray record after %d", i)
		if _, err := os.Stat(path + ".1"); err == nil {
			break
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("the tray never rotated on its own accounting: %v", err)
	}
	rotated := aiParse(t, string(aiRead(t, path+".1"))+string(aiRead(t, path)))
	for i := 0; i < 40; i++ {
		aiOnly(t, rotated, audit.AuditEventCredentialIssued, fmt.Sprintf("ai-cli-%02d", i))
	}
}

// A CLI credential mint with no tray running used to write its record
// straight to disk anyway — every issuing command held "a recorder of its
// own" independent of the tray. Brokering (ADR-017 decision 2) retires that
// capability on purpose: CredentialOps.Mint runs inside the tray, not in
// this process, so "no tray at all" is no longer a state relay half
// supports — it is a clean, named refusal before anything is written
// (TestBrokeredCommands_RefuseByNameAndTouchNothing, AC-11/AC-12), and a
// record that DOES land, once the tray is running, reaches exactly the file
// `relay audit` reads (TestIssuance_CLICredentialMintAndRevokeAreRecorded,
// which now runs the same command through a real bridge server).

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func TestIssuance_RelayAuditRendersAnIssuanceRowLegibly(t *testing.T) {
	events := []audit.AuditEvent{
		audit.IssuanceEvent(audit.CredentialIssuance{
			Credential: auditCredentialAPI,
			Subject:    "cred_5e2a",
			Name:       "ci-deploy",
			Grants:     []string{"read", "grant"},
			Via:        auditViaCLI,
		}),
		audit.IssuanceEvent(audit.CredentialIssuance{
			Revoked:    true,
			Credential: auditCredentialEnrolment,
			Subject:    "hermes-mail",
			Grants:     []string{"proj_mail"},
			Via:        auditViaHTTP,
			CredID:     "cred_op",
		}),
	}

	var buf strings.Builder
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	writeAuditTable(w, events, false)
	assertNoErr(t, w.Flush(), "flush")
	rendered := buf.String()

	for _, want := range []string{
		"credential_issued row is legible|api_credential",
		"the identifier is on the row|cred_5e2a",
		"the name is on the row|ci-deploy",
		"the class set is on the row|grants=read,grant",
		"the door is on the row|via=cli",
		"a revocation renders too|hermes-mail",
		"an enrolment's grant renders|grants=proj_mail",
	} {
		label, needle, _ := strings.Cut(want, "|")
		if !strings.Contains(rendered, needle) {
			t.Errorf("%s: %q missing from\n%s", label, needle, rendered)
		}
	}
	if strings.Contains(rendered, "\n\n") {
		t.Errorf("the table has a blank row:\n%s", rendered)
	}
}

// TestIssuance_RenderedRowCarriesNoSecret drives the renderer with a record
// whose every field has been stuffed with a value that looks like a secret,
// proving the row is built from the issuance fields and nothing else.
func TestIssuance_RenderedRowCarriesNoSecret(t *testing.T) {
	ev := audit.IssuanceEvent(audit.CredentialIssuance{
		Credential: auditCredentialAPI,
		Subject:    "cred_5e2a",
		Name:       "ci-deploy",
		Grants:     []string{"read"},
		Via:        auditViaCLI,
	})
	// Fields no issuance record populates, set here to prove the issuance
	// branch reads none of them.
	ev.Args = json.RawMessage(`{"token":"ai-plaintext-should-not-render"}`)
	ev.ResultPreview = "ai-preview-should-not-render"

	var buf strings.Builder
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	writeAuditTable(w, []audit.AuditEvent{ev}, true)
	assertNoErr(t, w.Flush(), "flush")

	aiRefuseSecrets(t, buf.String(), map[string]string{
		"argument value": "ai-plaintext-should-not-render",
		"result preview": "ai-preview-should-not-render",
	})
}
