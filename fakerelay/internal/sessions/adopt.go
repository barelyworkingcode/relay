package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

const (
	adoptBodyMax     = 4 << 10
	adoptModelMax    = 200
	recoveryOrigin   = "relay-recovery"
	recoveryPrompt   = 300
	recoveryName     = 128
	recoveryInput    = 500
	adoptNotFoundMsg = "no live outside session matches"
	adoptEndedMsg    = "the session has ended; there is no process to adopt"
)

type adoptBody struct {
	OutsideID       string `json:"outsideId"`
	ClaudeSessionID string `json:"claudeSessionId"`
	Interrupt       bool   `json:"interrupt"`
	Model           string `json:"model"`
}

// adoptEarlyRefusals are the adopt.refuse values judged after the row resolves
// and before the project is judged.
var adoptEarlyRefusals = map[string]struct {
	status int
	msg    string
}{
	"audit_unavailable": {http.StatusServiceUnavailable, "auditing is off; no session can be adopted"},
	"unavailable":       {http.StatusBadGateway, "the session host is not available"},
	"adopt_in_progress": {http.StatusConflict, "an adopt of this conversation is already running"},
	"changed":           {http.StatusConflict, "the process or its registry entry changed"},
}

// adopt answers POST /api/sessions/adopt. The outcome comes from the row's
// adopt fixture; no process is signalled and no registry is read.
func (s *svc) adopt(w http.ResponseWriter, r *http.Request) {
	ev := s.Events.Begin(r.Context(), "session.adopt")
	var outsideID, claudeID, projectID, sessionID string
	interrupted := false
	// refuse ends the event; a parse refusal is the one answer that writes no keys.
	refuse := func(status int, code, msg string, keys bool) {
		if keys {
			ev.Set("outside_id", outsideID).Set("claude_session_id", claudeID).Set("project_id", projectID).
				Set("session_id", sessionID).Set("interrupted", interrupted)
		}
		outcome := "denied"
		if status >= 500 {
			outcome = "error"
		}
		ev.End(outcome, code, errors.New(code))
		cosErr(w, status, code, msg)
	}
	parse := func(status int, code, msg string) { refuse(status, code, msg, false) }
	late := func(status int, code, msg string) { refuse(status, code, msg, true) }

	body, status, code, msg := decodeAdoptBody(w, r)
	if code != "" {
		parse(status, code, msg)
		return
	}
	outsideID, claudeID = body.OutsideID, body.ClaudeSessionID

	row, status, code, msg := s.resolveOutside(body)
	if code != "" {
		late(status, code, msg)
		return
	}
	outsideID, claudeID = row.ID, row.ClaudeSessionID
	if row.HostID != nil {
		late(http.StatusConflict, "host_unsupported", "sessions on an SSH host cannot be adopted")
		return
	}
	if !row.Adoptable {
		reason := world.OutsideAdoptReason(row)
		late(http.StatusConflict, reason, "the session cannot be adopted: "+reason)
		return
	}
	adopt := world.OutsideAdopt{}
	if row.Adopt != nil {
		adopt = *row.Adopt
	}
	if e, early := adoptEarlyRefusals[adopt.Refuse]; early {
		late(e.status, adopt.Refuse, e.msg)
		return
	}

	var project world.Project
	var projectOK bool
	if row.ProjectID != nil {
		s.State.Read(func(m *state.Model) { project, projectOK = lookupProject(m, *row.ProjectID) })
	}
	if !projectOK || project.HostID != "" || project.Kind == "remote" {
		late(http.StatusConflict, "no_project", "the session is not in a registered local project")
		return
	}
	projectID = project.ID

	if s.holdsConversation(row.ClaudeSessionID) {
		late(http.StatusConflict, "managed", "relay already holds this conversation")
		return
	}
	if adopt.Busy != "" && !body.Interrupt {
		late(http.StatusConflict, "busy", fmt.Sprintf("the session is not idle (%s); ask to interrupt to take it over anyway", adopt.Busy))
		return
	}
	model := body.Model
	if model == "" {
		model = adopt.ModelFamily
	}
	if model == "" {
		late(http.StatusConflict, "model_unknown", "the model of the session is unknown; send model")
		return
	}
	t, ref := s.authorize(project.ID, row.Directory, model, "claude")
	if ref != nil {
		late(ref.status, cosCodeFor(ref.code), ref.msg)
		return
	}
	switch adopt.Refuse {
	case "stop_failed":
		late(http.StatusBadGateway, "stop_failed", "the process did not exit after SIGKILL; nothing was launched")
		return
	case "launch_failed":
		s.Outside.EndOutside(r.Context(), row.ID, s.now())
		late(http.StatusBadGateway, "launch_failed", "the process ended but relay could not start the session; the conversation is intact and can be resumed")
		return
	}

	// Removing the row first makes a second adopt of it a 404, not a second session.
	if !s.Outside.RemoveOutside(r.Context(), row.ID) {
		late(http.StatusNotFound, "outside_session_not_found", adoptNotFoundMsg)
		return
	}
	x := s.launch(t, launchBody{ProjectID: project.ID, Directory: row.Directory, Name: row.Name, Model: model,
		Settings: map[string]any{"headless": true, "agent": true}}, "claude", "", true)
	s.mu.Lock()
	x.claudeID = row.ClaudeSessionID
	s.mu.Unlock()
	sessionID = x.id
	interrupted = adopt.Busy != ""

	var recovery *session
	recoveryText := recoveryTextFor(row.Transcript)
	if interrupted {
		if rx, ref := s.beginTurn(x.id, recoveryText, recoveryOrigin); ref == nil {
			recovery = rx
		}
	}
	ev.Set("outside_id", outsideID).Set("claude_session_id", claudeID).Set("project_id", projectID).
		Set("session_id", sessionID).Set("interrupted", interrupted)
	ev.End("ok", "", nil)
	created := map[string]any{"sessionId": x.id, "claudeSessionId": row.ClaudeSessionID, "interrupted": interrupted, "recoverySent": recovery != nil}
	writeCreated(w, created)
	if recovery != nil {
		go s.runTurn(recovery, recoveryText, recoveryOrigin, events.TraceFrom(r.Context()))
	}
}

