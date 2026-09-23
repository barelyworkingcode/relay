package main

// Tests for ADR-015 decision 2: an execute-class route must be ABSENT from
// the TCP mux -- the mux's own refusal, never a handler that ran and
// refused. That refusal takes two shapes, because the "/" catch-all is
// socket-only (control.ClassProxy, ADR-016 decision 4) and absorbs nothing here: a
// text/plain 404 where no pattern claims the path, and a 405 where a pattern
// claims it under another method. Exercises the real wiring (NewFrontendServer +
// ListenLoopback + a real TCP/Unix listener + real HTTP requests), which is
// what distinguishes this file from capability_test.go's coverage of
// control.RouteRegistrar.Handle and control.ClassReachableOn in isolation -- that file
// already pins the fail-closed matrix (unknown class, known class on an
// unknown transport) so this file does not repeat it.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
)

// teCounters are flipped only by an ops method's success path (each Create /
// Update / SetRemoteConfig calls notify() after it persists), so they are an
// independent witness to "the real handler body ran" that does not rely on
// reading the HTTP response back.
type teCounters struct {
	serviceChanges int
	commits        atomic.Int64
}

type teServer struct {
	srv      *FrontendServer
	store    config.SettingsStore
	counters *teCounters
	token    string
	tcpBase  string
	sockHTTP *http.Client
}

// teNewServer wires a real FrontendServer -- real ServiceOps/EnrolmentOps/
// audit.AuditOps/McpOps over store, a real 0600 Unix socket, and a real loopback
// TCP listener via ListenLoopback -- so a test here exercises
// registerFrontendRoutes exactly as frontend_server.go calls it, not a
// synthetic mux. authz is passed straight to NewFrontendServer; nil (like
// frontend_server_test.go's hermetic tests) isolates the transport-routing
// property under test from ADR-015's separate credential-classing layer.
// store is a parameter rather than built internally so a caller can mint a
// credential against it (via NewCredentialAuthorizer) before or after the
// server exists.
func teNewServer(t *testing.T, store config.SettingsStore, authz control.Authorizer) *teServer {
	t.Helper()
	counters := &teCounters{}

	ops := &ServiceOps{Store: store, Registry: &svcRecorder{}, OnChange: func() { counters.serviceChanges++ }}
	queue := commitQueueFor(t, store, &counters.commits)
	enrolOps := &EnrolmentOps{Store: store, Queue: queue}
	auditOps := &audit.AuditOps{}
	mcpOps := &McpOps{Store: store, Ctx: context.Background(), Queue: queue}
	projOps := &ProjectOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	extMgr := mcpbroker.NewManager(nil)
	enhanced := NewEnhancedServiceRegistry(nil)

	const token = "te-bearer"
	sockDir := mkShortTempDir(t, "te-fe-")
	srv, err := NewFrontendServer(
		store, extMgr, extMgr, extMgr,
		seededEndpoint(t, store, filepath.Join(sockDir, "frontend.sock"), token),
		enhanced, nil, nil, ops, enrolOps, auditOps, mcpOps, projOps, nil, nil, nil, nil, authz, nil, nil,
		sessionRouteDeps{},
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

	return &teServer{
		srv:      srv,
		store:    store,
		counters: counters,
		token:    token,
		tcpBase:  "http://" + srv.tcpLn.Addr().String(),
		sockHTTP: dialFrontendHTTP(srv.socketPath),
	}
}

// teFixtureIDs names the seeded records every route in teRouteTable expects
// to find. Seeded directly against the store (never through the execute
// HTTP routes themselves, which is the property under test) so fixture setup
// can never be confused with the behavior being verified.
type teFixtureIDs struct {
	projID     string
	projDelID  string
	newProjDir string
}

func teSeed(t *testing.T, store config.SettingsStore) teFixtureIDs {
	t.Helper()
	proj := mkStoreProject(t, store, config.ProjectKindLocal, "te-proj1", t.TempDir())
	projDel := mkStoreProject(t, store, config.ProjectKindLocal, "te-proj-del", t.TempDir())

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{ID: "svc1", DisplayName: "svc1", Command: "/bin/true"})
		s.UpsertService(config.ServiceConfig{ID: "svc-del", DisplayName: "svc-del", Command: "/bin/true"})
		s.AddExternalMcp(config.ExternalMcp{ID: "mcp1", DisplayName: "mcp1", Command: "/bin/true"})
		s.Enrolments = append(s.Enrolments,
			config.Enrolment{ClientID: "enr1", Fingerprint: "fp-enr1"},
			config.Enrolment{ClientID: "enr-del", Fingerprint: "fp-enr-del"})
	}), "seed fixtures")

	return teFixtureIDs{projID: proj.ID, projDelID: projDel.ID, newProjDir: t.TempDir()}
}

