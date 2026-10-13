package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/barelyworkingcode/relay/fakerelay/internal/events"
	"github.com/barelyworkingcode/relay/fakerelay/internal/server"
	"github.com/barelyworkingcode/relay/fakerelay/internal/state"
	"github.com/barelyworkingcode/relay/fakerelay/internal/world"
)

func (a *api) cosConfigRoutes(r server.Registrar) {
	r.Route(server.ClassRead, "GET /api/chief-of-staff/config", a.getCoS)
	r.Route(server.ClassConfigure, "PUT /api/chief-of-staff/config", a.putCoS)
	r.Route(server.ClassConfigure, "DELETE /api/chief-of-staff/config", a.deleteCoS)
}

// cosView is the camelCase HTTP view; the world file is snake_case.
func cosView(c *world.ChiefOfStaff) map[string]any {
	if c == nil {
		return map[string]any{"configured": false}
	}
	return map[string]any{"configured": true, "projectId": c.ProjectID, "model": c.Model, "dailyModelCalls": c.DailyModelCalls}
}

func cosError(w http.ResponseWriter, status int, code, msg string) {
	server.WriteJSON(w, status, map[string]string{"error": code, "message": msg})
}

func (a *api) getCoS(w http.ResponseWriter, r *http.Request) {
	var v map[string]any
	a.State.Read(func(m *state.Model) { v = cosView(m.ChiefOfStaff) })
	a.Events.Begin(r.Context(), "chief_of_staff.config.get").End("ok", "", nil)
	ok(w, v)
}

func (a *api) putCoS(w http.ResponseWriter, r *http.Request) {
	ev := a.Events.Begin(r.Context(), "chief_of_staff.config.set")
	refuse := func(status int, reason, code, msg string) {
		ev.End("error", reason, errors.New(msg))
		cosError(w, status, code, msg)
	}
	var b struct {
		ProjectID       *string      `json:"projectId"`
		Model           *string      `json:"model"`
		DailyModelCalls *json.Number `json:"dailyModelCalls"`
	}
	if err := decode(w, r, 4<<10, &b, true); err != nil {
		if errors.Is(err, errTooLarge) {
			refuse(http.StatusRequestEntityTooLarge, "invalid", "body_too_large", "request body is larger than 4 KiB")
			return
		}
		refuse(http.StatusBadRequest, "invalid", "invalid_body", `body must be JSON {"projectId","model","dailyModelCalls"}`)
		return
	}
	switch {
	case b.ProjectID == nil || b.Model == nil || b.DailyModelCalls == nil:
		refuse(http.StatusBadRequest, "invalid", "invalid_body", "projectId, model and dailyModelCalls are all required")
		return
	case *b.ProjectID == "":
		refuse(http.StatusBadRequest, "invalid", "project_id_required", "project_id is required")
		return
	case *b.Model != "haiku" && *b.Model != "sonnet" && *b.Model != "opus":
		refuse(http.StatusBadRequest, "invalid", "model_invalid", "model must be haiku, sonnet or opus")
		return
	}
	calls, err := strconv.Atoi(b.DailyModelCalls.String())
	if err != nil || calls < 1 || calls > 10000 {
		refuse(http.StatusBadRequest, "invalid", "daily_model_calls_invalid", "dailyModelCalls must be a whole number from 1 to 10000")
		return
	}
	ev.Set("project_id", *b.ProjectID)
	var view map[string]any
	var pErr [3]string
	_ = a.State.Write(func(m *state.Model) error {
		p, found := findProject(m, *b.ProjectID)
		switch {
		case !found:
			pErr = [3]string{"project_not_found", `no project with id "` + *b.ProjectID + `"`, "not_found"}
		case p.Kind == "remote":
			pErr = [3]string{"project_unsuitable", p.Name + ": an access profile cannot host the Chief of Staff", "invalid"}
		default:
			m.ChiefOfStaff = &world.ChiefOfStaff{ProjectID: p.ID, Model: *b.Model, DailyModelCalls: calls}
			view = cosView(m.ChiefOfStaff)
			return nil
		}
		return errors.New(pErr[1])
	})
	if view == nil {
		refuse(http.StatusBadRequest, pErr[2], pErr[0], pErr[1])
		return
	}
	ev.End("ok", "", nil)
	ok(w, view)
}

func (a *api) deleteCoS(w http.ResponseWriter, r *http.Request) {
	_ = a.State.Write(func(m *state.Model) error { m.ChiefOfStaff = nil; return nil })
	a.Events.Begin(r.Context(), "chief_of_staff.config.clear").End("ok", "", nil)
	ok(w, cosView(nil))
}

