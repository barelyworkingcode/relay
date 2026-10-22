package hostapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/sessions/adopt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/sessions/childenv"
	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

// kindPTY mirrors cmd/relay/session_launch.go's KindPTY: C5's own wire
// sample ("kind": "pty | claude | pi | chat") is ground truth over
// spec-session-host.md's "chat_tools" prose aside. Duplicated, not imported,
// for the same reason every other wire-shape mirror in this package is
// (types.go's doc comments on permissionRequestBody/sessionRequestBody).
const kindPTY = "pty"

func isProviderKind(kind string) bool {
	switch kind {
	case session.KindClaude, session.KindPi, session.KindCodex, session.KindChat:
		return true
	default:
		return false
	}
}

// decodeHostSpec decodes LaunchRequest.Host (empty for a console session)
// into the *sessionstypes.HostSpec both terminal.CreateSpec and
// session.CreateSpec want.
//
// This is subtle: unmarshaling the JSON literal null into a non-pointer
// destination (h below) is a documented encoding/json no-op, not an error —
// it leaves h at its zero value rather than producing a nil *HostSpec. A
// caller that sends the literal "null" rather than omitting the key
// entirely is checked for explicitly so this never returns a non-nil,
// zero-value HostSpec that terminal.CreateSpec.validate() would then read as
// a real (if empty) SSH host.
func decodeHostSpec(raw json.RawMessage) (*sessionstypes.HostSpec, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var h sessionstypes.HostSpec
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// checkLaunchOrigin refuses any origin the host does not recognise: relay is
// the only caller, and the Chief of Staff is the only origin it stamps on a
// launch, so a stray value means a bug or a forged request, never a feature.
func checkLaunchOrigin(origin string) error {
	if origin != "" && origin != sessionstypes.OriginChiefOfStaff {
		return fmt.Errorf("hostapi: unknown launch origin %q", origin)
	}
	return nil
}

// claudeCodeTemplateID mirrors cmd/relay's kindTemplateIDs[KindClaude]: the
// template an unsandboxed drop-in terminal runs.
const claudeCodeTemplateID = "claude-code"

// hasProject reports whether the launch names a project, in project or in the
// session request. The literal null counts as none.
func hasProject(req LaunchRequest, sr sessionRequestBody) bool {
	return (len(req.Project) > 0 && string(req.Project) != "null") || sr.ProjectID != ""
}

// checkUnsandboxedTerminal allows the unsandboxed mark on a local drop-in
// terminal of the claude-code template only, with no project, no sandbox and
// no identity. Fail closed: any other shape that carries it is refused.
func checkUnsandboxedTerminal(req LaunchRequest, host *sessionstypes.HostSpec) error {
	if !req.Unsandboxed {
		return nil
	}
	if req.Kind != kindPTY || req.DropInFor == "" || req.TemplateID != claudeCodeTemplateID || host != nil ||
		req.Sandbox != nil || req.Identity != nil || hasProject(req, sessionRequestBody{}) ||
		req.Outside || req.ReadOnly || req.ViewTarget != nil || req.ModelKey != "" {
		return errors.New("hostapi: unsandboxed applies only to a local drop-in terminal with no project, sandbox or identity")
	}
	return nil
}

// checkUnsandboxedSession allows the unsandboxed mark on a local claude launch
// only, with no project, sandbox, identity, launcher, entry or model key, and
// refuses a local claude launch with no project and no mark. Deliberate: relay
// authorizes every claude launch against a project, so one that arrives
// without a project is a request relay never makes unless the mark says so.
func checkUnsandboxedSession(req LaunchRequest, sr sessionRequestBody, host *sessionstypes.HostSpec) error {
	noProject := !hasProject(req, sr)
	if !req.Unsandboxed {
		if req.Kind == session.KindClaude && host == nil && noProject {
			return errors.New("hostapi: a local claude launch needs a project or the unsandboxed mark")
		}
		return nil
	}
	if req.Kind != session.KindClaude || host != nil || !noProject || req.Sandbox != nil || req.Identity != nil ||
		req.Launcher != nil || sr.Entry != "" || req.ModelKey != "" || req.AttachCodexThread != "" {
		return errors.New("hostapi: unsandboxed applies only to a local claude launch with no project, sandbox or identity")
	}
	return nil
}

// createTerminal starts the terminal the launch describes. The mark reaches the
// manager here, not in the spec, so a terminal listed as unsandboxed is one
// this function checked.
func createTerminal(m *terminal.Manager, req LaunchRequest, spec terminal.CreateSpec) (*terminal.Session, error) {
	if req.Unsandboxed {
		return m.CreateUnsandboxed(spec)
	}
	return m.Create(spec)
}

// buildTerminalSpec translates a "pty" LaunchRequest into
// terminal.CreateSpec, field by field: the two types were designed
// independently (C5's wire contract vs. this package's own Go shape) and
// happen to share almost every name, which is exactly the situation where
// assuming instead of checking hides a mismatch.
func buildTerminalSpec(req LaunchRequest) (terminal.CreateSpec, error) {
	if err := checkLaunchOrigin(req.Origin); err != nil {
		return terminal.CreateSpec{}, err
	}
	if req.Launcher != nil {
		return terminal.CreateSpec{}, errors.New("hostapi: a launcher applies only to a claude, codex or pi session")
	}
	if req.ResumeConversation != "" || req.ResumeTranscript != "" {
		return terminal.CreateSpec{}, errors.New("hostapi: resume_conversation applies only to a claude session")
	}
	if req.AttachCodexThread != "" {
		return terminal.CreateSpec{}, errors.New("hostapi: attach_codex_thread applies only to a codex session")
	}
	if req.AdoptHost != nil {
		return terminal.CreateSpec{}, errors.New("hostapi: adopt_host applies only to a host claude session")
	}
	host, err := decodeHostSpec(req.Host)
	if err != nil {
		return terminal.CreateSpec{}, err
	}
	if err := checkUnsandboxedTerminal(req, host); err != nil {
		return terminal.CreateSpec{}, err
	}
	spec := terminal.CreateSpec{
		ReadOnly:    req.ReadOnly,
		Outside:     req.Outside,
		SessionID:   req.SessionID,
		TemplateID:  req.TemplateID,
		Name:        req.Name,
		Directory:   req.Directory,
		Argv:        req.Argv,
		Env:         req.Env,
		IdleTimeout: time.Duration(req.IdleTimeoutSec) * time.Second,
		Host:        host,
		Origin:      req.Origin,
	}
	if req.ViewTarget != nil {
		spec.View = &terminal.ViewTarget{TmuxPath: req.ViewTarget.TmuxPath, SessionID: req.ViewTarget.SessionID}
	}
	if req.PTY != nil {
		spec.Cols = uint16(req.PTY.Cols)
		spec.Rows = uint16(req.PTY.Rows)
	}
	// Carried through unconditionally, empty ProfilePath included: laundering
	// an empty-but-present profile path into a nil Sandbox here would disable
	// CreateSpec.validate()'s own fail-closed refusal for exactly that case
	// before it ever runs.
	if req.Sandbox != nil {
		spec.Sandbox = &terminal.SandboxSpec{ProfilePath: req.Sandbox.ProfilePath}
	}
	if req.Identity != nil {
		spec.Identity = &terminal.IdentitySpec{Secret: req.Identity.Secret}
	}
	spec.ModelKey = req.ModelKey
	spec.ClaudeToken = req.ClaudeToken
	if len(req.ClaudeEnv) > 0 {
		return terminal.CreateSpec{}, errors.New("hostapi: claude_env applies only to a local claude launch")
	}
	return spec, nil
}

// buildSessionSpec translates a "claude"/"pi"/"chat" LaunchRequest into
// session.CreateSpec. SessionRequest carries the eve-originated fields (C5:
// "the host consumes it exactly as relayLLM's create handler does today");
// Directory/Name fall back to the top-level LaunchRequest fields when
// SessionRequest is absent (a resume, or a caller that never set it), the
// same fallback shape terminal.CreateSpec's own top-level fields get.
func buildSessionSpec(req LaunchRequest) (session.CreateSpec, error) {
	if err := checkLaunchOrigin(req.Origin); err != nil {
		return session.CreateSpec{}, err
	}
	var sr sessionRequestBody
	if len(req.SessionRequest) > 0 {
		if err := json.Unmarshal(req.SessionRequest, &sr); err != nil {
			return session.CreateSpec{}, err
		}
	}
	// Deliberate: an entry start of claude or codex may carry no model, which
	// runs the binary's own default. A raw start still needs one.
	// A host adopt may carry none: the outside session's model family is unknown
	// and the binary then runs its own default.
	noModelOK := (sr.Entry != "" && (req.Kind == session.KindClaude || req.Kind == session.KindCodex)) || req.AdoptHost != nil
	if !req.Resume && strings.TrimSpace(sr.Model) == "" && !noModelOK {
		return session.CreateSpec{}, fmt.Errorf("hostapi: %s session %s has no model", req.Kind, req.SessionID)
	}
	if req.Launcher != nil {
		if req.Kind == session.KindChat {
			return session.CreateSpec{}, errors.New("hostapi: a launcher applies only to a claude, codex or pi session")
		}
		if !strings.HasPrefix(req.Launcher.Command, "/") {
			return session.CreateSpec{}, errors.New("hostapi: launcher command must be an absolute path")
		}
	}
	directory := sr.Directory
	if directory == "" {
		directory = req.Directory
	}
	name := sr.Name
	if name == "" {
		name = req.Name
	}

	host, err := decodeHostSpec(req.Host)
	if err != nil {
		return session.CreateSpec{}, err
	}
	if req.ClaudeToken != "" && (req.Kind != session.KindClaude || (req.Sandbox == nil && !req.Unsandboxed) || host != nil) {
		return session.CreateSpec{}, errors.New("hostapi: claude token requires a sandboxed or unsandboxed local claude launch")
	}
	if err := checkUnsandboxedSession(req, sr, host); err != nil {
		return session.CreateSpec{}, err
	}
	if err := checkResumeConversation(req, host); err != nil {
		return session.CreateSpec{}, err
	}
	if err := checkAdoptHost(req, host); err != nil {
		return session.CreateSpec{}, err
	}
	if err := checkAttachCodexThread(req, host, sr); err != nil {
		return session.CreateSpec{}, err
	}

	if err := checkClaudeEnv(req, host); err != nil {
		return session.CreateSpec{}, err
	}

	var projectID string
	if len(req.Project) > 0 {
		var p projectRef
		if err := json.Unmarshal(req.Project, &p); err != nil {
			return session.CreateSpec{}, err
		}
		projectID = p.ID
	}
	if projectID == "" {
		projectID = sr.ProjectID
	}

	spec := session.CreateSpec{
		SessionID:      req.SessionID,
		ProjectID:      projectID,
		Kind:           req.Kind,
		Directory:      directory,
		Name:           name,
		Model:          sr.Model,
		Settings:       sr.Settings,
		SystemPrompt:   sr.SystemPrompt,
		AppendClaudeMd: sr.AppendClaudeMd,
		Host:           host,
		ModelKey:       req.ModelKey,
		Resume:         req.Resume,
		Origin:         req.Origin,
		Unsandboxed:    req.Unsandboxed,
		Launcher:       req.Launcher,
		Entry:          sr.Entry,
		ClaudeToken:    req.ClaudeToken,
		ClaudeEnv:      req.ClaudeEnv,

		ResumeConversation: req.ResumeConversation,
		ResumeTranscript:   req.ResumeTranscript,

		AttachCodexThread: req.AttachCodexThread,
	}
	if a := req.AdoptHost; a != nil {
		spec.AdoptHost = &sshhost.AdoptPins{
			PID: a.PID, Lifetime: a.Lifetime, ConversationID: req.ResumeConversation, ChildTest: a.ChildTest,
			Interrupt: a.Interrupt, NoSignal: a.NoSignal,
			TranscriptSize: a.TranscriptSize, WindowFromMs: a.WindowFromMs, WindowToMs: a.WindowToMs,
		}
	}
	// session.CreateSpec has no validate() of its own to lean on the way
	// terminal.CreateSpec does (buildTerminalSpec's own comment): an empty
	// ProfilePath on a non-nil Sandbox is refused here instead, rather than
	// silently becoming SandboxProfile == "" — indistinguishable from "no
	// sandbox requested at all" the moment it reaches ChatConfig.
	if req.Sandbox != nil {
		if req.Sandbox.ProfilePath == "" {
			return session.CreateSpec{}, errors.New("hostapi: sandbox requested but profile path is empty")
		}
		spec.SandboxProfile = req.Sandbox.ProfilePath
	}
	if req.Identity != nil {
		spec.Identity = &sessionsmcp.IdentitySpec{Secret: req.Identity.Secret}
	}
	return spec, nil
}

// terminalLaunchStatus maps a terminal.Manager.Create error to C5's HTTP
// status and error code. Every named sentinel gets its own C5 code; anything
// else falls through to invalid_spec, since startSession's own validation
// (CreateSpec.validate, a host+identity conflict, ...) is the dominant
// unnamed-error source on this path.
func terminalLaunchStatus(err error) (int, string) {
	switch {
	case errors.Is(err, terminal.ErrSessionExists):
		return 409, ErrSessionExists
	case errors.Is(err, terminal.ErrIdentityRefused):
		return 502, ErrIdentityRefused
	case errors.Is(err, terminal.ErrSpawnFailed):
		return 500, ErrSpawnFailed
	default:
		return 400, ErrInvalidSpec
	}
}

// sessionLaunchStatus is terminalLaunchStatus's session.Manager mirror. The
// dominant unnamed-error source on this path is a provider's own Start()
// failing to spawn its process (claude/pi binary missing, chat's shim
// refused, ...), a host-side problem, not a caller mistake — the opposite
// weighting from the terminal path, where CreateSpec.validate() dominates.
func sessionLaunchStatus(err error) (int, string) {
	if errors.Is(err, session.ErrSessionExists) {
		return 409, ErrSessionExists
	}
	if errors.Is(err, session.ErrDroppedIn) {
		return 409, "dropped_in"
	}
	if errors.Is(err, session.ErrAttachRequired) {
		return 400, ErrInvalidSpec
	}
	if errors.Is(err, provider.ErrIdentityRefused) {
		return 502, ErrIdentityRefused
	}
	switch {
	case errors.Is(err, provider.ErrAdoptBusy):
		return 409, ErrAdoptBusy
	case errors.Is(err, provider.ErrAdoptChanged):
		return 409, ErrAdoptChanged
	case errors.Is(err, provider.ErrAdoptStopFailed):
		return 502, ErrAdoptStopFailed
	case errors.Is(err, provider.ErrAdoptLaunchFailed):
		return 502, ErrAdoptLaunchFailed
	case errors.Is(err, provider.ErrAdoptLinkLost):
		return 502, ErrAdoptLinkLost
	}
	return 500, ErrSpawnFailed
}

// checkResumeConversation allows resume_conversation on a fresh, local claude
// launch, and on a host claude launch that carries adopt_host, as a canonical
// UUID: the value becomes claude's --resume argument and a transcript file name.
func checkResumeConversation(req LaunchRequest, host *sessionstypes.HostSpec) error {
	if req.ResumeConversation == "" && req.ResumeTranscript == "" {
		return nil
	}
	if host != nil {
		if req.AdoptHost == nil || req.Kind != session.KindClaude || req.ResumeTranscript != "" {
			return errors.New("hostapi: conversation resume on a host requires adopt_host on a claude launch")
		}
	} else if (req.Kind != session.KindClaude && req.Kind != session.KindPi) || req.Resume {
		return errors.New("hostapi: conversation resume requires a fresh local agent launch")
	}
	if _, err := uuid.Parse(req.ResumeConversation); err != nil || len(req.ResumeConversation) != 36 {
		return errors.New("hostapi: resume_conversation is not a UUID")
	}
	if req.Kind == session.KindClaude {
		if req.ResumeTranscript != "" {
			return errors.New("hostapi: resume_transcript requires Pi")
		}
		return nil
	}
	var sr sessionRequestBody
	if err := json.Unmarshal(req.SessionRequest, &sr); err != nil {
		return err
	}
	var settings struct {
		Headless bool `json:"headless"`
	}
	if json.Unmarshal(sr.Settings, &settings) != nil || !settings.Headless {
		return errors.New("hostapi: Pi adoption requires headless")
	}
	cwd := sr.Directory
	if cwd == "" {
		cwd = req.Directory
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return adopt.ValidatePiTranscript(home, cwd, req.ResumeConversation, req.ResumeTranscript)
}

// checkAdoptHost allows adopt_host on a claude launch to a Unix host with
// detach on, with the conversation to resume, and nothing else. Every field the
// script puts in a comparison or a count is shaped here as well.
func checkAdoptHost(req LaunchRequest, host *sessionstypes.HostSpec) error {
	a := req.AdoptHost
	if a == nil {
		return nil
	}
	if req.Kind != session.KindClaude || host == nil || !host.Detach || sshhost.IsWindowsOS(host.OS) {
		return errors.New("hostapi: adopt_host requires a claude launch to a Unix host with detached sessions")
	}
	if req.ResumeConversation == "" {
		return errors.New("hostapi: adopt_host requires resume_conversation")
	}
	switch a.ChildTest {
	case "none", "all":
	case "window":
		if a.WindowFromMs <= 0 || a.WindowToMs < a.WindowFromMs {
			return errors.New("hostapi: adopt_host child_test window is not a window")
		}
	default:
		return errors.New("hostapi: adopt_host child_test is not none, window or all")
	}
	if a.PID <= 0 || a.Lifetime == "" || len(a.Lifetime) > 64 || a.TranscriptSize < 0 {
		return errors.New("hostapi: adopt_host names no process")
	}
	return nil
}

// checkAttachCodexThread allows attach_codex_thread on a local, headless codex
// launch only (fresh, or a resume the session manager matches to the stored
// attach), as a canonical UUID and with no other resume field: the value becomes
// a thread id relay sends to the codex daemon.
func checkAttachCodexThread(req LaunchRequest, host *sessionstypes.HostSpec, sr sessionRequestBody) error {
	if req.AttachCodexThread == "" {
		return nil
	}
	if req.Kind != session.KindCodex || host != nil {
		return errors.New("hostapi: attach_codex_thread requires a local codex launch")
	}
	if req.ResumeConversation != "" || req.ResumeTranscript != "" {
		return errors.New("hostapi: attach_codex_thread excludes resume_conversation and resume_transcript")
	}
	if id, err := uuid.Parse(req.AttachCodexThread); err != nil || len(req.AttachCodexThread) != 36 || id.String() != req.AttachCodexThread {
		return errors.New("hostapi: attach_codex_thread is not a UUID")
	}
	if req.Resume {
		// The stored session is headless or it is not an attach: Manager.Create
		// reads it.
		return nil
	}
	var settings struct {
		Headless bool `json:"headless"`
	}
	if json.Unmarshal(sr.Settings, &settings) != nil || !settings.Headless {
		return errors.New("hostapi: a codex attach requires headless")
	}
	return nil
}

// checkClaudeEnv refuses claude_env on anything but a local claude launch and
// any name that fails the name rule. Names only in the message, never values.
func checkClaudeEnv(req LaunchRequest, host *sessionstypes.HostSpec) error {
	if len(req.ClaudeEnv) == 0 {
		return nil
	}
	if req.Kind != session.KindClaude || host != nil {
		return errors.New("hostapi: claude_env applies only to a local claude launch")
	}
	for name := range req.ClaudeEnv {
		if err := childenv.CheckClaudeEnvName(name); err != nil {
			return fmt.Errorf("hostapi: claude_env: %w", err)
		}
	}
	return nil
}
