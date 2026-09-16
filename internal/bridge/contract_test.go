package bridge

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubRouter struct {
	mu sync.Mutex

	listToolsTokens   []string
	listToolsResponse json.RawMessage
	listToolsErr      error

	callToolNames    []string
	callToolArgs     []json.RawMessage
	callToolToks     []string
	callToolResp     json.RawMessage
	callToolErr      error
	callToolProgress []ProgressUpdate
	callToolProgDly  time.Duration

	validateAdminToks []string
	validateAdminErr  error

	reconcileCalls int

	reloadMcpIDs []string
	reloadMcpErr error

	reloadServiceIDs []string
	reloadServiceErr error

	describeProjectToks []string
	describeProjectResp ProjectDescription
	describeProjectErr  error

	helloKinds []string

	registerReqs []RegisterManifestRequest
	registerToks []string
	registerErr  error

	adminOpNames []string
	adminOpArgs  []json.RawMessage
	adminOpResp  json.RawMessage
	adminOpErr   error
}

func (s *stubRouter) ListTools(ctx context.Context, token string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listToolsTokens = append(s.listToolsTokens, token)
	return s.listToolsResponse, s.listToolsErr
}

func (s *stubRouter) CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error) {
	s.mu.Lock()
	s.callToolNames = append(s.callToolNames, name)
	s.callToolArgs = append(s.callToolArgs, args)
	s.callToolToks = append(s.callToolToks, token)
	prog := s.callToolProgress
	delay := s.callToolProgDly
	resp, err := s.callToolResp, s.callToolErr
	s.mu.Unlock()
	if sink := ProgressFromContext(ctx); sink != nil {
		for _, u := range prog {
			if delay > 0 {
				time.Sleep(delay)
			}
			sink(u)
		}
	}
	return resp, err
}

func (s *stubRouter) ValidateAdmin(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validateAdminToks = append(s.validateAdminToks, token)
	return s.validateAdminErr
}

func (s *stubRouter) ReconcileExternalMcps(_ context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconcileCalls++
}

func (s *stubRouter) ReloadExternalMcp(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadMcpIDs = append(s.reloadMcpIDs, id)
	return s.reloadMcpErr
}

func (s *stubRouter) ReloadService(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadServiceIDs = append(s.reloadServiceIDs, id)
	return s.reloadServiceErr
}

func (s *stubRouter) Hello(_ context.Context, name, secret, kind string) (HelloResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.helloKinds = append(s.helloKinds, kind)
	return HelloResult{Kind: "service", ServiceID: name, RelayPID: 1}, nil
}

func (s *stubRouter) DescribeProject(_ context.Context, token string) (ProjectDescription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.describeProjectToks = append(s.describeProjectToks, token)
	return s.describeProjectResp, s.describeProjectErr
}

func (s *stubRouter) RegisterManifest(_ context.Context, req RegisterManifestRequest, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registerReqs = append(s.registerReqs, req)
	s.registerToks = append(s.registerToks, token)
	return s.registerErr
}

func (s *stubRouter) RegisterModelHost(_ context.Context, _ RegisterModelHostRequest, _ string) error {
	return nil
}

func (s *stubRouter) SessionExited(_ context.Context, _ SessionExitedRequest, _ string) error {
	return nil
}

func (s *stubRouter) AdminOp(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminOpNames = append(s.adminOpNames, name)
	s.adminOpArgs = append(s.adminOpArgs, args)
	return s.adminOpResp, s.adminOpErr
}

// startTestBridge uses /tmp directly, not t.TempDir(): macOS caps a
// Unix-socket path at 104 chars, which t.TempDir() paths often exceed.
func startTestBridge(t *testing.T, router ToolRouter) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "br")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "b.sock")
	// Use the same listen+chmod sequence as NewBridgeServer (avoid touching ConfigDir).
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		ln.Close()
		t.Fatalf("chmod: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &BridgeServer{
		router:   router,
		listener: ln,
		sockPath: sockPath,
		ctx:      ctx,
		cancel:   cancel,
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { srv.Close() })

	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("bridge never became dialable: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return sockPath
}

