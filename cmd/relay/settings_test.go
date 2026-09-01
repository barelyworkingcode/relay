package main

import (
	"encoding/json"
	"github.com/barelyworkingcode/relay/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestSettings(t *testing.T, mcps []config.ExternalMcp) *config.Settings {
	t.Helper()
	if mcps == nil {
		mcps = []config.ExternalMcp{}
	}
	return &config.Settings{
		Version:      1,
		ExternalMcps: mcps,
		Services:     []config.ServiceConfig{},
	}
}

func TestAddExternalMcp(t *testing.T) {
	t.Run("adds MCP to slice", func(t *testing.T) {
		s := newTestSettings(t, nil)

		mcp := config.ExternalMcp{ID: "mcp1", DisplayName: "Test MCP"}
		s.AddExternalMcp(mcp)

		if len(s.ExternalMcps) != 1 {
			t.Fatalf("expected 1 MCP, got %d", len(s.ExternalMcps))
		}
		if s.ExternalMcps[0].ID != "mcp1" {
			t.Fatalf("expected ID 'mcp1', got %q", s.ExternalMcps[0].ID)
		}
	})
}

func TestRemoveExternalMcp(t *testing.T) {
	t.Run("removes MCP from slice", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddExternalMcp(config.ExternalMcp{ID: "mcp1", DisplayName: "Test MCP"})

		s.RemoveExternalMcp("mcp1")

		if len(s.ExternalMcps) != 0 {
			t.Fatalf("expected 0 MCPs, got %d", len(s.ExternalMcps))
		}
	})

	t.Run("no-op for unknown ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddExternalMcp(config.ExternalMcp{ID: "keep"})
		s.RemoveExternalMcp("nonexistent")
		if len(s.ExternalMcps) != 1 {
			t.Fatalf("expected 1 MCP, got %d", len(s.ExternalMcps))
		}
	})
}

func TestUpdateExternalMcp(t *testing.T) {
	t.Run("replaces config by ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddExternalMcp(config.ExternalMcp{
			ID:          "mcp1",
			DisplayName: "Old",
			Command:     "/old",
		})

		s.UpdateExternalMcp(config.ExternalMcp{
			ID:          "mcp1",
			DisplayName: "New",
			Command:     "/new",
		})

		mcp, _ := config.FindExternalMcpByID(s, "mcp1")
		if mcp == nil {
			t.Fatal("MCP should still exist after update")
		}
		if mcp.DisplayName != "New" {
			t.Fatalf("expected DisplayName 'New', got %q", mcp.DisplayName)
		}
		if mcp.Command != "/new" {
			t.Fatalf("expected Command '/new', got %q", mcp.Command)
		}
	})

	t.Run("no-op for unknown ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.UpdateExternalMcp(config.ExternalMcp{ID: "nonexistent", DisplayName: "Ghost"})
	})
}

func TestFindMcpByID(t *testing.T) {
	t.Run("finds existing MCP", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddExternalMcp(config.ExternalMcp{ID: "mcp1", DisplayName: "First"})
		s.AddExternalMcp(config.ExternalMcp{ID: "mcp2", DisplayName: "Second"})

		mcp, idx := config.FindExternalMcpByID(s, "mcp2")
		if mcp == nil {
			t.Fatal("expected to find MCP")
		}
		if mcp.DisplayName != "Second" {
			t.Fatalf("expected 'Second', got %q", mcp.DisplayName)
		}
		if idx != 1 {
			t.Fatalf("expected index 1, got %d", idx)
		}
	})

	t.Run("returns nil for missing ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		mcp, idx := config.FindExternalMcpByID(s, "nope")
		if mcp != nil {
			t.Fatal("expected nil for missing ID")
		}
		if idx != -1 {
			t.Fatalf("expected index -1, got %d", idx)
		}
	})
}

func TestFindServiceByID(t *testing.T) {
	t.Run("finds existing service", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddService(config.ServiceConfig{ID: "svc1", DisplayName: "Alpha"})
		s.AddService(config.ServiceConfig{ID: "svc2", DisplayName: "Beta"})

		svc, idx := config.FindServiceByID(s, "svc1")
		if svc == nil {
			t.Fatal("expected to find service")
		}
		if svc.DisplayName != "Alpha" {
			t.Fatalf("expected 'Alpha', got %q", svc.DisplayName)
		}
		if idx != 0 {
			t.Fatalf("expected index 0, got %d", idx)
		}
	})

	t.Run("returns nil for missing ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		svc, idx := config.FindServiceByID(s, "nope")
		if svc != nil {
			t.Fatal("expected nil for missing ID")
		}
		if idx != -1 {
			t.Fatalf("expected index -1, got %d", idx)
		}
	})
}

