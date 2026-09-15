package main

import (
	"context"
	"slices"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
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
