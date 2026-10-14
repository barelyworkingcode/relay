package contract

import (
	"testing"

	"relaye2e/harness"
)

// Regression: the fixture's transcript route answers a refusal with another status or code than
// relay, or lets the scope, a read-class credential or a request body through.
func TestOutsideTranscriptContractRefusals(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{Surface: Doors, Spec: Spec{Credentials: acmeOpsCreds()}, Body: func(r *Run) {
		const path = "/api/outside-sessions/outside:unknown/transcript"
		for _, c := range []struct {
			name, cred, method, path string
			body                     any
			scoped                   bool
			status                   int
			code                     string
		}{
			{"unknown id", "ops", "GET", path, nil, false, 404, "outside_session_not_found"},
			{"limit zero", "ops", "GET", path + "?limit=0", nil, false, 400, "invalid_limit"},
			{"cursor text", "ops", "GET", path + "?before=x", nil, false, 400, "invalid_cursor"},
			{"body", "ops", "GET", path, map[string]any{"limit": 1}, false, 400, "invalid_request"},
			{"unknown query key", "ops", "GET", path + "?after=1", nil, false, 400, "invalid_request"},
			{"chief of staff scope", "ops", "GET", path, nil, true, 403, ""},
			{"read class only", "reader", "GET", path, nil, false, 403, ""},
		} {
			opts := []harness.ReqOpts{}
			if c.scoped {
				opts = append(opts, cosScope())
			}
			resp := r.HTTP(c.cred, c.method, c.path, c.body, opts...)
			var e struct{ Error string }
			if c.code != "" {
				resp.JSON(r.T, &e)
			}
			if resp.Status != c.status || e.Error != c.code {
				r.T.Fatalf("%s: answered %d %q, want %d %q: %s", c.name, resp.Status, e.Error, c.status, c.code, resp.Body)
			}
		}
	}})
}