func TestAddRemoveService(t *testing.T) {
	t.Run("add and remove", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddService(config.ServiceConfig{ID: "svc1", DisplayName: "One"})
		s.AddService(config.ServiceConfig{ID: "svc2", DisplayName: "Two"})

		if len(s.Services) != 2 {
			t.Fatalf("expected 2 services, got %d", len(s.Services))
		}

		s.RemoveService("svc1")
		if len(s.Services) != 1 {
			t.Fatalf("expected 1 service, got %d", len(s.Services))
		}
		if s.Services[0].ID != "svc2" {
			t.Fatal("wrong service was removed")
		}
	})
}

func TestUpdateService(t *testing.T) {
	t.Run("updates existing service", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.AddService(config.ServiceConfig{ID: "svc1", DisplayName: "Old", Command: "/old"})
		s.UpdateService(config.ServiceConfig{ID: "svc1", DisplayName: "New", Command: "/new"})

		svc, _ := config.FindServiceByID(s, "svc1")
		if svc.DisplayName != "New" {
			t.Fatalf("expected 'New', got %q", svc.DisplayName)
		}
		if svc.Command != "/new" {
			t.Fatalf("expected '/new', got %q", svc.Command)
		}
	})

	t.Run("no-op for unknown ID", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.UpdateService(config.ServiceConfig{ID: "nonexistent"})
	})
}

func TestUpdateOAuthState(t *testing.T) {
	s := newTestSettings(t, nil)
	s.AddExternalMcp(config.ExternalMcp{ID: "mcp1", Transport: "http", URL: "https://example.com"})

	oauth := &config.OAuthState{ClientID: "cid", AccessToken: config.NewSecret("at")}
	s.UpdateOAuthState("mcp1", oauth)

	mcp, _ := config.FindExternalMcpByID(s, "mcp1")
	if mcp.OAuthState == nil {
		t.Fatal("OAuthState should be set")
	}
	if mcp.OAuthState.ClientID != "cid" {
		t.Fatalf("expected ClientID 'cid', got %q", mcp.OAuthState.ClientID)
	}

	s.UpdateOAuthState("nonexistent", oauth)
}

func TestAllExternalMcpIDs(t *testing.T) {
	s := newTestSettings(t, nil)
	s.AddExternalMcp(config.ExternalMcp{ID: "b"})
	s.AddExternalMcp(config.ExternalMcp{ID: "a"})
	s.AddExternalMcp(config.ExternalMcp{ID: "c"})

	ids := s.AllExternalMcpIDs()
	if len(ids) != 3 {
		t.Fatalf("expected 3 IDs, got %d", len(ids))
	}
	expected := []string{"b", "a", "c"}
	for i, want := range expected {
		if ids[i] != want {
			t.Fatalf("ids[%d] = %q, want %q", i, ids[i], want)
		}
	}
}

func TestIsHTTP(t *testing.T) {
	t.Run("http transport", func(t *testing.T) {
		m := &config.ExternalMcp{Transport: "http"}
		if !m.IsHTTP() {
			t.Fatal("expected true for http transport")
		}
	})
	t.Run("stdio transport", func(t *testing.T) {
		m := &config.ExternalMcp{Transport: "stdio"}
		if m.IsHTTP() {
			t.Fatal("expected false for stdio transport")
		}
	})
	t.Run("empty transport", func(t *testing.T) {
		m := &config.ExternalMcp{}
		if m.IsHTTP() {
			t.Fatal("expected false for empty transport")
		}
	})
}

func TestDefaultSettings(t *testing.T) {
	s := config.DefaultSettings()
	if s.Version != 1 {
		t.Fatalf("expected version 1, got %d", s.Version)
	}
	if s.ExternalMcps == nil || len(s.ExternalMcps) != 0 {
		t.Fatal("ExternalMcps should be non-nil empty slice")
	}
	if s.Services == nil || len(s.Services) != 0 {
		t.Fatal("Services should be non-nil empty slice")
	}
	if s.Projects == nil || len(s.Projects) != 0 {
		t.Fatal("Projects should be non-nil empty slice")
	}
}

