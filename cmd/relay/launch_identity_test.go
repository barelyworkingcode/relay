package main

// docs/launch-identity.md, exercised through relay's real bridge socket and
// real frontend socket: every identity bound here is bound to a kernel audit
// token read off a live connection, never to a value the test asserts.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/google/uuid"
)

// seededEndpoint records bearer as a read+configure+proxy credential in store
// (none when bearer is empty) and returns the frontend endpoint, for tests
// that authenticate to the frontend socket with a bearer.
func seededEndpoint(t *testing.T, store config.SettingsStore, socket, bearer string) Endpoint {
	t.Helper()
	if bearer != "" {
		assertNoErr(t, store.With(func(s *config.Settings) {
			addAPICredential(s, config.APICredential{
				ID:      uuid.New().String(),
				Name:    "test-bearer",
				Hash:    config.HashToken(bearer),
				Classes: slices.Clone(frontendConsumerClasses),
				Created: time.Now().UTC().Format(time.RFC3339),
			})
		}), "seed bearer credential")
	}
	return Endpoint{Socket: socket}
}

var testIdentityPIDs atomic.Int32

// bindTestIdentity binds a launch named name on r to a synthetic process and
// returns a context carrying that process as the bridge peer. For router
// tests that call methods directly rather than through a socket.
func bindTestIdentity(t *testing.T, r *appRouter, name string, caps []config.ServiceCapability) context.Context {
	t.Helper()
	if r.launches == nil {
		r.launches = service.NewLaunches()
	}
	secret, _, err := r.launches.Begin(service.Identity{Kind: service.IdentityKindService, Name: name, Capabilities: caps})
	assertNoErr(t, err, "Begin")
	peer := peertoken.ForProcessForTest(2_000_000+testIdentityPIDs.Add(1), 1)
	_, err = r.launches.Bind(name, secret, peer)
	assertNoErr(t, err, "Bind")
	return bridge.WithCallerPeer(context.Background(), peer)
}

func bindTestServiceIdentity(t *testing.T, r *appRouter) context.Context {
	t.Helper()
	return bindTestIdentity(t, r, "test-service", capsBridge)
}

// selfPeerToken is this test process's own audit token, as the kernel
// reports it across a Unix socket.
func selfPeerToken(t *testing.T) peertoken.Token {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(mkShortTempDir(t, "peer-"), "p.sock"))
	assertNoErr(t, err, "listen")
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err := net.Dial("unix", ln.Addr().String())
	assertNoErr(t, err, "dial")
	defer client.Close()
	server := <-accepted
	defer server.Close()
	tok, err := peertoken.FromConn(server)
	assertNoErr(t, err, "peer token")
	return tok
}

// helloAsLaunchedService begins a launch and completes its Hello over the
// real bridge socket, so the identity is bound to this test process.
func helloAsLaunchedService(t *testing.T, launches *service.Launches, sock, name string, caps []config.ServiceCapability) *service.Launch {
	t.Helper()
	secret, launch, err := launches.Begin(service.Identity{Kind: service.IdentityKindService, Name: name, Capabilities: caps})
	assertNoErr(t, err, "Begin")
	_, err = bridge.SendHello(sock, name, secret)
	assertNoErr(t, err, "Hello")
	return launch
}

type idBridge struct {
	launches *service.Launches
	enhanced *EnhancedServiceRegistry
	sock     string
}

func startIdentityBridge(t *testing.T) *idBridge {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	b := &idBridge{launches: service.NewLaunches(), enhanced: NewEnhancedServiceRegistry(nil)}
	router := &appRouter{
		store:    store,
		tools:    mcpbroker.NewManager(nil),
		services: &fakeServiceReloader{},
		enhanced: b.enhanced,
		launches: b.launches,
	}
	srv, err := bridge.NewBridgeServer(context.Background(), router)
	assertNoErr(t, err, "NewBridgeServer")
	go func() { _ = srv.Serve() }()
	t.Cleanup(srv.Close)
	b.sock = bridge.SocketPath()
	_ = dialUnixWithTimeout(t, b.sock, 2*time.Second).Close()
	return b
}

