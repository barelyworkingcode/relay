package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/sessions/hostapi"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

const refusalDirectoryUnusable = "directory_unusable"

// unsandboxedLaunch is what adopt, resume and drop-in hand the one launch
// builder for a Claude session that is in no project. No request decoder fills
// it: the person never asks for a launch with no project, they adopt a session
// that already runs outside every project.
type unsandboxedLaunch struct {
	Caller LaunchCaller
	// SessionID is empty for an adopt and for a drop-in terminal, and the
	// record's id for a resume.
	SessionID string
	Resume    bool
	// Kind is KindClaude, or KindPTY for a drop-in terminal.
	Kind      string
	Directory string
	Name      string
	// Model and ClientSettings apply to KindClaude only: the settings are
	// {"headless":true,"agent":true} on an adopt and the stored ones on a resume.
	Model          string
	ClientSettings json.RawMessage
	// Cols and Rows apply to KindPTY only.
	Cols, Rows int
}

// usableDirectory resolves dir and reports whether it is an absolute path to
// an existing directory. It returns the path with its symlinks resolved.
func usableDirectory(dir string) (string, bool) {
	if !filepath.IsAbs(dir) {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return resolved, true
}

// authorizeUnsandboxedLaunch builds the launch of a Claude session, or of its
// drop-in terminal, that runs in no project: no project lookup, no allowed-
// models check, no template allow-list, no model key, no sandbox profile and
// no launch identity. The session runs as the account that runs relay, in the
// folder the person started it in. AuthorizeLaunch is unchanged and still
// refuses a launch with no project; only adopt, resume and drop-in reach this.
func authorizeUnsandboxedLaunch(store config.SettingsStore, sessions *ledger.Ledger, req unsandboxedLaunch) (*LaunchResult, *LaunchRefusal) {
	settings := config.FreshSettings(store)
	fields := sessionLaunchAuditFields{
		Actor: callerAuditActor(req.Caller), Kind: req.Kind, NoProject: true,
	}
	if !callerGrantsExecute(req.Caller) {
		return nil, forbidden("caller_not_authorized", "caller does not hold execute on the frontend socket", fields)
	}
	if req.Kind != KindClaude && req.Kind != KindPTY {
		return nil, invalidRequest("invalid_kind", "only a claude session or its drop-in terminal runs with no project", fields)
	}
	if req.SessionID != "" && !req.Resume {
		return nil, forbidden("session_id_not_allowed", "a fresh launch may not name a session id", fields)
	}
	if req.Resume && req.Kind != KindClaude {
		return nil, invalidRequest("invalid_kind", "only a claude session resumes", fields)
	}

	var origin string
	if req.Resume {
		rec, ok := sessions.Get(req.SessionID)
		if !ok || rec.State != ledger.StateDormant || !rec.Unsandboxed || rec.ProjectID != "" || rec.Kind != KindClaude {
			return nil, forbidden("session_not_resumable", "session is not a dormant session that runs with no project", fields)
		}
		origin = rec.Origin
		fields.Origin = origin
	}

	directory, ok := usableDirectory(req.Directory)
	if !ok {
		return nil, deniedRefusal(http.StatusConflict, refusalDirectoryUnusable, "the session's folder is not a usable directory", nil, fields)
	}
	fields.Directory = directory

	var tmpl *config.TerminalTemplate
	if req.Kind == KindPTY {
		t, found := config.GetTerminalTemplate(settings, kindTemplateIDs[KindClaude])
		if !found {
			return nil, forbidden("template_not_allowed", "the claude-code terminal template is not available", fields)
		}
		tmpl = &t
		fields.TemplateID = t.ID
	}

	// Read at every launch and resume, before anything is minted or written, so
	// a refusal leaves nothing behind. The same token a sandboxed Claude gets,
	// over the same fd 5; the process runs as the same account and could read
	// the file by path, so it gains nothing.
	claudeToken, err := loadClaudeToken(settings)
	if err != nil {
		return nil, invalidRequest("claude_token_unavailable", err.Error(), fields)
	}

	var claudeEnv map[string]string
	var sessionRequest json.RawMessage
	if req.Kind == KindClaude {
		readOnly, err := readOnlyProjectsSetting(req.ClientSettings)
		if err != nil {
			return nil, invalidRequest("invalid_settings", err.Error(), fields)
		}
		if readOnly {
			return nil, invalidRequest("read_only_needs_sandbox", "readOnlyProjects needs a sandbox; this session has none", fields)
		}
		if claudeEnv, err = resolveClaudeEnvPassthrough(settings); err != nil {
			return nil, invalidRequest("invalid_claude_env", err.Error(), fields)
		}
		// A nil project: mergePermissionSettings applies no project policy.
		var refusal *LaunchRefusal
		sessionRequest, refusal = buildSessionRequest(LaunchRequest{
			Kind: KindClaude, Name: req.Name, Model: req.Model, ClientSettings: req.ClientSettings,
		}, nil, directory, fields)
		if refusal != nil {
			return nil, refusal
		}
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	fields.SessionID = sessionID

	spec := hostapi.LaunchRequest{
		V:              1,
		SessionID:      sessionID,
		Kind:           req.Kind,
		Resume:         req.Resume,
		Directory:      directory,
		Name:           req.Name,
		IdleTimeoutSec: 86400,
		SessionRequest: sessionRequest,
		ClaudeToken:    claudeToken,
		ClaudeEnv:      claudeEnv,
		Origin:         origin,
		Unsandboxed:    true,
	}

	if req.Kind == KindPTY {
		env, err := resolveTemplateEnv(*tmpl)
		if err != nil {
			return nil, invalidRequest("model_endpoint_unavailable", err.Error(), fields)
		}
		spec.Argv = resolveArgv(*tmpl, "", "")
		spec.Env = env
		spec.TemplateID = tmpl.ID
		if tmpl.IdleTimeout > 0 {
			spec.IdleTimeoutSec = tmpl.IdleTimeout * 60
		}
		cols, rows := req.Cols, req.Rows
		if cols <= 0 || rows <= 0 {
			cols, rows = 120, 40
		}
		spec.PTY = &hostapi.PTYSpec{Cols: cols, Rows: rows}
	}

	var record *ledger.Record
	if req.Kind == KindClaude {
		record = &ledger.Record{
			SessionID:      sessionID,
			Kind:           KindClaude,
			Directory:      directory,
			Created:        time.Now().UTC(),
			Origin:         origin,
			State:          ledger.StateLive,
			SessionRequest: sessionRequest,
			Unsandboxed:    true,
		}
	}
	return &LaunchResult{SessionID: sessionID, Spec: spec, Ledger: record, AuditFields: fields}, nil
}
