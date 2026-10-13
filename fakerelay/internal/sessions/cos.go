package sessions

import (
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

const cosOrigin = "chief-of-staff"

func cosErr(w http.ResponseWriter, status int, code, msg string) {
	server.WriteJSON(w, status, map[string]string{"error": code, "message": msg})
}

func (s *svc) cosMessage(w http.ResponseWriter, r *http.Request) {
	ev := s.Events.Begin(r.Context(), "chief_of_staff.send").Set("origin", cosOrigin)
	refuse := func(status int, reason, code, msg string) {
		ev.End("error", reason, errors.New(msg))
		cosErr(w, status, code, msg)
	}
	var b struct {
		SessionID string `json:"sessionId"`
		Text      string `json:"text"`
	}
	if err := decode(w, r, 64<<10, &b); err != nil {
		if errors.Is(err, errTooLarge) {
			refuse(http.StatusRequestEntityTooLarge, "invalid", "body_too_large", "request body is larger than 64 KiB")
			return
		}
		refuse(http.StatusBadRequest, "invalid", "invalid_body", `body must be JSON {"sessionId","text"}`)
		return
	}
	ev.Set("session_id", b.SessionID)
	switch {
	case b.SessionID == "":
		refuse(http.StatusBadRequest, "invalid", "session_id_required", "sessionId is required")
		return
	case b.Text == "":
		refuse(http.StatusBadRequest, "invalid", "text_required", "text is required")
		return
	}
	x, ref := s.beginTurn(b.SessionID, b.Text, cosOrigin)
	if ref != nil {
		switch ref.code {
		case "session_not_found":
			refuse(http.StatusNotFound, "not_found", "session_not_found", "session not found")
		case "already_processing":
			refuse(http.StatusConflict, "conflict", "already_processing", "the session is already processing a message")
		case "resume_required":
			refuse(http.StatusConflict, "conflict", "resume_required", "the session is not running; resume it first")
		default:
			refuse(http.StatusBadGateway, "upstream", "session_host_unavailable", "the session host could not be reached")
		}
		return
	}
	at := stamp(s.now())
	ev.End("ok", "", nil)
	s.Events.Begin(r.Context(), "session.message", events.Sessions()).Set("session_id", x.id).End("ok", "", nil)
	go s.runTurn(x, b.Text, cosOrigin, events.TraceFrom(r.Context()))
	server.WriteJSON(w, http.StatusAccepted, map[string]string{"sessionId": x.id, "origin": cosOrigin, "at": at})
}

// cosCodes renames the shared launch refusal codes to the ones the Chief of
// Staff routes answer.
var cosCodes = map[string]string{"project_unavailable": "project_not_available", "directory_outside": "directory_outside_project"}

func cosRefusalBody(ref *refusal, choices []string) map[string]any {
	code := ref.code
	if c, renamed := cosCodes[code]; renamed {
		code = c
	}
	body := map[string]any{"error": code, "message": ref.msg}
	if len(choices) > 0 {
		body["choices"] = choices
	}
	return body
}

func (s *svc) cosStart(w http.ResponseWriter, r *http.Request) {
	ev := s.Events.Begin(r.Context(), "chief_of_staff.start")
	refuse := func(status int, reason, code, msg string) {
		ev.End("error", reason, errors.New(msg))
		cosErr(w, status, code, msg)
	}
	// deny ends the event as denied under the refusal's code and answers with
	// its choices.
	deny := func(ref *refusal, choices []string) {
		status := "denied"
		if ref.status >= 500 {
			status = "error"
		}
		ev.End(status, ref.code, errors.New(ref.msg))
		server.WriteJSON(w, ref.status, cosRefusalBody(ref, choices))
	}
	var b struct {
		ProjectID string `json:"projectId"`
		Folder    string `json:"folder"`
		Prompt    string `json:"prompt"`
		Model     string `json:"model"`
		Entry     string `json:"entry"`
		Mode      string `json:"mode"`
	}
	if err := decode(w, r, 64<<10, &b); err != nil {
		if errors.Is(err, errTooLarge) {
			refuse(http.StatusRequestEntityTooLarge, "invalid", "body_too_large", "request body is larger than 64 KiB")
			return
		}
		refuse(http.StatusBadRequest, "invalid", "invalid_body", `body must be JSON {"projectId","folder","prompt","model","entry","mode"}`)
		return
	}
	ev.Set("project_id", b.ProjectID)
	if b.Mode == "" {
		b.Mode = "headless"
	}
	prompt := strings.TrimSpace(b.Prompt)
	switch {
	case b.ProjectID == "":
		refuse(http.StatusBadRequest, "invalid", "project_id_required", "projectId is required")
		return
	case prompt == "":
		refuse(http.StatusBadRequest, "invalid", "prompt_required", "prompt is required")
		return
	case utf8.RuneCountInString(prompt) > 8000:
		refuse(http.StatusBadRequest, "invalid", "prompt_too_long", "prompt is longer than 8000 characters")
		return
	case b.Model != "" && b.Entry != "":
		refuse(http.StatusBadRequest, "invalid", "model_or_entry", "send model or entry, not both")
		return
	case b.Model == "" && b.Entry == "":
		refuse(http.StatusBadRequest, "invalid", "model_or_entry", "send model or entry")
		return
	case b.Mode != "headless" && b.Mode != "terminal":
		refuse(http.StatusBadRequest, "invalid", "mode_invalid", `mode must be "headless" or "terminal"`)
		return
	case b.Entry != "" && b.Mode == "terminal":
		refuse(http.StatusBadRequest, "invalid", "mode_invalid", "an entry start is headless only")
		return
	case !folderSyntaxOK(b.Folder):
		refuse(http.StatusBadRequest, "invalid", "folder_invalid", "folder must be a relative path with no .. segment")
		return
	}
	if b.Entry != "" {
		s.cosStartEntry(w, r, ev, b.ProjectID, b.Folder, prompt, b.Entry, deny)
		return
	}
	kind := s.kindOf(b.Model)
	t, ref := s.authorize(b.ProjectID, path.Clean(b.Folder), b.Model, kind)
	if ref != nil {
		endHTTP(ev, ref.status, ref.msg)
		server.WriteJSON(w, ref.status, cosRefusalBody(ref, nil))
		return
	}
	x := s.launch(t, launchBody{Folder: b.Folder, Name: promptName(prompt), Model: b.Model}, kind, cosOrigin, b.Mode == "headless")
	ev.Set("session_id", x.id).Set("kind", kind).End("ok", "", nil)
	s.cosDeliver(r, x, prompt)
	server.WriteJSON(w, http.StatusCreated, s.cosCreated(x, b.Mode, kind, nil))
}

// promptName is the prompt's first line with whitespace collapsed, cut to 60
// runes.
func promptName(prompt string) string {
	first, _, _ := strings.Cut(prompt, "\n")
	name := strings.Join(strings.Fields(first), " ")
	if utf8.RuneCountInString(name) > 60 {
		name = string([]rune(name)[:60])
	}
	if name == "" {
		return "Chief of Staff agent"
	}
	return name
}

// folderSyntaxOK refuses a folder that is absolute, holds a NUL or has a ..
// segment.
func folderSyntaxOK(folder string) bool {
	if strings.ContainsRune(folder, 0) || strings.HasPrefix(folder, "/") {
		return false
	}
	for _, seg := range strings.Split(folder, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// endHTTP ends an event the way the real door's response writer does: the
// outcome follows the status.
func endHTTP(ev *events.Event, status int, _ string) {
	const msg = "chief of staff start refused"
	switch {
	case status == http.StatusForbidden:
		ev.End("denied", "not_granted", errors.New(msg))
	case status == http.StatusNotFound:
		ev.End("error", "not_found", errors.New(msg))
	case status == http.StatusConflict:
		ev.End("error", "conflict", errors.New(msg))
	case status == http.StatusServiceUnavailable:
		ev.End("error", "unavailable", errors.New(msg))
	case status < 500:
		ev.End("error", "invalid", errors.New(msg))
	default:
		ev.End("error", "internal", errors.New(msg))
	}
}

func (s *svc) cosDeliver(r *http.Request, x *session, prompt string) {
	if sx, ref := s.beginTurn(x.id, prompt, cosOrigin); ref == nil {
		go s.runTurn(sx, prompt, cosOrigin, events.TraceFrom(r.Context()))
	}
}

func (s *svc) cosCreated(x *session, mode, kind string, entry map[string]string) map[string]any {
	v := map[string]any{"sessionId": x.id, "name": x.name, "projectId": x.projectID,
		"directory": x.directory, "mode": mode, "kind": kind, "origin": cosOrigin, "at": stamp(s.now())}
	for k, val := range entry {
		v[k] = val
	}
	return v
}

// cosStartEntry starts a session from an agent entry or chat template of the
// project. The project and folder checks run first, so only a caller allowed
// into the project sees the entry names.
func (s *svc) cosStartEntry(w http.ResponseWriter, r *http.Request, ev *events.Event, projectID, folder, prompt, entry string, deny func(*refusal, []string)) {
	ev.Set("entry", entry)
	t, ref := s.authorize(projectID, path.Clean(folder), "", "agent")
	if ref != nil {
		endHTTP(ev, ref.status, ref.msg)
		server.WriteJSON(w, ref.status, cosRefusalBody(ref, nil))
		return
	}
	p := t.project
	var agents []world.AgentEntry
	for _, a := range p.Agents {
		if strings.EqualFold(a.Name, entry) {
			agents = append(agents, a)
		}
	}
	var chats []world.ChatTemplate
	for _, c := range world.ChatTemplatesOf(p) {
		if strings.EqualFold(c.Name, entry) {
			chats = append(chats, c)
		}
	}
	switch {
	case len(agents)+len(chats) == 0:
		choices := entryChoices(p)
		msg := "No agent or chat " + strconv.Quote(entry) + " in " + p.Name + "; " + p.Name + " has none"
		if len(choices) > 0 {
			msg = "No agent or chat " + strconv.Quote(entry) + " in " + p.Name + ". Choices: " + strings.Join(choices, ", ")
		}
		deny(&refusal{http.StatusNotFound, "entry_not_found", msg}, choices)
		return
	case len(agents) > 0 && len(chats) > 0:
		deny(&refusal{http.StatusConflict, "entry_ambiguous", "An agent and a chat in " + p.Name + " are both named " + strconv.Quote(entry) + "; rename the chat in eve"}, nil)
		return
	case len(agents)+len(chats) > 1:
		kind, first := "chats", ""
		if len(agents) > 0 {
			kind, first = "agents", agents[0].Name
		} else {
			first = chats[0].Name
		}
		deny(&refusal{http.StatusConflict, "entry_ambiguous", "Two " + kind + " in " + p.Name + " are named " + strconv.Quote(entry) + "; rename one in eve"}, []string{first})
		return
	}
	if len(agents) == 1 {
		ev.Set("entry", agents[0].Name).Set("entry_type", "agent")
		s.cosStartAgent(w, r, ev, t, folder, prompt, agents[0], deny)
		return
	}
	c := chats[0]
	if c.Model == "" {
		deny(&refusal{http.StatusBadRequest, "entry_invalid", "chat " + strconv.Quote(c.Name) + " in " + p.Name + " has no model"}, nil)
		return
	}
	ev.Set("entry", c.Name).Set("entry_type", "chat")
	kind := s.kindOf(c.Model)
	if _, ref := s.authorize(projectID, path.Clean(folder), c.Model, kind); ref != nil {
		deny(ref, nil)
		return
	}
	settings := map[string]any{"headless": true, "agent": true}
	if kind != "claude" {
		settings["useRelayTools"] = true
	}
	x := s.launch(t, launchBody{Folder: folder, Name: p.Name + " - " + c.Name, Model: c.Model, Settings: settings}, kind, cosOrigin, true)
	ev.Set("session_id", x.id).Set("kind", kind).End("ok", "", nil)
	s.cosDeliver(r, x, prompt)
	server.WriteJSON(w, http.StatusCreated, s.cosCreated(x, "headless", kind, map[string]string{"entry": c.Name, "entryType": "chat"}))
}

func (s *svc) cosStartAgent(w http.ResponseWriter, r *http.Request, ev *events.Event, t launchTarget, folder, prompt string, a world.AgentEntry, deny func(*refusal, []string)) {
	if rule := world.ValidateStoredAgent(a); rule != nil {
		deny(&refusal{http.StatusBadRequest, "entry_invalid", `agent "` + a.Name + `" is not valid: ` + rule.Message}, nil)
		return
	}
	model := world.SessionModel(a)
	if ref := s.checkEntryModel(t.project, a, model); ref != nil {
		deny(&ref.refusal, ref.choices)
		return
	}
	settings := map[string]any{"headless": true, "agent": true}
	x := s.launch(t, launchBody{Folder: folder, Name: promptName(prompt), Model: model, Settings: settings}, a.Kind, cosOrigin, true)
	if a.Command != "" {
		x.launcher = &launcher{Command: a.Command, Args: a.Args}
	}
	ev.Set("session_id", x.id).Set("kind", a.Kind).End("ok", "", nil)
	s.cosDeliver(r, x, prompt)
	server.WriteJSON(w, http.StatusCreated, s.cosCreated(x, "headless", a.Kind, map[string]string{"entry": a.Name, "entryType": "agent"}))
}

// entryChoices lists the project's agent names then chat names, each once
// without regard to case, in stored order.
func entryChoices(p world.Project) []string {
	var out []string
	add := func(n string) {
		if len(out) >= maxChoices {
			return
		}
		for _, o := range out {
			if strings.EqualFold(o, n) {
				return
			}
		}
		out = append(out, n)
	}
	for _, a := range p.Agents {
		add(a.Name)
	}
	for _, c := range world.ChatTemplatesOf(p) {
		add(c.Name)
	}
	return out
}
