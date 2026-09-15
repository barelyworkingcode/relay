package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/presence/presencetest"
)

// TestServiceRegister_AllowedModelFlagsRoundTrip covers the CLI half of the
// required test ("--allowed-model round-trips through settings"): repeated
// --allowed-model flags land on the persisted record in order, and a
// register with none given persists the explicit empty grant (no models),
// matching --capability's own "omitted means none" convention.
func TestServiceRegister_AllowedModelFlagsRoundTrip(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	serviceRegister([]string{
		"--name", "TTS",
		"--command", "/usr/bin/true",
		"--capability", "models",
		"--allowed-model", "vCode",
		"--allowed-model", "omlx/Chat",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want exactly 1 registered service, got %d", len(svcs))
	}
	want := []string{"vCode", "omlx/Chat"}
	if !slices.Equal(svcs[0].AllowedModels, want) {
		t.Errorf("allowed models = %v, want %v", svcs[0].AllowedModels, want)
	}
}

func TestServiceRegister_NoAllowedModelFlagGrantsNone(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	serviceRegister([]string{
		"--name", "TTS",
		"--command", "/usr/bin/true",
		"--capability", "models",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want exactly 1 registered service, got %d", len(svcs))
	}
	if len(svcs[0].AllowedModels) != 0 {
		t.Errorf("allowed models = %#v, want an explicit empty set", svcs[0].AllowedModels)
	}
}

// TestServiceOps_Update_OmittingAllowedModelsPreservesIt covers the other
// half of the required test: an Update that never mentions AllowedModels
// (the pointer is nil, the wire shape a request that genuinely omits the
// field produces) must not clear a grant a prior register or edit set.
func TestServiceOps_Update_OmittingAllowedModelsPreservesIt(t *testing.T) {
	store := newCLISandboxStore(t)
	r := newBrokerRouter(t, store, nil)

	allowed := []string{"vCode"}
	if _, err := r.serviceOps.Create(context.Background(), serviceFields{
		DisplayName:   "TTS",
		Command:       "/bin/true",
		Capabilities:  &[]config.ServiceCapability{config.ServiceCapabilityModels},
		AllowedModels: &allowed,
	}, auditViaCLI, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Update touches only Command, leaving AllowedModels nil -- the "this
	// request doesn't mention it" shape.
	if _, err := r.serviceOps.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS",
		Command:     "/bin/true2",
	}, auditViaCLI, ""); err != nil {
		t.Fatalf("Update: %v", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	if svc == nil {
		t.Fatal("service vanished after update")
	}
	if !slices.Equal(svc.AllowedModels, allowed) {
		t.Errorf("allowed models after an update omitting the field = %v, want %v", svc.AllowedModels, allowed)
	}
	if svc.Command != "/bin/true2" {
		t.Errorf("command was not updated: %q", svc.Command)
	}
}

// TestServiceOps_Update_AllowedModelsChangeIsPersisted confirms the other
// direction: a request that DOES set AllowedModels replaces the stored
// value, same as any other field this unit's presenceDigest covers.
func TestServiceOps_Update_AllowedModelsChangeIsPersisted(t *testing.T) {
	store := newCLISandboxStore(t)
	r := newBrokerRouter(t, store, nil)

	initial := []string{"vCode"}
	if _, err := r.serviceOps.Create(context.Background(), serviceFields{
		DisplayName:   "TTS",
		Command:       "/bin/true",
		AllowedModels: &initial,
	}, auditViaCLI, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	narrowed := []string{"*"}
	if _, err := r.serviceOps.Update(context.Background(), "tts", serviceFields{
		DisplayName:   "TTS",
		Command:       "/bin/true",
		AllowedModels: &narrowed,
	}, auditViaCLI, ""); err != nil {
		t.Fatalf("Update: %v", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	if svc == nil || !slices.Equal(svc.AllowedModels, narrowed) {
		t.Fatalf("allowed models after an explicit update = %+v, want %v", svc, narrowed)
	}
}

// ---------------------------------------------------------------------------
// Gating: adding a model id, or switching to the wildcard, widens what a
// service holding models may reach and must go through the presence gate,
// the same addition-only reading serviceAddsCapability gives capabilities.
// Removing ids, or resending the exact set, never gates.
// ---------------------------------------------------------------------------

func seedModelsService(t *testing.T, store config.SettingsStore, allowed []string) {
	t.Helper()
	if err := store.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{
			ID: "tts", DisplayName: "TTS", Command: "/bin/tts",
			Capabilities:  []config.ServiceCapability{config.ServiceCapabilityModels},
			AllowedModels: allowed,
		})
	}); err != nil {
		t.Fatalf("seed service: %v", err)
	}
}

func TestServiceOps_RemovingAllowedModelsDoesNotPrompt(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"vCode", "omlx/Chat"})

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	narrowed := []string{"vCode"}
	if _, err := ops.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &narrowed,
	}, auditViaCLI, ""); err != nil {
		t.Fatalf("dropping a model id must not reach the gate: %v", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	if svc == nil || !slices.Equal(svc.AllowedModels, narrowed) {
		t.Fatalf("allowed models = %+v, want %v", svc, narrowed)
	}
}

func TestServiceOps_UnchangedAllowedModelsResendDoesNotPrompt(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"vCode"})

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	same := []string{"vCode"}
	if _, err := ops.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &same,
	}, auditViaCLI, ""); err != nil {
		t.Fatalf("an unchanged resend must not reach the gate: %v", err)
	}
}

