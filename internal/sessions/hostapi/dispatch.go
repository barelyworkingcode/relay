package hostapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sessionsmcp "github.com/barelyworkingcode/relay/internal/sessions/mcp"
	"github.com/barelyworkingcode/relay/internal/sessions/provider"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// kindPTY mirrors cmd/relay/session_launch.go's KindPTY: C5's own wire
// sample ("kind": "pty | claude | pi | chat") is ground truth over
// spec-session-host.md's "chat_tools" prose aside, and cmd/relay's own
// comment on that constant names launch_test.go's "pty" literal as
// confirming it. Duplicated, not imported, for the same reason every other
// wire-shape mirror in this package is (types.go's doc comments on
// permissionRequestBody/sessionRequestBody).
const kindPTY = "pty"

func isProviderKind(kind string) bool {
	switch kind {
	case session.KindClaude, session.KindPi, session.KindChat:
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

// buildTerminalSpec translates a "pty" LaunchRequest into
// terminal.CreateSpec, field by field: the two types were designed
// independently (C5's wire contract vs. this package's own Go shape) and
// happen to share almost every name, which is exactly the situation where
// assuming instead of checking hides a mismatch.
func buildTerminalSpec(req LaunchRequest) (terminal.CreateSpec, error) {
	host, err := decodeHostSpec(req.Host)
	if err != nil {
		return terminal.CreateSpec{}, err
	}
	spec := terminal.CreateSpec{
		SessionID:   req.SessionID,
		TemplateID:  req.TemplateID,
		Name:        req.Name,
		Directory:   req.Directory,
		Argv:        req.Argv,
		Env:         req.Env,
		IdleTimeout: time.Duration(req.IdleTimeoutSec) * time.Second,
		Host:        host,
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
	return spec, nil
}

// buildSessionSpec translates a "claude"/"pi"/"chat" LaunchRequest into
// session.CreateSpec. SessionRequest carries the eve-originated fields (C5:
// "the host consumes it exactly as relayLLM's create handler does today");
// Directory/Name fall back to the top-level LaunchRequest fields when
// SessionRequest is absent (a resume, or a caller that never set it), the
// same fallback shape terminal.CreateSpec's own top-level fields get.
func buildSessionSpec(req LaunchRequest) (session.CreateSpec, error) {
	var sr sessionRequestBody
	if len(req.SessionRequest) > 0 {
		if err := json.Unmarshal(req.SessionRequest, &sr); err != nil {
			return session.CreateSpec{}, err
		}
	}
	if req.Kind == session.KindChat && !req.Resume && strings.TrimSpace(sr.Model) == "" {
		return session.CreateSpec{}, fmt.Errorf("hostapi: chat session %s has no model", req.SessionID)
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
	if errors.Is(err, provider.ErrIdentityRefused) {
		return 502, ErrIdentityRefused
	}
	return 500, ErrSpawnFailed
}
