package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
)

func newModelEndpointTestServer(t *testing.T) (*ModelEndpointServer, config.SettingsStore, *service.Launches, *ModelHostRegistry) {
	t.Helper()
	store := sealedSettingsStoreAt(mkEmptySandboxRelayHome(t))
	assertNoErr(t, store.EnsureInitialized(), "EnsureInitialized")
	launches := service.NewLaunches()
	hosts := NewModelHostRegistry(launches)
	keys := NewModelKeyTable()
	return NewModelEndpointServer(store, launches, keys, hosts), store, launches, hosts
}

func addModelProject(t *testing.T, store config.SettingsStore, id string, allowedModels []string, remote bool) string {
	t.Helper()
	token := "proj-token-" + id
	hash := config.HashToken(token)
	kind := config.ProjectKindLocal
	if remote {
		kind = config.ProjectKindRemote
	}
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.Projects = append(s.Projects, config.Project{
			ID: id, Name: id, Kind: kind, AllowedModels: allowedModels,
			Token: config.NewSecret(token), TokenHash: hash,
		})
	}), "add project")
	return token
}

func addModelServiceRecord(t *testing.T, store config.SettingsStore, id string, allowedModels []string) {
	t.Helper()
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.Services = append(s.Services, config.ServiceConfig{ID: id, DisplayName: id, Command: "/bin/true", AllowedModels: allowedModels})
	}), "add service")
}

// bindModelIdentity binds a launch named name holding caps and returns a
// context carrying that peer, for calling the endpoint's handler directly
// (no real socket needed: the handler reads the peer straight out of the
// request context).
func bindModelIdentity(t *testing.T, launches *service.Launches, name string, caps []config.ServiceCapability, pid int32) context.Context {
	t.Helper()
	secret, _, err := launches.Begin(service.Identity{Kind: service.IdentityKindService, Name: name, Capabilities: caps})
	assertNoErr(t, err, "Begin")
	peer := peertoken.ForProcessForTest(pid, 1)
	_, err = launches.Bind(name, secret, peer)
	assertNoErr(t, err, "Bind")
	return bridge.WithCallerPeer(context.Background(), peer)
}

func newFakeRouterSocket(t *testing.T, handler http.Handler) string {
	t.Helper()
	sock := filepath.Join(mkShortTempDir(t, "fake-router-"), "router.sock")
	ln, err := net.Listen("unix", sock)
	assertNoErr(t, err, "listen")
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return sock
}

// registerFakeHost begins and binds a model_host launch, then registers it,
// so a real dial to sock is verified against a real, live launch.
func registerFakeHost(t *testing.T, hosts *ModelHostRegistry, launches *service.Launches, serviceID, sock string, process peertoken.Process) *service.Launch {
	t.Helper()
	secret, launch, err := launches.Begin(service.Identity{Kind: service.IdentityKindService, Name: serviceID, Capabilities: []config.ServiceCapability{config.ServiceCapabilityModelHost}})
	assertNoErr(t, err, "Begin")
	peer := peertoken.ForProcessForTest(process.PID, process.PIDVersion)
	_, err = launches.Bind(serviceID, secret, peer)
	assertNoErr(t, err, "Bind")
	assertNoErr(t, hosts.Register(serviceID, sock, process), "Register")
	return launch
}

const fakeCatalogJSON = `{"object":"list","data":[{"id":"vCode","owned_by":"virtual"},{"id":"omlx/Chat","owned_by":"mlx-omni"}]}`

func fakeRouterMux(t *testing.T, chatHandler http.HandlerFunc) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeCatalogJSON))
	})
	if chatHandler != nil {
		mux.HandleFunc("/v1/chat/completions", chatHandler)
	}
	return mux
}

// ---- resolveCaller auth matrix ----

