package contract

import (
	"testing"

	"relaye2e/harness"
)

func projectsSpec() Spec {
	return Spec{
		Credentials: acmeOpsCreds(),
		Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeApprove},
		Projects: []Project{
			{ID: "p_acme", Name: "Acme", Mode: "work"},
			{ID: "p_beta", Name: "Beta", Mode: "home"},
		},
	}
}

func TestProjectsReadList(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Projects,
		Spec:    projectsSpec(),
		Body: func(r *Run) {
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "GET", "/api/projects", nil, harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "project.list", Trace: tr})
			r.HTTP("ops", "GET", "/api/projects/p_acme", nil)
			r.HTTP("ops", "GET", "/api/projects/p_missing", nil)
		},
	})
}

func TestProjectsCreateUpdateDelete(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Projects,
		Spec:    projectsSpec(),
		Body: func(r *Run) {
			dir := newProjectDir(r, "acme3")

			trC := harness.NewTrace(r.T)
			var created struct {
				ID string `json:"id"`
			}
			r.HTTP("ops", "POST", "/api/projects",
				map[string]any{"name": "Acme Three", "path": dir, "mode": "work"},
				harness.ReqOpts{Trace: trC}).JSON(r.T, &created)
			r.Event(harness.EventQuery{Key: "project.create", Trace: trC})
			r.HTTP("ops", "GET", "/api/projects/"+created.ID, nil)

			trU := harness.NewTrace(r.T)
			r.HTTP("ops", "PUT", "/api/projects/"+created.ID,
				map[string]any{"name": "Acme Renamed"}, harness.ReqOpts{Trace: trU})
			r.Event(harness.EventQuery{Key: "project.update", Trace: trU})

			trD := harness.NewTrace(r.T)
			r.HTTP("ops", "DELETE", "/api/projects/"+created.ID, nil, harness.ReqOpts{Trace: trD})
			r.Event(harness.EventQuery{Key: "project.remove", Trace: trD})
			r.HTTP("ops", "GET", "/api/projects/"+created.ID, nil)
			r.HTTP("ops", "DELETE", "/api/projects/"+created.ID, nil)
			r.HTTP("ops", "PUT", "/api/projects/p_missing", map[string]any{"name": "Nope"})
			r.HTTP("ops", "POST", "/api/projects", map[string]any{"path": dir})
		},
	})
}

func TestProjectsDefault(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Projects,
		Spec:    projectsSpec(),
		Body: func(r *Run) {
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "PUT", "/api/default_project/work",
				map[string]any{"project_id": "p_acme"}, harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "project.default.set", Trace: tr})
			r.HTTP("ops", "PUT", "/api/default_project/home", map[string]any{"project_id": "p_beta"})
			r.HTTP("ops", "GET", "/api/projects/p_acme", nil)
			r.HTTP("ops", "PUT", "/api/default_project/work", map[string]any{"project_id": ""})
			r.HTTP("ops", "PUT", "/api/default_project/work", map[string]any{})
			r.HTTP("ops", "PUT", "/api/default_project/work", map[string]any{"project_id": "p_missing"})
		},
	})
}
