package service

import (
	"slices"

	"github.com/barelyworkingcode/relay/internal/config"
)

// Operation is one act a launch identity may attempt, on the bridge or the
// frontend socket.
type Operation string

const (
	OpHello                  Operation = "Hello"
	OpFrontendSocket         Operation = "FrontendSocket"
	OpRegisterManifest       Operation = "RegisterManifest"
	OpResolvePtyEnv          Operation = "ResolvePtyEnv"
	OpResolveProjectTemplate Operation = "ResolveProjectTemplate"
	OpListProjects           Operation = "ListProjects"
	OpGetProject             Operation = "GetProject"
	// OpServiceTools is ListTools and CallTool with no token: every MCP,
	// unfiltered by any project's grant.
	OpServiceTools Operation = "ServiceTools"
)

// Operations is every Operation Allowed decides.
var Operations = []Operation{
	OpHello, OpFrontendSocket, OpRegisterManifest, OpResolvePtyEnv,
	OpResolveProjectTemplate, OpListProjects, OpGetProject, OpServiceTools,
}

// serviceOperationCapability names the one capability each service
// operation requires. OpHello is absent: every launched service may say it.
var serviceOperationCapability = map[Operation]config.ServiceCapability{
	OpFrontendSocket:         config.ServiceCapabilityFrontend,
	OpRegisterManifest:       config.ServiceCapabilityManifest,
	OpResolvePtyEnv:          config.ServiceCapabilityProjects,
	OpResolveProjectTemplate: config.ServiceCapabilityProjects,
	OpListProjects:           config.ServiceCapabilityProjects,
	OpGetProject:             config.ServiceCapabilityProjects,
	OpServiceTools:           config.ServiceCapabilityProjects,
}

// Allowed is relay's one capability decision for a launch identity: whether
// an identity of kind holding caps may perform op. The bridge router and the
// frontend server both ask it and nothing else. An operation with no entry, a
// kind with no table, and a capability name relay does not know all refuse.
func Allowed(kind IdentityKind, caps []config.ServiceCapability, op Operation) bool {
	if kind != IdentityKindService {
		return false
	}
	if op == OpHello {
		return true
	}
	required, ok := serviceOperationCapability[op]
	return ok && slices.Contains(caps, required)
}