type teRoute struct {
	method string
	path   string
	class  control.CapabilityClass
	body   any
}

// teNonexistentMcpCommand never spawns a real process: exec itself fails
// before any pipe is opened (the same command TestMcpRoutes_CreateDiscoveryFailureMaps502
// uses), so a "reachable" check on POST /api/mcps can run on a real listener
// in this file without a live MCP binary or a stdio handshake to wait out.
const teNonexistentMcpCommand = "/nonexistent/does-not-exist-binary-zzz"

// teDispatcherNoService is the body FrontendDispatcher writes when no
// enhanced service claims a path. Reaching it is proof the catch-all is
// mounted, which http.ServeMux's own 404 -- same status, same text/plain --
// cannot be told from any other way.
const teDispatcherNoService = "no service registered for this path"

// teRouteTable reproduces every route relay registers, in the order the
// production files declare them. This is deliberately exhaustive rather than
// a sample: the property under test (execute absent from TCP, everything
// else present on both) is a claim about the WHOLE table, and a table that
// silently drifted from the real route set would let a fifth execute route
// slip onto TCP undetected.
func teRouteTable(ids teFixtureIDs) []teRoute {
	return []teRoute{
		// service_routes.go
		{"GET", "/api/services", control.ClassRead, nil},
		{"GET", "/api/services/svc1", control.ClassRead, nil},
		{"POST", "/api/services", control.ClassExecute, map[string]any{"display_name": "te-phantom-service", "command": "/bin/true"}},
		{"PUT", "/api/services/svc1", control.ClassExecute, map[string]any{"display_name": "svc1", "command": "/bin/false"}},
		{"DELETE", "/api/services/svc-del", control.ClassConfigure, nil},
		{"POST", "/api/services/svc1/start", control.ClassConfigure, nil},
		{"POST", "/api/services/svc1/stop", control.ClassConfigure, nil},
		{"PUT", "/api/services/svc1/autostart", control.ClassConfigure, map[string]any{"autostart": true}},
		{"PUT", "/api/services/svc1/position", control.ClassConfigure, map[string]any{"index": 0}},
		{"PUT", "/api/services/svc1/menu", control.ClassConfigure, map[string]any{"hidden": true}},

		// enrolment_routes.go
		{"GET", "/api/enrolments", control.ClassRead, nil},
		{"GET", "/api/enrolments/enr1", control.ClassRead, nil},
		{"POST", "/api/enrolments", control.ClassGrant, map[string]any{"client_id": "te-new-enrolment"}},
		{"DELETE", "/api/enrolments/enr-del", control.ClassGrant, nil},
		{"GET", "/api/remote", control.ClassRead, nil},
		{"PUT", "/api/remote", control.ClassExecute, map[string]any{"enabled": true, "listen": "127.0.0.1:9910"}},

		// mcp_routes.go
		{"POST", "/api/mcps", control.ClassExecute, map[string]any{"display_name": "te-phantom-mcp", "command": teNonexistentMcpCommand}},
		{"DELETE", "/api/mcps/mcp1", control.ClassConfigure, nil},

		// project_routes.go
		{"GET", "/api/projects", control.ClassRead, nil},
		{"GET", "/api/projects/" + ids.projID, control.ClassRead, nil},
		{"GET", "/api/mcps", control.ClassRead, nil},
		{"GET", "/api/mcps/mcp1/scope_fields", control.ClassRead, nil},
		{"GET", "/api/mcps/mcp1/tools", control.ClassRead, nil},
		{"POST", "/api/mcps/mcp1/enumerate", control.ClassRead, map[string]any{"field": "x"}},
		{"POST", "/api/projects", control.ClassConfigure, map[string]any{"name": "te-new-project", "path": ids.newProjDir}},
		{"PUT", "/api/projects/" + ids.projID, control.ClassConfigure, map[string]any{}},
		{"DELETE", "/api/projects/" + ids.projDelID, control.ClassConfigure, nil},
		{"POST", "/api/projects/" + ids.projID + "/regen_skill", control.ClassConfigure, nil},
		{"POST", "/api/projects/" + ids.projID + "/rotate_token", control.ClassGrant, nil},

		// audit_routes.go
		{"GET", "/api/audit", control.ClassRead, nil},
		{"GET", "/api/audit/log", control.ClassRead, nil},
		{"POST", "/api/audit/export", control.ClassConfigure, map[string]any{}},
	}
}

