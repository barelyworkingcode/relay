package main

// The permission set has to survive the round trip on BOTH surfaces. ADR-004's
// model is two co-equal editors over one mutator layer, so a field that only
// eve can set, or only the tray, is a field an operator will eventually set
// from the wrong window and watch vanish.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newV2ProjectRoutesServer is newProjectRoutesServer with a LIVE v2 surface
// (macMCP's worked example) instead of fsMCP's v1 fixture, so the scope
// validation has a declaration to check against.
func newV2ProjectRoutesServer(t *testing.T) (string, SettingsStore) {
	t.Helper()
	store := NewSettingsStoreAt(t.TempDir())
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	store.With(func(s *Settings) {
		s.ExternalMcps = []ExternalMcp{{ID: "macmcp", DisplayName: "macMCP"}}
	})
	mux := http.NewServeMux()
	RegisterProjectRoutes(mux, store, schemaProviderFunc(v2Surfaces), nil, nil, nil, nil)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, store
}

// The profile ADR-011's worked example describes, created over HTTP and read
// back through projectView. Every field the PUT accepted has to be on the GET:
// a UI that cannot render its own state is one an operator edits blind.
func TestProjectRoutes_PermissionSetRoundTrips(t *testing.T) {
	base, store := newV2ProjectRoutesServer(t)

	resp, body := doJSON(t, "POST", base+"/api/projects", map[string]interface{}{
		"name":            "Hermes — Bob INBOX (read-only)",
		"kind":            "remote",
		"allowed_mcp_ids": []string{"macmcp"},
		"allowed_tools":   map[string][]string{"macmcp": {"mail_*"}},
		"access":          map[string]string{"macmcp": "read"},
		"context": map[string]interface{}{
			"macmcp": map[string]interface{}{
				"mail_accounts":  []string{"Bob"},
				"mail_mailboxes": []string{"INBOX"},
			},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d: %s", resp.StatusCode, body)
	}
	var created projectView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Kind != ProjectKindRemote {
		t.Errorf("kind missing from the view: %q", created.Kind)
	}
	if got := created.Access["macmcp"]; got != AccessRead {
		t.Errorf("access missing from the view: %#v", created.Access)
	}
	if got := created.AllowedTools["macmcp"]; len(got) != 1 || got[0] != "mail_*" {
		t.Errorf("allowed_tools missing from the view: %#v", created.AllowedTools)
	}
	if !strings.Contains(string(created.Context["macmcp"]), `"Bob"`) {
		t.Errorf("context missing from the view: %s", created.Context["macmcp"])
	}

	// And it is what actually reached settings.json, not just what the
	// response echoed.
	stored, _ := store.Get().findProjectByID(created.ID)
	if stored == nil || stored.Access["macmcp"] != AccessRead {
		t.Fatalf("the mode did not persist: %#v", stored)
	}

	// GET renders the same thing.
	resp, body = doJSON(t, "GET", base+"/api/projects/"+created.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d", resp.StatusCode)
	}
	var fetched projectView
	if err := json.Unmarshal(body, &fetched); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if fetched.Access["macmcp"] != AccessRead || len(fetched.AllowedTools["macmcp"]) != 1 {
		t.Errorf("GET does not show what the POST accepted: %#v", fetched)
	}

	// PUT patches one layer and leaves the others alone — nil means no change.
	resp, body = doJSON(t, "PUT", base+"/api/projects/"+created.ID, map[string]interface{}{
		"access": map[string]string{"macmcp": "write"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update: status %d: %s", resp.StatusCode, body)
	}
	var updated projectView
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updated.Access["macmcp"] != AccessWrite {
		t.Errorf("mode did not change: %#v", updated.Access)
	}
	if len(updated.AllowedTools["macmcp"]) != 1 || !strings.Contains(string(updated.Context["macmcp"]), "INBOX") {
		t.Errorf("an untouched layer was cleared by a patch of another: %#v", updated)
	}
}

// A bad value is refused with a 400 and NOTHING is mutated. The whole point of
// validating on save is that an invalid confinement never becomes a stored one.
func TestProjectRoutes_RefusesInvalidPermissionsAndMutatesNothing(t *testing.T) {
	base, store := newV2ProjectRoutesServer(t)

	resp, body := doJSON(t, "POST", base+"/api/projects", map[string]interface{}{
		"name": "Profile", "kind": "remote",
		"allowed_mcp_ids": []string{"macmcp"},
		"access":          map[string]string{"macmcp": "read"},
		"context":         map[string]interface{}{"macmcp": map[string]interface{}{"mail_accounts": []string{"Bob"}}},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed: status %d: %s", resp.StatusCode, body)
	}
	var created projectView
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, bad := range []struct {
		label string
		patch map[string]interface{}
		says  string
	}{
		{"undeclared field", map[string]interface{}{"context": map[string]interface{}{"macmcp": map[string]interface{}{"mail_folders": []string{"INBOX"}}}}, "mail_folders"},
		{"empty scope", map[string]interface{}{"context": map[string]interface{}{"macmcp": map[string]interface{}{"mail_accounts": []string{}}}}, "mail_accounts"},
		{"derived field", map[string]interface{}{"context": map[string]interface{}{"macmcp": map[string]interface{}{"file_dirs": []string{"/etc"}}}}, "file_dirs"},
		{"bad mode", map[string]interface{}{"access": map[string]string{"macmcp": "admin"}}, "admin"},
		{"bad pattern", map[string]interface{}{"allowed_tools": map[string][]string{"macmcp": {"mail_["}}}, "mail_["},
	} {
		resp, body := doJSON(t, "PUT", base+"/api/projects/"+created.ID, bad.patch)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", bad.label, resp.StatusCode, body)
			continue
		}
		if !strings.Contains(string(body), bad.says) {
			t.Errorf("%s: refusal does not name the problem (%q): %s", bad.label, bad.says, body)
		}
		stored, _ := store.Get().findProjectByID(created.ID)
		if stored == nil || !strings.Contains(string(stored.Context["macmcp"]), "Bob") || stored.Access["macmcp"] != AccessRead {
			t.Fatalf("%s: a refused patch mutated the stored record: %#v", bad.label, stored)
		}
	}
}

// GET /api/mcps/{id}/scope_fields is how eve renders the same panel the tray
// does. An MCP relay has never connected to is a 404, not an empty list:
// "scopes nothing" and "cannot say" are different answers and only one of them
// lets an editor safely offer no fields.
func TestProjectRoutes_ScopeFields(t *testing.T) {
	base, _ := newV2ProjectRoutesServer(t)

	resp, body := doJSON(t, "GET", base+"/api/mcps/macmcp/scope_fields", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var fields []ScopeFieldView
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("want macMCP's three restrict fields, got %d: %s", len(fields), body)
	}
	byName := map[string]ScopeFieldView{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	// The absent-source-means-operator rule is applied ON THE GO SIDE so no
	// consumer has to re-derive it.
	if got := byName["mail_accounts"]; got.Source != ContextSourceOperator || !got.Enumerable || got.Description == "" {
		t.Errorf("mail_accounts projected wrong: %#v", got)
	}
	if got := byName["file_dirs"]; got.Source != ContextSourceProjectPath {
		t.Errorf("file_dirs must be marked derived, got %q", got.Source)
	}
	if got := byName["mail_mailboxes"]; len(got.DependsOn) != 1 || got.DependsOn[0] != "mail_accounts" {
		t.Errorf("depends_on lost: %#v", got)
	}

	resp, _ = doJSON(t, "GET", base+"/api/mcps/nosuch/scope_fields", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown MCP: status %d, want 404", resp.StatusCode)
	}
}

// The IPC path decodes the SAME DTOs, so it accepts and refuses identically.
// This is the half a tray-only operator sees.
func TestIpcUpdateProject_PermissionSetRoundTrips(t *testing.T) {
	ctx, store, ui, _ := newProjectsIPC(t)
	ctx.Tools.(*fakeTools).surfaces = v2Surfaces()

	ipcCreateProject(ctx, mustJSON(t, map[string]interface{}{
		"name": "Profile", "kind": "remote",
		"allowed_mcp_ids": []string{"macmcp"},
		"allowed_tools":   map[string][]string{"macmcp": {"mail_*"}},
		"access":          map[string]string{"macmcp": "read"},
		"context":         map[string]interface{}{"macmcp": map[string]interface{}{"mail_accounts": []string{"Bob"}}},
	}))
	if args, ok := findEvent(ui, "onProjectError"); ok {
		t.Fatalf("create emitted an error: %v", args)
	}
	projects := store.Get().Projects
	if len(projects) != 1 {
		t.Fatalf("want 1 project, got %d", len(projects))
	}
	id := projects[0].ID
	if projects[0].Access["macmcp"] != AccessRead || len(projects[0].AllowedTools["macmcp"]) != 1 {
		t.Fatalf("create did not persist the permission set: %#v", projects[0])
	}

	ipcUpdateProject(ctx, mustJSON(t, map[string]interface{}{
		"id":      id,
		"context": map[string]interface{}{"macmcp": map[string]interface{}{"mail_accounts": []string{"Bob"}, "mail_mailboxes": []string{"INBOX"}}},
	}))
	stored, _ := store.Get().findProjectByID(id)
	if !strings.Contains(string(stored.Context["macmcp"]), "INBOX") {
		t.Fatalf("update did not persist the scope: %s", stored.Context["macmcp"])
	}

	// And the same refusal, over IPC, as an error event rather than a 400.
	ipcUpdateProject(ctx, mustJSON(t, map[string]interface{}{
		"id":      id,
		"context": map[string]interface{}{"macmcp": map[string]interface{}{"mail_folders": []string{"x"}}},
	}))
	args, ok := findEvent(ui, "onProjectError")
	if !ok {
		t.Fatal("an undeclared scope field was accepted over IPC")
	}
	if len(args) == 0 || !strings.Contains(fmt.Sprint(args[0]), "mail_folders") {
		t.Errorf("IPC refusal does not name the field: %v", args)
	}
	stored, _ = store.Get().findProjectByID(id)
	if !strings.Contains(string(stored.Context["macmcp"]), "INBOX") {
		t.Errorf("a refused IPC patch mutated the record: %s", stored.Context["macmcp"])
	}
}
