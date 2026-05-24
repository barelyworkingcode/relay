package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// schemaProviderFunc adapts a plain function to ContextSchemasProvider
// so the tests can reuse the existing testSchemas() helper.
type schemaProviderFunc func() map[string]json.RawMessage

func (f schemaProviderFunc) AllContextSchemas() map[string]json.RawMessage { return f() }

func newProjectRoutesServer(t *testing.T) (*httptest.Server, SettingsStore) {
	t.Helper()
	store := NewSettingsStoreAt(t.TempDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	store.With(func(s *Settings) {
		s.ExternalMcps = []ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
		}
	})
	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store, schemaProviderFunc(testSchemas), nil, nil, nil)
	return httptest.NewServer(mux), store
}

// mcpToolsProviderFunc adapts a plain function so tests can supply tool
// info via a literal map without spinning up an ExternalMcpManager.
type mcpToolsProviderFunc func(id string) []ToolInfo

func (f mcpToolsProviderFunc) ToolInfos(id string) []ToolInfo { return f(id) }

// fixedTokenLister returns a constant JSON tools array so EmitSkill can
// produce a SKILL.md without spawning real MCP processes.
type fixedTokenLister struct{}

func (fixedTokenLister) ListTools(_ context.Context, _ string) (json.RawMessage, error) {
	return json.RawMessage(`[{"name":"fs_read","description":"read a file"}]`), nil
}

// newProjectRoutesServerFull wires the extra dependencies (MCPToolsProvider,
// SkillLister, onChange) so tests can exercise the rotate_token /
// regen_skill / list_mcp_tools routes.
func newProjectRoutesServerFull(t *testing.T, tools MCPToolsProvider, lister SkillLister, onChange func()) (*httptest.Server, SettingsStore) {
	t.Helper()
	store := NewSettingsStoreAt(t.TempDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	store.With(func(s *Settings) {
		s.ExternalMcps = []ExternalMcp{
			{ID: "fsmcp", DisplayName: "fsMCP"},
			{ID: "macmcp", DisplayName: "macMCP"},
		}
	})
	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store, schemaProviderFunc(testSchemas), tools, lister, onChange)
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
	var created Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v (body: %s)", err, body)
	}
	if created.ID == "" || created.Token == "" || created.TokenHash == "" {
		t.Fatalf("expected id/token/token_hash to be populated, got %+v", created)
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

	// GET single
	resp, body = doJSON(t, "GET", srv.URL+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d, body %s", resp.StatusCode, body)
	}
	var fetched Project
	if err := json.Unmarshal(body, &fetched); err != nil {
		t.Fatalf("decode fetched: %v", err)
	}
	if fetched.ID != created.ID {
		t.Errorf("id mismatch: got %s, want %s", fetched.ID, created.ID)
	}

	// GET list
	resp, body = doJSON(t, "GET", srv.URL+"/api/projects", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d, body %s", resp.StatusCode, body)
	}
	var listed []Project
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Errorf("expected 1 project with id %s, got %+v", created.ID, listed)
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
	var created Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	originalToken := created.Token

	// Patch only the name; everything else must be preserved including the
	// inline token (rotating tokens on every PUT would break Eve sessions).
	resp, body := doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"name": "Beta-Renamed",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: status %d body %s", resp.StatusCode, body)
	}
	var renamed Project
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

	// Patch the templates list — exercises the new UpdateProjectChatTemplates mutator.
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
	var withTemplates Project
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
	var created Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}

	resp, _ := doJSON(t, "DELETE", srv.URL+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	// Subsequent GET should 404.
	resp, _ = doJSON(t, "GET", srv.URL+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", resp.StatusCode)
	}

	// Second delete is also 404 (idempotent failure mode, not a 204).
	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 on second delete, got %d", resp.StatusCode)
	}
}

