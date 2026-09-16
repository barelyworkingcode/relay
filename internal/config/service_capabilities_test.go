package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const legacyServicesJSON = `[
  {"id":"eve","display_name":"eve","command":"/bin/eve","args":[],"env":{}},
  {"id":"stt","display_name":"stt","command":"/bin/stt","args":[],"env":{},"frontend_consumer":true},
  {"id":"llm","display_name":"llm","command":"/bin/llm","args":[],"env":{},"frontend_consumer":false},
  {"id":"sched","display_name":"sched","command":"/bin/sched","args":[],"env":{},"capabilities":["frontend","manifest"],"frontend_consumer":false},
  {"id":"bare","display_name":"bare","command":"/bin/bare","args":[],"env":{},"capabilities":[]},
  {"id":"bogus","display_name":"bogus","command":"/bin/bogus","args":[],"env":{},"capabilities":["bogus"]},
  {"id":"old-bridge","display_name":"old-bridge","command":"/bin/old-bridge","args":[],"env":{},"capabilities":["manifest","projects"]}
]`

func TestServiceCapabilities_MigrateOnLoadAndNeverWriteTheLegacyField(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	path := filepath.Join(dir, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	doc["services"] = json.RawMessage(legacyServicesJSON)
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write legacy settings.json: %v", err)
	}

	s := store.Reload()
	want := map[string][]ServiceCapability{
		"eve":        {ServiceCapabilityFrontend},
		"stt":        {ServiceCapabilityFrontend},
		"llm":        {ServiceCapabilityManifest},
		"sched":      {ServiceCapabilityFrontend, ServiceCapabilityManifest},
		"bare":       {},
		"bogus":      {"bogus"},
		"old-bridge": {ServiceCapabilityManifest},
	}
	for id, caps := range want {
		svc, _ := FindServiceByID(s, id)
		if svc == nil {
			t.Fatalf("service %q was not loaded", id)
		}
		if !slices.Equal(svc.Capabilities, caps) || svc.Capabilities == nil {
			t.Errorf("%s: capabilities = %#v, want %#v", id, svc.Capabilities, caps)
		}
		if svc.LegacyFrontendConsumer != nil {
			t.Errorf("%s: the legacy field survived the load", id)
		}
	}
	bogus, _ := FindServiceByID(s, "bogus")
	if err := bogus.Validate(); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("a record with an unknown capability validated: %v", err)
	}

	if err := store.With(func(*Settings) {}); err != nil {
		t.Fatalf("With: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "frontend_consumer") {
		t.Fatal("frontend_consumer was written back")
	}
	var after struct {
		Services []map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(written, &after); err != nil {
		t.Fatal(err)
	}
	for _, svc := range after.Services {
		if _, ok := svc["capabilities"]; !ok {
			t.Errorf("service %s was written without capabilities", svc["id"])
		}
	}
	reloaded := store.Reload()
	if bare, _ := FindServiceByID(reloaded, "bare"); bare == nil || len(bare.Capabilities) != 0 {
		t.Fatal("an explicitly empty set was migrated into a grant on the second load")
	}
}

// TestServiceCapabilities_ProjectsIsDroppedNotRefused pins the R-S1
// requirement directly: a record loaded holding the retired "projects"
// capability starts (Validate succeeds) and, on the next write, persists to
// disk without it — never a hard error, since the operations it once named
// (ResolvePtyEnv, ListProjects, GetProject, tokenless service ListTools/
// CallTool) were deleted out from under any existing install that named it.
func TestServiceCapabilities_ProjectsIsDroppedNotRefused(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	path := filepath.Join(dir, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	doc["services"] = json.RawMessage(`[{"id":"old-bridge","display_name":"old-bridge","command":"/bin/old-bridge","args":[],"env":{},"capabilities":["manifest","projects"]}]`)
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write legacy settings.json: %v", err)
	}

	s := store.Reload()
	svc, _ := FindServiceByID(s, "old-bridge")
	if svc == nil {
		t.Fatal("the record with a retired capability was not loaded at all")
	}
	if err := svc.Validate(); err != nil {
		t.Fatalf("a record holding the retired projects capability failed to start: %v", err)
	}
	if !slices.Equal(svc.Capabilities, []ServiceCapability{ServiceCapabilityManifest}) {
		t.Fatalf("capabilities after load = %#v, want [manifest] (projects silently dropped)", svc.Capabilities)
	}

	if err := store.With(func(*Settings) {}); err != nil {
		t.Fatalf("With: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		Services []struct {
			ID           string   `json:"id"`
			Capabilities []string `json:"capabilities"`
		} `json:"services"`
	}
	if err := json.Unmarshal(written, &onDisk); err != nil {
		t.Fatalf("parse written settings.json: %v", err)
	}
	found := false
	for _, svc := range onDisk.Services {
		if svc.ID != "old-bridge" {
			continue
		}
		found = true
		if slices.Contains(svc.Capabilities, "projects") {
			t.Fatalf("the retired capability was written back to disk: %v", svc.Capabilities)
		}
	}
	if !found {
		t.Fatal("old-bridge was not written back at all")
	}
	reloaded := store.Reload()
	if svc, _ := FindServiceByID(reloaded, "old-bridge"); svc == nil || !slices.Equal(svc.Capabilities, []ServiceCapability{ServiceCapabilityManifest}) {
		t.Fatalf("capabilities did not persist across a reload: %+v", svc)
	}
}

func TestServiceConfig_ValidateRefusesAnUnknownCapability(t *testing.T) {
	cfg := ServiceConfig{ID: "svc", DisplayName: "svc", Command: "/bin/x"}
	knownButSessions := []ServiceCapability{
		ServiceCapabilityFrontend, ServiceCapabilityManifest,
		ServiceCapabilityModels, ServiceCapabilityModelHost,
	}
	for _, ok := range [][]ServiceCapability{nil, {}, knownButSessions} {
		cfg.Capabilities = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	cfg.Capabilities = []ServiceCapability{ServiceCapabilityFrontend, "Frontend"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("an unknown capability name validated")
	}
}

// TestServiceConfig_ValidateRestrictsSessionsToTheBuiltinRecord pins the R-S1
// requirement: only the built-in RelaySessionsServiceID record may ever hold
// ServiceCapabilitySessions. R-S9 is the unit that later creates that record;
// this only enforces the rule it will rely on.
func TestServiceConfig_ValidateRestrictsSessionsToTheBuiltinRecord(t *testing.T) {
	other := ServiceConfig{ID: "relaytts", DisplayName: "x", Command: "/bin/x", Capabilities: []ServiceCapability{ServiceCapabilitySessions}}
	if err := other.Validate(); err == nil || !strings.Contains(err.Error(), "sessions") {
		t.Fatalf("a non-built-in service validated holding sessions: %v", err)
	}
	builtin := ServiceConfig{ID: RelaySessionsServiceID, DisplayName: "x", Command: "/bin/x", Capabilities: []ServiceCapability{ServiceCapabilitySessions}}
	if err := builtin.Validate(); err != nil {
		t.Fatalf("the built-in relaysessions record was refused sessions: %v", err)
	}
}

// TestServiceConfig_StoredCommandIgnoredForBuiltinRelaySessions pins R-S9's
// own requirement (spec-session-host.md §2.1): settings.json may hold
// enable/disable and autostart for the built-in relaysessions record, but a
// stored Command (and the Args that go with it) must never reach anything
// that would exec it — internal/service always resolves its own, from
// relay's bundle path, never from disk.
func TestServiceConfig_StoredCommandIgnoredForBuiltinRelaySessions(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	path := filepath.Join(dir, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	doc["services"] = json.RawMessage(`[{
		"id": "` + RelaySessionsServiceID + `",
		"display_name": "Session Host",
		"command": "/tmp/evil-relay-sessions",
		"args": ["--do-something-bad"],
		"autostart": false
	}]`)
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write edited settings.json: %v", err)
	}

	s := store.Reload()
	svc, idx := FindServiceByID(s, RelaySessionsServiceID)
	if idx < 0 || svc == nil {
		t.Fatal("the built-in relaysessions record did not survive load")
	}
	if svc.Command != "" {
		t.Fatalf("stored command was NOT ignored: got %q", svc.Command)
	}
	if len(svc.Args) != 0 {
		t.Fatalf("stored args were NOT ignored: got %v", svc.Args)
	}
	if svc.Autostart {
		t.Fatal("autostart, the one field the operator may set, was not honoured")
	}
}
