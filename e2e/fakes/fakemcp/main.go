// Command fakemcp is a fake MCP server for end-to-end tests. It serves a tool
// catalogue read from JSON over stdio or streamable HTTP, optionally behind
// the MCP authorization flow, and records every request to a call log.
//
//	fakemcp --catalogue FILE --call-log FILE [--transport stdio|http]
//	        [--listen 127.0.0.1:0] [--oauth] [--token-ttl DUR]
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"relaye2e/fakes/calllog"
)

const defaultProtocolVersion = "2025-06-18"

const (
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errParse          = -32700
)

type catalogue struct {
	Initialize json.RawMessage   `json:"initialize,omitempty"`
	Tools      []json.RawMessage `json:"tools"`
	// Enumerate maps a field name to the values context/enumerate answers for
	// it. A field not listed gets method-not-found, as an MCP with no
	// enumerable fields does.
	Enumerate map[string][]enumValue `json:"enumerate,omitempty"`
}

type enumValue struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type tool struct {
	name string
	// listed is the tool as sent in tools/list: the catalogue entry minus the
	// control key.
	listed json.RawMessage
	fake   fakeMode
}

// fakeMode is the decoded "x-fake" control key.
type fakeMode struct {
	result   json.RawMessage
	echo     bool
	rpcError *rpcError
	// exit, when set, ends the process with that code once the call's reply is
	// written (stdio only).
	exit *int
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type server struct {
	init      json.RawMessage
	tools     []tool
	enumerate map[string][]enumValue
	log       *calllog.Writer
	// exitCode is set by a call to a tool with an "exit" knob and read by the
	// stdio loop after it has written the reply.
	exitCode *int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakemcp:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		catPath   = flag.String("catalogue", "", "tool catalogue JSON file (required)")
		logPath   = flag.String("call-log", "", "call log file (required)")
		transport = flag.String("transport", "stdio", "stdio or http")
		listen    = flag.String("listen", "127.0.0.1:0", "http listen address")
		oauth     = flag.Bool("oauth", false, "require OAuth on /mcp (http only)")
		tokenTTL  = flag.Duration("token-ttl", time.Hour, "access token lifetime with --oauth")
	)
	flag.Parse()
	if *catPath == "" || *logPath == "" {
		return errors.New("--catalogue and --call-log are required")
	}
	if *transport != "stdio" && *transport != "http" {
		return fmt.Errorf("unknown --transport %q", *transport)
	}
	if *oauth && *transport != "http" {
		return errors.New("--oauth needs --transport http")
	}
	if *tokenTTL <= 0 {
		return errors.New("--token-ttl must be positive")
	}
	s, err := load(*catPath, *logPath)
	if err != nil {
		return err
	}
	if *transport == "stdio" {
		return s.serveStdio(os.Stdin, os.Stdout)
	}
	return s.serveHTTP(*listen, *oauth, *tokenTTL)
}

func load(catPath, logPath string) (*server, error) {
	raw, err := os.ReadFile(catPath)
	if err != nil {
		return nil, fmt.Errorf("read catalogue: %w", err)
	}
	var c catalogue
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse catalogue %q: %w", catPath, err)
	}
	s := &server{init: c.Initialize, enumerate: c.Enumerate}
	for i, entry := range c.Tools {
		t, err := parseTool(entry)
		if err != nil {
			return nil, fmt.Errorf("catalogue tool %d: %w", i, err)
		}
		s.tools = append(s.tools, t)
	}
	if s.log, err = calllog.Open(logPath); err != nil {
		return nil, err
	}
	return s, nil
}

func parseTool(entry json.RawMessage) (tool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(entry, &fields); err != nil {
		return tool{}, fmt.Errorf("not an object: %w", err)
	}
	var name string
	if err := json.Unmarshal(fields["name"], &name); err != nil || name == "" {
		return tool{}, errors.New("missing name")
	}
	t := tool{name: name}
	if ctl, ok := fields["x-fake"]; ok {
		var m struct {
			Result   json.RawMessage `json:"result"`
			Echo     bool            `json:"echo"`
			RPCError *rpcError       `json:"rpc_error"`
			Exit     *int            `json:"exit"`
		}
		if err := json.Unmarshal(ctl, &m); err != nil {
			return tool{}, fmt.Errorf("tool %q: bad x-fake: %w", name, err)
		}
		t.fake = fakeMode{result: m.Result, echo: m.Echo, rpcError: m.RPCError, exit: m.Exit}
		delete(fields, "x-fake")
	}
	listed, err := json.Marshal(fields)
	if err != nil {
		return tool{}, err
	}
	t.listed = listed
	return t, nil
}

