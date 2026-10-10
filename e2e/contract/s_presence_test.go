package contract

import (
	"os"
	"path/filepath"
	"testing"

	"relaye2e/harness"
)

// newProjectDir makes a directory under the target's instance dir for a
// project the scenario creates over HTTP.
func newProjectDir(r *Run, name string) string {
	dir := filepath.Join(r.Target.I.Dir, "made", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.T.Fatalf("mkdir %s: %v", dir, err)
	}
	return dir
}

func TestPresenceProjectCreate(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Presence,
		Spec: Spec{
			Credentials: acmeOpsCreds(),
			Presence:    map[string]harness.Outcome{"project.grant": harness.OutcomeDeny},
		},
		Body: func(r *Run) {
			body := map[string]any{"name": "Acme Two", "path": newProjectDir(r, "acme2"), "mode": "work"}

			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "POST", "/api/projects", body, harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "project.create", Trace: tr,
				Fields: map[string]any{"status": "denied", "reason": "presence_refused"}})
			r.Audit(harness.AuditQuery{Event: "control_decision", Outcome: "denied"})
			r.HTTP("ops", "GET", "/api/projects", nil)

			r.Target.SetPresence(map[string]harness.Outcome{"project.grant": harness.OutcomeApprove})
			r.HTTP("ops", "POST", "/api/projects", body)
			r.HTTP("ops", "GET", "/api/projects", nil)
		},
	})
}
