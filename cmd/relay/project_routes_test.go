package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/mcp"
	"github.com/barelyworkingcode/relay/internal/project"
)

type schemaProviderFunc func() project.McpSurfaces

func (f schemaProviderFunc) AllMcpSurfaces() project.McpSurfaces { return f() }

func newProjectRoutesServer(t *testing.T) (*httptest.Server, config.SettingsStore) {
	t.Helper()
	store := sealedSettingsStoreAt(t.TempDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	store.With(func(s *config.Settings) {
		s.ExternalMcps = []config.ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
		}
	})
	ops := &ProjectOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	mux := http.NewServeMux()
	RegisterProjectRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, store, ops, schemaProviderFunc(testSchemas), nil, nil, nil, nil)
	return httptest.NewServer(mux), store
}

type mcpToolsProviderFunc func(id string) []config.ToolInfo

func (f mcpToolsProviderFunc) ToolInfos(id string) []config.ToolInfo { return f(id) }

type fixedTokenLister struct{}

func (fixedTokenLister) ListTools(_ context.Context, _ string) (json.RawMessage, error) {
	return json.RawMessage(`[{"name":"fs_read","description":"read a file"}]`), nil
}

func (fixedTokenLister) ListSkillBuckets(_ context.Context, _ string) ([]SkillBucket, error) {
	return []SkillBucket{{Key: "Files", Slug: "files", Tools: []mcp.Tool{{Name: "fs_read", Description: "read a file"}}}}, nil
}

func newProjectRoutesServerFull(t *testing.T, tools MCPToolsProvider, lister SkillLister, onChange func()) (*httptest.Server, config.SettingsStore) {
	t.Helper()
	store := sealedSettingsStoreAt(t.TempDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	store.With(func(s *config.Settings) {
		s.ExternalMcps = []config.ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
		}
	})
	ops := &ProjectOps{Store: store, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t), OnChange: onChange}
	mux := http.NewServeMux()
	RegisterProjectRoutes(&control.RouteRegistrar{CredentialID: APICredentialIDFromContext, Mux: mux, Transport: control.TransportSocket}, store, ops, schemaProviderFunc(testSchemas), tools, nil, lister, onChange)
	return httptest.NewServer(mux), store
}

func doJSON(t *testing.T, method, url string, body interface{}) (*http.Response, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, respBody
}

func TestProjectRoutes_CreateAndGet(t *testing.T) {
	srv, _ := newProjectRoutesServer(t)
	defer srv.Close()

	tmpDir := t.TempDir()
	resp, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name":            "Alpha",
		"path":            tmpDir,
		"allowed_mcp_ids": []string{"fsmcp"},
		"allowed_models":  []string{"claude-opus"},
		"chat_templates": []map[string]interface{}{
			{
				"id":               "t1",
				"name":             "Quick",
				"model":            "claude-sonnet",
				"system_prompt":    "be brief",
				"append_claude_md": true,
				"use_relay_tools":  true,
			},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", resp.StatusCode, body)
	}
	var created config.Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v (body: %s)", err, body)
	}
	if created.ID == "" {
		t.Fatalf("expected id to be populated, got %+v", created)
	}
	// projectView strips Token/TokenHash from every eve-facing project
	// response; rotate_token is the sole exception.
	if tok, _ := created.Token.Reveal(); tok != "" || created.TokenHash != "" {
		t.Fatalf("frontend create response leaked token/token_hash: %+v", created)
	}
	if created.Name != "Alpha" || created.Path != tmpDir {
		t.Errorf("unexpected name/path: %+v", created)
	}
	if len(created.ChatTemplates) != 1 || created.ChatTemplates[0].SystemPrompt != "be brief" {
		t.Errorf("chat_templates not round-tripped: %+v", created.ChatTemplates)
	}
	if !created.ChatTemplates[0].AppendClaudeMd || !created.ChatTemplates[0].UseRelayTools {
		t.Errorf("template bool flags not round-tripped on create: %+v", created.ChatTemplates[0])
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d, body %s", resp.StatusCode, body)
	}
	var fetched config.Project
	if err := json.Unmarshal(body, &fetched); err != nil {
		t.Fatalf("decode fetched: %v", err)
	}
	if fetched.ID != created.ID {
		t.Errorf("id mismatch: got %s, want %s", fetched.ID, created.ID)
	}

	resp, body = doJSON(t, "GET", srv.URL+"/api/projects", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, body %s", resp.StatusCode, body)
	}
	var listed []config.Project
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Errorf("expected 1 project with id %s, got %+v", created.ID, listed)
	}
}

