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

// ---------------------------------------------------------------------------
// Gating: adding a model id, switching to the wildcard, or dropping one
// (any actual change to the set) goes through the presence gate. Resending
// the exact set never gates. Narrowing gates too: any frontend-capable
// service can reach this route for any service's record (see
// serviceAllowedModelsChanged's comment in service_ops.go).
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

func TestServiceOps_RemovingAllowedModelsIsNowGated(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"vCode", "omlx/Chat"})

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	narrowed := []string{"vCode"}
	if _, err := ops.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &narrowed,
	}, auditViaCLI, ""); !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("dropping a model id: err = %v, want presence.ErrRefused", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	if svc == nil || !slices.Equal(svc.AllowedModels, []string{"vCode", "omlx/Chat"}) {
		t.Fatalf("a refused update must not persist: allowed models = %+v", svc)
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
	cases := []struct {
		name    string
		widened []string
	}{
		{"another model id", []string{"vCode", "omlx/Chat"}},
		{"the wildcard", []string{"*"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newCLISandboxStore(t)
			seedModelsService(t, store, []string{"vCode"})

			ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

			widened := tc.widened
			if _, err := ops.Update(context.Background(), "tts", serviceFields{
				DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &widened,
			}, auditViaCLI, ""); err != nil {
				t.Fatalf("Update under an allowing gate: %v", err)
			}

			svc, _ := config.FindServiceByID(store.Get(), "tts")
			if svc == nil || !slices.Equal(svc.AllowedModels, tc.widened) {
				t.Fatalf("allowed models = %+v, want %v", svc, tc.widened)
			}
		})
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

// TestServiceOps_ExistingWildcardNeverGatesFurtherAdditions covers
// serviceAllowedModelsChanged's one carve-out from "any change gates": once
// a service already holds the wildcard, it already reaches every model, so
// nothing else listed alongside it (another id, or a resent "*") changes
// what's actually reachable, and that comparison alone must not gate.
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

// TestServiceOps_DroppingTheWildcardIsGated is the wildcard carve-out's
// mirror case: scoping a service down FROM the wildcard TO a specific set is
// a real narrowing of what's reachable (not "nothing changed", the way
// listing extra ids alongside a resent "*" is), so it must gate like any
// other allowed_models change now does.
func TestServiceOps_DroppingTheWildcardIsGated(t *testing.T) {
	store := newCLISandboxStore(t)
	seedModelsService(t, store, []string{"*"})

	gate, err := presence.NewGate(presencetest.Deny())
	assertNoErr(t, err, "NewGate")
	ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: gate, Issuance: enabledIssuanceRecorder(t)}

	scoped := []string{"vCode"}
	_, err = ops.Update(context.Background(), "tts", serviceFields{
		DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &scoped,
	}, auditViaCLI, "")
	if !errors.Is(err, presence.ErrRefused) {
		t.Fatalf("dropping the wildcard: err = %v, want presence.ErrRefused", err)
	}

	svc, _ := config.FindServiceByID(store.Get(), "tts")
	if svc == nil || !slices.Equal(svc.AllowedModels, []string{"*"}) {
		t.Fatalf("a refused update must not persist: allowed models = %+v", svc)
	}
}

// ---------------------------------------------------------------------------
// Validation: an empty or whitespace-only id is refused before persisting,
// and a valid id survives with surrounding whitespace trimmed.
// ---------------------------------------------------------------------------

func TestServiceOps_Create_RefusesEmptyAllowedModelID(t *testing.T) {
	cases := []struct {
		name string
		bad  []string
	}{
		{"empty id", []string{"vCode", ""}},
		{"whitespace-only id", []string{"   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newCLISandboxStore(t)
			ops := &ServiceOps{Store: store, Registry: &noopServiceManager{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

			bad := tc.bad
			_, err := ops.Create(context.Background(), serviceFields{
				DisplayName: "TTS", Command: "/bin/tts", AllowedModels: &bad,
			}, auditViaCLI, "")
			if !errors.Is(err, errServiceInvalid) {
				t.Fatalf("err = %v, want errServiceInvalid", err)
			}
			if len(store.Get().Services) != 0 {
				t.Fatalf("a refused create persisted a record: %+v", store.Get().Services)
			}
		})
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
