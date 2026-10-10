package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runClaude speaks claude's stream-json wire: user lines in, then a system
// init (once, on the first user line, as the real CLI does), an assistant
// snapshot and a result per turn.
func (a *agent) runClaude() int {
	sessionID := flagValue(a.args, "--resume")
	if sessionID == "" {
		sessionID = flagValue(a.args, "--session-id")
	}
	if sessionID == "" {
		sessionID = randomID("sess-")
	}
	model := flagValue(a.args, "--model")
	cwd, _ := os.Getwd()
	mcpServers := mcpServerEntries(flagValue(a.args, "--mcp-config"))

	inited := false
	turn := 0
	a.stdinLines(func(line []byte) {
		var msg struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.Type != "user" {
			a.logInput(line, nil)
			return
		}
		text := claudeUserText(msg.Message.Content)
		a.logInput(line, &text)
		if !inited {
			inited = true
			a.emitJSON(map[string]any{
				"type": "system", "subtype": "init",
				"session_id": sessionID, "model": model, "cwd": cwd,
				"tools": []string{}, "mcp_servers": mcpServers,
			})
		}
		turn++
		answer := "echo: " + text
		if cmdline, ok := strings.CutPrefix(text, "!sh "); ok {
			answer = runShellTurn(cmdline)
		}
		a.emitJSON(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id": randomID("msg_"), "role": "assistant",
				"content": []map[string]any{{"type": "text", "text": answer}},
			},
		})
		a.emitJSON(map[string]any{
			"type": "result", "subtype": "success", "is_error": false,
			"session_id": sessionID, "num_turns": turn, "result": answer,
			"total_cost_usd": 0,
			"usage":          map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	})
	return 0
}

func (a *agent) emitJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	a.emit(b)
}

// claudeUserText joins the text blocks of a user message; content may also be
// a bare string.
func claudeUserText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// mcpServerEntries reports each server named in an --mcp-config file as
// connected, so relay does not warn that its tools failed to load. A missing
// or unreadable file reports none.
func mcpServerEntries(path string) []map[string]string {
	out := []map[string]string{}
	if path == "" {
		return out
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return out
	}
	for name := range cfg.MCPServers {
		out = append(out, map[string]string{"name": name, "status": "connected"})
	}
	return out
}

// runShellTurn runs cmdline through /bin/sh -c in the session's working
// directory and answers "exit: N". A command that cannot start answers
// "exit: -1".
func runShellTurn(cmdline string) string {
	cmd := exec.Command("/bin/sh", "-c", cmdline)
	code := 0
	if err := cmd.Run(); err != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	return fmt.Sprintf("exit: %d", code)
}
