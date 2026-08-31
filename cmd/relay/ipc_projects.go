package main

import (
	"encoding/json"
	"log/slog"
)

// Project IPC handlers for relay's native Projects tab. These mirror
// project_routes.go (the HTTP surface eve uses) but emit events instead of
// returning HTTP bodies so the in-tray WebView can stay reactive; both paths
// share the same Settings mutators, so a project created over IPC is
// identical to one created over HTTP.

// ipcUpdateProjectMsg carries the project id inline, unlike the HTTP PUT
// route. The patch fields are the shared projectUpdateFields so the update
// orchestration stays in one place (applyProjectUpdate).
type ipcUpdateProjectMsg struct {
	ID string `json:"id"`
	projectUpdateFields
}

type ipcProjectDisabledToolsMsg struct {
	ID       string   `json:"id"`
	McpID    string   `json:"mcp_id"`
	Disabled []string `json:"disabled"`
}

type ipcListMcpToolsMsg struct {
	McpID string `json:"mcp_id"`
}

type ipcEnumerateScopeFieldMsg struct {
	McpID  string                     `json:"mcp_id"`
	Field  string                     `json:"field"`
	Values map[string]json.RawMessage `json:"values,omitempty"`
}

func ipcCreateProject(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[projectCreateFields](raw, "create_project")
	if !ok {
		return
	}
	// Validated before any mutation so a bad policy can't create a project
	// that then has to be rolled back — mirrors the HTTP POST route. Cheap
	// and non-blocking, so it stays on the IPC thread rather than paying a
	// GoFunc/DispatchToMain round trip for the common "bad input" case.
	if msg.PermissionPolicy != nil {
		if err := validatePermissionPolicy(msg.PermissionPolicy); err != nil {
			ctx.UI.EmitEvent("onProjectError", err.Error())
			return
		}
	}

	surfaces := mcpSurfacesFrom(ctx)
	// Off the main thread: ProjectOps.Create is gated (project.grant, §6.4
	// of the ADR-017 implementation spec) whenever the request sets a
	// grant-widening field, and Gate.Require blocks on LocalAuthentication's
	// async completion handler, which needs the Cocoa run loop pumped to be
	// delivered — the same deadlock showLoginCode's doc comment in
	// trayapp.go describes.
	ctx.GoFunc(func() {
		created, createErr := ctx.ProjectOps.Create(ctx.Ctx, *msg, surfaces, auditViaIPC, "")
		if createErr != nil {
			dispatchEmit(ctx, "onProjectError", createErr.Error())
			return
		}

		if ctx.SkillLister != nil && created.GenerateSkill {
			ctx.GoFunc(func() {
				reconcileProjectSkill(ctx.Ctx, ctx.SkillLister, created)
			})
		}

		dispatchEmit(ctx, "onProjectAdded", marshalForUI(projectToNativeView(created)))
	})
}

func ipcUpdateProject(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcUpdateProjectMsg](raw, "update_project")
	if !ok || msg.ID == "" {
		return
	}
	if msg.PermissionPolicy != nil {
		if err := validatePermissionPolicy(msg.PermissionPolicy); err != nil {
			ctx.UI.EmitEvent("onProjectError", err.Error())
			return
		}
	}
	// Off the main thread: ProjectOps.Update is gated (project.grant, §6.4)
	// whenever the patch widens the grant, and Gate.Require blocks on the
	// same async LocalAuthentication completion the Cocoa run loop must
	// pump — see ipcCreateProject just above.
	ctx.GoFunc(func() {
		// Shape/grant validation (including path) happens inside
		// applyProjectUpdate against the fully-merged candidate — mirrors
		// the HTTP PUT route.
		updated, found, updateErr := ctx.ProjectOps.Update(ctx.Ctx, msg.ID, msg.projectUpdateFields, func() McpSurfaces {
			return mcpSurfacesFrom(ctx)
		}, auditViaIPC, "")
		if updateErr != nil {
			dispatchEmit(ctx, "onProjectError", updateErr.Error())
			return
		}
		if !found {
			dispatchEmit(ctx, "onProjectError", "project not found")
			return
		}

		if ctx.SkillLister != nil && updated.GenerateSkill {
			ctx.GoFunc(func() {
				reconcileProjectSkill(ctx.Ctx, ctx.SkillLister, updated)
			})
		}

		dispatchEmit(ctx, "onProjectUpdated", marshalForUI(projectToNativeView(updated)))
	})
}

func ipcRemoveProject(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "remove_project")
	if !ok || msg.ID == "" {
		return
	}

	var removed Project
	var existed bool
	okSettings := ctx.withSettings(func(s *Settings) {
		proj, _ := s.findProjectByID(msg.ID)
		if proj == nil {
			return
		}
		existed = true
		removed = *proj
		s.RemoveProject(msg.ID)
	})
	if !okSettings {
		return
	}
	if !existed {
		ctx.UI.EmitEvent("onProjectError", "project not found")
		return
	}

	if dir := projectSkillDir(removed); dir != "" {
		if err := RemoveSkill(dir); err != nil {
			slog.Warn("project skill remove failed", "project", removed.Name, "error", err)
		}
	}

	ctx.UI.EmitEvent("onProjectRemoved", msg.ID)
}