func TestProjectRoutes_ShellTemplates(t *testing.T) {
	srv, _ := newProjectRoutesServer(t)
	defer srv.Close()

	tmpDir := t.TempDir()
	resp, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name":           "Shells",
		"path":           tmpDir,
		"allowed_models": []string{"claude-opus"},
		"shell_templates": []map[string]interface{}{
			{
				"id":          "ssh-box",
				"name":        "Box SSH",
				"command":     "ssh",
				"args":        []string{"me@box"},
				"description": "private shell",
			},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d body %s", resp.StatusCode, body)
	}
	var created config.Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if tok, _ := created.Token.Reveal(); tok != "" || created.TokenHash != "" {
		t.Fatalf("create response leaked token: %+v", created)
	}
	if len(created.ShellTemplates) != 1 || created.ShellTemplates[0].Command != "ssh" {
		t.Fatalf("shell_templates not round-tripped on create/view: %+v", created.ShellTemplates)
	}
	if len(created.ShellTemplates[0].Args) != 1 || created.ShellTemplates[0].Args[0] != "me@box" {
		t.Errorf("shell template args not round-tripped: %+v", created.ShellTemplates[0])
	}

	// project.UpdateFields uses a nil pointer for "no change": omitting
	// shell_templates from the patch must leave the list untouched.
	resp, body = doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"name": "Shells-Renamed",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: status %d body %s", resp.StatusCode, body)
	}
	var renamed config.Project
	if err := json.Unmarshal(body, &renamed); err != nil {
		t.Fatalf("decode renamed: %v", err)
	}
	if len(renamed.ShellTemplates) != 1 {
		t.Errorf("rename wiped shell templates (absent != clear): %+v", renamed.ShellTemplates)
	}

	// An explicit empty array, by contrast, sets the pointer to an empty
	// slice and clears the list.
	resp, body = doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"shell_templates": []map[string]interface{}{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear: status %d body %s", resp.StatusCode, body)
	}
	var cleared config.Project
	if err := json.Unmarshal(body, &cleared); err != nil {
		t.Fatalf("decode cleared: %v", err)
	}
	if len(cleared.ShellTemplates) != 0 {
		t.Errorf("explicit empty array did not clear shell templates: %+v", cleared.ShellTemplates)
	}
}

func TestProjectRoutes_CreateValidation(t *testing.T) {
	srv, _ := newProjectRoutesServer(t)
	defer srv.Close()

	resp, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name": "",
		"path": "/tmp/x",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on missing name, got %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "name") {
		t.Errorf("expected error to mention 'name', got %s", body)
	}
}

