package contract

import (
	"testing"

	"relaye2e/harness"
)

func eventsWorld() Spec {
	return Spec{
		Credentials: []harness.CredentialSpec{{Name: "ops", Classes: []string{"read"}}},
		Projects:    []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
	}
}

func TestEventsLogsAfterRequest(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Events,
		Spec:    eventsWorld(),
		Body: func(r *Run) {
			trace := harness.NewTrace(r.T)
			r.HTTP("ops", "GET", "/api/projects", nil, harness.ReqOpts{Trace: trace})
			r.CLI("logs", "--json", "--trace", trace, "--event", "project.list")
			r.CLI("logs", "--json", "--trace", trace, "--event", "project.get")
		},
	})
}

func TestEventsFollowReturnsOnKey(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Events,
		Spec:    eventsWorld(),
		Body: func(r *Run) {
			trace := harness.NewTrace(r.T)
			r.HTTP("ops", "GET", "/api/projects/p_acme", nil, harness.ReqOpts{Trace: trace})
			r.CLI("logs", "--json", "--trace", trace, "--follow", "--event", "project.get", "--timeout", "30s")
			r.Event(harness.EventQuery{Key: "project.get", Trace: trace})
		},
	})
}
