package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

const (
	maxChiefOfStaffPromptRunes = 8000
	maxChiefOfStaffNameRunes   = 60
	defaultChiefOfStaffName    = "Chief of Staff agent"

	chiefOfStaffModeHeadless = "headless"
	chiefOfStaffModeTerminal = "terminal"
)

// chiefOfStaffStartBody is the whole of what the route reads. A field that is
// not here, origin and settings included, is never decoded.
type chiefOfStaffStartBody struct {
	ProjectID string `json:"projectId"`
	Folder    string `json:"folder"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model"`
	Mode      string `json:"mode"`
}

type chiefOfStaffStartResult struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	ProjectID string `json:"projectId"`
	Directory string `json:"directory"`
	Mode      string `json:"mode"`
	Kind      string `json:"kind"`
	Origin    string `json:"origin"`
	At        string `json:"at"`
}

// handleChiefOfStaffStart is the only door that starts a session on the Chief
// of Staff's behalf. It builds the launch request itself: the origin is the
// constant below, the headless settings are relay's own, and nothing the
// caller sends can name settings, a template or extra arguments. The launch
// still passes AuthorizeLaunch, so the project's policy and the sandbox apply
// exactly as for any other start.
//
// Every refusal after the audit-ready check writes a session_launch row
// before the response, so a denied start is never silent.
func (d sessionRouteDeps) handleChiefOfStaffStart(w http.ResponseWriter, r *http.Request) {
	const origin = sessiontypes.OriginChiefOfStaff
	if d.sessionRoutesUnavailable(w) {
		return
	}

	var body chiefOfStaffStartBody
	r.Body = http.MaxBytesReader(w, r.Body, maxChiefOfStaffBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeChiefOfStaffError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is larger than 64 KiB")
			return
		}
		writeChiefOfStaffError(w, http.StatusBadRequest, "invalid_body", "body must be JSON {\"projectId\",\"folder\",\"prompt\",\"model\",\"mode\"}")
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	mode := body.Mode
	if mode == "" {
		mode = chiefOfStaffModeHeadless
	}
	switch {
	case body.ProjectID == "":
		writeChiefOfStaffError(w, http.StatusBadRequest, "project_id_required", "projectId is required")
		return
	case prompt == "":
		writeChiefOfStaffError(w, http.StatusBadRequest, "prompt_required", "prompt is required")
		return
	case utf8.RuneCountInString(prompt) > maxChiefOfStaffPromptRunes:
		writeChiefOfStaffError(w, http.StatusBadRequest, "prompt_too_long", "prompt is longer than 8000 characters")
		return
	case body.Model == "":
		writeChiefOfStaffError(w, http.StatusBadRequest, "model_required", "model is required")
		return
	case mode != chiefOfStaffModeHeadless && mode != chiefOfStaffModeTerminal:
		writeChiefOfStaffError(w, http.StatusBadRequest, "mode_invalid", `mode must be "headless" or "terminal"`)
		return
	case !folderSyntaxOK(body.Folder):
		writeChiefOfStaffError(w, http.StatusBadRequest, "folder_invalid", "folder must be a relative path with no .. segment")
		return
	}
	if !d.auditor.Ready() {
		writeChiefOfStaffError(w, http.StatusServiceUnavailable, "audit_unavailable", "auditing is off; the Chief of Staff cannot start a session")
		return
	}

	caller := resolveLaunchCaller(r, d.store)
	kind := KindPTY
	if mode == chiefOfStaffModeHeadless {
		kind = deriveSessionKind(body.Model)
	}
	fields := sessionLaunchAuditFields{
		Actor: callerAuditActor(caller), ProjectID: body.ProjectID, Kind: kind,
		Origin: origin, PromptBytes: len(prompt),
	}
	deny := func(status int, code, message string) {
		d.auditor.Record(newSessionLaunchAuditEvent(fields, audit.AuditOutcomeDenied, message))
		writeChiefOfStaffError(w, status, code, message)
	}

	proj, _ := config.FindProjectByID(config.FreshSettings(d.store), body.ProjectID)
	if proj == nil || proj.IsRemote() {
		deny(http.StatusForbidden, "project_not_available", "project is not available for a session launch")
		return
	}
	fields.ProjectName = proj.Name
	fields.HostID = proj.HostID

	directory, status, code, message := confineChiefOfStaffFolder(proj, body.Folder)
	fields.Directory = directory
	if code != "" {
		deny(status, code, message)
		return
	}

	req := LaunchRequest{
		Caller: caller, ProjectID: body.ProjectID, Kind: kind, Directory: directory,
		Name: chiefOfStaffSessionName(prompt), Model: body.Model,
		Origin: origin, PromptBytes: len(prompt),
	}
	if mode == chiefOfStaffModeHeadless {
		req.ClientSettings = json.RawMessage(`{"headless":true,"agent":true}`)
	} else {
		// A terminal session cannot run on a host (checkTerminalExtraArgs
		// refuses extra args there), so refuse before anything else.
		if proj.IsHosted() {
			deny(http.StatusBadRequest, "terminal_on_host", "a terminal start is not available in a project on an SSH host; start a headless agent")
			return
		}
		// The terminal runs Claude Code, the only program that takes --model
		// and a prompt argument.
		if deriveSessionKind(body.Model) != KindClaude {
			deny(http.StatusBadRequest, "terminal_needs_claude", "a terminal start needs a Claude model")
			return
		}
		if !modelAllowedForProject(d.store, body.ProjectID, body.Model) {
			deny(http.StatusForbidden, "model_not_allowed", "model is not allowed for this project")
			return
		}
		req.TemplateID = "claude-code"
		req.Model = ""
		req.ExtraArgs = []string{"--model", body.Model, "--", prompt}
	}

	result, resp, refusal, err := d.launch(r.Context(), req)
	switch {
	case refusal != nil:
		writeChiefOfStaffError(w, refusal.Status, refusal.Code, refusal.Message)
		return
	case err != nil || result == nil || resp == nil:
		writeChiefOfStaffError(w, http.StatusBadGateway, "launch_failed", "the session could not be started")
		return
	}

	if mode == chiefOfStaffModeHeadless {
		status, _, _ := d.deliverChiefOfStaffText(r.Context(), fields.Actor, result.SessionID, prompt)
		if status != http.StatusAccepted {
			message := "the session started but the prompt could not be delivered; it was ended"
			if err := d.host().Terminate(context.WithoutCancel(r.Context()), result.SessionID, "chief_of_staff_start_failed"); err != nil {
				slog.ErrorContext(r.Context(), "chief of staff start: terminate after undelivered prompt failed", "session", result.SessionID, "error", err)
				message = "the session started but the prompt could not be delivered; it could not be ended and may still be running"
			}
			writeChiefOfStaffError(w, http.StatusBadGateway, "prompt_not_delivered", message)
			return
		}
	}

	writeJSON(w, http.StatusCreated, chiefOfStaffStartResult{
		SessionID: result.SessionID, Name: req.Name, ProjectID: body.ProjectID,
		Directory: result.Spec.Directory, Mode: mode, Kind: kind, Origin: origin,
		At: time.Now().UTC().Format(time.RFC3339),
	})
}

