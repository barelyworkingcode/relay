package prooftest

import (
	"testing"

	"github.com/gorilla/websocket"
)

// permWindowMs is the 60 s a request waits, in clock milliseconds.
const permWindowMs = int64(60_000)

// permMarginMs is how far short of the window the first clock step stops.
const permMarginMs = int64(1_000)

// Relay's expiry reason with a connection joined, as its docs state it.
const reasonExpired = "No approval: the request was not answered within 60 seconds."

// denyContent returns the content of the tool_result the session's turn got for
// its denied permission request.
func denyContent(t *testing.T, fr []J, sid string) string {
	t.Helper()
	for _, f := range fr {
		ev, _ := f["event"].(J)
		if f["type"] == "llm_event" && f["sessionId"] == sid && ev["subtype"] == "tool_result" {
			s, _ := ev["content"].(string)
			return s
		}
	}
	t.Fatalf("no tool_result in %v", fr)
	return ""
}

// raisePermission starts a turn that waits on a permission, as a joined viewer,
// and returns the viewer, the session id and the permission_request frame.
func raisePermission(t *testing.T, in *instance) (*websocket.Conn, string, J) {
	t.Helper()
	sid := in.api(t, "POST", "/api/sessions", J{"projectId": "p_main", "model": "pi/perm", "name": "s"}).is(t, 201).obj(t)["sessionId"].(string)
	c := in.ws(t, "/ws", opsTok)
	must(t, c.WriteJSON(J{"type": "join_session", "sessionId": sid}), "join")
	until(t, c, isType("session_joined", "sessionId", sid))
	must(t, c.WriteJSON(J{"type": "send_message", "sessionId": sid, "text": "run it"}), "send_message")
	fr := until(t, c, isType("permission_request", "sessionId", sid))
	return c, sid, fr[len(fr)-1]
}

// barrier renames the session and reads to the echo; the frames it returns are
// everything the connection was sent before the rename took effect.
func barrier(t *testing.T, c *websocket.Conn, sid, name string) []J {
	t.Helper()
	must(t, c.WriteJSON(J{"type": "rename_session", "sessionId": sid, "name": name}), "rename")
	return until(t, c, isType("session_renamed", "sessionId", sid))
}

func advanceClock(t *testing.T, in *instance, ms int64) {
	t.Helper()
	in.ctl(t, "POST", "/v1/clock", J{"advance_ms": ms}).is(t, 200)
}

func hasType(fr []J, typ string) bool {
	for _, f := range fr {
		if f["type"] == typ {
			return true
		}
	}
	return false
}

// Criteria: fakerelay expires a request when its clock moves to 60 s past the
// request, and sends the viewer permission_resolved `expired` with the request's
// permissionId; a request one millisecond short does not expire; an answer that
// arrives after the expiry gets no reply.
func TestPermissionExpiresOnClock(t *testing.T) {
	t.Parallel()
	in := startInstance(t, replayWorld)
	c, sid, req := raisePermission(t, in)

	// The instance clock is real time plus an offset, so the margin must outlast
	// the real time a loaded run spends between the two steps.
	advanceClock(t, in, permWindowMs-permMarginMs)
	if fr := barrier(t, c, sid, "short"); hasType(fr, "permission_resolved") {
		t.Errorf("a request expired before its 60 s: %v", fr)
	}

	advanceClock(t, in, permMarginMs)
	fr := until(t, c, isType("permission_resolved", "sessionId", sid))
	got := fr[len(fr)-1]
	eq(t, got["permissionId"], req["permissionId"], "permissionId")
	eq(t, got["outcome"], any("expired"), "outcome")
	rest := until(t, c, isType("message_complete", "sessionId", sid))
	eq(t, denyContent(t, append(fr, rest...), sid), reasonExpired, "deny reason with a viewer joined")

	must(t, c.WriteJSON(J{"type": "permission_response", "permissionId": req["permissionId"], "approved": true}), "late answer")
	for _, f := range barrier(t, c, sid, "late") {
		if f["type"] == "permission_resolved" || f["type"] == "error" {
			t.Errorf("a late answer got a reply: %v", f)
		}
	}
}

// Criteria: fakerelay does not replay an expired request on join.
func TestNoReplayAfterExpiry(t *testing.T) {
	t.Parallel()
	in := startInstance(t, replayWorld)
	a, sid, _ := raisePermission(t, in)
	advanceClock(t, in, permWindowMs)
	until(t, a, isType("message_complete", "sessionId", sid))

	b := in.ws(t, "/ws", opsTok)
	must(t, b.WriteJSON(J{"type": "join_session", "sessionId": sid}), "join")
	until(t, b, isType("session_joined", "sessionId", sid))
	if fr := barrier(t, b, sid, "after"); hasType(fr, "permission_request") {
		t.Errorf("an expired request was replayed: %v", fr)
	}
}

// Criteria: a viewer gets permission_resolved `answered` after a joined connection
// answers, approve or deny, and `ended` when the session is stopped, ended or
// deleted while the request waits; each names the request's permissionId.
func TestPermissionResolvedOutcomes(t *testing.T) {
	t.Parallel()
	in := startInstance(t, replayWorld)
	cases := []struct {
		name    string
		outcome string
		act     func(c *websocket.Conn, sid string, req J) error
	}{
		{"approve", "answered", func(c *websocket.Conn, _ string, req J) error {
			return c.WriteJSON(J{"type": "permission_response", "permissionId": req["permissionId"], "approved": true})
		}},
		{"deny", "answered", func(c *websocket.Conn, _ string, req J) error {
			return c.WriteJSON(J{"type": "permission_response", "permissionId": req["permissionId"], "approved": false, "reason": "no"})
		}},
		{"stop_generation", "ended", func(c *websocket.Conn, sid string, _ J) error {
			return c.WriteJSON(J{"type": "stop_generation", "sessionId": sid})
		}},
		{"end_session", "ended", func(c *websocket.Conn, sid string, _ J) error {
			return c.WriteJSON(J{"type": "end_session", "sessionId": sid})
		}},
		{"delete_session", "ended", func(c *websocket.Conn, sid string, _ J) error {
			return c.WriteJSON(J{"type": "delete_session", "sessionId": sid})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, sid, req := raisePermission(t, in)
			must(t, tc.act(c, sid, req), tc.name)
			fr := until(t, c, isType("permission_resolved", "sessionId", sid))
			got := fr[len(fr)-1]
			eq(t, got["permissionId"], req["permissionId"], "permissionId")
			eq(t, got["outcome"], any(tc.outcome), "outcome")
		})
	}
}
