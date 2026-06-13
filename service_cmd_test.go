package main

// CLI-level coverage for `relay service register`, focused on the security-
// relevant --no-frontend-creds plumbing (previously only the frontendCredsEnabled
// predicate was tested, never the CLI flag that sets it). serviceRegister takes
// an injectable SettingsStore, so we drive it against a sandbox store and read
// the persisted config back. The trailing SendReloadService notify fails
// harmlessly (no tray running) via warnNotifyFailure.

import "testing"

func newCLISandboxStore(t *testing.T) SettingsStore {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := NewSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	return store
}

func TestServiceRegister_NoFrontendCredsSetsOptOut(t *testing.T) {
	store := newCLISandboxStore(t)

	serviceRegister(store, []string{
		"--name", "Backend Svc",
		"--command", "/usr/bin/true",
		"--no-frontend-creds",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want exactly 1 registered service, got %d", len(svcs))
	}
	cfg := svcs[0]
	if cfg.FrontendConsumer == nil || *cfg.FrontendConsumer {
		t.Errorf("FrontendConsumer = %v, want explicit false (opted out)", cfg.FrontendConsumer)
	}
	if cfg.Command != "/usr/bin/true" || cfg.DisplayName != "Backend Svc" {
		t.Errorf("config not assembled correctly: %+v", cfg)
	}
}

func TestServiceRegister_DefaultLeavesFrontendCredsUnset(t *testing.T) {
	store := newCLISandboxStore(t)

	serviceRegister(store, []string{
		"--name", "Frontend Svc",
		"--command", "/usr/bin/true",
		"--autostart",
		"--url", "http://127.0.0.1:9000",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want exactly 1 registered service, got %d", len(svcs))
	}
	cfg := svcs[0]
	// Absent flag → nil, so MergeServiceDefaults leaves it untouched on
	// re-register and the default (inject) applies.
	if cfg.FrontendConsumer != nil {
		t.Errorf("FrontendConsumer = %v, want nil when --no-frontend-creds absent", *cfg.FrontendConsumer)
	}
	if !cfg.Autostart {
		t.Error("--autostart not applied")
	}
	if cfg.URL != "http://127.0.0.1:9000" {
		t.Errorf("URL = %q, want http://127.0.0.1:9000", cfg.URL)
	}
}
