package sessions

import (
	"errors"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
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

func (s *svc) cosStart(w http.ResponseWriter, r *http.Request) {
	ev := s.Events.Begin(r.Context(), "chief_of_staff.start")
	refuse := func(status int, reason, code, msg string) {
		ev.End("error", reason, errors.New(msg))
		cosErr(w, status, code, msg)
	}
	var b struct {
		ProjectID string `json:"projectId"`
		Folder    string `json:"folder"`
		Prompt    string `json:"prompt"`
		Model     string `json:"model"`
		Mode      string `json:"mode"`
	}
	if err := decode(w, r, 64<<10, &b); err != nil {
		if errors.Is(err, errTooLarge) {
			refuse(http.StatusRequestEntityTooLarge, "invalid", "body_too_large", "request body is larger than 64 KiB")
			return
		}
		refuse(http.StatusBadRequest, "invalid", "invalid_body", `body must be JSON {"projectId","folder","prompt","model","mode"}`)
		return
	}
	ev.Set("project_id", b.ProjectID)
	if b.Mode == "" {
		b.Mode = "headless"
	}
	switch {
	case b.ProjectID == "":
		refuse(http.StatusBadRequest, "invalid", "project_id_required", "project_id is required")
		return
	case b.Prompt == "":
		refuse(http.StatusBadRequest, "invalid", "prompt_required", "prompt is required")
		return
	case utf8.RuneCountInString(b.Prompt) > 8000:
		refuse(http.StatusBadRequest, "invalid", "prompt_too_long", "prompt is longer than 8000 characters")
		return
	case b.Model == "":
		refuse(http.StatusBadRequest, "invalid", "model_required", "model is required")
		return
	case b.Mode != "headless" && b.Mode != "terminal":
		refuse(http.StatusBadRequest, "invalid", "mode_invalid", "mode must be headless or terminal")
		return
	case strings.HasPrefix(b.Folder, "/") || strings.Contains(b.Folder, ".."):
		refuse(http.StatusBadRequest, "invalid", "folder_invalid", "folder must be a relative path inside the project")
		return
	}
	kind := s.kindOf(b.Model)
	t, ref := s.authorize(b.ProjectID, path.Clean(b.Folder), b.Model, kind)
	if ref != nil {
		refuse(ref.status, "denied", ref.code, ref.msg)
		return
	}
	name := b.Prompt
	if utf8.RuneCountInString(name) > 40 {
		name = string([]rune(name)[:40])
	}
	x := s.launch(t, launchBody{Folder: b.Folder, Name: name, Model: b.Model}, kind, cosOrigin, b.Mode == "headless")
	ev.Set("session_id", x.id).Set("kind", kind).End("ok", "", nil)
	if sx, ref := s.beginTurn(x.id, b.Prompt, cosOrigin); ref == nil {
		go s.runTurn(sx, b.Prompt, cosOrigin, events.TraceFrom(r.Context()))
	}
	server.WriteJSON(w, http.StatusCreated, map[string]any{"sessionId": x.id, "name": x.name, "projectId": x.projectID,
		"directory": x.directory, "mode": b.Mode, "kind": kind, "origin": cosOrigin, "at": stamp(s.now())})
}
