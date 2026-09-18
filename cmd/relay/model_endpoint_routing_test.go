package main

// Client model routing (plans/client-model-routing.md): one endpoint, two
// branches. A model relay manages is served by the broker under the caller's
// grant with relay's own credential (X-Relay-Key) and audited; a provider's own
// request is forwarded with the client's credential untouched and relay's
// stripped.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/membership"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
)

// routingCatalogJSON: a managed alias, an endpoint model, a virtual model, a
// modelMap key that is not Claude-shaped and one that is.
const routingCatalogJSON = `{"object":"list","data":[
 {"id":"qwen3-8b","owned_by":"llama.cpp"},
 {"id":"omlx/Chat","owned_by":"mlx-omni"},
 {"id":"vCode","owned_by":"virtual"},
 {"id":"relay/coder","owned_by":"anthropic-map","target":"vCode"},
 {"id":"claude-haiku-4-5","owned_by":"anthropic-map","target":"vCode"}]}`

// seenRequest is what the fake model host received, exactly as it arrived.
type seenRequest struct {
	Method, Path, Query string
	Header              http.Header
	Body                []byte
}

// routingHost is a fake relayLLM router.sock: it records every request that
// reaches it and answers the catalog, so a test can assert both what was
// forwarded and that something was NOT.
type routingHost struct {
	mu          sync.Mutex
	seen        []seenRequest
	catalogFail bool
}

