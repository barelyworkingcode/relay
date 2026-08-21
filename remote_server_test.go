package main

// RemoteServer: the mTLS listener a client on another machine reaches relay
// through (ADR-010 decisions 1, 2, 3-point-3, 4, 8 and 9).
//
// Every test here is hermetic and binds 127.0.0.1:0, reading the assigned port
// back off the listener — a remote project is reachable from loopback exactly
// as it is from another machine, because remoteness is a property of the
// grant's shape and not of the caller's location, so the whole model is
// exercised on one box with every guard live.

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"relaygo/bridge"
	"relaygo/mcp"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

type remoteFixture struct {
	t       *testing.T
	dir     string
	store   SettingsStore
	router  *appRouter
	audit   *AuditRecorder
	server  *RemoteServer
	project Project
	bundle  *enrolmentBundle
	// mcpCalls counts how many times a tool actually reached the (mock) MCP.
	// Several tests assert on it being zero: a refusal that happens after the
	// mailbox was read is not a refusal. Atomic because it is incremented on a
	// connection-handler goroutine and read on the test's.
	mcpCalls atomic.Int32
}

type remoteFixtureOpts struct {
	// listen overrides the configured listen address. Empty means
	// 127.0.0.1:0.
	listen string
	// noRemoteBlock omits the "remote" settings block entirely.
	noRemoteBlock bool
	// enabled, when non-nil, is written as remote.enabled verbatim; omitEnabled
	// leaves the key out entirely, so a test can distinguish all three of
	// absent / false / true.
	enabled     *bool
	omitEnabled bool
	// disableAudit builds the router with no audit recorder at all, which is
	// what audit.enabled:false produces (NewAuditRecorder returns nil).
	disableAudit bool
	// extraProjects creates additional remote projects the enrolment does not
	// grant, so a test can widen or misname a grant.
	extraProjects int
	// skipServe leaves the server unstarted so a test can assert on
	// construction alone.
	skipServe bool
	// budget, when non-nil, is stored on the enrolment instead of the
	// conservative defaults, so a test can drive throttling deterministically
	// without making hundreds of calls.
	budget *EnrolmentBudget
}

// newRemoteFixture builds a sandboxed relay install with one remote project,
// one enrolment granting it, a mock MCP, and a started RemoteServer.
func newRemoteFixture(t *testing.T, opts remoteFixtureOpts) *remoteFixture {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")

	f := &remoteFixture{t: t, dir: dir, store: store}

	listen := opts.listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	enabled := opts.enabled
	if enabled == nil && !opts.noRemoteBlock && !opts.omitEnabled {
		enabled = ptr(true)
	}

	var createErr error
	assertNoErr(t, store.With(func(s *Settings) {
		s.ExternalMcps = append(s.ExternalMcps, ExternalMcp{ID: "macmcp", DisplayName: "macMCP"})
		if !opts.noRemoteBlock {
			s.Remote = &RemoteConfig{Enabled: enabled, Listen: listen}
		}
		f.project, createErr = s.CreateProjectWithTokenKind(
			ProjectKindRemote, "Mail", "", []string{"macmcp"}, []string{}, nil, nil)
		for i := 0; i < opts.extraProjects; i++ {
			if _, err := s.CreateProjectWithTokenKind(
				ProjectKindRemote, fmt.Sprintf("Extra %d", i), "", []string{"macmcp"}, []string{}, nil, nil); err != nil {
				createErr = err
			}
		}
	}), "seed settings")
	assertNoErr(t, createErr, "create remote project")

	mgr := NewExternalMcpManager(nil)
	addMockConn(mgr, "macmcp", newMockConn("macmcp", simpleTools("mail_search"),
		func(context.Context, string, interface{}) (json.RawMessage, error) {
			f.mcpCalls.Add(1)
			return json.RawMessage(`{"content":[{"type":"text","text":"3 messages"}]}`), nil
		}))

	if !opts.disableAudit {
		f.audit = newTestAudit(t, nil)
	}
	f.router = &appRouter{
		store:    store,
		tools:    mgr,
		services: &fakeServiceReloader{},
		enhanced: NewEnhancedServiceRegistry(nil),
		audit:    f.audit,
	}

	grants := []string{f.project.ID}
	req := enrolmentRequest{ClientID: "hermes-mail", ProjectIDs: grants}
	if opts.budget != nil {
		req.Budget = *opts.budget
	}
	bundle, err := createEnrolment(store, req)
	assertNoErr(t, err, "createEnrolment")
	f.bundle = bundle

	if opts.skipServe {
		return f
	}
	f.start()
	return f
}