func TestContract_ListTools(t *testing.T) {
	router := &stubRouter{listToolsResponse: json.RawMessage(`[{"name":"tool1"}]`)}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "proj-token"}

	tools, err := c.ListTools()
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if string(tools) != `[{"name":"tool1"}]` {
		t.Fatalf("tools payload mismatch: %s", tools)
	}
	if len(router.listToolsTokens) != 1 || router.listToolsTokens[0] != "proj-token" {
		t.Fatalf("token not forwarded; got %v", router.listToolsTokens)
	}
}

// A cwd is never part of the Client/ToolRouter contract any more
// (plan-broker-and-sessions.md §2 C3, "allow_cwd_auth is removed"): the
// Client type carries no cwd field, NewClient never captures one, and
// ToolRouter's methods take no cwd parameter for anything to travel
// through. There is nothing left to construct a client-side test around —
// the wire-level guarantee (a hand-crafted BridgeRequest.Cwd is ignored
// server-side) is asserted end to end in cmd/relay's
// TestMembershipAuth_AClientAssertedCwdIsIgnored.

func TestContract_CallTool(t *testing.T) {
	router := &stubRouter{callToolResp: json.RawMessage(`{"ok":true}`)}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "proj-token"}

	args := json.RawMessage(`{"foo":"bar"}`)
	result, err := c.CallTool("fs__read_file", args)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if string(result) != `{"ok":true}` {
		t.Fatalf("result mismatch: %s", result)
	}
	if router.callToolNames[0] != "fs__read_file" {
		t.Fatalf("name not forwarded; got %q", router.callToolNames[0])
	}
	if string(router.callToolArgs[0]) != `{"foo":"bar"}` {
		t.Fatalf("args not forwarded: %s", router.callToolArgs[0])
	}
}

func TestContract_CallToolStreamsProgress(t *testing.T) {
	router := &stubRouter{
		callToolResp: json.RawMessage(`{"ok":true}`),
		callToolProgress: []ProgressUpdate{
			{Message: "queued", Progress: 1, Total: 3},
			{Message: "generating", Progress: 2, Total: 3},
		},
	}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "proj-token"}

	var got []ProgressUpdate
	result, err := c.CallToolStreaming("generate_image", json.RawMessage(`{}`), func(u ProgressUpdate) {
		got = append(got, u)
	})
	if err != nil {
		t.Fatalf("CallToolStreaming: %v", err)
	}
	if string(result) != `{"ok":true}` {
		t.Fatalf("terminal result mismatch: %s", result)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 progress frames before the result, got %d: %+v", len(got), got)
	}
	if got[0].Message != "queued" || got[1].Message != "generating" || got[1].Progress != 2 {
		t.Fatalf("progress frames out of order or malformed: %+v", got)
	}

	result2, err := c.CallTool("generate_image", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if string(result2) != `{"ok":true}` {
		t.Fatalf("result2 mismatch: %s", result2)
	}
}

func TestContract_RegisterManifest(t *testing.T) {
	router := &stubRouter{}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "svc-token"}

	req := RegisterManifestRequest{
		ServiceID:      "svc-foo",
		Manifest:       Manifest{Routes: []string{"/api/foo"}},
		InternalSocket: "/tmp/foo.sock",
		InternalToken:  "internal-token",
	}
	if err := c.RegisterManifest(req); err != nil {
		t.Fatalf("RegisterManifest: %v", err)
	}
	if len(router.registerReqs) != 1 {
		t.Fatalf("expected 1 RegisterManifest call, got %d", len(router.registerReqs))
	}
	got := router.registerReqs[0]
	if got.ServiceID != "svc-foo" || got.InternalSocket != "/tmp/foo.sock" || got.InternalToken != "internal-token" {
		t.Fatalf("manifest fields wrong: %+v", got)
	}
}

func TestContract_RegisterManifest_RejectsInvalidPayload(t *testing.T) {
	router := &stubRouter{}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "svc-token"}

	err := c.RegisterManifest(RegisterManifestRequest{
		Manifest:       Manifest{Routes: []string{"/api/foo"}},
		InternalSocket: "/tmp/foo.sock",
		InternalToken:  "internal-token",
	})
	if err == nil {
		t.Fatal("expected validation error on missing serviceID")
	}
	if len(router.registerReqs) != 0 {
		t.Fatalf("router must not be invoked on invalid payload; got %d calls", len(router.registerReqs))
	}
}

