package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

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
// access, context and allow_external ARE what that boundary
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
	Queue *config.CommandQueue
	// Gate is the presence check Create, Update and RotateToken demand
	// before they touch the store. A nil Gate refuses all three — see
	// requireGate.
	Gate *presence.Gate
	// Issuance records the config_change every gated create/update leaves
	// and the credential_issued a rotation is (§7.5), and is the hard
	// dependency §7.4 checks before Gate.
	Issuance IssuanceAuditor
	// SessionCleanup ends and terminates a deleted project's live sessions
	// (plan-broker-and-sessions.md §2 C5). Its zero value is a legitimate
	// "no session-host wiring" -- see cleanupProject's own ready() guard,
	// which every caller not wiring session routes relies on.
	SessionCleanup sessionRouteDeps
}

func (o *ProjectOps) runQueued(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.Do(ctx, func(context.Context) error { return fn() })
}

// runCommitted is runQueued for steps whose results the caller reads after
// return: an admitted step is never abandoned on caller cancellation, so the
// closure's outputs cannot race the worker.
func (o *ProjectOps) runCommitted(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.DoCommitted(ctx, func(context.Context) error { return fn() })
}

// errProjectSaveFailed distinguishes an internal settings-write failure
// from a validation refusal (project.ApplyCreate/project.ApplyUpdate's own
// error) and from a presence/audit refusal, so a door can map each to its
// own status code without inspecting error text.
var errProjectSaveFailed = errors.New("failed to save settings")

// errProjectChangedDuringApproval means the record moved between the
// presence decision and the queued commit far enough that the request now
// widens a field its approval did not cover. Nothing was written; the caller
// retries against the current record.
var errProjectChangedDuringApproval = errors.New("project changed while the request was being approved; retry the update")

// errProjectUpdateTargetMissing declines the write when Update's id matches
// no project. It never leaves Update: the caller sees found == false.
var errProjectUpdateTargetMissing = errors.New("project to update not found")

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
// This is deliberate: keep binding every field below even after a future
// change narrows project.grant's GATE to fire on fewer of them (ADR-018,
// blocked on the local cli-admin identity binding — see
// docs/decisions/018-configuration-is-a-capability-of-an-identity.md). The
// prompt authorises the request, not the reason the request was
// privileged, so shrinking this digest to only the field that triggers the
// gate would let a grant answered for "widen allowed_tools" redeem against
// "widen allowed_tools AND repoint path at /". Narrowing what gates and
// narrowing what the digest binds are two different questions; only the
// first one changes.
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
	if len(f.Mounts) > 0 {
		names = append(names, "mounts")
	}
	return names
}

// projectGrantUpdateReason names the actual act (§6.5.2), naming only the
// fields project.UpdateWidensGrant found to actually widen the grant — a
// request that also resends several unchanged or narrowed fields must not
// read as widening all of them.
func projectGrantUpdateReason(id string, widened []string) string {
	return fmt.Sprintf("widen the grant for the project %q (%s)", id, strings.Join(widened, ", "))
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
	if err := o.runCommitted(ctx, func() error {
		if err := o.Store.With(func(s *config.Settings) {
			created, createErr = project.ApplyCreate(s, f, surfaces)
		}); err != nil {
			return fmt.Errorf("%w: %w", errProjectSaveFailed, err)
		}
		if createErr != nil {
			return createErr
		}
		if auditErr := recordConfigChange(o.Issuance, auditCredentialProjectGrant, created.ID,
			projectCreateGrantFieldNames(f), via, credID, grant.ID()); auditErr != nil {
			slog.Error("project created but not recorded in the audit log", "id", created.ID, "error", auditErr)
		}
		return nil
	}); err != nil {
		if createErr != nil {
			return config.Project{}, createErr
		}
		return config.Project{}, err
	}
	return created, nil
}

