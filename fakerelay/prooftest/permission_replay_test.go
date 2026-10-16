package prooftest

import "testing"

func replayWorld(dir string) any {
	return J{"schema": 1, "credentials": creds(),
		"projects": []J{{"id": "p_main", "name": "Acme", "mode": "work", "allowed_models": []string{"*"}}},
		"models": []J{{"value": "pi/perm", "label": "Perm", "group": "Pi", "provider": "pi",
			"reply": J{"kind": "permission", "tool": "Bash"}}}}
}

// Criteria: a connection that joins a session receives, after session_joined, each
// unanswered permission_request of that session and no other session's; an answered
// request is not replayed.
func TestJoinReplaysPendingPermission(t *testing.T) {
	t.Parallel()
	in := startInstance(t, replayWorld)
	newSession := func() string {
		return in.api(t, "POST", "/api/sessions", J{"projectId": "p_main", "model": "pi/perm", "name": "s"}).is(t, 201).obj(t)["sessionId"].(string)
	}
	first, other := newSession(), newSession()

	// Two turns wait on a permission; the viewer that raised them leaves.
	a := in.ws(t, "/ws", opsTok)
	var raised J
	for _, sid := range []string{first, other} {
		must(t, a.WriteJSON(J{"type": "join_session", "sessionId": sid}), "join")
		until(t, a, isType("session_joined", "sessionId", sid))
		must(t, a.WriteJSON(J{"type": "send_message", "sessionId": sid, "text": "run it"}), "send_message")
		fr := until(t, a, isType("permission_request", "sessionId", sid))
		if sid == first {
			raised = fr[len(fr)-1]
		}
		must(t, a.WriteJSON(J{"type": "leave_session", "sessionId": sid}), "leave")
	}

	// A late connection gets the first session's request after session_joined, once,
	// and the other session's not at all. The rename's echo marks the end of the join.
	b := in.ws(t, "/ws", opsTok)
	must(t, b.WriteJSON(J{"type": "join_session", "sessionId": first}), "join")
	must(t, b.WriteJSON(J{"type": "rename_session", "sessionId": first, "name": "late"}), "rename")
	fr := until(t, b, isType("session_renamed", "sessionId", first))
	var types []string
	var replayed []J
	for _, f := range fr {
		types = append(types, f["type"].(string))
		if f["type"] == "permission_request" {
			replayed = append(replayed, f)
		}
	}
	eq(t, types[0], "session_joined", "first frame of a join")
	if len(replayed) != 1 {
		t.Fatalf("want one replayed permission_request, got %d in %v", len(replayed), fr)
	}
	eq(t, replayed[0]["permissionId"], raised["permissionId"], "replayed permissionId")
	eq(t, replayed[0]["sessionId"], any(first), "replayed sessionId")
	eq(t, replayed[0]["toolName"], any("Bash"), "replayed toolName")
	eq(t, replayed[0]["toolUseId"], raised["toolUseId"], "replayed toolUseId")

	// Its answer resolves the held turn; a later join replays nothing.
	must(t, b.WriteJSON(J{"type": "permission_response", "permissionId": replayed[0]["permissionId"], "approved": true}), "permission_response")
	until(t, b, isType("message_complete", "sessionId", first))
	c := in.ws(t, "/ws", opsTok)
	must(t, c.WriteJSON(J{"type": "join_session", "sessionId": first}), "join")
	must(t, c.WriteJSON(J{"type": "rename_session", "sessionId": first, "name": "later"}), "rename")
	for _, f := range until(t, c, isType("session_renamed", "sessionId", first)) {
		if f["type"] == "permission_request" {
			t.Errorf("an answered request was replayed: %v", f)
		}
	}
}
