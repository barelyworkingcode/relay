package main

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
)

// The Chief of Staff scope narrows `proxy`: four doors and nothing else.
var chiefOfStaffDoors = map[string]bool{
	"GET /api/sessions":                 true,
	"GET /ws":                           true,
	"POST /api/chief-of-staff/messages": true,
	"POST /api/chief-of-staff/sessions": true,
}

const (
	reasonOutsideScope = "outside chief-of-staff scope"
	reasonNotGranted   = "class not granted"
	reasonUnknownScope = "unknown scope"
)

var allFiveClasses = []control.CapabilityClass{
	control.ClassRead, control.ClassConfigure, control.ClassGrant, control.ClassExecute, control.ClassProxy,
}

// cosServer is a real FrontendServer on a real socket, with the real
// credential authorizer and a real audit recorder.
type cosServer struct {
	sock string
	srv  *FrontendServer
	f    *sessionRoutesFixture
	rec  *audit.AuditRecorder
	enum *fakeEnumerator
}

func startCoSServer(t *testing.T) *cosServer {
	t.Helper()
	rec := newCoSRecorder(t, &config.AuditConfig{})
	f := newSessionRoutesFixture(t, rec)
	enum := okEnum("Alice")
	sock := filepath.Join(mkShortTempDir(t, "cos-fe-"), "frontend.sock")
	srv, err := NewFrontendServer(f.store, schemaProviderFunc(enumSurfaces), nil, enum,
		Endpoint{Socket: sock}, f.deps.enhanced, nil, nil,
		&ServiceOps{Store: f.store}, &EnrolmentOps{Store: f.store}, &audit.AuditOps{Audit: rec}, &McpOps{Store: f.store},
		nil, nil, nil, nil, nil,
		NewCredentialAuthorizer(f.store), rec, f.deps.launches, f.deps)
	assertNoErr(t, err, "NewFrontendServer")
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	_ = dialUnixWithTimeout(t, sock, 2*time.Second).Close()
	return &cosServer{sock: sock, srv: srv, f: f, rec: rec, enum: enum}
}

// bearer mints a credential holding classes and returns its plaintext.
func (s *cosServer) bearer(t *testing.T, classes ...control.CapabilityClass) string {
	t.Helper()
	_, plaintext := acsMintCredential(t, s.f.store, classes...)
	return plaintext
}

// bindFrontendIdentity makes this test process a frontend launch identity,
// the way eve is on the real socket.
func (s *cosServer) bindFrontendIdentity(t *testing.T) {
	t.Helper()
	launches := s.f.deps.launches
	secret, _, err := launches.Begin(service.Identity{
		Kind: service.IdentityKindService, Name: "chief-like",
		Capabilities: []config.ServiceCapability{config.ServiceCapabilityFrontend},
	})
	assertNoErr(t, err, "Begin")
	_, err = launches.Bind("chief-like", secret, selfPeerToken(t))
	assertNoErr(t, err, "Bind")
}

func (s *cosServer) do(t *testing.T, method, path string, header http.Header, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, "http://unix"+path, strings.NewReader(body))
	assertNoErr(t, err, "NewRequest")
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := dialFrontendHTTP(s.sock).Do(req)
	assertNoErr(t, err, method+" "+path)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func bearerHeader(token string, scoped bool) http.Header {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	if scoped {
		h.Set(scopeHeader, chiefOfStaffScope)
	}
	return h
}

// decisionReasons counts the refused control_decision rows per reason.
func decisionReasons(t *testing.T, rec *audit.AuditRecorder) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, ev := range readLoggedEvents(t, rec) {
		if ev.Event == audit.AuditEventControlDecision && ev.Outcome == audit.AuditOutcomeDenied {
			out[ev.Error]++
		}
	}
	return out
}

type patternCollector struct{ patterns []string }

func (c *patternCollector) ReserveRelayRoute(p string) { c.patterns = append(c.patterns, p) }

var pathWildcard = regexp.MustCompile(`\{[^}]*\}`)

// socketPatterns lists every pattern the socket mux registers, from the same
// registration the server itself ran.
func socketPatterns(srv *FrontendServer) []string {
	c := &patternCollector{}
	registerFrontendRoutes(&control.RouteRegistrar{Mux: http.NewServeMux(), Transport: control.TransportSocket, Reserve: c}, srv.routeDeps)
	return c.patterns
}

type scopeProbe struct{ method, path string }