func TestSettingsCache(t *testing.T) {
	newStore := func(t *testing.T) (*config.FileSettingsStore, string) {
		t.Helper()
		dir := t.TempDir()
		return sealedSettingsStoreAt(dir), filepath.Join(dir, "settings.json")
	}

	t.Run("Get returns defaults when no file and no cache", func(t *testing.T) {
		store, _ := newStore(t)

		s := store.Get()
		if s == nil {
			t.Fatal("Get should never return nil")
		}
		if s.Version != 1 {
			t.Fatalf("expected version 1, got %d", s.Version)
		}
	})

	t.Run("Get returns distinct snapshots on each call", func(t *testing.T) {
		store, _ := newStore(t)

		s1 := store.Get()
		s2 := store.Get()
		if s1 == s2 {
			t.Fatal("Get should return distinct snapshot pointers")
		}
		if s1.Version != s2.Version {
			t.Fatal("snapshots should be structurally equal")
		}
	})

	t.Run("With writes to disk and updates cache", func(t *testing.T) {
		store, sp := newStore(t)

		err := store.With(func(s *config.Settings) {
			s.ExternalMcps = append(s.ExternalMcps, config.ExternalMcp{
				ID:          "cache-test-mcp",
				DisplayName: "Cache Test",
			})
		})
		if err != nil {
			t.Fatalf("With failed: %v", err)
		}

		s := store.Get()
		found := false
		for _, mcp := range s.ExternalMcps {
			if mcp.ID == "cache-test-mcp" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("cached settings should contain MCP added by With")
		}

		data, err := os.ReadFile(sp)
		if err != nil {
			t.Fatalf("failed to read settings from disk: %v", err)
		}
		var diskSettings config.Settings
		if err := json.Unmarshal(data, &diskSettings); err != nil {
			t.Fatalf("failed to parse settings from disk: %v", err)
		}
		found = false
		for _, mcp := range diskSettings.ExternalMcps {
			if mcp.ID == "cache-test-mcp" {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("disk settings should contain MCP added by With")
		}
	})

	t.Run("Reload refreshes cache from disk", func(t *testing.T) {
		store, sp := newStore(t)

		err := store.With(func(s *config.Settings) {
			s.ExternalMcps = []config.ExternalMcp{{ID: "original", DisplayName: "Original"}}
		})
		if err != nil {
			t.Fatalf("With failed: %v", err)
		}

		modified := config.Settings{
			Version:      1,
			ExternalMcps: []config.ExternalMcp{{ID: "disk-written", DisplayName: "Disk"}},
			Services:     []config.ServiceConfig{},
		}
		if err := config.SealAllSecrets(&modified, testSealer()); err != nil {
			t.Fatalf("seal: %v", err)
		}
		data, err2 := json.Marshal(modified)
		if err2 != nil {
			t.Fatalf("marshal: %v", err2)
		}
		if err := os.WriteFile(sp, data, 0600); err != nil {
			t.Fatalf("failed to write modified settings: %v", err)
		}

		s := store.Get()
		if len(s.ExternalMcps) == 0 || s.ExternalMcps[0].ID != "original" {
			t.Fatal("Get should return cached (stale) value")
		}

		s = store.Reload()
		if len(s.ExternalMcps) == 0 || s.ExternalMcps[0].ID != "disk-written" {
			t.Fatalf("Reload should return fresh disk data, got %+v", s.ExternalMcps)
		}

		s = store.Get()
		if len(s.ExternalMcps) == 0 || s.ExternalMcps[0].ID != "disk-written" {
			t.Fatal("Get after Reload should return refreshed data")
		}
	})

	t.Run("EnsureInitialized generates AdminSecret if missing", func(t *testing.T) {
		store, _ := newStore(t)

		err := store.EnsureInitialized()
		if err != nil {
			t.Fatalf("EnsureInitialized failed: %v", err)
		}

		s := store.Get()
		pt, ok := s.AdminSecret.Reveal()
		if !ok || pt == "" {
			t.Fatal("AdminSecret should be auto-generated by EnsureInitialized")
		}
		if len(pt) != 32 {
			t.Fatalf("AdminSecret should be 32 hex chars, got %d", len(pt))
		}
	})

	t.Run("With does not generate AdminSecret", func(t *testing.T) {
		store, _ := newStore(t)

		err := store.With(func(s *config.Settings) {})
		if err != nil {
			t.Fatalf("With failed: %v", err)
		}

		s := store.Get()
		if pt, ok := s.AdminSecret.Reveal(); ok && pt != "" {
			t.Fatal("With should not generate AdminSecret; that is EnsureInitialized's job")
		}
	})
}

func TestSettingsClone_MapIsolation(t *testing.T) {
	original := &config.Settings{
		Version: 1,
		ExternalMcps: []config.ExternalMcp{{
			ID:          "mcp-a",
			DisplayName: "A",
			Env:         secretMapFromPlain(map[string]string{"FOO": "bar"}),
		}},
		Services: []config.ServiceConfig{{
			ID:  "svc-a",
			Env: secretMapFromPlain(map[string]string{"BAZ": "qux"}),
		}},
	}

	cp := original.Clone()

	cp.ExternalMcps[0].Env["FOO"] = config.NewSecret("changed")
	cp.ExternalMcps[0].Env["NEW"] = config.NewSecret("added")
	cp.Services[0].Env["BAZ"] = config.NewSecret("changed")

	if got, _ := original.ExternalMcps[0].Env["FOO"].Reveal(); got != "bar" {
		t.Fatal("original ExternalMcp Env was corrupted")
	}
	if _, ok := original.ExternalMcps[0].Env["NEW"]; ok {
		t.Fatal("original ExternalMcp Env has unexpected key")
	}
	if got, _ := original.Services[0].Env["BAZ"].Reveal(); got != "qux" {
		t.Fatal("original Service Env was corrupted")
	}
}

func TestSettingsClone_AllFieldsCovered(t *testing.T) {
	original := &config.Settings{
		Version: 1,
		ExternalMcps: []config.ExternalMcp{{
			ID:  "mcp1",
			Env: secretMapFromPlain(map[string]string{"K": "V"}),
		}},
		Services: []config.ServiceConfig{{
			ID:  "svc1",
			Env: secretMapFromPlain(map[string]string{"A": "B"}),
		}},
	}

	cp := original.Clone()

	checkSliceCopy(t, "ExternalMcps", original.ExternalMcps, cp.ExternalMcps)
	checkSliceCopy(t, "Services", original.Services, cp.Services)

	checkMapCopy(t, "ExternalMcps[0].Env", original.ExternalMcps[0].Env, cp.ExternalMcps[0].Env)

	checkMapCopy(t, "Services[0].Env", original.Services[0].Env, cp.Services[0].Env)
}

func checkSliceCopy[T any](t *testing.T, name string, orig, cp []T) {
	t.Helper()
	if len(orig) == 0 {
		return
	}
	if &orig[0] == &cp[0] {
		t.Errorf("Clone: %s shares backing array with original", name)
	}
}

func checkMapCopy[K comparable, V any](t *testing.T, name string, orig, cp map[K]V) {
	t.Helper()
	if orig == nil {
		return
	}
	origLen := len(orig)
	var zeroK K
	for k := range cp {
		zeroK = k
		break
	}
	delete(cp, zeroK)
	if len(orig) != origLen {
		t.Errorf("Clone: %s shares map with original", name)
	}
	var zeroV V
	cp[zeroK] = zeroV
}

func TestUpsertExternalMcp(t *testing.T) {
	t.Run("inserts new MCP and returns false", func(t *testing.T) {
		s := newTestSettings(t, nil)
		cfg := config.ExternalMcp{ID: "new-mcp", DisplayName: "New"}
		updated := s.UpsertExternalMcp(cfg)
		if updated {
			t.Fatal("expected insert (false), got update (true)")
		}
		if len(s.ExternalMcps) != 1 || s.ExternalMcps[0].ID != "new-mcp" {
			t.Fatal("MCP not added")
		}
	})

	t.Run("updates existing MCP and returns true", func(t *testing.T) {
		s := newTestSettings(t, []config.ExternalMcp{
			{ID: "mcp1", DisplayName: "Old", Command: "old-cmd"},
		})
		cfg := config.ExternalMcp{ID: "mcp1", DisplayName: "Updated", Command: "new-cmd"}
		updated := s.UpsertExternalMcp(cfg)
		if !updated {
			t.Fatal("expected update (true), got insert (false)")
		}
		if len(s.ExternalMcps) != 1 {
			t.Fatal("should still have 1 MCP")
		}
		if s.ExternalMcps[0].Command != "new-cmd" {
			t.Fatal("command not updated")
		}
	})
}

func TestUpsertService(t *testing.T) {
	t.Run("inserts new service and returns false", func(t *testing.T) {
		s := newTestSettings(t, nil)
		cfg := config.ServiceConfig{ID: "svc1", DisplayName: "Svc 1", Command: "cmd"}
		updated := s.UpsertService(cfg)
		if updated {
			t.Fatal("expected insert (false), got update (true)")
		}
		if len(s.Services) != 1 || s.Services[0].ID != "svc1" {
			t.Fatal("service not added")
		}
	})

	t.Run("updates existing service and returns true", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.Services = []config.ServiceConfig{{ID: "svc1", DisplayName: "Old", Command: "old-cmd"}}
		cfg := config.ServiceConfig{ID: "svc1", DisplayName: "New", Command: "new-cmd"}
		updated := s.UpsertService(cfg)
		if !updated {
			t.Fatal("expected update (true), got insert (false)")
		}
		if len(s.Services) != 1 {
			t.Fatal("should still have 1 service")
		}
		if s.Services[0].Command != "new-cmd" {
			t.Fatal("command not updated")
		}
	})
}

