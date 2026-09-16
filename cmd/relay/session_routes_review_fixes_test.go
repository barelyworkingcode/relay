package main

// Regression tests for the adversarial review of R-S4b ("launch-routes"):
// two blocking findings (B1, B2) and four required fixes (R1-R4). Each test
// here is named for the finding it pins and fails against the code as it
// stood before that finding's fix.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

// --- B1: registering the session routes must not reserve the whole
// /api/sessions//api/terminals/ subtree against relay-sessions' own manifest ---

// TestReviewFix_B1_RealFrontendRoutesCoexistWithRelaySessionsManifest drives
// the real NewFrontendServer/registerFrontendRoutes call (not a stripped-down
// fixture with no Reserve wired, which is why B1 was invisible to this
// unit's own test suite) with a genuinely ready sessionRouteDeps, then
// registers relay-sessions' own expected manifest on the SAME registry and
// confirms it still succeeds.
func TestReviewFix_B1_RealFrontendRoutesCoexistWithRelaySessionsManifest(t *testing.T) {
	store := newLaunchTestStore(t)
	extMgr := mcpbroker.NewManager(nil)
	enhanced := NewEnhancedServiceRegistry(nil)
	sessionDeps := sessionRouteDeps{
		store:       store,
		launches:    service.NewLaunches(),
		sessions:    newLaunchTestLedger(t),
		modelKeys:   NewModelKeyTable(),
		enhanced:    enhanced,
		accounting:  newSessionAccounting(),
		resumeGuard: newResumeGuard(),
	}

	sockDir := mkShortTempDir(t, "b1-fe-")
	srv, err := NewFrontendServer(
		store,
		extMgr, extMgr, extMgr,
		seededEndpoint(t, store, filepath.Join(sockDir, "frontend.sock"), "b1-bearer"),
		enhanced,
		nil, // skillLister
		nil, // onProjectsChanged
		nil, // ops
		nil, // enrolmentOps
		nil, // auditOps
		nil, // mcpOps
		nil, // projectOps
		nil, // hostOps
		nil, // eveEnrolmentOps
		nil, // evePasskeyOps
		nil, // authz
		nil, // auditor
		nil, // launches (peer-identity resolution; unused by this test)
		sessionDeps,
	)
	assertNoErr(t, err, "NewFrontendServer")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	// Proof the real registration ran on THIS registry, not a fixture that
	// skipped it: relay's own bare route is genuinely reserved.
	if err := enhanced.RegisterManifest("squatter", "/tmp/squatter.sock", "tok", newManifest("/api/sessions")); err == nil {
		t.Fatal("a manifest claiming /api/sessions, which relay's own session routes reserve, was accepted")
	}

	// The B1 regression itself: relay-sessions' own manifest legitimately
	// claims /api/sessions/ and /api/terminals/ -- the prefixes the resume
	// route's reservation used to swallow whole -- and must still register.
	if err := enhanced.RegisterManifest(config.RelaySessionsServiceID, "/tmp/relaysessions.sock", "tok", fakeSessionsManifest()); err != nil {
		t.Fatalf("relay-sessions' own manifest was refused: %v", err)
	}
}

// --- B2: a concurrent resume race must never let the loser's rollback
// revoke the winner's live model key ---

// modelKeysLiveForTest counts entries currently minted under
// (projectID, label). Same package as ModelKeyTable, so this reaches its
// private map directly -- the winning session's plaintext key is never
// exposed to eve or to this test, by design (see
// TestSessionRoutes_CreateSession_ResponseNeverCarriesSecretOrKey), so
// Lookup's own plaintext-in, bool-out shape cannot answer "is the winner's
// key still there" without it.
func modelKeysLiveForTest(mk *ModelKeyTable, projectID, label string) int {
	mk.mu.Lock()
	defer mk.mu.Unlock()
	n := 0
	for _, rec := range mk.byID {
		if rec.projectID == projectID && rec.label == label {
			n++
		}
	}
	return n
}

