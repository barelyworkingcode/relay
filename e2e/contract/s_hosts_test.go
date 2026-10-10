package contract

import (
	"encoding/json"
	"testing"

	"relaye2e/harness"
)

const (
	acmeHostID   = "h_box0001"
	acmeHostProj = "p_box0001"
)

// acmeHostSpec is one probed host with one project on it. The project is also
// what the real target's host hook watches to see host_status frames.
func acmeHostSpec() Spec {
	return Spec{
		Credentials: acmeOpsCreds(),
		Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeApprove},
		Hosts: []Host{{
			ID: acmeHostID, Name: "testbox", Target: "acme@testbox", Probed: true,
			Templates: []json.RawMessage{json.RawMessage(`{"id":"shell","name":"Shell"}`)},
		}},
		Projects: []Project{{
			ID: acmeHostProj, Name: "Box app", Mode: "work", HostID: acmeHostID,
			Path: "{remote}/home/acme/app",
			Files: map[string]File{
				"main.go":      {Text: "package main\n"},
				"src":          {Dir: true},
				"src/util.go":  {Text: "package main\n// util\n"},
				"docs":         {Dir: true},
				"docs/one.txt": {Text: "one\n"},
			},
		}},
	}
}

func TestHostsReadList(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Hosts,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "GET", "/api/hosts", nil, harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "host.list", Trace: tr})
			r.HTTP("ops", "GET", "/api/hosts/"+acmeHostID, nil)
			r.HTTP("ops", "GET", "/api/hosts/h_missing", nil)
		},
	})
}

func TestHostsCreateUpdateDelete(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Hosts,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			var created struct {
				ID string `json:"id"`
			}
			trC := harness.NewTrace(r.T)
			r.HTTP("ops", "POST", "/api/hosts",
				map[string]any{"name": "Spare", "target": "acme@spare"},
				harness.ReqOpts{Trace: trC}).JSON(r.T, &created)
			r.Event(harness.EventQuery{Key: "host.create", Trace: trC})
			r.HTTP("ops", "POST", "/api/hosts", map[string]any{"target": "acme@spare"})
			r.HTTP("ops", "POST", "/api/hosts", map[string]any{"name": "Spare"})

			trU := harness.NewTrace(r.T)
			r.HTTP("ops", "PUT", "/api/hosts/"+created.ID,
				map[string]any{"name": "Spare renamed"}, harness.ReqOpts{Trace: trU})
			r.Event(harness.EventQuery{Key: "host.update", Trace: trU})
			r.HTTP("ops", "PUT", "/api/hosts/h_missing", map[string]any{"name": "Nope"})

			// A host a project uses cannot go.
			r.HTTP("ops", "DELETE", "/api/hosts/"+acmeHostID, nil)

			trD := harness.NewTrace(r.T)
			r.HTTP("ops", "DELETE", "/api/hosts/"+created.ID, nil, harness.ReqOpts{Trace: trD})
			r.Event(harness.EventQuery{Key: "host.remove", Trace: trD})
			r.HTTP("ops", "GET", "/api/hosts/"+created.ID, nil)
		},
	})
}

func TestHostsProbe(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Hosts,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "POST", "/api/hosts/"+acmeHostID+"/probe", nil, harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "host.probe", Trace: tr})
			r.HTTP("ops", "POST", "/api/hosts/h_missing/probe", nil)
		},
	})
}

func TestHostsProbeFailure(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Hosts,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			r.Target.SetHostStatus(acmeHostID, "unreachable")
			r.HTTP("ops", "POST", "/api/hosts/"+acmeHostID+"/probe", nil)
			r.HTTP("ops", "GET", "/api/hosts/"+acmeHostID, nil)
			r.Target.SetHostStatus(acmeHostID, "connected")
		},
	})
}

func TestHostsDisconnect(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Hosts,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			// A file op opens the host's agent, so the disconnect has a link to drop.
			r.HTTP("ops", "POST", "/api/projects/"+acmeHostProj+"/files/stat", map[string]any{"path": "main.go"})
			w := r.WS("/ws/files", "ops")
			w.Send(map[string]any{"type": "watch", "project_id": acmeHostProj})
			w.Expect("watch_ok", nil)

			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "POST", "/api/hosts/"+acmeHostID+"/disconnect", nil, harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "host.disconnect", Trace: tr})
			w.Expect("host_status", func(f map[string]any) bool { return f["status"] == "unreachable" })
			r.HTTP("ops", "POST", "/api/hosts/h_missing/disconnect", nil)
		},
	})
}

func TestHostsTemplates(t *testing.T) {
	t.Parallel()
	base := "/api/hosts/" + acmeHostID + "/templates"
	Check(t, Scenario{
		Surface: Hosts,
		Spec:    acmeHostSpec(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", base, nil)
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "POST", base, map[string]any{"id": "cat", "name": "Cat", "command": "/bin/cat"},
				harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "host_template.create", Trace: tr})
			r.HTTP("ops", "POST", base, map[string]any{"id": "cat", "name": "Cat", "command": "/bin/cat"})
			r.HTTP("ops", "PUT", base+"/cat", map[string]any{"name": "Cat two", "command": "/bin/cat"})
			r.HTTP("ops", "GET", base, nil)
			r.HTTP("ops", "GET", "/api/terminal/templates?project="+acmeHostProj, nil)
			r.HTTP("ops", "DELETE", base+"/cat", nil)
			r.HTTP("ops", "DELETE", base+"/cat", nil)
			r.HTTP("ops", "GET", "/api/hosts/h_missing/templates", nil)
		},
	})
}

func TestHostsPersistentSessions(t *testing.T) {
	t.Parallel()
	spec := acmeHostSpec()
	spec.Hosts[0].Persistent = []string{"relay-p_box000-shell-1"}
	base := "/api/projects/" + acmeHostProj + "/persistent-sessions"
	Check(t, Scenario{
		Surface: Hosts,
		Spec:    spec,
		Body: func(r *Run) {
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "GET", base, nil, harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "session.persistent.list", Trace: tr})
			r.HTTP("ops", "DELETE", base+"/relay-p_box000-shell-9", nil)
			r.HTTP("ops", "DELETE", base+"/not-a-relay-name", nil)
			r.HTTP("ops", "DELETE", base+"/relay-p_box000-shell-1", nil)
			r.HTTP("ops", "GET", base, nil)
			r.HTTP("ops", "GET", "/api/projects/p_missing/persistent-sessions", nil)
		},
	})
}