func TestMergeServiceDefaults(t *testing.T) {
	t.Run("fills zero-value fields from existing", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.Services = []config.ServiceConfig{{
			ID:         "svc1",
			Command:    "cmd",
			Args:       []string{"--flag"},
			Env:        secretMapFromPlain(map[string]string{"K": "V"}),
			WorkingDir: "/old/dir",
			URL:        "http://old",
		}}
		cfg := config.ServiceConfig{ID: "svc1", Command: "new-cmd"}
		s.MergeServiceDefaults(&cfg)
		if cfg.Command != "new-cmd" {
			t.Fatal("should not overwrite non-zero Command")
		}
		if len(cfg.Args) != 1 || cfg.Args[0] != "--flag" {
			t.Fatal("should inherit Args")
		}
		if got, _ := cfg.Env["K"].Reveal(); got != "V" {
			t.Fatal("should inherit Env")
		}
		if cfg.WorkingDir != "/old/dir" {
			t.Fatal("should inherit WorkingDir")
		}
		if cfg.URL != "http://old" {
			t.Fatal("should inherit URL")
		}
	})

	t.Run("does not overwrite non-zero fields", func(t *testing.T) {
		s := newTestSettings(t, nil)
		s.Services = []config.ServiceConfig{{
			ID:         "svc1",
			Args:       []string{"--old"},
			Env:        secretMapFromPlain(map[string]string{"OLD": "1"}),
			WorkingDir: "/old",
			URL:        "http://old",
		}}
		cfg := config.ServiceConfig{
			ID:         "svc1",
			Args:       []string{"--new"},
			Env:        secretMapFromPlain(map[string]string{"NEW": "2"}),
			WorkingDir: "/new",
			URL:        "http://new",
		}
		s.MergeServiceDefaults(&cfg)
		if cfg.Args[0] != "--new" {
			t.Fatal("should keep caller's Args")
		}
		if got, _ := cfg.Env["NEW"].Reveal(); got != "2" {
			t.Fatal("should keep caller's Env")
		}
		if cfg.WorkingDir != "/new" {
			t.Fatal("should keep caller's WorkingDir")
		}
		if cfg.URL != "http://new" {
			t.Fatal("should keep caller's URL")
		}
	})

	t.Run("no-op for unknown service", func(t *testing.T) {
		s := newTestSettings(t, nil)
		cfg := config.ServiceConfig{ID: "missing", Command: "cmd"}
		s.MergeServiceDefaults(&cfg)
		if cfg.Command != "cmd" {
			t.Fatal("should not mutate when service not found")
		}
	})
}

