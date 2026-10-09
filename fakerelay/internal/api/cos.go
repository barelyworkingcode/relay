package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

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
