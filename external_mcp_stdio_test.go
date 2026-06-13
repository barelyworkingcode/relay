//go:build !windows

package main

// Real stdio-transport coverage for externalMcpConn. Every other test in the
// suite substitutes mockMcpConn, which bypasses the JSON-RPC framing, the
// pending-request ID map, the reader-death signaling, and the stdin-write
// path entirely. These tests drive the genuine spawnStdioConn → readLoop →
// SendRequest pipeline against the in-tree cmd/testmcp peer, so a framing,
// ID-routing, or deadlock regression in the actual fsMCP/macMCP code path is
// caught — none of it was before.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	testmcpBinOnce sync.Once
	testmcpBinPath string
	testmcpBinErr  error
)

// buildTestMcpBinary compiles cmd/testmcp once per test run and returns the
// path. Mirrors buildTestServiceBinary.
func buildTestMcpBinary(t *testing.T) string {
	t.Helper()
	testmcpBinOnce.Do(func() {
		dir, err := os.MkdirTemp("/tmp", "testmcp-bin-")
		if err != nil {
			testmcpBinErr = err
			return
		}
		path := filepath.Join(dir, "testmcp")
		cmd := exec.Command("go", "build", "-o", path, "./cmd/testmcp")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			testmcpBinErr = err
			return
		}
		testmcpBinPath = path
	})
	if testmcpBinErr != nil {
		t.Fatalf("build cmd/testmcp: %v", testmcpBinErr)
	}
	return testmcpBinPath
}

// newTestMcpConn spawns a real stdio connection to the testmcp peer and
// registers cleanup.
func newTestMcpConn(t *testing.T) *externalMcpConn {
	t.Helper()
	bin := buildTestMcpBinary(t)
	conn, err := spawnStdioConn(bin, nil, nil, nil)
	if err != nil {
		t.Fatalf("spawnStdioConn: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// markerOf decodes the {"marker":...} echo result.
func markerOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var p struct {
		Marker string `json:"marker"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode echo result %q: %v", raw, err)
	}
	return p.Marker
}

func TestStdioConn_RoundTrip(t *testing.T) {
	conn := newTestMcpConn(t)
	res, err := conn.SendRequest(context.Background(), "echo", map[string]any{"marker": "hello"})
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if got := markerOf(t, res); got != "hello" {
		t.Errorf("round-trip marker = %q, want hello", got)
	}
}

// Concurrent requests with deliberately reordered replies must each return
// their OWN response — proving the pending map routes by JSON-RPC ID, not by
// arrival order.
func TestStdioConn_ConcurrentIDRouting(t *testing.T) {
	conn := newTestMcpConn(t)

	const n = 8
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := fmt.Sprintf("m%d", i)
			// Earlier requests get the longest delay, so replies arrive in
			// reverse order relative to send order.
			delay := (n - i) * 15
			res, err := conn.SendRequest(context.Background(), "echo",
				map[string]any{"marker": marker, "delayMs": delay})
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			results[i] = markerOf(t, res)
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		want := fmt.Sprintf("m%d", i)
		if results[i] != want {
			t.Errorf("request %d got %q, want %q (ID routing crossed responses)", i, results[i], want)
		}
	}
}

// readLoop must skip a malformed response line and still deliver the valid
// response that follows it.
func TestStdioConn_SkipsMalformedLine(t *testing.T) {
	conn := newTestMcpConn(t)
	res, err := conn.SendRequest(context.Background(), "garbage_then_echo", map[string]any{"marker": "survived"})
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if got := markerOf(t, res); got != "survived" {
		t.Errorf("marker = %q, want survived (malformed line should have been skipped)", got)
	}
}

// When the child process dies mid-request, the in-flight SendRequest must
// return the reader error rather than hang, and every subsequent request must
// fail fast (prepareRequest sees readerDone closed).
func TestStdioConn_ReaderDeathOnProcessExit(t *testing.T) {
	conn := newTestMcpConn(t)

	if _, err := conn.SendRequest(context.Background(), "exit", nil); err == nil {
		t.Fatal("expected an error when the child exits mid-request")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := conn.SendRequest(ctx, "echo", map[string]any{"marker": "x"}); err == nil {
		t.Fatal("expected fast failure after reader death, got success")
	}
}

// A caller-supplied context deadline must cancel a stuck request and reclaim
// its pending slot.
func TestStdioConn_ContextCancellation(t *testing.T) {
	conn := newTestMcpConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := conn.SendRequest(ctx, "hang", nil)
	if err == nil {
		t.Fatal("expected context-deadline error on a hung request")
	}
	if ctx.Err() == nil {
		t.Errorf("context should be done; SendRequest returned %v", err)
	}
}

// The MCPRequestTimeout fallback (the timer.C branch) must fire when neither a
// response nor a caller deadline arrives. MCPRequestTimeout is a var precisely
// so this path can be exercised deterministically.
func TestStdioConn_RequestTimeout(t *testing.T) {
	old := MCPRequestTimeout
	MCPRequestTimeout = 150 * time.Millisecond
	t.Cleanup(func() { MCPRequestTimeout = old })

	conn := newTestMcpConn(t)
	_, err := conn.SendRequest(context.Background(), "hang", nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want request-timeout error, got %v", err)
	}
}
