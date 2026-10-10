package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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
		// "!error <code>" ends the turn as the CLI ends one on an API error:
		// a top-level error on the assistant line and an error result.
		errCode, isErr := strings.CutPrefix(text, "!error ")
		if !isErr {
			errCode = ""
		} else {
			answer = "API Error: " + errCode
		}
		msgID := randomID("msg_")
		content := []map[string]any{{"type": "text", "text": answer}}
		frame := map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"id": msgID, "role": "assistant", "content": content,
			},
		}
		if isErr {
			frame["error"] = errCode
			frame["isApiErrorMessage"] = true
		}
		a.emitJSON(frame)
		writeTranscript(sessionID, cwd, text, msgID, content, errCode)
		subtype := "success"
		if isErr {
			subtype = "error_during_execution"
		}
		a.emitJSON(map[string]any{
			"type": "result", "subtype": subtype, "is_error": isErr,
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

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// claudeDirName is Claude CLI's folder name for a working directory: every
// character other than A-Z, a-z and 0-9 becomes "-".
func claudeDirName(cwd string) string {
	return nonAlnum.ReplaceAllString(cwd, "-")
}

// writeTranscript appends the turn's user and assistant lines to
// $HOME/.claude/projects/<dir name>/<conversation id>.jsonl, as the real CLI
// does before it reports the result. The directory name encodes the working
// directory with symlinks resolved, as the CLI sees it. A write that fails is
// ignored: the transcript is a convenience for history tests, and a test that
// needs it fails on its absence.
func writeTranscript(sessionID, cwd, userText, assistantID string, content []map[string]any, errCode string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	dir := filepath.Join(home, ".claude", "projects", claudeDirName(cwd))
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, sessionID+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	lines := []map[string]any{
		{"type": "user", "sessionId": sessionID, "timestamp": ts,
			"message": map[string]any{"id": randomID("usr_"), "role": "user", "content": userText}},
		{"type": "assistant", "sessionId": sessionID, "timestamp": ts,
			"message": map[string]any{"id": assistantID, "role": "assistant", "content": content}},
	}
	if errCode != "" {
		lines[1]["error"] = errCode
		lines[1]["isApiErrorMessage"] = true
	}
	for _, l := range lines {
		if b, err := json.Marshal(l); err == nil {
			_, _ = f.Write(append(b, '\n'))
		}
	}
}
