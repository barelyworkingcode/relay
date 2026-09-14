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
	listToolsCwds     []string
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

	listProjectsToks []string
	listProjectsResp json.RawMessage
	listProjectsErr  error

	getProjectIDs  []string
	getProjectToks []string
	getProjectResp json.RawMessage
	getProjectErr  error

	describeProjectToks []string
	describeProjectResp ProjectDescription
	describeProjectErr  error

	resolvePtyReqs []PtyEnvRequest
	resolvePtyToks []string
	resolvePtyResp PtyEnvResponse
	resolvePtyErr  error

	resolveTemplateReqs []ShellTemplateRequest
	resolveTemplateToks []string
	resolveTemplateResp ShellTemplateResponse
	resolveTemplateErr  error

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
	s.listToolsCwds = append(s.listToolsCwds, CallerCwdFromContext(ctx))
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

func (s *stubRouter) Hello(_ context.Context, name, secret string) (HelloResult, error) {
	return HelloResult{Kind: "service", ServiceID: name, RelayPID: 1}, nil
}

func (s *stubRouter) ListProjects(_ context.Context, token string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listProjectsToks = append(s.listProjectsToks, token)
	return s.listProjectsResp, s.listProjectsErr
}

func (s *stubRouter) GetProject(_ context.Context, id, token string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getProjectIDs = append(s.getProjectIDs, id)
	s.getProjectToks = append(s.getProjectToks, token)
	return s.getProjectResp, s.getProjectErr
}

func (s *stubRouter) DescribeProject(_ context.Context, token string) (ProjectDescription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.describeProjectToks = append(s.describeProjectToks, token)
	return s.describeProjectResp, s.describeProjectErr
}

func (s *stubRouter) ResolvePtyEnv(_ context.Context, req PtyEnvRequest, token string) (PtyEnvResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolvePtyReqs = append(s.resolvePtyReqs, req)
	s.resolvePtyToks = append(s.resolvePtyToks, token)
	return s.resolvePtyResp, s.resolvePtyErr
}

func (s *stubRouter) ResolveProjectTemplate(_ context.Context, req ShellTemplateRequest, token string) (ShellTemplateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveTemplateReqs = append(s.resolveTemplateReqs, req)
	s.resolveTemplateToks = append(s.resolveTemplateToks, token)
	return s.resolveTemplateResp, s.resolveTemplateErr
}

func (s *stubRouter) RegisterManifest(_ context.Context, req RegisterManifestRequest, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registerReqs = append(s.registerReqs, req)
	s.registerToks = append(s.registerToks, token)
	return s.registerErr
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

func TestContract_TokenlessSendsCwd(t *testing.T) {
	router := &stubRouter{listToolsResponse: json.RawMessage(`[]`)}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, cwd: "/Users/you/projects/acme/sub"}

	if _, err := c.ListTools(); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := router.listToolsCwds[0]; got != "/Users/you/projects/acme/sub" {
		t.Fatalf("cwd not delivered to router; got %q", got)
	}
	if got := router.listToolsTokens[0]; got != "" {
		t.Fatalf("expected an empty token, got %q", got)
	}
}

// A directory must never be able to re-scope an authenticated call.
func TestContract_TokenSuppressesCwd(t *testing.T) {
	router := &stubRouter{listToolsResponse: json.RawMessage(`[]`)}
	sock := startTestBridge(t, router)

	// NewClient wouldn't populate cwd alongside a token; set both by hand so
	// this asserts the SERVER-side rule, not just the client's restraint.
	c := &Client{sockPath: sock, token: "proj-token", cwd: "/Users/you/projects/acme"}
	if _, err := c.ListTools(); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := router.listToolsCwds[0]; got != "" {
		t.Fatalf("cwd leaked into an authenticated call: %q", got)
	}
}

func TestNewClient_CwdOnlyWhenTokenless(t *testing.T) {
	if c := NewClient(""); c.cwd == "" {
		t.Error("tokenless client should capture its working directory")
	}
	if c := NewClient("some-token"); c.cwd != "" {
		t.Errorf("tokened client should not capture a working directory, got %q", c.cwd)
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
		},
	}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "svc"}

	resp, err := c.ResolvePtyEnv(PtyEnvRequest{
		Project:   "acme",
		Directory: "/tmp/proj",
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

func TestContract_ResolveProjectTemplate(t *testing.T) {
	router := &stubRouter{
		resolveTemplateResp: ShellTemplateResponse{
			ID:      "ssh-box",
			Name:    "Box SSH",
			Command: "ssh",
			Args:    []string{"me@box"},
			Env:     map[string]string{"TERM": "xterm"},
		},
	}
	sock := startTestBridge(t, router)
	c := &Client{sockPath: sock, token: "svc"}

	resp, err := c.ResolveProjectTemplate(ShellTemplateRequest{
		ProjectID:  "proj-1",
		TemplateID: "ssh-box",
	})
	if err != nil {
		t.Fatalf("ResolveProjectTemplate: %v", err)
	}
	if resp.Command != "ssh" || resp.Name != "Box SSH" || len(resp.Args) != 1 || resp.Args[0] != "me@box" {
		t.Fatalf("response mangled over the wire: %+v", resp)
	}
	if resp.Env["TERM"] != "xterm" {
		t.Fatalf("env not carried over the wire: %+v", resp.Env)
	}
	if len(router.resolveTemplateReqs) != 1 ||
		router.resolveTemplateReqs[0].ProjectID != "proj-1" ||
		router.resolveTemplateReqs[0].TemplateID != "ssh-box" {
		t.Fatalf("request ids not forwarded: %+v", router.resolveTemplateReqs)
	}
	if router.resolveTemplateToks[0] != "svc" {
		t.Fatalf("token not forwarded: %v", router.resolveTemplateToks)
	}
}

func TestContract_ResolveProjectTemplate_MissingArguments(t *testing.T) {
	router := &stubRouter{}
	sock := startTestBridge(t, router)

	resp := sendRaw(t, sock, BridgeRequest{
		Type:  ReqResolveProjectTemplate,
		Token: "svc",
	})
	if resp.Type != RespError {
		t.Fatalf("expected error response for missing arguments; got %+v", resp)
	}
	if len(router.resolveTemplateReqs) != 0 {
		t.Fatalf("router must not be invoked when arguments are missing; got %+v", router.resolveTemplateReqs)
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
