package features

import (
	"strings"
	"testing"

	"relaye2e/harness"
)

// The person at the terminal sees Home|Work, never the product's internal name.
func TestEveCLIOutputSaysHomeWork(t *testing.T) {
	t.Parallel()
	i := harness.Start(t, harness.Options{
		Presence: map[string]harness.Outcome{"eve.enrolment.open": harness.OutcomeApprove},
	})

	for _, args := range [][]string{{"eve", "enrol"}, {"eve", "list"}} {
		r := i.CLI(args...)
		if r.Code != 0 {
			t.Fatalf("relay %v exited %d\nstderr: %s", args, r.Code, r.Stderr)
		}
		out := string(r.Stdout)
		if !strings.Contains(out, "Home|Work") {
			t.Errorf("relay %v output does not name Home|Work:\n%s", args, out)
		}
		if strings.Contains(out, "Eve") {
			t.Errorf("relay %v output still says Eve:\n%s", args, out)
		}
	}
}