func (b *idBridge) begin(t *testing.T, name string, caps []config.ServiceCapability) (string, *service.Launch) {
	t.Helper()
	secret, launch, err := b.launches.Begin(service.Identity{Kind: service.IdentityKindService, Name: name, Capabilities: caps})
	assertNoErr(t, err, "Begin")
	return secret, launch
}

// send writes req on a fresh connection and returns the response and its raw
// line.
func (b *idBridge) send(t *testing.T, req bridge.BridgeRequest) (bridge.BridgeResponse, string) {
	t.Helper()
	conn := dialUnixWithTimeout(t, b.sock, 2*time.Second)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	payload, err := json.Marshal(req)
	assertNoErr(t, err, "marshal")
	_, err = conn.Write(append(payload, '\n'))
	assertNoErr(t, err, "write")
	sc := bridge.NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("%s: no response: %v", req.Type, sc.Err())
	}
	line := sc.Text()
	var resp bridge.BridgeResponse
	assertNoErr(t, json.Unmarshal([]byte(line), &resp), "unmarshal %q", line)
	return resp, line
}

func (b *idBridge) hello(t *testing.T, name, secret string) (bridge.BridgeResponse, string) {
	t.Helper()
	return b.send(t, bridge.BridgeRequest{Type: bridge.ReqHello, Name: name, Token: secret})
}

func assertBridgeUnauthorized(t *testing.T, resp bridge.BridgeResponse, what string) {
	t.Helper()
	if resp.Type != bridge.RespError || resp.Code != jsonrpc.CodeUnauthorized {
		t.Fatalf("%s: got %+v, want an Error with code %d", what, resp, jsonrpc.CodeUnauthorized)
	}
}

// serviceOperationRequests is every bridge operation only a launch identity with the right capability
// identity may make, each sent with no token.
func serviceOperationRequests(t *testing.T, serviceID string) map[string]bridge.BridgeRequest {
	t.Helper()
	manifest, err := json.Marshal(bridge.RegisterManifestRequest{
		ServiceID:      serviceID,
		InternalSocket: "/tmp/launch-identity-test-internal.sock",
		InternalToken:  "internal-bearer",
		Manifest:       bridge.Manifest{Routes: []string{"/api/" + serviceID}},
	})
	assertNoErr(t, err, "marshal manifest")
	modelHost, err := json.Marshal(bridge.RegisterModelHostRequest{
		ServiceID:    serviceID,
		RouterSocket: "/tmp/launch-identity-test-router.sock",
	})
	assertNoErr(t, err, "marshal model host")
	return map[string]bridge.BridgeRequest{
		bridge.ReqRegisterManifest:  {Type: bridge.ReqRegisterManifest, Arguments: manifest},
		bridge.ReqRegisterModelHost: {Type: bridge.ReqRegisterModelHost, Arguments: modelHost},
	}
}

func TestHello_BindsTheCallingProcessAndAnswersWithoutACredential(t *testing.T) {
	b := startIdentityBridge(t)
	secret, _ := b.begin(t, "svc-hello", capsBridge)

	resp, line := b.hello(t, "svc-hello", secret)
	if resp.Type != bridge.RespOK {
		t.Fatalf("Hello: got %s", line)
	}
	if strings.Contains(line, secret) {
		t.Fatal("the Hello response echoed the launch secret")
	}
	var data map[string]json.RawMessage
	assertNoErr(t, json.Unmarshal(resp.Data, &data), "decode data")
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !slices.Equal(keys, []string{"kind", "relay_pid", "service_id"}) {
		t.Fatalf("Hello data keys = %v; the response may recognise the caller and carry nothing else", keys)
	}
	var result bridge.HelloResult
	assertNoErr(t, json.Unmarshal(resp.Data, &result), "decode result")
	if result.ServiceID != "svc-hello" || result.Kind != "service" || result.RelayPID != os.Getpid() {
		t.Fatalf("Hello result = %+v", result)
	}

	id, ok := b.launches.Lookup(selfPeerToken(t))
	if !ok || id.Name != "svc-hello" || int(id.Process.PID) != os.Getpid() {
		t.Fatalf("the kernel's view of this process is not the bound identity: %+v, %v", id, ok)
	}
}