func TestProjectRoutes_PartialUpdate(t *testing.T) {
	srv, _ := newProjectRoutesServer(t)
	defer srv.Close()

	tmpDir := t.TempDir()
	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name":            "Beta",
		"path":            tmpDir,
		"allowed_mcp_ids": []string{"fsmcp"},
		"allowed_models":  []string{"claude-opus"},
	})
	var created config.Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	originalToken := created.Token

	resp, body := doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"name": "Beta-Renamed",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: status %d body %s", resp.StatusCode, body)
	}
	var renamed config.Project
	if err := json.Unmarshal(body, &renamed); err != nil {
		t.Fatalf("decode renamed: %v", err)
	}
	if renamed.Name != "Beta-Renamed" {
		t.Errorf("name not updated: %s", renamed.Name)
	}
	if renamed.Path != tmpDir {
		t.Errorf("path was not preserved on rename: got %q, want %q", renamed.Path, tmpDir)
	}
	if renamed.Token != originalToken {
		t.Errorf("token rotated on rename — would break active sessions")
	}
	if len(renamed.AllowedMcpIDs) != 1 || renamed.AllowedMcpIDs[0] != "fsmcp" {
		t.Errorf("allowed_mcp_ids dropped on rename: %+v", renamed.AllowedMcpIDs)
	}

	resp, body = doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"chat_templates": []map[string]interface{}{
			{
				"id":              "tmpl-a",
				"name":            "A",
				"model":           "claude-sonnet",
				"system_prompt":   "alpha",
				"use_relay_tools": true,
			},
			{
				"id":               "tmpl-b",
				"name":             "B",
				"model":            "claude-haiku",
				"mode":             "voice",
				"voice":            "af_heart",
				"append_claude_md": true,
			},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("template update: status %d body %s", resp.StatusCode, body)
	}
	var withTemplates config.Project
	if err := json.Unmarshal(body, &withTemplates); err != nil {
		t.Fatalf("decode templates: %v", err)
	}
	if len(withTemplates.ChatTemplates) != 2 {
		t.Fatalf("expected 2 templates, got %d", len(withTemplates.ChatTemplates))
	}
	if withTemplates.ChatTemplates[1].Mode != "voice" || withTemplates.ChatTemplates[1].Voice != "af_heart" {
		t.Errorf("voice template fields not round-tripped: %+v", withTemplates.ChatTemplates[1])
	}
	if !withTemplates.ChatTemplates[0].UseRelayTools || withTemplates.ChatTemplates[0].AppendClaudeMd {
		t.Errorf("template[0] bool flags not round-tripped on update: %+v", withTemplates.ChatTemplates[0])
	}
	if !withTemplates.ChatTemplates[1].AppendClaudeMd || withTemplates.ChatTemplates[1].UseRelayTools {
		t.Errorf("template[1] bool flags not round-tripped on update: %+v", withTemplates.ChatTemplates[1])
	}
}

func TestProjectRoutes_SessionFolders(t *testing.T) {
	srv, _ := newProjectRoutesServer(t)
	defer srv.Close()

	tmpDir := t.TempDir()
	// A blank entry and a duplicate prove the mutator trims and de-dupes.
	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name":            "Folders",
		"path":            tmpDir,
		"session_folders": []string{"Bugs", " ", "Bugs", "Experiments"},
	})
	var created config.Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if got := created.SessionFolders; len(got) != 2 || got[0] != "Bugs" || got[1] != "Experiments" {
		t.Fatalf("session_folders not cleaned/round-tripped on create: %+v", got)
	}

	resp, body := doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"session_folders": []string{"Archive"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("folder update: status %d body %s", resp.StatusCode, body)
	}
	var updated config.Project
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatalf("decode updated: %v", err)
	}
	if len(updated.SessionFolders) != 1 || updated.SessionFolders[0] != "Archive" {
		t.Errorf("session_folders not replaced on update: %+v", updated.SessionFolders)
	}
	if updated.Path != tmpDir {
		t.Errorf("path dropped on folder update: %q", updated.Path)
	}

	resp, body = doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"name": "Folders-Renamed",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: status %d body %s", resp.StatusCode, body)
	}
	var renamed config.Project
	if err := json.Unmarshal(body, &renamed); err != nil {
		t.Fatalf("decode renamed: %v", err)
	}
	if len(renamed.SessionFolders) != 1 || renamed.SessionFolders[0] != "Archive" {
		t.Errorf("session_folders should persist when omitted from patch: %+v", renamed.SessionFolders)
	}

	resp, body = doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"session_folders": []string{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear: status %d body %s", resp.StatusCode, body)
	}
	var cleared config.Project
	if err := json.Unmarshal(body, &cleared); err != nil {
		t.Fatalf("decode cleared: %v", err)
	}
	if len(cleared.SessionFolders) != 0 {
		t.Errorf("session_folders should be cleared by an empty list: %+v", cleared.SessionFolders)
	}
}

