// Command testclaude is a hermetic Claude Code stand-in the suite spawns in
// place of the real CLI. It speaks the stream-json protocol relay's
// ClaudeProvider uses (--input-format/--output-format stream-json) and, for
// every user message, makes exactly one tool call the way Claude Code would:
// it runs the PreToolUse hooks from ./.claude/settings.local.json, interprets
// their output, and calls the tool on the --mcp-config server only on allow.
//
// Environment:
//
//	TESTCLAUDE_TOOL        tool to call (default: mcp__relay__<first tool the
//	                       "relay" server lists>, else mcp__relay__probe)
//	TESTCLAUDE_TOOL_INPUT  JSON object for tool_input / tools/call arguments
//	                       (default {})
//	TESTCLAUDE_OUT         if set, one JSON line per turn is appended here:
//	                       {tool, hookStdout, hookExit, decision, reason, ran, result}
//
// decision is one of allow, deny, ask, exit2 or none (no decision: exit 0
// with no usable output, any other exit code, or a hook timeout). hookExit is
// -1 when no hook ran or the hook timed out.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const relayServer = "relay"

type options struct {
	mcpConfig      string
	permissionMode string
	sessionID      string
}

// parseArgs picks out the flags this stand-in honours and ignores the rest,
// so relay can pass the real CLI's full argv unchanged.
func parseArgs(args []string) options {
	o := options{permissionMode: "default"}
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		next := func() string {
			if hasVal {
				return val
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch name {
		case "--mcp-config":
			o.mcpConfig = next()
		case "--permission-mode":
			o.permissionMode = next()
		case "--resume":
			o.sessionID = next()
		}
	}
	if o.sessionID == "" {
		o.sessionID = randomID()
	}
	return o
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// stream-json output

var stdout = bufio.NewWriter(os.Stdout)

func emit(v any) {
	b, _ := json.Marshal(v)
	_, _ = stdout.Write(append(b, '\n'))
	_ = stdout.Flush()
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ---------------------------------------------------------------------------
// MCP

func connectRelay(ctx context.Context, configPath string) (*mcp.ClientSession, error) {
	if configPath == "" {
		return nil, errors.New("no --mcp-config")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read mcp config: %w", err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse mcp config: %w", err)
	}
	srv, ok := cfg.MCPServers[relayServer]
	if !ok || srv.Command == "" {
		return nil, fmt.Errorf("mcp config has no %q server", relayServer)
	}
	cmd := exec.Command(srv.Command, srv.Args...)
	cmd.Env = os.Environ()
	for k, v := range srv.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "testclaude", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect %q: %w", relayServer, err)
	}
	return session, nil
}

func firstToolName(ctx context.Context, session *mcp.ClientSession) string {
	if session == nil {
		return ""
	}
	res, err := session.ListTools(ctx, nil)
	if err != nil || len(res.Tools) == 0 {
		return ""
	}
	return res.Tools[0].Name
}

// callTool returns the tool's text and whether it is an error result.
func callTool(session *mcp.ClientSession, tool string, input json.RawMessage) (string, bool) {
	name, ok := strings.CutPrefix(tool, "mcp__"+relayServer+"__")
	if !ok {
		return "testclaude: only mcp__" + relayServer + "__ tools can run", true
	}
	if session == nil {
		return "testclaude: no " + relayServer + " MCP server connected", true
	}
	var args map[string]any
	_ = json.Unmarshal(input, &args)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "tools/call " + name + ": " + err.Error(), true
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String(), res.IsError
}

// ---------------------------------------------------------------------------
// PreToolUse hooks

type hookCommand struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// preToolUseHooks returns the command hooks whose matcher selects tool.
// Matchers follow Claude Code: empty or "*" matches everything, otherwise
// the matcher is a regex that must match the whole tool name.
func preToolUseHooks(tool string) []hookCommand {
	data, err := os.ReadFile(filepath.Join(".claude", "settings.local.json"))
	if err != nil {
		return nil
	}
	var settings struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string        `json:"matcher"`
				Hooks   []hookCommand `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return nil
	}
	var out []hookCommand
	for _, m := range settings.Hooks.PreToolUse {
		if m.Matcher != "" && m.Matcher != "*" {
			re, err := regexp.Compile("^(?:" + m.Matcher + ")$")
			if err != nil || !re.MatchString(tool) {
				continue
			}
		}
		out = append(out, m.Hooks...)
	}
	return out
}

type hookOutcome struct {
	stdout   string
	exit     int
	decision string // allow, deny, ask, exit2, none
	reason   string
}

func runHook(h hookCommand, payload []byte) hookOutcome {
	timeout := time.Duration(h.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", h.Command)
	cmd.Stdin = bytes.NewReader(payload)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()

	o := hookOutcome{stdout: out.String(), exit: -1, decision: "none"}
	if cmd.ProcessState != nil {
		o.exit = cmd.ProcessState.ExitCode()
	}
	switch {
	case ctx.Err() != nil:
		o.exit = -1
	case o.exit == 2:
		o.decision, o.reason = "exit2", strings.TrimSpace(errOut.String())
	case err == nil:
		var resp struct {
			HookSpecificOutput struct {
				PermissionDecision       string `json:"permissionDecision"`
				PermissionDecisionReason string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		}
		if json.Unmarshal(out.Bytes(), &resp) == nil {
			switch d := resp.HookSpecificOutput.PermissionDecision; d {
			case "allow", "deny", "ask":
				o.decision, o.reason = d, resp.HookSpecificOutput.PermissionDecisionReason
			}
		}
	}
	return o
}

// decide runs every matching hook. Like Claude Code, any refusal beats an
// allow; with no hook or no decision, the default permission check applies,
// which under --print refuses.
func decide(hooks []hookCommand, payload []byte) hookOutcome {
	final := hookOutcome{exit: -1, decision: "none"}
	for _, h := range hooks {
		o := runHook(h, payload)
		switch {
		case o.decision == "deny" || o.decision == "ask" || o.decision == "exit2":
			return o
		case o.decision == "allow" || final.decision == "none":
			final = o
		}
	}
	return final
}

func refusal(tool string, o hookOutcome) string {
	if o.decision == "deny" || o.decision == "exit2" {
		return o.reason
	}
	return "Claude requested permissions to use " + tool + ", but you haven't granted it yet."
}

// ---------------------------------------------------------------------------
// turns

type record struct {
	Tool       string `json:"tool"`
	HookStdout string `json:"hookStdout"`
	HookExit   int    `json:"hookExit"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason"`
	Ran        bool   `json:"ran"`
	Result     string `json:"result"`
}

func appendRecord(r record) {
	path := os.Getenv("TESTCLAUDE_OUT")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testclaude: open TESTCLAUDE_OUT:", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, _ = f.Write(append(b, '\n'))
}

type runner struct {
	opts    options
	cwd     string
	mcp     *mcp.ClientSession
	mcpErr  error
	tool    string
	input   json.RawMessage
	started bool
	turn    int
}

func (r *runner) init() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r.mcp, r.mcpErr = connectRelay(ctx, r.opts.mcpConfig)

	r.tool = os.Getenv("TESTCLAUDE_TOOL")
	if r.tool == "" {
		if name := firstToolName(ctx, r.mcp); name != "" {
			r.tool = "mcp__" + relayServer + "__" + name
		} else {
			r.tool = "mcp__" + relayServer + "__probe"
		}
	}
	r.input = json.RawMessage(`{}`)
	if in := os.Getenv("TESTCLAUDE_TOOL_INPUT"); in != "" && json.Valid([]byte(in)) {
		r.input = json.RawMessage(in)
	}

	servers := []map[string]string{}
	switch {
	case r.mcp != nil:
		servers = append(servers, map[string]string{"name": relayServer, "status": "connected"})
	case r.opts.mcpConfig != "":
		fmt.Fprintln(os.Stderr, "testclaude:", r.mcpErr)
		servers = append(servers, map[string]string{"name": relayServer, "status": "failed"})
	}
	emit(map[string]any{
		"type": "system", "subtype": "init",
		"session_id": r.opts.sessionID, "model": "testclaude", "cwd": r.cwd,
		"permissionMode": r.opts.permissionMode,
		"tools":          []string{r.tool}, "mcp_servers": servers,
	})
}