func TestHello_AForgedSecretIsRefusedAndBindsNothing(t *testing.T) {
	b := startIdentityBridge(t)
	secret, _ := b.begin(t, "svc-forged", capsBridge)
	forged := strings.Repeat("0", service.LaunchSecretHexLen)

	resp, line := b.hello(t, "svc-forged", forged)
	assertBridgeUnauthorized(t, resp, "forged Hello")
	if strings.Contains(line, forged) || strings.Contains(line, secret) {
		t.Fatalf("a refused Hello echoed a secret: %s", line)
	}
	if _, ok := b.launches.Lookup(selfPeerToken(t)); ok {
		t.Fatal("a forged Hello bound an identity")
	}
	reqs := serviceOperationRequests(t, "svc-forged")
	resp, _ = b.send(t, reqs[bridge.ReqRegisterManifest])
	assertBridgeUnauthorized(t, resp, "RegisterManifest after a forged Hello")

	resp, _ = b.hello(t, "no-such-launch", secret)
	assertBridgeUnauthorized(t, resp, "Hello naming a launch that does not exist")
}

func TestHello_ASecondHelloWithTheRightSecretIsRefused(t *testing.T) {
	b := startIdentityBridge(t)
	secret, _ := b.begin(t, "svc-twice", capsBridge)

	if resp, line := b.hello(t, "svc-twice", secret); resp.Type != bridge.RespOK {
		t.Fatalf("first Hello: %s", line)
	}
	resp, line := b.hello(t, "svc-twice", secret)
	assertBridgeUnauthorized(t, resp, "second Hello with the right secret")
	if strings.Contains(line, secret) {
		t.Fatal("the refusal echoed the secret")
	}
	if _, ok := b.launches.Lookup(selfPeerToken(t)); !ok {
		t.Fatal("the refused second Hello disturbed the first binding")
	}
}

func TestBridge_ATokenlessRequestFromAnUnboundPeerIsNotAService(t *testing.T) {
	b := startIdentityBridge(t)
	b.begin(t, "svc-unbound", capsBridge)

	for op, req := range serviceOperationRequests(t, "svc-unbound") {
		resp, _ := b.send(t, req)
		assertBridgeUnauthorized(t, resp, op+" from a peer that never said Hello")
	}
	if b.enhanced.Get("svc-unbound") != nil {
		t.Fatal("an unbound peer registered a manifest")
	}

	// A tokenless caller that is neither a bound identity nor a session
	// member is refused outright. The Cwd is sent deliberately: it must buy
	// nothing, and must not appear in the refusal either.
	resp, line := b.send(t, bridge.BridgeRequest{Type: bridge.ReqListTools, Cwd: t.TempDir()})
	assertBridgeUnauthorized(t, resp, "tokenless ListTools")
	if strings.Contains(resp.Message, "working directory") || strings.Contains(line, "cwd") {
		t.Fatalf("the refusal still reasons about a caller-asserted directory: %q", line)
	}
}

func TestBridge_AFrontendOnlyIdentityIsRefusedEveryServiceOperation(t *testing.T) {
	b := startIdentityBridge(t)
	secret, _ := b.begin(t, "eve-like", capsFrontend)
	if resp, line := b.hello(t, "eve-like", secret); resp.Type != bridge.RespOK {
		t.Fatalf("Hello: %s", line)
	}

	for op, req := range serviceOperationRequests(t, "eve-like") {
		resp, _ := b.send(t, req)
		assertBridgeUnauthorized(t, resp, op+" from a frontend-only service")
	}
	if b.enhanced.Get("eve-like") != nil {
		t.Fatal("a frontend-only service registered a manifest")
	}
}

