package main

// R-S4a: the launch-authorisation decision core (spec-session-host.md §3.2,
// plan-broker-and-sessions.md C1/C5/C7). AuthorizeLaunch runs every SH §3.2
// check and, only if every one passes, builds the LaunchSpec relay will hand
// to relay-sessions' POST /launch. Nothing here calls that endpoint, mints
// the launch-identity secret Hello needs, or registers an HTTP route — that
// is R-S4b's job, using the LaunchResult this file produces.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/project"
	"github.com/barelyworkingcode/relay/internal/service"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
	"github.com/google/uuid"
)

// Session kinds. "chat" (not "chat_tools") is the resolution of a real
// inconsistency between plan-broker-and-sessions.md's C5 wire sample
// (`"kind": "pty | claude | pi | chat"`) and spec-session-host.md §3.1's
// prose (`pty` | `claude` | `pi` | `chat_tools`) — neither hostapi nor any
// landed unit fixes the string yet (internal/sessions/hostapi/launch_test.go
// only ever uses "pty"), so this picks C5's own wire-contract sample as
// ground truth over a parenthetical aside in the flow-diagram prose. The
// wire string lives in this one named constant, not inlined at each call
// site, so resolving that document conflict later stays a one-line edit.
const (
	KindPTY    = "pty"
	KindClaude = "claude"
	KindPi     = "pi"
	KindChat   = "chat"
)

func validKind(k string) bool {
	switch k {
	case KindPTY, KindClaude, KindPi, KindChat:
		return true
	default:
		return false
	}
}

// LaunchCaller is who asked relay to launch a session: a frontend launch
// identity (eve, relayScheduler, admitted the way frontendCredentialAuth
// admits one), a resolved bearer control credential, or the operator at the
// bridge socket. R-S4b resolves whichever one before calling AuthorizeLaunch —
// exactly one field is set, and neither set is this package's job to
// authenticate, only to authorize.
type LaunchCaller struct {
	Identity   *service.Identity
	Credential *config.APICredential
	// Operator is set only by the bridge's sandbox door, for a request whose
	// connection reached the 0600 socket and is not a member of any relay
	// session. No other door may set it: it grants execute unconditionally.
	Operator *OperatorCaller
}

// OperatorCaller is the audit attribution of an operator's CLI process: there
// is no credential to name, so the peer's own process names are the record.
type OperatorCaller struct {
	PID          int
	Proc, Parent string
}

// callerGrantsExecute reports whether c holds control.ClassExecute — the
// class sessionRouteClasses assigns POST /api/terminals and POST
// /api/sessions (api_credential.go), and the check the required test matrix
// names directly ("no execute → 403", "proxy-only bearer 403"). A genuine
// frontend launch identity holds frontendConsumerClasses, the same set
// credentialAuthorizer.Authorize grants it; a bearer credential answers for
// itself via config.APICredential.Grants, which is nil-safe by construction
// (an absent or empty Classes grants nothing). Neither field set, or an
// identity that does not actually hold the frontend capability, grants
// nothing — this function does not assume its caller was pre-validated by
// frontendCredentialAuth, since that is exactly the assumption a bypass
// would exploit.
func callerGrantsExecute(c LaunchCaller) bool {
	if c.Operator != nil {
		return true
	}
	if c.Identity != nil && c.Identity.Allows(service.OpFrontendSocket) {
		return slices.Contains(frontendConsumerClasses, control.ClassExecute)
	}
	if c.Credential != nil {
		return c.Credential.Grants(control.ClassExecute)
	}
	return false
}

// callerAuditActor mirrors control.go's own control_decision actor shape
// (AuditActorControl / AuditAuthToken with a CredID) rather than inventing a
// second shape for the same kind of caller: launchIdentityCredentialID
// already produces exactly C4's "launch:service:eve" convention for an
// identity caller, and a bearer credential's own id is the CredID a
// control_decision would carry for the same request.
func callerAuditActor(c LaunchCaller) audit.AuditActor {
	switch {
	case c.Operator != nil:
		return audit.AuditActor{Kind: audit.AuditActorOperator, Auth: audit.AuditAuthNone, PID: c.Operator.PID, Proc: c.Operator.Proc, Parent: c.Operator.Parent}
	case c.Identity != nil:
		return audit.AuditActor{Kind: audit.AuditActorControl, Auth: audit.AuditAuthToken, CredID: launchIdentityCredentialID(*c.Identity)}
	case c.Credential != nil:
		return audit.AuditActor{Kind: audit.AuditActorControl, Auth: audit.AuditAuthToken, CredID: c.Credential.ID}
	default:
		return audit.AuditActor{Kind: audit.AuditActorUnknown, Auth: audit.AuditAuthNone}
	}
}