func teExecuteRoutes(ids teFixtureIDs) []teRoute {
	var out []teRoute
	for _, r := range teRouteTable(ids) {
		if r.class == control.ClassExecute {
			out = append(out, r)
		}
	}
	return out
}

func teNonExecuteRoutes(ids teFixtureIDs) []teRoute {
	var out []teRoute
	for _, r := range teRouteTable(ids) {
		if r.class != control.ClassExecute {
			out = append(out, r)
		}
	}
	return out
}

func teDo(t *testing.T, client *http.Client, method, url, token string, body any) (*http.Response, []byte) {
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

func (ts *teServer) doTCP(t *testing.T, r teRoute) (*http.Response, []byte) {
	t.Helper()
	return teDo(t, http.DefaultClient, r.method, ts.tcpBase+r.path, ts.token, r.body)
}

func (ts *teServer) doSocket(t *testing.T, r teRoute) (*http.Response, []byte) {
	t.Helper()
	return teDo(t, ts.sockHTTP, r.method, "http://unix"+r.path, ts.token, r.body)
}

// teIsRouted reports whether some handler ran. Three answers are the mux
// refusing before any handler: a genuine "no route registered" 404
// (http.NotFound, Content-Type text/plain), the same 404 from a handler that
// ran and answered for a missing resource (writeJSON, Content-Type
// application/json -- routed), and a 405.
//
// The 405 arm exists because the TCP mux has no catch-all: control.ClassProxy is
// socket-only (ADR-016 decision 4), so nothing there absorbs a near-miss and
// http.ServeMux answers with its own 405 -- no handler, no dispatcher,
// nothing registered under that method. A catch-all on this mux would turn
// that same request into a dispatched one, which is why the distinction is
// worth making here rather than lumping every non-404 in as routed.
func teIsRouted(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusMethodNotAllowed:
		return false
	case http.StatusNotFound:
		return strings.Contains(resp.Header.Get("Content-Type"), "application/json")
	}
	return true
}

// teAssertMuxRefused asserts http.ServeMux itself refused the request before
// any relay handler ran, in either of the two shapes that means: a
// text/plain 404 when no pattern claims the path, or a 405 with an Allow
// header when a pattern claims the path under a different method. A JSON
// body is the thing being ruled out -- that would be a relay handler
// answering.
func teAssertMuxRefused(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()
	switch resp.StatusCode {
	case http.StatusNotFound:
	case http.StatusMethodNotAllowed:
		if resp.Header.Get("Allow") == "" {
			t.Fatalf("405 carries no Allow header, so it is not http.ServeMux's own refusal; body=%s", body)
		}
	default:
		t.Fatalf("status = %d, want 404 (no pattern registered) or 405 (ServeMux refusing the method); body=%s", resp.StatusCode, body)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q looks like a handler answered rather than the mux refusing; body=%s", resp.Header.Get("Content-Type"), body)
	}
}

