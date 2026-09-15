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
	capProjects  = config.ServiceCapabilityProjects
	capModels    = config.ServiceCapabilityModels
	capModelHost = config.ServiceCapabilityModelHost
)

// capsFor maps the two shapes the launch-table tests need onto capability
// sets: a frontend service, or one holding manifest and projects.
func capsFor(frontend bool) []config.ServiceCapability {
	if frontend {
		return []config.ServiceCapability{capFrontend}
	}
	return []config.ServiceCapability{capManifest, capProjects}
}

// The table restated independently of serviceOperationCapability, so a change
// to one without the other fails here.
var wantOperations = map[config.ServiceCapability][]Operation{
	capFrontend:  {OpFrontendSocket},
	capManifest:  {OpRegisterManifest},
	capProjects:  {OpResolvePtyEnv, OpResolveProjectTemplate, OpListProjects, OpGetProject, OpServiceTools},
	capModels:    {OpModelCall, OpModelList},
	capModelHost: {OpRegisterModelHost},
}

func TestAllowed_EachCapabilityGrantsExactlyItsOperations(t *testing.T) {
	for capability, ops := range wantOperations {
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

func TestAllowed_UnknownNamesKindsAndOperationsGrantNothing(t *testing.T) {
	all := []config.ServiceCapability{capFrontend, capManifest, capProjects}
	if Allowed(IdentityKindService, []config.ServiceCapability{"frontend "}, OpFrontendSocket) {
		t.Error("a capability name relay does not know granted the frontend socket")
	}
	if Allowed(IdentityKind("project_session"), all, OpHello) {
		t.Error("a kind with no capability table was allowed Hello")
	}
	if Allowed(IdentityKindService, all, Operation("SomethingNew")) {
		t.Error("an operation with no table entry was allowed")
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
