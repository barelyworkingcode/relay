package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"relaye2e/harness"
)

const terminalFrameDeadline = 30 * time.Second

var catTemplate = json.RawMessage(`{"id":"cat","name":"Cat","command":"/bin/cat","sandbox":false}`)

func terminalsWorld() Spec {
	return Spec{
		Credentials:      credsAllClasses,
		ConsoleTemplates: []json.RawMessage{catTemplate},
		Projects: []Project{{
			ID: "p_acme", Name: "Acme", Mode: "work",
			Files: map[string]File{"README.md": {Text: "# Acme\n"}},
		}},
	}
}

// hostTerminalsWorld has a probed host whose only template runs /bin/cat
// through the ssh stub.
func hostTerminalsWorld() Spec {
	return Spec{
		Credentials: credsAllClasses,
		Hosts: []Host{{
			ID: "h_box0001", Name: "testbox", Target: "acme@testbox", Probed: true,
			Templates: []json.RawMessage{json.RawMessage(`{"id":"cat","name":"Cat","command":"/bin/cat"}`)},
		}},
		Projects: []Project{{
			ID: "p_box0001", Name: "Box app", Mode: "work", HostID: "h_box0001",
			Path:  "{remote}/app",
			Files: map[string]File{"main.go": {Text: "package main\n"}},
		}},
	}
}

func TestTerminalsTemplateList(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Terminals,
		Spec:    terminalsWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/terminal/templates?project=p_acme", nil)
			r.HTTP("ops", "GET", "/api/terminal/templates", nil)
			r.HTTP("ops", "POST", "/api/terminal/templates", json.RawMessage(`{"id":"extra","name":"Extra","command":"/bin/echo","sandbox":false}`))
			r.HTTP("ops", "GET", "/api/terminal/templates?project=p_acme", nil)
			r.HTTP("ops", "DELETE", "/api/terminal/templates/extra", nil)
			r.HTTP("ops", "GET", "/api/terminal/templates?project=p_acme", nil)
		},
	})
}

func TestTerminalsTemplateCRUD(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Terminals,
		Spec:    terminalsWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "POST", "/api/terminal/templates", json.RawMessage(`{"id":"extra","name":"Extra","command":"/bin/echo","args":["acme"],"sandbox":false}`))
			r.HTTP("ops", "POST", "/api/terminal/templates", json.RawMessage(`{"id":"extra","name":"Extra","command":"/bin/echo","sandbox":false}`))
			r.HTTP("ops", "POST", "/api/terminal/templates", json.RawMessage(`{"id":"bad id","name":"Bad","command":"/bin/echo","sandbox":false}`))
			r.HTTP("ops", "GET", "/api/terminal/templates/extra", nil)
			r.HTTP("ops", "PUT", "/api/terminal/templates/extra", json.RawMessage(`{"name":"Extra two","command":"/bin/echo","args":["testbox"],"sandbox":false}`))
			r.HTTP("ops", "DELETE", "/api/terminal/templates/extra", nil)
			r.HTTP("ops", "GET", "/api/terminal/templates/extra", nil)
			r.HTTP("ops", "PUT", "/api/terminal/templates/extra", json.RawMessage(`{"name":"Gone","command":"/bin/echo","sandbox":false}`))
		},
	})
}

// catSession drives one /bin/cat terminal: create, list, join, input echoed,
// close. Output is read on a raw connection and recorded as a derived value,
// because the two targets echo by different means.
func catSession(r *Run, projectID string) {
	r.T.Helper()
	resp := r.HTTP("ops", "POST", "/api/terminals", map[string]any{
		"templateId": "cat", "name": "Cat", "projectId": projectID, "cols": 80, "rows": 24,
	})
	var made struct {
		TerminalID string `json:"terminalId"`
	}
	resp.JSON(r.T, &made)
	if made.TerminalID == "" {
		r.T.Fatalf("create terminal answered %d with no terminalId", resp.Status)
	}
	r.HTTP("ops", "GET", "/api/terminals", nil)

	i := r.Target.I
	ws := i.WebSocket("/ws", i.Credential("ops"))
	defer ws.Close()
	ws.Send(map[string]any{"type": "join_terminal", "terminalId": made.TerminalID})
	joined := nextTerminalFrame(r, ws, "terminal_joined")
	r.Note("terminal_joined", map[string]any{
		"templateId": joined["templateId"], "name": joined["name"], "state": joined["state"], "host": joined["host"],
	})

	line := "hello acme"
	ws.Send(map[string]any{
		"type": "terminal_input", "terminalId": made.TerminalID,
		"data": base64.StdEncoding.EncodeToString([]byte(line + "\r")),
	})
	var seen []byte
	for !bytes.Contains(seen, []byte(line)) {
		f := nextTerminalFrame(r, ws, "terminal_output")
		data, err := base64.StdEncoding.DecodeString(frameField(f, "data"))
		if err != nil {
			r.T.Fatalf("terminal_output data is not base64: %v", err)
		}
		seen = append(seen, data...)
	}
	r.Note("terminal_output", map[string]any{"echoed": true})

	ws.Send(map[string]any{"type": "terminal_close", "terminalId": made.TerminalID})
	nextTerminalFrame(r, ws, "terminal_closed")
	r.Note("terminal_closed", map[string]any{"closed": true})
	r.HTTP("ops", "GET", "/api/terminals", nil)
}

// nextTerminalFrame reads frames until one of the wanted type arrives.
func nextTerminalFrame(r *Run, ws *harness.WSConn, want string) map[string]any {
	r.T.Helper()
	for {
		var f map[string]any
		if err := json.Unmarshal(ws.Next(terminalFrameDeadline), &f); err != nil {
			r.T.Fatalf("frame is not JSON: %v", err)
		}
		if frameField(f, "type") == want {
			return f
		}
	}
}

func TestTerminalsConsoleCat(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Terminals,
		Spec:    terminalsWorld(),
		Body:    func(r *Run) { catSession(r, "p_acme") },
	})
}

func TestTerminalsHostCat(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Terminals,
		Spec:    hostTerminalsWorld(),
		Body:    func(r *Run) { catSession(r, "p_box0001") },
	})
}
