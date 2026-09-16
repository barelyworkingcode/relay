package main

// Validate-before-persist coverage for ServiceOps.Create/Update: a
// misconfigured service (here, an operator env key that collides with
// relay's own RELAY_* namespace) must never land in settings.json, whether
// it is a brand-new record or an edit to an existing one.

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

func TestServiceOps_Create_RefusesRelayPrefixedEnvBeforePersisting(t *testing.T) {
	store := newCLISandboxStore(t)
	r := newBrokerRouter(t, store, nil)

	_, err := r.serviceOps.Create(context.Background(), serviceFields{
		DisplayName: "Bad Env Svc",
		Command:     "/bin/true",
		Env:         map[string]*string{"RELAY_SERVICE_TOKEN": ptr("poison")},
	}, auditViaCLI, "")
	if err == nil {
		t.Fatal("expected an error for a RELAY_-prefixed env key")
	}
	if len(store.Get().Services) != 0 {
		t.Fatalf("no service should have been persisted, got %+v", store.Get().Services)
	}
}

func TestServiceOps_Create_RefusesUnsafeIDBeforePersisting(t *testing.T) {
	store := newCLISandboxStore(t)
	r := newBrokerRouter(t, store, nil)

	_, err := r.serviceOps.Create(context.Background(), serviceFields{
		ID:          "../etc",
		DisplayName: "Bad ID Svc",
		Command:     "/bin/true",
	}, auditViaCLI, "")
	if err == nil {
		t.Fatal("expected an error for an unsafe ID")
	}
	if len(store.Get().Services) != 0 {
		t.Fatalf("no service should have been persisted, got %+v", store.Get().Services)
	}
}

// TestServiceOps_Create_RefusesTheBuiltinRelaySessionsID pins R-S9: relay
// service register --capability sessions is refused, whichever id it names.
// Naming any OTHER id already fails ServiceConfig.Validate's own capability
// check (config.RelaySessionsServiceID is the only record allowed to hold
// ServiceCapabilitySessions); this closes the one id that check lets
// through, since relay-sessions is built in and never user-registered
// (spec-session-host.md §2.1) regardless of what capability is requested.
func TestServiceOps_Create_RefusesTheBuiltinRelaySessionsID(t *testing.T) {
	store := newCLISandboxStore(t)
	r := newBrokerRouter(t, store, nil)
	sessionsCaps := []config.ServiceCapability{config.ServiceCapabilitySessions}

	_, err := r.serviceOps.Create(context.Background(), serviceFields{
		ID:           config.RelaySessionsServiceID,
		DisplayName:  "Evil Session Host",
		Command:      "/tmp/evil-relay-sessions",
		Capabilities: &sessionsCaps,
	}, auditViaCLI, "")
	if err == nil {
		t.Fatal("expected registering the reserved relaysessions id to be refused")
	}
	if len(store.Get().Services) != 0 {
		t.Fatalf("no service should have been persisted, got %+v", store.Get().Services)
	}
}

// TestServiceOps_Start_RefusesTheBuiltinRelaySessionsID pins the third site
// ServiceConfig.Validate's Command requirement reaches: Registry.Start,
// behind the tray's "Start" button and `relay service start`. Without this
// refusal, starting the bare record EnsureBuiltinRelaySessionsRecord
// persists hits Validate() directly and surfaces "service command is
// required" -- true, but not a caller's fault to decode.
func TestServiceOps_Start_RefusesTheBuiltinRelaySessionsID(t *testing.T) {
	store := newCLISandboxStore(t)
	r := newBrokerRouter(t, store, nil)

	err := r.serviceOps.Start(config.RelaySessionsServiceID)
	if err == nil {
		t.Fatal("expected starting the reserved relaysessions id manually to be refused")
	}
	if !errors.Is(err, errServiceInvalid) {
		t.Fatalf("Start(%q) error = %v, want errServiceInvalid", config.RelaySessionsServiceID, err)
	}
}