// probesFor turns a registered pattern into concrete requests. A pattern
// with no method answers every method, so it is tried with three.
func probesFor(pattern string) []scopeProbe {
	methods := []string{"GET", "POST", "DELETE"}
	path := pattern
	if m, rest, ok := strings.Cut(pattern, " "); ok {
		methods, path = []string{m}, rest
	}
	path = strings.ReplaceAll(path, "{$}", "")
	path = pathWildcard.ReplaceAllString(path, "x")
	var out []scopeProbe
	for _, m := range methods {
		out = append(out, scopeProbe{m, path})
	}
	return out
}

func TestChiefOfStaffScopeReach(t *testing.T) {
	// Probes that only the catch-all proxy mount serves: sends, deletes, log
	// reads and the model list that relay-sessions answers behind it.
	catchAll := []scopeProbe{
		{"POST", "/api/sessions/x/message"},
		{"DELETE", "/api/sessions/x"},
		{"GET", "/api/terminals/x/log"},
		{"DELETE", "/api/terminals/x"},
		{"GET", "/api/models"},
	}
	for _, tc := range []struct {
		name string
		auth func(t *testing.T, s *cosServer) http.Header
	}{
		{"frontend launch identity", func(t *testing.T, s *cosServer) http.Header {
			s.bindFrontendIdentity(t)
			return bearerHeader("", true)
		}},
		{"bearer holding all five classes", func(t *testing.T, s *cosServer) http.Header {
			return bearerHeader(s.bearer(t, allFiveClasses...), true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startCoSServer(t)
			header := tc.auth(t, s)

			patterns := socketPatterns(s.srv)
			if len(patterns) < 40 {
				t.Fatalf("only %d route patterns scanned; the scan is not seeing the real mux", len(patterns))
			}
			have := map[string]bool{}
			for _, p := range patterns {
				have[p] = true
			}
			for _, must := range []string{
				"POST /api/terminals", "POST /api/sessions", "POST /api/sessions/{id}/resume",
				"POST /api/mcps", "POST /api/mcps/{id}/enumerate", "POST /api/services/{id}/start",
				"POST /api/chief-of-staff/messages", "POST /api/chief-of-staff/sessions", "PUT /api/projects/{id}",
			} {
				if !have[must] {
					t.Fatalf("pattern %q is not in the scanned set; the scan lost a door it must try", must)
				}
			}

			var probes []scopeProbe
			for _, p := range patterns {
				probes = append(probes, probesFor(p)...)
			}
			probes = append(probes, catchAll...)

			refused := 0
			for _, p := range probes {
				status, body := s.do(t, p.method, p.path, header, "{}")
				if chiefOfStaffDoors[p.method+" "+p.path] {
					if status == http.StatusUnauthorized || status == http.StatusForbidden {
						t.Errorf("%s %s: refused (%d) inside the scope: %s", p.method, p.path, status, body)
					}
					continue
				}
				refused++
				if status != http.StatusForbidden {
					t.Errorf("%s %s: status %d, want 403 outside the scope", p.method, p.path, status)
				}
			}

			t.Logf("scanned %d patterns, %d requests, %d refused", len(patterns), len(probes), refused)
			got := decisionReasons(t, s.rec)
			if got[reasonOutsideScope] != refused || len(got) != 1 {
				t.Errorf("refusals recorded = %v, want %d x %q and nothing else", got, refused, reasonOutsideScope)
			}
		})
	}
}

func TestChiefOfStaffScopeEntry(t *testing.T) {
	t.Run("a caller without proxy cannot enter", func(t *testing.T) {
		s := startCoSServer(t)
		h := bearerHeader(s.bearer(t, control.ClassRead, control.ClassConfigure, control.ClassGrant, control.ClassExecute), true)
		status, _ := s.do(t, "GET", "/api/sessions", h, "")
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
		if got := decisionReasons(t, s.rec); got[reasonNotGranted] != 1 || len(got) != 1 {
			t.Fatalf("refusals = %v, want one %q", got, reasonNotGranted)
		}
	})
	for _, tc := range []struct {
		name   string
		scopes []string
	}{
		{"another scope value", []string{"operator"}},
		{"an empty scope value", []string{""}},
		{"the value twice", []string{chiefOfStaffScope, chiefOfStaffScope}},
		{"the value with another", []string{chiefOfStaffScope, "operator"}},
	} {
		t.Run("unknown scope: "+tc.name, func(t *testing.T) {
			s := startCoSServer(t)
			h := bearerHeader(s.bearer(t, control.ClassProxy), false)
			h[scopeHeader] = tc.scopes
			status, _ := s.do(t, "GET", "/api/sessions", h, "")
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", status)
			}
			if got := decisionReasons(t, s.rec); got[reasonUnknownScope] != 1 || len(got) != 1 {
				t.Fatalf("refusals = %v, want one %q", got, reasonUnknownScope)
			}
		})
	}
}

