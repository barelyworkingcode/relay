package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/modelbroker"
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

// countingListener tracks how many accepted connections are currently open,
// and the peak seen, for S1's fd-leak regression test: a fresh
// keep-alive-enabled Transport per call would hold each connection open
// for IdleConnTimeout with nothing left able to reuse it, so the peak
// concurrent count would climb roughly linearly with the number of calls
// made in quick succession. DisableKeepAlives keeps it flat.
type countingListener struct {
	net.Listener
	mu      sync.Mutex
	current int
	peak    int
}

func (c *countingListener) Accept() (net.Conn, error) {
	conn, err := c.Listener.Accept()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.current++
	if c.current > c.peak {
		c.peak = c.current
	}
	c.mu.Unlock()
	return &countingConn{Conn: conn, l: c}, nil
}

func (c *countingListener) Snapshot() (current, peak int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current, c.peak
}

type countingConn struct {
	net.Conn
	l    *countingListener
	once sync.Once
}

func (c *countingConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.l.mu.Lock()
		c.l.current--
		c.l.mu.Unlock()
	})
	return err
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
	projTok := addModelProject(t, store, "p1", nil, false)      // unrestricted
	otherProjTok := addModelProject(t, store, "p2", nil, false) // unrestricted, DIFFERENT project
	keys := m.modelKeys
	modelKey, err := keys.Mint("p1", "session:x")
	assertNoErr(t, err, "Mint")
	keys.Revoke("p1", "dead-label")
	revoked, err := keys.Mint("p1", "dead-label")
	assertNoErr(t, err, "Mint revoked")
	keys.Revoke("p1", "dead-label")

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
		// This case must be a real conflict of two INDEPENDENTLY VALID
		// credentials, over a connection that ALSO holds a bound identity
		// (S3/S4 of the security review): "something-else" in x-api-key
		// would 401 on its own regardless of whether the conflict check
		// ever ran, so it can't prove the check fired. With two different
		// projects' real tokens plus a bound identity, any of the three
		// wrong behaviours a broken conflict check could produce --
		// silently preferring Authorization (succeeds as p1), silently
		// preferring x-api-key (succeeds as p2), or treating "conflicting"
		// as "absent" (falls through and succeeds as the identity) --
		// shows up as a 200 instead of the 401 only a real conflict
		// refusal produces.
		{"socket/conflicting-x-api-key", transportSocket, withHeaders("Bearer "+projTok, otherProjTok), serviceCtx, false, "", ""},
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

