package main

import "github.com/barelyworkingcode/relay/internal/config"

// register, unregister and restart are brokered (ADR-017 decision 2), so a
// test exercising them needs a real bridge server behind a wired ServiceOps
// — newBrokerRouter + serveBroker give it one, over the same store the
// assertions read back from afterward.

import "testing"

func newCLISandboxStore(t *testing.T) config.SettingsStore {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	return store
}

func TestServiceRegister_NoFrontendCredsSetsOptOut(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

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
	serveBroker(t, newBrokerRouter(t, store, nil))

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
	// Absent flag → nil, so a re-register leaves it untouched and the
	// default (inject) applies.
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
