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

// Contract tests for the bridge wire protocol.
//
// These exercise the REAL BridgeServer with a stub ToolRouter, dialing a
// REAL Client over a per-test Unix socket. Catches accidental wire-format
// breaks that unit tests of either side alone would miss — every external
// consumer (relayLLM, scheduler, eve, the relay MCP subprocess) decodes
// the same JSON shape, so a field rename here cascades silently.
//
// Sockets live in /tmp because macOS Unix-socket paths are capped at 104
// chars and t.TempDir() paths often exceed that.

// stubRouter implements ToolRouter with scripted responses recorded for
// later assertion. Every method records its arguments and returns
// whatever the test scripted via the public Set* hooks.
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
	callToolProgress []ProgressUpdate // emitted via the ctx sink before the result

	validateAdminToks []string
	validateAdminErr  error

	reconcileCalls int

	reloadMcpIDs []string

	reloadServiceIDs []string

	listProjectsToks []string
	listProjectsResp json.RawMessage
	listProjectsErr  error

	getProjectIDs  []string
	getProjectToks []string
	getProjectResp json.RawMessage
	getProjectErr  error

	resolvePtyReqs []PtyEnvRequest
	resolvePtyToks []string
	resolvePtyResp PtyEnvResponse
	resolvePtyErr  error

	registerReqs []RegisterManifestRequest
	registerToks []string
	registerErr  error
}

func (s *stubRouter) ListTools(_ context.Context, token string) (json.RawMessage, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.listToolsTokens = append(s.listToolsTokens, token)
	return s.listToolsResponse, s.listToolsErr
}

func (s *stubRouter) CallTool(ctx context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error) {
	s.mu.Lock()
	s.callToolNames = append(s.callToolNames, name)
	s.callToolArgs = append(s.callToolArgs, args)
	s.callToolToks = append(s.callToolToks, token)
	prog := s.callToolProgress
	resp, err := s.callToolResp, s.callToolErr
	s.mu.Unlock()
	if sink := ProgressFromContext(ctx); sink != nil {
		for _, u := range prog {
			sink(u)
		}
	}
	return resp, err
}

func (s *stubRouter) ValidateAdmin(token string) error {
	s.mu.Lock(); defer s.mu.Unlock()
	s.validateAdminToks = append(s.validateAdminToks, token)
	return s.validateAdminErr
}

func (s *stubRouter) ReconcileExternalMcps(_ context.Context) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.reconcileCalls++
}

func (s *stubRouter) ReloadExternalMcp(_ context.Context, id string) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.reloadMcpIDs = append(s.reloadMcpIDs, id)
}

func (s *stubRouter) ReloadService(id string) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.reloadServiceIDs = append(s.reloadServiceIDs, id)
}

func (s *stubRouter) ListProjects(token string) (json.RawMessage, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.listProjectsToks = append(s.listProjectsToks, token)
	return s.listProjectsResp, s.listProjectsErr
}

func (s *stubRouter) GetProject(id, token string) (json.RawMessage, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.getProjectIDs = append(s.getProjectIDs, id)
	s.getProjectToks = append(s.getProjectToks, token)
	return s.getProjectResp, s.getProjectErr
}

func (s *stubRouter) ResolvePtyEnv(_ context.Context, req PtyEnvRequest, token string) (PtyEnvResponse, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.resolvePtyReqs = append(s.resolvePtyReqs, req)
	s.resolvePtyToks = append(s.resolvePtyToks, token)
	return s.resolvePtyResp, s.resolvePtyErr
}

func (s *stubRouter) RegisterManifest(_ context.Context, req RegisterManifestRequest, token string) error {
	s.mu.Lock(); defer s.mu.Unlock()
	s.registerReqs = append(s.registerReqs, req)
	s.registerToks = append(s.registerToks, token)
	return s.registerErr
}

// startTestBridge spins up a BridgeServer on a short /tmp socket path
// against the given router. Returns the socket path and a cleanup func.
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

	// Wait for the socket to be dialable.
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

	// Plain CallTool must still work (progress frames silently discarded).
	result2, err := c.CallTool("generate_image", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if string(result2) != `{"ok":true}` {
		t.Fatalf("result2 mismatch: %s", result2)
	}
}

func TestContract_RegisterManifest(t *testing.T) {
	router := &stubRouter{} // ValidateAdmin returns nil — service-token check is in routermod
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
	// Validation happens server-side via Manifest.Validate before reaching
	// the router. Verify a bad payload yields an error response without
	// invoking the router.
	router := &stubRouter{}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "svc-token"}

	// Missing serviceID — should fail Validate().
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

func TestContract_ListProjects(t *testing.T) {
	router := &stubRouter{listProjectsResp: json.RawMessage(`[{"id":"p1"}]`)}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "svc"}

	data, err := c.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if string(data) != `[{"id":"p1"}]` {
		t.Fatalf("payload: %s", data)
	}
}

func TestContract_GetProject(t *testing.T) {
	router := &stubRouter{getProjectResp: json.RawMessage(`{"id":"acme"}`)}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "svc"}

	data, err := c.GetProject("acme")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if string(data) != `{"id":"acme"}` {
		t.Fatalf("payload: %s", data)
	}
	if router.getProjectIDs[0] != "acme" {
		t.Fatalf("id not forwarded: %v", router.getProjectIDs)
	}
}

func TestContract_ResolvePtyEnv(t *testing.T) {
	router := &stubRouter{
		resolvePtyResp: PtyEnvResponse{
			RelayToken: "scoped-tok",
			WorkingDir: "/tmp/proj",
			SkillPath:  "/tmp/proj/.claude/skills/relay",
		},
	}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "svc"}

	resp, err := c.ResolvePtyEnv(PtyEnvRequest{
		Project:     "acme",
		Directory:   "/tmp/proj",
		RegenSkills: RegenSkillsAlways,
		SkillPath:   "/tmp/proj/.claude/skills/relay",
	})
	if err != nil {
		t.Fatalf("ResolvePtyEnv: %v", err)
	}
	if resp.RelayToken != "scoped-tok" || resp.WorkingDir != "/tmp/proj" {
		t.Fatalf("response mangled: %+v", resp)
	}
	if router.resolvePtyReqs[0].Project != "acme" {
		t.Fatalf("request not forwarded: %+v", router.resolvePtyReqs[0])
	}
}

func TestContract_AdminGated_RejectsBadToken(t *testing.T) {
	// ValidateAdmin returns an error → bridge must reject before invoking handler.
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
	// ValidateAdmin returns nil → handler runs.
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

// sendRaw writes one request and reads one response over a fresh
// connection. Bypasses Client so tests can exercise admin-gated calls
// without depending on bridge.SocketPath().
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

// errString is a small constant-error helper.
type errString string

func (e errString) Error() string { return string(e) }
