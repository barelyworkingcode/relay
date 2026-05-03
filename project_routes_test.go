package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	RegisterProjectRoutes(mux, store, schemaProviderFunc(testSchemas))
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
		"name":             "Alpha",
		"path":             tmpDir,
		"allowed_mcp_ids":  []string{"fsmcp"},
		"allowed_models":   []string{"claude-opus"},
		"chat_templates": []map[string]string{
			{"id": "t1", "name": "Quick", "model": "claude-sonnet", "system_prompt": "be brief"},
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
		"chat_templates": []map[string]string{
			{"id": "tmpl-a", "name": "A", "model": "claude-sonnet", "system_prompt": "alpha"},
			{"id": "tmpl-b", "name": "B", "model": "claude-haiku", "mode": "voice", "voice": "af_heart"},
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
