package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

// fakeRootSource is a fixed pid -> ProcInfo membership.Source: the hermetic
// stand-in a project_session Bind needs for a pid this test binary does not
// actually have (internal/service's own launch_identity_test.go uses an
// identical shape, kept private to that package's tests, hence the small
// duplicate here rather than an import that test files cannot cross).
type fakeRootSource map[int]membership.ProcInfo

func (f fakeRootSource) Info(pid int) (membership.ProcInfo, bool) {
	info, ok := f[pid]
	return info, ok
}

// --- shared fixture plumbing -------------------------------------------------

// sessionRoutesFixture bundles everything a session_routes.go handler test
// needs: a real store/project, a real (in-memory) ledger, a real launch
// table, and the mux RegisterSessionRoutes wires its handlers onto.
type sessionRoutesFixture struct {
	store config.SettingsStore
	proj  config.Project
	deps  sessionRouteDeps
	mux   *http.ServeMux
}

// newSessionRoutesFixture builds deps and registers them onto a fresh mux
// once. auditor is optional (most tests don't inspect it): sessionRouteDeps'
// handler methods have value receivers, so a route bound via rr.Handle
// closes over a COPY of deps taken at registration time -- an auditor must
// be supplied here, not assigned onto the returned fixture's deps field
// afterward, or the already-registered handlers would never see it.
func newSessionRoutesFixture(t *testing.T, auditor ...*audit.AuditRecorder) *sessionRoutesFixture {
	t.Helper()
	store := newLaunchTestStore(t)
	proj := addLaunchTestProject(t, store, nil)
	var rec *audit.AuditRecorder
	if len(auditor) > 0 {
		rec = auditor[0]
	}
	deps := sessionRouteDeps{
		store:       store,
		launches:    service.NewLaunches(),
		sessions:    newLaunchTestLedger(t),
		modelKeys:   NewModelKeyTable(),
		enhanced:    NewEnhancedServiceRegistry(nil),
		auditor:     rec,
		accounting:  newSessionAccounting(),
		resumeGuard: newResumeGuard(),
	}
	mux := http.NewServeMux()
	rr := &control.RouteRegistrar{Mux: mux, Transport: control.TransportSocket}
	RegisterSessionRoutes(rr, deps)
	return &sessionRoutesFixture{store: store, proj: proj, deps: deps, mux: mux}
}

// withExecuteCredential mints a real APICredential holding ClassExecute and
// attaches its id to r's context the same way credentialAuthorizer.Authorize
// does after resolving a bearer -- this package's handlers only ever read
// that context, never re-authenticate, so this is what a real gated request
// looks like from their point of view.
func (f *sessionRoutesFixture) withExecuteCredential(t *testing.T, r *http.Request) *http.Request {
	t.Helper()
	const credID = "cred-exec"
	if findAPICredential(config.FreshSettings(f.store), credID) == nil {
		assertNoErr(t, f.store.With(func(s *config.Settings) {
			s.APICredentials = append(s.APICredentials, config.APICredential{
				ID: credID, Name: "test", Classes: []control.CapabilityClass{control.ClassExecute},
				Hash: config.HashToken("exec-token-" + credID),
			})
		}), "seed execute credential")
	}
	return r.WithContext(withAPICredentialID(r.Context(), credID))
}

// registerFakeSessionsHost registers fs as the relaysessions service on
// f.deps.enhanced and binds a "relaysessions" service launch identity on
// f.deps.launches to peer -- the two independent facts sessionhost_client.go
// checks before ever writing a byte to the wire (C5's "relay side" mutual
// check).
func (f *sessionRoutesFixture) registerFakeSessionsHost(t *testing.T, fs *FakeService, peer peertoken.Process) {
	t.Helper()
	assertNoErr(t, f.deps.enhanced.RegisterManifest(fs.ServiceID(), fs.Socket(), fs.Token(), fs.Manifest()), "RegisterManifest")
	secret, _, err := f.deps.launches.Begin(service.Identity{
		Kind: service.IdentityKindService, Name: config.RelaySessionsServiceID,
		Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilitySessions},
	})
	assertNoErr(t, err, "Begin relaysessions launch")
	_, err = f.deps.launches.Bind(config.RelaySessionsServiceID, secret, peertoken.ForProcessForTest(peer.PID, peer.PIDVersion))
	assertNoErr(t, err, "Bind relaysessions launch")
}

