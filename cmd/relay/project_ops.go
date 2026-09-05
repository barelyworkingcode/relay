package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/project"
)

// ProjectOps wraps project.ApplyCreate, project.ApplyUpdate and
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
// (ipc_projects.go) go through this rather than calling project.ApplyCreate /
// project.ApplyUpdate / RotateProjectToken directly, so neither can drift
// from the other's gate.
type ProjectOps struct {
	Store config.SettingsStore
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
// from a validation refusal (project.ApplyCreate/project.ApplyUpdate's own
// error) and from a presence/audit refusal, so a door can map each to its
// own status code without inspecting error text.
var errProjectSaveFailed = errors.New("failed to save settings")

// errProjectTokenUnrecorded means the rotation committed and the audit
// write then failed: the new plaintext is withheld (§7.6's rotate_token
// withhold, unchanged) but the old token is already dead either way, so a
// door must treat this as an internal failure, not a validation refusal.
var errProjectTokenUnrecorded = errors.New("the token was rotated but could not be recorded in the audit log, so it was not returned; rotate again")

// errProjectHosted refuses a skill regen for a project whose directory
// lives on a Host: proj.Path is meaningful only on that host, and
// project.ValidateShape already refuses generate_skill on a hosted project
// for the same reason, so the explicit "regen now" doors need the same
// check the automatic path gets for free.
var errProjectHosted = errors.New("project lives on a host; skills are generated on a console project's own filesystem only")

// errProjectHasNoPath refuses a skill regen for a console project with an
// empty Path (nothing for EmitSkills to write under).
var errProjectHasNoPath = errors.New("project has no path")

func (o *ProjectOps) notify() {
	if o.OnChange != nil {
		o.OnChange()
	}
}

// projectCreateDigest binds a project.grant grant to exactly the shape being
// created (§6.4). project_id is absent on create — there is none yet.
func projectCreateDigest(f project.CreateFields) presence.Digest {
	return presence.NewDigestBuilder("project.grant").
		StringField("project_id", false, "").
		StringSetField("allowed_mcp_ids", true, f.AllowedMcpIDs).
		StringSetMapField("allowed_tools", true, f.AllowedTools).
		StringMapField("access", true, f.Access).
		RawJSONMapField("context", true, rawJSONMapOf(f.Context)).
		BoolMapField("allow_external", true, f.AllowExternal).
		BoolField("allow_cwd_auth", true, f.AllowCwdAuth).
		StringField("kind", true, string(f.Kind)).
		StringField("host_id", true, f.HostID).
		StringField("path", true, f.Path).
		RawJSONSeqField("mounts", true, mountsDigestJSON(f.Mounts)).
		Build()
}

// projectUpdateDigest binds a project.grant grant to exactly the fields id's
// update touches, absent-aware (§6.4): a grant answered for one field must
// not be spendable on a request that also, or instead, touches another.
//
// This is deliberate: keep binding all eight fields below even after a
// future change narrows project.grant's GATE to fire only on
// allow_cwd_auth (ADR-018, blocked on the local cli-admin identity binding
// — see docs/decisions/018-configuration-is-a-capability-of-an-identity.md).
// The prompt authorises the request, not the reason the request was
// privileged, so shrinking this digest to the field that triggers the gate
// would let a grant answered for "turn on directory auth" redeem against
// "turn on directory auth AND set allowed_tools to * AND repoint path at
// /". Narrowing what gates and narrowing what the digest binds are two
// different questions; only the first one changes.
func projectUpdateDigest(id string, f project.UpdateFields) presence.Digest {
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
	if f.HostID != nil {
		b.StringField("host_id", true, *f.HostID)
	} else {
		b.StringField("host_id", false, "")
	}
	if f.Path != nil {
		b.StringField("path", true, *f.Path)
	} else {
		b.StringField("path", false, "")
	}
	if f.Mounts != nil {
		b.RawJSONSeqField("mounts", true, mountsDigestJSON(*f.Mounts))
	} else {
		b.RawJSONSeqField("mounts", false, nil)
	}
	return b.Build()
}

// projectUpdateTouchesGrant is AC-16c's "does" list: allowed_mcp_ids,
// allowed_tools, access, context, allow_external, allow_cwd_auth, kind,
// path or mounts. A request touching only name, chat_templates,
// session_folders, generate_skill, permission_policy, allowed_models or
// shell_templates must NOT prompt — none of those widen what a token
// reaches. disabled_tools is deliberately absent too: it is a denylist and
// can only narrow (§6.4). mounts is exactly as grant-widening as
// allowed_tools — a mount is a filesystem reach the token gains — so it is
// gated on the same footing, not treated as a lesser field because it
// shipped later.
func projectUpdateTouchesGrant(f project.UpdateFields) bool {
	return f.AllowedMcpIDs != nil || f.AllowedTools != nil || f.Access != nil ||
		f.Context != nil || f.AllowExternal != nil || f.AllowCwdAuth != nil || f.Kind != nil ||
		f.Path != nil || f.Mounts != nil || f.HostID != nil
}

func projectUpdateGrantFieldNames(f project.UpdateFields) []string {
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
	if f.HostID != nil {
		names = append(names, "host_id")
	}
	if f.Path != nil {
		names = append(names, "path")
	}
	if f.Mounts != nil {
		names = append(names, "mounts")
	}
	return names
}

func projectCreateGrantFieldNames(f project.CreateFields) []string {
	names := []string{"kind", "path"}
	if f.HostID != "" {
		names = append(names, "host_id")
	}
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
	if len(f.Mounts) > 0 {
		names = append(names, "mounts")
	}
	return names
}

// projectGrantUpdateReason names the actual act (§6.5.2). allow_cwd_auth
// gets its own sentence when it is the field being turned on: the ADR
// singles it out because turning it on hands the project's whole tool set
// to any process standing in the directory, with no token at all.
func projectGrantUpdateReason(id string, f project.UpdateFields, cwdAuthTurningOn bool) string {
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
func (o *ProjectOps) Create(ctx context.Context, f project.CreateFields, surfaces project.McpSurfaces, via, credID string) (config.Project, error) {
	if strings.TrimSpace(f.Name) == "" {
		return config.Project{}, fmt.Errorf("project name is required")
	}

	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return config.Project{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "project.grant", projectCreateDigest(f),
		fmt.Sprintf("create the project %q and grant it its initial scope", f.Name))
	if err != nil {
		return config.Project{}, err
	}

	var created config.Project
	var createErr error
	if err := o.Store.With(func(s *config.Settings) {
		created, createErr = project.ApplyCreate(s, f, surfaces)
	}); err != nil {
		return config.Project{}, fmt.Errorf("%w: %w", errProjectSaveFailed, err)
	}
	if createErr != nil {
		return config.Project{}, createErr
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
func (o *ProjectOps) Update(ctx context.Context, id string, f project.UpdateFields, surfaces func() project.McpSurfaces, via, credID string) (config.Project, bool, error) {
	touchesGrant := projectUpdateTouchesGrant(f)
	var presenceID string
	if touchesGrant {
		if err := requireIssuanceAuditor(o.Issuance); err != nil {
			return config.Project{}, false, err
		}
		cwdAuthTurningOn := f.AllowCwdAuth != nil && *f.AllowCwdAuth
		grant, err := requireGate(o.Gate, ctx, "project.grant", projectUpdateDigest(id, f),
			projectGrantUpdateReason(id, f, cwdAuthTurningOn))
		if err != nil {
			return config.Project{}, false, err
		}
		presenceID = grant.ID()
	}

	var updated config.Project
	var found bool
	var updateErr error
	if err := o.Store.With(func(s *config.Settings) {
		updated, found, updateErr = project.ApplyUpdate(s, id, f, surfaces)
	}); err != nil {
		return config.Project{}, false, fmt.Errorf("%w: %w", errProjectSaveFailed, err)
	}
	if updateErr != nil {
		return config.Project{}, true, updateErr
	}
	if !found {
		return config.Project{}, false, nil
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

// RegenSkill is the shared core behind the HTTP `POST
// /api/projects/{id}/regen_skill` route and the IPC `regen_project_skill`
// handler — both force a SKILL.md write regardless of the project's
// GenerateSkill flag (that flag only gates *automatic* regen), so unlike
// reconcileProjectSkill neither door gets IsHosted's refusal for free and
// each needs to ask here explicitly. Not gated: it writes only into
// .claude/skills, which project.grant already covers via allowed_tools/
// allowed_mcp_ids at the time GenerateSkill was turned on.
func (o *ProjectOps) RegenSkill(ctx context.Context, lister SkillLister, id string) (dir string, found bool, err error) {
	proj, _ := config.FindProjectByID(o.Store.Get(), id)
	if proj == nil {
		return "", false, nil
	}
	if proj.IsHosted() {
		return "", true, fmt.Errorf("%w: project %q lives on host %q", errProjectHosted, proj.Name, proj.HostID)
	}
	dir = projectSkillDir(*proj)
	if dir == "" {
		return "", true, errProjectHasNoPath
	}
	if _, err := EmitSkills(ctx, lister, *proj, dir, RegenAlways); err != nil {
		return "", true, err
	}
	return dir, true, nil
}

// Remove deletes a project and its on-disk skill directory (best-effort;
// EmitSkills only ever wrote there, so a hosted project — which cannot
// carry GenerateSkill — has nothing to clean up and projectSkillDir simply
// returns "" for one with an empty Path). The one core HTTP DELETE
// /api/projects/{id} and IPC delete_project share, so removal and its
// skill cleanup cannot drift between the two doors.
func (o *ProjectOps) Remove(id string) (removed config.Project, found bool, err error) {
	if err := o.Store.With(func(s *config.Settings) {
		proj, _ := config.FindProjectByID(s, id)
		if proj == nil {
			return
		}
		found = true
		removed = *proj
		s.RemoveProject(id)
	}); err != nil {
		return config.Project{}, false, fmt.Errorf("%w: %w", errProjectSaveFailed, err)
	}
	if !found {
		return config.Project{}, false, nil
	}
	if dir := projectSkillDir(removed); dir != "" {
		if err := RemoveSkill(dir); err != nil {
			slog.Warn("project skill remove failed", "project", removed.Name, "error", err)
		}
	}
	o.notify()
	return removed, true, nil
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
	if err := o.Store.With(func(s *config.Settings) {
		newPlaintext, ok, genErr = s.RotateProjectToken(id)
	}); err != nil {
		return "", false, fmt.Errorf("%w: %w", errProjectSaveFailed, err)
	}
	if genErr != nil {
		return "", false, fmt.Errorf("%w: %w", errProjectSaveFailed, genErr)
	}
	if !ok {
		return "", false, nil
	}
	if auditErr := recordProjectTokenRotated(o.Issuance, id, via, credID, grant.ID()); auditErr != nil {
		return "", false, fmt.Errorf("%w: %w", errProjectTokenUnrecorded, auditErr)
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
func (o *ProjectOps) DescribeGrant(s *config.Settings, proj *config.Project) grantView {
	v := newGrantView(s, *proj)
	v.Enrolments = nil
	return v
}

// NarrowForEnrolment is the one core behind the remote listener's
// configuration plane. It is deliberately NOT gated: project.NarrowsOnly makes a
// widening unrepresentable, so this is not one of the acts ADR-017
// decision 3 names, and a presence prompt reachable from a VM would be a
// prompt the caller cannot see and the host did not ask for (§9.2) — a
// certificate resolved by TLS is the authorization, the same way a project
// token already is for CallTool.
//
// It reuses project.ApplyUpdate rather than writing a second merge path, so
// every existing validation rule — project.ValidateShape, the package's own
// permission and tool-pattern validation, project.ValidateGrants — applies
// identically to a remote's own edit and to an operator's.
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
// The same is true one step short of a refusal: project.NarrowsOnly accepts a
// request that asks for exactly what is already stored (that is not a
// widening either), and project.ApplyUpdate's mutators write unconditionally
// once a non-nil field pointer reaches them. Left unchecked, a certificate
// resending an already-applied NarrowGrant — deliberately, or simply
// because it does not track what it already asked for — would reseal every
// sealed token in the file and append a fresh config_change on every
// resend, an unbounded write and an unbounded audit-log entry from a path
// with no presence prompt to slow it down. project.NarrowingIsNoop is the second
// half of "leave settings.json exactly as it was": it stands between
// project.NarrowsOnly's yes and project.ApplyUpdate's unconditional write.
func (o *ProjectOps) NarrowForEnrolment(
	ctx context.Context, projectID string, f project.NarrowFields,
	caller bridge.RemoteCaller, surfaces func() project.McpSurfaces,
) (config.Project, []string, error) {
	if err := requireIssuanceAuditor(o.Issuance); err != nil {
		return config.Project{}, nil, err
	}

	var updated config.Project
	var found, noop bool
	err := config.WithDeclinable(o.Store, func(s *config.Settings) error {
		// Resolved INSIDE the callback, not from a value the caller
		// captured earlier: the store's lock is what makes "narrower than
		// what is stored right now" an answerable question rather than a
		// race with whatever else touched this project between the request
		// arriving and this closure running.
		proj, _ := config.FindProjectByID(s, projectID)
		if proj == nil {
			return fmt.Errorf("project %q no longer exists", projectID)
		}
		if err := project.NarrowsOnly(*proj, f); err != nil {
			return err
		}
		if project.NarrowingIsNoop(*proj, f) {
			found, noop = true, true
			updated = *proj
			return errNarrowingIsNoop
		}
		var applyErr error
		updated, found, applyErr = project.ApplyUpdate(s, projectID, project.NarrowUpdateFields(*proj, f), surfaces)
		return applyErr
	})
	if err != nil && !noop {
		return config.Project{}, nil, err
	}
	if !found {
		return config.Project{}, nil, fmt.Errorf("project %q no longer exists", projectID)
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

	changed := project.NarrowFieldNames(f)
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
// see project.NarrowingIsNoop's doc comment for why the caller sees success.
var errNarrowingIsNoop = errors.New("narrowing request matches the stored grant")
