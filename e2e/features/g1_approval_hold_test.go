package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaye2e/harness"
)

// g1aExpiryDeadline bounds a held request's life: relay waits 60 s, the hook
// client 90 s. relay-sessions is not on the debug clock, so an expiry test
// takes the real 60 s; the bound is only the outer limit.
const g1aExpiryDeadline = 150 * time.Second

// g1aSetup boots an instance with a claude project whose PreToolUse hook is
// relay-sessions' own hook mode, and starts one session in it.
func g1aSetup(t *testing.T, name string) (*harness.Instance, string) {
	t.Helper()
	i := harness.Start(t, harness.Options{
		Credentials: g1tCreds, Presence: g1tApprove,
		Settings: map[string]json.RawMessage{"terminal_templates": json.RawMessage(g1hClaudeTemplate)},
	})
	i.WaitSessionHost(g1tHostDeadline)
	p := g1tProject(t, i, name, map[string]any{"allowed_templates": []string{"claude-code"}})
	settings := map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{
		"matcher": "",
		"hooks":   []any{map[string]any{"type": "command", "command": "'" + harness.BundlePaths().Sessions + "' hook"}},
	}}}}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("encoding the hook settings: %v", err)
	}
	dir := filepath.Join(g1tDir(i, name), ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.local.json"), raw, 0o600); err != nil {
		t.Fatalf("writing the hook settings: %v", err)
	}
	return i, g1tStartSession(t, i, p.ID, "sonnet", "")
}

// g1aNext reads frames on ws until pred accepts one, and returns every frame
// read with the match last.
func g1aNext(t *testing.T, ws *harness.WSConn, deadline time.Duration, pred func(map[string]any) bool) []map[string]any {
	t.Helper()
	var seen []map[string]any
	for n := 0; n < g1tFrameLimit; n++ {
		var f map[string]any
		if err := json.Unmarshal(ws.Next(deadline), &f); err != nil {
			t.Fatalf("decoding a frame: %v", err)
		}
		seen = append(seen, f)
		if pred(f) {
			return seen
		}
	}
	t.Fatalf("no wanted frame in %d frames: %v", g1tFrameLimit, seen)
	return nil
}

func g1aIs(typ, sid string) func(map[string]any) bool {
	return func(f map[string]any) bool { return f["type"] == typ && f["sessionId"] == sid }
}

func g1aAsking(sid string) func(map[string]any) bool {
	return func(f map[string]any) bool { return g1aIs("session_state", sid)(f) && f["state"] == "asking" }
}

// g1aReplay joins sid on a fresh connection and returns the connection and the
// permission_request that follows session_joined.
func g1aReplay(t *testing.T, i *harness.Instance, sid string) (*harness.WSConn, map[string]any) {
	t.Helper()
	ws := i.WebSocket("/ws", i.Credential("mounter"))
	t.Cleanup(ws.Close)
	ws.Send(map[string]any{"type": "join_session", "sessionId": sid})
	frames := g1aNext(t, ws, g1tFrameDeadline, g1aIs("permission_request", sid))
	joined := -1
	for n, f := range frames {
		if f["type"] == "session_joined" {
			joined = n
		}
	}
	if joined < 0 || joined > len(frames)-2 {
		t.Fatalf("permission_request did not follow session_joined: %v", frames)
	}
	req := frames[len(frames)-1]
	for _, k := range []string{"permissionId", "toolUseId"} {
		if s, _ := req[k].(string); s == "" {
			t.Errorf("replayed request has no %s: %v", k, req)
		}
	}
	if req["toolName"] != "Bash" {
		t.Errorf("replayed toolName = %v, want Bash", req["toolName"])
	}
	return ws, req
}

// g1aMessageText waits for a `relay session message` and returns its reply.
func g1aMessageText(t *testing.T, p *harness.Proc) string {
	t.Helper()
	res := p.Wait()
	if res.Code != 0 {
		t.Fatalf("session message exited %d\nstderr: %s", res.Code, res.Stderr)
	}
	var out struct {
		Text string `json:"text"`
	}
	res.JSON(t, &out)
	return out.Text
}

