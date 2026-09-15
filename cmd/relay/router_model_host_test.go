package main

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"github.com/barelyworkingcode/relay/internal/service"
)

func newModelHostTestRouter(t *testing.T) *appRouter {
	t.Helper()
	r := newTestRouter(t, makeSettings(nil, nil, nil), mcpbroker.NewManager(nil))
	// Built before any bindTestIdentity call so the registry shares the same
	// table bindTestIdentity binds into, rather than one it lazily creates
	// (bindTestIdentity only allocates a fresh Launches when r.launches is
	// still nil).
	r.launches = service.NewLaunches()
	r.modelHosts = NewModelHostRegistry(r.launches)
	return r
}

func TestRegisterModelHost_RequiresTheModelHostCapability(t *testing.T) {
	r := newModelHostTestRouter(t)
	ctx := bindTestIdentity(t, r, "relayllm", []config.ServiceCapability{config.ServiceCapabilityManifest})

	err := r.RegisterModelHost(ctx, bridge.RegisterModelHostRequest{ServiceID: "relayllm", RouterSocket: "/tmp/r.sock"}, "")
	if err == nil {
		t.Fatal("an identity without model_host registered a model host")
	}
	if _, _, _, ok := r.modelHosts.Current(); ok {
		t.Fatal("a refused registration left a host recorded")
	}
}

func TestRegisterModelHost_TokenIsRefused(t *testing.T) {
	r := newModelHostTestRouter(t)
	ctx := bindTestIdentity(t, r, "relayllm", []config.ServiceCapability{config.ServiceCapabilityModelHost})

	err := r.RegisterModelHost(ctx, bridge.RegisterModelHostRequest{ServiceID: "relayllm", RouterSocket: "/tmp/r.sock"}, "some-token")
	if err == nil {
		t.Fatal("RegisterModelHost accepted a bearer token")
	}
}

func TestRegisterModelHost_OwnServiceIDOnly(t *testing.T) {
	r := newModelHostTestRouter(t)
	ctx := bindTestIdentity(t, r, "relayllm", []config.ServiceCapability{config.ServiceCapabilityModelHost})

	err := r.RegisterModelHost(ctx, bridge.RegisterModelHostRequest{ServiceID: "someone-else", RouterSocket: "/tmp/r.sock"}, "")
	if err == nil {
		t.Fatal("a service registered a model host under another service's id")
	}
	if _, _, _, ok := r.modelHosts.Current(); ok {
		t.Fatal("an own-id-violating registration left a host recorded")
	}
}

func TestRegisterModelHost_SucceedsAndIsVisibleOnTheRegistry(t *testing.T) {
	r := newModelHostTestRouter(t)
	ctx := bindTestIdentity(t, r, "relayllm", []config.ServiceCapability{config.ServiceCapabilityModelHost})

	if err := r.RegisterModelHost(ctx, bridge.RegisterModelHostRequest{ServiceID: "relayllm", RouterSocket: "/tmp/r.sock"}, ""); err != nil {
		t.Fatalf("RegisterModelHost: %v", err)
	}
	id, sock, _, ok := r.modelHosts.Current()
	if !ok || id != "relayllm" || sock != "/tmp/r.sock" {
		t.Fatalf("Current = %q, %q, %v", id, sock, ok)
	}
}

func TestRegisterModelHost_ReplacementRefusedWhileLive(t *testing.T) {
	r := newModelHostTestRouter(t)
	ctxA := bindTestIdentity(t, r, "relayllm", []config.ServiceCapability{config.ServiceCapabilityModelHost})
	assertNoErr(t, r.RegisterModelHost(ctxA, bridge.RegisterModelHostRequest{ServiceID: "relayllm", RouterSocket: "/tmp/a.sock"}, ""), "first register")

	ctxB := bindTestIdentity(t, r, "other-llm", []config.ServiceCapability{config.ServiceCapabilityModelHost})
	if err := r.RegisterModelHost(ctxB, bridge.RegisterModelHostRequest{ServiceID: "other-llm", RouterSocket: "/tmp/b.sock"}, ""); err == nil {
		t.Fatal("a second registration was accepted while the first launch is still live")
	}
	id, _, _, _ := r.modelHosts.Current()
	if id != "relayllm" {
		t.Fatalf("current host changed to %q despite the refusal", id)
	}
}

func TestRegisterModelHost_NilRegistryRefusesRatherThanPanics(t *testing.T) {
	r := newTestRouter(t, makeSettings(nil, nil, nil), mcpbroker.NewManager(nil))
	ctx := bindTestIdentity(t, r, "relayllm", []config.ServiceCapability{config.ServiceCapabilityModelHost})
	if err := r.RegisterModelHost(ctx, bridge.RegisterModelHostRequest{ServiceID: "relayllm", RouterSocket: "/tmp/r.sock"}, ""); err == nil {
		t.Fatal("RegisterModelHost succeeded with no registry wired")
	}
}
