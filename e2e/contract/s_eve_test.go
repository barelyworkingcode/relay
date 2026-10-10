package contract

import (
	"testing"
	"time"

	"relaye2e/harness"
)

func eveWorld() Spec {
	return Spec{
		Credentials: []harness.CredentialSpec{{Name: "ops", Classes: []string{"read", "configure"}}},
		Presence: map[string]harness.Outcome{
			"eve.enrolment.open": harness.OutcomeApprove,
			"eve.passkey.revoke": harness.OutcomeApprove,
		},
	}
}

func TestEveEnrolWindow(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Eve,
		Spec:    eveWorld(),
		Body: func(r *Run) {
			r.HTTP("ops", "GET", "/api/eve/passkey-enrolment", nil)
			eveEnrol(r)
			r.HTTP("ops", "GET", "/api/eve/passkey-enrolment", nil)
			r.HTTP("ops", "POST", "/api/eve/passkey-enrolment/consume", map[string]any{"ip": "203.0.113.9", "label": "Phone"})
			r.HTTP("ops", "GET", "/api/eve/passkey-enrolment", nil)
			r.HTTP("ops", "POST", "/api/eve/passkey-enrolment/consume", map[string]any{"ip": "203.0.113.9", "label": "Phone"})
		},
	})
}

func TestEvePasskeys(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Eve,
		Spec:    eveWorld(),
		Body: func(r *Run) {
			r.CLI("eve", "list")
			r.HTTP("ops", "PUT", "/api/eve/passkeys", map[string]any{"passkeys": []map[string]any{
				{"id": "pk-acme-0001", "label": "Laptop", "created": "2026-10-01T09:00:00Z", "last_used": "2026-10-02T09:00:00Z"},
				{"id": "pk-acme-0002", "label": "Phone", "created": "2026-10-03T09:00:00Z", "last_used": "2026-10-04T09:00:00Z"},
			}})
			r.CLI("eve", "list")
			r.HTTP("ops", "GET", "/api/eve/passkeys/revocations", nil)
			r.CLI("eve", "revoke", "--id", "pk-acme-0002")
			r.HTTP("ops", "GET", "/api/eve/passkeys/revocations", nil)
			r.CLI("eve", "list")
			r.CLI("eve", "revoke", "--id", "pk-acme-0001")
			r.HTTP("ops", "GET", "/api/eve/passkeys/revocations", nil)
		},
	})
}

func TestEveWindowCloses(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: Eve,
		Spec:    eveWorld(),
		Body: func(r *Run) {
			eveEnrol(r)
			r.HTTP("ops", "GET", "/api/eve/passkey-enrolment", nil)
			r.Target.ClockAdvance(6 * time.Minute)
			r.HTTP("ops", "GET", "/api/eve/passkey-enrolment", nil)
			r.HTTP("ops", "POST", "/api/eve/passkey-enrolment/consume", map[string]any{"ip": "203.0.113.9", "label": "Phone"})
		},
	})
}

// eveEnrol opens the enrolment window through the CLI.
func eveEnrol(r *Run) {
	r.T.Helper()
	r.CLI("eve", "enrol")
}