// start binds and serves, failing the test if construction refused.
func (f *remoteFixture) start() {
	f.t.Helper()
	rs, err := NewRemoteServer(context.Background(), f.store, f.router, f.audit)
	assertNoErr(f.t, err, "NewRemoteServer")
	if rs == nil {
		f.t.Fatal("NewRemoteServer returned no listener for an enabled remote block")
	}
	f.server = rs
	go rs.Serve()
	f.t.Cleanup(rs.Close)
}

// caPool returns a pool holding relay's CA — what a client verifies the server
// with, exactly as shipped in an enrolment bundle.
func (f *remoteFixture) caPool() *x509.CertPool {
	f.t.Helper()
	pem, err := os.ReadFile(f.bundle.CACertPath)
	assertNoErr(f.t, err, "read ca.crt from bundle")
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		f.t.Fatal("bundle ca.crt is not a usable certificate")
	}
	return pool
}

// dial connects with the enrolled client's own bundle.
func (f *remoteFixture) dial() *remoteTestClient {
	f.t.Helper()
	cert, err := tls.LoadX509KeyPair(f.bundle.CertPath, f.bundle.KeyPath)
	assertNoErr(f.t, err, "load client keypair from bundle")
	return f.dialWith(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      f.caPool(),
	})
}

// dialWith connects with an arbitrary client TLS config. Returns nil when the
// handshake itself failed, which is a legitimate outcome for several tests.
func (f *remoteFixture) dialWith(cfg *tls.Config) *remoteTestClient {
	f.t.Helper()
	conn, err := tls.Dial("tcp", f.server.Addr(), cfg)
	if err != nil {
		return nil
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	return &remoteTestClient{t: f.t, conn: conn, scanner: bridge.NewScanner(conn)}
}

// remoteTestClient speaks the remote wire protocol by hand. Deliberately raw:
// several tests send frames a typed client could not construct (a `cwd` field,
// an admin request type), and those are precisely the frames that matter.
type remoteTestClient struct {
	t       *testing.T
	conn    net.Conn
	scanner *bufio.Scanner
}

func (c *remoteTestClient) sendRaw(line string) error {
	c.t.Helper()
	_ = c.conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write([]byte(line + "\n"))
	return err
}

// readFrame reads one response frame. The error is meaningful: a closed
// connection is how this listener refuses a caller it will not talk to.
func (c *remoteTestClient) readFrame() (bridge.BridgeResponse, error) {
	c.t.Helper()
	_ = c.conn.SetDeadline(time.Now().Add(10 * time.Second))
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return bridge.BridgeResponse{}, err
		}
		return bridge.BridgeResponse{}, fmt.Errorf("connection closed without a response")
	}
	var resp bridge.BridgeResponse
	if err := json.Unmarshal(c.scanner.Bytes(), &resp); err != nil {
		return bridge.BridgeResponse{}, err
	}
	return resp, nil
}

