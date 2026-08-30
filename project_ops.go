package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"relaygo/bridge"
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

// DescribeGrant is the RemoteConfigurer half of ADR-018 decision 4's read
// side: the caller's own posture, built through the same newGrantView
// `relay grant` uses, so an operator and the enrolment describing itself
// read the identical record for everything BUT Enrolments (ADR-018
// decision 5, symmetrically).
//
// Enrolments is cleared before returning: newGrantView populates it with
// every enrolment granting this profile, for the operator-facing `relay
// grant` — going out over the wire verbatim would let a remote caller
// describing its OWN posture enumerate its siblings, learning their
// client_id and cli_admin state. The reachability boundary (a caller
// cannot ACT on another enrolment's grant) already holds; this is what
// keeps the read side to "the caller's own posture" the spec defines it
// as. handleRemoteNarrowGrant's result rides through this same method, so
// it is covered without a separate fix.
func (o *ProjectOps) DescribeGrant(s *Settings, proj *Project) grantView {
	v := newGrantView(s, *proj)
	v.Enrolments = nil
	return v
}

// narrowUpdateFields carries a NarrowGrant request's already-validated
// fields into applyProjectUpdate's patch shape. The two structs' pointer
// fields share exactly the same underlying types by construction — see
// remoteNarrowFields' own doc comment — so this is a relabelling, not a
// conversion, and it sets nothing narrowsOnly did not already clear.
func narrowUpdateFields(f remoteNarrowFields) projectUpdateFields {
	return projectUpdateFields{
		AllowedMcpIDs: f.AllowedMcpIDs,
		AllowedTools:  f.AllowedTools,
		Access:        f.Access,
		AllowExternal: f.AllowExternal,
	}
}

// NarrowForEnrolment is the one core behind the remote listener's
// configuration plane. It is deliberately NOT gated: narrowsOnly makes a
// widening unrepresentable, so this is not one of the acts ADR-017
// decision 3 names, and a presence prompt reachable from a VM would be a
// prompt the caller cannot see and the host did not ask for (§9.2) — a
// certificate resolved by TLS is the authorization, the same way a project
// token already is for CallTool.
//
// It reuses applyProjectUpdate rather than writing a second merge path, so
// every existing validation rule — validateProjectShape,
// validateProjectPermissions, validateToolPattern, ValidateProjectGrants —
// applies identically to a remote's own edit and to an operator's.
//
// This is deliberate: the mutation goes through withDeclinable, not the
// plain Store.With every other ops core here uses. Store.With resaves
// (and reseals every sealed token) even when its callback changes nothing
// — the right default for a door a human just drove, and the wrong one for
// a request a VM can send at will: a widening this function refuses must
// leave settings.json exactly as it was, not merely logically equivalent,
// or a script hammering a refused NarrowGrant would spend the settings
// file's one-writer-at-a-time window for nothing every time it tried.
//
// The same is true one step short of a refusal: narrowsOnly accepts a
// request that asks for exactly what is already stored (that is not a
// widening either), and applyProjectUpdate's mutators write unconditionally
// once a non-nil field pointer reaches them. Left unchecked, a certificate
// resending an already-applied NarrowGrant — deliberately, or simply
// because it does not track what it already asked for — would reseal every
// sealed token in the file and append a fresh config_change on every
// resend, an unbounded write and an unbounded audit-log entry from a path
// with no presence prompt to slow it down. narrowingIsNoop is the second
// half of "leave settings.json exactly as it was": it stands between
// narrowsOnly's yes and applyProjectUpdate's unconditional write.
func (o *ProjectOps) NarrowForEnrolment(
	ctx context.Context, projectID string, f remoteNarrowFields,
	caller bridge.RemoteCaller, surfaces func() McpSurfaces,
) (Project, []string, error) {
	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return Project{}, nil, err
	}

	var updated Project
	var found, noop bool
	err := withDeclinable(o.Store, func(s *Settings) error {
		// Resolved INSIDE the callback, not from a value the caller
		// captured earlier: the store's lock is what makes "narrower than
		// what is stored right now" an answerable question rather than a
		// race with whatever else touched this project between the request
		// arriving and this closure running.
		proj, _ := s.findProjectByID(projectID)
		if proj == nil {
			return fmt.Errorf("project %q no longer exists", projectID)
		}
		if err := narrowsOnly(*proj, f); err != nil {
			return err
		}
		if narrowingIsNoop(*proj, f) {
			found, noop = true, true
			updated = *proj
			return errNarrowingIsNoop
		}
		var applyErr error
		updated, found, applyErr = applyProjectUpdate(s, projectID, narrowUpdateFields(f), surfaces)
		return applyErr
	})
	if err != nil && !noop {
		return Project{}, nil, err
	}
	if !found {
		return Project{}, nil, fmt.Errorf("project %q no longer exists", projectID)
	}
	if noop {
		// Nothing changed: no write happened (withDeclinable declined it
		// above) and there is nothing for the audit log to say — a record
		// reading "cli_admin=on changed allowed_tools" would be false. This
		// is reported to the caller as an ordinary success with an empty
		// Changed list, not as an error: the caller asked for exactly what
		// it already has, which is not a mistake.
		return updated, nil, nil
	}

	changed := projectUpdateGrantFieldNames(narrowUpdateFields(f))
	// Reported and not undone, the same balance EnrolmentOps.Update and
	// Revoke strike: a narrowing act has no side artifact to roll back, and
	// refusing to narrow because the log is broken would make a failing
	// disk the reason a remote keeps a grant it was trying to shed.
	if auditErr := recordConfigChangeRemote(o.Issuance, auditCredentialProjectGrant, projectID, changed, caller); auditErr != nil {
		slog.Error("a remote narrowed its own grant but the change was not recorded in the audit log",
			"project_id", projectID, "client_id", caller.ClientID, "error", auditErr)
	}
	o.notify()
	return updated, changed, nil
}

// errNarrowingIsNoop is withDeclinable's only lever for skipping a write
// that is not a refusal: a callback error is the sole signal it honours.
// NarrowForEnrolment unwraps this one immediately and never returns it —
// see narrowingIsNoop's doc comment for why the caller sees success.
var errNarrowingIsNoop = errors.New("narrowing request matches the stored grant")
