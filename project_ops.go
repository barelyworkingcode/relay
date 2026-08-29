package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"relaygo/presence"
)

// ProjectOps wraps applyProjectCreate, applyProjectUpdate and
// RotateProjectToken with the presence gate ADR-017 decision 3's
// `configure` subset names: creating a project or widening its grant shape
// needs the same presence check minting a credential does, because a
// project's token is a security boundary and allowed_mcp_ids, allowed_tools,
// access, context, allow_external and allow_cwd_auth ARE what that boundary
// reaches. allow_external is ADR-011 decision 2c's second axis of the same
// permission set: an agent holding a project token whose project it can
// edit does not need to mint anything to reach outbound — it widens the
// grant it already has (ADR-017 decision 3's own configure-subset
// argument), so it is gated on exactly the same footing as allowed_tools.
//
// The one door each for HTTP (project_routes.go) and the WebView IPC
// (ipc_projects.go) go through this rather than calling applyProjectCreate /
// applyProjectUpdate / RotateProjectToken directly, so neither can drift
// from the other's gate.
type ProjectOps struct {
	Store SettingsStore
	// Gate is the presence check Create, Update and RotateToken demand
	// before they touch the store. A nil Gate refuses all three — see
	// requireGate.
	Gate *presence.Gate
	// Issuance records the config_change every gated create/update leaves
	// and the credential_issued a rotation is (§7.5), and is the hard
	// dependency §7.4 checks before Gate.
	Issuance IssuanceAuditor
	OnChange func()
}

// errProjectSaveFailed distinguishes an internal settings-write failure
// from a validation refusal (applyProjectCreate/applyProjectUpdate's own
// error) and from a presence/audit refusal, so a door can map each to its
// own status code without inspecting error text.
var errProjectSaveFailed = errors.New("failed to save settings")

// errProjectTokenUnrecorded means the rotation committed and the audit
// write then failed: the new plaintext is withheld (§7.6's rotate_token
// withhold, unchanged) but the old token is already dead either way, so a
// door must treat this as an internal failure, not a validation refusal.
var errProjectTokenUnrecorded = errors.New("the token was rotated but could not be recorded in the audit log, so it was not returned; rotate again")

func (o *ProjectOps) notify() {
	if o.OnChange != nil {
		o.OnChange()
	}
}

// presenceDigest binds a project.grant grant to exactly the shape being
// created (§6.4). project_id is absent on create — there is none yet.
func (f projectCreateFields) presenceDigest() presence.Digest {
	return presence.NewDigestBuilder("project.grant").
		StringField("project_id", false, "").
		StringSetField("allowed_mcp_ids", true, f.AllowedMcpIDs).
		StringSetMapField("allowed_tools", true, f.AllowedTools).
		StringMapField("access", true, f.Access).
		RawJSONMapField("context", true, rawJSONMapOf(f.Context)).
		BoolMapField("allow_external", true, f.AllowExternal).
		BoolField("allow_cwd_auth", true, f.AllowCwdAuth).
		StringField("kind", true, string(f.Kind)).
		StringField("path", true, f.Path).
		Build()
}

// presenceDigest binds a project.grant grant to exactly the fields id's
// update touches, absent-aware (§6.4): a grant answered for one field must
// not be spendable on a request that also, or instead, touches another.
func (f projectUpdateFields) presenceDigest(id string) presence.Digest {
	b := presence.NewDigestBuilder("project.grant").StringField("project_id", true, id)
	if f.AllowedMcpIDs != nil {
		b.StringSetField("allowed_mcp_ids", true, *f.AllowedMcpIDs)
	} else {
		b.StringSetField("allowed_mcp_ids", false, nil)
	}
	if f.AllowedTools != nil {
		b.StringSetMapField("allowed_tools", true, *f.AllowedTools)
	} else {
		b.StringSetMapField("allowed_tools", false, nil)
	}
	if f.Access != nil {
		b.StringMapField("access", true, *f.Access)
	} else {
		b.StringMapField("access", false, nil)
	}
	if f.Context != nil {
		b.RawJSONMapField("context", true, rawJSONMapOf(*f.Context))
	} else {
		b.RawJSONMapField("context", false, nil)
	}
	if f.AllowExternal != nil {
		b.BoolMapField("allow_external", true, *f.AllowExternal)
	} else {
		b.BoolMapField("allow_external", false, nil)
	}
	if f.AllowCwdAuth != nil {
		b.BoolField("allow_cwd_auth", true, *f.AllowCwdAuth)
	} else {
		b.BoolField("allow_cwd_auth", false, false)
	}
	if f.Kind != nil {
		b.StringField("kind", true, string(*f.Kind))
	} else {
		b.StringField("kind", false, "")
	}
	if f.Path != nil {
		b.StringField("path", true, *f.Path)
	} else {
		b.StringField("path", false, "")
	}
	return b.Build()
}