// LaunchRequest is what a caller asks relay to launch, decoded by R-S4b from
// either eve's POST /api/terminals body (Kind == KindPTY: TemplateID,
// Cols, Rows) or its POST /api/sessions body (Kind == KindClaude/KindPi/
// KindChat: Model, ClientSettings, SystemPrompt, AppendClaudeMd) —
// session-messages.js's handleCreateSession is the wire shape for the
// latter. Every request names a project: AuthorizeLaunch refuses one without
// (project_required), because the project's allowed_templates is what permits
// a template at all.
type LaunchRequest struct {
	Caller LaunchCaller

	// SessionID is empty for a fresh launch (AuthorizeLaunch mints a UUIDv4)
	// and the existing session's id for a resume (SH §3.4): the id never
	// changes across a resume, only the secret and model key do. A caller
	// that sets SessionID without Resume is refused outright — only
	// AuthorizeLaunch itself names a session id for a fresh launch.
	SessionID string
	Resume    bool

	ProjectID string
	Kind      string
	Directory string
	Name      string

	// pty only.
	TemplateID string
	Cols, Rows int
	// PersistSession names an existing tmux session of a hosted project's
	// persist template to reattach to; empty mints the next name.
	PersistSession string

	// claude/pi/chat only.
	Model          string
	ClientSettings json.RawMessage
	SystemPrompt   string
	AppendClaudeMd bool

	ExtraArgs []string
}

// LaunchRefusal is a SH §3.2 check's refusal. Status is what R-S4b should
// answer eve with; every authorization-check refusal is 403, deliberately
// never distinguishing "project not found" from "project is remote" or
// "template not in the effective set" from "template exists but is not
// yours" in the message — the same not-an-oracle discipline
// frontendCredentialAuth already applies to its own 401s. A structurally
// invalid request (unknown kind, an ad-hoc pty with no template) is 400
// instead: that is a caller mistake, not a boundary probe.
type LaunchRefusal struct {
	Status  int
	Code    string
	Message string
	// Audit is a complete, ready-to-record session_launch event: a refusal
	// is fully decided at this layer, unlike an accepted request, whose
	// session_launch completion depends on R-S4b's round trip to
	// relay-sessions (see LaunchResult.AuditFields's doc comment).
	Audit audit.AuditEvent
}

func (r *LaunchRefusal) Error() string { return r.Message }

func forbidden(code, message string, fields sessionLaunchAuditFields) *LaunchRefusal {
	return &LaunchRefusal{Status: 403, Code: code, Message: message,
		Audit: newSessionLaunchAuditEvent(fields, audit.AuditOutcomeDenied, message)}
}

func invalidRequest(code, message string, fields sessionLaunchAuditFields) *LaunchRefusal {
	return &LaunchRefusal{Status: 400, Code: code, Message: message,
		Audit: newSessionLaunchAuditEvent(fields, audit.AuditOutcomeError, message)}
}

// sessionLaunchAuditFields is every C4 session_launch field this layer can
// determine on its own. Outcome is deliberately not part of it: a refusal's
// outcome (denied) is known here, but an accepted request's outcome (ok vs
// error) depends on relay-sessions' own /launch response, which only R-S4b
// ever sees — so R-S4b, not this package, calls newSessionLaunchAuditEvent
// a second time for the accepted case, once it knows how the host answered.
type sessionLaunchAuditFields struct {
	Actor       audit.AuditActor
	ProjectID   string
	ProjectName string
	SessionID   string
	Kind        string
	TemplateID  string
	Directory   string
	Sandbox     bool
}

type sessionLaunchAuditArgs struct {
	SessionID   string `json:"session_id,omitempty"`
	SessionKind string `json:"session_kind,omitempty"`
	TemplateID  string `json:"template_id,omitempty"`
	Directory   string `json:"directory,omitempty"`
	Sandbox     bool   `json:"sandbox"`
}

func newSessionLaunchAuditEvent(f sessionLaunchAuditFields, outcome, errMsg string) audit.AuditEvent {
	actor := f.Actor
	actor.ProjectID = f.ProjectID
	actor.ProjectName = f.ProjectName
	args, _ := json.Marshal(sessionLaunchAuditArgs{
		SessionID: f.SessionID, SessionKind: f.Kind, TemplateID: f.TemplateID,
		Directory: f.Directory, Sandbox: f.Sandbox,
	})
	return audit.AuditEvent{
		ID:      audit.NewAuditID(),
		TS:      time.Now(),
		Event:   audit.AuditEventSessionLaunch,
		Actor:   actor,
		Outcome: outcome,
		Error:   errMsg,
		Args:    args,
	}
}