// TestContract_Hello_KindTravelsToTheRouter proves BridgeRequest.Kind reaches
// ToolRouter.Hello verbatim over the wire — the C2 wire contract this unit
// adds. What a kind mismatch DOES (refuses before the secret is spent) is
// internal/service's job to prove; this only proves the bridge plumbs the
// field through.
func TestContract_Hello_KindTravelsToTheRouter(t *testing.T) {
	router := &stubRouter{}
	sock := startTestBridge(t, router)

	resp := sendRaw(t, sock, BridgeRequest{Type: ReqHello, Name: "sess-1", Token: strings.Repeat("a", 64), Kind: "project_session"})
	if resp.Type != RespOK {
		t.Fatalf("Hello: %+v", resp)
	}
	if len(router.helloKinds) != 1 || router.helloKinds[0] != "project_session" {
		t.Fatalf("kind not forwarded: %v", router.helloKinds)
	}

	resp = sendRaw(t, sock, BridgeRequest{Type: ReqHello, Name: "svc", Token: strings.Repeat("a", 64)})
	if resp.Type != RespOK {
		t.Fatalf("Hello with no kind: %+v", resp)
	}
	if len(router.helloKinds) != 2 || router.helloKinds[1] != "" {
		t.Fatalf("an absent kind must forward as empty: %v", router.helloKinds)
	}
}

func TestContract_AdminGated_RejectsBadToken(t *testing.T) {
	router := &stubRouter{validateAdminErr: errString("not-admin")}
	sock := startTestBridge(t, router)

	resp := sendRaw(t, sock, BridgeRequest{
		Type:  ReqReconcileExternalMcps,
		Token: "wrong-token",
	})
	if resp.Type != RespError {
		t.Fatalf("expected error response from admin gate; got %+v", resp)
	}
	if router.reconcileCalls != 0 {
		t.Fatalf("reconcile must not run when ValidateAdmin fails; got %d", router.reconcileCalls)
	}
}

func TestContract_AdminGated_AcceptsGoodToken(t *testing.T) {
	router := &stubRouter{}
	sock := startTestBridge(t, router)

	resp := sendRaw(t, sock, BridgeRequest{
		Type:  ReqReconcileExternalMcps,
		Token: "good-token",
	})
	if resp.Type != RespOK {
		t.Fatalf("expected OK response; got %+v", resp)
	}
	if router.reconcileCalls != 1 {
		t.Fatalf("expected exactly 1 reconcile call; got %d", router.reconcileCalls)
	}
}

func sendRaw(t *testing.T, sockPath string, req BridgeRequest) BridgeResponse {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	sc := NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("read: %v", sc.Err())
	}
	var resp BridgeResponse
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return resp
}

func TestContract_RejectsUnknownRequestType(t *testing.T) {
	router := &stubRouter{}
	sock := startTestBridge(t, router)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	_, _ = conn.Write([]byte(`{"type":"NoSuchType","token":"x"}` + "\n"))
	sc := NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("read: %v", sc.Err())
	}
	var resp BridgeResponse
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Type != RespError {
		t.Fatalf("expected error response, got %+v", resp)
	}
	if !strings.Contains(resp.Message, "unknown request type") {
		t.Fatalf("error message should explain unknown type; got %q", resp.Message)
	}
}

// TestContract_RemovedRequestTypesHitTheUnknownRequestPath pins C1's
// "Deleted" row precisely: ResolvePtyEnv, ResolveProjectTemplate,
// ListProjects and GetProject are not merely refused, they are UNKNOWN — the
// exact path an arbitrary made-up type hits, not a retired type getting its
// own special-cased error. bridgeHandlers no longer has entries for any of
// them, so this also serves as a regression guard: re-adding one by mistake
// changes this test's outcome for that type from "unknown" to something else.
func TestContract_RemovedRequestTypesHitTheUnknownRequestPath(t *testing.T) {
	router := &stubRouter{}
	sock := startTestBridge(t, router)

	for _, removed := range []string{
		"ResolvePtyEnv", "ResolveProjectTemplate", "ListProjects", "GetProject", "ServiceTools",
	} {
		resp := sendRaw(t, sock, BridgeRequest{Type: removed, Token: "svc"})
		if resp.Type != RespError {
			t.Errorf("%s: expected an error response, got %+v", removed, resp)
			continue
		}
		if !strings.Contains(resp.Message, "unknown request type: "+removed) {
			t.Errorf("%s: message = %q, want it to name the type as unknown", removed, resp.Message)
		}
	}
}