func TestModelEndpoint_ResolveCaller_AuthMatrix(t *testing.T) {
	m, store, launches, _ := newModelEndpointTestServer(t)
	projTok := addModelProject(t, store, "p1", nil, false) // unrestricted
	keys := m.modelKeys
	modelKey, err := keys.Mint("p1", "session:x")
	assertNoErr(t, err, "Mint")
	keys.Revoke("dead-label")
	revoked, err := keys.Mint("p1", "dead-label")
	assertNoErr(t, err, "Mint revoked")
	keys.Revoke("dead-label")

	withHeaders := func(auth, apiKey string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		if apiKey != "" {
			r.Header.Set("x-api-key", apiKey)
		}
		return r
	}

	serviceCtx := bindModelIdentity(t, launches, "tts", []config.ServiceCapability{config.ServiceCapabilityModels}, 51001)
	noModelsCtx := bindModelIdentity(t, launches, "sched", []config.ServiceCapability{config.ServiceCapabilityManifest}, 51002)

	cases := []struct {
		name      string
		transport string
		req       *http.Request
		ctx       context.Context
		wantOK    bool
		wantKind  string
		wantAuth  string
	}{
		{"socket/no-header/no-identity", transportSocket, withHeaders("", ""), context.Background(), false, "", ""},
		{"socket/no-header/service-with-models", transportSocket, withHeaders("", ""), serviceCtx, true, "service", "identity"},
		{"socket/no-header/service-without-models", transportSocket, withHeaders("", ""), noModelsCtx, false, "", ""},
		{"socket/bad-bearer", transportSocket, withHeaders("Bearer garbage", ""), context.Background(), false, "", ""},
		{"socket/project-token", transportSocket, withHeaders("Bearer "+projTok, ""), context.Background(), true, "project", "token"},
		{"socket/model-key", transportSocket, withHeaders("Bearer "+modelKey, ""), context.Background(), true, "project", "model_key"},
		{"socket/revoked-model-key", transportSocket, withHeaders("Bearer "+revoked, ""), context.Background(), false, "", ""},
		{"socket/identity-plus-bearer-uses-bearer", transportSocket, withHeaders("Bearer "+projTok, ""), serviceCtx, true, "project", "token"},
		{"socket/conflicting-x-api-key", transportSocket, withHeaders("Bearer "+projTok, "something-else"), context.Background(), false, "", ""},
		{"tcp/no-header", transportTCP, withHeaders("", ""), context.Background(), false, "", ""},
		{"tcp/project-token", transportTCP, withHeaders("Bearer "+projTok, ""), context.Background(), true, "project", "token"},
		{"tcp/identity-is-irrelevant", transportTCP, withHeaders("", ""), serviceCtx, false, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req.WithContext(tc.ctx)
			caller, errBody := m.resolveCaller(req, tc.transport)
			if tc.wantOK {
				if errBody != nil {
					t.Fatalf("got error %+v, want success", errBody)
				}
				if caller.kind != tc.wantKind || caller.auth != tc.wantAuth {
					t.Fatalf("caller = %+v, want kind=%s auth=%s", caller, tc.wantKind, tc.wantAuth)
				}
			} else if errBody == nil {
				t.Fatalf("got success %+v, want a 401", caller)
			} else if errBody.Status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", errBody.Status)
			}
		})
	}
}

// ---- full-handler scoping tests ----

func doHandlerRequest(t *testing.T, h http.Handler, method, path, bearer string, ctx context.Context, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestModelEndpoint_TCPTokenlessAlwaysUnauthorized(t *testing.T) {
	m, _, _, _ := newModelEndpointTestServer(t)
	w := doHandlerRequest(t, m.Handler(transportTCP), http.MethodGet, "/v1/models", "", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestModelEndpoint_RemoteProjectForbidden(t *testing.T) {
	m, store, _, _ := newModelEndpointTestServer(t)
	tok := addModelProject(t, store, "remote1", nil, true)
	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodGet, "/v1/models", tok, nil, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

func TestModelEndpoint_ServiceAllowedModelsEmptyDeniesWildcardAllows(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())

	addModelServiceRecord(t, store, "tts-empty", nil)
	emptyCtx := bindModelIdentity(t, launches, "tts-empty", []config.ServiceCapability{config.ServiceCapabilityModels}, 52001)
	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", "", emptyCtx, `{"model":"vCode"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("empty AllowedModels: status = %d, want 404; body=%s", w.Code, w.Body.String())
	}

	addModelServiceRecord(t, store, "tts-wild", []string{"*"})
	wildCtx := bindModelIdentity(t, launches, "tts-wild", []config.ServiceCapability{config.ServiceCapabilityModels}, 52002)
	w = doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", "", wildCtx, `{"model":"vCode"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("wildcard AllowedModels: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestModelEndpoint_ModelsListFiltered(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, nil))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "restricted", []string{"vCode"}, false)

	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodGet, "/v1/models", tok, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "omlx/Chat") {
		t.Fatalf("filtered list leaked a model outside the grant: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "vCode") {
		t.Fatalf("filtered list dropped the granted model: %s", w.Body.String())
	}
}

func TestModelEndpoint_NoHostUnavailable(t *testing.T) {
	m, store, _, _ := newModelEndpointTestServer(t)
	tok := addModelProject(t, store, "p1", nil, false)
	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodGet, "/v1/models", tok, nil, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestModelEndpoint_RouterSocketPeerMismatchIsUnavailable(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, nil))
	self := selfPeerToken(t).Process()
	mismatched := peertoken.ForProcessForTest(self.PID+123456, self.PIDVersion+7).Process()
	registerFakeHost(t, hosts, launches, "relayllm", sock, mismatched)

	tok := addModelProject(t, store, "p1", nil, false)
	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodGet, "/v1/models", tok, nil, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 on a router-socket peer mismatch; body=%s", w.Code, w.Body.String())
	}
}