func g1aSendHeadless(i *harness.Instance, sid string) *harness.Proc {
	return i.StartCLI(harness.CLIOpts{Deadline: g1aExpiryDeadline},
		"session", "message", "--id", sid, "--text", "!tool Bash", "--json")
}

// A tool call that needs approval while no screen is open waits instead of
// denying; a connection that joins later gets the request after session_joined
// and its answer resolves the call; an unjoined connection saw `asking`.
func TestApprovalHoldsWithoutViewer(t *testing.T) {
	t.Parallel()
	i, sid := g1aSetup(t, "acme-hold")
	unjoined := i.WebSocket("/ws", i.Credential("mounter"))
	defer unjoined.Close()

	turn := g1aSendHeadless(i, sid)
	g1aNext(t, unjoined, g1tFrameDeadline, g1aAsking(sid))

	late, req := g1aReplay(t, i, sid)
	late.Send(map[string]any{"type": "permission_response", "permissionId": req["permissionId"], "approved": true})
	if got := g1aMessageText(t, turn); !strings.HasPrefix(got, "hook: allow") {
		t.Errorf("turn answered %q, want a hook allow", got)
	}
}

// The request stays pending when its last viewer leaves; a later connection
// receives it and its denial reason reaches the model.
func TestApprovalHoldSurvivesViewerLeaving(t *testing.T) {
	t.Parallel()
	i, sid := g1aSetup(t, "acme-leave")
	first := i.WebSocket("/ws", i.Credential("mounter"))
	first.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1aNext(t, first, g1tFrameDeadline, g1aIs("session_joined", sid))
	first.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "!tool Bash", "trace_id": harness.NewTrace(t)})
	g1aNext(t, first, g1tFrameDeadline, g1aIs("permission_request", sid))
	first.Send(map[string]any{"type": "leave_session", "sessionId": sid})
	first.Close()

	late, req := g1aReplay(t, i, sid)
	late.Send(map[string]any{"type": "permission_response", "permissionId": req["permissionId"], "approved": false, "reason": "Acme says no"})
	frames := g1aNext(t, late, g1tFrameDeadline, g1aIs("turn_done", sid))
	if got, _ := frames[len(frames)-1]["excerpt"].(string); got != "hook: deny: Acme says no" {
		t.Errorf("turn answered %q, want the denial and its reason", got)
	}
}

// With no viewer joined at expiry the model is told the session is interactive
// and the owner was away.
func TestApprovalExpiryReasonWithoutViewer(t *testing.T) {
	t.Parallel()
	i, sid := g1aSetup(t, "acme-expire-away")
	got := g1aMessageText(t, g1aSendHeadless(i, sid))
	want := "hook: deny: No approval: the request waited 60 seconds and no screen was open to answer it. This session is interactive; the owner was away."
	if got != want {
		t.Errorf("turn answered %q, want %q", got, want)
	}
	for _, old := range []string{"non-interactive", "no client is viewing"} {
		if strings.Contains(got, old) {
			t.Errorf("reason still says %q: %q", old, got)
		}
	}
}

// With a viewer joined at expiry the reason is the plain not-answered one.
func TestApprovalExpiryReasonWithViewer(t *testing.T) {
	t.Parallel()
	i, sid := g1aSetup(t, "acme-expire-viewed")
	viewer := i.WebSocket("/ws", i.Credential("mounter"))
	defer viewer.Close()
	viewer.Send(map[string]any{"type": "join_session", "sessionId": sid})
	g1aNext(t, viewer, g1tFrameDeadline, g1aIs("session_joined", sid))
	viewer.Send(map[string]any{"type": "send_message", "sessionId": sid, "text": "!tool Bash", "trace_id": harness.NewTrace(t)})
	frames := g1aNext(t, viewer, g1aExpiryDeadline, g1aIs("turn_done", sid))
	want := "hook: deny: No approval: the request was not answered within 60 seconds."
	if got, _ := frames[len(frames)-1]["excerpt"].(string); got != want {
		t.Errorf("turn answered %q, want %q", got, want)
	}
}