func (r *runner) handleTurn() {
	if !r.started {
		r.started = true
		r.init()
	}
	r.turn++
	toolUseID := fmt.Sprintf("toolu_testclaude_%d", r.turn)

	emit(map[string]any{
		"type": "assistant", "session_id": r.opts.sessionID,
		"message": map[string]any{
			"id": fmt.Sprintf("msg_testclaude_%d", r.turn), "role": "assistant",
			"content": []map[string]any{{"type": "tool_use", "id": toolUseID, "name": r.tool, "input": r.input}},
		},
	})

	payload, _ := json.Marshal(map[string]any{
		"session_id":      r.opts.sessionID,
		"hook_event_name": "PreToolUse",
		"tool_name":       r.tool,
		"tool_input":      r.input,
		"tool_use_id":     toolUseID,
		"cwd":             r.cwd,
		"permission_mode": r.opts.permissionMode,
	})
	outcome := decide(preToolUseHooks(r.tool), payload)

	rec := record{Tool: r.tool, HookStdout: outcome.stdout, HookExit: outcome.exit,
		Decision: outcome.decision, Reason: outcome.reason}
	var text string
	var isError bool
	if outcome.decision == "allow" {
		rec.Ran = true
		text, isError = callTool(r.mcp, r.tool, r.input)
		rec.Result = text
	} else {
		text, isError = refusal(r.tool, outcome), true
	}
	appendRecord(rec)

	emit(map[string]any{
		"type": "user", "session_id": r.opts.sessionID,
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{{
				"type": "tool_result", "tool_use_id": toolUseID,
				"content": []textBlock{{Type: "text", Text: text}}, "is_error": isError,
			}},
		},
	})
	emit(map[string]any{
		"type": "result", "subtype": "success", "is_error": false,
		"session_id": r.opts.sessionID, "result": text, "num_turns": 1,
		"total_cost_usd": 0,
		"usage":          map[string]int{"input_tokens": 1, "output_tokens": 1},
	})
}

func main() {
	cwd, _ := os.Getwd()
	r := &runner{opts: parseArgs(os.Args[1:]), cwd: cwd}
	defer func() {
		if r.mcp != nil {
			_ = r.mcp.Close()
		}
	}()

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 10<<20)
	for in.Scan() {
		var msg struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(in.Bytes(), &msg) == nil && msg.Type == "user" {
			r.handleTurn()
		}
	}
}
