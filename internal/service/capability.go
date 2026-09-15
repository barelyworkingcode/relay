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
	// OpRegisterModelHost is RegisterModelHost: registering this service's
	// router socket as the model endpoint's upstream, under its own id only.
	OpRegisterModelHost Operation = "RegisterModelHost"
	// OpModelCall is a model-endpoint call (model.sock, no header) made by a
	// service's launch identity rather than a project grant. The endpoint
	// itself further limits it to the identity's config.ServiceConfig.AllowedModels.
	OpModelCall Operation = "ModelCall"
	// OpModelList is GET /v1/models on the model endpoint, made by a service's
	// launch identity. Granted by ServiceCapabilityModels here; a future
	// `sessions` capability grants the unfiltered list too (plan-broker-and-
	// sessions.md §2 C1) but that capability does not exist yet in this repo.
	OpModelList Operation = "ModelList"
)

// Operations is every Operation Allowed decides.
var Operations = []Operation{
	OpHello, OpFrontendSocket, OpRegisterManifest, OpResolvePtyEnv,
	OpResolveProjectTemplate, OpListProjects, OpGetProject, OpServiceTools,
	OpRegisterModelHost, OpModelCall, OpModelList,
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
	OpRegisterModelHost:      config.ServiceCapabilityModelHost,
	OpModelCall:              config.ServiceCapabilityModels,
	OpModelList:              config.ServiceCapabilityModels,
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