func TestProjectRoutes_UpdateUnknown(t *testing.T) {
	srv, _ := newProjectRoutesServer(t)
	defer srv.Close()

	resp, _ := doJSON(t, "PUT", srv.URL+"/api/projects/does-not-exist", map[string]interface{}{
		"name": "Whatever",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestProjectRoutes_Delete(t *testing.T) {
	srv, _ := newProjectRoutesServer(t)
	defer srv.Close()

	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name": "Gamma",
		"path": t.TempDir(),
	})
	var created config.Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}

	resp, _ := doJSON(t, "DELETE", srv.URL+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	resp, _ = doJSON(t, "GET", srv.URL+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", resp.StatusCode)
	}

	// A second delete is a 404, not a repeat 204 — deleting an already-deleted
	// project is an error, not a no-op success.
	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 on second delete, got %d", resp.StatusCode)
	}
}

func TestProjectRoutes_PermissionPolicy(t *testing.T) {
	srv, _ := newProjectRoutesServer(t)
	defer srv.Close()

	tmpDir := t.TempDir()

	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name": "PolicyProj",
		"path": tmpDir,
		"permission_policy": map[string]interface{}{
			"default_mode":  "plan",
			"allowed_tools": []string{"Read", "Grep", "Glob"},
			"denied_tools":  []string{"Write"},
		},
	})
	var created config.Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.PermissionPolicy == nil {
		t.Fatalf("policy not persisted on create")
	}
	if created.PermissionPolicy.DefaultMode != "plan" {
		t.Errorf("default_mode round-trip: got %q", created.PermissionPolicy.DefaultMode)
	}
	if len(created.PermissionPolicy.AllowedTools) != 3 || created.PermissionPolicy.AllowedTools[0] != "Read" {
		t.Errorf("allowed_tools round-trip: %+v", created.PermissionPolicy.AllowedTools)
	}

	resp, body := doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"permission_policy": map[string]interface{}{
			"default_mode":  "default",
			"allowed_tools": []string{"Read"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update policy: status %d body %s", resp.StatusCode, body)
	}
	var updated config.Project
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatalf("decode updated: %v", err)
	}
	if updated.PermissionPolicy == nil || updated.PermissionPolicy.DefaultMode != "default" {
		t.Errorf("policy update not applied: %+v", updated.PermissionPolicy)
	}
	if len(updated.PermissionPolicy.DeniedTools) != 0 {
		t.Errorf("denied_tools should have been cleared: %+v", updated.PermissionPolicy.DeniedTools)
	}

	resp, _ = doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"permission_policy": map[string]interface{}{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear policy: status %d", resp.StatusCode)
	}
	resp, body = doJSON(t, "GET", srv.URL+"/api/projects/"+created.ID, nil)
	var after config.Project
	json.Unmarshal(body, &after)
	if after.PermissionPolicy != nil {
		t.Errorf("policy not cleared by empty struct: %+v", after.PermissionPolicy)
	}

	resp, body = doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"permission_policy": map[string]interface{}{
			"default_mode": "totallyMadeUp",
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid mode, got %d body=%s", resp.StatusCode, body)
	}
}

func TestProjectRoutes_ListMcps(t *testing.T) {
	srv, _ := newProjectRoutesServer(t)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/mcps", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list mcps: status %d body %s", resp.StatusCode, body)
	}
	var mcps []map[string]string
	if err := json.Unmarshal(body, &mcps); err != nil {
		t.Fatalf("decode mcps: %v", err)
	}
	if len(mcps) != 2 {
		t.Fatalf("expected 2 mcps, got %d (%+v)", len(mcps), mcps)
	}
	for _, m := range mcps {
		if m["id"] == "" || m["display_name"] == "" {
			t.Errorf("missing id or display_name in mcp entry: %+v", m)
		}
		if _, hasOAuth := m["oauth_state"]; hasOAuth {
			t.Errorf("oauth_state leaked into /api/mcps response: %+v", m)
		}
	}
}

