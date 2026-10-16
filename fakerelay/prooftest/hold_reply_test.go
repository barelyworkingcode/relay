package prooftest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func holdWorld(dir string) any {
	return J{"schema": 1, "credentials": creds(),
		"projects": []J{{"id": "p_main", "name": "Acme", "mode": "work", "allowed_models": []string{"*"}}},
		"models": []J{
			{"value": "pi/hold", "label": "Hold", "group": "Pi", "provider": "pi", "reply": J{"kind": "hold"}},
			{"value": "pi/echo", "label": "Echo", "group": "Pi", "provider": "pi", "reply": J{"kind": "echo"}},
			{"value": "pi/perm", "label": "Perm", "group": "Pi", "provider": "pi", "reply": J{"kind": "permission", "tool": "Bash"}},
		}}
}

// attentionOf reads a session's attention from the session list.
func attentionOf(t *testing.T, in *instance, sid string) J {
	t.Helper()
	for _, s := range in.api(t, "GET", "/api/sessions", nil).is(t, 200).obj(t)["sessions"].([]any) {
		if s.(J)["id"] == sid {
			att, _ := s.(J)["attention"].(J)
			return att
		}
	}
	t.Fatalf("session %s missing from the list", sid)
	return nil
}

// sessionTypes lists the frame types of one session, in order.
func sessionTypes(fr []J, sid string) []string {
	var types []string
	for _, f := range fr {
		if f["sessionId"] == sid && f["type"] != "session_renamed" {
			types = append(types, f["type"].(string))
		}
	}
	return types
}

// isTurnStart is true for the llm_events that start a turn: system init and the
// assistant message start. A block, a delta or a result means the turn went on.
func isTurnStart(f J) bool {
	ev, _ := f["event"].(J)
	if ev["type"] == "system" && ev["subtype"] == "init" {
		return true
	}
	_, msg := ev["message"]
	_, idx := ev["index"]
	return ev["type"] == "assistant" && msg && !idx
}

// Criteria: a turn on a hold model stays running until it is stopped, raises no
// permission_request, and stop_generation ends it the way it ends a turn held on
// a permission.
func TestHoldTurnStaysRunningUntilStopped(t *testing.T) {
	t.Parallel()
	in := startInstance(t, holdWorld)
	newSession := func(model string) string {
		return in.api(t, "POST", "/api/sessions", J{"projectId": "p_main", "model": model, "name": "s"}).is(t, 201).obj(t)["sessionId"].(string)
	}
	sid, echo, perm := newSession("pi/hold"), newSession("pi/echo"), newSession("pi/perm")

	c := in.ws(t, "/ws", opsTok)
	must(t, c.WriteJSON(J{"type": "join_session", "sessionId": sid}), "join")
	until(t, c, isType("session_joined", "sessionId", sid))
	must(t, c.WriteJSON(J{"type": "send_message", "sessionId": sid, "text": "go"}), "send_message")
	until(t, c, isType("session_state", "sessionId", sid, "state", "running"))

	// The barrier: a whole echo turn, started after the held turn was running,
	// runs to its message_complete. A turn that ended on its own would have sent
	// its llm_events, turn_done, idle and message_complete by then.
	e := in.ws(t, "/ws", opsTok)
	must(t, e.WriteJSON(J{"type": "join_session", "sessionId": echo}), "join echo")
	until(t, e, isType("session_joined", "sessionId", echo))
	must(t, e.WriteJSON(J{"type": "send_message", "sessionId": echo, "text": "barrier"}), "send echo")
	until(t, e, isType("message_complete", "sessionId", echo))

	att := attentionOf(t, in, sid)
	if att == nil || att["state"] != "running" {
		t.Errorf("a held turn's attention after a full echo turn: %v, want running", att)
	}

	// The rename's echo closes what c has read of the held session so far.
	must(t, c.WriteJSON(J{"type": "rename_session", "sessionId": sid, "name": "held"}), "rename")
	for _, f := range until(t, c, isType("session_renamed", "sessionId", sid)) {
		if f["sessionId"] != sid {
			continue
		}
		switch f["type"] {
		case "permission_request", "message_complete", "turn_done", "stats_update":
			t.Errorf("a held turn sent %v before it was stopped", f)
		case "llm_event":
			if !isTurnStart(f) {
				t.Errorf("a held turn sent %v before it was stopped", f)
			}
		case "session_state":
			if f["state"] != "running" {
				t.Errorf("a held turn left running before it was stopped: %v", f)
			}
		}
	}

	// A turn that ended on its own has spent its message_complete, so the wait
	// after stop_generation below would only run out its bound.
	if t.Failed() {
		t.FailNow()
	}

	// A late viewer is replayed no permission_request either.
	d := in.ws(t, "/ws", opsTok)
	must(t, d.WriteJSON(J{"type": "join_session", "sessionId": sid}), "join")
	must(t, d.WriteJSON(J{"type": "rename_session", "sessionId": sid, "name": "again"}), "rename")
	for _, f := range until(t, d, isType("session_renamed", "sessionId", sid)) {
		if f["type"] == "permission_request" {
			t.Errorf("a late join replayed a permission_request on a hold turn: %v", f)
		}
	}

	// The reference: a turn held on a permission, then stopped.
	p := in.ws(t, "/ws", opsTok)
	must(t, p.WriteJSON(J{"type": "join_session", "sessionId": perm}), "join perm")
	until(t, p, isType("session_joined", "sessionId", perm))
	must(t, p.WriteJSON(J{"type": "send_message", "sessionId": perm, "text": "run it"}), "send perm")
	until(t, p, isType("permission_request", "sessionId", perm))
	must(t, p.WriteJSON(J{"type": "stop_generation", "sessionId": perm}), "stop perm")
	wantTypes := sessionTypes(until(t, p, isType("message_complete", "sessionId", perm)), perm)
	wantAtt := attentionOf(t, in, perm)

	must(t, c.WriteJSON(J{"type": "stop_generation", "sessionId": sid}), "stop_generation")
	gotTypes := sessionTypes(until(t, c, isType("message_complete", "sessionId", sid)), sid)
	eq(t, strings.Join(gotTypes, ","), strings.Join(wantTypes, ","), "frames from stop_generation to message_complete")
	gotAtt := attentionOf(t, in, sid)
	if (gotAtt == nil) != (wantAtt == nil) || (gotAtt != nil && gotAtt["state"] != wantAtt["state"]) {
		t.Errorf("attention after stop: hold %v, permission %v", gotAtt, wantAtt)
	}
}

// Criteria: a world that names hold with tool, text, code or stderr is refused.
func TestHoldRefusesExtraFields(t *testing.T) {
	t.Parallel()
	for name, extra := range map[string]J{
		"tool":   {"tool": "Bash"},
		"text":   {"text": "hi"},
		"code":   {"code": 3},
		"stderr": {"stderr": []string{"boom"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reply := J{"kind": "hold"}
			for k, v := range extra {
				reply[k] = v
			}
			world := J{"schema": 1, "models": []J{{"value": "pi/hold", "label": "Hold", "group": "Pi", "provider": "pi", "reply": reply}}}
			b, err := json.Marshal(world)
			must(t, err, "marshal")
			dir, err := newDir()
			must(t, err, "dir")
			must(t, os.WriteFile(filepath.Join(dir, "world.json"), b, 0o600), "write world")
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