// LaunchResult is what AuthorizeLaunch built once every check passed.
type LaunchResult struct {
	SessionID string
	Spec      hostapi.LaunchRequest

	// Ledger is nil for a pty launch — terminals are never persisted (C5).
	// R-S4b Puts it only after relay-sessions actually answers 201; a
	// record for a launch the host refused would claim a session that
	// never existed.
	Ledger *ledger.Record

	// ModelKeyLabel is "" when no key was minted (Spec.ModelKey == ""), and
	// "session:<id>" otherwise — the exact label ModelKeyTable.Revoke needs
	// at teardown.
	ModelKeyLabel string

	// AuditFields is handed to newSessionLaunchAuditEvent by R-S4b once the
	// host round trip completes (see sessionLaunchAuditFields's doc
	// comment) — exported so that call can happen outside this package's
	// own file without re-deriving any of it.
	AuditFields sessionLaunchAuditFields
}

// mintModelKey is an indirection, not a call: Mint fails only when the
// kernel refuses entropy, which no test can provoke, and the refusal it
// produces is the one that must not leave a session's artifacts behind.
var mintModelKey = func(t *ModelKeyTable, projectID, label string) (string, error) {
	return t.Mint(projectID, label)
}

// AuthorizeLaunch runs every SH §3.2 check in order — caller, project,
// directory, template/model, permission policy — and, only once every one
// has passed, mints the session id (unless req.Resume supplies one already)
// and the model key (last, strictly after authorization: minting one that a
// later check could still refuse would be exactly the hygiene issue the
// task calls out) and builds the LaunchSpec.
//
// store, not a pre-resolved *config.Settings: modelAllowedForProject
// (frontend_model_guard.go, reused rather than reimplemented) takes a
// config.SettingsStore, and a caller with only a store is the shape R-S4b
// actually has.
//
// sessions is consulted only for req.Resume: a resumed session id must name
// an existing, dormant record whose own project_id matches req.ProjectID —
// otherwise a caller could squat a live session id (minting a second model
// key under the label ModelKeyTable.Revoke keys on, so revoking the
// squatter's key also revokes the victim's) or resume one project's session
// under another project's name.
func AuthorizeLaunch(store config.SettingsStore, modelKeys *ModelKeyTable, sessions *ledger.Ledger, req LaunchRequest) (*LaunchResult, *LaunchRefusal) {
	settings := config.FreshSettings(store)

	baseFields := sessionLaunchAuditFields{Actor: callerAuditActor(req.Caller), ProjectID: req.ProjectID, Kind: req.Kind}

	if !callerGrantsExecute(req.Caller) {
		return nil, forbidden("caller_not_authorized", "caller does not hold execute on the frontend socket", baseFields)
	}

	if !validKind(req.Kind) {
		return nil, invalidRequest("invalid_kind", fmt.Sprintf("unknown session kind %q", req.Kind), baseFields)
	}

	// Only AuthorizeLaunch mints a session id for a fresh launch (below); a
	// caller naming one itself without Resume could squat an id that is
	// already live under another caller's session.
	if req.SessionID != "" && !req.Resume {
		return nil, forbidden("session_id_not_allowed", "a fresh launch may not name a session id", baseFields)
	}

	if req.Resume {
		rec, ok := sessions.Get(req.SessionID)
		if !ok || rec.State != ledger.StateDormant || rec.ProjectID != req.ProjectID {
			return nil, forbidden("session_not_resumable", "session is not a dormant session of this project", baseFields)
		}
	}

	// Every launch names a project: a project's allowed_templates is what
	// says which template it may run, so a launch without one has none.
	if req.ProjectID == "" {
		return nil, forbidden("project_required", fmt.Sprintf("%s sessions require a project", req.Kind), baseFields)
	}

	var proj *config.Project
	if req.ProjectID != "" {
		p, _ := config.FindProjectByID(settings, req.ProjectID)
		// Not found and remote refuse identically and with the same
		// message: distinguishing them would be an existence oracle, the
		// same reasoning frontendCredentialAuth's identical 401s follow.
		if p == nil || p.IsRemote() {
			return nil, forbidden("project_not_available", "project is not available for a session launch", baseFields)
		}
		proj = p
		baseFields.ProjectName = proj.Name
	}

	directory, refusal := resolveDirectory(proj, req.Directory, baseFields)
	if refusal != nil {
		return nil, refusal
	}
	baseFields.Directory = directory

	var tmpl *config.TerminalTemplate
	if req.Kind == KindPTY {
		t, ok := findTemplate(settings, proj, req.TemplateID)
		if !ok {
			return nil, forbidden("template_not_allowed", fmt.Sprintf("terminal template %q is not available for this project", req.TemplateID), baseFields)
		}
		tmpl = &t
		baseFields.TemplateID = tmpl.ID
	} else {
		if req.Kind == KindPi && proj != nil && proj.IsHosted() {
			return nil, forbidden("provider_not_available_on_host", `provider "pi" is not available on a host project`, baseFields)
		}
		if !kindAllowed(settings, proj, req.Kind) {
			return nil, forbidden("template_not_allowed", fmt.Sprintf("template %q is not available for this project", kindTemplateIDs[req.Kind]), baseFields)
		}
		// Resume is exempt: it sends the session's stored model back through here, and a stored model may be blank.
		if req.Kind == KindChat && !req.Resume && strings.TrimSpace(req.Model) == "" {
			return nil, invalidRequest("model_required", chatModelRequiredMessage(req.Name), baseFields)
		}
		if req.Model != "" && !modelAllowedForProject(store, req.ProjectID, req.Model) {
			return nil, forbidden("model_not_allowed", "model is not allowed for this project", baseFields)
		}
	}

	if req.PersistSession != "" && (tmpl == nil || !tmpl.Persist || !proj.IsHosted()) {
		return nil, forbidden("persist_session_invalid", "persist_session applies only to a persist template of a host project", baseFields)
	}

	// An SSH-hosted project's target runs on the far end, where a profile
	// written on this disk confines nothing — and sandboxing the local `ssh`
	// client instead only breaks it (SH §5.2: "SSH host terminal: off").
	sandbox := wantsSandbox(req.Kind, tmpl) && !(proj != nil && proj.IsHosted())
	baseFields.Sandbox = sandbox

	sessionRequest, refusal := buildSessionRequest(req, proj, directory, baseFields)
	if refusal != nil {
		return nil, refusal
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	baseFields.SessionID = sessionID

	spec := hostapi.LaunchRequest{
		V:              1,
		SessionID:      sessionID,
		Kind:           req.Kind,
		Resume:         req.Resume,
		Directory:      directory,
		Name:           req.Name,
		ExtraArgs:      req.ExtraArgs,
		IdleTimeoutSec: 86400,
		SessionRequest: sessionRequest,
	}

	if proj != nil {
		projJSON, err := json.Marshal(struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Path string `json:"path"`
		}{proj.ID, proj.Name, proj.Path})
		if err != nil {
			return nil, invalidRequest("internal", fmt.Sprintf("encode project: %v", err), baseFields)
		}
		spec.Project = projJSON
	}

	if proj != nil && proj.IsHosted() {
		host, hostErr := buildHostSpec(settings, proj.HostID)
		if hostErr != nil {
			return nil, invalidRequest("host_unavailable", hostErr.Error(), baseFields)
		}
		hostJSON, err := json.Marshal(host)
		if err != nil {
			return nil, invalidRequest("internal", fmt.Sprintf("encode host: %v", err), baseFields)
		}
		spec.Host = hostJSON
	}
	// Spec.Identity stays nil out of this function for every project shape.
	// SH §3.1 requires null outright for ad-hoc and SSH sessions — a host
	// project's target process never dials relay's bridge socket the way a
	// project_session shim does. For a local project, minting the secret
	// needs the live service.Launches table (Launches.Begin), which is
	// wired in main/trayapp, not passed to this package: R-S4b calls
	// Launches.Begin right before it POSTs to relay-sessions and sets
	// Spec.Identity itself.

	if req.Kind == KindPTY {
		if proj != nil && proj.IsHosted() && tmpl.Persist {
			argv, name, refusal := resolvePersistArgv(settings, proj, *tmpl, req.PersistSession, baseFields)
			if refusal != nil {
				return nil, refusal
			}
			spec.Argv = argv
			spec.Name = name
		} else if proj != nil && proj.IsHosted() {
			spec.Argv = resolveHostArgv(*tmpl, proj.Path, req.ProjectID)
		} else {
			spec.Argv = resolveArgv(*tmpl, projectPathOrEmpty(proj), req.ProjectID)
		}
		env, err := resolveTemplateEnv(*tmpl, settings)
		if err != nil {
			return nil, invalidRequest("model_endpoint_unavailable", err.Error(), baseFields)
		}
		spec.Env = env
		spec.TemplateID = tmpl.ID
		idleMinutes := tmpl.IdleTimeout
		if idleMinutes == 0 {
			idleMinutes = 1440
		}
		spec.IdleTimeoutSec = idleMinutes * 60
		cols, rows := req.Cols, req.Rows
		if cols <= 0 || rows <= 0 {
			cols, rows = 120, 40
		}
		spec.PTY = &hostapi.PTYSpec{Cols: cols, Rows: rows}
	}

	var modelKeyLabel string
	// A host terminal runs on the far end, which has no route to the console's
	// model endpoint. ValidateHostTemplate already refuses model_key on a host
	// template; this guard keeps a key from being minted if one slips through.
	hostedPTY := req.Kind == KindPTY && proj != nil && proj.IsHosted()
	if wantsModelKey(req.Kind, tmpl) && !hostedPTY {
		label := "session:" + sessionID
		key, err := mintModelKey(modelKeys, req.ProjectID, label)
		if err != nil {
			return nil, invalidRequest("model_key_mint_failed", err.Error(), baseFields)
		}
		spec.ModelKey = key
		modelKeyLabel = label
	}

	// Last, after every refusal above: writing the profile creates a file on
	// disk that only a launch or a session end sweeps away, so a refusal
	// reached after this point would leave one behind with no session to
	// own it.
	if sandbox {
		grants := tmpl
		if grants == nil {
			grants = templateForKind(settings, proj, req.Kind)
		}
		profilePath, err := writeSessionSandboxProfile(settings, proj, directory, sessionID, req.Kind, grants)
		if err != nil {
			return nil, invalidRequest("sandbox_unavailable", err.Error(), baseFields)
		}
		spec.Sandbox = &hostapi.SandboxSpec{ProfilePath: profilePath}
	}

	var ledgerRecord *ledger.Record
	if req.Kind != KindPTY {
		ledgerRecord = &ledger.Record{
			SessionID:      sessionID,
			Kind:           req.Kind,
			ProjectID:      req.ProjectID,
			Directory:      directory,
			Created:        time.Now().UTC(),
			State:          ledger.StateLive,
			SessionRequest: sessionRequest,
		}
	}

	return &LaunchResult{
		SessionID:     sessionID,
		Spec:          spec,
		Ledger:        ledgerRecord,
		ModelKeyLabel: modelKeyLabel,
		AuditFields:   baseFields,
	}, nil
}

