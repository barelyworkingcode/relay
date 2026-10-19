package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
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
	Entry     string `json:"entry"`
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
	Entry     string `json:"entry,omitempty"`
	EntryType string `json:"entryType,omitempty"`
	At        string `json:"at"`
}

const (
	chiefOfStaffEntryAgent = "agent"
	chiefOfStaffEntryChat  = "chat"
)

// chiefOfStaffTarget is what an entry name resolved to: exactly one of Agent
// and Chat is meaningful, as Type says.
type chiefOfStaffTarget struct {
	Type  string
	Agent config.AgentEntry
	Chat  config.ChatTemplate
}

// chiefOfStaffRefusalBody is the coded error family plus the names a caller
// could have used instead. Choices is omitted when there are none.
type chiefOfStaffRefusalBody struct {
	Error   string   `json:"error"`
	Message string   `json:"message"`
	Choices []string `json:"choices,omitempty"`
}

func writeChiefOfStaffRefusal(w http.ResponseWriter, r *LaunchRefusal) {
	writeJSON(w, r.Status, chiefOfStaffRefusalBody{Error: r.Code, Message: r.Message, Choices: r.Choices})
}

// chiefOfStaffEntryChoices lists the names a start could use: agents, then
// chats, in stored order, each name once.
func chiefOfStaffEntryChoices(proj *config.Project) []string {
	var names []string
	add := func(name string) {
		if len(names) >= maxRefusalChoices {
			return
		}
		for _, n := range names {
			if strings.EqualFold(n, name) {
				return
			}
		}
		names = append(names, name)
	}
	for _, a := range proj.Agents {
		add(a.Name)
	}
	for _, t := range proj.ChatTemplates {
		add(t.Name)
	}
	return names
}

// resolveChiefOfStaffEntry finds name among the project's agents and chat
// templates, case-insensitively. Names are unique across both, so a second
// match (hand-edited settings) is refused rather than guessed at.
func resolveChiefOfStaffEntry(proj *config.Project, name string, fields sessionLaunchAuditFields) (chiefOfStaffTarget, *LaunchRefusal) {
	shown := capAuditText(name, maxRefusalNameRunes)
	var agents []config.AgentEntry
	var chats []config.ChatTemplate
	for _, a := range proj.Agents {
		if strings.EqualFold(a.Name, name) {
			agents = append(agents, a)
		}
	}
	for _, t := range proj.ChatTemplates {
		if strings.EqualFold(t.Name, name) {
			chats = append(chats, t)
		}
	}
	switch {
	case len(agents)+len(chats) == 0:
		choices := chiefOfStaffEntryChoices(proj)
		message := fmt.Sprintf("No agent or chat %q in %s; %s has none", shown, proj.Name, proj.Name)
		if len(choices) > 0 {
			message = fmt.Sprintf("No agent or chat %q in %s. Choices: %s", shown, proj.Name, strings.Join(choices, ", "))
		}
		return chiefOfStaffTarget{}, deniedRefusal(http.StatusNotFound, "entry_not_found", message, choices, fields)
	case len(agents) > 0 && len(chats) > 0:
		return chiefOfStaffTarget{}, deniedRefusal(http.StatusConflict, "entry_ambiguous",
			fmt.Sprintf("An agent and a chat in %s are both named %q; rename the chat in eve", proj.Name, shown), nil, fields)
	case len(agents)+len(chats) > 1:
		kind, first := "chats", ""
		if len(agents) > 0 {
			kind, first = "agents", agents[0].Name
		} else {
			first = chats[0].Name
		}
		return chiefOfStaffTarget{}, deniedRefusal(http.StatusConflict, "entry_ambiguous",
			fmt.Sprintf("Two %s in %s are named %q; rename one in eve", kind, proj.Name, shown), []string{first}, fields)
	case len(agents) == 1:
		return chiefOfStaffTarget{Type: chiefOfStaffEntryAgent, Agent: agents[0]}, nil
	}
	t := chats[0]
	if t.Model == "" {
		return chiefOfStaffTarget{}, deniedRefusal(http.StatusBadRequest, "entry_invalid",
			fmt.Sprintf("chat %q in %s has no model", capAuditText(t.Name, maxRefusalNameRunes), proj.Name), nil, fields)
	}
	return chiefOfStaffTarget{Type: chiefOfStaffEntryChat, Chat: t}, nil
}