func fakeSessionsManifest() bridge.Manifest {
	return bridge.Manifest{Routes: []string{"/api/terminals/", "/api/sessions/"}}
}

// fakeLaunchHandler answers /launch 201 with C5's shape, using whatever
// session_id the incoming spec named -- relay mints it, so a fake host must
// read it back off the request rather than hardcoding one. It reads the
// request body from fsPtr's own recorded request rather than r.Body: the
// FakeService mux wrapper (support_test.go) already drains r.Body into
// fs.requests before calling this handler, so a second read here would see
// nothing but EOF.
func fakeLaunchHandler(fsPtr **FakeService, bodyFor func(sessionID string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := (*fsPtr).LastRequest().Body
		var spec hostapi.LaunchRequest
		_ = json.Unmarshal(data, &spec)
		resp := hostapi.LaunchResponse{
			SessionID: spec.SessionID, RootPID: 4242,
			Body: json.RawMessage(bodyFor(spec.SessionID)),
		}
		out, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(out)
	}
}

// --- golden LaunchSpec --------------------------------------------------

// TestSessionRoutes_CreateTerminal_GoldenLaunchSpec drives a full
// POST /api/terminals through the real handler against a fake relaysessions
// host, and asserts on the EXACT bytes of the LaunchRequest it received
// (secret excluded, since it's random by construction) -- a true golden
// comparison, not just a status code.
func TestSessionRoutes_CreateTerminal_GoldenLaunchSpec(t *testing.T) {
	f := newSessionRoutesFixture(t)
	t.Setenv("SHELL", "")

	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: fakeLaunchHandler(&fs, func(id string) string {
			return `{"terminalId":"` + id + `","templateId":"shell","name":"term 1","directory":"` + f.proj.Path + `","host":null}`
		}),
	})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	reqBody := `{"templateId":"shell","name":"term 1","directory":"","projectId":"` + f.proj.ID + `","cols":100,"rows":30}`
	req := httptest.NewRequest(http.MethodPost, "/api/terminals", strings.NewReader(reqBody))
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	gotBody := fs.LastRequest().Body
	var gotSpec hostapi.LaunchRequest
	assertNoErr(t, json.Unmarshal(gotBody, &gotSpec), "unmarshal spec relay-sessions received")
	if gotSpec.Identity == nil || len(gotSpec.Identity.Secret) != 64 {
		t.Fatalf("Spec.Identity = %+v, want a 64-hex secret", gotSpec.Identity)
	}
	secretSent := gotSpec.Identity.Secret
	gotSpec.Identity = nil // compared separately above; the rest must be byte-exact

	wantDir, _ := filepath.EvalSymlinks(f.proj.Path)
	projJSON, _ := json.Marshal(struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Path string `json:"path"`
	}{f.proj.ID, f.proj.Name, f.proj.Path})
	want := hostapi.LaunchRequest{
		V: 1, SessionID: gotSpec.SessionID, Kind: KindPTY, Resume: false,
		Project: projJSON, Directory: wantDir, Name: "term 1", TemplateID: "shell",
		Argv: []string{"/bin/zsh"}, IdleTimeoutSec: 1440 * 60,
		PTY: &hostapi.PTYSpec{Cols: 100, Rows: 30},
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(gotSpec)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("golden LaunchSpec mismatch (secret excluded from comparison):\n got  %s\n want %s", gotJSON, wantJSON)
	}

	if strings.Contains(rec.Body.String(), secretSent) {
		t.Fatal("eve's 201 response leaked the launch secret")
	}
}

// --- host peer mismatch: no secret written ----------------------------------