// TestReviewFix_B2_ConcurrentResumeDoesNotRevokeWinnersKey races two
// concurrent resumes of the same dormant session against the resumeGuard: one
// passes and reaches AuthorizeLaunch, mints a model key under "session:<id>",
// and dials relay-sessions' /launch; the other is refused by the guard before
// it ever reaches AuthorizeLaunch, so it never mints a second key to begin
// with. The fake host's own 409 (session_exists) handling for a second
// /launch arrival stays wired as a defense-in-depth check: it must never
// actually fire, since the guard is what keeps a second attempt from getting
// that far. After both requests finish, the winner's key must still be the
// only one alive under that label.
func TestReviewFix_B2_ConcurrentResumeDoesNotRevokeWinnersKey(t *testing.T) {
	f := newSessionRoutesFixture(t)
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{
		SessionID: "s1", Kind: KindChat, ProjectID: f.proj.ID, Directory: f.proj.Path, State: ledger.StateDormant,
		SessionRequest: json.RawMessage(`{"projectId":"` + f.proj.ID + `","directory":"` + f.proj.Path + `","model":"gpt-5"}`),
	}), "seed dormant record")

	var mu sync.Mutex
	claimed := false
	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/launch" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			mu.Lock()
			winner := !claimed
			claimed = true
			mu.Unlock()
			if !winner {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"session_exists","message":"session is already live"}`))
				return
			}
			fakeLaunchHandler(&fs, func(id string) string { return `{"session_id":"` + id + `"}` })(w, r)
		},
	})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	// Both requests built up front (withExecuteCredential itself writes to
	// the shared store) so only the actual resume dispatch races.
	req1 := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/resume", nil)
	req1.SetPathValue("id", "s1")
	req1 = f.withExecuteCredential(t, req1)
	req2 := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/resume", nil)
	req2.SetPathValue("id", "s1")
	req2 = f.withExecuteCredential(t, req2)

	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, req := range []*http.Request{req1, req2} {
		wg.Add(1)
		go func(i int, req *http.Request) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			f.mux.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i, req)
	}
	wg.Wait()

	var okCount, conflictCount int
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Fatalf("resume outcomes = %v, want exactly one 200 and one 409 (the resumeGuard refusing the loser)", codes)
	}

	if n := modelKeysLiveForTest(f.deps.modelKeys, f.proj.ID, "session:s1"); n != 1 {
		t.Fatalf("live model keys under (project, session:s1) after both resumes = %d, want exactly 1 -- "+
			"the winner's key must never be silently revoked by the loser's rollback", n)
	}
}

// TestReviewFix_B2_WinningResumeSecretStillBindsAfterConcurrentLoser is the
// deeper half of B2: Launches.Begin's own contract ends whatever launch was
// previously recorded under a name before recording the new one, and both
// concurrent resumes for the same dormant session name their launch identity
// after the session id -- without resumeGuard serializing them, a losing
// attempt's Begin call can end the winning attempt's launch identity, secret
// included, before the winner's own host round trip even finishes, even
// though the LOSING HTTP response is the one relay-sessions itself refused.
// The fake host enforces the same per-session_id dedup C5 documents the real
// host does (first /launch for a session_id wins, a concurrent second gets
// session_exists), so exactly one of the two concurrent resume requests gets
// eve's 200 -- exactly as in production. This races the real resume handler
// the way the reviewer reproduced it, then asserts on the one fact that
// actually matters: the secret relay-sessions received for the WINNING
// (200-response) request still successfully binds afterward, not just that
// one request got 200 and the other got refused.
func TestReviewFix_B2_WinningResumeSecretStillBindsAfterConcurrentLoser(t *testing.T) {
	f := newSessionRoutesFixture(t)
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{
		SessionID: "s1", Kind: KindChat, ProjectID: f.proj.ID, Directory: f.proj.Path, State: ledger.StateDormant,
		SessionRequest: json.RawMessage(`{"projectId":"` + f.proj.ID + `","directory":"` + f.proj.Path + `","model":"gpt-5"}`),
	}), "seed dormant record")

	// A project_session Bind reads the root process's real start time and
	// registers a real ancestry watch; this test process has no pid of its
	// own to stand in for the session's root, so a fake root source/watcher
	// plays that part instead -- same discipline as
	// TestReviewFix_R3_ProjectDeleteRevokesTerminalModelKey's "doomed"
	// session above.
	const rootPID = 424242
	f.deps.launches.SetRootSourceForTest(fakeRootSource{rootPID: membership.ProcInfo{StartSec: 1}})
	f.deps.launches.SetRootWatcherForTest(func(pid int, want membership.ProcInfo, onExit func()) (func(), error) {
		return func() {}, nil
	})

	var mu sync.Mutex
	claimed := false
	var winnerSessionID, winnerSecret string
	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/launch" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			var spec hostapi.LaunchRequest
			_ = json.Unmarshal(fs.LastRequest().Body, &spec)

			mu.Lock()
			winner := !claimed
			claimed = true
			if winner && spec.Identity != nil {
				winnerSessionID, winnerSecret = spec.SessionID, spec.Identity.Secret
			}
			mu.Unlock()

			if !winner {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"session_exists","message":"session is already live"}`))
				return
			}
			fakeLaunchHandler(&fs, func(id string) string { return `{"session_id":"` + id + `"}` })(w, r)
		},
	})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	req1 := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/resume", nil)
	req1.SetPathValue("id", "s1")
	req1 = f.withExecuteCredential(t, req1)
	req2 := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/resume", nil)
	req2.SetPathValue("id", "s1")
	req2 = f.withExecuteCredential(t, req2)

	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, req := range []*http.Request{req1, req2} {
		wg.Add(1)
		go func(i int, req *http.Request) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			f.mux.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i, req)
	}
	wg.Wait()

	okCount := 0
	for _, c := range codes {
		if c == http.StatusOK {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("resume outcomes = %v, want exactly one 200", codes)
	}
	if winnerSecret == "" {
		t.Fatal("no winning /launch call was ever recorded")
	}

	if _, err := f.deps.launches.Bind(winnerSessionID, winnerSecret, peertoken.ForProcessForTest(rootPID, 1)); err != nil {
		t.Fatalf("the winning (200-response) resume's own secret failed to bind afterward: %v", err)
	}
}