func TestResolveMcpID(t *testing.T) {
	s := newTestSettings(t, []config.ExternalMcp{
		{ID: "mcp1", DisplayName: "My MCP"},
		{ID: "mcp2", DisplayName: "Other MCP"},
	})

	t.Run("finds by id", func(t *testing.T) {
		if s.ResolveMcpID("mcp1", "") != "mcp1" {
			t.Fatal("should find by id")
		}
	})

	t.Run("finds by name", func(t *testing.T) {
		if s.ResolveMcpID("", "Other MCP") != "mcp2" {
			t.Fatal("should find by display name")
		}
	})

	t.Run("returns empty for unknown id", func(t *testing.T) {
		if s.ResolveMcpID("nope", "") != "" {
			t.Fatal("should return empty for unknown id")
		}
	})

	t.Run("returns empty for unknown name", func(t *testing.T) {
		if s.ResolveMcpID("", "Nope") != "" {
			t.Fatal("should return empty for unknown name")
		}
	})

	t.Run("id takes precedence over name", func(t *testing.T) {
		if s.ResolveMcpID("mcp1", "Other MCP") != "mcp1" {
			t.Fatal("id should take precedence")
		}
	})
}

func TestResolveServiceID(t *testing.T) {
	s := newTestSettings(t, nil)
	s.Services = []config.ServiceConfig{
		{ID: "svc1", DisplayName: "My Service"},
		{ID: "svc2", DisplayName: "Other Service"},
	}

	t.Run("finds by id", func(t *testing.T) {
		if s.ResolveServiceID("svc1", "") != "svc1" {
			t.Fatal("should find by id")
		}
	})

	t.Run("finds by name", func(t *testing.T) {
		if s.ResolveServiceID("", "Other Service") != "svc2" {
			t.Fatal("should find by display name")
		}
	})

	t.Run("returns empty for unknown id", func(t *testing.T) {
		if s.ResolveServiceID("nope", "") != "" {
			t.Fatal("should return empty for unknown id")
		}
	})

	t.Run("returns empty for unknown name", func(t *testing.T) {
		if s.ResolveServiceID("", "Nope") != "" {
			t.Fatal("should return empty for unknown name")
		}
	})
}

