package service

import (
	"errors"
	"slices"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

var (
	capFrontend  = config.ServiceCapabilityFrontend
	capManifest  = config.ServiceCapabilityManifest
	capModels    = config.ServiceCapabilityModels
	capModelHost = config.ServiceCapabilityModelHost
	capSessions  = config.ServiceCapabilitySessions
)

// capsFor maps the two shapes the launch-table tests need onto capability
// sets: a frontend service, or one holding manifest.
func capsFor(frontend bool) []config.ServiceCapability {
	if frontend {
		return []config.ServiceCapability{capFrontend}
	}
	return []config.ServiceCapability{capManifest}
}

// The table restated independently of serviceOperationCapability, so a change
// to one without the other fails here. OpModelList is listed under BOTH
// capModels and capSessions (plan-broker-and-sessions.md §2 C1: "models OR
// sessions" grants the unfiltered list; sessions grants no calls).
var wantServiceOperations = map[config.ServiceCapability][]Operation{
	capFrontend:  {OpFrontendSocket},
	capManifest:  {OpRegisterManifest},
	capModels:    {OpModelCall, OpModelList},
	capModelHost: {OpRegisterModelHost},
	capSessions:  {OpModelList, OpSessionExited},
}

func TestAllowed_EachServiceCapabilityGrantsExactlyItsOperations(t *testing.T) {
	for capability, ops := range wantServiceOperations {
		for _, op := range Operations {
			want := op == OpHello || slices.Contains(ops, op)
			if got := Allowed(IdentityKindService, []config.ServiceCapability{capability}, op); got != want {
				t.Errorf("[%s] %s: allowed = %v, want %v", capability, op, got, want)
			}
		}
	}
}

func TestAllowed_TheEmptySetHoldsOnlyHello(t *testing.T) {
	for _, caps := range [][]config.ServiceCapability{nil, {}} {
		for _, op := range Operations {
			if got := Allowed(IdentityKindService, caps, op); got != (op == OpHello) {
				t.Errorf("%v %s: allowed = %v", caps, op, got)
			}
		}
	}
}

func TestAllowed_FrontendAndManifestTogetherHoldBothAndNothingElse(t *testing.T) {
	caps := []config.ServiceCapability{capFrontend, capManifest}
	for _, op := range Operations {
		want := op == OpHello || op == OpFrontendSocket || op == OpRegisterManifest
		if got := Allowed(IdentityKindService, caps, op); got != want {
			t.Errorf("%s: allowed = %v, want %v", op, got, want)
		}
	}
}

func TestAllowed_ModelsAloneNeverGrantsTheUnfilteredListAlone(t *testing.T) {
	// Both capModels and capSessions grant OpModelList (covered above); this
	// pins that OpModelCall is capModels-only, never capSessions (C1: sessions
	// "gets the unfiltered list too... but that capability does not exist yet
	// in this repo" — now it does, and it still grants no calls).
	if Allowed(IdentityKindService, []config.ServiceCapability{capSessions}, OpModelCall) {
		t.Error("the sessions capability granted a model call")
	}
	if !Allowed(IdentityKindService, []config.ServiceCapability{capModels}, OpModelCall) {
		t.Error("the models capability did not grant a model call")
	}
}

func TestAllowed_UnknownNamesKindsAndOperationsGrantNothing(t *testing.T) {
	all := []config.ServiceCapability{capFrontend, capManifest, capModels, capModelHost, capSessions}
	if Allowed(IdentityKindService, []config.ServiceCapability{"frontend "}, OpFrontendSocket) {
		t.Error("a capability name relay does not know granted the frontend socket")
	}
	if Allowed(IdentityKind("bogus-kind"), all, OpHello) {
		t.Error("a kind with no capability table was allowed Hello")
	}
	if Allowed(IdentityKindService, all, Operation("SomethingNew")) {
		t.Error("an operation with no table entry was allowed")
	}
}

func TestAllowed_ProjectSessionHoldsExactlyItsFixedOperationSet(t *testing.T) {
	want := map[Operation]bool{
		OpHello:             true,
		OpProjectTools:      true,
		OpProjectDescribe:   true,
		OpProjectListSkills: true,
		OpModelCall:         true,
		OpModelList:         true,
	}
	// A project_session identity carries no capabilities at all — its
	// authority is the project's own live grant, read elsewhere — so every
	// capability set, including the empty one, must decide the same way.
	for _, caps := range [][]config.ServiceCapability{
		nil, {}, {capFrontend, capManifest, capModels, capModelHost, capSessions},
	} {
		for _, op := range Operations {
			if got := Allowed(IdentityKindProjectSession, caps, op); got != want[op] {
				t.Errorf("caps=%v %s: allowed = %v, want %v", caps, op, got, want[op])
			}
		}
	}
}

func TestAllowed_ServiceAndProjectSessionDecideOperationsDisjointly(t *testing.T) {
	// OpModelCall/OpModelList are the only operations legitimately reachable
	// by both kinds (under different grants); every other project_session
	// operation must be refused for a service identity holding every
	// capability, and vice versa for the service-only operations.
	allCaps := []config.ServiceCapability{capFrontend, capManifest, capModels, capModelHost, capSessions}
	for _, op := range []Operation{OpProjectTools, OpProjectDescribe, OpProjectListSkills} {
		if Allowed(IdentityKindService, allCaps, op) {
			t.Errorf("a service identity holding every capability was allowed %s", op)
		}
	}
	for _, op := range []Operation{OpFrontendSocket, OpRegisterManifest, OpRegisterModelHost, OpSessionExited} {
		if Allowed(IdentityKindProjectSession, nil, op) {
			t.Errorf("a project_session identity was allowed %s", op)
		}
	}
}

func TestRegistry_RefusesToStartAServiceWithAnUnknownCapability(t *testing.T) {
	reg := NewRegistry()
	reg.Launches = NewLaunches()
	err := reg.Start(&config.ServiceConfig{
		ID: "svc", DisplayName: "svc", Command: "/usr/bin/true",
		Capabilities: []config.ServiceCapability{capManifest, "everything"},
	})
	if err == nil {
		t.Fatal("a service with an unknown capability was started")
	}
	if reg.Launches.Len() != 0 || reg.IsRunning("svc") {
		t.Fatal("a refused start left a launch or a process behind")
	}
	if errors.Is(err, ErrHelloRefused) {
		t.Fatalf("wrong refusal: %v", err)
	}
}