// --- R1: trailing-slash create requests must reach the same authorizing
// handler as their non-slash counterparts, never the legacy catch-all ---

// TestReviewFix_R1_TrailingSlashCreateRoutesAreAuthorized uses
// newSessionRoutesFixture's mux, which registers ONLY the session routes
// (no catch-all): if a trailing-slash create fell through the way it did
// before this fix, Go's ServeMux would answer 404 here, not run
// AuthorizeLaunch's own refusal -- so a 403 without a credential, and a 502
// once one is presented but no host is registered, are proof the request
// reached the real handler.
func TestReviewFix_R1_TrailingSlashCreateRoutesAreAuthorized(t *testing.T) {
	f := newSessionRoutesFixture(t)

	for _, path := range []string{"/api/sessions/", "/api/terminals/"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s with no credential: status = %d, body = %s, want 403 (AuthorizeLaunch's own refusal, not a 404 from an unregistered pattern)",
				path, rec.Code, rec.Body.String())
		}
	}

	bodies := map[string]string{
		"/api/sessions/":  `{"projectId":"` + f.proj.ID + `","model":"gpt-5"}`,
		"/api/terminals/": `{"projectId":"` + f.proj.ID + `","templateId":"shell"}`,
	}
	for path, body := range bodies {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req = f.withExecuteCredential(t, req)
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%s with an execute credential and no host registered: status = %d, body = %s, want 502 (reached launchOnHost)",
				path, rec.Code, rec.Body.String())
		}
	}
}

// TestReviewFix_R1_SubtreePOSTsReachManifestProxyNotCreateHandler drives the
// real NewFrontendServer/registerFrontendRoutes call, with relay-sessions'
// own manifest registered as a live fake service, and asserts on the actual
// destination a subtree POST reaches -- not just its status code. An open
// "/api/sessions/" prefix pattern would route "/api/sessions/s1/message" into
// handleCreateSession, minting a new session and dialing /launch, instead of
// letting it fall through to the "/" catch-all and on to relay-sessions.
func TestReviewFix_R1_SubtreePOSTsReachManifestProxyNotCreateHandler(t *testing.T) {
	store := newLaunchTestStore(t)
	enhanced := NewEnhancedServiceRegistry(nil)
	sessionDeps := sessionRouteDeps{
		store:       store,
		launches:    service.NewLaunches(),
		sessions:    newLaunchTestLedger(t),
		modelKeys:   NewModelKeyTable(),
		enhanced:    enhanced,
		accounting:  newSessionAccounting(),
		resumeGuard: newResumeGuard(),
	}

	fs := NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
	})
	assertNoErr(t, enhanced.RegisterManifest(fs.ServiceID(), fs.Socket(), fs.Token(), fs.Manifest()), "RegisterManifest")

	sockDir := mkShortTempDir(t, "r1-fe-")
	const bearer = "r1-bearer"
	extMgr := mcpbroker.NewManager(nil)
	srv, err := NewFrontendServer(
		store,
		extMgr, extMgr, extMgr,
		seededEndpoint(t, store, filepath.Join(sockDir, "frontend.sock"), bearer),
		enhanced,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil,
		sessionDeps,
	)
	assertNoErr(t, err, "NewFrontendServer")
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	client := dialFrontendHTTP(filepath.Join(sockDir, "frontend.sock"))
	get := func(method, path, body string) *http.Response {
		req, err := http.NewRequest(method, "http://unix"+path, strings.NewReader(body))
		assertNoErr(t, err, "build request")
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := client.Do(req)
		assertNoErr(t, err, method+" "+path)
		return resp
	}

	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/sessions/s1/message"},
		{http.MethodPost, "/api/terminals/t1/resize"},
	}
	for _, tc := range cases {
		resp := get(tc.method, tc.path, `{"projectId":"whatever","model":"gpt-5"}`)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var echoed struct {
			Service string `json:"service"`
			Path    string `json:"path"`
		}
		if err := json.Unmarshal(body, &echoed); err != nil {
			t.Fatalf("%s %s: response was not relay-sessions' own echo body (status %d, body %s): %v",
				tc.method, tc.path, resp.StatusCode, body, err)
		}
		if echoed.Service != config.RelaySessionsServiceID || echoed.Path != tc.path {
			t.Fatalf("%s %s: reached service %q path %q, want relay-sessions at the original path",
				tc.method, tc.path, echoed.Service, echoed.Path)
		}
	}

	for _, req := range fs.Requests() {
		if req.Path == "/launch" {
			t.Fatalf("a subtree POST reached relay-sessions' /launch: %+v", req)
		}
	}
}

