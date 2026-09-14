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
  {"id":"bogus","display_name":"bogus","command":"/bin/bogus","args":[],"env":{},"capabilities":["bogus"]}
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
		"eve":   {ServiceCapabilityFrontend},
		"stt":   {ServiceCapabilityFrontend},
		"llm":   {ServiceCapabilityManifest, ServiceCapabilityProjects},
		"sched": {ServiceCapabilityFrontend, ServiceCapabilityManifest},
		"bare":  {},
		"bogus": {"bogus"},
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

func TestServiceConfig_ValidateRefusesAnUnknownCapability(t *testing.T) {
	cfg := ServiceConfig{ID: "svc", DisplayName: "svc", Command: "/bin/x"}
	for _, ok := range [][]ServiceCapability{nil, {}, ServiceCapabilities} {
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
