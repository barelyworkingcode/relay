package mcp

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"relaygo/bridge"
	"relaygo/jsonrpc"
)

// Tests for the MCP stdio server. The transport (stdin/stdout) is not
// exercised directly here — handleMethod is the logic under test, and
// it talks to a real bridge.BridgeServer over a /tmp socket for honest
// end-to-end behavior.

// stubRouter for the mcp package — mirrors bridge.stubRouter but
// duplicated to avoid cross-package test imports.
type stubRouter struct {
	mu          sync.Mutex
	tools       json.RawMessage
	toolsErr    error
	callResult  json.RawMessage
	callErr     error
	listedToken string
	calledName  string
	calledArgs  json.RawMessage
	calledToken string
}

func (s *stubRouter) ListTools(_ context.Context, token string) (json.RawMessage, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.listedToken = token
	return s.tools, s.toolsErr
}
func (s *stubRouter) CallTool(_ context.Context, name string, args json.RawMessage, token string) (json.RawMessage, error) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.calledName = name
	s.calledArgs = args
	s.calledToken = token
	return s.callResult, s.callErr
}
func (s *stubRouter) ValidateAdmin(string) error                                           { return nil }
func (s *stubRouter) ReconcileExternalMcps(context.Context)                                {}
func (s *stubRouter) ReloadExternalMcp(context.Context, string)                            {}
func (s *stubRouter) ReloadService(string)                                                 {}
func (s *stubRouter) ListProjects(string) (json.RawMessage, error)                         { return nil, nil }
func (s *stubRouter) GetProject(string, string) (json.RawMessage, error)                   { return nil, nil }
func (s *stubRouter) ResolvePtyEnv(context.Context, bridge.PtyEnvRequest, string) (bridge.PtyEnvResponse, error) {
	return bridge.PtyEnvResponse{}, nil
}
func (s *stubRouter) RegisterManifest(context.Context, bridge.RegisterManifestRequest, string) error {
	return nil
}

// startBridgeForMCP boots a BridgeServer on a /tmp socket and returns
// a bridge.Client connected to it.
func startBridgeForMCP(t *testing.T, router bridge.ToolRouter, token string) *bridge.Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mcptest-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// Override ConfigDir → dir so bridge.NewBridgeServer's SocketPath()
	// and bridge.NewClient both resolve to dir/relay.sock.
	bridge.SetConfigDirForTest(dir)
	t.Cleanup(func() { bridge.SetConfigDirForTest("") })

	srv, err := bridge.NewBridgeServer(context.Background(), router)
	if err != nil {
		t.Fatalf("NewBridgeServer: %v", err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { srv.Close() })

	// Wait briefly for accept loop.
	sockPath := bridge.SocketPath()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.DialTimeout("unix", sockPath, 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket never became dialable: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return bridge.NewClient(token)
}

func TestHandleMethod_Initialize_ReturnsServerInfo(t *testing.T) {
	router := &stubRouter{}
	client := startBridgeForMCP(t, router, "tok")

	id := json.RawMessage(`1`)
	resp := handleMethod(client, &jsonrpc.ServerRequest{Method: MethodInitialize, ID: id})
	if resp == nil {
		t.Fatal("initialize should return a response")
	}
	if resp.Error != nil {
		t.Fatalf("initialize errored: %+v", resp.Error)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result["protocolVersion"] != ProtocolVersion {
		t.Fatalf("protocolVersion = %v; want %s", result["protocolVersion"], ProtocolVersion)
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "relay" {
		t.Fatalf("serverInfo.name = %v; want relay", info["name"])
	}
}

func TestHandleMethod_Initialized_NotificationIgnored(t *testing.T) {
	router := &stubRouter{}
	client := startBridgeForMCP(t, router, "tok")
	resp := handleMethod(client, &jsonrpc.ServerRequest{Method: MethodInitialized}) // notification, no ID
	if resp != nil {
		t.Fatalf("notification must not produce a response; got %+v", resp)
	}
}

func TestHandleMethod_ToolsList_ProxiesToBridge(t *testing.T) {
	router := &stubRouter{tools: json.RawMessage(`[{"name":"alpha"}]`)}
	client := startBridgeForMCP(t, router, "proj-tok")

	id := json.RawMessage(`2`)
	resp := handleMethod(client, &jsonrpc.ServerRequest{Method: MethodToolsList, ID: id})
	if resp == nil || resp.Error != nil {
		t.Fatalf("tools/list errored: %+v", resp)
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(resp.Result, &result)
	if string(result["tools"]) != `[{"name":"alpha"}]` {
		t.Fatalf("tools forwarded wrong: %s", result["tools"])
	}
	if router.listedToken != "proj-tok" {
		t.Fatalf("project token not forwarded; got %q", router.listedToken)
	}
}

func TestHandleMethod_ToolsCall_ForwardsNameAndArgs(t *testing.T) {
	router := &stubRouter{callResult: json.RawMessage(`{"content":[{"type":"text","text":"hi"}]}`)}
	client := startBridgeForMCP(t, router, "proj-tok")

	params, _ := json.Marshal(map[string]any{
		"name":      "fs__read_file",
		"arguments": map[string]any{"path": "/etc/hostname"},
	})
	id := json.RawMessage(`3`)
	resp := handleMethod(client, &jsonrpc.ServerRequest{
		Method: MethodToolsCall,
		ID:     id,
		Params: params,
	})
	if resp == nil || resp.Error != nil {
		t.Fatalf("tools/call errored: %+v", resp)
	}
	if router.calledName != "fs__read_file" {
		t.Fatalf("name not forwarded; got %q", router.calledName)
	}
	if !json.Valid(router.calledArgs) {
		t.Fatalf("forwarded args invalid JSON: %s", router.calledArgs)
	}
}

func TestHandleMethod_ToolsCall_RejectsMissingName(t *testing.T) {
	router := &stubRouter{}
	client := startBridgeForMCP(t, router, "tok")

	params, _ := json.Marshal(map[string]any{"arguments": map[string]any{}})
	id := json.RawMessage(`4`)
	resp := handleMethod(client, &jsonrpc.ServerRequest{
		Method: MethodToolsCall,
		ID:     id,
		Params: params,
	})
	if resp == nil || resp.Error == nil {
		t.Fatalf("expected error response for missing name; got %+v", resp)
	}
	if resp.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("expected InvalidParams; got code %d", resp.Error.Code)
	}
}

func TestHandleMethod_UnknownMethod_Returns404Style(t *testing.T) {
	router := &stubRouter{}
	client := startBridgeForMCP(t, router, "tok")

	id := json.RawMessage(`5`)
	resp := handleMethod(client, &jsonrpc.ServerRequest{Method: "totally/made/up", ID: id})
	if resp == nil || resp.Error == nil {
		t.Fatalf("expected MethodNotFound; got %+v", resp)
	}
	if resp.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("expected MethodNotFound code; got %d", resp.Error.Code)
	}
}

func TestHandleMethod_UnknownMethod_NotificationIgnored(t *testing.T) {
	router := &stubRouter{}
	client := startBridgeForMCP(t, router, "tok")
	// JSON-RPC 2.0: never respond to notifications, even unknown ones.
	resp := handleMethod(client, &jsonrpc.ServerRequest{Method: "totally/made/up"}) // no ID
	if resp != nil {
		t.Fatalf("notification must not produce a response; got %+v", resp)
	}
}