func (a *api) cosAgentRoutes(r server.Registrar) {
	r.Route(server.ClassChiefOfStaff, "GET /api/chief-of-staff/projects/{projectId}/agents", a.cosListAgents)
	r.Route(server.ClassChiefOfStaff, "POST /api/chief-of-staff/projects/{projectId}/agents/preview", a.cosPreviewAgent)
}

const cosPreviewBodyMsg = `request body must be JSON {"name","kind","model","command"}`

var errCosProject = &agentErr{status: http.StatusForbidden, code: "project_not_available", msg: "project is not available for a session launch"}

// cosProject reads the project fresh; a missing or remote one is refused the
// way a start refuses it.
func (a *api) cosProject(id string) (world.Project, *world.Host, string, *agentErr) {
	var p world.Project
	var host *world.Host
	var found bool
	a.State.Read(func(m *state.Model) {
		p, found = findProject(m, id)
		if found && p.HostID != "" {
			if h, hok := findHost(m, p.HostID); hok {
				c := *h
				host = &c
			}
		}
	})
	if !found || p.Kind == "remote" {
		return p, nil, "", errCosProject
	}
	machine := localMachine
	switch {
	case host != nil:
		machine = host.Name
	case p.HostID != "":
		machine = p.HostID
	}
	return p, host, machine, nil
}

// cosEnd ends the event as denied for the 403 and as the error's own class
// otherwise.
func cosEnd(ev *events.Event, e *agentErr, refused string) {
	if e.status == http.StatusForbidden {
		ev.End("denied", "not_granted", errors.New(refused))
		return
	}
	ev.End("error", e.reason(), e)
}

func (a *api) cosListAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("projectId")
	ev := a.Events.Begin(r.Context(), "chief_of_staff.agents.list").Set("project_id", id)
	p, _, machine, e := a.cosProject(id)
	if e != nil {
		cosEnd(ev, e, "chief of staff agent list refused")
		e.write(w)
		return
	}
	entries := []any{}
	for _, ag := range p.Agents {
		row := map[string]any{"type": "agent", "name": ag.Name, "kind": ag.Kind}
		if ag.Model != "" {
			row["model"] = ag.Model
		}
		if ag.Command != "" {
			row["command"] = ag.Command
		}
		entries = append(entries, row)
	}
	for _, t := range world.ChatTemplatesOf(p) {
		entries = append(entries, map[string]any{"type": "chat", "name": t.Name, "model": t.Model})
	}
	ev.Set("count", len(entries)).End("ok", "", nil)
	ok(w, map[string]any{"projectId": id, "projectName": p.Name, "machine": machine, "entries": entries})
}

// cosPreviewAgent resolves a proposed entry exactly as the gated add would,
// and writes nothing and raises no prompt.
func (a *api) cosPreviewAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("projectId")
	ev := a.Events.Begin(r.Context(), "chief_of_staff.agents.preview").Set("project_id", id)
	refuse := func(e *agentErr) {
		cosEnd(ev, e, "chief of staff agent preview refused")
		e.write(w)
	}
	body, err := readBody(w, r, maxAgentBody)
	var in world.AgentEntry
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		var b struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Model   string `json:"model"`
			Command string `json:"command"`
		}
		if err = dec.Decode(&b); err == nil {
			if _, terr := dec.Token(); terr != io.EOF {
				err = errors.New("trailing data")
			}
		}
		in = world.AgentEntry{Name: b.Name, Kind: b.Kind, Model: b.Model, Command: b.Command}
	}
	if err != nil {
		refuse(&agentErr{status: http.StatusBadRequest, code: "invalid_body", msg: cosPreviewBodyMsg})
		return
	}
	p, host, machine, perr := a.cosProject(id)
	if perr != nil {
		refuse(perr)
		return
	}
	if rule := world.ValidateAgentShape(in); rule != nil {
		refuse(ruleToErr(rule))
		return
	}
	switch {
	case in.Kind == "pi" && p.HostID != "":
		refuse(&agentErr{status: http.StatusBadRequest, code: "kind_not_available_on_host", msg: "pi is not available on a host project"})
		return
	case len(p.Agents) >= world.MaxAgentEntries:
		refuse(tooMany())
		return
	}
	if rule := world.AgentNameClash(p, in.Name, ""); rule != nil {
		refuse(ruleToErr(rule))
		return
	}
	if in.Command != "" {
		abs, e := a.lookupAgentCommand(p, host, machine, in.Command)
		if e != nil {
			refuse(e)
			return
		}
		in.Command = abs
	}
	ev.Set("machine", machine).End("ok", "", nil)
	ok(w, map[string]any{"projectId": id, "projectName": p.Name, "machine": machine, "entry": in})
}
