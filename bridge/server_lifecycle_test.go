package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaygo/jsonrpc"
)

// newTestServer builds a BridgeServer on a short /tmp socket and starts its
// accept loop. Unlike startTestBridge it returns the *BridgeServer so the
// caller can drive Close() directly and assert on shutdown behavior.
func newTestServer(t *testing.T, router ToolRouter) (*BridgeServer, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "br")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "b.sock")
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &BridgeServer{router: router, listener: ln, sockPath: sockPath, ctx: ctx, cancel: cancel}
	go func() { _ = srv.Serve() }()
	return srv, sockPath
}

// CR-1: Close() must not deadlock when a connection handler is parked in
// scanner.Scan() with nothing to read. Before the socket-close-on-cancel fix,
// StopAccepting() only cancelled the context and closed the listener, so an
// in-flight Scan() never unblocked and wg.Wait() hung forever — the documented
// "SIGTERM ignored, SIGKILL required" stall.
func TestServer_CloseDoesNotHangOnIdleConnection(t *testing.T) {
	srv, sockPath := newTestServer(t, &stubRouter{listToolsResponse: json.RawMessage(`[]`)})

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Drive one request/response so we know the handler goroutine is alive and
	// has looped back into Scan(); the connection is then left idle.
	_, _ = conn.Write([]byte(`{"type":"ListTools","token":"x"}` + "\n"))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	sc := NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("no response to priming request: %v", sc.Err())
	}
	_ = conn.SetReadDeadline(time.Time{})

	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("BridgeServer.Close() hung on an idle connection (CR-1 regression)")
	}
}

// CR-14: a single line larger than MaxMessageSize must yield an InvalidParams
// error frame before the connection is dropped, rather than a silent close
// that surfaces to the client as a generic "read failed".
func TestServer_OversizedLineGetsErrorFrame(t *testing.T) {
	sock := startTestBridge(t, &stubRouter{})
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Write one oversized, newline-free payload. The write may error (EPIPE)
	// once the server gives up reading; that's expected, so it runs detached.
	go func() {
		_, _ = conn.Write(bytes.Repeat([]byte("a"), MaxMessageSize+128*1024))
	}()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	sc := NewScanner(conn)
	if !sc.Scan() {
		t.Fatalf("expected an error frame before close; got read err: %v", sc.Err())
	}
	var resp BridgeResponse
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Type != RespError {
		t.Fatalf("expected RespError, got %+v", resp)
	}
	if resp.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("expected CodeInvalidParams (%d), got %d", jsonrpc.CodeInvalidParams, resp.Code)
	}
	if !strings.Contains(resp.Message, "maximum size") {
		t.Errorf("message should mention the size limit: %q", resp.Message)
	}
}
