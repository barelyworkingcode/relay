package sessions

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/fakes"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

type session struct {
	id, projectID, name, directory, folder, model, kind, origin, permMode string
	headless, agentOn, hidden, held, dropped, unsandboxed                 bool
	createdAt, lastMsg, since                                             time.Time
	host                                                                  map[string]any
	history                                                               []map[string]any
	stats                                                                 map[string]any
	live, processing                                                      bool
	askedName, claudeID, tool                                             string
	launcher                                                              *launcher
	changed                                                               chan struct{}
	state                                                                 string
	exit                                                                  *fakes.ExitError
	agent                                                                 fakes.Agent
	cancel                                                                context.CancelFunc
	viewers                                                               map[*conn]bool
}

func newStats() map[string]any {
	return map[string]any{"inputTokens": 0, "outputTokens": 0, "cacheReadTokens": 0, "cacheCreationTokens": 0, "costUsd": 0}
}

// tracked sessions carry attention state; chat sessions and headless ones
// launched without agent do not.
func (x *session) tracked() bool {
	return (!x.headless || x.agentOn) && (x.kind == "claude" || x.kind == "pi" || x.kind == "codex")
}

// wake releases every drop-in waiting on this session. Callers hold s.mu.
func (x *session) wake() {
	if x.changed != nil {
		close(x.changed)
		x.changed = nil
	}
}

func textBlocks(text string) []map[string]any {
	return []map[string]any{{"type": "text", "text": text}}
}

func (s *svc) seed() {
	for _, ws := range s.World.Sessions {
		x := &session{id: ws.ID, projectID: ws.ProjectID, name: ws.Name, model: ws.Model, kind: s.kindOf(ws.Model),
			createdAt: s.started, since: s.started, live: ws.State != "dormant", state: "idle", stats: newStats(),
			viewers: map[*conn]bool{}}
		if x.live == false {
			x.state = "ended"
		}
		s.State.Read(func(m *state.Model) {
			if p, found := lookupProject(m, ws.ProjectID); found {
				x.directory = p.Path
			}
		})
		for _, msg := range ws.Messages {
			x.history = append(x.history, historyMsg(msg.Role, msg.Text, s.started, ""))
		}
		s.sessions[x.id] = x
	}
}

func historyMsg(role, text string, at time.Time, origin string) map[string]any {
	m := map[string]any{"timestamp": at.UTC().Format(time.RFC3339), "role": role, "content": any(text)}
	if role == "assistant" {
		m["content"] = textBlocks(text)
	}
	if origin != "" {
		m["origin"] = origin
	}
	return m
}

type launchBody struct {
	ProjectID      string         `json:"projectId"`
	Directory      string         `json:"directory"`
	Folder         string         `json:"folder"`
	Name           string         `json:"name"`
	Model          string         `json:"model"`
	Settings       map[string]any `json:"settings"`
	SystemPrompt   string         `json:"systemPrompt"`
	AppendClaudeMd bool           `json:"appendClaudeMd"`
	Entry          string         `json:"entry"`
}

// launcher is the command an agent entry starts the session with.
type launcher struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// launch creates a live session. The caller has authorised the target.
func (s *svc) launch(t launchTarget, b launchBody, kind, origin string, headless bool) *session {
	x := &session{id: newUUID(), projectID: t.project.ID, name: b.Name, directory: t.dir, folder: b.Folder, model: b.Model, kind: kind,
		origin: origin, headless: headless, askedName: b.Name, createdAt: s.now(), host: t.host, live: true, state: "idle", stats: newStats(),
		viewers: map[*conn]bool{}, unsandboxed: t.unsandboxed}
	x.since = x.createdAt
	x.agentOn, _ = b.Settings["agent"].(bool)
	// Chief of Staff headless sessions stay listed; a client's headless launch
	// without agent is not.
	x.hidden = headless && !x.agentOn && origin == ""
	if mode, _ := b.Settings["permissionMode"].(string); mode != "" {
		x.permMode = mode
	}
	if x.name == "" {
		x.name = "Session " + x.id[:8]
	}
	s.mu.Lock()
	s.sessions[x.id] = x
	s.mu.Unlock()
	return x
}