// Update gates only when the request actually widens the grant
// (project.UpdateWidensGrant) — AC-16c requires that a rename or a
// chat-template edit not prompt, and ADR-18 decision 1 requires the same of
// an unchanged resend or a pure narrowing: a caller like the Settings
// window that always sends the whole record must not have that resend read
// as widening every field it happens to carry.
//
// stored is read before the queue and only decides whether to prompt: a
// human prompt must not hold the queue lane, so approval is necessarily made
// against a snapshot that another request may outdate. The queued step
// therefore recomputes the widening against the live record and refuses
// (errProjectChangedDuringApproval, nothing written) when it names any field
// the approval did not cover. A live widening within the approved set commits
// as approved; a stale snapshot that over-prompted needs no handling.
//
// The recheck and ApplyUpdate deliberately share one surfaces fetch: a
// schema lost between the prompt and the write, after the gate ignored a
// stored derived field, then shows up as an unapproved context widening and
// is refused (unless the approval already covered context), rather than the
// recheck and the write each seeing a different schema.
func (o *ProjectOps) Update(ctx context.Context, id string, f project.UpdateFields, surfaces func() project.McpSurfaces, via, credID string) (config.Project, bool, error) {
	var stored config.Project
	if existing, _ := config.FindProjectByID(config.FreshSettings(o.Store), id); existing != nil {
		stored = *existing
	}
	var gateSurfaces project.McpSurfaces
	if f.Context != nil {
		gateSurfaces = surfaces()
	}
	widened := project.UpdateWidensGrant(stored, f, gateSurfaces)
	touchesGrant := len(widened) > 0
	var presenceID string
	if touchesGrant {
		if err := requireIssuanceAuditor(o.Issuance); err != nil {
			return config.Project{}, false, err
		}
		grant, err := requireGate(o.Gate, ctx, "project.grant", projectUpdateDigest(id, f),
			projectGrantUpdateReason(id, widened))
		if err != nil {
			return config.Project{}, false, err
		}
		presenceID = grant.ID()
	}

	var updated config.Project
	var found bool
	var updateErr error
	if err := o.runCommitted(ctx, func() error {
		queued := sync.OnceValue(surfaces)
		if err := config.WithDeclinable(o.Store, func(s *config.Settings) error {
			if live, _ := config.FindProjectByID(s, id); live != nil {
				var recheckSurfaces project.McpSurfaces
				if f.Context != nil {
					recheckSurfaces = queued()
				}
				if unapproved := project.UnapprovedWidening(project.UpdateWidensGrant(*live, f, recheckSurfaces), widened); len(unapproved) > 0 {
					return fmt.Errorf("%w: %s", errProjectChangedDuringApproval, strings.Join(unapproved, ", "))
				}
			}
			updated, found, updateErr = project.ApplyUpdate(s, id, f, queued)
			if updateErr != nil {
				return updateErr
			}
			if !found {
				return errProjectUpdateTargetMissing
			}
			return nil
		}); err != nil {
			switch {
			case updateErr != nil:
				return updateErr
			case errors.Is(err, errProjectUpdateTargetMissing):
				return nil
			case errors.Is(err, errProjectChangedDuringApproval):
				return err
			}
			return fmt.Errorf("%w: %w", errProjectSaveFailed, err)
		}
		if touchesGrant {
			if auditErr := recordConfigChange(o.Issuance, auditCredentialProjectGrant, id,
				widened, via, credID, presenceID); auditErr != nil {
				slog.Error("project grant updated but not recorded in the audit log", "id", id, "error", auditErr)
			}
		}
		return nil
	}); err != nil {
		if updateErr != nil {
			return config.Project{}, true, updateErr
		}
		return config.Project{}, false, err
	}
	if !found {
		return config.Project{}, false, nil
	}
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
	proj, _ := config.FindProjectByID(config.FreshSettings(o.Store), id)
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
	var clearedDefaults []config.ProjectMode
	if err := o.runQueued(context.Background(), func() error {
		return o.Store.With(func(s *config.Settings) {
			proj, _ := config.FindProjectByID(s, id)
			if proj == nil {
				return
			}
			found = true
			removed = *proj
			clearedDefaults = s.DefaultModesFor(id)
			s.RemoveProject(id)
		})
	}); err != nil {
		return config.Project{}, false, fmt.Errorf("%w: %w", errProjectSaveFailed, err)
	}
	if !found {
		return config.Project{}, false, nil
	}
	if len(clearedDefaults) > 0 {
		slog.Info("project removed; default cleared", "project_id", id, "modes", clearedDefaults)
	}
	if dir := projectSkillDir(removed); dir != "" {
		if err := RemoveSkill(dir); err != nil {
			slog.Warn("project skill remove failed", "project", removed.Name, "error", err)
		}
	}
	// Best-effort, after the project record itself is already gone: every
	// live session belonging to it gets /terminate'd on the host and its
	// launch identity, model key and ledger record cleaned up
	// (plan-broker-and-sessions.md §2 C5's model-key "revoked on ... project
	// delete"). A zero SessionCleanup (a caller with no session-host wiring)
	// is a no-op, guarded by sessionRouteDeps.ready() inside cleanupProject.
	o.SessionCleanup.cleanupProject(id)
	return removed, true, nil
}

