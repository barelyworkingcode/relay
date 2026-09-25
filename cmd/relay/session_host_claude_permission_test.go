//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
)

// createClaudeSession launches a claude session in projectID through eve's
// POST /api/sessions and returns its id.
func (f *relayToolsFixture) createClaudeSession(t *testing.T, projectID string) string {
	t.Helper()
	create, _ := json.Marshal(map[string]any{"projectId": projectID, "model": "sonnet", "settings": map[string]any{"useRelayTools": true}})
	req, _ := http.NewRequest(http.MethodPost, "http://unix/api/sessions", bytes.NewReader(create))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.eveBearer)
	resp, err := dialFrontendHTTP(f.frontendSock).Do(req)
	assertNoErr(t, err, "POST /api/sessions")
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var sess struct {
		ID string `json:"sessionId"`
	}
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(data, &sess) != nil || sess.ID == "" {
		t.Fatalf("POST /api/sessions: status %d, body %s", resp.StatusCode, data)
	}
	return sess.ID
}

func (f *relayToolsFixture) callToolEvent(t *testing.T, sessionID string) map[string]any {
	t.Helper()
	f.audit.Flush()
	for _, ev := range rs10AuditLines(t, f.audit.Path()) {
		actor, _ := ev["actor"].(map[string]any)
		if ev["event"] == string(audit.AuditEventCallTool) && actor["session_id"] == sessionID {
			return ev
		}
	}
	return nil
}

func readWSFrame(t *testing.T, conn *websocket.Conn, typ string) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(rs10Timeout))
	for {
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for a %s frame: %v", typ, err)
		}
		if m["type"] == typ {
			return m
		}
	}
}

func TestSessionHost_ClaudeRelayTools_PermissionGatesCallTool(t *testing.T) {
	f := newRelayToolsFixture(t)
	proj := config.Project{ID: "rt-p1", Name: "Acme", Path: t.TempDir(),
		AllowedMcpIDs: []string{"rt-mcp"}, AllowedModels: []string{"*"}, AllowedTemplates: []string{"*"}}
	assertNoErr(t, f.store.With(func(s *config.Settings) { s.Projects = append(s.Projects, proj) }), "seed project")
	dispatcher := httptest.NewServer(NewFrontendDispatcher(f.enhanced))
	t.Cleanup(dispatcher.Close)

	for _, tc := range []struct {
		name    string
		approve bool
	}{
		{"approve", true},
		{"deny", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := f.createClaudeSession(t, proj.ID)

			conn, resp, err := (&websocket.Dialer{HandshakeTimeout: 5 * time.Second}).
				Dial("ws"+strings.TrimPrefix(dispatcher.URL, "http")+"/ws", nil)
			assertNoErr(t, err, "WS dial through the dispatcher")
			resp.Body.Close()
			defer conn.Close()
			assertNoErr(t, conn.WriteJSON(map[string]any{"type": "join_session", "sessionId": sessionID}), "join_session")
			readWSFrame(t, conn, "session_joined")

			turnDone := make(chan struct{})
			go func() {
				defer close(turnDone)
				r, err := http.Post(dispatcher.URL+"/api/sessions/"+sessionID+"/message", "application/json",
					bytes.NewReader([]byte(`{"text":"list the files in this project"}`)))
				if err == nil {
					r.Body.Close()
				}
			}()

			frame := readWSFrame(t, conn, "permission_request")
			if tool, _ := frame["toolName"].(string); !strings.HasPrefix(tool, "mcp__relay__") {
				t.Fatalf("permission_request toolName = %q, want a relay tool", tool)
			}
			assertNoErr(t, conn.WriteJSON(map[string]any{
				"type": "permission_response", "permissionId": frame["permissionId"], "approved": tc.approve,
			}), "permission_response")

			select {
			case <-turnDone:
			case <-time.After(rs10Timeout):
				t.Fatal("the claude turn never finished after the decision")
			}

			ev := f.callToolEvent(t, sessionID)
			if !tc.approve {
				if ev != nil {
					t.Fatalf("a denied tool call reached relay: %v", ev)
				}
				return
			}
			if ev == nil {
				t.Fatalf("no %s audit event for session %s", audit.AuditEventCallTool, sessionID)
			}
			actor, _ := ev["actor"].(map[string]any)
			if ev["outcome"] != audit.AuditOutcomeOK || actor["kind"] != audit.AuditActorProjectSession {
				t.Fatalf("call_tool outcome %v actor %v, want %q from kind %q", ev["outcome"], actor, audit.AuditOutcomeOK, audit.AuditActorProjectSession)
			}
		})
	}
}