func TestContract_RejectsMalformedJSON(t *testing.T) {
	router := &stubRouter{}
	sock := startTestBridge(t, router)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	_, _ = conn.Write([]byte("{this is not json}\n"))
	sc := NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("read: %v", sc.Err())
	}
	var resp BridgeResponse
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Type != RespError {
		t.Fatalf("expected parse error response, got %+v", resp)
	}
}

// The round-trip deadline must reset on every received frame so it behaves as
// an inactivity timeout, not a hard cap: five progress frames arrive 80ms
// apart (~400ms total, longer than bridgeTimeout) but each lands within the
// idle window. With a fixed deadline the call would die after the third
// frame.
func TestContract_StreamingResetsIdleDeadline(t *testing.T) {
	old := bridgeTimeout
	bridgeTimeout = 200 * time.Millisecond
	defer func() { bridgeTimeout = old }()

	router := &stubRouter{
		callToolResp:    json.RawMessage(`{"ok":true}`),
		callToolProgDly: 80 * time.Millisecond,
		callToolProgress: []ProgressUpdate{
			{Message: "1", Progress: 1}, {Message: "2", Progress: 2},
			{Message: "3", Progress: 3}, {Message: "4", Progress: 4},
			{Message: "5", Progress: 5},
		},
	}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "t"}

	got := 0
	result, err := c.CallToolStreaming("slow", json.RawMessage(`{}`), func(ProgressUpdate) { got++ })
	if err != nil {
		t.Fatalf("streaming call died mid-stream (idle deadline not reset?): %v", err)
	}
	if got != 5 {
		t.Fatalf("expected 5 progress frames, got %d", got)
	}
	if string(result) != `{"ok":true}` {
		t.Fatalf("result mismatch: %s", result)
	}
}

func TestContract_ReloadService_SurfacesError(t *testing.T) {
	router := &stubRouter{reloadServiceErr: errString("service crashed on reload")}
	sock := startTestBridge(t, router)

	resp := sendRaw(t, sock, BridgeRequest{Type: ReqReloadService, Name: "svc-x", Token: "admin"})
	if resp.Type != RespError {
		t.Fatalf("expected error response when ReloadService fails; got %+v", resp)
	}
	if !strings.Contains(resp.Message, "service crashed on reload") {
		t.Fatalf("reload error not surfaced to caller: %q", resp.Message)
	}
}

func TestContract_ReloadService_OKOnSuccess(t *testing.T) {
	router := &stubRouter{}
	sock := startTestBridge(t, router)

	resp := sendRaw(t, sock, BridgeRequest{Type: ReqReloadService, Name: "svc-x", Token: "admin"})
	if resp.Type != RespOK {
		t.Fatalf("expected OK on successful reload; got %+v", resp)
	}
	if len(router.reloadServiceIDs) != 1 || router.reloadServiceIDs[0] != "svc-x" {
		t.Fatalf("reload not forwarded: %v", router.reloadServiceIDs)
	}
}

func TestContract_ReloadExternalMcp_SurfacesError(t *testing.T) {
	router := &stubRouter{reloadMcpErr: errString("mcp failed to start")}
	sock := startTestBridge(t, router)

	resp := sendRaw(t, sock, BridgeRequest{Type: ReqReloadExternalMcp, Name: "fs", Token: "admin"})
	if resp.Type != RespError {
		t.Fatalf("expected error response when ReloadExternalMcp fails; got %+v", resp)
	}
	if !strings.Contains(resp.Message, "mcp failed to start") {
		t.Fatalf("reload error not surfaced to caller: %q", resp.Message)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
