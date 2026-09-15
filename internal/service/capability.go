package service

import (
	"slices"

	"github.com/barelyworkingcode/relay/internal/config"
)

// Operation is one act a launch identity may attempt, on the bridge or the
// frontend socket.
type Operation string

const (
	OpHello            Operation = "Hello"
	OpFrontendSocket   Operation = "FrontendSocket"
	OpRegisterManifest Operation = "RegisterManifest"
	// OpRegisterModelHost is RegisterModelHost: registering this service's
	// router socket as the model endpoint's upstream, under its own id only.
	OpRegisterModelHost Operation = "RegisterModelHost"
	// OpModelCall is a model-endpoint call (model.sock, no header) made by a
	// service's launch identity rather than a project grant. The endpoint
	// itself further limits it to the identity's config.ServiceConfig.AllowedModels.
	OpModelCall Operation = "ModelCall"
	// OpModelList is GET /v1/models on the model endpoint, made by a service's
	// launch identity. Granted by ServiceCapabilityModels, and also by
	// ServiceCapabilitySessions (the unfiltered list, but no calls) —
	// plan-broker-and-sessions.md §2 C1.
	OpModelList Operation = "ModelList"
	// OpSessionExited is the host->relay SessionExited bridge report
	// (plan-broker-and-sessions.md §2 C1, C5). Only the Allowed() gate is
	// R-S1's job: nothing in this repo yet sends or dispatches the request
	// itself — R-S4b adds the bridge request type, the handler, and the
	// ledger/model-key/identity cleanup it drives.
	OpSessionExited Operation = "SessionExited"

	// OpProjectTools is ListTools/CallTool for a project_session identity —
	// root or C3 member alike (plan-broker-and-sessions.md §2 C1's
	// project_session table). Scoping to the project's own live grant is not
	// decided here; Allowed only answers "may an identity of this kind ever
	// reach this operation at all". R-S2a wires resolveAuth's step 3 (a
	// tokenless caller who is a C3 member) to actually reach it.
	OpProjectTools Operation = "ProjectTools"
	// OpProjectDescribe is DescribeProject for a project_session identity,
	// alongside the existing project-token path.
	OpProjectDescribe Operation = "ProjectDescribe"
	// OpProjectListSkills is ListSkillBuckets for a project_session identity.
	OpProjectListSkills Operation = "ProjectListSkills"
)

// Operations is every Operation Allowed decides.
var Operations = []Operation{
	OpHello, OpFrontendSocket, OpRegisterManifest,
	OpRegisterModelHost, OpModelCall, OpModelList, OpSessionExited,
	OpProjectTools, OpProjectDescribe, OpProjectListSkills,
}

// serviceOperationCapability names the one capability each service
// operation requires, for every op whose requirement is a single fixed
// capability. OpHello is absent: every launched service may say it.
// OpModelList is absent too — allowedForService special-cases it, since it
// is the one operation two different capabilities each grant on their own.
var serviceOperationCapability = map[Operation]config.ServiceCapability{
	OpFrontendSocket:    config.ServiceCapabilityFrontend,
	OpRegisterManifest:  config.ServiceCapabilityManifest,
	OpRegisterModelHost: config.ServiceCapabilityModelHost,
	OpModelCall:         config.ServiceCapabilityModels,
	OpSessionExited:     config.ServiceCapabilitySessions,
}

// projectSessionOperations is every operation a project_session identity may
// attempt, root or C3 member alike (plan-broker-and-sessions.md §2 C1). A
// project_session identity carries no capability list — its authority is the
// project's own live grant, read elsewhere — so this is a fixed set, not a
// capability lookup.
var projectSessionOperations = []Operation{
	OpHello, OpProjectTools, OpProjectDescribe, OpProjectListSkills, OpModelCall, OpModelList,
}

// Allowed is relay's one capability decision for a launch identity: whether
// an identity of kind holding caps may perform op. The bridge router and the
// frontend server both ask it and nothing else. An operation with no entry, a
// kind with no table, and a capability name relay does not know all refuse.
func Allowed(kind IdentityKind, caps []config.ServiceCapability, op Operation) bool {
	switch kind {
	case IdentityKindService:
		return allowedForService(caps, op)
	case IdentityKindProjectSession:
		return slices.Contains(projectSessionOperations, op)
	default:
		return false
	}
}

func allowedForService(caps []config.ServiceCapability, op Operation) bool {
	if op == OpHello {
		return true
	}
	if op == OpModelList {
		return slices.Contains(caps, config.ServiceCapabilityModels) || slices.Contains(caps, config.ServiceCapabilitySessions)
	}
	required, ok := serviceOperationCapability[op]
	return ok && slices.Contains(caps, required)
}