// TestModelEndpoint_PresentButBlankHeaderIsRefusedNeverFallsThroughToIdentity
// is S4's proof: a header sent with an empty or whitespace-only value is
// still "present" (spec §3.1 — a present header is judged as a bearer,
// never as the identity beside it) and must 401, even on the socket with a
// bound identity that would otherwise succeed.
func TestModelEndpoint_PresentButBlankHeaderIsRefusedNeverFallsThroughToIdentity(t *testing.T) {
	m, _, launches, _ := newModelEndpointTestServer(t)
	serviceCtx := bindModelIdentity(t, launches, "tts-blank", []config.ServiceCapability{config.ServiceCapabilityModels}, 54001)

	cases := []struct {
		name   string
		modify func(r *http.Request)
	}{
		{"blank Authorization", func(r *http.Request) { r.Header.Set("Authorization", "") }},
		{"whitespace Authorization", func(r *http.Request) { r.Header.Set("Authorization", "   ") }},
		{"blank x-api-key", func(r *http.Request) { r.Header.Set("x-api-key", "") }},
		{"whitespace x-api-key", func(r *http.Request) { r.Header.Set("x-api-key", "   ") }},
		{"two Authorization values", func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer aaaa")
			r.Header.Add("Authorization", "Bearer bbbb")
		}},
		{"two x-api-key values", func(r *http.Request) {
			r.Header.Add("x-api-key", "aaaa")
			r.Header.Add("x-api-key", "bbbb")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			tc.modify(r)
			r = r.WithContext(serviceCtx)
			caller, errBody := m.resolveCaller(r, transportSocket)
			if errBody == nil {
				t.Fatalf("%s: got success %+v, want 401 (must not fall through to the bound identity)", tc.name, caller)
			}
			if errBody.Status != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d, want 401", tc.name, errBody.Status)
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

// TestModelEndpoint_SessionsCapabilityGetsTheUnfilteredListButNoCalls pins
// plan-broker-and-sessions.md §2 C1: "sessions gets the unfiltered list and
// no calls" — even with no AllowedModels set (empty means none for a
// service, MB decision 4), a sessions-capability identity's GET /v1/models
// still sees every model, and a call attempt is still refused.
func TestModelEndpoint_SessionsCapabilityGetsTheUnfilteredListButNoCalls(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())

	addModelServiceRecord(t, store, "relaysessions", nil) // no AllowedModels at all
	ctx := bindModelIdentity(t, launches, "relaysessions", []config.ServiceCapability{config.ServiceCapabilitySessions}, 52003)

	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodGet, "/v1/models", "", ctx, "")
	if w.Code != http.StatusOK {
		t.Fatalf("list: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "vCode") || !strings.Contains(w.Body.String(), "omlx/Chat") {
		t.Fatalf("sessions capability did not get the unfiltered list: %s", w.Body.String())
	}

	w = doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", "", ctx, `{"model":"vCode"}`)
	if w.Code == http.StatusOK {
		t.Fatalf("sessions capability made a model call: status = %d, body=%s", w.Code, w.Body.String())
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

// TestModelEndpoint_UpstreamConnectionsDoNotAccumulate is S1's regression
// test: measured before the fix, 200 sequential calls left ~408 open fds
// (a fresh keep-alive Transport per call, each holding its own idle
// connection for 90s with nothing left able to reuse it). With
// DisableKeepAlives, the peak number of connections concurrently open on
// the fake upstream should never exceed a small constant regardless of how
// many sequential calls are made.
func TestModelEndpoint_UpstreamConnectionsDoNotAccumulate(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sockPath := filepath.Join(mkShortTempDir(t, "fake-router-"), "router.sock")
	ln, err := net.Listen("unix", sockPath)
	assertNoErr(t, err, "listen")
	cl := &countingListener{Listener: ln}
	srv := &http.Server{Handler: fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})}
	go func() { _ = srv.Serve(cl) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	registerFakeHost(t, hosts, launches, "relayllm", sockPath, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	const calls = 50
	for i := 0; i < calls; i++ {
		w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, `{"model":"vCode"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, body=%s", i, w.Code, w.Body.String())
		}
	}

	// Give any not-yet-closed connection a moment to finish closing rather
	// than racing the assertion against the last call's own teardown.
	deadline := time.Now().Add(2 * time.Second)
	var current, peak int
	for time.Now().Before(deadline) {
		current, peak = cl.Snapshot()
		if current == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if current != 0 {
		t.Fatalf("%d connections to the upstream are still open after %d sequential calls", current, calls)
	}
	// A generous bound: with DisableKeepAlives, at most a couple of
	// connections should ever be concurrently open (in-flight overlap
	// between one call's close and the next's accept), never one per call.
	if peak > 5 {
		t.Fatalf("peak concurrent upstream connections = %d over %d calls, want a small constant (connections are not being closed promptly)", peak, calls)
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

// TestModelEndpoint_B1_DuplicateJSONKeyNeverReachesUpstream is B1/B3's
// end-to-end proof, against the REAL handler and a fake upstream over a
// real Unix socket, asserting on what the upstream actually received (or
// didn't) rather than on the response status alone: relayLLM's own
// json.Unmarshal is case-insensitive and takes the LAST matching key, so
// before this fix {"model":"vCode","model":"omlx/Chat"} and
// {"model":"vCode","Model":"omlx/Chat"} were approved against "vCode"
// (the grant) and forwarded verbatim, landing on "omlx/Chat" upstream — a
// grant of exactly ["vCode"] would have been silently bypassed.
func TestModelEndpoint_B1_DuplicateJSONKeyNeverReachesUpstream(t *testing.T) {
	var receivedBodies [][]byte
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBodies = append(receivedBodies, b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", []string{"vCode"}, false)

	cases := map[string]string{
		"exact duplicate": `{"model":"vCode","model":"omlx/Chat"}`,
		"case-variant":    `{"model":"vCode","Model":"omlx/Chat"}`,
		"upper duplicate": `{"model":"vCode","MODEL":"omlx/Chat"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			receivedBodies = nil
			w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400; body=%s", name, w.Code, w.Body.String())
			}
			if len(receivedBodies) != 0 {
				t.Fatalf("%s: the upstream received %d request(s) for a body relay should have refused before forwarding: %q",
					name, len(receivedBodies), receivedBodies)
			}
		})
	}
}

// TestModelEndpoint_B2_DuplicateMultipartPartNeverReachesUpstream is B2's
// mirror of the JSON test above: relayLLM's multipart reader keeps the
// LAST same-named part, so a duplicate "model" part must never reach it
// either.
func TestModelEndpoint_B2_DuplicateMultipartPartNeverReachesUpstream(t *testing.T) {
	var calls int
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"transcribed"}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", []string{"vCode"}, false)

	body, boundary := buildMultipartWithFieldsForTest(t, [][2]string{{"model", "vCode"}, {"Model", "omlx/Chat"}})
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	r.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	m.Handler(transportSocket).ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if calls != 0 {
		t.Fatalf("the upstream received %d call(s) for a multipart body relay should have refused before forwarding", calls)
	}
}

// buildMultipartWithFieldsForTest mirrors internal/modelbroker's test
// helper of the same shape (unexported there), since cmd/relay cannot
// import a _test.go symbol across the package boundary.
func buildMultipartWithFieldsForTest(t *testing.T, fields [][2]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.Boundary()
}

// TestModelEndpoint_B1_NormalBodyForwardsCorrectlyWithOtherFieldsIntact is
// B3's positive case: a normal, single-key request still reaches the
// upstream with the canonical model value and every other field intact —
// the fix must not have turned forwarding into mush.
func TestModelEndpoint_B1_NormalBodyForwardsCorrectlyWithOtherFieldsIntact(t *testing.T) {
	var received []byte
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", []string{"vCode"}, false)

	body := `{"model":"vCode","messages":[{"role":"user","content":"hi"}],"temperature":0.5}`
	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var got map[string]json.RawMessage
	assertNoErr(t, json.Unmarshal(received, &got), "decode what the upstream received")
	if string(got["model"]) != `"vCode"` {
		t.Fatalf("upstream received model=%s, want \"vCode\"", got["model"])
	}
	var messages []map[string]string
	assertNoErr(t, json.Unmarshal(got["messages"], &messages), "decode messages")
	if len(messages) != 1 || messages[0]["content"] != "hi" {
		t.Fatalf("messages did not survive forwarding intact: %s", got["messages"])
	}
	if string(got["temperature"]) != "0.5" {
		t.Fatalf("temperature did not survive forwarding intact: %s", got["temperature"])
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
		w.Header().Set("X-Relay-Debug", "internal-detail")
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
	if w.Header().Get("X-Relay-Debug") != "" {
		t.Fatal("an x-relay-* response header other than the target reached the client")
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

// TestModelEndpoint_MidStreamAbortStillAudits is the mid-stream-abort nit's
// proof: a client that goes away AFTER response headers (and some body)
// have already been flushed never reaches ErrorHandler (that only fires
// for a RoundTrip failure, before any response is written) — net/http
// instead panics the handler goroutine with http.ErrAbortHandler on the
// next failed write to the client. Without the recover in proxy(), this
// case would produce no audit record at all.
func TestModelEndpoint_MidStreamAbortStillAudits(t *testing.T) {
	upstreamDone := make(chan struct{})
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		defer close(upstreamDone)
		for i := 0; i < 100; i++ {
			if _, err := w.Write([]byte("data: {\"choices\":[]}\n\n")); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	var mu sync.Mutex
	var auditEv ModelCallAudit
	var auditFired bool
	m.AuditHook = func(ev ModelCallAudit) {
		mu.Lock()
		defer mu.Unlock()
		auditEv, auditFired = ev, true
	}

	server := httptest.NewServer(m.Handler(transportSocket))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
	assertNoErr(t, err, "new request")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	assertNoErr(t, err, "do request")

	buf := make([]byte, 64)
	_, err = resp.Body.Read(buf)
	assertNoErr(t, err, "read first chunk")
	// Abrupt: close without draining to EOF, so the underlying connection
	// is torn down rather than the body being read out and the connection
	// returned to the pool.
	assertNoErr(t, resp.Body.Close(), "close response body")

	select {
	case <-upstreamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream never noticed the client went away")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		fired, ev := auditFired, auditEv
		mu.Unlock()
		if fired {
			if ev.Outcome != "client_abort" {
				t.Fatalf("audit outcome = %q, want client_abort", ev.Outcome)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no audit record was produced for a mid-stream client abort")
		}
		time.Sleep(10 * time.Millisecond)
	}
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

// setModelListenForTest sets the test-only Go override and restores it to ""
// on cleanup, so a test never leaks its override into a later one sharing
// the process (the override is a package-level var, not per-instance).
func setModelListenForTest(t *testing.T, addr string) {
	t.Helper()
	SetModelListenOverrideForTest(addr)
	t.Cleanup(func() { SetModelListenOverrideForTest("") })
}

func setModelEndpointSettingsListen(t *testing.T, store config.SettingsStore, addr string) {
	t.Helper()
	assertNoErr(t, store.With(func(s *config.Settings) {
		if addr == "" {
			s.ModelEndpoint = nil
			return
		}
		s.ModelEndpoint = &config.ModelEndpointConfig{Listen: addr}
	}), "set model_endpoint.listen")
}

func TestModelEndpoint_ReconcileRefusesNonLoopback(t *testing.T) {
	m, _, _, _ := newModelEndpointTestServer(t)
	setModelListenForTest(t, "0.0.0.0:0")
	m.Reconcile()
	if m.tcpAddr != "" || m.tcpLn != nil {
		t.Fatal("a non-loopback listen address was bound")
	}
}

func TestModelEndpoint_ReconcileBindsAndClosesLoopback(t *testing.T) {
	m, _, _, _ := newModelEndpointTestServer(t)
	setModelListenForTest(t, "127.0.0.1:0")
	m.Reconcile()
	if m.tcpLn == nil {
		t.Fatal("a loopback listen address was refused")
	}
	setModelListenForTest(t, "")
	m.Reconcile()
	if m.tcpLn != nil || m.tcpAddr != "" {
		t.Fatal("clearing the listen address left a listener bound")
	}
}

// TestModelEndpoint_ReconcileDrivenBySettingsStore is S5's positive case:
// Reconcile driven entirely by settings.json's model_endpoint.listen, with
// no test-only override in play at all.
func TestModelEndpoint_ReconcileDrivenBySettingsStore(t *testing.T) {
	m, store, _, _ := newModelEndpointTestServer(t)
	setModelEndpointSettingsListen(t, store, "127.0.0.1:0")
	m.Reconcile()
	if m.tcpLn == nil {
		t.Fatal("a loopback listen address from settings.json was refused")
	}

	setModelEndpointSettingsListen(t, store, "0.0.0.0:0")
	m.Reconcile()
	if m.tcpLn != nil || m.tcpAddr != "" {
		t.Fatal("a non-loopback listen address from settings.json was bound")
	}
}

// TestModelEndpoint_EnvVarHasNoEffect is S5's required proof: setting
// RELAY_MODEL_LISTEN in the process environment must do nothing at all —
// only settings.json (or the package-level test seam) can configure the
// listener.
func TestModelEndpoint_EnvVarHasNoEffect(t *testing.T) {
	m, store, _, _ := newModelEndpointTestServer(t)
	t.Setenv("RELAY_MODEL_LISTEN", "127.0.0.1:0")
	m.Reconcile()
	if m.tcpLn != nil || m.tcpAddr != "" {
		t.Fatal("RELAY_MODEL_LISTEN in the environment bound a listener")
	}
	_ = store
}

// TestModelEndpoint_RealSocketIdentityAuthEndToEnd is S3(a)'s proof: unlike
// every other test in this file (which forges the peer via
// bridge.WithCallerPeer directly on an in-memory request), this one binds
// ListenSocket for real, dials model.sock as a real Unix-socket client, and
// proves the connection's KERNEL-REPORTED peer audit token — read inside
// ListenSocket's ConnContext, never asserted by the test — is what
// resolveIdentity actually authenticates against. Deleting the ConnContext
// peer-token read (or wiring it to the wrong context key) fails this test:
// the identity is bound to THIS TEST PROCESS's real audit token
// (selfPeerToken), so if ConnContext never places a peer in the request
// context, resolveIdentity's lookup misses and every call here 401s
// instead of succeeding.
func TestModelEndpoint_RealSocketIdentityAuthEndToEnd(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, nil))

	// One identity, both capabilities: the fake router is served BY this
	// same test process, so whatever Launches binds "relayllm" to must
	// equal this process's own real peer token for BOTH the dial-
	// verification liveness check (ModelHostRegistry) and the model.sock
	// connection auth this test exists to prove — a real OS process can
	// only ever hold one bound identity (Launches.Bind refuses a second
	// name for the same peer), so a separate "caller" identity is not an
	// option here.
	addModelServiceRecord(t, store, "relayllm", []string{"*"})
	secret, _, err := launches.Begin(service.Identity{
		Kind: service.IdentityKindService, Name: "relayllm",
		Capabilities: []config.ServiceCapability{config.ServiceCapabilityModelHost, config.ServiceCapabilityModels},
	})
	assertNoErr(t, err, "Begin")
	self := selfPeerToken(t)
	_, err = launches.Bind("relayllm", secret, self)
	assertNoErr(t, err, "Bind")
	assertNoErr(t, hosts.Register("relayllm", sock, self.Process()), "Register")

	assertNoErr(t, m.ListenSocket(), "ListenSocket")
	go func() { _ = m.ServeSocket() }()
	t.Cleanup(m.Close)

	modelSock := bridge.ModelSocketPath()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialUnixWithTimeout(t, modelSock, 2*time.Second), nil
		},
	}}

	req, err := http.NewRequest(http.MethodGet, "http://model.sock/v1/models", nil)
	assertNoErr(t, err, "new request")
	resp, err := client.Do(req)
	assertNoErr(t, err, "do request")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (identity auth over the real socket); body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "vCode") {
		t.Fatalf("body missing the catalog: %s", body)
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

// ---- relay#116 re-review (R-M1b2): S6 memory bound ----

// buildJSONBodyOfSize builds `{"model":"<model>","padding":"aaa..."}` at
// exactly totalSize bytes, for tests that need to sit precisely on either
// side of a byte cap.
func buildJSONBodyOfSize(t *testing.T, totalSize int, model string) string {
	t.Helper()
	prefix := fmt.Sprintf(`{"model":%q,"padding":"`, model)
	const suffix = `"}`
	padLen := totalSize - len(prefix) - len(suffix)
	if padLen < 0 {
		t.Fatalf("totalSize %d too small for prefix+suffix of %d bytes", totalSize, len(prefix)+len(suffix))
	}
	body := prefix + strings.Repeat("a", padLen) + suffix
	if len(body) != totalSize {
		t.Fatalf("built %d bytes, want %d", len(body), totalSize)
	}
	return body
}

// TestModelEndpoint_BodyJustUnderCapForwardsIntact is the positive half of
// S6's cap change: a body one byte under the (now smaller) JSONBodyCap must
// still forward to the upstream completely intact, not truncated or mangled
// by the tighter limit.
func TestModelEndpoint_BodyJustUnderCapForwardsIntact(t *testing.T) {
	var received []byte
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	body := buildJSONBodyOfSize(t, modelbroker.JSONBodyCap-1, "vCode")

	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var sent, got map[string]json.RawMessage
	assertNoErr(t, json.Unmarshal([]byte(body), &sent), "decode what the test sent")
	assertNoErr(t, json.Unmarshal(received, &got), "decode what the upstream received")
	if string(got["padding"]) != string(sent["padding"]) {
		t.Fatal("padding did not survive forwarding a body just under the cap byte-for-byte")
	}
	if string(got["model"]) != `"vCode"` {
		t.Fatalf("upstream model = %s, want \"vCode\"", got["model"])
	}
}

// TestModelEndpoint_BodyOverCapRefused413 is the negative half: a body over
// JSONBodyCap is refused (never forwarded), and the body budget it briefly
// held is released even on this error path.
func TestModelEndpoint_BodyOverCapRefused413(t *testing.T) {
	var calls int
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	body := buildJSONBodyOfSize(t, modelbroker.JSONBodyCap+1, "vCode")

	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, body)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", w.Code, w.Body.String())
	}
	if calls != 0 {
		t.Fatalf("an oversize body reached the upstream %d time(s)", calls)
	}
	if got := m.bodyBudget.InFlight(); got != 0 {
		t.Fatalf("InFlight after a refused oversize body = %d, want 0 (the budget must still release on this error path)", got)
	}
}

// TestModelEndpoint_S6_BodyBudgetLimitsConcurrentAdmission fires more
// concurrent requests than the (shrunk, for test speed) body budget can
// admit at once, and proves — by counting real simultaneous holders via
// bodyBudgetHeldHookForTest, never by elapsed wall time — that admission
// never exceeds the budget's slot count, and that everything eventually
// completes once earlier holders release. Weight is accounted by the
// route's cap regardless of the tiny actual body each goroutine sends here
// (docs/model-endpoint.md; BodyBudget's own doc), which is exactly what
// makes a cheap test of this able to stand in for "M concurrent near-cap
// requests".
func TestModelEndpoint_S6_BodyBudgetLimitsConcurrentAdmission(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	const slots = 2
	m.bodyBudget = modelbroker.NewBodyBudget(int64(modelbroker.JSONBodyCap) * slots)

	const goroutines = 5
	started := make(chan struct{}, goroutines)
	gate := make(chan struct{})
	var mu sync.Mutex
	current, peak := 0, 0

	bodyBudgetHeldHookForTest = func() {
		mu.Lock()
		current++
		if current > peak {
			peak = current
		}
		mu.Unlock()
		started <- struct{}{}
		<-gate
		mu.Lock()
		current--
		mu.Unlock()
	}
	t.Cleanup(func() { bodyBudgetHeldHookForTest = nil })

	server := httptest.NewServer(m.Handler(transportSocket))
	defer server.Close()

	results := make(chan int, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
			if err != nil {
				results <- -1
				return
			}
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- -1
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			results <- resp.StatusCode
		}()
	}

	for i := 0; i < slots; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of %d expected simultaneous admissions were observed", i, slots)
		}
	}
	// A bounded grace window checking for the ABSENCE of a third admission
	// while both slots are held — not a pass/fail on how long anything took.
	select {
	case <-started:
		t.Fatal("a third request was admitted while the budget's two slots were both already held")
	case <-time.After(150 * time.Millisecond):
	}

	close(gate)

	for i := 0; i < goroutines-slots; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("the remaining requests were never admitted after the held ones released")
		}
	}
	for i := 0; i < goroutines; i++ {
		select {
		case code := <-results:
			if code != http.StatusOK {
				t.Fatalf("a request finished with status %d, want 200", code)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a request never finished")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if peak > slots {
		t.Fatalf("peak concurrent admissions = %d, want <= %d (the budget's own capacity/weight)", peak, slots)
	}
}

// ---- relay#116 re-review (R-M1b2): S7, panic outcomes ----

// TestModelEndpoint_S7_NonAbortPanicIsErrorAndRepanics proves the fix: a
// panic recovered inside proxy() that is NOT http.ErrAbortHandler is
// audited "error" (never mislabelled "client_abort", which would hide a
// real bug behind a client-fault outcome) and still reaches the caller —
// swallowing it would hide the bug from whatever would otherwise crash or
// log it.
func TestModelEndpoint_S7_NonAbortPanicIsErrorAndRepanics(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, nil))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	var auditEv ModelCallAudit
	var auditFired bool
	m.AuditHook = func(ev ModelCallAudit) { auditEv, auditFired = ev, true }

	synthetic := errors.New("synthetic bug, not a client abort")
	proxyPanicForTest = synthetic
	t.Cleanup(func() { proxyPanicForTest = nil })

	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("the panic must reach the caller, not be swallowed")
		}
		if rec != error(synthetic) {
			t.Fatalf("recovered %v, want the original synthetic value unchanged", rec)
		}
		if !auditFired {
			t.Fatal("no audit record was produced for the panic")
		}
		if auditEv.Outcome != "error" {
			t.Fatalf("audit outcome = %q, want error", auditEv.Outcome)
		}
	}()

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	m.Handler(transportSocket).ServeHTTP(w, r)
	t.Fatal("ServeHTTP returned normally; the synthetic panic should have propagated")
}

