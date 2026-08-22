// Command testmcp is a minimal stdio JSON-RPC peer used by the hermetic test
// suite to exercise relay's real external-MCP stdio transport
// (externalMcpConn.SendRequest / readLoop) without mocking the connection.
//
// It reads newline-delimited JSON-RPC requests on stdin and writes responses
// on stdout. Behavior is selected by the request method so one binary covers
// every transport test:
//
//	initialize           respond with a minimal MCP initialize result (plus a
//	                     v2 contextSchema when RELAY_TESTMCP_CONTEXT is set)
//	context/enumerate    ADR-011 decision 6. Behaviour is selected by the
//	                     RELAY_TESTMCP_CONTEXT env var: unset or "off" answers
//	                     -32601 (the shape of an MCP that does not implement
//	                     it), "v2" declares the schema and answers real values,
//	                     "unsupported" declares the schema and still answers
//	                     -32601.
//	tools/list           respond with one stub tool (so mcpHandshake succeeds)
//	echo                 respond with result == the request params (honors an
//	                     optional {"delayMs":N} to force out-of-order replies)
//	garbage_then_echo    write one malformed line, then a valid echo response
//	                     (exercises readLoop's skip-malformed path)
//	hang                 never respond (exercises ctx-cancel / request-timeout)
//	exit                 os.Exit(0) immediately (exercises reader-death/EOF)
//	<anything else>      treated as echo
//
// Requests with no id (notifications, e.g. notifications/initialized) get no
// response. Implementing initialize + tools/list makes testmcp a real minimal
// MCP, so it doubles as the upstream for manager Reconcile/Reload tests.
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

// contextMode selects the context/enumerate behaviour. Off by default so every
// test that predates ADR-011 sees exactly the peer it always saw — including
// declaring no contextSchema at all, which is what makes "relay has never seen
// a schema for this MCP" reachable.
func contextMode() string { return os.Getenv("RELAY_TESTMCP_CONTEXT") }

// enumSchema is macMCP's worked example from ADR-011, which is what relay's
// operator surfaces are built against: two operator-set enumerable fields, the
// second depending on the first, plus one relay derives from the project path
// and therefore never enumerates.
const enumSchema = `{
  "mail_accounts": {"type":"array","items":{"type":"string"},
    "description":"Mail accounts this client may read from or send as",
    "scope":"restrict","source":"operator","applies_to":["mail_*"],"enumerable":true},
  "mail_mailboxes": {"type":"array","items":{"type":"string"},
    "description":"Mailbox paths within those accounts this client may reach",
    "scope":"restrict","source":"operator","applies_to":["mail_*"],"enumerable":true,
    "depends_on":["mail_accounts"]},
  "write_dirs": {"type":"array","items":{"type":"string"},
    "description":"Directories this client may write files into",
    "scope":"restrict","source":"project_path","applies_to":["mail_save_attachment"]}
}`

// allAccounts is what the peer holds. mail_mailboxes enumerates ONE entry per
// account in scope, so a test can tell "listed within Bob" from "listed across
// every account" by counting.
var allAccounts = []string{"Alice", "Bob"}

// enumerate answers a context/enumerate request, or returns a JSON-RPC error.
func enumerate(params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	var req struct {
		Field  string              `json:"field"`
		Values map[string][]string `json:"values"`
	}
	_ = json.Unmarshal(params, &req)

	type value struct {
		Value string `json:"value"`
		Label string `json:"label,omitempty"`
	}
	out := struct {
		Field  string  `json:"field"`
		Values []value `json:"values"`
	}{Field: req.Field, Values: []value{}}

	switch req.Field {
	case "mail_accounts":
		for _, a := range allAccounts {
			out.Values = append(out.Values, value{Value: a, Label: a})
		}
	case "mail_mailboxes":
		// An absent OR EMPTY dependency means "across everything". Reading an
		// empty list as "match nothing" is the bug that shows an empty picker
		// in exactly the state an operator opens one in.
		accounts := req.Values["mail_accounts"]
		if len(accounts) == 0 {
			accounts = allAccounts
		}
		for _, a := range accounts {
			// One distinct value per account in scope, so a test can tell
			// "listed within Bob" from "listed across every account" by what
			// came back rather than by trusting the request it sent.
			out.Values = append(out.Values, value{Value: a + "/INBOX", Label: "INBOX (" + a + ")"})
		}
	case "no_such_mailbox_field":
		// A field this peer holds nothing for: a real, empty answer.
	default:
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams,
			Message: "no enumerable field named " + req.Field}
	}
	b, _ := json.Marshal(out)
	return b, nil
}

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
	writeErr := func(id interface{}, code int, msg string) {
		b, _ := json.Marshal(jsonrpc.Response{JSONRPC: jsonrpc.Version, ID: id,
			Error: &jsonrpc.Error{Code: code, Message: msg}})
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
		if req.ID == nil {
			continue // notification (e.g. notifications/initialized) — no response
		}

		switch req.Method {
		case "initialize":
			if contextMode() == "v2" || contextMode() == "unsupported" {
				writeResp(req.ID, json.RawMessage(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"testmcp","contextSchemaVersion":2,"contextSchema":`+enumSchema+`},"capabilities":{}}`))
			} else {
				writeResp(req.ID, json.RawMessage(`{"protocolVersion":"2024-11-05","serverInfo":{"name":"testmcp"},"capabilities":{}}`))
			}
		case "context/enumerate":
			if contextMode() != "v2" {
				writeErr(req.ID, jsonrpc.CodeMethodNotFound, "method not found: context/enumerate")
				break
			}
			result, rpcErr := enumerate(req.Params)
			if rpcErr != nil {
				writeErr(req.ID, rpcErr.Code, rpcErr.Message)
				break
			}
			writeResp(req.ID, result)
		case "tools/list":
			writeResp(req.ID, json.RawMessage(`{"tools":[{"name":"testmcp_ping","description":"ping","inputSchema":{"type":"object"}}]}`))
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