// resolveDirectory applies SH §3.2's directory rule: confined within the
// project path, defaulting to the project path itself. proj == nil (ad-hoc)
// skips the confinement check entirely — there is no project path to
// confine against, the same "nothing to contain" reasoning DirWithin's own
// empty-dir case documents, extended one level up.
//
// A hosted (SSH) project's directory lives on the remote target, not
// relay's own disk, so it is confined lexically instead — see
// lexicalDirWithin. A local project's directory is confined by
// project.DirWithin, which resolves symlinks against relay's own
// filesystem.
func resolveDirectory(proj *config.Project, requested string, fields sessionLaunchAuditFields) (string, *LaunchRefusal) {
	dir := requested
	if proj != nil && dir == "" {
		dir = proj.Path
	}
	if dir == "" {
		return "", nil
	}

	if proj != nil && proj.IsHosted() {
		cleaned := filepath.Clean(dir)
		if !lexicalDirWithin(cleaned, filepath.Clean(proj.Path)) {
			return "", forbidden("directory_outside_project", "requested directory is outside the project", fields)
		}
		return cleaned, nil
	}

	// Resolved before the containment check, not after: DirWithin's own
	// identity fast path stats through a symlink to decide identity but
	// climbs the UNRESOLVED literal path's parent chain, so an unresolved
	// symlink leaf could stat outside projectPath and then climb straight
	// back into the project through its own literal parent. Resolving here
	// first means DirWithin only ever sees a path with no symlink left to
	// be confused by, independent of dirWithinProjectByIdentity's own
	// internal resolution.
	resolved := realpath(dir)
	if proj != nil && !project.DirWithin(resolved, proj.Path) {
		return "", forbidden("directory_outside_project", "requested directory is outside the project", fields)
	}
	return resolved, nil
}

