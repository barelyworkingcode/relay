package features

import (
	"testing"

	"relaye2e/harness"
)

// g1rViewRequest joins sid on a fresh connection, sends a tool call that needs
// approval and returns the connection, the permission_request it raised and
// whether the `asking` session_state was already read on the way.
func g1rViewRequest(t *testing.T, i *harness.Instance, sid string) (*harness.WSConn, map[string]any, bool) {
	t.Helper()
	viewer := i.WebSocket("/ws", i.Credential("mounter"))
	t.Cleanup(viewer.Close)
	viewer.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1aNext(t, viewer, g1tFrameDeadline, g1aIs("session_joined", sid))
	viewer.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "!tool Bash", "trace_id": harness.NewTrace(t)})
	frames := g1aNext(t, viewer, g1tFrameDeadline, g1aIs("permission_request", sid))
	req := frames[len(frames)-1]
	asked := false
	for _, f := range frames {
		asked = asked || g1aAsking(sid)(f)
	}
	if id, _ := req["permissionId"].(string); id == "" {
		t.Fatalf("permission_request has no permissionId: %v", req)
	}
	return viewer, req, asked
}

// g1rResolved reads until the session's permission_resolved and checks it names
// the request and the outcome. It returns every frame read.
func g1rResolved(t *testing.T, ws *harness.WSConn, sid string, req map[string]any, outcome string) []map[string]any {
	t.Helper()
	frames := g1aNext(t, ws, g1aExpiryDeadline, g1aIs("permission_resolved", sid))
	got := frames[len(frames)-1]
	if got["permissionId"] != req["permissionId"] {
		t.Errorf("permission_resolved permissionId = %v, want %v", got["permissionId"], req["permissionId"])
	}
	if got["outcome"] != outcome {
		t.Errorf("permission_resolved outcome = %v, want %v", got["outcome"], outcome)
	}
	return frames
}

// An unanswered request tells its viewer it expired, and the frame comes
// before the turn ends.
func TestPermissionResolvedOnExpiry(t *testing.T) {
	t.Parallel()
	i, sid := g1aSetup(t, "acme-resolved-expiry")
	viewer, req, _ := g1rViewRequest(t, i, sid)
	frames := g1rResolved(t, viewer, sid, req, "expired")
	for _, f := range frames {
		if f["type"] == "turn_done" {
			t.Errorf("turn_done came before permission_resolved: %v", frames)
		}
	}
	g1aNext(t, viewer, g1tFrameDeadline, g1aIs("turn_done", sid))
}

// A joined connection's answer tells the viewer the request was answered.
func TestPermissionResolvedOnAnswer(t *testing.T) {
	t.Parallel()
	i, sid := g1aSetup(t, "acme-resolved-answer")
	viewer, req, _ := g1rViewRequest(t, i, sid)
	viewer.Send(map[string]any{"type": "permission_response", "permissionId": req["permissionId"], "approved": true})
	g1rResolved(t, viewer, sid, req, "answered")
}

// Stopping the session while a request waits tells the viewer it ended.
func TestPermissionResolvedOnStop(t *testing.T) {
	t.Parallel()
	i, sid := g1aSetup(t, "acme-resolved-stop")
	viewer, req, asked := g1rViewRequest(t, i, sid)
	if !asked {
		g1aNext(t, viewer, g1tFrameDeadline, g1aAsking(sid))
	}
	viewer.Send(map[string]any{"type": "stop_generation", "sessionId": sid})
	g1rResolved(t, viewer, sid, req, "ended")
}