func (s *svc) row(x *session) map[string]any {
	v := map[string]any{"id": x.id, "projectId": x.projectID, "name": x.name, "directory": x.directory, "model": x.model,
		"live": x.live, "createdAt": x.createdAt.Format(time.RFC3339), "messageCount": len(x.history)}
	if !x.lastMsg.IsZero() {
		v["lastMessageAt"] = x.lastMsg.Format(time.RFC3339)
	}
	if x.folder != "" {
		v["folder"] = x.folder
	}
	if x.host != nil {
		v["host"] = x.host
	}
	if x.headless {
		v["headless"] = true
	}
	if x.origin != "" {
		v["origin"] = x.origin
	}
	if x.unsandboxed {
		v["unsandboxed"] = true
	}
	if (x.live || x.dropped || x.exit != nil) && x.tracked() {
		att := map[string]any{"state": x.state, "since": stamp(x.since)}
		if x.exit != nil {
			att["exit"] = exitView(x.exit)
		}
		v["attention"] = att
	}
	return v
}

func (s *svc) listSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	rows := []map[string]any{}
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !s.sessions[id].hidden {
			rows = append(rows, s.row(s.sessions[id]))
		}
	}
	s.mu.Unlock()
	s.Events.Begin(r.Context(), "session.list").Set("count", len(rows)).End("ok", "", nil)
	ok(w, map[string]any{"sessions": rows})
}