// --- R2: a project delete racing an in-flight launch must never leave a
// session live and tracked against the deleted project id ---

// TestReviewFix_R2_ProjectDeletedDuringCommitAbortsLaunch simulates the
// race directly: AuthorizeLaunch succeeds against a real project, the
// project is then deleted (as a concurrent ProjectOps.Remove would), and
// only then does the host round trip and commitLaunch run -- the exact
// window between AuthorizeLaunch's project-existence check and
// commitLaunch's ledger write that a concurrent delete's own cleanup sweep
// cannot see, because it already ran and found nothing.
func TestReviewFix_R2_ProjectDeletedDuringCommitAbortsLaunch(t *testing.T) {
	f := newSessionRoutesFixture(t)

	var terminateReqs []string
	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/launch":
				fakeLaunchHandler(&fs, func(id string) string { return `{"session_id":"` + id + `"}` })(w, r)
			case "/terminate":
				terminateReqs = append(terminateReqs, string(fs.LastRequest().Body))
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		},
	})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	caller := LaunchCaller{Credential: &config.APICredential{ID: "r2-exec", Classes: []control.CapabilityClass{control.ClassExecute}}}
	req := LaunchRequest{Caller: caller, ProjectID: f.proj.ID, Kind: KindChat, Model: "gpt-5"}
	result, refusal := AuthorizeLaunch(f.store, f.deps.modelKeys, f.deps.sessions, req)
	if refusal != nil {
		t.Fatalf("AuthorizeLaunch refused: %+v", refusal)
	}

	// The race: the project vanishes after authorization succeeded but
	// before the host round trip (and therefore commitLaunch) runs.
	assertNoErr(t, f.store.With(func(s *config.Settings) { s.RemoveProject(f.proj.ID) }), "remove project")

	ctx := context.Background()
	if _, err := f.deps.launchOnHost(ctx, result); err != nil {
		t.Fatalf("launchOnHost: %v", err)
	}

	if f.deps.commitLaunch(ctx, result) {
		t.Fatal("commitLaunch reported success for a session whose project no longer exists")
	}

	if rec, ok := f.deps.sessions.Get(result.SessionID); ok {
		t.Fatalf("a session for a deleted project ended up live in the ledger: %+v", rec)
	}
	if _, ok := f.deps.accounting.take(result.SessionID); ok {
		t.Fatal("a session for a deleted project survived in relay's own accounting")
	}
	if len(terminateReqs) != 1 || !strings.Contains(terminateReqs[0], `"session_id":"`+result.SessionID+`"`) {
		t.Fatalf("/terminate requests = %+v, want exactly one naming %s", terminateReqs, result.SessionID)
	}
}

// --- R3: deleting a project must revoke a live terminal's model key too,
// not only claude/pi/chat sessions the ledger tracks ---