// TestModelEndpoint_S7_ErrAbortHandlerPanicIsClientAbort is the positive
// case, via the same seam: only http.ErrAbortHandler itself is audited
// client_abort.
func TestModelEndpoint_S7_ErrAbortHandlerPanicIsClientAbort(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, nil))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	var auditEv ModelCallAudit
	m.AuditHook = func(ev ModelCallAudit) { auditEv = ev }

	proxyPanicForTest = http.ErrAbortHandler
	t.Cleanup(func() { proxyPanicForTest = nil })

	defer func() {
		rec := recover()
		if rec != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler unchanged", rec)
		}
		if auditEv.Outcome != "client_abort" {
			t.Fatalf("audit outcome = %q, want client_abort", auditEv.Outcome)
		}
	}()

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	m.Handler(transportSocket).ServeHTTP(w, r)
	t.Fatal("ServeHTTP returned normally; http.ErrAbortHandler should have propagated")
}

// ---- relay#116 re-review (R-M1b2): S8, completed-call false client_abort ----

// TestModelEndpoint_S8_CompletedCallWithLateDisconnectIsOK proves the fix:
// a call whose response body was copied all the way to EOF is audited "ok"
// with usage, even if the caller's context reports done by the time proxy()
// checks it — the same race a client disconnecting the instant after
// receiving every byte produces in production.
func TestModelEndpoint_S8_CompletedCallWithLateDisconnectIsOK(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":6}}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	ctx, cancel := context.WithCancel(context.Background())
	postServeHookForTest = cancel
	t.Cleanup(func() { postServeHookForTest = nil })

	var auditEv ModelCallAudit
	m.AuditHook = func(ev ModelCallAudit) { auditEv = ev }

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	m.Handler(transportSocket).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if auditEv.Outcome != "ok" {
		t.Fatalf("outcome = %q, want ok (a fully copied response must not be misreported because the caller's context was cancelled right after)", auditEv.Outcome)
	}
	if auditEv.Usage.PromptTokens != 5 || auditEv.Usage.CompletionTokens != 6 {
		t.Fatalf("usage = %+v, want it recorded despite the late cancellation", auditEv.Usage)
	}
}