// ipcRotateProjectToken: the old token stops authenticating on the next call
// to AuthenticateProject, so any active session (Eve, relayLLM, CLI) must
// re-auth.
func ipcRotateProjectToken(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "rotate_project_token")
	if !ok || msg.ID == "" {
		return
	}

	// Off the main thread: RotateToken is gated (project.rotate_token,
	// §6.4) and Gate.Require blocks on the same async LocalAuthentication
	// completion the Cocoa run loop must pump — see ipcCreateProject above.
	ctx.GoFunc(func() {
		// ProjectOps.RotateToken records the rotation itself (so it can
		// attach the presence_id the gate minted) and withholds the new
		// plaintext when that record cannot be written — rotate again once
		// the log is writable.
		newPlaintext, found, err := ctx.ProjectOps.RotateToken(ctx.Ctx, msg.ID, auditViaIPC, "")
		if err != nil {
			dispatchEmit(ctx, "onProjectError", err.Error())
			return
		}
		if !found {
			dispatchEmit(ctx, "onProjectError", "project not found")
			return
		}

		dispatchEmit(ctx, "onProjectTokenRotated", msg.ID, newPlaintext)
	})
}

// ipcRegenProjectSkill forces regeneration regardless of the GenerateSkill
// flag, which only gates *automatic* regen on save/MCP-change — this is the
// explicit user-initiated path.
func ipcRegenProjectSkill(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "regen_project_skill")
	if !ok || msg.ID == "" {
		return
	}
	if ctx.SkillLister == nil {
		ctx.UI.EmitEvent("onProjectSkillRegen", msg.ID, false, "skill regeneration not available")
		return
	}
	proj, _ := ctx.Store.Get().findProjectByID(msg.ID)
	if proj == nil {
		ctx.UI.EmitEvent("onProjectSkillRegen", msg.ID, false, "project not found")
		return
	}
	dir := projectSkillDir(*proj)
	if dir == "" {
		ctx.UI.EmitEvent("onProjectSkillRegen", msg.ID, false, "project has no path")
		return
	}
	projCopy := *proj
	ctx.GoFunc(func() {
		if _, err := EmitSkills(ctx.Ctx, ctx.SkillLister, projCopy, dir, RegenAlways); err != nil {
			dispatchEmit(ctx, "onProjectSkillRegen", msg.ID, false, err.Error())
			return
		}
		dispatchEmit(ctx, "onProjectSkillRegen", msg.ID, true, dir)
	})
}

// ipcUpdateProjectDisabledTools lets the form toggle one tool without
// resending the entire project body on every checkbox click; kept alongside
// update_project (which patches the full map) because it makes the per-row
// UX cheap.
func ipcUpdateProjectDisabledTools(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcProjectDisabledToolsMsg](raw, "update_project_disabled_tools")
	if !ok || msg.ID == "" || msg.McpID == "" {
		return
	}

	var updated Project
	var found bool
	okSettings := ctx.withSettings(func(s *Settings) {
		if proj, _ := s.findProjectByID(msg.ID); proj == nil {
			return
		}
		s.UpdateProjectDisabledTools(msg.ID, msg.McpID, msg.Disabled)
		if proj, _ := s.findProjectByID(msg.ID); proj != nil {
			updated = *proj
			found = true
		}
	})
	if !okSettings {
		return
	}
	if !found {
		ctx.UI.EmitEvent("onProjectError", "project not found")
		return
	}
	ctx.UI.EmitEvent("onProjectUpdated", marshalForUI(projectToNativeView(updated)))
}

// ipcListMcpTools emits an empty list rather than an error when the MCP is
// registered-but-not-connected (e.g. HTTP MCP awaiting OAuth) so the UI can
// show its own "authenticate first" hint without a noisy error.
func ipcListMcpTools(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcListMcpToolsMsg](raw, "list_mcp_tools")
	if !ok || msg.McpID == "" {
		return
	}
	var infos []ToolInfo
	if ctx.Tools != nil {
		infos = ctx.Tools.ToolInfos(msg.McpID)
	}
	if infos == nil {
		infos = []ToolInfo{}
	}
	ctx.UI.EmitEvent("onMcpToolsListed", msg.McpID, marshalForUI(infos))
}

// ipcEnumerateScopeField is the tray's counterpart of
// POST /api/mcps/{id}/enumerate (ADR-004 keeps the two editors co-equal).
// Both surfaces call enumerateScopeField, so every check relay makes — is
// this a field the MCP declared, did it declare it enumerable — is made
// once and identically. The result is emitted VERBATIM, including its
// status, because the picker must render "no values" and "could not ask"
// differently.
//
// Runs off the UI thread: a context/enumerate is a live round trip to
// another process, and blocking the main thread on one would freeze the
// settings window for as long as the MCP takes.
func ipcEnumerateScopeField(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcEnumerateScopeFieldMsg](raw, "enumerate_scope_field")
	if !ok {
		return
	}
	surfaces := mcpSurfacesFrom(ctx)
	enum := ctx.Enumerate
	ctx.GoFunc(func() {
		res := enumerateScopeField(ctx.Ctx, surfaces, enum, msg.McpID, msg.Field, msg.Values)
		dispatchEmit(ctx, "onScopeFieldEnumerated", marshalForUI(res))
	})
}

// mcpSurfacesFrom returns nil when ctx.Tools doesn't also implement
// McpSurfaceProvider (a narrow test stub, typically) — SyncProjectToken
// falls back to its "no scope derivation" path in that case.
func mcpSurfacesFrom(ctx *IPCContext) McpSurfaces {
	if p, ok := ctx.Tools.(McpSurfaceProvider); ok {
		return p.AllMcpSurfaces()
	}
	return nil
}
