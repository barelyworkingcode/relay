package prooftest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var exitStderr = []string{"acme-provider: cannot start", "acme-provider: exiting"}

func exitWorld(dir string) any {
	return J{"schema": 1, "credentials": creds(),
		"projects": []J{{"id": "p_main", "name": "Acme", "mode": "work", "allowed_models": []string{"*"}}},
		"models": []J{{"value": "pi/exit", "label": "Exit", "group": "Pi", "provider": "pi",
			"reply": J{"kind": "exit", "code": 127, "stderr": exitStderr}}}}
}

// Criteria: an exit turn ends errored with exit, process_exited carries the same exit,
// attention.exit on the list matches, a second send gets resume_required.
func TestExitReply(t *testing.T) {
	t.Parallel()
	in := startInstance(t, exitWorld)
	w := in.ws(t, "/ws", opsTok)
	sid := in.api(t, "POST", "/api/sessions", J{"projectId": "p_main", "model": "pi/exit", "name": "s"}).is(t, 201).obj(t)["sessionId"].(string)
	must(t, w.WriteJSON(J{"type": "join_session", "sessionId": sid}), "join")
	until(t, w, isType("session_joined", "sessionId", sid))

	must(t, w.WriteJSON(J{"type": "send_message", "sessionId": sid, "text": "go", "trace_id": "trace-exit-0001"}), "send_message")
	fr := until(t, w, isType("process_exited", "sessionId", sid))
	idx := func(typ, state string) int {
		for k, f := range fr {
			if f["type"] == typ && (state == "" || f["state"] == state) {
				return k
			}
		}
		return -1
	}
	user, running, errored, exited := idx("user_message", ""), idx("session_state", "running"), idx("session_state", "errored"), len(fr)-1
	if user < 0 || running < 0 || errored < 0 || !(user < running && running < errored && errored < exited) {
		t.Fatalf("frames out of order: %v", fr)
	}
	for _, typ := range []string{"turn_done", "message_complete", "llm_event"} {
		if k := idx(typ, ""); k >= 0 {
			t.Errorf("%s frame before process_exited: %v", typ, fr)
		}
	}
	want := J{"code": float64(127), "stderr": []any{exitStderr[0], exitStderr[1]}}
	eq(t, fr[errored]["exit"], any(want), "errored frame exit")
	eq(t, fr[exited]["exit"], any(want), "process_exited exit")
	if ev := in.events(t, "trace-exit-0001", "chat.turn"); len(ev) != 1 || ev[0]["session_id"] != sid {
		t.Errorf("chat.turn events by trace: %v", ev)
	}

	var row J
	for _, s := range in.api(t, "GET", "/api/sessions", nil).is(t, 200).obj(t)["sessions"].([]any) {
		if s.(J)["id"] == sid {
			row = s.(J)
		}
	}
	if row == nil {
		t.Fatal("exited session missing from the list")
	}
	att, _ := row["attention"].(J)
	eq(t, att["state"], any("errored"), "attention state")
	eq(t, att["since"], fr[errored]["since"], "attention since")
	eq(t, att["exit"], any(want), "attention exit")

	must(t, w.WriteJSON(J{"type": "send_message", "sessionId": sid, "text": "again"}), "second send")
	fr = until(t, w, isType("error", "code", "resume_required"))
	for _, f := range fr {
		if f["type"] == "turn_done" || f["type"] == "message_complete" || f["type"] == "user_message" {
			t.Errorf("second send started a turn: %v", fr)
		}
	}
}

// Criteria: the world refuses code 0 and refuses code on a fail reply.
func TestExitWorldValidation(t *testing.T) {
	t.Parallel()
	for name, reply := range map[string]string{
		"exit with code 0":    `{"kind":"exit","code":0}`,
		"fail with a code":    `{"kind":"fail","code":1}`,
		"fail with a stderr":  `{"kind":"fail","stderr":["acme"]}`,
		"exit with 256":       `{"kind":"exit","code":256}`,
		"exit without a code": `{"kind":"exit"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, err := newDir()
			must(t, err, "dir")
			world := `{"schema":1,"models":[{"value":"pi/x","label":"X","group":"Pi","provider":"pi","reply":` + reply + `}]}`
			must(t, os.WriteFile(filepath.Join(dir, "world.json"), []byte(world), 0o600), "write world")
			out, se, code := serveUntilReadyOrExit(t, dir)
			if code != 1 || out != "" || !strings.Contains(se, "world.json") {
				t.Errorf("exit %d stdout %q stderr %q", code, out, se)
			}
			if _, err := os.Stat(filepath.Join(dir, "ready.json")); err == nil {
				t.Error("ready.json written for an invalid world")
			}
		})
	}
}
