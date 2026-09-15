package main

import (
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/peertoken"
	"github.com/barelyworkingcode/relay/internal/service"
)

// beginAndBindLaunch begins a launch named name and binds it to a synthetic
// process, returning the launch handle (so the test can End it) and the
// process peertoken.Bind actually recorded.
func beginAndBindLaunch(t *testing.T, launches *service.Launches, name string, caps []config.ServiceCapability, pid int32) (*service.Launch, peertoken.Process) {
	t.Helper()
	secret, launch, err := launches.Begin(service.Identity{Kind: service.IdentityKindService, Name: name, Capabilities: caps})
	assertNoErr(t, err, "Begin")
	peer := peertoken.ForProcessForTest(pid, 1)
	id, err := launches.Bind(name, secret, peer)
	assertNoErr(t, err, "Bind")
	return launch, id.Process
}

func TestModelHostRegistry_RegisterAndCurrent(t *testing.T) {
	launches := service.NewLaunches()
	hosts := NewModelHostRegistry(launches)

	if _, _, _, ok := hosts.Current(); ok {
		t.Fatal("an empty registry reports a live host")
	}

	_, process := beginAndBindLaunch(t, launches, "relayllm", []config.ServiceCapability{config.ServiceCapabilityModelHost}, 42001)
	assertNoErr(t, hosts.Register("relayllm", "/tmp/router.sock", process), "Register")

	id, sock, gotProcess, ok := hosts.Current()
	if !ok || id != "relayllm" || sock != "/tmp/router.sock" || gotProcess != process {
		t.Fatalf("Current = %q, %q, %+v, %v", id, sock, gotProcess, ok)
	}
}

func TestModelHostRegistry_ReplacementRefusedWhileLiveAllowedAfterEnd(t *testing.T) {
	launches := service.NewLaunches()
	hosts := NewModelHostRegistry(launches)

	launchA, processA := beginAndBindLaunch(t, launches, "relayllm", []config.ServiceCapability{config.ServiceCapabilityModelHost}, 42002)
	assertNoErr(t, hosts.Register("relayllm", "/tmp/a.sock", processA), "first Register")

	// A second registration from a DIFFERENT service, while A's launch is
	// still live, is refused.
	_, processB := beginAndBindLaunch(t, launches, "other", []config.ServiceCapability{config.ServiceCapabilityModelHost}, 42003)
	if err := hosts.Register("other", "/tmp/b.sock", processB); err == nil {
		t.Fatal("a replacement was accepted while the old launch was still live")
	}
	if id, _, _, _ := hosts.Current(); id != "relayllm" {
		t.Fatalf("a refused replacement changed the current host to %q", id)
	}

	// A re-registration from the SAME service, while its own launch is still
	// live, is refused too (docs/model-endpoint.md: only after the old
	// launch has ended).
	if err := hosts.Register("relayllm", "/tmp/a2.sock", processA); err == nil {
		t.Fatal("a same-service re-registration was accepted while its own launch was still live")
	}

	launchA.End()
	if err := hosts.Register("other", "/tmp/b.sock", processB); err != nil {
		t.Fatalf("a replacement after the old launch ended was refused: %v", err)
	}
	if id, sock, _, ok := hosts.Current(); !ok || id != "other" || sock != "/tmp/b.sock" {
		t.Fatalf("Current after replacement = %q, %q, %v", id, sock, ok)
	}
}

func TestModelHostRegistry_CurrentIsNotLiveAfterLaunchEnds(t *testing.T) {
	launches := service.NewLaunches()
	hosts := NewModelHostRegistry(launches)

	launch, process := beginAndBindLaunch(t, launches, "relayllm", []config.ServiceCapability{config.ServiceCapabilityModelHost}, 42004)
	assertNoErr(t, hosts.Register("relayllm", "/tmp/a.sock", process), "Register")

	launch.End()
	if _, _, _, ok := hosts.Current(); ok {
		t.Fatal("Current reports a host whose launch has ended")
	}
}

func TestModelHostRegistry_NilLaunchesNeverReportsLive(t *testing.T) {
	hosts := NewModelHostRegistry(nil)
	if err := hosts.Register("x", "/tmp/x.sock", peertoken.ForProcessForTest(1, 1).Process()); err != nil {
		t.Fatalf("Register with a nil launch table: %v", err)
	}
	if _, _, _, ok := hosts.Current(); ok {
		t.Fatal("a registry with no launch table reports a live host")
	}
}