// Without the scope header the send door is closed to everyone, whatever they
// hold, and nothing reaches the session host.
func TestChiefOfStaffScopeSendDoorClosedWithoutScope(t *testing.T) {
	const send = `{"sessionId":"s-1","text":"hello"}`
	t.Run("bearer holding all five classes", func(t *testing.T) {
		s := startCoSServer(t)
		host := &cosHost{svc: NewFakeService(t, FakeServiceOptions{ServiceID: config.RelaySessionsServiceID, Manifest: fakeSessionsManifest()})}
		s.f.registerFakeSessionsHost(t, host.svc, selfPeerToken(t).Process())

		status, _ := s.do(t, "POST", "/api/chief-of-staff/messages", bearerHeader(s.bearer(t, allFiveClasses...), false), send)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
		if got := decisionReasons(t, s.rec); got[reasonNotGranted] != 1 || len(got) != 1 {
			t.Fatalf("refusals = %v, want one %q", got, reasonNotGranted)
		}
		if n := len(host.sendRequests()); n != 0 {
			t.Fatalf("host got %d /send request(s) from an unscoped caller", n)
		}
	})
	t.Run("frontend launch identity", func(t *testing.T) {
		s := startCoSServer(t)
		s.bindFrontendIdentity(t)

		status, _ := s.do(t, "POST", "/api/chief-of-staff/messages", bearerHeader("", false), send)
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", status)
		}
		if got := decisionReasons(t, s.rec); got[reasonNotGranted] != 1 || len(got) != 1 {
			t.Fatalf("refusals = %v, want one %q", got, reasonNotGranted)
		}
	})
}

func TestChiefOfStaffScopeIsNotMintable(t *testing.T) {
	store := newCLISandboxStore(t)
	_, _, err := mintAPICredential(store, credentialMintRequest{Name: "cos-try", Classes: []string{"chief_of_staff"}})
	if err == nil {
		t.Fatal("a credential was minted holding chief_of_staff")
	}
	if len(store.Get().APICredentials) != 0 {
		t.Fatal("a refused mint still wrote a credential")
	}
}

// The grant cannot call a tool: a route that does something is refused, and
// the thing it would have done does not happen.
func TestChiefOfStaffScopeCannotCallTool(t *testing.T) {
	s := startCoSServer(t)
	token := s.bearer(t, allFiveClasses...)
	const enumerate = `{"field":"mail_accounts"}`

	// Control: the same credential, unscoped, reaches the route and the
	// enumerator runs.
	if status, body := s.do(t, "POST", "/api/mcps/macmcp/enumerate", bearerHeader(token, false), enumerate); status != http.StatusOK {
		t.Fatalf("unscoped enumerate = %d (%s), want 200", status, body)
	}
	if n := len(s.enum.calls); n != 1 {
		t.Fatalf("unscoped enumerate made %d call(s), want 1", n)
	}

	status, _ := s.do(t, "POST", "/api/mcps/macmcp/enumerate", bearerHeader(token, true), enumerate)
	if status != http.StatusForbidden {
		t.Fatalf("scoped enumerate = %d, want 403", status)
	}
	if n := len(s.enum.calls); n != 1 {
		t.Fatalf("the scoped request reached the enumerator: %d calls, want still 1", n)
	}
}

// cosUpstream is a fake relay-sessions /ws: it announces two sessions in two
// projects, then counts every frame a client sends it.
type cosUpstream struct {
	svc *FakeService

	mu       sync.Mutex
	received []string
	exited   chan struct{}
}