func TestModelEndpoint_ForwardsWithHeaderStrippingAndTargetRemoval(t *testing.T) {
	var gotAuth, gotAPIKey, gotRelayHeader string
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		gotRelayHeader = r.Header.Get("X-Relay-Secret")
		w.Header().Set("X-Relay-Model-Target", "ep/gpt-x")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	var auditEv ModelCallAudit
	m.AuditHook = func(ev ModelCallAudit) { auditEv = ev }

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	// Same value as Authorization's bearer, not a conflicting credential —
	// this test is about the header being stripped before forwarding, not
	// about the conflicting-headers-are-401 rule (covered separately in the
	// auth matrix).
	r.Header.Set("x-api-key", tok)
	r.Header.Set("X-Relay-Secret", "should-not-reach-upstream-either")
	w := httptest.NewRecorder()
	m.Handler(transportSocket).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if gotAuth != "" {
		t.Fatalf("upstream saw Authorization: %q", gotAuth)
	}
	if gotAPIKey != "" {
		t.Fatalf("upstream saw x-api-key: %q", gotAPIKey)
	}
	if gotRelayHeader != "" {
		t.Fatalf("upstream saw an x-relay-* header: %q", gotRelayHeader)
	}
	if w.Header().Get("X-Relay-Model-Target") != "" {
		t.Fatal("X-Relay-Model-Target reached the client")
	}
	if auditEv.Target != "ep/gpt-x" {
		t.Fatalf("audit Target = %q, want ep/gpt-x", auditEv.Target)
	}
	if auditEv.Usage.PromptTokens != 3 || auditEv.Usage.CompletionTokens != 4 {
		t.Fatalf("audit usage = %+v", auditEv.Usage)
	}
	if auditEv.Outcome != "ok" {
		t.Fatalf("audit outcome = %q, want ok", auditEv.Outcome)
	}
}

func TestModelEndpoint_SSEFirstByteFlushedBeforeUpstreamFinishes(t *testing.T) {
	release := make(chan struct{})
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	server := httptest.NewServer(m.Handler(transportSocket))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
	assertNoErr(t, err, "new request")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	assertNoErr(t, err, "do request")
	defer resp.Body.Close()

	type readResult struct {
		line string
		err  error
	}
	lineCh := make(chan readResult, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		line, err := br.ReadString('\n')
		lineCh <- readResult{line, err}
	}()

	select {
	case res := <-lineCh:
		assertNoErr(t, res.err, "read first SSE line")
		if !strings.Contains(res.line, "choices") {
			t.Fatalf("first line = %q", res.line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first SSE chunk was not flushed before the upstream finished")
	}
	close(release)
}

func TestModelEndpoint_ClientAbortCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		// This is subtle: net/http only detects a client disconnect while a
		// handler is blocked if the connection's read side isn't sitting on
		// an unconsumed body — an unread body suppresses the background
		// read that would otherwise notice the peer went away. A real
		// upstream (relayLLM) always reads the body to do its work; drain it
		// here too, or r.Context() never observes the cancellation this test
		// exists to prove.
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(5 * time.Second):
		}
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	server := httptest.NewServer(m.Handler(transportSocket))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
	assertNoErr(t, err, "new request")
	req.Header.Set("Authorization", "Bearer "+tok)

	go func() { _, _ = http.DefaultClient.Do(req) }()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream handler never started")
	}
	cancel()

	// The client's own Do() call returns as soon as it locally notices its
	// context is canceled, with no need to wait on the network — that alone
	// proves nothing about the upstream. What must be observed is the fake
	// router's OWN handler seeing its request context canceled, which
	// requires the full chain (relay's RoundTrip aborts -> the connection to
	// the fake router closes -> the fake router's server notices and cancels
	// its handler's context) to complete, hence the longer allowance here.
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("client abort did not cancel the upstream request")
	}
}

func TestModelEndpoint_ReconcileRefusesNonLoopback(t *testing.T) {
	m, _, _, _ := newModelEndpointTestServer(t)
	t.Setenv(EnvModelListen, "0.0.0.0:0")
	m.Reconcile()
	if m.tcpAddr != "" || m.tcpLn != nil {
		t.Fatal("a non-loopback listen address was bound")
	}
}

func TestModelEndpoint_ReconcileBindsAndClosesLoopback(t *testing.T) {
	m, _, _, _ := newModelEndpointTestServer(t)
	t.Setenv(EnvModelListen, "127.0.0.1:0")
	m.Reconcile()
	if m.tcpLn == nil {
		t.Fatal("a loopback listen address was refused")
	}
	t.Setenv(EnvModelListen, "")
	m.Reconcile()
	if m.tcpLn != nil || m.tcpAddr != "" {
		t.Fatal("clearing the listen address left a listener bound")
	}
}

func TestModelEndpoint_UnknownCapabilityStillRefused(t *testing.T) {
	m, store, launches, _ := newModelEndpointTestServer(t)
	_ = store
	ctx := bindModelIdentity(t, launches, "weird", []config.ServiceCapability{"bogus"}, 53001)
	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodGet, "/v1/models", "", ctx, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an identity with only an unknown capability", w.Code)
	}
}