// TestSessionRoutes_HostPeerMismatch_NoSecretWritten is the required
// "host peer mismatch -> no secret written" test: relaysessions' manifest
// registration names a real, reachable socket, but the bound launch
// identity's process does NOT match whoever is actually listening on it
// (the test binary itself, via the fake service). The dial must be refused
// before a single byte -- let alone the identity secret -- reaches the wire.
func TestSessionRoutes_HostPeerMismatch_NoSecretWritten(t *testing.T) {
	f := newSessionRoutesFixture(t)

	fs := NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
	})
	self := selfPeerToken(t)
	// Bound to a process that is NOT the one actually listening on fs's
	// socket (that's this test binary, i.e. self) -- the exact shape
	// model_endpoint_test.go's own router-socket-mismatch test uses.
	mismatched := peertoken.ForProcessForTest(self.PID()+123456, self.PIDVersion()+7).Process()
	f.registerFakeSessionsHost(t, fs, mismatched)

	reqBody := `{"templateId":"shell","name":"t","directory":"","projectId":"` + f.proj.ID + `","cols":80,"rows":24}`
	req := httptest.NewRequest(http.MethodPost, "/api/terminals", strings.NewReader(reqBody))
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", rec.Code, rec.Body.String())
	}
	if got := fs.Requests(); len(got) != 0 {
		t.Fatalf("fake host received %d request(s) despite a peer mismatch: %+v", len(got), got)
	}
}

// --- dispatcher cannot reach /launch -----------------------------------

// TestSessionRoutes_NoRelaySessionsRegistered_502sWithoutPanicking covers
// "dispatcher cannot reach /launch": nothing has registered as
// relaysessions at all, so sessionhost_client.go's resolve() must fail
// closed rather than dial anything.
func TestSessionRoutes_NoRelaySessionsRegistered_502s(t *testing.T) {
	f := newSessionRoutesFixture(t)

	reqBody := `{"templateId":"shell","name":"t","directory":"","projectId":"` + f.proj.ID + `","cols":80,"rows":24}`
	req := httptest.NewRequest(http.MethodPost, "/api/terminals", strings.NewReader(reqBody))
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", rec.Code, rec.Body.String())
	}
}

// A manifest can never claim /launch in the first place (internal/bridge's
// own TestManifestValidate_SessionHostRoutesRejected covers the validation
// rule); here the registry-level lookup a real request would use confirms
// the same route is simply never found even when relaysessions IS registered
// with its real (non-reserved) routes.
func TestSessionRoutes_LookupByPath_NeverReachesLaunchOrTerminate(t *testing.T) {
	f := newSessionRoutesFixture(t)
	fs := NewFakeService(t, FakeServiceOptions{ServiceID: config.RelaySessionsServiceID, Manifest: fakeSessionsManifest()})
	assertNoErr(t, f.deps.enhanced.RegisterManifest(fs.ServiceID(), fs.Socket(), fs.Token(), fs.Manifest()), "RegisterManifest")

	for _, path := range []string{"/launch", "/terminate"} {
		if svc := f.deps.enhanced.LookupByPath(path); svc != nil {
			t.Fatalf("LookupByPath(%q) resolved to service %q; it must never be reachable via the front-door dispatcher", path, svc.ServiceID)
		}
	}
}

// --- eve response never carries a secret or a key ---------------------------

