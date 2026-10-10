package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const codexModels = `{"models":[{"slug":"fake-echo","display_name":"Fake Echo","visibility":"list"}]}` + "\n"

// runCodex speaks the app-server wire: newline-delimited JSON-RPC with no
// "jsonrpc" field. Requests get a reply carrying their id; a turn also
// produces the notifications relay turns into events.
func (a *agent) runCodex() int {
	if len(a.args) >= 2 && a.args[0] == "debug" && a.args[1] == "models" {
		fmt.Fprint(os.Stdout, codexModels)
		return 0
	}
	if len(a.args) == 0 || a.args[0] != "app-server" {
		fmt.Fprintln(os.Stderr, "fakeagent codex: only app-server and debug models are supported")
		return 2
	}

	threadID := ""
	turnN := 0
	a.stdinLines(func(line []byte) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			a.logInput(line, nil)
			return
		}
		reply := func(result any) {
			if len(req.ID) == 0 {
				return
			}
			b, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
			a.emit(b)
		}
		switch req.Method {
		case "initialize":
			a.logInput(line, nil)
			reply(map[string]any{"userAgent": "fakeagent/1.0"})
		case "thread/start", "thread/resume":
			a.logInput(line, nil)
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(req.Params, &p)
			threadID = p.ThreadID
			if threadID == "" {
				threadID = randomID("thr_")
			}
			reply(map[string]any{"thread": map[string]any{"id": threadID}})
		case "turn/start":
			var p struct {
				ThreadID string `json:"threadId"`
				Input    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"input"`
			}
			_ = json.Unmarshal(req.Params, &p)
			var texts []string
			for _, in := range p.Input {
				if in.Type == "text" {
					texts = append(texts, in.Text)
				}
			}
			text := strings.Join(texts, "\n")
			a.logInput(line, &text)
			if p.ThreadID != "" {
				threadID = p.ThreadID
			}
			turnN++
			turnID := fmt.Sprintf("turn-%d", turnN)
			itemID := fmt.Sprintf("item-%d", turnN)
			answer := "echo: " + text
			turn := func(status string) map[string]any {
				return map[string]any{"id": turnID, "status": status, "items": []any{}}
			}
			reply(map[string]any{"turn": turn("inProgress")})
			a.notify("turn/started", map[string]any{"threadId": threadID, "turn": turn("inProgress")})
			a.notify("item/agentMessage/delta", map[string]any{"threadId": threadID, "turnId": turnID, "itemId": itemID, "delta": answer})
			a.notify("item/completed", map[string]any{"threadId": threadID, "turnId": turnID, "item": map[string]any{"type": "agentMessage", "id": itemID, "text": answer}})
			a.notify("turn/completed", map[string]any{"threadId": threadID, "turn": turn("completed")})
		case "turn/interrupt":
			a.logInput(line, nil)
			reply(map[string]any{})
		default:
			a.logInput(line, nil)
			if len(req.ID) != 0 {
				b, _ := json.Marshal(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
				a.emit(b)
			}
		}
	})
	return 0
}

func (a *agent) notify(method string, params any) {
	b, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return
	}
	a.emit(b)
}