func TestBridge_ABridgeServiceIdentityAuthenticatesLaterTokenlessConnections(t *testing.T) {
	b := startIdentityBridge(t)
	secret, _ := b.begin(t, "llm-like", capsBridge)
	if resp, line := b.hello(t, "llm-like", secret); resp.Type != bridge.RespOK {
		t.Fatalf("Hello: %s", line)
	}

	reqs := serviceOperationRequests(t, "llm-like")
	if resp, line := b.send(t, reqs[bridge.ReqRegisterManifest]); resp.Type != bridge.RespOK {
		t.Fatalf("RegisterManifest by identity on a new connection: %s", line)
	}
	if b.enhanced.Get("llm-like") == nil {
		t.Fatal("the manifest never reached the registry")
	}

	withToken := reqs[bridge.ReqRegisterManifest]
	withToken.Token = "not-an-identity"
	resp, _ := b.send(t, withToken)
	assertBridgeUnauthorized(t, resp, "RegisterManifest presenting a token")

	someoneElse := serviceOperationRequests(t, "someone-else")[bridge.ReqRegisterManifest]
	resp, _ = b.send(t, someoneElse)
	assertBridgeUnauthorized(t, resp, "RegisterManifest under another service's id")
	if b.enhanced.Get("someone-else") != nil {
		t.Fatal("a service registered a manifest under an id that is not its own")
	}
}

func TestBridge_TheIdentityIsClearedWhenTheLaunchEnds(t *testing.T) {
	b := startIdentityBridge(t)
	secret, launch := b.begin(t, "svc-ends", capsBridge)
	if resp, line := b.hello(t, "svc-ends", secret); resp.Type != bridge.RespOK {
		t.Fatalf("Hello: %s", line)
	}
	reqs := serviceOperationRequests(t, "svc-ends")
	if resp, line := b.send(t, reqs[bridge.ReqRegisterManifest]); resp.Type != bridge.RespOK {
		t.Fatalf("RegisterManifest while live: %s", line)
	}
	launch.End()
	resp, _ := b.send(t, reqs[bridge.ReqRegisterManifest])
	assertBridgeUnauthorized(t, resp, "RegisterManifest after the launch ended")
}

// TestResolveAuth_BridgeServiceIdentity pins the C1 deletion directly: a
// service's launch identity no longer resolves to a tool-calling actor over
// the bridge at all (OpServiceTools is gone). C3's step 2 refuses it by name
// — a service identity is a known principal holding no project authority,
// and it never continues to the membership step beside it.
func TestResolveAuth_BridgeServiceIdentity(t *testing.T) {
	r := newTestRouter(t, makeSettings(nil, nil, nil), mcpbroker.NewManager(nil))

	svcCtx := bindTestServiceIdentity(t, r)
	if auth, err := r.resolveAuth(svcCtx, "", service.OpProjectTools); err == nil {
		t.Fatalf("a bound service identity reached tokenless tool auth as %+v; that path was retired with OpServiceTools", auth.stored)
	}
	if _, err := r.resolveAuth(svcCtx, "not-a-project-token", service.OpProjectTools); err == nil {
		t.Fatal("a token beside a bound identity must be judged as a token")
	}

	feCtx := bindTestIdentity(t, r, "eve-like", capsFrontend)
	if auth, err := r.resolveAuth(feCtx, "", service.OpProjectTools); err == nil {
		t.Fatalf("a frontend-only service authenticated on the bridge as %q", auth.stored.Name)
	}
}

// startIdentityFrontend brings up a frontend server whose store holds no
// credential at all, so anything admitted is admitted by identity.
func startIdentityFrontend(t *testing.T, enhanced *EnhancedServiceRegistry) (*FrontendServer, *service.Launches, config.SettingsStore) {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	launches := service.NewLaunches()
	extMgr := mcpbroker.NewManager(nil)
	sock := filepath.Join(mkShortTempDir(t, "fe-id-"), "frontend.sock")
	srv, err := NewFrontendServer(store, extMgr, extMgr, extMgr, Endpoint{Socket: sock}, enhanced,
		nil, nil, &ServiceOps{Store: store, Registry: &svcRecorder{}}, nil, nil, nil, nil, nil, nil, nil, nil,
		NewCredentialAuthorizer(store), nil, launches, sessionRouteDeps{})
	assertNoErr(t, err, "NewFrontendServer")
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	_ = dialUnixWithTimeout(t, sock, 2*time.Second).Close()
	if n := len(store.Get().APICredentials); n != 0 {
		t.Fatalf("fixture holds %d credentials; it must hold none", n)
	}
	return srv, launches, store
}