// reply is one JSON-RPC response ready to send.
type reply struct {
	body []byte
}

// handle answers one parsed message. A nil reply means the message was a
// notification. The call is logged before the reply is returned.
func (s *server) handle(transport string, req rpcRequest, auth string) *reply {
	_ = s.log.Append(calllog.NewCall(transport, req.Method, req.ID, req.Params, req.Params, auth))
	if len(req.ID) == 0 {
		return nil
	}
	switch req.Method {
	case "initialize":
		return result(req.ID, s.initializeResult(req.Params))
	case "tools/list":
		tools := make([]json.RawMessage, 0, len(s.tools))
		for _, t := range s.tools {
			tools = append(tools, t.listed)
		}
		return result(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		return s.callTool(req)
	case "context/enumerate":
		return s.enumerateField(req)
	default:
		return failure(req.ID, errMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *server) enumerateField(req rpcRequest) *reply {
	var p struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return failure(req.ID, errInvalidParams, "invalid params")
	}
	values, ok := s.enumerate[p.Field]
	if !ok {
		return failure(req.ID, errMethodNotFound, "method not found: context/enumerate")
	}
	if values == nil {
		values = []enumValue{}
	}
	return result(req.ID, map[string]any{"field": p.Field, "values": values})
}

func (s *server) initializeResult(params json.RawMessage) map[string]any {
	version := defaultProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		version = p.ProtocolVersion
	}
	res := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "fakemcp", "version": "1"},
	}
	var extra map[string]json.RawMessage
	if len(s.init) > 0 && json.Unmarshal(s.init, &extra) == nil {
		for k, v := range extra {
			res[k] = v
		}
	}
	return res
}

func (s *server) callTool(req rpcRequest) *reply {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return failure(req.ID, errInvalidParams, "invalid params")
	}
	for _, t := range s.tools {
		if t.name != p.Name {
			continue
		}
		s.exitCode = t.fake.exit
		switch {
		case t.fake.rpcError != nil:
			return failure(req.ID, t.fake.rpcError.Code, t.fake.rpcError.Message)
		case t.fake.result != nil:
			return &reply{body: envelope(req.ID, "result", t.fake.result)}
		case t.fake.echo:
			args := p.Arguments
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			return result(req.ID, textResult(string(args)))
		default:
			return result(req.ID, textResult("ok"))
		}
	}
	return failure(req.ID, errInvalidParams, "unknown tool: "+p.Name)
}

func textResult(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func result(id json.RawMessage, v any) *reply {
	b, err := json.Marshal(v)
	if err != nil {
		return failure(id, -32603, "internal error")
	}
	return &reply{body: envelope(id, "result", b)}
}

func failure(id json.RawMessage, code int, msg string) *reply {
	b, _ := json.Marshal(rpcError{Code: code, Message: msg})
	return &reply{body: envelope(id, "error", b)}
}

func envelope(id json.RawMessage, key string, payload json.RawMessage) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out := []byte(`{"jsonrpc":"2.0","id":`)
	out = append(out, id...)
	out = append(out, `,"`+key+`":`...)
	out = append(out, payload...)
	return append(out, '}')
}

// parseFailure logs and answers a body that is not a JSON-RPC object.
func (s *server) parseFailure(transport string, raw []byte, auth string) *reply {
	_ = s.log.Append(calllog.NewCall(transport, "parse_error", nil, nil, raw, auth))
	return failure(nil, errParse, "parse error")
}

func (s *server) serveStdio(in io.Reader, out io.Writer) error {
	var mu sync.Mutex
	br := bufio.NewReader(in)
	for {
		line, err := br.ReadBytes('\n')
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			var req rpcRequest
			var rep *reply
			if json.Unmarshal([]byte(trimmed), &req) != nil {
				rep = s.parseFailure("stdio", []byte(trimmed), "none")
			} else {
				rep = s.handle("stdio", req, "none")
			}
			if rep != nil {
				mu.Lock()
				_, werr := out.Write(append(rep.body, '\n'))
				mu.Unlock()
				if werr != nil {
					return werr
				}
				if s.exitCode != nil {
					os.Exit(*s.exitCode)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