func TestProjectRoutes_PermissionPolicy(t *testing.T) {
	srv, _ := newProjectRoutesServer(t)
	defer srv.Close()

	tmpDir := t.TempDir()

	// Create with policy.
	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name": "PolicyProj",
		"path": tmpDir,
		"permission_policy": map[string]interface{}{
			"default_mode":  "plan",
			"allowed_tools": []string{"Read", "Grep", "Glob"},
			"denied_tools":  []string{"Write"},
		},
	})
	var created Project
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

	// Update policy via PUT.
	resp, body := doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"permission_policy": map[string]interface{}{
			"default_mode": "default",
			"allowed_tools": []string{"Read"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update policy: status %d body %s", resp.StatusCode, body)
	}
	var updated Project
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatalf("decode updated: %v", err)
	}
	if updated.PermissionPolicy == nil || updated.PermissionPolicy.DefaultMode != "default" {
		t.Errorf("policy update not applied: %+v", updated.PermissionPolicy)
	}
	if len(updated.PermissionPolicy.DeniedTools) != 0 {
		t.Errorf("denied_tools should have been cleared: %+v", updated.PermissionPolicy.DeniedTools)
	}

	// Empty policy struct clears the policy.
	resp, _ = doJSON(t, "PUT", srv.URL+"/api/projects/"+created.ID, map[string]interface{}{
		"permission_policy": map[string]interface{}{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear policy: status %d", resp.StatusCode)
	}
	resp, body = doJSON(t, "GET", srv.URL+"/api/projects/"+created.ID, nil)
	var after Project
	json.Unmarshal(body, &after)
	if after.PermissionPolicy != nil {
		t.Errorf("policy not cleared by empty struct: %+v", after.PermissionPolicy)
	}

	// Invalid mode rejected.
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
	// Verify the picker fields are present and OAuth-y fields are absent.
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
	var created Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	oldToken := created.Token

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
	var created Project
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
	var created Project
	_ = json.Unmarshal(body, &created)

	resp, _ := doJSON(t, "POST", srv.URL+"/api/projects/"+created.ID+"/regen_skill", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no skill lister wired, got %d", resp.StatusCode)
	}
}

func TestProjectRoutes_ListMcpTools_ReturnsLiveList(t *testing.T) {
	provider := mcpToolsProviderFunc(func(id string) []ToolInfo {
		if id == "fsmcp" {
			return []ToolInfo{{Name: "fs_read"}, {Name: "fs_write"}}
		}
		return nil
	})
	srv, _ := newProjectRoutesServerFull(t, provider, nil, nil)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/mcps/fsmcp/tools", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body %s", resp.StatusCode, body)
	}
	var infos []ToolInfo
	if err := json.Unmarshal(body, &infos); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(infos) != 2 {
		t.Errorf("expected 2 tools, got %d", len(infos))
	}
}

func TestProjectRoutes_ListMcpTools_UnknownMcp404(t *testing.T) {
	provider := mcpToolsProviderFunc(func(id string) []ToolInfo { return nil })
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
	var created Project
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

// TestProjectRoutes_ListMcpTools_DoesNotLeakCredentials guards the picker
// response surface against drift — if someone later "helpfully" includes the
// full ExternalMcp struct in /api/mcps/{id}/tools, OAuth tokens and stdio
// envs would leak into the project editor UI.
func TestProjectRoutes_ListMcpTools_DoesNotLeakCredentials(t *testing.T) {
	provider := mcpToolsProviderFunc(func(id string) []ToolInfo {
		return []ToolInfo{{Name: "fs_read", Description: "read"}}
	})
	srv, _ := newProjectRoutesServerFull(t, provider, nil, nil)
	defer srv.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/mcps/fsmcp/tools", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	// Decode into a generic shape so we can assert no surprise keys exist.
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

// TestProjectRoutes_FullLifecycle exercises the HTTP API the way Eve does:
// create a project with generate_skill=true, verify SKILL.md lands on disk,
// rotate the token, delete the project, verify SKILL.md is gone. Mirrors
// TestProjectLifecycle_CreateWithSkill_Delete_CleansUpSkillFile but through
// the HTTP layer Eve consumes today.
func TestProjectRoutes_FullLifecycle(t *testing.T) {
	srv, _ := newProjectRoutesServerFull(t, nil, fixedTokenLister{}, nil)
	defer srv.Close()

	projDir := t.TempDir()
	_, body := doJSON(t, "POST", srv.URL+"/api/projects", map[string]interface{}{
		"name":            "FullLifecycle",
		"path":            projDir,
		"allowed_mcp_ids": []string{"fsmcp"},
		"generate_skill":  true,
	})
	var created Project
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	skillFile := projectSkillDir(created) + "/SKILL.md"
	if _, err := os.Stat(skillFile); err != nil {
		t.Fatalf("SKILL.md not created at %s: %v", skillFile, err)
	}

	// Confirm content doesn't leak the token.
	skillContent, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	if bytes.Contains(skillContent, []byte(created.Token)) {
		t.Errorf("SKILL.md leaks the project token (%d bytes) — content: %s", len(created.Token), skillContent)
	}

	// Rotate the token via HTTP.
	resp, body := doJSON(t, "POST", srv.URL+"/api/projects/"+created.ID+"/rotate_token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: %d body %s", resp.StatusCode, body)
	}

	// Delete project; SKILL.md must be cleaned up.
	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if _, err := os.Stat(skillFile); err == nil {
		t.Fatalf("SKILL.md still present at %s after delete", skillFile)
	}
}

// Suppress unused-import warning if a test that uses context is conditionally
// excluded by a build tag later.
var _ = context.Background