// projectUpdateTouchesGrant is AC-16c's "does" list: allowed_mcp_ids,
// allowed_tools, access, context, allow_external, allow_cwd_auth, kind or
// path. A request touching only name, chat_templates, session_folders,
// generate_skill, permission_policy, allowed_models or shell_templates must
// NOT prompt — none of those widen what a token reaches. disabled_tools is
// deliberately absent too: it is a denylist and can only narrow (§6.4).
func projectUpdateTouchesGrant(f projectUpdateFields) bool {
	return f.AllowedMcpIDs != nil || f.AllowedTools != nil || f.Access != nil ||
		f.Context != nil || f.AllowExternal != nil || f.AllowCwdAuth != nil || f.Kind != nil || f.Path != nil
}

func projectUpdateGrantFieldNames(f projectUpdateFields) []string {
	var names []string
	if f.AllowedMcpIDs != nil {
		names = append(names, "allowed_mcp_ids")
	}
	if f.AllowedTools != nil {
		names = append(names, "allowed_tools")
	}
	if f.Access != nil {
		names = append(names, "access")
	}
	if f.Context != nil {
		names = append(names, "context")
	}
	if f.AllowExternal != nil {
		names = append(names, "allow_external")
	}
	if f.AllowCwdAuth != nil {
		names = append(names, "allow_cwd_auth")
	}
	if f.Kind != nil {
		names = append(names, "kind")
	}
	if f.Path != nil {
		names = append(names, "path")
	}
	return names
}

func projectCreateGrantFieldNames(f projectCreateFields) []string {
	names := []string{"kind", "path"}
	if len(f.AllowedMcpIDs) > 0 {
		names = append(names, "allowed_mcp_ids")
	}
	if len(f.AllowedTools) > 0 {
		names = append(names, "allowed_tools")
	}
	if len(f.Access) > 0 {
		names = append(names, "access")
	}
	if len(f.Context) > 0 {
		names = append(names, "context")
	}
	if len(f.AllowExternal) > 0 {
		names = append(names, "allow_external")
	}
	if f.AllowCwdAuth {
		names = append(names, "allow_cwd_auth")
	}
	return names
}

// projectGrantUpdateReason names the actual act (§6.5.2). allow_cwd_auth
// gets its own sentence when it is the field being turned on: the ADR
// singles it out because turning it on hands the project's whole tool set
// to any process standing in the directory, with no token at all.
func projectGrantUpdateReason(id string, f projectUpdateFields, cwdAuthTurningOn bool) string {
	if cwdAuthTurningOn {
		return fmt.Sprintf("turn on directory authentication for the project %q", id)
	}
	return fmt.Sprintf("widen the grant for the project %q (%s)", id, strings.Join(projectUpdateGrantFieldNames(f), ", "))
}