// bindSelf binds a launch to this test process's real audit token.
func bindSelf(t *testing.T, launches *service.Launches, name string, caps []config.ServiceCapability) *service.Launch {
	t.Helper()
	secret, launch, err := launches.Begin(service.Identity{Kind: service.IdentityKindService, Name: name, Capabilities: caps})
	assertNoErr(t, err, "Begin")
	_, err = launches.Bind(name, secret, selfPeerToken(t))
	assertNoErr(t, err, "Bind")
	return launch
}

func frontendStatus(t *testing.T, client *http.Client, method, url, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader("{}"))
	assertNoErr(t, err, "new request")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	assertNoErr(t, err, "%s %s", method, url)
	resp.Body.Close()
	return resp.StatusCode
}

func TestFrontend_AConsumerIdentityAuthenticatesWithNoAuthorizationHeader(t *testing.T) {
	srv, launches, _ := startIdentityFrontend(t, NewEnhancedServiceRegistry(nil))
	client := dialFrontendHTTP(srv.socketPath)

	if got := frontendStatus(t, client, "GET", "http://unix/api/services", ""); got != http.StatusUnauthorized {
		t.Fatalf("before any identity: status = %d, want 401", got)
	}

	launch := bindSelf(t, launches, "eve-like", capsFrontend)
	if got := frontendStatus(t, client, "GET", "http://unix/api/services", ""); got != http.StatusOK {
		t.Fatalf("read-class route by identity: status = %d, want 200", got)
	}
	if got := frontendStatus(t, client, "POST", "http://unix/api/projects/no-such/rotate_token", ""); got != http.StatusForbidden {
		t.Fatalf("grant-class route by identity: status = %d, want 403", got)
	}
	if got := frontendStatus(t, client, "GET", "http://unix/api/services", "unknown-bearer"); got != http.StatusUnauthorized {
		t.Fatalf("a presented bearer is always judged as a bearer: status = %d, want 401", got)
	}

	launch.End()
	if got := frontendStatus(t, client, "GET", "http://unix/api/services", ""); got != http.StatusUnauthorized {
		t.Fatalf("after the launch ended, on the same client: status = %d, want 401", got)
	}
}

func TestFrontend_ABridgeServiceIdentityIsRefused(t *testing.T) {
	srv, launches, _ := startIdentityFrontend(t, NewEnhancedServiceRegistry(nil))
	bindSelf(t, launches, "llm-like", capsBridge)
	if got := frontendStatus(t, dialFrontendHTTP(srv.socketPath), "GET", "http://unix/api/services", ""); got != http.StatusUnauthorized {
		t.Fatalf("a service without the frontend capability on the frontend socket: status = %d, want 401", got)
	}
}

// Eve dials the frontend socket with no header and must reach an enhanced
// service's proxied routes; the loopback TCP bind has no peer audit token and
// reaches nothing by identity.
func TestFrontend_AConsumerIdentityReachesTheProxiedSurfaceOverTheSocketOnly(t *testing.T) {
	registry := NewEnhancedServiceRegistry(nil)
	fake := NewFakeService(t, FakeServiceOptions{ServiceID: "id-svc", Manifest: newManifest("/api/a/")})
	assertNoErr(t, registry.RegisterManifest(fake.ServiceID(), fake.Socket(), fake.Token(), fake.Manifest()), "register manifest")
	srv, launches, _ := startIdentityFrontend(t, registry)
	assertNoErr(t, srv.ListenLoopback("127.0.0.1:0"), "ListenLoopback")
	go func() { _ = srv.ServeLoopback() }()
	bindSelf(t, launches, "eve-like", capsFrontend)

	if got := frontendStatus(t, dialFrontendHTTP(srv.socketPath), "POST", "http://unix/api/a/echo", ""); got != http.StatusOK {
		t.Fatalf("proxied route by identity over the socket: status = %d, want 200", got)
	}
	if got := fake.LastRequest(); got == nil || got.Path != "/api/a/echo" {
		t.Fatalf("the enhanced service never saw the request: %+v", got)
	}

	before := len(fake.Requests())
	if got := frontendStatus(t, http.DefaultClient, "POST", "http://"+srv.tcpLn.Addr().String()+"/api/a/echo", ""); got != http.StatusUnauthorized {
		t.Fatalf("headerless request over loopback TCP: status = %d, want 401", got)
	}
	if after := len(fake.Requests()); after != before {
		t.Fatal("a headerless TCP request reached the enhanced service")
	}
}