// roundTrip sends a raw line and expects exactly one frame back.
func (c *remoteTestClient) roundTrip(line string) bridge.BridgeResponse {
	c.t.Helper()
	if err := c.sendRaw(line); err != nil {
		c.t.Fatalf("send %s: %v", line, err)
	}
	resp, err := c.readFrame()
	if err != nil {
		c.t.Fatalf("read response to %s: %v", line, err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// The happy path
// ---------------------------------------------------------------------------

// An enrolled client can do the two things this listener exists for, and its
// identity on the audit record comes from the certificate rather than from
// anything it sent.
func TestRemoteServer_EnrolledClientListsToolsAndCallsTool(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()
	if c == nil {
		t.Fatal("enrolled client could not complete the handshake")
	}

	resp := c.roundTrip(`{"type":"ListTools"}`)
	if resp.Type != bridge.RespTools {
		t.Fatalf("ListTools returned %s: %s", resp.Type, resp.Message)
	}
	var tools []mcp.Tool
	assertNoErr(t, json.Unmarshal(resp.Tools, &tools), "parse tools")
	if len(tools) != 1 || tools[0].Name != "mail_search" {
		t.Fatalf("tool list = %+v, want the one tool the grant allows", tools)
	}

	resp = c.roundTrip(`{"type":"CallTool","name":"mail_search","arguments":{"q":"invoice"}}`)
	if resp.Type != bridge.RespResult {
		t.Fatalf("CallTool returned %s: %s", resp.Type, resp.Message)
	}
	if !strings.Contains(string(resp.Result), "3 messages") {
		t.Errorf("result did not come from the MCP: %s", resp.Result)
	}
	if f.mcpCalls.Load() != 1 {
		t.Errorf("MCP was invoked %d times, want 1", f.mcpCalls.Load())
	}

	// The connection put the call on the fail-closed audit path with an
	// attested identity — the whole point of resolving the certificate before
	// the request.
	events := readLoggedEvents(t, f.audit)
	if len(events) == 0 {
		t.Fatal("a remote tool call was not recorded at all")
	}
	last := events[len(events)-1]
	if last.Actor.Kind != AuditActorRemote || last.Actor.ClientID != "hermes-mail" {
		t.Errorf("actor = %+v, want the enrolled client attested from the certificate", last.Actor)
	}
	if last.Actor.Fingerprint != f.bundle.Enrolment.Fingerprint {
		t.Errorf("fingerprint = %q, want the full enrolled fingerprint %q", last.Actor.Fingerprint, f.bundle.Enrolment.Fingerprint)
	}
	if last.Actor.ProjectID != f.project.ID {
		t.Errorf("project = %q, want the granted project %q", last.Actor.ProjectID, f.project.ID)
	}
}

// An enrolment holding exactly one grant may leave project_id off; holding
// several, it must say which. Guessing would make the choice relay's.
func TestRemoteServer_ProjectIDIsOptionalForOneGrantAndRequiredForSeveral(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{extraProjects: 1})

	// Single grant: absent project_id defaults to it (asserted by the happy
	// path above, repeated here as the baseline for the widening below).
	if resp := f.dial().roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("single-grant ListTools without a project_id was refused: %s", resp.Message)
	}

	// Widen the enrolment to two grants without touching the certificate:
	// capability and device revocation are independent.
	var second string
	assertNoErr(t, f.store.With(func(s *Settings) {
		for _, p := range s.Projects {
			if p.ID != f.project.ID {
				second = p.ID
			}
		}
		s.UpdateEnrolmentGrants("hermes-mail", []string{f.project.ID, second})
	}), "widen grants")

	c := f.dial()
	resp := c.roundTrip(`{"type":"ListTools"}`)
	if resp.Type != bridge.RespError || !strings.Contains(resp.Message, "project_id is required") {
		t.Fatalf("two grants and no project_id returned %s/%q, want a refusal naming the ambiguity", resp.Type, resp.Message)
	}
	if resp = c.roundTrip(`{"type":"ListTools","project_id":"` + second + `"}`); resp.Type != bridge.RespTools {
		t.Fatalf("naming a held grant was refused: %s", resp.Message)
	}
}

// ---------------------------------------------------------------------------
// Identity: resolved from the certificate before a request is read
// ---------------------------------------------------------------------------

// A certificate relay itself signed, for which no enrolment exists — the state
// a revoked client is in. The connection must be closed WITHOUT a request being
// processed, so an unenrolled caller cannot probe for valid grants, valid tool
// names, or even the shape of an error.
func TestRemoteServer_UnenrolledCertificateIsClosedWithoutReadingARequest(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})

	ca, err := LoadOrCreateCA()
	assertNoErr(t, err, "LoadOrCreateCA")
	keyPEM, certPEM, _, err := ca.IssueClientCert("ghost")
	assertNoErr(t, err, "IssueClientCert")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	assertNoErr(t, err, "X509KeyPair")

	c := f.dialWith(&tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: f.caPool()})
	if c == nil {
		return // refused at the handshake — also acceptable, and stronger
	}
	// The write may succeed into a socket buffer; what must not happen is an
	// answer.
	_ = c.sendRaw(`{"type":"ListTools"}`)
	if resp, err := c.readFrame(); err == nil {
		t.Fatalf("an unenrolled certificate got a response: %s %s", resp.Type, resp.Message)
	}
	if f.mcpCalls.Load() != 0 {
		t.Error("an unenrolled certificate reached an MCP")
	}
}