// ---- relay#116 re-review (R-M1b2): UseNumber and trailing-value nits ----

// TestModelEndpoint_BigNumbersForwardedByteForByte is the endpoint-level
// proof of the UseNumber fix: a body carrying a number outside float64's
// safe range in a field relay never looks at is neither refused nor
// mangled — the upstream receives it exactly as sent.
func TestModelEndpoint_BigNumbersForwardedByteForByte(t *testing.T) {
	var received []byte
	m, store, launches, hosts := newModelEndpointTestServer(t)
	sock := newFakeRouterSocket(t, fakeRouterMux(t, func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	tok := addModelProject(t, store, "p1", nil, false)

	const thirtyDigitInt = "123456789012345678901234567890"
	const hugeExponent = "1e400"
	body := fmt.Sprintf(`{"model":"vCode","big_int":%s,"big_exp":%s}`, thirtyDigitInt, hugeExponent)

	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an out-of-range number elsewhere in the body must not be refused); body=%s", w.Code, w.Body.String())
	}

	var got map[string]json.RawMessage
	assertNoErr(t, json.Unmarshal(received, &got), "decode what the upstream received")
	if string(got["big_int"]) != thirtyDigitInt {
		t.Fatalf("upstream big_int = %s, want byte-identical %s", got["big_int"], thirtyDigitInt)
	}
	if string(got["big_exp"]) != hugeExponent {
		t.Fatalf("upstream big_exp = %s, want byte-identical %s", got["big_exp"], hugeExponent)
	}
}

// TestModelEndpoint_TrailingValueReturns400NotInternalError proves the
// trailing-value nit's fix at the full handler level: before the fix, this
// body's trailing object surfaced only inside RewriteJSONModel's own
// json.Unmarshal, which the endpoint turned into a bare 500. It must now be
// caught at extraction time and answered in the endpoint's normal
// shape-appropriate 400.
func TestModelEndpoint_TrailingValueReturns400NotInternalError(t *testing.T) {
	m, store, _, _ := newModelEndpointTestServer(t)
	tok := addModelProject(t, store, "p1", nil, false)

	body := `{"model":"vCode"} {"unexpected":"trailer"}`
	w := doHandlerRequest(t, m.Handler(transportSocket), http.MethodPost, "/v1/chat/completions", tok, nil, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (not 500) for a body with trailing data; body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &got), "error body must be valid JSON in the endpoint's normal shape")
}
