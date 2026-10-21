package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// piModelTable is `pi --list-models`: a fixed-width table whose header sets
// the column offsets relay slices each row by.
const piModelTable = "provider  model       context\nfake      fake-echo   128K\n"

// runPi speaks pi's RPC mode: commands in, one response per command that has
// an id, and agent events for each prompt.
func (a *agent) runPi() int {
	if hasArg(a.args, "--list-models") {
		fmt.Fprint(os.Stdout, piModelTable)
		return 0
	}
	if flagValue(a.args, "--mode") != "rpc" {
		fmt.Fprintln(os.Stderr, "fakeagent pi: only --mode rpc and --list-models are supported")
		return 2
	}
	sessionID := flagValue(a.args, "--session")
	sessionFile := ""
	leaf := ""
	if filepath.IsAbs(sessionID) {
		sessionFile = sessionID
		if _, err := os.Stat(sessionFile + ".fail-start"); err == nil {
			a.record(agentLine{Event: "pi_start_failed", Text: sessionFile})
			return 1
		}
		f, err := os.Open(sessionFile)
		if err != nil {
			return 1
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var entry struct{ Type, ID string }
			if json.Unmarshal(sc.Bytes(), &entry) == nil {
				if entry.Type == "session" {
					sessionID = entry.ID
				} else {
					leaf = entry.ID
				}
			}
		}
		_ = f.Close()
		if sc.Err() != nil {
			return 1
		}
	}
	if sessionID == "" {
		sessionID = randomID("pi-")
	}

	holding := false
	a.stdinLines(func(line []byte) {
		var cmd struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(line, &cmd) != nil {
			a.logInput(line, nil)
			return
		}
		if cmd.Type != "prompt" {
			a.logInput(line, nil)
			if cmd.Type == "abort" {
				a.record(agentLine{Event: "pi_abort", Text: sessionID})
				if holding {
					holding = false
					a.emitJSON(map[string]any{"type": "agent_end", "messages": []any{}, "willRetry": false})
					a.emitJSON(map[string]any{"type": "agent_settled"})
				}
			}
			if cmd.ID != "" {
				resp := map[string]any{"type": "response", "id": cmd.ID, "command": cmd.Type, "success": true}
				if cmd.Type == "get_state" {
					resp["data"] = map[string]any{"sessionId": sessionID}
					if sessionFile != "" {
						resp["data"].(map[string]any)["sessionFile"] = sessionFile
					}
				}
				a.emitJSON(resp)
			}
			return
		}
		a.logInput(line, &cmd.Message)
		reply := "echo: " + cmd.Message
		if cmd.ID != "" {
			a.emitJSON(map[string]any{"type": "response", "id": cmd.ID, "command": "prompt", "success": true})
		}
		a.emitJSON(map[string]any{"type": "agent_start"})
		a.emitJSON(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_start", "contentIndex": 0}})
		a.emitJSON(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": reply}})
		if cmd.Message == "!hold" {
			holding = true
			a.record(agentLine{Event: "pi_holding", Text: sessionID})
			return
		}
		if sessionFile != "" {
			f, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				panic(err)
			}
			enc := json.NewEncoder(f)
			for _, msg := range []map[string]any{
				{"role": "user", "content": cmd.Message, "timestamp": time.Now().UnixMilli()},
				{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": reply}}, "stopReason": "stop", "timestamp": time.Now().UnixMilli()},
			} {
				id := randomID("")
				if err := enc.Encode(map[string]any{"type": "message", "id": id, "parentId": leaf, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "message": msg}); err != nil {
					panic(err)
				}
				leaf = id
			}
			if err := f.Close(); err != nil {
				panic(err)
			}
		}
		a.emitJSON(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_end", "contentIndex": 0, "content": reply}})
		a.emitJSON(map[string]any{"type": "agent_end", "messages": []any{}, "willRetry": false})
		a.emitJSON(map[string]any{"type": "agent_settled"})
	})
	return 0
}

func hasArg(args []string, name string) bool {
	for _, x := range args {
		if x == name {
			return true
		}
	}
	return false
}
