package main

// Validate-before-persist coverage for ServiceOps.Create/Update: a
// misconfigured service (here, an operator env key that collides with
// relay's own RELAY_* namespace) must never land in settings.json, whether
// it is a brand-new record or an edit to an existing one.

import (
	"context"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func TestServiceOps_Create_RefusesRelayPrefixedEnvBeforePersisting(t *testing.T) {
	store := newCLISandboxStore(t)
	r := newBrokerRouter(t, store, nil)

	_, err := r.serviceOps.Create(context.Background(), serviceFields{
		DisplayName: "Bad Env Svc",
		Command:     "/bin/true",
		Env:         map[string]string{"RELAY_SERVICE_TOKEN": "poison"},
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
		Env:         map[string]string{"RELAY_FRONTEND_TOKEN": "poison"},
	}, auditViaCLI, "")
	if err == nil {
		t.Fatal("expected an error for a RELAY_-prefixed env key")
	}

	svc, _ := config.FindServiceByID(store.Get(), "svc1")
	if svc == nil || svc.Command != "/bin/old" {
		t.Fatalf("the existing record must be untouched by a refused update, got %+v", svc)
	}
}