// schemaHasField decides whether an MCP is filesystem-scoped, and a false
// negative fails OPEN: the grant is permitted, the second defence then declines
// to derive allowed_dirs for a remote project, the MCP receives nothing, and an
// MCP that reads an absent allowlist as "unrestricted" hands a client on another
// machine the whole host filesystem. So the answer must not depend on which of
// two equivalent spellings an MCP chose.
func TestSchemaHasField_DetectsBothSchemaShapes(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		field  string
		want   bool
	}{
		{"flat, as fsMCP declares it", `{"allowed_dirs":{"type":"array"}}`, "allowed_dirs", true},
		{"nested under properties, the ordinary JSON-Schema shape",
			`{"type":"object","properties":{"allowed_dirs":{"type":"array"}}}`, "allowed_dirs", true},
		{"nested, field genuinely absent",
			`{"type":"object","properties":{"allowed_mailboxes":{"type":"array"}}}`, "allowed_dirs", false},
		{"flat, field genuinely absent", `{"allowed_mailboxes":{"type":"array"}}`, "allowed_dirs", false},
		{"a field literally named properties still matches flat first",
			`{"properties":{"type":"array"}}`, "properties", true},
		{"properties present but not an object", `{"properties":"nonsense"}`, "allowed_dirs", false},
		{"empty schema", ``, "allowed_dirs", false},
		{"malformed json", `{not valid`, "allowed_dirs", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := schemaHasField(json.RawMessage(tc.schema), tc.field); got != tc.want {
				t.Errorf("schemaHasField(%s, %q) = %v, want %v", tc.schema, tc.field, got, tc.want)
			}
		})
	}
}

// The same filesystem-scoped MCP must be refused a remote grant regardless of
// how it spelled its schema — the exact outcome ADR-009 decision 3 exists to
// prevent.
func TestValidateProjectGrants_RefusesFilesystemMcpInEitherSchemaShape(t *testing.T) {
	shapes := map[string]string{
		"flat":   `{"allowed_dirs":{"type":"array"}}`,
		"nested": `{"type":"object","properties":{"allowed_dirs":{"type":"array"}}}`,
	}
	for name, schema := range shapes {
		t.Run(name, func(t *testing.T) {
			s := &config.Settings{Projects: []config.Project{{
				ID: "p1", Name: "Remote", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"fsmcp"},
			}}}
			err := validateProjectGrants(&s.Projects[0], McpSurfaces{
				"fsmcp": {Schema: json.RawMessage(schema)},
			})
			if err == nil {
				t.Fatalf("%s schema: remote project was granted a filesystem-scoped MCP", name)
			}
			if !strings.Contains(err.Error(), "fsmcp") {
				t.Errorf("refusal should name the offending MCP, got: %v", err)
			}
		})
	}
}