// TestServiceOps_Create_CapabilitySessionsRefusedForAnyOtherID is the other
// half: naming ANY id but the reserved one and asking for the sessions
// capability is refused too (ServiceConfig.Validate, exercised here through
// the same door an operator actually uses).
func TestServiceOps_Create_CapabilitySessionsRefusedForAnyOtherID(t *testing.T) {
	store := newCLISandboxStore(t)
	r := newBrokerRouter(t, store, nil)
	sessionsCaps := []config.ServiceCapability{config.ServiceCapabilitySessions}

	_, err := r.serviceOps.Create(context.Background(), serviceFields{
		DisplayName:  "Not The Session Host",
		Command:      "/bin/true",
		Capabilities: &sessionsCaps,
	}, auditViaCLI, "")
	if err == nil {
		t.Fatal("expected the sessions capability to be refused for a non-built-in id")
	}
	if len(store.Get().Services) != 0 {
		t.Fatalf("no service should have been persisted, got %+v", store.Get().Services)
	}
}

func TestServiceOps_Update_RefusesRelayPrefixedEnvBeforePersisting(t *testing.T) {
	store := newCLISandboxStore(t)
	r := newBrokerRouter(t, store, nil)

	if err := store.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{ID: "svc1", DisplayName: "Svc1", Command: "/bin/old"})
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	_, err := r.serviceOps.Update(context.Background(), "svc1", serviceFields{
		DisplayName: "Svc1",
		Command:     "/bin/new",
		Env:         map[string]*string{"RELAY_FRONTEND_TOKEN": ptr("poison")},
	}, auditViaCLI, "")
	if err == nil {
		t.Fatal("expected an error for a RELAY_-prefixed env key")
	}

	svc, _ := config.FindServiceByID(store.Get(), "svc1")
	if svc == nil || svc.Command != "/bin/old" {
		t.Fatalf("the existing record must be untouched by a refused update, got %+v", svc)
	}
}

// ---------------------------------------------------------------------------
// Bug 2: the env wire representation. A key's value is *string on the wire:
// nil ("no new value") keeps whatever is already sealed for that key, a
// non-nil value replaces it, and a key omitted from the map entirely is
// removed (Env's whole-map-replace semantics predate this field).
// ---------------------------------------------------------------------------

// svcEnvSandbox seeds a service with two env values and returns the record
// as the store itself now holds it -- after store.With's own seal pass,
// which populates each Secret's envelope -- not the pre-seal literal this
// function built, so a later `!=` against a freshly re-read Secret compares
// like with like.
func svcEnvSandbox(t *testing.T) (config.SettingsStore, config.ServiceConfig) {
	t.Helper()
	store := newCLISandboxStore(t)
	cfg := config.ServiceConfig{
		ID: "svc", DisplayName: "Svc", Command: "/bin/old",
		Env: map[string]config.Secret{"A": config.NewSecret("secret-a"), "B": config.NewSecret("secret-b")},
	}
	if err := store.With(func(s *config.Settings) { s.UpsertService(cfg) }); err != nil {
		t.Fatalf("seed service: %v", err)
	}
	seeded, _ := config.FindServiceByID(store.Get(), "svc")
	return store, *seeded
}

