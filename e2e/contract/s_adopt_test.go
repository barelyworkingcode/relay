package contract

import (
	"strings"
	"testing"
)

// Regression: fakerelay answers the adopt door's body checks, unknown-row refusals or scope refusal
// differently from relay.
func TestAdoptContractRefusals(t *testing.T) {
	t.Parallel()
	Check(t, Scenario{Surface: Sessions, Spec: sessionsWorld(), Body: func(r *Run) {
		const (
			path    = "/api/sessions/adopt"
			outside = "outside:0123456789abcdef0123456789abcdef"
			uuid    = "0b6e1f2a-3c4d-4e5f-8a9b-0c1d2e3f4a5b"
		)
		post := func(body any) { r.HTTP("ops", "POST", path, body) }

		post([]byte(`{`))
		post([]byte(`{"outsideId":"` + outside + `","bogus":1}`))
		post([]byte(`{"outsideId":5}`))
		post([]byte(`{"outsideId":"` + outside + `"} {}`))
		post([]byte(`{"outsideId":"` + strings.Repeat("a", 5000) + `"}`))
		post(map[string]any{"outsideId": outside, "claudeSessionId": uuid})
		post(map[string]any{"interrupt": true})
		post(map[string]any{"outsideId": "outside:acme"})
		post(map[string]any{"claudeSessionId": "stem-id"})
		post(map[string]any{"outsideId": outside, "model": strings.Repeat("m", 201)})
		post(map[string]any{"outsideId": outside, "model": "a\x01b"})

		post(map[string]any{"outsideId": outside})
		post(map[string]any{"claudeSessionId": uuid})

		r.HTTP("ops", "POST", path, map[string]any{"outsideId": outside}, cosScope())
	}})
}
