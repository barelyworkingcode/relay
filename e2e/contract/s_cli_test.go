package contract

import (
	"testing"

	"relaye2e/harness"
)

func cliWorld() Spec {
	return Spec{
		Credentials: []harness.CredentialSpec{{Name: "ops", Classes: []string{"read"}}},
		Projects: []Project{
			{ID: "p_acme", Name: "Acme", Mode: "work"},
			{ID: "p_zenith", Name: "Zenith", Mode: "home"},
		},
	}
}

func TestCLIGrantWithRecords(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: CLI,
		Spec:    cliWorld(),
		Body: func(r *Run) {
			r.CLI("grant", "--json")
			r.CLI("grant", "--json", "--project", "Acme")
			r.CLI("grant", "--json", "--project", "p_missing")
		},
	})
}

func TestCLIGrantWithoutRecords(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: CLI,
		Spec:    Spec{Credentials: []harness.CredentialSpec{{Name: "ops", Classes: []string{"read"}}}},
		Body: func(r *Run) {
			r.CLI("grant", "--json")
		},
	})
}

func TestCLINoServer(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{
		Surface: CLI,
		Spec:    cliWorld(),
		Body: func(r *Run) {
			r.Target.I.Stop()
			r.CLI("mcp", "list")
			r.CLI("grant", "--json")
		},
	})
}