func TestServiceOps_UpdateEnv_NullKeepsTheStoredValue(t *testing.T) {
	store, seeded := svcEnvSandbox(t)
	beforeA, ok := seeded.Env["A"].Reveal()
	if !ok {
		t.Fatal("seeded value A did not reveal")
	}
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	_, err := ops.Update(context.Background(), "svc", serviceFields{
		DisplayName: "Svc", Command: "/bin/old",
		Env: map[string]*string{"A": nil, "B": ptr("new-b")},
	}, auditViaIPC, "")
	assertNoErr(t, err, "Update")

	after, _ := config.FindServiceByID(store.Get(), "svc")
	if after == nil {
		t.Fatal("service not found")
	}
	// Every settings.json write reseals every Secret with a fresh envelope
	// regardless of whether its plaintext changed (SealAllSecrets' own
	// comment: "always calls Seal fresh"), so the byte-identical check is
	// Reveal()'s plaintext, not the struct or its envelope.
	if pt, ok := after.Env["A"].Reveal(); !ok || pt != beforeA {
		t.Fatalf("A = %q, ok=%v; want %q, true -- an unchanged key must keep its exact stored value", pt, ok, beforeA)
	}
	if pt, ok := after.Env["B"].Reveal(); !ok || pt != "new-b" {
		t.Fatalf("B = %q, ok=%v; want \"new-b\", true", pt, ok)
	}
}

func TestServiceOps_UpdateEnv_OmittedKeyIsRemoved(t *testing.T) {
	store, _ := svcEnvSandbox(t)
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	_, err := ops.Update(context.Background(), "svc", serviceFields{
		DisplayName: "Svc", Command: "/bin/old",
		Env: map[string]*string{"A": nil},
	}, auditViaIPC, "")
	assertNoErr(t, err, "Update")

	after, _ := config.FindServiceByID(store.Get(), "svc")
	if after == nil {
		t.Fatal("service not found")
	}
	if _, ok := after.Env["B"]; ok {
		t.Fatalf("B should have been removed (omitted from the request), got %+v", after.Env)
	}
	if _, ok := after.Env["A"]; !ok {
		t.Fatalf("A should still be present, got %+v", after.Env)
	}
}

func TestServiceOps_UpdateEnv_RefusesSerializedObjectArtifact(t *testing.T) {
	store, seeded := svcEnvSandbox(t)
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	_, err := ops.Update(context.Background(), "svc", serviceFields{
		DisplayName: "Svc", Command: "/bin/old",
		Env: map[string]*string{"A": ptr("[object Object]")},
	}, auditViaIPC, "")
	if !errors.Is(err, errServiceInvalid) {
		t.Fatalf("err = %v, want errServiceInvalid", err)
	}

	after, _ := config.FindServiceByID(store.Get(), "svc")
	if after.Env["A"] != seeded.Env["A"] {
		t.Fatalf("a refused update must leave the stored secret untouched")
	}
}

func TestServiceOps_CreateEnv_RefusesSerializedObjectArtifact(t *testing.T) {
	store := newCLISandboxStore(t)
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	_, err := ops.Create(context.Background(), serviceFields{
		DisplayName: "Bad Env", Command: "/bin/true",
		Env: map[string]*string{"A": ptr("[object Object]")},
	}, auditViaCLI, "")
	if !errors.Is(err, errServiceInvalid) {
		t.Fatalf("err = %v, want errServiceInvalid", err)
	}
	if len(store.Get().Services) != 0 {
		t.Fatalf("a refused create persisted a record: %+v", store.Get().Services)
	}
}

// ---------------------------------------------------------------------------
// Capability gating. Both directions now gate: an earlier revision treated
// dropping a capability as narrow-safe and never prompted for it, which was
// correct while only an operator-minted credential could reach this route.
// Since eve's frontend launch identity was granted execute
// (plan-broker-and-sessions.md's F1 decision), this route is reachable by
// any frontend-capable service for ANY service's record, not just its own —
// a frontend service silently narrowing another service's own capabilities
// (e.g. stripping relay-llm's model_host capability) is a real availability
// exposure that widening a background service's reach introduced. Gating
// narrowing again is the user's own explicit call once that trade-off was
// raised (STATUS-relay-security.md), accepted deliberately over the
// operator-convenience cost the narrow-doesn't-gate rule existed to avoid.
// ---------------------------------------------------------------------------