// chatTemplateLaunch builds the launch for a chat template as eve's
// applyChatDefaults does. Entry stays empty on purpose: the start is then a
// raw-model one, so the model policy and the system-model refusal apply as
// for any chat. Mode, voice and preset are ignored.
func chatTemplateLaunch(req LaunchRequest, proj *config.Project, t config.ChatTemplate) LaunchRequest {
	req.Kind = deriveSessionKind(t.Model)
	req.Model = t.Model
	req.SystemPrompt = t.SystemPrompt
	req.Name = proj.Name + " - " + t.Name
	req.Entry = ""
	if req.Kind == KindClaude {
		req.ClientSettings = json.RawMessage(`{"headless":true,"agent":true}`)
	} else {
		req.ClientSettings = json.RawMessage(`{"headless":true,"agent":true,"useRelayTools":true}`)
		req.AppendClaudeMd = true
	}
	return req
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
	ev := logging.BeginEvent(r.Context(), "chief_of_staff.start")
	w = newEventResponseWriter(w, func(status int) { endEventHTTP(ev, status, "chief of staff start refused") })
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
		writeChiefOfStaffError(w, http.StatusBadRequest, "invalid_body", "body must be JSON {\"projectId\",\"folder\",\"prompt\",\"model\",\"entry\",\"mode\"}")
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
	case body.Model != "" && body.Entry != "":
		writeChiefOfStaffError(w, http.StatusBadRequest, "model_or_entry", "send model or entry, not both")
		return
	case body.Model == "" && body.Entry == "":
		writeChiefOfStaffError(w, http.StatusBadRequest, "model_or_entry", "send model or entry")
		return
	case mode != chiefOfStaffModeHeadless && mode != chiefOfStaffModeTerminal:
		writeChiefOfStaffError(w, http.StatusBadRequest, "mode_invalid", `mode must be "headless" or "terminal"`)
		return
	case body.Entry != "" && mode == chiefOfStaffModeTerminal:
		writeChiefOfStaffError(w, http.StatusBadRequest, "mode_invalid", "an entry start is headless only")
		return
	case !folderSyntaxOK(body.Folder):
		writeChiefOfStaffError(w, http.StatusBadRequest, "folder_invalid", "folder must be a relative path with no .. segment")
		return
	}
	if !d.auditor.Ready() {
		ev.End(logging.OutcomeDenied, "audit_unavailable", errors.New("audit_unavailable"))
		writeChiefOfStaffError(w, http.StatusServiceUnavailable, "audit_unavailable", "auditing is off; the Chief of Staff cannot start a session")
		return
	}

	caller := resolveLaunchCaller(r, d.store)
	// An entry's kind is known only once it resolves.
	kind := ""
	switch {
	case body.Entry != "":
	case mode == chiefOfStaffModeHeadless:
		kind = deriveSessionKind(body.Model)
	default:
		kind = KindPTY
	}
	ev.Set("project_id", body.ProjectID)
	if kind != "" {
		ev.Set("kind", kind)
	}
	if body.Entry != "" {
		ev.Set("entry", capAuditText(body.Entry, maxRefusalNameRunes))
	}
	fields := sessionLaunchAuditFields{
		Actor: callerAuditActor(caller), ProjectID: body.ProjectID, Kind: kind,
		Origin: origin, PromptBytes: len(prompt),
		Entry: body.Entry,
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
	entryName, entryType := "", ""
	if body.Entry != "" {
		// After the project and folder checks, so a name is never confirmed to
		// a caller who could not start there anyway.
		target, refusal := resolveChiefOfStaffEntry(proj, body.Entry, fields)
		if refusal != nil {
			d.auditor.Record(refusal.Audit)
			endEvent(ev, refusal)
			writeChiefOfStaffRefusal(w, refusal)
			return
		}
		entryType = target.Type
		req.ClientSettings = json.RawMessage(`{"headless":true,"agent":true}`)
		if target.Type == chiefOfStaffEntryAgent {
			entryName = target.Agent.Name
			req.Entry = entryName
			fields.Entry = entryName
		} else {
			entryName = target.Chat.Name
			req = chatTemplateLaunch(req, proj, target.Chat)
		}
		ev.Set("entry", capAuditText(entryName, maxRefusalNameRunes)).Set("entry_type", entryType)
	} else if mode == chiefOfStaffModeHeadless {
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
		endRawModelRefusal(ev, req, refusal)
		writeChiefOfStaffRefusal(w, refusal)
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

	ev.Set("session_id", result.SessionID).Set("kind", result.Spec.Kind)
	writeJSON(w, http.StatusCreated, chiefOfStaffStartResult{
		SessionID: result.SessionID, Name: req.Name, ProjectID: body.ProjectID,
		Directory: result.Spec.Directory, Mode: mode, Kind: result.Spec.Kind, Origin: origin,
		Entry: entryName, EntryType: entryType,
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