// TestTCPServiceCreate_ExecuteRouteAbsent_HandlerNeverRan proves POST
// /api/services on the TCP listener is not merely refused: the response
// carries the mux's own refusal signature (not a JSON body from
// service_routes.go), nothing was persisted, and ServiceOps' OnChange -- a
// counter only the successful tail of Create touches -- never fired.
//
// It also proves the fact that turns this route's answer from a 404 into a
// 405: the "/" catch-all is absent from the TCP mux altogether, so nothing
// is left there to absorb a near-miss. A path only the catch-all could ever
// claim gets a text/plain 404 on TCP and reaches the dispatcher on the
// socket, from the same server (ADR-016 decision 4).
func TestTCPServiceCreate_ExecuteRouteAbsent_HandlerNeverRan(t *testing.T) {
	store := newCLISandboxStore(t)
	ts := teNewServer(t, store, nil)
	teSeed(t, store)
	before := store.Get().Services

	resp, body := ts.doTCP(t, teRoute{"POST", "/api/services", control.ClassExecute,
		map[string]any{"display_name": "te-phantom-service", "command": "/bin/true"}})

	teAssertMuxRefused(t, resp, body)
	if ts.counters.serviceChanges != 0 {
		t.Fatalf("ServiceOps.OnChange fired %d times; the handler must never have run", ts.counters.serviceChanges)
	}
	after := store.Get().Services
	if len(after) != len(before) {
		t.Fatalf("service count changed %d -> %d; POST /api/services must not have reached ServiceOps.Create", len(before), len(after))
	}
	for _, svc := range after {
		if svc.DisplayName == "te-phantom-service" {
			t.Fatal("the phantom service was persisted despite the mux refusing")
		}
	}

	// Both answers are a text/plain 404, so the BODY is what tells them
	// apart: the dispatcher names itself, and http.ServeMux's NotFoundHandler
	// says "404 page not found".
	catchAllOnly := teRoute{"POST", "/api/sessions", control.ClassProxy, map[string]any{}}
	resp, body = ts.doSocket(t, catchAllOnly)
	if !strings.Contains(string(body), teDispatcherNoService) {
		t.Fatalf("POST /api/sessions on the socket: status=%d body=%s; want the dispatcher's own answer, or the TCP check below proves nothing",
			resp.StatusCode, body)
	}
	resp, body = ts.doTCP(t, catchAllOnly)
	if resp.StatusCode != http.StatusNotFound || strings.Contains(string(body), teDispatcherNoService) {
		t.Fatalf("POST /api/sessions on TCP: status=%d body=%s; the catch-all must be absent from the TCP mux entirely",
			resp.StatusCode, body)
	}
}

func TestTCPServiceUpdate_ExecuteRouteAbsent_HandlerNeverRan(t *testing.T) {
	store := newCLISandboxStore(t)
	ts := teNewServer(t, store, nil)
	teSeed(t, store)

	resp, body := ts.doTCP(t, teRoute{"PUT", "/api/services/svc1", control.ClassExecute,
		map[string]any{"display_name": "svc1", "command": "/bin/false"}})

	teAssertMuxRefused(t, resp, body)
	if ts.counters.serviceChanges != 0 {
		t.Fatalf("ServiceOps.OnChange fired %d times; the handler must never have run", ts.counters.serviceChanges)
	}
	svc, _ := config.FindServiceByID(store.Get(), "svc1")
	if svc == nil || svc.Command != "/bin/true" {
		t.Fatalf("svc1.Command = %+v; PUT must never have reached ServiceOps.Update", svc)
	}
}

func TestTCPMcpCreate_ExecuteRouteAbsent_HandlerNeverRan(t *testing.T) {
	store := newCLISandboxStore(t)
	ts := teNewServer(t, store, nil)
	teSeed(t, store)
	before := store.Get().ExternalMcps

	resp, body := ts.doTCP(t, teRoute{"POST", "/api/mcps", control.ClassExecute,
		map[string]any{"display_name": "te-phantom-mcp", "command": teNonexistentMcpCommand}})

	teAssertMuxRefused(t, resp, body)
	if ts.counters.commits.Load() != 0 {
		t.Fatalf("McpOps committed %d times; the handler must never have run", ts.counters.commits.Load())
	}
	after := store.Get().ExternalMcps
	if len(after) != len(before) {
		t.Fatalf("mcp count changed %d -> %d; POST /api/mcps must not have reached McpOps.Add", len(before), len(after))
	}
}

func TestTCPRemoteConfigPut_ExecuteRouteAbsent_HandlerNeverRan(t *testing.T) {
	store := newCLISandboxStore(t)
	ts := teNewServer(t, store, nil)
	teSeed(t, store)

	resp, body := ts.doTCP(t, teRoute{"PUT", "/api/remote", control.ClassExecute,
		map[string]any{"enabled": true, "listen": "127.0.0.1:9910"}})

	teAssertMuxRefused(t, resp, body)
	if ts.counters.commits.Load() != 0 {
		t.Fatalf("EnrolmentOps committed %d times; the handler must never have run", ts.counters.commits.Load())
	}
	if cfg := store.Get().Remote; cfg != nil {
		t.Fatalf("remote config = %+v, want nil; PUT /api/remote must not have reached EnrolmentOps.SetRemoteConfig", cfg)
	}
}

