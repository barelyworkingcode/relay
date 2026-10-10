package contract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime"
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

const absentTerminalID = "123e4567-e89b-42d3-a456-426614174000"

var shTemplate = json.RawMessage(`{"id":"sh","name":"Shell","command":"/bin/sh","sandbox":false}`)

// startTerminal creates a terminal of template and joins it on a raw
// connection. The caller closes the connection.
func startTerminal(r *Run, template string) (string, *harness.WSConn) {
	r.T.Helper()
	resp := r.HTTP("ops", "POST", "/api/terminals", map[string]any{
		"templateId": template, "name": "Term", "projectId": "p_acme", "cols": 80, "rows": 24,
	})
	var made struct {
		TerminalID string `json:"terminalId"`
	}
	resp.JSON(r.T, &made)
	if made.TerminalID == "" {
		r.T.Fatalf("create terminal answered %d with no terminalId", resp.Status)
	}
	i := r.Target.I
	ws := i.WebSocket("/ws", i.Credential("ops"))
	ws.Send(map[string]any{"type": "join_terminal", "terminalId": made.TerminalID})
	nextTerminalFrame(r, ws, "terminal_joined")
	return made.TerminalID, ws
}

func sendTerminalInput(ws *harness.WSConn, id, text string) {
	ws.Send(map[string]any{
		"type": "terminal_input", "terminalId": id,
		"data": base64.StdEncoding.EncodeToString([]byte(text)),
	})
}

// readLog reads the terminal's log unrecorded and notes its shape.
func readLog(r *Run, id, label string) {
	r.T.Helper()
	i := r.Target.I
	resp := i.SocketHTTP(i.Credential("ops")).Do("GET", "/api/terminals/"+id+"/log", nil)
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	r.Note(label, map[string]any{
		"status": resp.Status, "type": mt, "has_input": bytes.Contains(resp.Body, []byte("hello acme")),
	})
}

func TestTerminalsLog(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Terminals,
		Spec:    terminalsWorld(),
		Body: func(r *Run) {
			id, ws := startTerminal(r, "cat")
			defer ws.Close()
			sendTerminalInput(ws, id, "hello acme\r")
			var seen []byte
			for !bytes.Contains(seen, []byte("hello acme")) {
				f := nextTerminalFrame(r, ws, "terminal_output")
				data, err := base64.StdEncoding.DecodeString(frameField(f, "data"))
				if err != nil {
					r.T.Fatalf("terminal_output data is not base64: %v", err)
				}
				seen = append(seen, data...)
			}
			readLog(r, id, "log")

			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "GET", "/api/terminals/"+absentTerminalID+"/log", nil, harness.ReqOpts{Trace: tr})
			r.HTTP("ops", "GET", "/api/terminals/not-a-terminal/log", nil)
			r.Event(harness.EventQuery{Key: "terminal.log", Trace: tr})

			r.HTTP("ops", "DELETE", "/api/terminals/"+id, nil)
			readLog(r, id, "log after delete")
		},
	})
}

func TestTerminalsEOFExit(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Terminals,
		Spec:    terminalsWorld(),
		Body: func(r *Run) {
			id, ws := startTerminal(r, "cat")
			defer ws.Close()
			sendTerminalInput(ws, id, "\x04")
			exit := nextTerminalFrame(r, ws, "terminal_exit")
			r.Note("terminal_exit", map[string]any{"exitCode": exit["exitCode"]})
			r.Target.I.WaitEvent(harness.EventQuery{Key: "session.exited", Fields: map[string]any{"session_id": id}}, frameWait)
			r.Note("session.exited", map[string]any{"session_exited": true})
			r.HTTP("ops", "GET", "/api/terminals", nil)

			i := r.Target.I
			again := i.WebSocket("/ws", i.Credential("ops"))
			defer again.Close()
			again.Send(map[string]any{"type": "join_terminal", "terminalId": id})
			joined := nextTerminalFrame(r, again, "terminal_joined")
			late := nextTerminalFrame(r, again, "terminal_exit")
			r.Note("rejoin", map[string]any{"state": joined["state"], "exitCode": late["exitCode"]})
		},
	})
}

func TestTerminalsShellExit(t *testing.T) {
	t.Parallel()
	spec := terminalsWorld()
	spec.ConsoleTemplates = []json.RawMessage{shTemplate}
	Check(t, Scenario{
		Surface: Terminals,
		Spec:    spec,
		Body: func(r *Run) {
			id, ws := startTerminal(r, "sh")
			defer ws.Close()
			sendTerminalInput(ws, id, "exit 3\r")
			exit := nextTerminalFrame(r, ws, "terminal_exit")
			r.Note("terminal_exit", map[string]any{"exitCode": exit["exitCode"]})
			r.HTTP("ops", "GET", "/api/terminals", nil)
		},
	})
}