// TestSessionRoutes_CreateSession_ResponseNeverCarriesSecretOrKey drives a
// pi session create (which mints both an identity secret and an rmk_ model
// key) and asserts neither ever appears in eve's response bytes, and that
// the ledger + accounting bookkeeping is exactly what a successful launch
// should leave behind.
func TestSessionRoutes_CreateSession_ResponseNeverCarriesSecretOrKey(t *testing.T) {
	f := newSessionRoutesFixture(t)

	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: fakeLaunchHandler(&fs, func(id string) string {
			return `{"sessionId":"` + id + `","directory":"` + f.proj.Path + `","model":"gpt-5","name":"","host":null}`
		}),
	})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	reqBody := `{"projectId":"` + f.proj.ID + `","directory":"","name":"","model":"gpt-5","settings":null,"systemPrompt":"","appendClaudeMd":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(reqBody))
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var sentSpec hostapi.LaunchRequest
	assertNoErr(t, json.Unmarshal(fs.LastRequest().Body, &sentSpec), "unmarshal spec relay-sessions received")
	if sentSpec.Identity == nil || sentSpec.ModelKey == "" {
		t.Fatalf("pi session spec did not carry an identity+model_key: %+v", sentSpec)
	}
	if strings.Contains(rec.Body.String(), sentSpec.Identity.Secret) {
		t.Fatal("eve's response leaked the launch secret")
	}
	if strings.Contains(rec.Body.String(), sentSpec.ModelKey) || strings.Contains(rec.Body.String(), "rmk_") {
		t.Fatal("eve's response leaked the model key")
	}

	rec2, ok := f.deps.sessions.Get(sentSpec.SessionID)
	if !ok || rec2.State != ledger.StateLive {
		t.Fatalf("ledger record after a successful pi launch = %+v, ok=%v", rec2, ok)
	}
	if acc, ok := f.deps.accounting.take(sentSpec.SessionID); !ok || acc.launch == nil || acc.modelKeyLabel == "" {
		t.Fatalf("session accounting after a successful pi launch = %+v, ok=%v", acc, ok)
	}
}

// --- resume: dormant / live / unknown / narrowed ---------------------------

func TestSessionRoutes_Resume_DormantSucceeds(t *testing.T) {
	f := newSessionRoutesFixture(t)
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{
		SessionID: "s1", Kind: KindChat, ProjectID: f.proj.ID, Directory: f.proj.Path, State: ledger.StateDormant,
		SessionRequest: json.RawMessage(`{"projectId":"` + f.proj.ID + `","directory":"` + f.proj.Path + `","model":"gpt-5"}`),
	}), "seed dormant record")

	var launchCalls int
	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			launchCalls++
			fakeLaunchHandler(&fs, func(id string) string { return `{"sessionId":"` + id + `"}` })(w, r)
		},
	})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/resume", nil)
	req.SetPathValue("id", "s1")
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var body resumeResponseBody
	assertNoErr(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode resume response")
	if body.SessionID != "s1" || !body.Resumed {
		t.Fatalf("resume response = %+v, want {s1 true}", body)
	}
	if launchCalls != 1 {
		t.Fatalf("host /launch calls = %d, want 1", launchCalls)
	}
	if got, ok := f.deps.sessions.Get("s1"); !ok || got.State != ledger.StateLive {
		t.Fatalf("ledger record after resume = %+v, ok=%v, want live", got, ok)
	}
}

func TestSessionRoutes_Resume_AlreadyLiveReturnsFalseWithoutContactingHost(t *testing.T) {
	f := newSessionRoutesFixture(t)
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{SessionID: "s1", Kind: KindChat, ProjectID: f.proj.ID, State: ledger.StateLive}), "seed live record")

	fs := NewFakeService(t, FakeServiceOptions{ServiceID: config.RelaySessionsServiceID, Manifest: fakeSessionsManifest()})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/resume", nil)
	req.SetPathValue("id", "s1")
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var body resumeResponseBody
	assertNoErr(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode resume response")
	if body.SessionID != "s1" || body.Resumed {
		t.Fatalf("resume response = %+v, want {s1 false}", body)
	}
	if got := fs.Requests(); len(got) != 0 {
		t.Fatalf("an already-live resume dialed the host: %+v", got)
	}
}

func TestSessionRoutes_Resume_UnknownSessionIs404(t *testing.T) {
	f := newSessionRoutesFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/does-not-exist/resume", nil)
	req.SetPathValue("id", "does-not-exist")
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
}

func TestSessionRoutes_Resume_DeletedSessionIs404(t *testing.T) {
	f := newSessionRoutesFixture(t)
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{SessionID: "s1", Kind: KindChat, ProjectID: f.proj.ID, State: ledger.StateDormant}), "seed dormant record")
	assertNoErr(t, f.deps.sessions.Remove("s1"), "delete record")

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/resume", nil)
	req.SetPathValue("id", "s1")
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s, want 404", rec.Code, rec.Body.String())
	}
}

// A dormant record naming a project that no longer exists ("narrowed" per
// the plan: SH §3.2's re-check against the CURRENT project catches this the
// same way a fresh launch's project-not-found check does) is refused 403,
// not 200 or 404 -- the session id is real, the project it depended on is
// not.
func TestSessionRoutes_Resume_ProjectGoneRefuses403(t *testing.T) {
	f := newSessionRoutesFixture(t)
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{
		SessionID: "s1", Kind: KindChat, ProjectID: "deleted-project", State: ledger.StateDormant,
	}), "seed dormant record for a gone project")

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/resume", nil)
	req.SetPathValue("id", "s1")
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s, want 403", rec.Code, rec.Body.String())
	}
}

// --- audit: session_launch, session_resume ----------------------------------

func TestSessionRoutes_Create_AuditsSessionLaunchOnSuccessAndFailure(t *testing.T) {
	f := newSessionRoutesFixture(t, newTestAudit(t, nil))

	// Failure first: no relaysessions registered at all.
	req := httptest.NewRequest(http.MethodPost, "/api/terminals", strings.NewReader(
		`{"templateId":"shell","name":"t","directory":"","projectId":"`+f.proj.ID+`","cols":80,"rows":24}`))
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID, Manifest: fakeSessionsManifest(),
		Handler: fakeLaunchHandler(&fs, func(id string) string { return `{"terminalId":"` + id + `"}` }),
	})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	req2 := httptest.NewRequest(http.MethodPost, "/api/terminals", strings.NewReader(
		`{"templateId":"shell","name":"t","directory":"","projectId":"`+f.proj.ID+`","cols":80,"rows":24}`))
	req2 = f.withExecuteCredential(t, req2)
	rec2 := httptest.NewRecorder()
	f.mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", rec2.Code, rec2.Body.String())
	}

	got := readLoggedEvents(t, f.deps.auditor)
	var errorCount, okCount int
	for _, ev := range got {
		if ev.Event != "session_launch" {
			t.Fatalf("unexpected event kind %q in a create-only test", ev.Event)
		}
		switch ev.Outcome {
		case "error":
			errorCount++
		case "ok":
			okCount++
		}
	}
	if errorCount != 1 || okCount != 1 {
		t.Fatalf("session_launch events = %+v, want exactly one error and one ok", got)
	}
}

func TestSessionRoutes_Resume_AuditsSessionResumeNotSessionLaunch(t *testing.T) {
	f := newSessionRoutesFixture(t, newTestAudit(t, nil))

	req := httptest.NewRequest(http.MethodPost, "/api/sessions/unknown-id/resume", nil)
	req.SetPathValue("id", "unknown-id")
	req = f.withExecuteCredential(t, req)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	got := readLoggedEvents(t, f.deps.auditor)
	if len(got) != 1 || got[0].Event != "session_resume" {
		t.Fatalf("events = %+v, want exactly one session_resume record", got)
	}
	if got[0].Outcome != "not_found" {
		t.Fatalf("outcome = %q, want not_found", got[0].Outcome)
	}
}

// --- project delete cleanup --------------------------------------------

// TestProjectOps_Remove_TerminatesLiveSessionsAndCleansUpBookkeeping is
// item 7's own required behavior: deleting a project /terminate's every
// live session the ledger knows about for it, ends that session's launch
// identity and revokes its model key, removes the ledger record, and never
// touches a live session belonging to a DIFFERENT project.
func TestProjectOps_Remove_TerminatesLiveSessionsAndCleansUpBookkeeping(t *testing.T) {
	f := newSessionRoutesFixture(t)
	other := addLaunchTestProject(t, f.store, func(p *config.Project) { p.ID = "other-project" })

	var terminateReqs []string
	var fs *FakeService
	fs = NewFakeService(t, FakeServiceOptions{
		ServiceID: config.RelaySessionsServiceID,
		Manifest:  fakeSessionsManifest(),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/terminate" {
				terminateReqs = append(terminateReqs, string(fs.LastRequest().Body))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		},
	})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	// A live session belonging to the project about to be deleted, with a
	// real minted identity and model key tracked exactly as a successful
	// create would leave them. Its Bind needs its OWN process, distinct from
	// the one relaysessions itself is bound to (Launches allows at most one
	// identity per process) -- a fake root source/watcher stands in for the
	// real kernel calls a project_session Bind would otherwise make against
	// a pid this test binary does not actually have.
	f.deps.launches.SetRootSourceForTest(fakeRootSource{555555: membership.ProcInfo{StartSec: 1}})
	f.deps.launches.SetRootWatcherForTest(func(pid int, want membership.ProcInfo, onExit func()) (func(), error) {
		return func() {}, nil
	})
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{SessionID: "doomed", Kind: KindPi, ProjectID: f.proj.ID, State: ledger.StateLive}), "seed doomed session")
	key, err := f.deps.modelKeys.Mint(f.proj.ID, "session:doomed")
	assertNoErr(t, err, "Mint")
	secret, launch, err := f.deps.launches.Begin(service.Identity{
		Kind: service.IdentityKindProjectSession, Name: "doomed",
		ProjectID: f.proj.ID, SessionID: "doomed", ParentLaunch: config.RelaySessionsServiceID,
	})
	assertNoErr(t, err, "Begin")
	_, err = f.deps.launches.Bind("doomed", secret, peertoken.ForProcessForTest(555555, 1))
	assertNoErr(t, err, "Bind")
	f.deps.accounting.track("doomed", sessionAccount{projectID: f.proj.ID, modelKeyLabel: "session:doomed", launch: launch})

	// A live session belonging to a DIFFERENT project: must survive intact.
	assertNoErr(t, f.deps.sessions.Put(ledger.Record{SessionID: "survivor", Kind: KindPi, ProjectID: other.ID, State: ledger.StateLive}), "seed survivor session")
	survivorKey, err := f.deps.modelKeys.Mint(other.ID, "session:survivor")
	assertNoErr(t, err, "Mint survivor key")

	ops := &ProjectOps{Store: f.store, SessionCleanup: f.deps}
	_, found, err := ops.Remove(f.proj.ID)
	assertNoErr(t, err, "Remove")
	if !found {
		t.Fatal("Remove reported the project as not found")
	}

	if len(terminateReqs) != 1 || !strings.Contains(terminateReqs[0], `"session_id":"doomed"`) || !strings.Contains(terminateReqs[0], `"reason":"project_deleted"`) {
		t.Fatalf("/terminate requests = %+v, want exactly one naming doomed/project_deleted", terminateReqs)
	}
	if _, ok := f.deps.sessions.Get("doomed"); ok {
		t.Fatal("the deleted project's ledger record survived Remove")
	}
	if _, ok := f.deps.accounting.take("doomed"); ok {
		t.Fatal("the deleted project's session accounting entry survived Remove")
	}
	if _, _, ok := f.deps.modelKeys.Lookup(key); ok {
		t.Fatal("the deleted project's model key survived Remove")
	}
	if _, ok := f.deps.launches.Bound("doomed"); ok {
		t.Fatal("the deleted project's launch identity survived Remove")
	}

	// The other project's session is untouched.
	if rec, ok := f.deps.sessions.Get("survivor"); !ok || rec.State != ledger.StateLive {
		t.Fatalf("an unrelated project's live session was disturbed: %+v, ok=%v", rec, ok)
	}
	if _, _, ok := f.deps.modelKeys.Lookup(survivorKey); !ok {
		t.Fatal("an unrelated project's model key was revoked by another project's delete")
	}
}

// --- bare GET list proxy (SP6) ------------------------------------------

func TestSessionRoutes_GetTerminals_ProxiesToHostByServiceID(t *testing.T) {
	f := newSessionRoutesFixture(t)
	fs := NewFakeService(t, FakeServiceOptions{ServiceID: config.RelaySessionsServiceID, Manifest: fakeSessionsManifest()})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	req := httptest.NewRequest(http.MethodGet, "/api/terminals", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	got := fs.Requests()
	if len(got) != 1 || got[0].Path != "/api/terminals" || got[0].Method != http.MethodGet {
		t.Fatalf("fake host requests = %+v, want exactly one GET /api/terminals", got)
	}
}

func TestSessionRoutes_GetSessions_ProxiesToHostByServiceID(t *testing.T) {
	f := newSessionRoutesFixture(t)
	fs := NewFakeService(t, FakeServiceOptions{ServiceID: config.RelaySessionsServiceID, Manifest: fakeSessionsManifest()})
	self := selfPeerToken(t)
	f.registerFakeSessionsHost(t, fs, self.Process())

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	got := fs.Requests()
	if len(got) != 1 || got[0].Path != "/api/sessions" || got[0].Method != http.MethodGet {
		t.Fatalf("fake host requests = %+v, want exactly one GET /api/sessions", got)
	}
}

func TestSessionRoutes_GetTerminals_NoHostRegistered503s(t *testing.T) {
	f := newSessionRoutesFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/terminals", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", rec.Code, rec.Body.String())
	}
}
