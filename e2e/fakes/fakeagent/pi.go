package main

import (
	"encoding/json"
	"fmt"
	"os"
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
	if sessionID == "" {
		sessionID = randomID("pi-")
	}

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
			if cmd.ID != "" {
				resp := map[string]any{"type": "response", "id": cmd.ID, "command": cmd.Type, "success": true}
				if cmd.Type == "get_state" {
					resp["data"] = map[string]any{"sessionId": sessionID}
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
		a.emitJSON(map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_end", "contentIndex": 0, "content": reply}})
		a.emitJSON(map[string]any{"type": "agent_end", "messages": []any{}})
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