// SetDefaultProject makes projectID the default project for mode, or clears
// it when projectID is "", and returns the effective defaults afterwards. Not
// gated and not audited: a default is a label, never a grant. A refusal
// declines the write so settings.json stays byte-identical, and comes back
// as config.ErrInvalidDefaultProject, never errProjectSaveFailed, so a door
// can tell it from a store failure.
func (o *ProjectOps) SetDefaultProject(ctx context.Context, mode config.ProjectMode, projectID string) (config.DefaultProjects, error) {
	var refusal error
	var effective config.DefaultProjects
	if err := o.runCommitted(ctx, func() error {
		return config.WithDeclinable(o.Store, func(s *config.Settings) error {
			if refusal = s.SetDefaultProject(mode, projectID); refusal != nil {
				return refusal
			}
			effective = effectiveDefaultProjects(s)
			return nil
		})
	}); err != nil {
		if refusal != nil {
			return config.DefaultProjects{}, refusal
		}
		return config.DefaultProjects{}, fmt.Errorf("%w: %w", errProjectSaveFailed, err)
	}
	return effective, nil
}

func effectiveDefaultProjects(s *config.Settings) config.DefaultProjects {
	return config.DefaultProjects{
		Home: s.DefaultProjectFor(config.ProjectModeHome),
		Work: s.DefaultProjectFor(config.ProjectModeWork),
	}
}

// SetDisabledTools replaces the disabled-tool list one MCP has on a project.
// Not gated: like RegenSkill it narrows what the project's client sees and
// cannot widen a grant. The record is resolved and mutated inside one queued
// step, and found is false when the project no longer exists.
func (o *ProjectOps) SetDisabledTools(ctx context.Context, id, mcpID string, disabled []string) (updated config.Project, found bool, err error) {
	if err := o.runCommitted(ctx, func() error {
		return o.Store.With(func(s *config.Settings) {
			if proj, _ := config.FindProjectByID(s, id); proj == nil {
				return
			}
			s.UpdateProjectDisabledTools(id, mcpID, disabled)
			if proj, _ := config.FindProjectByID(s, id); proj != nil {
				updated = *proj
				found = true
			}
		})
	}); err != nil {
		return config.Project{}, false, fmt.Errorf("%w: %w", errProjectSaveFailed, err)
	}
	return updated, found, nil
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
	if err := o.runCommitted(ctx, func() error {
		if err := o.Store.With(func(s *config.Settings) {
			newPlaintext, ok, genErr = s.RotateProjectToken(id)
		}); err != nil {
			return fmt.Errorf("%w: %w", errProjectSaveFailed, err)
		}
		if genErr != nil {
			return fmt.Errorf("%w: %w", errProjectSaveFailed, genErr)
		}
		if !ok {
			return nil
		}
		if auditErr := recordProjectTokenRotated(o.Issuance, id, via, credID, grant.ID()); auditErr != nil {
			return fmt.Errorf("%w: %w", errProjectTokenUnrecorded, auditErr)
		}
		return nil
	}); err != nil {
		if genErr != nil {
			return "", false, fmt.Errorf("%w: %w", errProjectSaveFailed, genErr)
		}
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
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
	err := o.runCommitted(ctx, func() error {
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
			return err
		}
		if !found || noop {
			return nil
		}
		changed := project.NarrowFieldNames(f)
		if auditErr := recordConfigChangeRemote(o.Issuance, auditCredentialProjectGrant, projectID, changed, caller); auditErr != nil {
			slog.Error("a remote narrowed its own grant but the change was not recorded in the audit log",
				"project_id", projectID, "client_id", caller.ClientID, "error", auditErr)
		}
		return nil
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
	return updated, changed, nil
}

// errNarrowingIsNoop is withDeclinable's only lever for skipping a write
// that is not a refusal: a callback error is the sole signal it honours.
// NarrowForEnrolment unwraps this one immediately and never returns it —
// see project.NarrowingIsNoop's doc comment for why the caller sees success.
var errNarrowingIsNoop = errors.New("narrowing request matches the stored grant")
