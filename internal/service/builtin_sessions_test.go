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

// TestEnsureBuiltinRelaySessionsRecord_AddsBareRecordIfAbsent is RF4's proof
// that a settings.json which has never seen a relaysessions record gets one
// persisted with an off switch a caller can act on -- List and SetAutostart
// (cmd/relay/service_ops.go) both read s.Services directly, so without this
// write neither has anything to find.
func TestEnsureBuiltinRelaySessionsRecord_AddsBareRecordIfAbsent(t *testing.T) {
	s := &config.Settings{}
	EnsureBuiltinRelaySessionsRecord(s)

	svc, _ := config.FindServiceByID(s, config.RelaySessionsServiceID)
	if svc == nil {
		t.Fatal("no relaysessions record was persisted")
	}
	if !svc.Autostart {
		t.Fatal("the persisted record should default autostart true")
	}
	if svc.Command != "" || svc.Args != nil {
		t.Fatalf("the persisted record carries Command/Args it should leave to synthesis: %+v", svc)
	}
	if !svc.HasCapability(config.ServiceCapabilityManifest) || !svc.HasCapability(config.ServiceCapabilitySessions) {
		t.Fatalf("capabilities = %v, want manifest+sessions", svc.Capabilities)
	}
}

// TestEnsureBuiltinRelaySessionsRecord_LeavesAnExistingRecordAlone pins the
// other half of the off switch: once an operator (or a prior tray start) has
// a stored record, EnsureBuiltinRelaySessionsRecord must never touch it
// again, including an explicit autostart=false -- the same "operator's own
// choice is never re-decided" discipline EnsureDefaultModelEndpoint applies
// to model_endpoint.
func TestEnsureBuiltinRelaySessionsRecord_LeavesAnExistingRecordAlone(t *testing.T) {
	s := &config.Settings{Services: []config.ServiceConfig{
		{ID: config.RelaySessionsServiceID, DisplayName: "Session Host", Autostart: false},
	}}
	EnsureBuiltinRelaySessionsRecord(s)

	if len(s.Services) != 1 {
		t.Fatalf("len(s.Services) = %d, want 1", len(s.Services))
	}
	if s.Services[0].Autostart {
		t.Fatal("an operator's own autostart=false was overwritten")
	}
}