func TestServiceOps_RemovingACapabilityIsNowGated(t *testing.T) {
	store := newCLISandboxStore(t)
	if err := store.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{
			ID: "relaytts", DisplayName: "relayTTS", Command: "/bin/tts",
			Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilityModels},
		})
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	caps := []config.ServiceCapability{config.ServiceCapabilityManifest}
	_, err = ops.Update(context.Background(), "relaytts", serviceFields{
		DisplayName: "relayTTS", Command: "/bin/tts", Capabilities: &caps,
	}, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("narrowing a service's capabilities: err = %v, want presence.ErrRefused", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "relaytts")
	if svc == nil || !slices.Equal(svc.Capabilities, []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilityModels}) {
		t.Fatalf("a refused update must not persist: capabilities = %+v", svc)
	}
}

func TestServiceOps_AddingACapabilityIsGated(t *testing.T) {
	store := newCLISandboxStore(t)
	if err := store.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{
			ID: "relaytts", DisplayName: "relayTTS", Command: "/bin/tts",
			Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest},
		})
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	caps := []config.ServiceCapability{config.ServiceCapabilityManifest, config.ServiceCapabilityModels}
	_, err = ops.Update(context.Background(), "relaytts", serviceFields{
		DisplayName: "relayTTS", Command: "/bin/tts", Capabilities: &caps,
	}, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("adding a capability: err = %v, want presence.ErrRefused", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "relaytts")
	if svc == nil || !slices.Equal(svc.Capabilities, []config.ServiceCapability{config.ServiceCapabilityManifest}) {
		t.Fatalf("capabilities were changed despite the gate refusing: %+v", svc)
	}
}

// TestServiceOps_UnchangedUpdateDoesNotPrompt is bug 1's precondition,
// mirrored from the same fix applied to ProjectOps.Update: the Settings
// window resends the whole record on every save, and a resend that changes
// nothing at all must not reach the gate either.
func TestServiceOps_UnchangedUpdateDoesNotPrompt(t *testing.T) {
	store := newCLISandboxStore(t)
	if err := store.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{
			ID: "svc", DisplayName: "Svc", Command: "/bin/old", Args: []string{"--flag"},
			WorkingDir: "/tmp", URL: "http://localhost", Autostart: true,
			Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest},
			Env:          map[string]config.Secret{"A": config.NewSecret("secret-a")},
		})
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	caps := []config.ServiceCapability{config.ServiceCapabilityManifest}
	_, err = ops.Update(context.Background(), "svc", serviceFields{
		DisplayName: "Svc", Command: "/bin/old", Args: []string{"--flag"},
		WorkingDir: ptr("/tmp"), URL: ptr("http://localhost"), Autostart: ptr(true),
		Capabilities: &caps,
		Env:          map[string]*string{"A": nil},
	}, auditViaIPC, "")
	if err != nil {
		t.Fatalf("an unchanged resend must not reach the gate: %v", err)
	}
}

// TestServiceOps_RenamingAServiceIsGated closes the gap the F1/SP8 execute
// grant opened: serviceUpdateNeedsGate never inspected display_name at all,
// so any frontend-capable service (not just an operator) could silently
// rename another service's record -- an operator-facing spoofing primitive
// in the tray menu and `service list` output. See
// STATUS-relay-security.md and serviceCapabilitiesChanged's comment for the
// full reasoning.
func TestServiceOps_RenamingAServiceIsGated(t *testing.T) {
	store := newCLISandboxStore(t)
	if err := store.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{
			ID: "relaytts", DisplayName: "relayTTS", Command: "/bin/tts",
		})
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	_, err = ops.Update(context.Background(), "relaytts", serviceFields{
		DisplayName: "Definitely Not Malicious", Command: "/bin/tts",
	}, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("renaming a service: err = %v, want presence.ErrRefused", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "relaytts")
	if svc == nil || svc.DisplayName != "relayTTS" {
		t.Fatalf("a refused rename must not persist: display_name = %+v", svc)
	}
}