// folderSyntaxOK refuses a folder that is absolute, holds a NUL or has a ..
// segment. It is a cheap first cut; the containment check in
// confineChiefOfStaffFolder is the one that decides.
func folderSyntaxOK(folder string) bool {
	if strings.ContainsRune(folder, 0) || filepath.IsAbs(folder) || strings.HasPrefix(folder, "/") {
		return false
	}
	for _, seg := range strings.Split(folder, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// confineChiefOfStaffFolder resolves folder under the project and answers the
// resolved directory. A non-empty code is a refusal. Symlinks are resolved
// before the containment test, so a link inside the project that points out
// of it is refused.
//
// A hosted project's path is on the host, not on this Mac, so there is
// nothing to resolve: the join is text only, as for any hosted launch
// (AuthorizeLaunch checks it lexically too). A symlink on the host is not
// seen here; the session runs as the host account either way.
func confineChiefOfStaffFolder(proj *config.Project, folder string) (directory string, status int, code, message string) {
	if proj.IsHosted() {
		root := filepath.Clean(proj.Path)
		dir := filepath.Clean(filepath.Join(proj.Path, folder))
		if !lexicalDirWithin(dir, root) {
			return dir, http.StatusForbidden, "directory_outside_project", "requested directory is outside the project"
		}
		return dir, 0, "", ""
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(proj.Path, folder))
	if err != nil {
		return "", http.StatusBadRequest, "folder_not_found", "folder does not exist in the project"
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", http.StatusBadRequest, "folder_not_found", "folder does not exist in the project"
	}
	if !project.DirWithin(resolved, realpath(proj.Path)) {
		return resolved, http.StatusForbidden, "directory_outside_project", "requested directory is outside the project"
	}
	return resolved, 0, "", ""
}

// chiefOfStaffSessionName is the prompt's first line with whitespace
// collapsed, cut to 60 runes.
func chiefOfStaffSessionName(prompt string) string {
	first, _, _ := strings.Cut(prompt, "\n")
	name := strings.Join(strings.Fields(first), " ")
	if utf8.RuneCountInString(name) > maxChiefOfStaffNameRunes {
		name = string([]rune(name)[:maxChiefOfStaffNameRunes])
	}
	if name == "" {
		return defaultChiefOfStaffName
	}
	return name
}
