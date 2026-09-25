package hostapi_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/sessions/permission"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/testutil"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const flowTool = "mcp__relay__fs_list"

// flowHost is a mounted host whose claude sessions run on FakeProviders, one
// per session id, each of which can be handed a process root.
type flowHost struct {
	*mountedHost
	sessions *session.Manager
	fakes    map[string]*testutil.FakeProvider
}

func startFlowHost(t *testing.T) *flowHost {
	t.Helper()
	terminals, sessions := buildManagers(t)
	f := &flowHost{sessions: sessions, fakes: map[string]*testutil.FakeProvider{}}
	sessions.SetProviderFactory(func(sess *sessionstypes.Session, _ session.CreateSpec, handler sessionstypes.EventHandler) (sessionstypes.Provider, error) {
		fp := testutil.NewFakeProvider(handler)
		f.fakes[sess.ID] = fp
		return fp, nil
	})
	hookSock := filepath.Join(mkShortTempDir(t, "hostapi-hook-"), "hook.sock")
	f.mountedHost = startMountedHost(t, os.Getpid(), hookSock, terminals, sessions, permission.NewPermissionManager())
	return f
}

// create starts a claude session and returns its directory.
func (f *flowHost) create(t *testing.T, id, settings string) string {
	t.Helper()
	dir := mkShortTempDir(t, "hostapi-proj-")
	if _, err := f.sessions.Create(session.CreateSpec{
		SessionID: id, ProjectID: "p1", Kind: session.KindClaude, Directory: dir, Model: "sonnet",
		Settings: json.RawMessage(settings),
	}); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	return dir
}

// hookCall posts /permission from a child process that is made the process
// root of rootSession's provider before it connects.
func (f *flowHost) hookCall(t *testing.T, rootSession, bodySession, tool, input string) *childPost {
	t.Helper()
	c := startChildPost(t, f.hookSock, "/permission", map[string]any{
		"sessionId": bodySession, "toolName": tool, "toolInput": input, "toolUseId": "tu1",
	})
	root, ok := testutil.ProcessRootOf(c.PID())
	if !ok {
		t.Fatalf("ProcessRootOf(%d) failed", c.PID())
	}
	f.fakes[rootSession].SetProcessRoot(root)
	return c
}

func (f *flowHost) joinViewer(t *testing.T, id string) *websocket.Conn {
	t.Helper()
	conn, resp, err := dialMountedWS(f.internalSock, f.bearer)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	resp.Body.Close()
	t.Cleanup(func() { conn.Close() })
	if err := conn.WriteJSON(map[string]any{"type": "join_session", "sessionId": id}); err != nil {
		t.Fatalf("write join_session: %v", err)
	}
	if _, ok := readFrame(conn, "session_joined", 5*time.Second); !ok {
		t.Fatalf("no session_joined for %s", id)
	}
	return conn
}

// readFrame skips frames until one of type typ arrives. A timeout leaves conn
// unusable, which is why callers only use it last on a connection.
func readFrame(conn *websocket.Conn, typ string, timeout time.Duration) (map[string]any, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			return nil, false
		}
		if m["type"] == typ {
			return m, true
		}
	}
}

type hookDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func decodeDecision(t *testing.T, c *childPost) hookDecision {
	t.Helper()
	status, body := c.Wait()
	var d hookDecision
	if status != http.StatusOK || json.Unmarshal(body, &d) != nil {
		t.Fatalf("status %d body %q, want 200 with a decision", status, body)
	}
	return d
}

func TestPermissionFlow_ViewerDecides(t *testing.T) {
	for _, tc := range []struct {
		name         string
		approved     bool
		wantDecision string
		wantReason   string // "" skips the check
	}{
		{"approve", true, "allow", ""},
		{"deny_without_reason", false, "deny", "Denied by user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startFlowHost(t)
			const id = "aaaaaaaa-0000-0000-0000-000000000001"
			f.create(t, id, `{}`)
			conn := f.joinViewer(t, id)

			c := f.hookCall(t, id, id, flowTool, `{"path":"."}`)
			frame, ok := readFrame(conn, "permission_request", 10*time.Second)
			if !ok {
				c.Kill()
				t.Fatal("no permission_request frame reached the viewer")
			}
			want := map[string]any{"sessionId": id, "toolName": flowTool, "toolInput": `{"path":"."}`, "toolUseId": "tu1"}
			for k, v := range want {
				if frame[k] != v {
					t.Errorf("frame[%q] = %v, want %v (frame %v)", k, frame[k], v, frame)
				}
			}
			permID, _ := frame["permissionId"].(string)
			if permID == "" {
				t.Fatalf("frame has no permissionId: %v", frame)
			}

			if err := conn.WriteJSON(map[string]any{"type": "permission_response", "permissionId": permID, "approved": tc.approved}); err != nil {
				t.Fatalf("write permission_response: %v", err)
			}
			d := decodeDecision(t, c)
			if d.Decision != tc.wantDecision || (tc.wantReason != "" && d.Reason != tc.wantReason) {
				t.Fatalf("decision = %+v, want %s %q", d, tc.wantDecision, tc.wantReason)
			}
		})
	}
}