func TestProjectRoutes_RotateToken_NewTokenInvalidatesOld(t *testing.T) {
	srv, store := newProjectRoutesServerFull(t, nil, nil, nil)
	defer srv.Close()

	tmpDir := t.TempDir()
	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name":            "RotateMe",
		"path":            tmpDir,
		"allowed_mcp_ids": []string{"fsmcp"},
	})
	var created config.Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	oldToken, _ := created.Token.Reveal()

	resp, body := doJSON(t, "POST", srv.URL+"/api/projects/"+created.ID+"/rotate_token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: status %d body %s", resp.StatusCode, body)
	}
	var rotated map[string]string
	if err := json.Unmarshal(body, &rotated); err != nil {
		t.Fatalf("decode rotated: %v", err)
	}
	newToken := rotated["token"]
	if newToken == "" || newToken == oldToken {
		t.Fatalf("rotated token unchanged or empty")
	}

	if _, err := store.Get().AuthenticateProject(oldToken); err == nil {
		t.Fatalf("old token still authenticates after rotation")
	}
	if _, err := store.Get().AuthenticateProject(newToken); err != nil {
		t.Fatalf("new token does not authenticate: %v", err)
	}
}

func TestProjectRoutes_RotateToken_Unknown404(t *testing.T) {
	srv, _ := newProjectRoutesServerFull(t, nil, nil, nil)
	defer srv.Close()

	resp, _ := doJSON(t, "POST", srv.URL+"/api/projects/nope/rotate_token", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestProjectRoutes_RegenSkill_OK(t *testing.T) {
	srv, _ := newProjectRoutesServerFull(t, nil, fixedTokenLister{}, nil)
	defer srv.Close()

	tmpDir := t.TempDir()
	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name":            "WithSkill",
		"path":            tmpDir,
		"allowed_mcp_ids": []string{"fsmcp"},
		"generate_skill":  true,
	})
	var created config.Project
	_ = json.Unmarshal(body, &created)

	resp, body := doJSON(t, "POST", srv.URL+"/api/projects/"+created.ID+"/regen_skill", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("regen: status %d body %s", resp.StatusCode, body)
	}
	var out map[string]string
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode regen: %v", err)
	}
	if out["path"] == "" {
		t.Errorf("missing path in regen response: %s", body)
	}
}

func TestProjectRoutes_RegenSkill_NoListerReturns503(t *testing.T) {
	srv, _ := newProjectRoutesServerFull(t, nil, nil, nil)
	defer srv.Close()

	tmpDir := t.TempDir()
	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name":            "NoLister",
		"path":            tmpDir,
		"allowed_mcp_ids": []string{"fsmcp"},
	})
	var created config.Project
	_ = json.Unmarshal(body, &created)

	resp, _ := doJSON(t, "POST", srv.URL+"/api/projects/"+created.ID+"/regen_skill", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no skill lister wired, got %d", resp.StatusCode)
	}
}

func TestProjectRoutes_ListMcpTools_ReturnsLiveList(t *testing.T) {
	provider := mcpToolsProviderFunc(func(id string) []config.ToolInfo {
		if id == "fsmcp" {
			return []config.ToolInfo{{Name: "fs_read"}, {Name: "fs_write"}}
		}
		return nil
	})
	srv, _ := newProjectRoutesServerFull(t, provider, nil, nil)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/mcps/fsmcp/tools", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body %s", resp.StatusCode, body)
	}
	var infos []config.ToolInfo
	if err := json.Unmarshal(body, &infos); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(infos) != 2 {
		t.Errorf("expected 2 tools, got %d", len(infos))
	}
}

