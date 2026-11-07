package features

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A session whose registry status is "shell" (a background command runs
// between turns) reads Idle on its row; Adopt must give the same verdict.
func TestAdoptShellStatusReadsIdle(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app")
	now := time.Now().UTC()
	f.transcript(a.i.Home, f.user(f.start, adPrompt), f.answer(now, adModelSn, "acme parser done"))
	f.register(a.i.Home, "shell", now)

	s, frame := a.probeFrame()
	live := s.liveAt(f.dir)
	if live == nil {
		t.Fatalf("no live row for %s; snapshot %+v", f.dir, s)
	}
	if row := osRowByID(frame, live.ID); row["state"] != "idle" || row["adoptable"] != true {
		t.Fatalf("shell row = %v, want state idle and adoptable true", row)
	}
	res := adCreated(t, a.adopt(map[string]any{"outsideId": live.ID, "interrupt": false}))
	if res.ClaudeSessionID != f.sid || res.Interrupted {
		t.Fatalf("result = %+v, want claudeSessionId %s and interrupted false", res, f.sid)
	}
	f.signalAt(f.expect("signal TERM"), "TERM")
	f.gone()
}

// The refusal names the veto in words, not the code.
func TestAdoptRefusalNamesVeto(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	f := a.fake("app", "INT", "TERM")
	f.busy(a.i.Home, adModelSn)
	resp := a.adopt(map[string]any{"claudeSessionId": f.sid, "interrupt": false})
	otRefusal(t, resp, http.StatusConflict, "busy")
	if !strings.Contains(string(resp.Body), "turn is running") {
		t.Fatalf("refusal = %s, want it to say a turn is running", resp.Body)
	}
	f.alive()
}