// No certificate at all: mutual TLS is the door, and there is nothing behind it
// for a caller who cannot prove an identity.
func TestRemoteServer_ClientWithNoCertificateIsRejected(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})

	c := f.dialWith(&tls.Config{RootCAs: f.caPool()})
	if c == nil {
		return // handshake refused outright
	}
	_ = c.sendRaw(`{"type":"ListTools"}`)
	if resp, err := c.readFrame(); err == nil {
		t.Fatalf("a client with no certificate got a response: %s %s", resp.Type, resp.Message)
	}
}

// A certificate signed by some other authority must not complete a handshake:
// ClientCAs holds relay's CA and nothing else.
func TestRemoteServer_ForeignCertificateIsRejected(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})

	// A second, unrelated CA — the same code path relay uses for its own,
	// pointed at a different config dir.
	otherDir := mkShortTempDir(t, "other-ca-")
	otherCA, err := generateCA(otherDir+"/ca.key", otherDir+"/ca.crt")
	assertNoErr(t, err, "generate foreign CA")
	keyPEM, certPEM, _, err := otherCA.IssueClientCert("impostor")
	assertNoErr(t, err, "issue foreign client cert")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	assertNoErr(t, err, "X509KeyPair")

	c := f.dialWith(&tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: f.caPool()})
	if c == nil {
		return
	}
	_ = c.sendRaw(`{"type":"ListTools"}`)
	if resp, err := c.readFrame(); err == nil {
		t.Fatalf("a foreign-CA certificate got a response: %s %s", resp.Type, resp.Message)
	}
}

// ---------------------------------------------------------------------------
// The wire has no field to authenticate with (decision 4)
// ---------------------------------------------------------------------------

// A `cwd` must be REJECTED, not ignored. Ignoring it would leave a client
// believing it had authenticated by directory while it had in fact
// authenticated by certificate — the two succeed identically today, so the
// divergence would only surface later and in the confusing direction.
func TestRemoteServer_CwdFieldIsRejectedRatherThanIgnored(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	resp := c.roundTrip(`{"type":"CallTool","name":"mail_search","cwd":"/Users/someone/project"}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("a request carrying cwd was accepted: %s", resp.Type)
	}
	if !strings.Contains(resp.Message, "cwd") {
		t.Errorf("the refusal does not name the offending field: %q", resp.Message)
	}
	if f.mcpCalls.Load() != 0 {
		t.Error("a request carrying cwd still reached an MCP")
	}
}

// Same for a token: there is no bearer secret anywhere on this path, and a
// client that thinks otherwise should learn so at the door.
func TestRemoteServer_TokenFieldIsRejected(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	resp := f.dial().roundTrip(`{"type":"ListTools","token":"` + f.project.Token + `"}`)
	if resp.Type != bridge.RespError || !strings.Contains(resp.Message, "token") {
		t.Fatalf("a request carrying a token returned %s/%q, want a refusal naming the field", resp.Type, resp.Message)
	}
}

// ---------------------------------------------------------------------------
// The dispatch table has two entries (decision 1)
// ---------------------------------------------------------------------------

// The other eight bridge request types have NO code path from a remote
// connection. This asserts the outcome; the structural guarantee is that
// remoteHandlers has two entries and nothing falls through to bridgeHandlers.
func TestRemoteServer_EveryNonToolRequestTypeIsUnreachable(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	for _, typ := range []string{
		bridge.ReqResolvePtyEnv,
		bridge.ReqRegisterManifest,
		bridge.ReqListProjects,
		bridge.ReqGetProject,
		bridge.ReqResolveProjectTemplate,
		bridge.ReqReconcileExternalMcps,
		bridge.ReqReloadExternalMcp,
		bridge.ReqReloadService,
	} {
		resp := c.roundTrip(`{"type":"` + typ + `"}`)
		if resp.Type != bridge.RespError {
			t.Errorf("%s was answered on the remote listener: %s", typ, resp.Type)
			continue
		}
		if !strings.Contains(resp.Message, "not available to remote clients") {
			t.Errorf("%s refused with an unexpected message: %q", typ, resp.Message)
		}
	}
}

// The two-entry table is the security boundary, so assert its size directly:
// a third entry should have to be added deliberately, in a diff someone reads.
func TestRemoteDispatchTable_HoldsExactlyListToolsAndCallTool(t *testing.T) {
	if len(remoteHandlers) != 2 {
		t.Fatalf("remote dispatch table has %d entries: %v", len(remoteHandlers), remoteHandlers)
	}
	for _, want := range []string{bridge.ReqListTools, bridge.ReqCallTool} {
		if _, ok := remoteHandlers[want]; !ok {
			t.Errorf("remote dispatch table is missing %s", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Grants (decision 2) and the call-time re-check (decision 3, point 3)
// ---------------------------------------------------------------------------

// A project id is not a secret and confers nothing: naming one the enrolment
// does not hold is a refusal, not an escalation.
func TestRemoteServer_RefusesAProjectTheEnrolmentDoesNotGrant(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})

	// A second remote project exists but is granted to nobody.
	var ungranted Project
	var err error
	assertNoErr(t, f.store.With(func(s *Settings) {
		ungranted, err = s.CreateProjectWithTokenKind(ProjectKindRemote, "Calendar", "", []string{"macmcp"}, []string{}, nil, nil)
	}), "create ungranted project")
	assertNoErr(t, err, "create ungranted project")

	resp := f.dial().roundTrip(`{"type":"CallTool","name":"mail_search","project_id":"` + ungranted.ID + `"}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("a project the enrolment does not grant was honoured: %s", resp.Type)
	}
	if !strings.Contains(resp.Message, "does not grant") {
		t.Errorf("refusal message = %q, want it to name the missing grant", resp.Message)
	}
	if f.mcpCalls.Load() != 0 {
		t.Error("an ungranted project reached an MCP")
	}
}