func TestServiceOps_AddingAnAllowedModelIsGated(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"vCode"})

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	widened := []string{"vCode", "omlx/Chat"}
	_, err = ops.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &widened,
	}, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("adding a model id: err = %v, want presence.ErrRefused", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	if svc == nil || !slices.Equal(svc.AllowedModels, []string{"vCode"}) {
		t.Fatalf("allowed models changed despite the gate refusing: %+v", svc)
	}
}

func TestServiceOps_AddingAnAllowedModelIsAppliedUnderAnAllowingGate(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"vCode"})

	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	widened := []string{"vCode", "omlx/Chat"}
	if _, err := ops.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &widened,
	}, auditViaCLI, ""); err != nil {
		t.Fatalf("Update under an allowing gate: %v", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	if svc == nil || !slices.Equal(svc.AllowedModels, widened) {
		t.Fatalf("allowed models = %+v, want %v", svc, widened)
	}
}

func TestServiceOps_SwitchingToWildcardIsGated(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"vCode"})

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	wildcard := []string{"*"}
	_, err = ops.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &wildcard,
	}, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("switching to * : err = %v, want presence.ErrRefused", err)
	}
}

// TestServiceOps_ExistingWildcardNeverGatesFurtherAdditions covers the other
// edge of serviceWidensAllowedModels: once a service already holds the
// wildcard, it already reaches every model, so no further addition (another
// id, or a resent "*") can widen it further.
func TestServiceOps_ExistingWildcardNeverGatesFurtherAdditions(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"*"})

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	stillEverything := []string{"*", "vCode"}
	if _, err := ops.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &stillEverything,
	}, auditViaCLI, ""); err != nil {
		t.Fatalf("an addition already covered by an existing wildcard must not reach the gate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validation: an empty or whitespace-only id is refused before persisting,
// and a valid id survives with surrounding whitespace trimmed.
// ---------------------------------------------------------------------------

func TestServiceOps_Create_RefusesEmptyAllowedModelID(t *testing.T) {
	store := newCLISandboxStore(t)
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	bad := []string{"vCode", ""}
	_, err := ops.Create(context.Background(), serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &bad,
	}, auditViaCLI, "")
	if !errors.Is(err, errServiceInvalid) {
		t.Fatalf("err = %v, want errServiceInvalid", err)
	}
	if len(store.Get().Services) != 0 {
		t.Fatalf("a refused create persisted a record: %+v", store.Get().Services)
	}
}

func TestServiceOps_Create_RefusesWhitespaceOnlyAllowedModelID(t *testing.T) {
	store := newCLISandboxStore(t)
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	bad := []string{"   "}
	_, err := ops.Create(context.Background(), serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &bad,
	}, auditViaCLI, "")
	if !errors.Is(err, errServiceInvalid) {
		t.Fatalf("err = %v, want errServiceInvalid", err)
	}
}