// realpath resolves symlinks for the value AuthorizeLaunch writes into the
// LaunchSpec / ledger record for a local project or an ad-hoc (local)
// launch. The security decision itself is project.DirWithin's, which does
// its own resolution internally — this is only about what string ends up on
// the wire and on disk. Never called for a hosted project: see
// lexicalDirWithin.
func realpath(p string) string {
	if p == "" {
		return p
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// lexicalDirWithin reports whether dir is projectPath or nested under it,
// comparing cleaned path text only — never the local filesystem. This is
// deliberate: a hosted project's path exists on the SSH target, not on
// relay's own disk, so calling realpath/EvalSymlinks/Stat against it here
// would resolve symlinks or volume aliases that only exist locally,
// answering a containment question about the wrong machine. Both arguments
// must already be filepath.Clean'd by the caller.
func lexicalDirWithin(dir, projectPath string) bool {
	if dir == projectPath {
		return true
	}
	rel, err := filepath.Rel(projectPath, dir)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// kindTemplateIDs names the template a claude, pi or chat session reads its
// sandbox folders from. Those kinds are not launched from a template, but they
// run the same tools, so the template that describes a tool's folders is the
// one place they are written down.
var kindTemplateIDs = map[string]string{
	KindClaude: "claude-code",
	KindPi:     "pi",
	KindChat:   "chat",
}

func chatModelRequiredMessage(name string) string {
	if name = strings.TrimSpace(name); name != "" {
		return fmt.Sprintf("chat session %q has no model; choose a model and try again", name)
	}
	return "chat session has no model; choose a model and try again"
}

// kindAllowed is the template gate for a claude, pi or chat session. A hosted
// project's claude session runs the host's own Claude Code, so the host having
// a claude-code template is what permits it; allowed_templates names console
// templates only and says nothing about a host's. A pi session on a hosted
// project never reaches here: AuthorizeLaunch refuses it first, since pi's
// overlay writes into the project dir and links into the console's home.
// Every other kind is gated by allowed_templates.
func kindAllowed(settings *config.Settings, proj *config.Project, kind string) bool {
	id := kindTemplateIDs[kind]
	if kind == KindClaude && proj != nil && proj.IsHosted() {
		_, ok := findTemplate(settings, proj, id)
		return ok
	}
	return proj.AllowsTemplate(id)
}

// templateForKind is the template whose folders a non-terminal session gets,
// or nil when settings.json holds none by that id. A missing template is not a
// refusal: the session runs with only what every session gets, which is the
// fail-closed direction, and the warning names what to add. A hosted project
// gets nil without a warning: its session is never sandboxed, and its host's
// templates carry no folders.
func templateForKind(settings *config.Settings, proj *config.Project, kind string) *config.TerminalTemplate {
	id := kindTemplateIDs[kind]
	if id == "" || (proj != nil && proj.IsHosted()) {
		return nil
	}
	if t, ok := findTemplate(settings, proj, id); ok {
		return &t
	}
	slog.Warn("session sandbox: no template for this session kind, so it gets no folders beyond its project", "kind", kind, "template", id)
	return nil
}

func findTemplate(settings *config.Settings, proj *config.Project, id string) (config.TerminalTemplate, bool) {
	for _, t := range config.TemplatesForProject(settings, proj) {
		if t.ID == id {
			return t, true
		}
	}
	return config.TerminalTemplate{}, false
}

func projectPathOrEmpty(proj *config.Project) string {
	if proj == nil {
		return ""
	}
	return proj.Path
}

// resolveArgv expands a pty template into a full argv, relay's own job per
// C5 ("argv: pty only; relay resolves it from the template"). Only the
// empty-Command ("shell") case is resolved to an actual path here, mirroring
// relayLLM's own resolveShell (internal/terminal/terminal_template.go) —
// every other command is passed through as the template wrote it and
// resolved against PATH by whatever actually execs it (the shim/host), not
// here: relay's own PATH at authorization time has no reason to match the
// session's eventual login-shell PATH.
func resolveArgv(t config.TerminalTemplate, projectPath, projectID string) []string {
	command := t.Command
	if command == "" {
		command = defaultShell()
	}
	return expandArgv(command, t.Args, projectPath, projectID)
}

// resolveHostArgv is resolveArgv for a host template, whose command is
// interpreted on the host. An empty command stays nil rather than becoming
// defaultShell(), which names the console's shell: relay-sessions turns nil
// into the host's own login shell.
func resolveHostArgv(t config.TerminalTemplate, projectPath, projectID string) []string {
	if t.Command == "" {
		return nil
	}
	return expandArgv(t.Command, t.Args, projectPath, projectID)
}

// listPersistSessionNames is an indirection so a test can stand in for the
// host's tmux: the real one runs ssh.
var listPersistSessionNames = persistSessionNames

// resolvePersistArgv is resolveHostArgv for a persist template: the command
// runs inside the tmux session it names, and the session name becomes the
// terminal's Name, which is how PersistentSessionOps tells a session is
// attached here. requested reattaches; empty takes the host's next free n.
//
// -A is safe to pass unconditionally: new-session looks -s up as an exact
// session name, unlike -t, which tmux resolves by prefix.
//
// Deliberate: the host listing runs ssh with no settings lock held (settings
// is a snapshot), bounded by sshhost's own tmux timeout, so an unreachable
// host delays only this launch.
func resolvePersistArgv(settings *config.Settings, proj *config.Project, t config.TerminalTemplate, requested string, fields sessionLaunchAuditFields) ([]string, string, *LaunchRefusal) {
	h, _ := config.FindHostByID(settings, proj.HostID)
	if h == nil {
		return nil, "", invalidRequest("host_unavailable", fmt.Sprintf("host %q not found", proj.HostID), fields)
	}
	tmuxPath := h.EffectiveTmuxPath()
	if tmuxPath == "" {
		return nil, "", forbidden("tmux_not_available", fmt.Sprintf("host %s has no tmux: install it or set tmux_path", h.Name), fields)
	}

	name := requested
	if name != "" {
		// Rebuilding the name from the parsed n pins both the project and the
		// template: a session of another template in this project parses too.
		_, n, ok := config.ParseProjectPersistSessionName(name, proj.ID)
		if !ok || config.PersistSessionName(proj.ID, t.ID, n) != name {
			return nil, "", forbidden("persist_session_invalid", fmt.Sprintf("%q is not a persistent session of template %q in this project", name, t.ID), fields)
		}
	} else {
		names, err := listPersistSessionNames(context.Background(), *h)
		if err != nil {
			msg := fmt.Sprintf("cannot name a persistent session on host %s: %v", h.Name, err)
			return nil, "", &LaunchRefusal{Status: http.StatusBadGateway, Code: "persist_list_failed", Message: msg,
				Audit: newSessionLaunchAuditEvent(fields, audit.AuditOutcomeError, msg)}
		}
		name = config.PersistSessionName(proj.ID, t.ID, config.NextPersistSessionN(names, proj.ID, t.ID))
	}

	argv := []string{tmuxPath, "new-session", "-A", "-s", name}
	if t.Command != "" {
		argv = append(argv, expandArgv(t.Command, t.Args, proj.Path, proj.ID)...)
	}
	return argv, name, nil
}

func expandArgv(command string, args []string, projectPath, projectID string) []string {
	argv := make([]string, 0, 1+len(args))
	argv = append(argv, config.ExpandTemplateVars(command, projectPath, projectID))
	for _, a := range args {
		argv = append(argv, config.ExpandTemplateVars(a, projectPath, projectID))
	}
	return argv
}

func defaultShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/zsh"
}

// terminalEnvDefaults are set when the template supplies neither. Relay and
// relay-sessions run with no TERM, and a pty child inherits that: zsh then
// treats the terminal as dumb and cannot redraw on backspace. Set here rather
// than in buildShimEnv so an SSH host terminal, which only sees spec.Env,
// gets them too.
var terminalEnvDefaults = map[string]string{
	"TERM":      "xterm-256color",
	"COLORTERM": "truecolor",
}

// resolveTemplateEnv is a template's env as the session sees it: the template's
// own values, then the passthrough variables, then TERM and COLORTERM where
// neither set them. ${MODEL_ENDPOINT_URL} in a value becomes the model
// endpoint's URL; with the listener off it refuses, because dropping only the
// URL would leave a ${MODEL_KEY} header pointed at the client's real provider,
// which would then receive the relay key.
func resolveTemplateEnv(t config.TerminalTemplate, settings *config.Settings) (map[string]string, error) {
	env := make(map[string]string, len(t.Env)+len(t.EnvPassthrough)+len(terminalEnvDefaults))
	for k, v := range t.Env {
		if strings.Contains(v, config.ModelEndpointURLMarker) {
			if settings == nil || settings.ModelEndpoint == nil || settings.ModelEndpoint.Listen == "" {
				return nil, fmt.Errorf("template %q uses %s in env %q but model_endpoint.listen is not set in settings.json", t.ID, config.ModelEndpointURLMarker, k)
			}
			v = strings.ReplaceAll(v, config.ModelEndpointURLMarker, "http://"+settings.ModelEndpoint.Listen)
		}
		env[k] = v
	}
	for _, name := range t.EnvPassthrough {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	for k, v := range terminalEnvDefaults {
		if _, set := env[k]; !set {
			env[k] = v
		}
	}
	return env, nil
}

// wantsSandbox is C7's default table: on for claude/pi/chat unconditionally,
// and for a pty launch on unless the template explicitly opts out with
// "sandbox": false (this function does not special-case any template id).
func wantsSandbox(kind string, tmpl *config.TerminalTemplate) bool {
	switch kind {
	case KindClaude, KindPi, KindChat:
		return true
	default:
		return tmpl == nil || tmpl.Sandboxed()
	}
}

// wantsModelKey is C5's model_key rule: pi and chat sessions always want
// one, claude sessions never do (Claude Code brings its own API key via
// EnvPassthrough), and a pty template opts in via its own ModelKey field.
func wantsModelKey(kind string, tmpl *config.TerminalTemplate) bool {
	switch kind {
	case KindPi, KindChat:
		return true
	case KindPTY:
		return tmpl != nil && tmpl.ModelKey
	default:
		return false
	}
}

// eveSessionRequestBody is eve's POST /api/sessions body shape
// (E/ws/session-messages.js's handleCreateSession) — the exact fields C5
// says relay forwards to relay-sessions as SessionRequest "after relay's
// policy merge (permission policy, directory)".
type eveSessionRequestBody struct {
	ProjectID      string          `json:"projectId"`
	Directory      string          `json:"directory"`
	Name           string          `json:"name"`
	Model          string          `json:"model"`
	Settings       json.RawMessage `json:"settings,omitempty"`
	SystemPrompt   string          `json:"systemPrompt,omitempty"`
	AppendClaudeMd bool            `json:"appendClaudeMd,omitempty"`
}

// buildSessionRequest builds the claude/pi/chat SessionRequest passthrough
// (nil for pty, which has none) with the client's settings run through
// mergePermissionSettings first, and the directory field forced to the
// already-authorized realpath — a caller-supplied directory never reaches
// relay-sessions unvalidated.
func buildSessionRequest(req LaunchRequest, proj *config.Project, directory string, fields sessionLaunchAuditFields) (json.RawMessage, *LaunchRefusal) {
	if req.Kind == KindPTY {
		return nil, nil
	}
	var projectPolicy *config.PermissionPolicy
	if proj != nil {
		projectPolicy = proj.PermissionPolicy
	}
	merged, err := mergePermissionSettings(req.ClientSettings, projectPolicy)
	if err != nil {
		return nil, invalidRequest("invalid_settings", err.Error(), fields)
	}
	body := eveSessionRequestBody{
		ProjectID:      req.ProjectID,
		Directory:      directory,
		Name:           req.Name,
		Model:          req.Model,
		Settings:       merged,
		SystemPrompt:   req.SystemPrompt,
		AppendClaudeMd: req.AppendClaudeMd,
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, invalidRequest("internal", fmt.Sprintf("encode session request: %v", err), fields)
	}
	return out, nil
}

// mergePermissionSettings ports eve's handleCreateSession merge
// (eve/ws/session-messages.js:26-39) into relay: the project's own
// PermissionPolicy, when it has one, is the only source for allowedTools
// and deniedTools — never the client's — while the client's own
// permissionMode survives as long as it set one explicitly, falling back to
// the project's defaultMode only when the client left it unset and the
// project's defaultMode is itself not the literal string "default".
//
// This is deliberate: when proj has NO PermissionPolicy configured at all,
// eve's own logic never touches settings.permissionPolicy, so a client's
// own permissionPolicy value passes through completely unexamined here too
// — an exact port of eve's existing asymmetry, not a tightening of it. The
// project owner who left no policy configured is the one accepting that.
func mergePermissionSettings(clientSettings json.RawMessage, projectPolicy *config.PermissionPolicy) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(clientSettings) > 0 {
		if err := json.Unmarshal(clientSettings, &fields); err != nil {
			return nil, fmt.Errorf("session settings: %w", err)
		}
	}
	if projectPolicy == nil {
		if len(fields) == 0 {
			return nil, nil
		}
		return json.Marshal(fields)
	}

	policy := sessiontypes.PermissionPolicy{
		DefaultMode:  projectPolicy.DefaultMode,
		AllowedTools: projectPolicy.AllowedTools,
		DeniedTools:  projectPolicy.DeniedTools,
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("merge permission policy: %w", err)
	}
	fields["permissionPolicy"] = policyJSON

	var clientMode string
	if raw, ok := fields["permissionMode"]; ok {
		_ = json.Unmarshal(raw, &clientMode)
	}
	if clientMode == "" && projectPolicy.DefaultMode != "" && projectPolicy.DefaultMode != "default" {
		modeJSON, err := json.Marshal(projectPolicy.DefaultMode)
		if err != nil {
			return nil, fmt.Errorf("merge permission mode: %w", err)
		}
		fields["permissionMode"] = modeJSON
	}

	return json.Marshal(fields)
}

// buildHostSpec resolves an SSH-hosted project's config.Host into the
// sessiontypes.HostSpec relay-sessions needs to reach it, reusing
// sshhost.SSHArgv (the one place relay derives ssh arguments) rather than
// reimplementing any part of it.
func buildHostSpec(settings *config.Settings, hostID string) (*sessiontypes.HostSpec, error) {
	var h *config.Host
	for i := range settings.Hosts {
		if settings.Hosts[i].ID == hostID {
			h = &settings.Hosts[i]
			break
		}
	}
	if h == nil {
		return nil, fmt.Errorf("host %q not found", hostID)
	}
	controlDir, err := sshhost.ControlDir()
	if err != nil {
		return nil, fmt.Errorf("ssh control dir: %w", err)
	}
	spec := &sessiontypes.HostSpec{
		ID:      h.ID,
		Name:    h.Name,
		SSHArgv: sshhost.SSHArgv(*h, controlDir),
	}
	if h.Probe != nil && h.Probe.OK {
		spec.NodePath = h.Probe.NodePath
		spec.ClaudePath = h.Probe.ClaudePath
		spec.Shell = h.Probe.Shell
		spec.OS = h.Probe.OS
	}
	return spec, nil
}