// TestTCPExecuteRoutes_StayAbsentEvenForACredentialGrantedExecute is the
// strongest available proof that decision 2 is a routing property and not an
// authorization refusal in disguise: it wires the REAL credentialAuthorizer
// and mints a credential carrying control.ClassExecute (among all four classes),
// hashed to the SAME bearer this test sends. If
// ADR-015 decision 2 were a policy check inside the handler instead of an
// absent registration, this credential would sail through it, so a 404 here
// can only mean the pattern was never handed to the TCP mux at all.
func TestTCPExecuteRoutes_StayAbsentEvenForACredentialGrantedExecute(t *testing.T) {
	store := newCLISandboxStore(t)
	ts := teNewServer(t, store, NewCredentialAuthorizer(store))
	ids := teSeed(t, store)

	assertNoErr(t, store.With(func(s *config.Settings) {
		addAPICredential(s, config.APICredential{
			ID:      "te-all-classes-cred",
			Name:    "te-all-classes-cred",
			Hash:    config.HashToken(ts.token),
			Classes: []control.CapabilityClass{control.ClassRead, control.ClassConfigure, control.ClassGrant, control.ClassExecute},
			Created: time.Now().UTC().Format(time.RFC3339),
		})
	}), "seed all-classes credential")

	for _, r := range teExecuteRoutes(ids) {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			resp, body := ts.doTCP(t, r)
			teAssertMuxRefused(t, resp, body)
		})
	}
}

func TestTCPNonExecuteRoutes_AreReachable(t *testing.T) {
	store := newCLISandboxStore(t)
	ts := teNewServer(t, store, nil)
	ids := teSeed(t, store)

	for _, r := range teNonExecuteRoutes(ids) {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			resp, body := ts.doTCP(t, r)
			if !teIsRouted(resp) {
				t.Fatalf("class %s route not reachable on TCP: status=%d content-type=%q body=%s",
					r.class, resp.StatusCode, resp.Header.Get("Content-Type"), body)
			}
		})
	}
}

func TestSocketRoutes_AllRoutesAreReachable(t *testing.T) {
	store := newCLISandboxStore(t)
	ts := teNewServer(t, store, nil)
	ids := teSeed(t, store)

	for _, r := range teRouteTable(ids) {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			resp, body := ts.doSocket(t, r)
			if !teIsRouted(resp) {
				t.Fatalf("class %s route not reachable on the socket: status=%d content-type=%q body=%s",
					r.class, resp.StatusCode, resp.Header.Get("Content-Type"), body)
			}
		})
	}
}

// TestRouteSetDivergence_TCPEqualsSocketMinusExecuteRoutes drives the FULL
// table against two independently seeded servers -- one probed only over
// TCP, one only over the socket, so neither run's mutations (deletes,
// creates) can contaminate the other's routing verdict -- and asserts the
// only place the two transports disagree is on the four execute routes,
// where TCP must be unrouted and the socket must be routed. Every other
// route must agree exactly. http.ServeMux exposes no API to enumerate its
// registered patterns, so comparing outcomes across the full table is the
// available substitute for diffing the two mux's pattern sets directly.
func TestRouteSetDivergence_TCPEqualsSocketMinusExecuteRoutes(t *testing.T) {
	tcpStore := newCLISandboxStore(t)
	tcpServer := teNewServer(t, tcpStore, nil)
	tcpIDs := teSeed(t, tcpStore)

	sockStore := newCLISandboxStore(t)
	sockServer := teNewServer(t, sockStore, nil)
	sockIDs := teSeed(t, sockStore)

	tcpTable := teRouteTable(tcpIDs)
	sockTable := teRouteTable(sockIDs)
	if len(tcpTable) != len(sockTable) {
		t.Fatalf("route tables diverged in length: %d vs %d", len(tcpTable), len(sockTable))
	}

	for i := range tcpTable {
		r := tcpTable[i] // method/path/class identical between the two tables; only seeded ids vary
		tcpResp, tcpBody := tcpServer.doTCP(t, tcpTable[i])
		sockResp, sockBody := sockServer.doSocket(t, sockTable[i])
		tcpRouted := teIsRouted(tcpResp)
		sockRouted := teIsRouted(sockResp)

		if !sockRouted {
			t.Fatalf("%s %s: not reachable on the socket at all (status=%d body=%s); the divergence check requires a socket baseline",
				r.method, r.path, sockResp.StatusCode, sockBody)
		}

		wantTCPRouted := r.class != control.ClassExecute
		if tcpRouted != wantTCPRouted {
			t.Fatalf("%s %s (class %s): TCP routed=%v, want %v (tcp status=%d body=%s)",
				r.method, r.path, r.class, tcpRouted, wantTCPRouted, tcpResp.StatusCode, tcpBody)
		}
	}
}