// TestFrontendCapability_HoldsExactlyReadConfigureProxyAndExecute pins the
// approved F1/SP8 decision (plan-broker-and-sessions.md, "Decisions on this
// plan"): a frontend launch identity now holds control.ClassExecute too, so
// eve can reach the session-host launch routes once R-S4b registers them.
// control.ClassGrant remains refused — nothing on the frontend socket ever
// grants that to a launch identity, only to a bearer credential naming it.
func TestFrontendCapability_HoldsExactlyReadConfigureProxyAndExecute(t *testing.T) {
	authz := NewCredentialAuthorizer(newCLISandboxStore(t))
	id := service.Identity{Kind: service.IdentityKindService, Name: "eve-like", Capabilities: capsFrontend}
	for class, want := range map[control.CapabilityClass]error{
		control.ClassRead:      nil,
		control.ClassConfigure: nil,
		control.ClassProxy:     nil,
		control.ClassExecute:   nil,
		control.ClassGrant:     control.ErrClassNotGranted,
	} {
		r := httptest.NewRequest("GET", "/x", nil)
		r = r.WithContext(withFrontendIdentity(r.Context(), id))
		if err := authz.Authorize(r, class); !errors.Is(err, want) || (want == nil && err != nil) {
			t.Errorf("class %s: err = %v, want %v", class, err, want)
		}
		if credID, _ := APICredentialIDFromContext(r.Context()); credID != "launch:service:eve-like" {
			t.Errorf("class %s: decision attributed to %q", class, credID)
		}
	}
}

func TestFrontendEnv_CarriesOnlyTheSocketPath(t *testing.T) {
	got := Endpoint{Socket: "/tmp/fe.sock"}.FrontendEnv()
	if len(got) != 1 || got[EnvFrontendSocket] != "/tmp/fe.sock" {
		t.Fatalf("FrontendEnv = %v, want only %s", got, EnvFrontendSocket)
	}
}

func TestRetireLegacyFrontendCredential_DeletesOnlyTheReservedName(t *testing.T) {
	s := &config.Settings{}
	addAPICredential(s, config.APICredential{ID: "legacy", Name: legacyFrontendCredentialName, Hash: config.HashToken("once-in-an-environment"), Classes: frontendConsumerClasses})
	addAPICredential(s, config.APICredential{ID: "other", Name: "other", Hash: config.HashToken("other"), Classes: []control.CapabilityClass{control.ClassRead}})

	if !retireLegacyFrontendCredential(s) {
		t.Fatal("retirement reported nothing deleted")
	}
	if authenticateAPICredential(s, "once-in-an-environment") != nil {
		t.Fatal("the retired bearer still authenticates")
	}
	if findAPICredential(s, "other") == nil {
		t.Fatal("retirement deleted an unrelated credential")
	}
	if retireLegacyFrontendCredential(s) {
		t.Fatal("a second retirement reported a change")
	}
}

func TestRetireLegacyFrontendCredentialOnStart_RemovesThePersistedRecord(t *testing.T) {
	store := newCLISandboxStore(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		addAPICredential(s, config.APICredential{ID: "legacy", Name: legacyFrontendCredentialName, Hash: config.HashToken("once-in-an-environment"), Classes: frontendConsumerClasses})
	}), "seed legacy record")

	retireLegacyFrontendCredentialOnStart(store)

	if authenticateAPICredential(config.FreshSettings(store), "once-in-an-environment") != nil {
		t.Fatal("the retired bearer still authenticates after a start")
	}
}

func TestRetireLegacyFrontendCredentialOnStart_WritesNothingWhenThereIsNothingToRetire(t *testing.T) {
	dir, store := odwSandbox(t)
	before := odwSnap(t, dir)
	retireLegacyFrontendCredentialOnStart(store)
	before.assertUntouched(t, dir, "retireLegacyFrontendCredentialOnStart")
}
