// Command testmcp is a minimal stdio JSON-RPC peer used by the hermetic test
// suite to exercise relay's real external-MCP stdio transport
// (externalMcpConn.SendRequest / readLoop) without mocking the connection.
//
// It reads newline-delimited JSON-RPC requests on stdin and writes responses
// on stdout. Behavior is selected by the request method so one binary covers
// every transport test:
//
//	echo                 respond with result == the request params (honors an
//	                     optional {"delayMs":N} to force out-of-order replies)
//	garbage_then_echo    write one malformed line, then a valid echo response
//	                     (exercises readLoop's skip-malformed path)
//	hang                 never respond (exercises ctx-cancel / request-timeout)
//	exit                 os.Exit(0) immediately (exercises reader-death/EOF)
//	<anything else>      treated as echo
//
// Built on demand by buildTestMcpBinary in external_mcp_stdio_test.go.
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"

	"relaygo/jsonrpc"
)

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1<<20)

	out := bufio.NewWriter(os.Stdout)
	var mu sync.Mutex // serializes writes from the main loop + delayed goroutines

	writeLine := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
	writeResp := func(id interface{}, result json.RawMessage) {
		b, _ := json.Marshal(jsonrpc.Response{JSONRPC: jsonrpc.Version, ID: id, Result: result})
		writeLine(b)
	}

	var wg sync.WaitGroup
	for in.Scan() {
		var req struct {
			ID     interface{}     `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			continue
		}

		switch req.Method {
		case "hang":
			// Never respond — the caller's ctx or request timeout must fire.
		case "exit":
			os.Exit(0)
		case "garbage_then_echo":
			writeLine([]byte("{ this is not valid json"))
			writeResp(req.ID, req.Params)
		default: // "echo" and everything else
			var p struct {
				DelayMs int `json:"delayMs"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.DelayMs > 0 {
				wg.Add(1)
				go func(id interface{}, params json.RawMessage, d int) {
					defer wg.Done()
					time.Sleep(time.Duration(d) * time.Millisecond)
					writeResp(id, params)
				}(req.ID, req.Params, p.DelayMs)
			} else {
				writeResp(req.ID, req.Params)
			}
		}
	}
	wg.Wait()
}