func (s *svc) createSession(w http.ResponseWriter, r *http.Request) {
	ev := s.Events.Begin(r.Context(), "session.launch")
	var b launchBody
	if err := decode(w, r, 1<<20, &b); err != nil {
		ev.End("error", "invalid", err)
		if errors.Is(err, errTooLarge) {
			server.WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		server.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if b.Entry != "" {
		s.createByEntry(w, ev, b)
		return
	}
	kind := s.kindOf(b.Model)
	ev.Set("kind", kind).Set("project_id", b.ProjectID)
	if kind == "chat" && b.Model == "" {
		ev.End("error", "invalid", errors.New("no model"))
		server.WriteError(w, http.StatusBadRequest, "chat session has no model; choose a model and try again")
		return
	}
	t, ref := s.authorize(b.ProjectID, b.Directory, b.Model, kind)
	if ref != nil {
		ev.End("denied", ref.code, errors.New(ref.msg))
		server.WriteError(w, ref.status, ref.msg)
		return
	}
	headless, _ := b.Settings["headless"].(bool)
	x := s.launch(t, b, kind, "", headless)
	ev.Set("session_id", x.id).End("ok", "", nil)
	server.WriteJSON(w, http.StatusCreated, s.createdView(x))
}

// createByEntry starts a session from a project's agent entry. The project and
// directory checks run first, so only a caller allowed into the project sees
// the entry names.
func (s *svc) createByEntry(w http.ResponseWriter, ev *events.Event, b launchBody) {
	ev.Set("project_id", b.ProjectID).Set("entry", b.Entry)
	refuse := func(ref *refusal) {
		ev.End("denied", ref.code, errors.New(ref.msg))
		server.WriteError(w, ref.status, ref.msg)
	}
	if b.Model != "" {
		refuse(&refusal{http.StatusBadRequest, "model_or_entry", "send model or entry, not both"})
		return
	}
	t, ref := s.authorize(b.ProjectID, b.Directory, "", "agent")
	if ref != nil {
		refuse(ref)
		return
	}
	entry, ref := s.resolveEntry(t.project, b.Entry)
	if ref != nil {
		refuse(ref)
		return
	}
	ev.Set("kind", entry.Kind)
	if entry.Command != "" {
		ev.Set("command", entry.Command)
	}
	model := world.SessionModel(entry)
	if ref := s.checkEntryModel(t.project, entry, model); ref != nil {
		refuse(&ref.refusal)
		return
	}
	b.Model = model
	x := s.launch(t, b, entry.Kind, "", false)
	if entry.Command != "" {
		x.launcher = &launcher{Command: entry.Command, Args: entry.Args}
	}
	ev.Set("session_id", x.id).End("ok", "", nil)
	server.WriteJSON(w, http.StatusCreated, s.createdView(x))
}

func (s *svc) resolveEntry(p world.Project, name string) (world.AgentEntry, *refusal) {
	entry, _, found := world.FindAgent(p, name)
	if !found {
		msg := `no agent "` + name + `" in ` + p.Name + "; " + p.Name + " has no agents"
		if len(p.Agents) > 0 {
			names := make([]string, len(p.Agents))
			for i, a := range p.Agents {
				names[i] = a.Name
			}
			msg = `no agent "` + name + `" in ` + p.Name + "; agents: " + strings.Join(names, ", ")
		}
		return entry, &refusal{http.StatusNotFound, "entry_not_found", msg}
	}
	if rule := world.ValidateStoredAgent(entry); rule != nil {
		return entry, &refusal{http.StatusBadRequest, "entry_invalid", `agent "` + entry.Name + `" is not valid: ` + rule.Message}
	}
	return entry, nil
}

// maxChoices caps the names a refusal offers.
const maxChoices = 50

// choiceRefusal is a refusal that offers the caller the names it could have
// sent. refusal has positional literals elsewhere, so the list rides here.
type choiceRefusal struct {
	refusal
	choices []string
}

// checkEntryModel applies the project's model allowlist and the kind's offered
// models to the model an entry asks for.
func (s *svc) checkEntryModel(p world.Project, e world.AgentEntry, model string) *choiceRefusal {
	if model == "" {
		if allowed(p.AllowedModels, "") {
			return nil
		}
		return &choiceRefusal{refusal: refusal{http.StatusForbidden, "model_not_allowed", `agent "` + e.Name + `" has no model, and ` + p.Name + ` allows only its listed models`}}
	}
	if !allowed(p.AllowedModels, model) {
		return &choiceRefusal{refusal: refusal{http.StatusForbidden, "model_not_allowed", "model is not allowed for this project"}}
	}
	offered := s.offeredModels(e.Kind)
	if len(offered) == 0 {
		return &choiceRefusal{refusal: refusal{http.StatusServiceUnavailable, "models_unavailable", "the model list for " + e.Kind + " could not be read"}}
	}
	var choices []string
	for _, o := range offered {
		if allowed(p.AllowedModels, world.SessionModel(world.AgentEntry{Kind: e.Kind, Model: o})) {
			choices = append(choices, o)
		}
		if o == e.Model {
			return nil
		}
	}
	if len(choices) > maxChoices {
		choices = choices[:maxChoices]
	}
	return &choiceRefusal{refusal: refusal{http.StatusBadRequest, "model_not_offered", `model "` + e.Model + `" is not offered for ` + e.Kind + "; models: " + strings.Join(choices, ", ")}, choices: choices}
}

// offeredModels is what a kind offers: claude's fixed rows, the world's codex
// rows without their prefix, and the world's chat rows for pi.
func (s *svc) offeredModels(kind string) []string {
	var out []string
	if kind == "claude" {
		for _, m := range fixedModels {
			out = append(out, m.Value)
		}
		return out
	}
	s.State.Read(func(m *state.Model) {
		for _, row := range m.Models {
			switch {
			case kind == "codex" && row.Provider == "codex":
				out = append(out, strings.TrimPrefix(row.Value, "codex/"))
			case kind == "pi" && s.kindOf(row.Value) == "chat":
				out = append(out, row.Value)
			}
		}
	})
	return out
}

func (s *svc) createdView(x *session) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := map[string]any{"sessionId": x.id, "projectId": x.projectID, "name": x.name, "directory": x.directory, "model": x.model,
		"providerType": x.kind, "createdAt": x.createdAt.Format(time.RFC3339), "messages": []any{}, "stats": x.stats,
	}
	if x.permMode != "" {
		v["permissionMode"] = x.permMode
	}
	if x.headless {
		v["headless"] = true
	}
	if x.agentOn {
		v["agent"] = true
	}
	if x.host != nil {
		v["host"] = x.host
	}
	if x.folder != "" {
		v["folder"] = x.folder
	}
	return v
}

func (s *svc) resumeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev := s.Events.Begin(r.Context(), "session.resume").Set("session_id", id)
	s.mu.Lock()
	x := s.sessions[id]
	resumed := false
	if x != nil && !x.live {
		x.live, x.state, x.since, x.exit, resumed = true, "idle", s.now(), nil, true
	}
	s.mu.Unlock()
	if x == nil {
		ev.End("error", "not_found", errors.New("session not found"))
		server.WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	ev.Set("project_id", x.projectID).End("ok", "", nil)
	ok(w, map[string]any{"session_id": id, "resumed": resumed})
}

func (s *svc) deleteSession(w http.ResponseWriter, r *http.Request) {
	s.removeSession(r.Context(), r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// removeSession drops a session and tells every connection. An unknown id is
// not an error.
func (s *svc) removeSession(ctx context.Context, id string) {
	s.mu.Lock()
	x := s.sessions[id]
	delete(s.sessions, id)
	var ended []string
	if x != nil {
		ended = s.dropPerms(x)
		if x.cancel != nil {
			x.cancel()
		}
	}
	s.mu.Unlock()
	s.resolved(x, ended, "ended")
	s.Events.Begin(ctx, "session.delete", events.Sessions()).Set("session_id", id).End("ok", "", nil)
	if x != nil {
		s.broadcast(map[string]any{"type": "session_ended", "sessionId": id})
	}
}

// turnRefusal explains why a message cannot start a turn.
type turnRefusal struct{ code, msg string }

func (s *svc) beginTurn(id, text, origin string) (*session, *turnRefusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.sessions[id]
	switch {
	case x == nil:
		return nil, &turnRefusal{"session_not_found", "session: not found"}
	case x.held:
		return nil, &turnRefusal{"dropped_in", "session: a terminal holds this session"}
	case !x.live:
		return nil, &turnRefusal{"resume_required", "session: provider not running; resume required"}
	case x.processing:
		return nil, &turnRefusal{"already_processing", "session: already processing a message"}
	}
	if x.agent == nil {
		model, _ := s.modelByValue(x.model)
		if model.Value == "" {
			model = world.Model{Value: x.model}
		}
		ag, err := s.factory(model, fakes.SessionInfo{ID: x.id, ProjectID: x.projectID, Name: x.name, Directory: x.directory, Model: x.model, Kind: x.kind})
		if err != nil {
			return nil, &turnRefusal{"", err.Error()}
		}
		x.agent = ag
	}
	if x.claudeID == "" {
		x.claudeID = newUUID()
	}
	x.processing = true
	x.history = append(x.history, historyMsg("user", text, s.now(), origin))
	x.lastMsg = s.now()
	return x, nil
}

func (s *svc) setState(x *session, st string) {
	if !x.tracked() {
		return
	}
	s.mu.Lock()
	x.state, x.since, x.exit = st, s.now(), nil
	since := x.since
	s.mu.Unlock()
	s.broadcast(map[string]any{"type": "session_state", "sessionId": x.id, "state": st, "since": stamp(since)})
}

func words(s string) int { return len(strings.Fields(s)) }

// runTurn drives one turn on a session beginTurn accepted. Frames leave in
// wire order; chat.turn is written before message_complete.
func (s *svc) runTurn(x *session, text, origin, trace string) {
	ctx, cancel := context.WithCancel(events.WithTrace(context.Background(), trace))
	s.mu.Lock()
	x.cancel = cancel
	agent := x.agent
	s.mu.Unlock()
	defer cancel()
	user := map[string]any{"type": "user_message", "sessionId": x.id, "text": text}
	if origin != "" {
		user["origin"] = origin
	}
	s.toViewers(x, user)
	s.setState(x, "running")
	var reply strings.Builder
	err := agent.Turn(ctx, text, func(f fakes.Frame) {
		out := map[string]any(f)
		out["sessionId"] = x.id
		switch f["type"] {
		case "llm_event":
			if ev, _ := f["event"].(map[string]any); ev != nil {
				if d, _ := ev["delta"].(map[string]any); d != nil && d["type"] == "text_delta" {
					reply.WriteString(str(d["text"]))
				}
			}
		case "permission_request":
			s.mu.Lock()
			s.perms[str(f["permissionId"])] = x.id
			s.permRaised[str(f["permissionId"])] = s.now()
			x.tool = str(f["toolName"])
			x.wake()
			s.mu.Unlock()
			s.setState(x, "asking")
		}
		s.toViewers(x, out)
	})
	var terr *fakes.TurnError
	failed := errors.As(err, &terr)
	var xerr *fakes.ExitError
	exited := errors.As(err, &xerr)

	s.mu.Lock()
	live := x.live
	if !failed && !exited {
		// A headless agent session's reply is not kept in its history.
		if !(x.headless && x.agentOn) {
			x.history = append(x.history, historyMsg("assistant", reply.String(), s.now(), ""))
		}
		x.lastMsg = s.now()
		x.stats["inputTokens"] = x.stats["inputTokens"].(int) + words(text)
		x.stats["outputTokens"] = x.stats["outputTokens"].(int) + words(reply.String())
		// relay reports measured speeds after a turn; the fake reports fixed
		// non-zero ones so the keys appear as they do there.
		x.stats["timeToFirstToken"], x.stats["tokensPerSecond"] = 0.01, 1.0
	}
	stats := map[string]any{}
	for k, v := range x.stats {
		stats[k] = v
	}
	x.processing, x.tool = false, ""
	x.wake()
	s.mu.Unlock()

	if exited {
		s.providerExited(ctx, x, xerr)
		return
	}
	done := map[string]any{"type": "message_complete", "sessionId": x.id}
	ev := s.Events.Begin(ctx, "chat.turn", events.Sessions()).Set("session_id", x.id)
	switch {
	case failed:
		s.setState(x, "errored")
		done["isError"], done["apiErrorStatus"] = true, terr.Status
		ev.End("error", "upstream", err)
	default:
		s.toViewers(x, map[string]any{"type": "stats_update", "sessionId": x.id, "stats": stats})
		if live && x.tracked() {
			s.broadcast(map[string]any{"type": "turn_done", "sessionId": x.id, "excerpt": lastRunes(reply.String(), 500), "at": stamp(s.now())})
			s.setState(x, "idle")
		}
		ev.End("ok", "", nil)
	}
	s.toViewers(x, done)
}

func exitView(e *fakes.ExitError) map[string]any {
	v := map[string]any{"code": e.Code}
	if len(e.Stderr) > 0 {
		v["stderr"] = e.Stderr
	}
	return v
}

// providerExited plays relay's order for a provider that dies mid-turn. The
// state and the exit are in place before process_exited leaves, so a list read
// after that frame already sees them. No turn_done and no message_complete: a
// process exit does not end a turn.
func (s *svc) providerExited(ctx context.Context, x *session, e *fakes.ExitError) {
	s.Events.Begin(ctx, "chat.turn", events.Sessions()).Set("session_id", x.id).End("error", "internal", errors.New("process_exited"))
	s.mu.Lock()
	x.live = false
	tracked := x.tracked()
	var since time.Time
	if tracked {
		x.state, x.since, x.exit = "errored", s.now(), e
		since = x.since
	}
	s.mu.Unlock()
	if tracked {
		s.broadcast(map[string]any{"type": "session_state", "sessionId": x.id, "state": "errored", "since": stamp(since), "exit": exitView(e)})
	}
	s.toViewers(x, map[string]any{"type": "process_exited", "sessionId": x.id, "exit": exitView(e)})
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[len(r)-n:]
	}
	return string(r)
}

func str(v any) string { s, _ := v.(string); return s }