// TestReviewFix_R3_ProjectDeleteRevokesTerminalModelKey creates a pty
// session from the built-in "pi" template (the one built-in template with
// ModelKey: true), confirms it minted a live key, deletes the owning
// project, and confirms the key is gone -- the ledger never held this
// session (terminals are never persisted, C5), so only cleanupProject's own
// accounting sweep can find it.
func TestReviewFix_R3_ProjectDeleteRevokesTerminalModelKey(t *testing.T) {
	f := newSessionRoutesFixture(t)
	t.Setenv("SHELL", "")

	var terminateReqs []string
	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/launch":
				fakeLaunchHandler(&fs, func(id string) string {
					return `{"terminalId":"` + id + `","templateId":"pi","name":"","directory":"` + f.proj.Path + `","host":null}`
				})(w, r)
			case "/terminate":
				terminateReqs = append(terminateReqs, string(fs.LastRequest().Body))
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		},
	})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	reqBody := `{"templateId":"pi","name":"t","directory":"","projectId":"` + f.proj.ID + `","cols":80,"rows":24}`
	req := httptest.NewRequest(http.MethodPost, "/api/terminals", strings.NewReader(reqBody))
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", rec.Code, rec.Body.String())
	}

	var sentSpec hostapi.LaunchRequest
	assertNoErr(t, json.Unmarshal(fs.LastRequest().Body, &sentSpec), "unmarshal spec relay-sessions received")
	if sentSpec.ModelKey == "" {
		t.Fatalf("the pi template's ModelKey flag did not mint a key: %+v", sentSpec)
	}
	sessionID, key := sentSpec.SessionID, sentSpec.ModelKey
	if _, _, ok := f.deps.modelKeys.Lookup(key); !ok {
		t.Fatal("model key is not live immediately after create")
	}

	ops := &ProjectOps{Store: f.store, SessionCleanup: f.deps}
	_, found, err := ops.Remove(f.proj.ID)
	assertNoErr(t, err, "Remove")
	if !found {
		t.Fatal("Remove reported the project as not found")
	}

	if _, _, ok := f.deps.modelKeys.Lookup(key); ok {
		t.Fatal("a deleted project's terminal model key survived Remove")
	}
	if _, ok := f.deps.launches.Bound(sessionID); ok {
		t.Fatal("a deleted project's terminal launch identity survived Remove")
	}
	if len(terminateReqs) != 1 || !strings.Contains(terminateReqs[0], `"session_id":"`+sessionID+`"`) {
		t.Fatalf("/terminate requests = %+v, want exactly one naming %s", terminateReqs, sessionID)
	}
}

// --- R4: a ledger-open failure must fail closed onto these routes' own
// 503, never fall through to the unauthorized legacy catch-all ---

// TestReviewFix_R4_LedgerUnavailableFailsClosedNotToCatchAll simulates
// trayapp.go's own "ledger.Open failed" state (sessions left nil, every
// other dependency wired) directly against RegisterSessionRoutes, with a
// catch-all registered on the same mux the way registerFrontendRoutes
// always mounts one: every session route must answer its own 503, and the
// catch-all must never see any of these requests.
func TestReviewFix_R4_LedgerUnavailableFailsClosedNotToCatchAll(t *testing.T) {
	store := newLaunchTestStore(t)
	deps := sessionRouteDeps{
		store:       store,
		launches:    service.NewLaunches(),
		sessions:    nil, // the ledger-open failure this test simulates
		modelKeys:   NewModelKeyTable(),
		enhanced:    NewEnhancedServiceRegistry(nil),
		accounting:  newSessionAccounting(),
		resumeGuard: newResumeGuard(),
	}
	if !deps.ready() {
		t.Fatal("sessionRouteDeps.ready() must be true with only the ledger missing, or these routes never register at all")
	}

	mux := http.NewServeMux()
	rr := &control.RouteRegistrar{Mux: mux, Transport: control.TransportSocket}
	RegisterSessionRoutes(rr, deps)

	catchAllHit := false
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		catchAllHit = true
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/sessions"},
		{http.MethodPost, "/api/sessions/"},
		{http.MethodPost, "/api/terminals"},
		{http.MethodPost, "/api/terminals/"},
		{http.MethodPost, "/api/sessions/s1/resume"},
		{http.MethodGet, "/api/sessions"},
		{http.MethodGet, "/api/terminals"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.SetPathValue("id", "s1")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status = %d, body = %s, want 503", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	if catchAllHit {
		t.Fatal("a session route fell through to the catch-all while the ledger was unavailable")
	}
}