// Create is unconditionally gated: every create sets Kind and Path, so
// establishing a project's initial grant shape from nothing is exactly the
// widening act project.grant exists to prompt for (§6.4's note that the op
// "fires on any project create or update whose request sets any of the
// listed fields").
func (o *ProjectOps) Create(ctx context.Context, f projectCreateFields, surfaces McpSurfaces, via, credID string) (Project, error) {
	if strings.TrimSpace(f.Name) == "" {
		return Project{}, fmt.Errorf("project name is required")
	}

	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return Project{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "project.grant", f.presenceDigest(),
		fmt.Sprintf("create the project %q and grant it its initial scope", f.Name))
	if err != nil {
		return Project{}, err
	}

	var created Project
	var createErr error
	if err := o.Store.With(func(s *Settings) {
		created, createErr = applyProjectCreate(s, f, surfaces)
	}); err != nil {
		return Project{}, fmt.Errorf("%w: %v", errProjectSaveFailed, err)
	}
	if createErr != nil {
		return Project{}, createErr
	}
	if auditErr := recordConfigChange(o.Issuance, auditCredentialProjectGrant, created.ID,
		projectCreateGrantFieldNames(f), via, credID, grant.ID()); auditErr != nil {
		slog.Error("project created but not recorded in the audit log", "id", created.ID, "error", auditErr)
	}
	o.notify()
	return created, nil
}

// Update gates only when the request touches the configure subset
// (projectUpdateTouchesGrant) — AC-16c requires that a rename or a
// chat-template edit not prompt.
func (o *ProjectOps) Update(ctx context.Context, id string, f projectUpdateFields, surfaces func() McpSurfaces, via, credID string) (Project, bool, error) {
	touchesGrant := projectUpdateTouchesGrant(f)
	var presenceID string
	if touchesGrant {
		if err := requireIssuanceAuditor(o.Issuance); err != nil {
			return Project{}, false, err
		}
		cwdAuthTurningOn := f.AllowCwdAuth != nil && *f.AllowCwdAuth
		grant, err := requireGate(o.Gate, ctx, "project.grant", f.presenceDigest(id),
			projectGrantUpdateReason(id, f, cwdAuthTurningOn))
		if err != nil {
			return Project{}, false, err
		}
		presenceID = grant.ID()
	}

	var updated Project
	var found bool
	var updateErr error
	if err := o.Store.With(func(s *Settings) {
		updated, found, updateErr = applyProjectUpdate(s, id, f, surfaces)
	}); err != nil {
		return Project{}, false, fmt.Errorf("%w: %v", errProjectSaveFailed, err)
	}
	if updateErr != nil {
		return Project{}, true, updateErr
	}
	if !found {
		return Project{}, false, nil
	}
	if touchesGrant {
		if auditErr := recordConfigChange(o.Issuance, auditCredentialProjectGrant, id,
			projectUpdateGrantFieldNames(f), via, credID, presenceID); auditErr != nil {
			slog.Error("project grant updated but not recorded in the audit log", "id", id, "error", auditErr)
		}
	}
	o.notify()
	return updated, true, nil
}

// RotateToken issues a new token and withholds it on an audit failure,
// exactly as project_routes.go and ipc_projects.go already did (§7.6's
// "rotate_token withhold" stays unchanged) — moved here only so the record
// can carry the presence_id the gate minted.
func (o *ProjectOps) RotateToken(ctx context.Context, id, via, credID string) (string, bool, error) {
	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return "", false, err
	}
	grant, err := requireGate(o.Gate, ctx, "project.rotate_token",
		singleStringDigest("project.rotate_token", "project_id", id), fmt.Sprintf("rotate the token for the project %q", id))
	if err != nil {
		return "", false, err
	}

	var newPlaintext string
	var ok bool
	var genErr error
	if err := o.Store.With(func(s *Settings) {
		newPlaintext, ok, genErr = s.RotateProjectToken(id)
	}); err != nil {
		return "", false, fmt.Errorf("%w: %v", errProjectSaveFailed, err)
	}
	if genErr != nil {
		return "", false, fmt.Errorf("%w: %v", errProjectSaveFailed, genErr)
	}
	if !ok {
		return "", false, nil
	}
	if auditErr := recordProjectTokenRotated(o.Issuance, id, via, credID, grant.ID()); auditErr != nil {
		return "", false, fmt.Errorf("%w: %v", errProjectTokenUnrecorded, auditErr)
	}
	o.notify()
	return newPlaintext, true, nil
}