func newCoSUpstream(t *testing.T, s *cosServer) *cosUpstream {
	t.Helper()
	u := &cosUpstream{exited: make(chan struct{}, 8)}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	u.svc = NewFakeService(t, FakeServiceOptions{
		ServiceID: "svc-ws",
		Manifest:  newManifest("/ws"),
		Handler: func(w http.ResponseWriter, r *http.Request) {
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			defer func() { u.exited <- struct{}{} }()
			_ = c.WriteJSON(map[string]any{"type": "session_state", "sessionId": "s-proj-a", "state": "running"})
			_ = c.WriteJSON(map[string]any{"type": "turn_done", "sessionId": "s-proj-b", "excerpt": "done"})
			for {
				_, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				u.mu.Lock()
				u.received = append(u.received, string(msg))
				u.mu.Unlock()
			}
		},
	})
	assertNoErr(t, s.f.deps.enhanced.RegisterManifest(u.svc.ServiceID(), u.svc.Socket(), u.svc.Token(), u.svc.Manifest()), "RegisterManifest")
	return u
}

func (u *cosUpstream) frames() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.received...)
}

func (u *cosUpstream) waitExit(t *testing.T) {
	t.Helper()
	select {
	case <-u.exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the upstream connection never closed")
	}
}

func dialCoSWS(t *testing.T, s *cosServer, header http.Header) *websocket.Conn {
	t.Helper()
	conn, resp, err := wsDialerOverUnix(s.sock).Dial("ws://unix/ws", header)
	if err != nil {
		t.Fatalf("WS dial: %v (resp %v)", err, resp)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestChiefOfStaffScopeWebSocketIsReadOnly(t *testing.T) {
	s := startCoSServer(t)
	token := s.bearer(t, control.ClassProxy)

	t.Run("receives broadcasts for sessions in two projects", func(t *testing.T) {
		newCoSUpstream(t, s)
		conn := dialCoSWS(t, s, bearerHeader(token, true))
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var types []string
		for i := 0; i < 2; i++ {
			var m map[string]any
			if err := conn.ReadJSON(&m); err != nil {
				t.Fatalf("read frame %d: %v", i, err)
			}
			types = append(types, m["type"].(string)+"/"+m["sessionId"].(string))
		}
		if types[0] != "session_state/s-proj-a" || types[1] != "turn_done/s-proj-b" {
			t.Fatalf("frames = %v", types)
		}
	})

	// A frame of any kind a viewer could send: the first one closes the
	// connection and none reaches relay-sessions.
	for _, frame := range []string{
		`{"type":"send_message","sessionId":"s-proj-a","text":"hello"}`,
		`{"type":"join_session","sessionId":"s-proj-a"}`,
		`{"type":"permission_response","sessionId":"s-proj-a","requestId":"r1","allow":true}`,
		`{"type":"set_permission_mode","sessionId":"s-proj-a","mode":"bypassPermissions"}`,
		`{"type":"terminal_input","terminalId":"t1","data":"ls\n"}`,
	} {
		name := frame[strings.Index(frame, `":"`)+3:]
		name = name[:strings.Index(name, `"`)]
		t.Run("a "+name+" frame", func(t *testing.T) {
			s := startCoSServer(t)
			up := newCoSUpstream(t, s)
			conn := dialCoSWS(t, s, bearerHeader(s.bearer(t, control.ClassProxy), true))
			assertNoErr(t, conn.WriteMessage(websocket.TextMessage, []byte(frame)), "write")

			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			var closeErr *websocket.CloseError
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					var ok bool
					closeErr, ok = err.(*websocket.CloseError)
					if !ok {
						t.Fatalf("read ended with %v, want a close frame", err)
					}
					break
				}
			}
			if closeErr.Code != websocket.ClosePolicyViolation || closeErr.Text != "chief-of-staff scope is read-only" {
				t.Fatalf("close = %d %q, want 1008 %q", closeErr.Code, closeErr.Text, "chief-of-staff scope is read-only")
			}
			up.waitExit(t)
			if got := up.frames(); len(got) != 0 {
				t.Fatalf("relay-sessions received %d frame(s) from a scoped viewer: %v", len(got), got)
			}
		})
	}

	// Control: without the scope the same connection does reach the upstream.
	t.Run("the same frame unscoped reaches the upstream", func(t *testing.T) {
		s := startCoSServer(t)
		up := newCoSUpstream(t, s)
		conn := dialCoSWS(t, s, bearerHeader(s.bearer(t, control.ClassProxy), false))
		assertNoErr(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"join_session","sessionId":"s-proj-a"}`)), "write")
		testutil.WaitFor(t, 3*time.Second, func() bool { return len(up.frames()) > 0 })
		if len(up.frames()) != 1 {
			t.Fatalf("upstream received %v, want the one frame", up.frames())
		}
	})
}