func TestProjectRoutes_ListMcpTools_UnknownMcp404(t *testing.T) {
	provider := mcpToolsProviderFunc(func(id string) []config.ToolInfo { return nil })
	srv, _ := newProjectRoutesServerFull(t, provider, nil, nil)
	defer srv.Close()

	resp, _ := doJSON(t, "GET", srv.URL+"/api/mcps/nope/tools", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestProjectRoutes_ListMcpTools_NoProvider503(t *testing.T) {
	srv, _ := newProjectRoutesServerFull(t, nil, nil, nil)
	defer srv.Close()

	resp, _ := doJSON(t, "GET", srv.URL+"/api/mcps/fsmcp/tools", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 with no provider, got %d", resp.StatusCode)
	}
}

func TestProjectRoutes_OnChangeFires(t *testing.T) {
	var changed int
	onChange := func() { changed++ }
	srv, _ := newProjectRoutesServerFull(t, nil, nil, onChange)
	defer srv.Close()

	tmpDir := t.TempDir()
	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name":            "ChangeMe",
		"path":            tmpDir,
		"allowed_mcp_ids": []string{"fsmcp"},
	})
	var created config.Project
	_ = json.Unmarshal(body, &created)

	if changed != 1 {
		t.Fatalf("expected onChange after create, got %d calls", changed)
	}

	_, _ = doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"name": "RenamedMe",
	})
	if changed != 2 {
		t.Fatalf("expected onChange after update, got %d calls total", changed)
	}

	_, _ = doJSON(t, "POST", srv.URL+"/api/projects/"+created.ID+"/rotate_token", nil)
	if changed != 3 {
		t.Fatalf("expected onChange after rotate, got %d calls total", changed)
	}

	_, _ = doJSON(t, "DELETE", srv.URL+"/api/projects/"+created.ID, nil)
	if changed != 4 {
		t.Fatalf("expected onChange after delete, got %d calls total", changed)
	}
}

func TestProjectRoutes_ListMcpTools_DoesNotLeakCredentials(t *testing.T) {
	provider := mcpToolsProviderFunc(func(id string) []config.ToolInfo {
		return []config.ToolInfo{{Name: "fs_read", Description: "read"}}
	})
	srv, _ := newProjectRoutesServerFull(t, provider, nil, nil)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/mcps/fsmcp/tools", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var generic []map[string]interface{}
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("decode: %v", err)
	}
	allowedKeys := map[string]bool{"name": true, "description": true, "category": true}
	for _, m := range generic {
		for k := range m {
			if !allowedKeys[k] {
				t.Errorf("unexpected key in tool entry: %q (allowed: name/description/category)", k)
			}
		}
	}
}

func TestProjectRoutes_FullLifecycle(t *testing.T) {
	srv, store := newProjectRoutesServerFull(t, nil, fixedTokenLister{}, nil)
	defer srv.Close()

	projDir := t.TempDir()
	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name":            "FullLifecycle",
		"path":            projDir,
		"allowed_mcp_ids": []string{"fsmcp"},
		"generate_skill":  true,
	})
	var created config.Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	// fixedTokenLister buckets its single tool under the "Files" key, which
	// slugs to the "relay-files" directory below.
	skillFile := filepath.Join(projectSkillDir(created), "relay-files", "SKILL.md")
	if _, err := os.Stat(skillFile); err != nil {
		t.Fatalf("SKILL.md not created at %s: %v", skillFile, err)
	}

	// created.Token is stripped by projectView, so read the real plaintext
	// from the store — otherwise this check would compare against an empty
	// string and pass vacuously.
	stored, _ := config.FindProjectByID(store.Get(), created.ID)
	if stored == nil {
		t.Fatalf("project not found in store")
	}
	storedToken, _ := stored.Token.Reveal()
	if storedToken == "" {
		t.Fatalf("expected a stored project token to check against")
	}
	skillContent, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if bytes.Contains(skillContent, []byte(storedToken)) {
		t.Errorf("SKILL.md leaks the project token (%d bytes) — content: %s", len(storedToken), skillContent)
	}

	resp, body := doJSON(t, "POST", srv.URL+"/api/projects/"+created.ID+"/rotate_token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: %d body %s", resp.StatusCode, body)
	}

	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if _, err := os.Stat(skillFile); err == nil {
		t.Fatalf("SKILL.md still present at %s after delete", skillFile)
	}
}
