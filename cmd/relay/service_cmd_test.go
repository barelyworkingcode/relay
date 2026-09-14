package main

// register, unregister and restart are brokered (ADR-017 decision 2), so a
// test exercising them needs a real bridge server behind a wired ServiceOps
// — newBrokerRouter + serveBroker give it one, over the same store the
// assertions read back from afterward.

import (
	"slices"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func newCLISandboxStore(t *testing.T) config.SettingsStore {
	t.Helper()
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	return store
}

func TestServiceRegister_CapabilityFlagsSetTheCapabilitySet(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	serviceRegister([]string{
		"--name", "Scheduler",
		"--command", "/usr/bin/true",
		"--capability", "frontend",
		"--capability", "manifest",
		"--capability", "frontend",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want exactly 1 registered service, got %d", len(svcs))
	}
	want := []config.ServiceCapability{config.ServiceCapabilityFrontend, config.ServiceCapabilityManifest}
	if !slices.Equal(svcs[0].Capabilities, want) {
		t.Errorf("capabilities = %v, want %v", svcs[0].Capabilities, want)
	}
}

func TestServiceRegister_NoCapabilityFlagGrantsNone(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	serviceRegister([]string{
		"--name", "Bare Svc",
		"--command", "/usr/bin/true",
		"--autostart",
		"--url", "http://127.0.0.1:9000",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want exactly 1 registered service, got %d", len(svcs))
	}
	cfg := svcs[0]
	if cfg.Capabilities == nil || len(cfg.Capabilities) != 0 {
		t.Errorf("capabilities = %#v, want an explicit empty set", cfg.Capabilities)
	}
	if !cfg.Autostart || cfg.URL != "http://127.0.0.1:9000" {
		t.Errorf("other flags not applied: %+v", cfg)
	}
}

// Refused before requireService is reached; the subprocess harness
// (cli_subprocess_test.go) lets this test see exitError's real os.Exit(1).
func TestServiceRegister_AnUnknownCapabilityIsRefused(t *testing.T) {
	out, code := runCLISubprocess(t, mkShortTempDir(t, "relay-refuse-"),
		"service", "register", "--name", "x", "--command", "/bin/true", "--capability", "admin",
	)
	if code == 0 {
		t.Fatalf("expected non-zero exit, output:\n%s", out)
	}
	if !strings.Contains(out, "unknown capability") {
		t.Errorf("refusal does not name the unknown capability: %q", out)
	}
}

// The flags that chose between two fixed identities no longer exist.
func TestServiceRegister_TheRemovedFrontendCredsFlagsAreRefused(t *testing.T) {
	for _, flag := range []string{"--frontend-creds", "--no-frontend-creds"} {
		out, code := runCLISubprocess(t, mkShortTempDir(t, "relay-refuse-"),
			"service", "register", "--name", "x", "--command", "/bin/true", flag,
		)
		if code == 0 {
			t.Fatalf("%s was accepted, output:\n%s", flag, out)
		}
	}
}

func TestCapabilitiesColumn(t *testing.T) {
	if got := capabilitiesColumn(nil); got != "none" {
		t.Errorf("nil = %q, want none", got)
	}
	if got := capabilitiesColumn([]config.ServiceCapability{config.ServiceCapabilityFrontend, config.ServiceCapabilityManifest}); got != "frontend,manifest" {
		t.Errorf("got %q", got)
	}
}