func TestPermissionFlow_NoViewer_DeniesAtOnce(t *testing.T) {
	f := startFlowHost(t)
	const id = "aaaaaaaa-0000-0000-0000-000000000002"
	f.create(t, id, `{}`)

	start := time.Now()
	d := decodeDecision(t, f.hookCall(t, id, id, flowTool, `{}`))
	if d.Decision != "deny" || d.Reason != "no client is viewing this session to approve the tool call" {
		t.Fatalf("decision = %+v, want the no-viewer deny", d)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("no-viewer deny took %s, want an immediate answer", elapsed)
	}
	if n := f.perms.PendingCount(); n != 0 {
		t.Fatalf("PendingCount = %d, want 0: no request is created without a viewer", n)
	}
}

func TestPermissionFlow_PreflightDecidesWithoutPrompt(t *testing.T) {
	for _, tc := range []struct {
		name, settings, tool, input string
		wantDecision, wantReason    string
	}{
		{"bypass", `{"permissionMode":"bypassPermissions"}`, flowTool, `{}`,
			"allow", "bypassPermissions mode"},
		{"policy_allow", `{"permissionPolicy":{"allowedTools":["` + flowTool + `"]}}`, flowTool, `{}`,
			"allow", "allowed by project policy"},
		{"policy_deny_beats_bypass", `{"permissionMode":"bypassPermissions","permissionPolicy":{"deniedTools":["` + flowTool + `"]}}`, flowTool, `{}`,
			"deny", "denied by project policy"},
		{"accept_edits_inside_dir", `{"permissionMode":"acceptEdits"}`, "Write", `{"file_path":"DIR/notes.txt","content":"x"}`,
			"allow", "acceptEdits mode: edit inside the session directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startFlowHost(t)
			const id = "aaaaaaaa-0000-0000-0000-000000000003"
			dir := f.create(t, id, tc.settings)
			conn := f.joinViewer(t, id)

			c := f.hookCall(t, id, id, tc.tool, strings.ReplaceAll(tc.input, "DIR", dir))
			if frame, prompted := readFrame(conn, "permission_request", time.Second); prompted {
				c.Kill()
				t.Fatalf("preflight case prompted the viewer: %v", frame)
			}
			d := decodeDecision(t, c)
			if d.Decision != tc.wantDecision || d.Reason != tc.wantReason {
				t.Fatalf("decision = %+v, want %s %q", d, tc.wantDecision, tc.wantReason)
			}
		})
	}
}

func TestPermissionFlow_UnresolvedCaller_Forbidden(t *testing.T) {
	const idA, idB = "aaaaaaaa-0000-0000-0000-00000000000a", "aaaaaaaa-0000-0000-0000-00000000000b"
	for _, tc := range []struct {
		name        string
		bodySession string
		killFirst   bool
	}{
		{"wrong_session_id", idB, false},
		{"dead_provider", idA, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startFlowHost(t)
			f.create(t, idA, `{"permissionMode":"bypassPermissions"}`)
			f.create(t, idB, `{"permissionMode":"bypassPermissions"}`)

			c := f.hookCall(t, idA, tc.bodySession, flowTool, `{}`)
			if tc.killFirst {
				f.fakes[idA].Kill()
			}
			status, body := c.Wait()
			if status != http.StatusForbidden || len(body) != 0 {
				t.Fatalf("status %d body %q, want 403 with an empty body", status, body)
			}
		})
	}
}

func TestPermissionFlow_HookDisconnect_CleansUp(t *testing.T) {
	f := startFlowHost(t)
	const id = "aaaaaaaa-0000-0000-0000-000000000004"
	f.create(t, id, `{}`)
	conn := f.joinViewer(t, id)

	c := f.hookCall(t, id, id, flowTool, `{}`)
	if _, ok := readFrame(conn, "permission_request", 10*time.Second); !ok {
		c.Kill()
		t.Fatal("no permission_request frame reached the viewer")
	}
	if n := f.perms.PendingCount(); n != 1 {
		t.Fatalf("PendingCount = %d before the disconnect, want 1", n)
	}
	c.Kill()

	deadline := time.Now().Add(5 * time.Second)
	for f.perms.PendingCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("pending request survived the hook's disconnect: %v", f.perms.PendingIDs())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