// The call-time re-check. Enrolment-time and conversion-time validation cover
// the routes relay anticipated; this covers the ones it did not — here, a
// project mutated straight in the store, exactly as a hand-edited settings.json
// would look. The grant is stale and the call must fail closed.
func TestRemoteServer_RefusesAProjectThatIsNoLongerRemote(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	if resp := c.roundTrip(`{"type":"CallTool","name":"mail_search"}`); resp.Type != bridge.RespResult {
		t.Fatalf("baseline call failed: %s %s", resp.Type, resp.Message)
	}

	// Bypass every validated path: flip the kind in place, leaving the
	// enrolment's grant pointing at what is now a local project.
	assertNoErr(t, f.store.With(func(s *Settings) {
		for i := range s.Projects {
			if s.Projects[i].ID == f.project.ID {
				s.Projects[i].Kind = ProjectKindLocal
				s.Projects[i].Path = "/tmp"
			}
		}
	}), "convert project to local behind the guards")

	before := f.mcpCalls.Load()
	resp := c.roundTrip(`{"type":"CallTool","name":"mail_search"}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("a stale grant on a now-local project was honoured: %s", resp.Type)
	}
	if !strings.Contains(resp.Message, "not a remote project") {
		t.Errorf("refusal message = %q, want it to name the reason", resp.Message)
	}
	if f.mcpCalls.Load() != before {
		t.Error("a stale grant reached an MCP")
	}
}

// A deleted project is refused too — the grant resolves to nothing, and
// nothing is not a fallback.
func TestRemoteServer_RefusesAProjectThatNoLongerExists(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	assertNoErr(t, f.store.With(func(s *Settings) { s.RemoveProject(f.project.ID) }), "remove project")

	resp := c.roundTrip(`{"type":"ListTools"}`)
	if resp.Type != bridge.RespError {
		t.Fatalf("a grant on a deleted project was honoured: %s", resp.Type)
	}
}

// ---------------------------------------------------------------------------
// Revocation cuts live connections (decision 8)
// ---------------------------------------------------------------------------

// Taking effect on the next connection is not enough: this protocol holds
// persistent connections in a scanner loop, so an agent that never reconnects
// would keep working for as long as the process lives.
func TestRemoteServer_RevokingAnEnrolmentClosesItsLiveConnection(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("baseline ListTools failed: %s", resp.Message)
	}

	if _, err := revokeEnrolment(f.store, "hermes-mail"); err != nil {
		t.Fatalf("revokeEnrolment: %v", err)
	}

	// The socket itself must go, not merely the record.
	_ = c.sendRaw(`{"type":"ListTools"}`)
	if resp, err := c.readFrame(); err == nil {
		t.Fatalf("a revoked client's live connection kept working: %s %s", resp.Type, resp.Message)
	}
}

// ---------------------------------------------------------------------------
// Configuration (decision 9)
// ---------------------------------------------------------------------------

// An absent block means no listener at all — not a listener that refuses
// everything, and not a bound port nobody looks at.
func TestRemoteServer_DoesNotStartWhenTheConfigBlockIsAbsent(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{noRemoteBlock: true, skipServe: true})

	rs, err := NewRemoteServer(context.Background(), f.store, f.router, f.audit)
	assertNoErr(t, err, "NewRemoteServer with no remote block")
	if rs != nil {
		rs.Close()
		t.Fatalf("a listener was opened with no remote block, bound to %s", rs.Addr())
	}
}

// enabled:false is explicit and must also open nothing; a block that names a
// listen address but omits enabled opens nothing either, because starting a
// network listener nobody asked for is the failure mode this ADR exists to
// avoid.
func TestRemoteServer_DoesNotStartWhenDisabledOrUnstated(t *testing.T) {
	for name, opts := range map[string]remoteFixtureOpts{
		"explicit false": {enabled: ptr(false), skipServe: true},
		"unstated":       {omitEnabled: true, skipServe: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRemoteFixture(t, opts)
			rs, err := NewRemoteServer(context.Background(), f.store, f.router, f.audit)
			assertNoErr(t, err, "NewRemoteServer")
			if rs != nil {
				rs.Close()
				t.Fatalf("a listener was opened for %s, bound to %s", name, rs.Addr())
			}
		})
	}
}

// The listen default binds loopback, so misconfiguration cannot expose the
// control plane to a LAN and same-machine development needs no configuration.
func TestRemoteConfig_DefaultsToLoopback(t *testing.T) {
	if got := (&RemoteConfig{Enabled: ptr(true)}).resolve(); got.Listen != "127.0.0.1:9910" || !got.Enabled {
		t.Fatalf("resolved config = %+v, want loopback:9910 enabled", got)
	}
	if got := (*RemoteConfig)(nil).resolve(); got.Enabled {
		t.Fatal("an absent block resolved to enabled")
	}
}

// The server certificate covers loopback whatever else is configured, and a
// wildcard bind adds nothing it cannot verify.
func TestRemoteCertHosts(t *testing.T) {
	loopback := []string{"127.0.0.1", "::1", "localhost"}
	for listen, want := range map[string][]string{
		"127.0.0.1:9910":    loopback,
		"0.0.0.0:9910":      loopback,
		"[::]:9910":         loopback,
		"192.168.1.10:9910": append(append([]string{}, loopback...), "192.168.1.10"),
		"relay.local:9910":  append(append([]string{}, loopback...), "relay.local"),
	} {
		got := remoteCertHosts(listen)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("remoteCertHosts(%q) = %v, want %v", listen, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Auditing is mandatory for remote callers
// ---------------------------------------------------------------------------

// The case for letting a VM reach host mail rests on detection: ADR-009's
// threat model is exfiltration, and decision 5 pays availability for evidence.
// Remove the evidence and the justification goes with it — so the listener
// refuses to open at all rather than serving calls nobody will ever see.
func TestRemoteServer_RefusesToServeWhenAuditingIsDisabled(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{disableAudit: true, skipServe: true})

	rs, err := NewRemoteServer(context.Background(), f.store, f.router, f.audit)
	if err == nil {
		if rs != nil {
			rs.Close()
		}
		t.Fatal("the remote listener started with auditing disabled")
	}
	if rs != nil {
		rs.Close()
		t.Fatal("a listener was bound despite the refusal")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Errorf("the refusal does not explain itself to an operator: %v", err)
	}
}

// Local tooling is entirely unaffected by that refusal: the bridge still
// serves with auditing off, exactly as it always has.
func TestRemoteServer_AuditRefusalDoesNotAffectLocalCallers(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{disableAudit: true, skipServe: true})

	if _, err := NewRemoteServer(context.Background(), f.store, f.router, f.audit); err == nil {
		t.Fatal("expected the remote listener to refuse")
	}
	// The same router, called locally with a project token, keeps working.
	if _, err := f.router.CallTool(context.Background(), "mail_search", json.RawMessage(`{}`), f.project.Token); err != nil {
		t.Fatalf("a local caller was affected by the remote listener's refusal: %v", err)
	}
	if f.mcpCalls.Load() != 1 {
		t.Errorf("local MCP calls = %d, want 1", f.mcpCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// Deadlines (decision 9)
// ---------------------------------------------------------------------------

// BridgeServer sets no deadlines, which is fine on a 0600 Unix socket and a
// slowloris vector on a network listener: a peer that connects and says
// nothing must not hold a goroutine and an fd indefinitely.
func TestRemoteServer_IdleConnectionIsClosed(t *testing.T) {
	restore := remoteIdleTimeout
	remoteIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { remoteIdleTimeout = restore })

	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	time.Sleep(600 * time.Millisecond)

	_ = c.sendRaw(`{"type":"ListTools"}`)
	if resp, err := c.readFrame(); err == nil {
		t.Fatalf("an idle connection was still served: %s %s", resp.Type, resp.Message)
	}
}

// ...and the inactivity timeout must not cut off a connection that is being
// used, however slowly relay itself answers.
func TestRemoteServer_ActiveConnectionSurvivesRepeatedShortGaps(t *testing.T) {
	restore := remoteIdleTimeout
	remoteIdleTimeout = 400 * time.Millisecond
	t.Cleanup(func() { remoteIdleTimeout = restore })

	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	for i := 0; i < 4; i++ {
		time.Sleep(150 * time.Millisecond)
		if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
			t.Fatalf("request %d on an active connection failed: %s %s", i, resp.Type, resp.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// Malformed input
// ---------------------------------------------------------------------------

func TestRemoteServer_MalformedFrameIsAnErrorNotADisconnect(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	c := f.dial()

	if resp := c.roundTrip(`{"type":`); resp.Type != bridge.RespError {
		t.Fatalf("malformed JSON returned %s", resp.Type)
	}
	// The connection survives, so a client can recover without re-handshaking.
	if resp := c.roundTrip(`{"type":"ListTools"}`); resp.Type != bridge.RespTools {
		t.Fatalf("connection did not survive a malformed frame: %s %s", resp.Type, resp.Message)
	}
}

func TestRemoteServer_TypelessFrameIsRefused(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{})
	if resp := f.dial().roundTrip(`{}`); resp.Type != bridge.RespError {
		t.Fatalf("a frame with no type returned %s", resp.Type)
	}
}

// Budgets and the listener were built independently: the budget tests inject a
// remote identity into the context directly, and the listener tests never set a
// budget. Neither proves the seam between them. This drives a real mTLS client
// through the real listener into the real enforcement, because a rate limit that
// only binds a synthetic context protects nothing.
func TestRemoteServer_BudgetIsEnforcedThroughTheListener(t *testing.T) {
	f := newRemoteFixture(t, remoteFixtureOpts{
		budget: &EnrolmentBudget{WindowSeconds: 3600, MaxCalls: 2, MaxResultBytes: 1 << 20},
	})
	c := f.dial()
	if c == nil {
		t.Fatal("enrolled client could not complete the handshake")
	}

	call := `{"type":"CallTool","name":"mail_search","arguments":{"q":"invoice"}}`
	for i := 1; i <= 2; i++ {
		if resp := c.roundTrip(call); resp.Type != bridge.RespResult {
			t.Fatalf("call %d within budget returned %s: %s", i, resp.Type, resp.Message)
		}
	}

	resp := c.roundTrip(call)
	if resp.Type != bridge.RespError {
		t.Fatalf("third call over a 2-call budget returned %s, want an error", resp.Type)
	}
	if !strings.Contains(resp.Message, "throttled") {
		t.Errorf("refusal message = %q, want it to say it was throttled", resp.Message)
	}

	// The refusal has to happen before the tool runs. A throttle that fires
	// after the mailbox has been read interdicts nothing.
	if got := f.mcpCalls.Load(); got != 2 {
		t.Errorf("MCP was invoked %d times, want 2 — the throttled call must not reach it", got)
	}

	// And it must be greppable as a throttle specifically: "the grant was
	// legitimate and the pattern of use was not" is a different signal from
	// denied (never granted) or tool_error (refused inside the MCP).
	events := readLoggedEvents(t, f.audit)
	var throttled *AuditEvent
	for i := range events {
		if events[i].Outcome == AuditOutcomeThrottled {
			throttled = &events[i]
			break
		}
	}
	if throttled == nil {
		t.Fatal("the throttled call produced no audit record with outcome throttled")
	}
	if throttled.Actor.Kind != AuditActorRemote || throttled.Actor.ClientID != "hermes-mail" {
		t.Errorf("throttled actor = %+v, want the attested remote client", throttled.Actor)
	}
	if throttled.Actor.Fingerprint != f.bundle.Enrolment.Fingerprint {
		t.Errorf("throttled fingerprint = %q, want the full enrolled fingerprint", throttled.Actor.Fingerprint)
	}
	if throttled.Tool != "mail_search" {
		t.Errorf("throttled record names tool %q, want mail_search", throttled.Tool)
	}
}