func TestServiceOps_Update_RefusesEmptyAllowedModelID(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"vCode"})
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	bad := []string{"vCode", ""}
	_, err := ops.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &bad,
	}, auditViaCLI, "")
	if !errors.Is(err, errServiceInvalid) {
		t.Fatalf("err = %v, want errServiceInvalid", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	if svc == nil || !slices.Equal(svc.AllowedModels, []string{"vCode"}) {
		t.Fatalf("a refused update must leave the stored grant untouched: %+v", svc)
	}
}

func TestServiceOps_Create_TrimsAllowedModelWhitespace(t *testing.T) {
	store := newCLISandboxStore(t)
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	padded := []string{"  vCode  ", "omlx/Chat"}
	if _, err := ops.Create(context.Background(), serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &padded,
	}, auditViaCLI, ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	want := []string{"vCode", "omlx/Chat"}
	if svc == nil || !slices.Equal(svc.AllowedModels, want) {
		t.Fatalf("allowed models = %+v, want %v (trimmed)", svc, want)
	}
}

// TestServiceOps_TrimmingMeansAWhitespaceOnlyResendDoesNotGate confirms the
// widen check trims before comparing: resending an existing id with
// incidental leading/trailing whitespace is the same id, not an addition.
func TestServiceOps_TrimmingMeansAWhitespaceOnlyResendDoesNotGate(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"vCode"})

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	padded := []string{"  vCode  "}
	if _, err := ops.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &padded,
	}, auditViaCLI, ""); err != nil {
		t.Fatalf("a whitespace-padded resend of an existing id must not reach the gate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Unknown capability still refused through IPC, and allowed_models round-trip
// through the IPC doors specifically (ipc_services.go), not just ServiceOps.
// ---------------------------------------------------------------------------

func TestIPCAddService_UnknownCapabilityRefused(t *testing.T) {
	store := newCLISandboxStore(t)
	ipc, ui := newServicesIPC(t, store, &svcRecorder{})

	bogus := []config.ServiceCapability{"not-a-real-capability"}
	ipcAddService(ipc, mustJSON(t, ipcServiceMsg{DisplayName: "X", Command: "/bin/x", Capabilities: &bogus}))

	if len(store.Get().Services) != 0 {
		t.Fatalf("an unknown capability must not persist: %+v", store.Get().Services)
	}
	if msg := lastSettingsError(t, ui); msg == "" {
		t.Error("expected onSettingsError for an unknown capability")
	}
}

func TestIPCUpdateService_OmittingAllowedModelsKeepsStored(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"vCode"})
	ipc, _ := newServicesIPC(t, store, &svcRecorder{})

	ipcUpdateService(ipc, mustJSON(t, ipcServiceMsg{
		ID: "tts", DisplayName: "TTS", Command: "/bin/tts2",
	}))

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	if svc == nil || !slices.Equal(svc.AllowedModels, []string{"vCode"}) {
		t.Fatalf("allowed models after an update omitting the field = %+v, want [vCode]", svc)
	}
	if svc.Command != "/bin/tts2" {
		t.Errorf("command was not updated: %q", svc.Command)
	}
}

func TestIPCUpdateService_SetsAllowedModels(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"vCode"})
	ipc, _ := newServicesIPC(t, store, &svcRecorder{})

	widened := []string{"vCode", "omlx/Chat"}
	ipcUpdateService(ipc, mustJSON(t, ipcServiceMsg{
		ID: "tts", DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &widened,
	}))

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	if svc == nil || !slices.Equal(svc.AllowedModels, widened) {
		t.Fatalf("allowed models = %+v, want %v", svc, widened)
	}
}