func (h *routingHost) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			h.mu.Lock()
			fail := h.catalogFail
			h.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(routingCatalogJSON))
			return
		}
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.seen = append(h.seen, seenRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Relay-Model-Target", "vCode")
		_, _ = w.Write([]byte(`{"ok":true,"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	})
}

func (h *routingHost) requests() []seenRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]seenRequest(nil), h.seen...)
}

func (h *routingHost) setCatalogFail(v bool) {
	h.mu.Lock()
	h.catalogFail = v
	h.mu.Unlock()
}

type routingFixture struct {
	m     *ModelEndpointServer
	store config.SettingsStore
	host  *routingHost
	key   string // a live session key for project "p1", unrestricted
	audit []ModelCallAudit
	mu    sync.Mutex
}

func newRoutingFixture(t *testing.T) *routingFixture {
	t.Helper()
	m, store, launches, hosts := newModelEndpointTestServer(t)
	host := &routingHost{}
	sock := newFakeRouterSocket(t, host.handler())
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())
	addModelProject(t, store, "p1", nil, false)
	key, err := m.modelKeys.Mint("p1", "session:s1")
	assertNoErr(t, err, "Mint")
	f := &routingFixture{m: m, store: store, host: host, key: key}
	m.AuditHook = func(ev ModelCallAudit) {
		f.mu.Lock()
		f.audit = append(f.audit, ev)
		f.mu.Unlock()
	}
	return f
}

func (f *routingFixture) lastAudit(t *testing.T) ModelCallAudit {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.audit) == 0 {
		t.Fatal("no audit record was written")
	}
	return f.audit[len(f.audit)-1]
}

// do sends one request through the TCP handler. headers are set verbatim.
func (f *routingFixture) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.m.Handler(transportTCP).ServeHTTP(w, r)
	return w
}

const (
	clientOAuth  = "Bearer sk-ant-oat01-AbC_dEf-1234567890=="
	clientAPIKey = "sk-ant-api03-ZzZ_client-own-key"
)

func TestRouting_ClassificationTable(t *testing.T) {
	f := newRoutingFixture(t)
	relayKey := map[string]string{"X-Relay-Key": f.key}
	claudeClient := map[string]string{"Authorization": clientOAuth}

	cases := []struct {
		name     string
		method   string
		path     string
		body     string
		headers  map[string]string
		wantCode int
		// wantPath is the path the model host must have received; "" means
		// the request must not have reached it at all.
		wantPath string
	}{
		{"chat completions, model in catalog -> local", "POST", "/v1/chat/completions", `{"model":"vCode"}`, relayKey, 200, "/v1/chat/completions"},
		{"messages, modelMap key -> local", "POST", "/v1/messages", `{"model":"relay/coder","messages":[]}`, relayKey, 200, "/v1/messages"},
		{"messages, modelMap key shaped like a Claude id -> local, not Anthropic", "POST", "/v1/messages", `{"model":"claude-haiku-4-5","messages":[]}`, relayKey, 200, "/v1/messages"},
		{"count_tokens, modelMap key -> local", "POST", "/v1/messages/count_tokens", `{"model":"relay/coder","messages":[]}`, relayKey, 200, "/v1/messages/count_tokens"},
		{"messages, Claude model not in catalog -> Anthropic passthrough", "POST", "/v1/messages?beta=true", `{"model":"claude-opus-5","messages":[]}`, claudeClient, 200, "/v1/messages"},
		{"count_tokens, Claude model not in catalog -> passthrough", "POST", "/v1/messages/count_tokens", `{"model":"claude-opus-5","messages":[]}`, claudeClient, 200, "/v1/messages/count_tokens"},
		{"/openai/ path -> passthrough", "GET", "/openai/v1/models", "", map[string]string{"Authorization": "Bearer sk-openai-own"}, 200, "/openai/v1/models"},
		{"/chatgpt/ path -> passthrough", "POST", "/chatgpt/codex/responses", `{"model":"gpt-5.5"}`, claudeClient, 200, "/chatgpt/codex/responses"},
		{"/api/ path -> passthrough", "HEAD", "/api/hello", "", nil, 200, "/api/hello"},
		{"messages, catalog model that is not a modelMap key -> 404", "POST", "/v1/messages", `{"model":"qwen3-8b","messages":[]}`, relayKey, 404, ""},
		{"messages, typo of a local name -> 404, never Anthropic", "POST", "/v1/messages", `{"model":"qwen3-8","messages":[]}`, relayKey, 404, ""},
		{"messages, another provider's model -> 404", "POST", "/v1/messages", `{"model":"gpt-5","messages":[]}`, relayKey, 404, ""},
		{"an unmanaged model on a local route is a 404", "POST", "/v1/chat/completions", `{"model":"claude-opus-5"}`, relayKey, 404, ""},
		{"a route that is neither is a 404", "GET", "/models/load", "", relayKey, 404, ""},
		{"path that only names a passthrough route once cleaned is not one", "POST", "/openai/../v1/chat/completions", `{"model":"vCode"}`, map[string]string{"Authorization": "Bearer sk-openai-own"}, 401, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.host.requests())
			w := f.do(tc.method, tc.path, tc.body, tc.headers)
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantCode, w.Body.String())
			}
			got := f.host.requests()[before:]
			if tc.wantPath == "" {
				if len(got) != 0 {
					t.Fatalf("the request reached the model host: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Path != tc.wantPath {
				t.Fatalf("model host saw %+v, want exactly one request for %s", got, tc.wantPath)
			}
		})
	}
}

// A caller with no relay credential gets a 401 on every branch that needs
// one, and never the 404 that would tell it which model names relay manages.
func TestRouting_NoRelayCredentialIs401NotAnOracle(t *testing.T) {
	f := newRoutingFixture(t)
	oauth := map[string]string{"Authorization": clientOAuth}
	for name, body := range map[string]string{
		"a catalog model that is not a modelMap key": `{"model":"qwen3-8b","messages":[]}`,
		"a mapped local model":                       `{"model":"relay/coder","messages":[]}`,
		"a typo":                                     `{"model":"qwen3-8","messages":[]}`,
	} {
		w := f.do("POST", "/v1/messages", body, oauth)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with no relay credential: status = %d, want 401", name, w.Code)
		}
	}
	if w := f.do("POST", "/v1/chat/completions", `{"model":"vCode"}`, oauth); w.Code != http.StatusUnauthorized {
		t.Errorf("local route with only the client's own credential: status = %d, want 401", w.Code)
	}
	if got := f.host.requests(); len(got) != 0 {
		t.Fatalf("an unauthenticated request reached the model host: %+v", got)
	}
}

// Local branch: relay's key is required, the project's allowed_models applies,
// the call is audited, and nothing of either credential reaches the backend.
func TestRouting_LocalBranchStripsEveryCredentialAndAudits(t *testing.T) {
	f := newRoutingFixture(t)
	w := f.do("POST", "/v1/chat/completions", `{"model":"vCode"}`, map[string]string{
		"X-Relay-Key":       f.key,
		"Authorization":     clientOAuth,
		"X-Api-Key":         clientAPIKey,
		"X-Relay-Extra":     "internal",
		"anthropic-version": "2023-06-01",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	seen := f.host.requests()
	if len(seen) != 1 {
		t.Fatalf("model host saw %d requests, want 1", len(seen))
	}
	for _, h := range []string{"Authorization", "X-Api-Key", "X-Relay-Key", "X-Relay-Extra"} {
		if v := seen[0].Header.Get(h); v != "" {
			t.Errorf("the model host received %s: %q", h, v)
		}
	}
	if seen[0].Header.Get("anthropic-version") == "" {
		t.Error("an unrelated header was dropped")
	}
	if strings.Contains(w.Header().Get("X-Relay-Model-Target"), "vCode") {
		t.Error("X-Relay-Model-Target reached the client")
	}

	ev := f.lastAudit(t)
	if ev.Outcome != "ok" || ev.CallerKind != "project" || ev.CallerName != "p1" || ev.Auth != "model_key" || ev.ModelKeyLabel != "session:s1" {
		t.Fatalf("audit = %+v, want an ok call attributed to project p1 by its model key", ev)
	}
	if ev.Target != "vCode" {
		t.Errorf("audit target = %q, want the model host's own account (vCode)", ev.Target)
	}
}

func TestRouting_LocalBranchAppliesTheProjectsAllowedModels(t *testing.T) {
	f := newRoutingFixture(t)
	addModelProject(t, f.store, "limited", []string{"vCode"}, false)
	key, err := f.m.modelKeys.Mint("limited", "session:lim")
	assertNoErr(t, err, "Mint")
	hdr := map[string]string{"X-Relay-Key": key}

	if w := f.do("POST", "/v1/chat/completions", `{"model":"vCode"}`, hdr); w.Code != http.StatusOK {
		t.Fatalf("the granted model: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	before := len(f.host.requests())
	w := f.do("POST", "/v1/chat/completions", `{"model":"omlx/Chat"}`, hdr)
	if w.Code != http.StatusNotFound {
		t.Fatalf("a model outside the grant: status = %d, want 404", w.Code)
	}
	if len(f.host.requests()) != before {
		t.Fatal("a denied model reached the model host")
	}
	if ev := f.lastAudit(t); ev.Outcome != "denied" {
		t.Fatalf("audit outcome = %q, want denied", ev.Outcome)
	}
	// The same grant governs the Messages route: relay/coder maps to vCode,
	// which the project holds, claude-haiku-4-5 maps to vCode too.
	if w := f.do("POST", "/v1/messages", `{"model":"relay/coder","messages":[]}`, hdr); w.Code != http.StatusOK {
		t.Fatalf("a modelMap key whose target is granted: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// The catalog listing is the caller's own grant, and Claude Code's
// Authorization (its subscription token) is not consulted or forwarded.
func TestRouting_ModelListUsesTheRelayKeyNotTheClientsAuthorization(t *testing.T) {
	f := newRoutingFixture(t)
	addModelProject(t, f.store, "limited", []string{"vCode"}, false)
	key, err := f.m.modelKeys.Mint("limited", "session:lim2")
	assertNoErr(t, err, "Mint")
	w := f.do("GET", "/v1/models", "", map[string]string{"X-Relay-Key": key, "Authorization": clientOAuth})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "vCode") || strings.Contains(w.Body.String(), "omlx/Chat") {
		t.Fatalf("status = %d body=%s, want the limited project's list", w.Code, w.Body.String())
	}
}

// Passthrough: no relay authentication, the client's credential forwarded
// byte for byte, relay's own credential and every x-relay-* stripped.
func TestRouting_PassthroughForwardsClientCredentialAndStripsRelays(t *testing.T) {
	f := newRoutingFixture(t)
	body := `{"model":"claude-opus-5",  "max_tokens":8, "messages":[{"role":"user","content":"héllo"}], "extra":1.0000000000000000001}`
	for name, headers := range map[string]map[string]string{
		"without a relay key":    {"Authorization": clientOAuth, "X-Api-Key": clientAPIKey, "anthropic-beta": "oauth-2025-04-20", "X-Relay-Stray": "x"},
		"with a valid relay key": {"Authorization": clientOAuth, "X-Api-Key": clientAPIKey, "anthropic-beta": "oauth-2025-04-20", "X-Relay-Key": f.key, "X-Relay-Stray": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(f.host.requests())
			w := f.do("POST", "/v1/messages?beta=true", body, headers)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			seen := f.host.requests()[before:]
			if len(seen) != 1 {
				t.Fatalf("model host saw %d requests, want 1", len(seen))
			}
			got := seen[0]
			if got.Header.Get("Authorization") != clientOAuth || got.Header.Get("X-Api-Key") != clientAPIKey {
				t.Errorf("client credential changed: Authorization=%q x-api-key=%q", got.Header.Get("Authorization"), got.Header.Get("X-Api-Key"))
			}
			if got.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
				t.Error("an ordinary header was dropped")
			}
			for _, h := range []string{"X-Relay-Key", "X-Relay-Stray"} {
				if v := got.Header.Get(h); v != "" {
					t.Errorf("%s reached the provider side: %q", h, v)
				}
			}
			if !bytes.Equal(got.Body, []byte(body)) {
				t.Errorf("body was not forwarded byte for byte:\n got %s\nwant %s", got.Body, body)
			}
			if got.Query != "beta=true" {
				t.Errorf("query = %q, want beta=true", got.Query)
			}
			if w.Header().Get("X-Relay-Model-Target") != "" {
				t.Error("an x-relay-* response header reached the client")
			}
		})
	}
}

func TestRouting_PassthroughIsAuditedAsPassthroughAndAttributedWhenKeyed(t *testing.T) {
	f := newRoutingFixture(t)
	if w := f.do("GET", "/openai/v1/models", "", map[string]string{"Authorization": "Bearer sk-openai-own"}); w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	ev := f.lastAudit(t)
	if ev.Outcome != "ok" || ev.Target != "passthrough:openai" || ev.Path != "/openai/v1/models" || ev.CallerKind != "" || ev.Auth != "" {
		t.Fatalf("anonymous passthrough audit = %+v", ev)
	}

	if w := f.do("POST", "/v1/messages", `{"model":"claude-opus-5","messages":[]}`, map[string]string{"Authorization": clientOAuth, "X-Relay-Key": f.key}); w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	ev = f.lastAudit(t)
	if ev.Target != "passthrough:anthropic" || ev.CallerName != "p1" || ev.Auth != "model_key" || ev.RequestedModel != "claude-opus-5" {
		t.Fatalf("keyed passthrough audit = %+v, want attribution to the session's project", ev)
	}
	if strings.Contains(fmt.Sprint(ev), f.key) {
		t.Fatal("the audit record carries the model key")
	}
}

// An rmk_-shaped credential that does not validate is a 401 on both branches
// and never reaches the model host, wherever it is presented.
func TestRouting_InvalidRelayKeyIs401OnBothBranchesAndIsNeverForwarded(t *testing.T) {
	f := newRoutingFixture(t)
	never := "rmk_" + strings.Repeat("0", 64)
	revoked, err := f.m.modelKeys.Mint("p1", "session:gone")
	assertNoErr(t, err, "Mint")
	f.m.modelKeys.Revoke("p1", "session:gone")

	type reqSpec struct{ method, path, body string }
	local := reqSpec{"POST", "/v1/chat/completions", `{"model":"vCode"}`}
	localMessages := reqSpec{"POST", "/v1/messages", `{"model":"relay/coder","messages":[]}`}
	claude := reqSpec{"POST", "/v1/messages", `{"model":"claude-opus-5","messages":[]}`}
	pathRoute := reqSpec{"GET", "/openai/v1/models", ""}

	for _, tc := range []struct {
		name    string
		req     reqSpec
		headers map[string]string
	}{
		{"local, unknown key in X-Relay-Key", local, map[string]string{"X-Relay-Key": never}},
		{"local, revoked key in X-Relay-Key", local, map[string]string{"X-Relay-Key": revoked}},
		{"local messages, unknown key", localMessages, map[string]string{"X-Relay-Key": never}},
		{"anthropic passthrough, unknown key in X-Relay-Key", claude, map[string]string{"X-Relay-Key": never, "Authorization": clientOAuth}},
		{"anthropic passthrough, revoked key in X-Relay-Key", claude, map[string]string{"X-Relay-Key": revoked, "Authorization": clientOAuth}},
		{"path passthrough, unknown key in X-Relay-Key", pathRoute, map[string]string{"X-Relay-Key": never}},
		{"anthropic passthrough, key-shaped Authorization", claude, map[string]string{"Authorization": "Bearer " + never}},
		{"path passthrough, key-shaped Authorization", pathRoute, map[string]string{"Authorization": "Bearer " + never}},
		{"path passthrough, key-shaped x-api-key", pathRoute, map[string]string{"X-Api-Key": never}},
		// Valid, but in a slot that is forwarded: still refused.
		{"anthropic passthrough, LIVE key in Authorization", claude, map[string]string{"Authorization": "Bearer " + f.key}},
		{"path passthrough, LIVE key in x-api-key", pathRoute, map[string]string{"X-Api-Key": f.key}},
		{"blank X-Relay-Key", claude, map[string]string{"X-Relay-Key": "  ", "Authorization": clientOAuth}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.host.requests())
			w := f.do(tc.req.method, tc.req.path, tc.req.body, tc.headers)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
			}
			if len(f.host.requests()) != before {
				t.Fatal("the request reached the model host")
			}
		})
	}
}

// A repeated X-Relay-Key is refused like a blank one, never resolved to
// "the first" or "the last".
func TestRouting_RepeatedRelayKeyHeaderIsRefused(t *testing.T) {
	f := newRoutingFixture(t)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"vCode"}`))
	r.Header.Add("X-Relay-Key", f.key)
	r.Header.Add("X-Relay-Key", f.key)
	w := httptest.NewRecorder()
	f.m.Handler(transportTCP).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// The bearer headers stay relay's own credential when no X-Relay-Key is sent:
// pi's overlay and the chat provider authenticate that way today.
func TestRouting_LegacyBearerStillAuthenticatesTheLocalBranch(t *testing.T) {
	f := newRoutingFixture(t)
	w := f.do("POST", "/v1/chat/completions", `{"model":"vCode"}`, map[string]string{"Authorization": "Bearer " + f.key})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if seen := f.host.requests(); len(seen) != 1 || seen[0].Header.Get("Authorization") != "" {
		t.Fatalf("the model host saw %+v; the relay bearer must never reach it", seen)
	}
}

// A catalog that cannot be read is a 503 on every path that needs it, and
// "not in the catalog" is never read as "unmanaged, send it to Anthropic".
// A path route never consults the catalog, so it is unaffected.
func TestRouting_CatalogDownIs503NeverUnmanaged(t *testing.T) {
	f := newRoutingFixture(t)
	f.host.setCatalogFail(true)

	for name, tc := range map[string]struct {
		path, body string
		headers    map[string]string
	}{
		"claude model on /v1/messages": {"/v1/messages", `{"model":"claude-opus-5","messages":[]}`, map[string]string{"Authorization": clientOAuth}},
		"local model on /v1/messages":  {"/v1/messages", `{"model":"relay/coder","messages":[]}`, map[string]string{"X-Relay-Key": f.key}},
		"local model on chat":          {"/v1/chat/completions", `{"model":"vCode"}`, map[string]string{"X-Relay-Key": f.key}},
	} {
		w := f.do("POST", tc.path, tc.body, tc.headers)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with the catalog down: status = %d, want 503", name, w.Code)
		}
	}
	if got := f.host.requests(); len(got) != 0 {
		t.Fatalf("a request was forwarded while the catalog was unreadable: %+v", got)
	}

	if w := f.do("GET", "/openai/v1/models", "", map[string]string{"Authorization": "Bearer sk-openai-own"}); w.Code != http.StatusOK {
		t.Fatalf("a path passthrough with the catalog down: status = %d, want 200 (it never needed the catalog)", w.Code)
	}
}

