package service

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func TestEnsureBuiltinRelaySessionsService_AddsOneIfAbsent(t *testing.T) {
	out := EnsureBuiltinRelaySessionsService(nil, "/Applications/Relay.app/Contents/MacOS/relay", "/cfg")
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	svc := out[0]
	if svc.ID != config.RelaySessionsServiceID {
		t.Fatalf("ID = %q, want %q", svc.ID, config.RelaySessionsServiceID)
	}
	if svc.Command != "/Applications/Relay.app/Contents/Helpers/relay-sessions" {
		t.Fatalf("Command = %q, want the Helpers path", svc.Command)
	}
	if !svc.Autostart {
		t.Fatal("a settings.json with no prior record should default autostart true")
	}
	if !svc.HasCapability(config.ServiceCapabilityManifest) || !svc.HasCapability(config.ServiceCapabilitySessions) {
		t.Fatalf("capabilities = %v, want manifest+sessions", svc.Capabilities)
	}
}

func TestEnsureBuiltinRelaySessionsService_PreservesAutostartAndDropsStoredFields(t *testing.T) {
	stored := []config.ServiceConfig{
		{ID: "other", DisplayName: "keep me", Command: "/bin/keep"},
		{ID: config.RelaySessionsServiceID, DisplayName: "stale", Command: "/tmp/evil", Autostart: false},
	}
	out := EnsureBuiltinRelaySessionsService(stored, "/Applications/Relay.app/Contents/MacOS/relay", "/cfg")
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	var found bool
	for _, svc := range out {
		if svc.ID != config.RelaySessionsServiceID {
			continue
		}
		found = true
		if svc.Autostart {
			t.Fatal("the stored autostart=false was not carried forward")
		}
		if svc.Command == "/tmp/evil" {
			t.Fatal("the stored command was carried forward instead of synthesized")
		}
	}
	if !found {
		t.Fatal("the built-in record was dropped entirely")
	}
	if out[0].ID != "other" || out[0].Command != "/bin/keep" {
		t.Fatalf("an unrelated service was disturbed: %+v", out[0])
	}
}
