package prooftest

import (
	"context"
	"path/filepath"
	"testing"
)

// Criteria: presence outcomes approve, deny and timeout; an absent op denies.
func TestPresenceOutcomes(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"approve", "deny", "timeout", "absent"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			in := startInstance(t, func(string) any {
				w := J{"schema": 1, "credentials": creds()}
				if outcome != "absent" {
					w["presence"] = J{"project.grant": "approve"}
				}
				return w
			})
			if outcome == "deny" || outcome == "timeout" {
				_, se, code := in.cli(t, "ctl", "presence", "project.grant="+outcome)
				eq(t, code, 0, "ctl presence exit: "+se)
			}
			body := J{"name": "Made", "path": filepath.Join(in.dir, "made"), "mode": "work"}
			tr := "trc-pres-" + outcome
			if outcome == "timeout" {
				ctx, cancel := context.WithCancel(context.Background())
				gone := make(chan error, 1)
				go func() {
					_, err := send(ctx, in.sockClient(), "http://relay", "POST", "/api/projects", opsTok, body, "X-Trace-Id", tr)
					gone <- err
				}()
				ans := in.waitEvent(t, "debug.presence.answer")
				eq(t, [2]any{ans["op"], ans["answer"]}, [2]any{"project.grant", "timeout"}, "presence answer")
				cancel()
				if err := <-gone; err == nil {
					t.Error("a timed-out request got an answer")
				}
				eq(t, in.waitEvent(t, "project.create")["reason"], any("presence_timeout"), "create event reason")
			} else if outcome == "approve" {
				in.api(t, "POST", "/api/projects", body, "X-Trace-Id", tr).is(t, 201)
				eq(t, in.events(t, tr, "project.create")[0]["status"], any("ok"), "create event")
			} else {
				r := in.api(t, "POST", "/api/projects", body, "X-Trace-Id", tr).is(t, 403)
				eq(t, r.obj(t)["error"], any("presence was refused"), "refusal body")
				ev := in.events(t, tr, "project.create")
				eq(t, [2]any{ev[0]["status"], ev[0]["reason"]}, [2]any{"denied", "presence_refused"}, "create event")
				ans := in.events(t, "", "debug.presence.answer")
				eq(t, [2]any{ans[0]["op"], ans[0]["answer"]}, [2]any{"project.grant", "deny"}, "presence answer")
				out, _, _ := in.cli(t, "audit", "--json", "--event", "control_decision", "--outcome", "denied")
				found := false
				for _, r := range parseRows(t, out) {
					found = found || r["method"] == "project.grant"
				}
				eq(t, found, true, "control_decision denied row")
			}
			made := 0
			if outcome == "approve" {
				made = 1
			}
			eq(t, len(in.api(t, "GET", "/api/projects", nil).is(t, 200).arr(t)), made, "projects after "+outcome)
		})
	}
}