func writeCreated(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(b)
}

func cosCodeFor(code string) string {
	if renamed, ok := cosCodes[code]; ok {
		return renamed
	}
	return code
}

// decodeAdoptBody reads one JSON value of at most 4 KiB and judges the
// selectors. code is "" when the body is good.
func decodeAdoptBody(w http.ResponseWriter, r *http.Request) (b adoptBody, status int, code, msg string) {
	invalid := func(m string) (adoptBody, int, string, string) {
		return b, http.StatusBadRequest, "invalid_body", m
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, adoptBodyMax))
	dec.DisallowUnknownFields()
	err := dec.Decode(&b)
	if err == nil {
		if _, extra := dec.Token(); extra != io.EOF {
			err = errors.New("trailing data")
			var tooBig *http.MaxBytesError
			if errors.As(extra, &tooBig) {
				err = extra
			}
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return b, http.StatusRequestEntityTooLarge, "body_too_large", "request body is larger than 4 KiB"
		}
		return invalid(`body must be JSON {"outsideId" or "claudeSessionId", "interrupt", "model"}`)
	}
	switch {
	case (b.OutsideID == "") == (b.ClaudeSessionID == ""):
		return invalid("send outsideId or claudeSessionId, not both and not neither")
	case b.OutsideID != "" && !world.IsOutsideID(b.OutsideID):
		return invalid("outsideId is not an outside session id")
	case b.ClaudeSessionID != "" && !world.IsConversationUUID(b.ClaudeSessionID):
		return invalid("claudeSessionId is not a UUID")
	case utf8.RuneCountInString(b.Model) > adoptModelMax || !utf8.ValidString(b.Model) || strings.ContainsFunc(b.Model, unicode.IsControl):
		return invalid("model is not a model name")
	}
	return b, 0, "", ""
}

// resolveOutside finds the live row the request names. code is "" when it
// found one.
func (s *svc) resolveOutside(b adoptBody) (row world.OutsideFixture, status int, code, msg string) {
	notFound := func() (world.OutsideFixture, int, string, string) {
		return row, http.StatusNotFound, "outside_session_not_found", adoptNotFoundMsg
	}
	ended := func() (world.OutsideFixture, int, string, string) {
		return row, http.StatusConflict, "ended", adoptEndedMsg
	}
	var live []world.OutsideFixture
	endedMatch := false
	s.State.Read(func(m *state.Model) {
		for _, o := range m.OutsideSessions {
			if b.OutsideID != "" {
				if o.ID != b.OutsideID {
					continue
				}
				if o.State == "ended" {
					endedMatch = true
				} else {
					live = append(live, o)
				}
				continue
			}
			if o.ClaudeSessionID != b.ClaudeSessionID {
				continue
			}
			if o.State == "ended" {
				endedMatch = true
			} else if o.HostID == nil {
				live = append(live, o)
			}
		}
	})
	switch {
	case len(live) == 1:
		return live[0], 0, "", ""
	case len(live) > 1:
		return row, http.StatusConflict, "ambiguous", "more than one live process holds this conversation"
	case endedMatch:
		return ended()
	}
	return notFound()
}

// holdsConversation: a session made by an adopt carries the conversation id.
func (s *svc) holdsConversation(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.sessions {
		if x.claudeID == id {
			return true
		}
	}
	return false
}

// recoveryTextFor is the message relay sends into a session it took over in
// the middle of a turn, filled from the fixture's transcript. The call id is
// the transcript route's entry id.
func recoveryTextFor(entries []world.OutsideTranscriptEntry) string {
	prompt := ""
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role == "user" {
			prompt = entries[i].Text
			break
		}
	}
	open := -1
scan:
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch {
		case e.Role == "tool" && e.Kind == "result":
			break scan
		case e.Role == "tool" && e.Kind == "call":
			open = i
			break scan
		}
	}
	var b strings.Builder
	b.WriteString("Relay took over this session from a terminal while a turn was running, and interrupted that turn.\n")
	fmt.Fprintf(&b, "You were working on: \"%s\"\n", oneLine(prompt, recoveryPrompt))
	if open < 0 {
		b.WriteString("No tool call was open.\n")
		b.WriteString("Check the state of anything this turn touched before you continue.")
		return b.String()
	}
	call := entries[open]
	input := "null"
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(call.Text)) == nil && compact.Len() > 0 {
		input = compact.String()
	}
	name := ""
	if call.Name != nil {
		name = *call.Name
	}
	fmt.Fprintf(&b, "The open tool call was %s (id %s) with input %s.\n",
		oneLine(name, recoveryName), oneLine("e"+strconv.Itoa(open), recoveryName), oneLine(input, recoveryInput))
	b.WriteString("That call was cut off before its result was recorded. Check whether it took effect, and check the state of anything it touched, before you continue.")
	return b.String()
}

// oneLine joins s's words with single spaces and keeps at most max
// characters, the last being "…" when it cut.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}