func TestRouting_NoModelHostIs503OnBothBranches(t *testing.T) {
	m, store, _, _ := newModelEndpointTestServer(t)
	addModelProject(t, store, "p1", nil, false)
	key, err := m.modelKeys.Mint("p1", "session:s")
	assertNoErr(t, err, "Mint")
	h := m.Handler(transportTCP)
	for name, r := range map[string]*http.Request{
		"path passthrough":      newReq("GET", "/openai/v1/models", "", map[string]string{"Authorization": "Bearer sk-x"}),
		"anthropic passthrough": newReq("POST", "/v1/messages", `{"model":"claude-opus-5"}`, map[string]string{"Authorization": clientOAuth}),
		"local":                 newReq("POST", "/v1/chat/completions", `{"model":"vCode"}`, map[string]string{"X-Relay-Key": key}),
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with no model host: status = %d, want 503", name, w.Code)
		}
	}
}

func newReq(method, path, body string, headers map[string]string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// A session key whose launch ended is refused at the endpoint too, with no
// SessionExited and no Revoke.
func TestRouting_KeyOfAnEndedLaunchIs401AtTheEndpoint(t *testing.T) {
	f := newRoutingFixture(t)
	launches := service.NewLaunches()
	procs := newProcTable()
	procs.set(membership.ProcInfo{PID: 73001, PPID: 1, StartSec: 1})
	var rootExits func()
	launches.SetRootSourceForTest(procs)
	launches.SetRootWatcherForTest(func(_ int, _ membership.ProcInfo, cb func()) (func(), error) {
		rootExits = cb
		return func() {}, nil
	})
	secret, launch, err := launches.Begin(service.Identity{Kind: service.IdentityKindProjectSession, Name: "s9", ProjectID: "p1", ParentLaunch: config.RelaySessionsServiceID})
	assertNoErr(t, err, "Begin")
	_, err = launches.BindKind("s9", secret, peertoken.ForProcessForTest(73001, 1), service.IdentityKindProjectSession)
	assertNoErr(t, err, "Bind")
	key, err := f.m.modelKeys.Mint("p1", "session:s9")
	assertNoErr(t, err, "Mint")
	f.m.modelKeys.BindLaunch(key, launch)

	call := func() int {
		return f.do("POST", "/v1/chat/completions", `{"model":"vCode"}`, map[string]string{"X-Relay-Key": key}).Code
	}
	if code := call(); code != http.StatusOK {
		t.Fatalf("live launch: status = %d, want 200", code)
	}
	rootExits()
	if code := call(); code != http.StatusUnauthorized {
		t.Fatalf("after the launch ended: status = %d, want 401", code)
	}
}

// A WebSocket upgrade on a passthrough route is carried end to end (OMP's
// codex transport prefers one), with the client's credential forwarded and
// relay's stripped, and is audited as an ok 101.
func TestRouting_PassthroughCarriesAWebSocketUpgrade(t *testing.T) {
	m, store, launches, hosts := newModelEndpointTestServer(t)
	addModelProject(t, store, "p1", nil, false)
	key, err := m.modelKeys.Mint("p1", "session:ws")
	assertNoErr(t, err, "Mint")

	var mu sync.Mutex
	var upgradeHeader http.Header
	sock := newFakeRouterSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(routingCatalogJSON))
			return
		}
		mu.Lock()
		upgradeHeader = r.Header.Clone()
		mu.Unlock()
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		line, _ := rw.ReadString('\n')
		_, _ = rw.WriteString("echo:" + line)
		_ = rw.Flush()
	}))
	registerFakeHost(t, hosts, launches, "relayllm", sock, selfPeerToken(t).Process())

	var auditMu sync.Mutex
	var events []ModelCallAudit
	m.AuditHook = func(ev ModelCallAudit) { auditMu.Lock(); events = append(events, ev); auditMu.Unlock() }

	srv := httptest.NewServer(m.Handler(transportTCP))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	assertNoErr(t, err, "dial")
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /chatgpt/codex/responses HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nAuthorization: %s\r\nX-Relay-Key: %s\r\n\r\n", clientOAuth, key)
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("status line = %q, want 101 Switching Protocols", strings.TrimSpace(status))
	}
	for {
		l, _ := br.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}
	fmt.Fprintf(conn, "ping\n")
	if echo, _ := br.ReadString('\n'); strings.TrimSpace(echo) != "echo:ping" {
		t.Fatalf("echo = %q, want the upgraded stream carried both ways", echo)
	}
	_ = conn.Close()

	mu.Lock()
	defer mu.Unlock()
	if upgradeHeader.Get("Authorization") != clientOAuth {
		t.Errorf("upstream saw Authorization %q, want the client's untouched", upgradeHeader.Get("Authorization"))
	}
	if upgradeHeader.Get("X-Relay-Key") != "" {
		t.Error("X-Relay-Key reached the provider side of a WebSocket upgrade")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		auditMu.Lock()
		n := len(events)
		var last ModelCallAudit
		if n > 0 {
			last = events[n-1]
		}
		auditMu.Unlock()
		if n > 0 {
			if last.Outcome != "ok" || last.Status != http.StatusSwitchingProtocols || last.Target != "passthrough:chatgpt" {
				t.Fatalf("audit = %+v, want an ok 101 passthrough:chatgpt", last)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the upgraded call was never audited")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Sanity that the fake catalog decodes the way the classification assumes:
// modelMap rows are in the cache, and Target never reaches a caller's list.
func TestRouting_CatalogCarriesModelMapKeysAndNeverExposesTheirTarget(t *testing.T) {
	f := newRoutingFixture(t)
	rows, err := f.m.catalog.Snapshot(context.Background())
	assertNoErr(t, err, "Snapshot")
	var found bool
	for _, row := range rows {
		if row.ID == "relay/coder" {
			found = row.OwnedBy == "anthropic-map" && row.Target == "vCode"
		}
	}
	if !found {
		t.Fatalf("relay/coder is not in the cached catalog as an anthropic-map row with its target: %+v", rows)
	}
	w := f.do("GET", "/v1/models", "", map[string]string{"X-Relay-Key": f.key})
	var body struct {
		Data []map[string]any `json:"data"`
	}
	assertNoErr(t, json.Unmarshal(w.Body.Bytes(), &body), "decode list")
	for _, row := range body.Data {
		if _, leaked := row["target"]; leaked {
			t.Fatalf("a listed row exposes its target: %+v", row)
		}
	}
	if !strings.Contains(w.Body.String(), "relay/coder") {
		t.Fatal("the modelMap key is missing from the caller's list")
	}
}
