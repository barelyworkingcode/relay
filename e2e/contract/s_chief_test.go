package contract

import (
	"testing"

	"relaye2e/harness"
)

func TestChiefOfStaffConfig(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: ChiefOfStaff,
		Spec: Spec{
			Credentials: acmeOpsCreds(),
			Projects:    []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
		},
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/chief-of-staff/config", nil)
			tr := harness.NewTrace(r.T)
			r.HTTP("ops", "PUT", "/api/chief-of-staff/config",
				map[string]any{"projectId": "p_acme", "model": "sonnet", "dailyModelCalls": 200},
				harness.ReqOpts{Trace: tr})
			r.Event(harness.EventQuery{Key: "chief_of_staff.config.set", Trace: tr})
			r.HTTP("ops", "GET", "/api/chief-of-staff/config", nil)
			r.HTTP("ops", "PUT", "/api/chief-of-staff/config",
				map[string]any{"projectId": "p_acme", "model": "gpt", "dailyModelCalls": 200})
			r.HTTP("ops", "PUT", "/api/chief-of-staff/config",
				map[string]any{"projectId": "p_missing", "model": "sonnet", "dailyModelCalls": 200})
			r.HTTP("ops", "PUT", "/api/chief-of-staff/config",
				map[string]any{"projectId": "p_acme", "model": "sonnet", "dailyModelCalls": 0})
			r.HTTP("ops", "PUT", "/api/chief-of-staff/config", map[string]any{"projectId": "p_acme"})
			r.HTTP("reader", "PUT", "/api/chief-of-staff/config",
				map[string]any{"projectId": "p_acme", "model": "sonnet", "dailyModelCalls": 200})
			r.HTTP("ops", "DELETE", "/api/chief-of-staff/config", nil)
			r.HTTP("ops", "GET", "/api/chief-of-staff/config", nil)
		},
	})
}

func TestChiefOfStaffSessionCreate(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: ChiefOfStaff,
		Spec: Spec{
			Credentials: acmeOpsCreds(),
			Projects:    []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
			ChiefOfStaff: &struct {
				ProjectID, Model string
				DailyModelCalls  int
			}{ProjectID: "p_acme", Model: "haiku", DailyModelCalls: 50},
		},
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/chief-of-staff/config", nil)
			// Without the scope header the route is refused for every credential.
			body := map[string]any{"projectId": "p_acme", "prompt": "hello", "model": "haiku"}
			r.HTTP("ops", "POST", "/api/chief-of-staff/sessions", body)
			r.HTTP("ops", "POST", "/api/chief-of-staff/sessions",
				map[string]any{"projectId": "p_acme", "model": "haiku"}, cosScope())
			r.HTTP("ops", "POST", "/api/chief-of-staff/sessions", body, cosScope())
		},
	})
}
