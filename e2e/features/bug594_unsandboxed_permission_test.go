package features

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"relaye2e/harness"
)

// These tests adopt a fake outside Claude and send it "!tool Bash". The fake
// runs the folder's PreToolUse hook (relay-sessions' own hook mode) and
// replies "hook: <decision>: <reason>". An adopted session in no project has
// no sandbox, so being headless must not turn on bypassPermissions for it; a
// session adopted in a project is bounded by its sandbox and keeps bypass.

const (
	npBypassFlag   = "--dangerously-skip-permissions"
	npBypassReason = "hook: allow: bypassPermissions mode"
	npDenyReason   = "Acme says no"
)

// npWriteHook plants the PreToolUse hook settings in dir, as g1aSetup does.
func npWriteHook(t *testing.T, dir string) {
	t.Helper()
	settings := map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{
		"matcher": "",
		"hooks":   []any{map[string]any{"type": "command", "command": "'" + harness.BundlePaths().Sessions + "' hook"}},
	}}}}
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("encoding the hook settings: %v", err)
	}
	cfg := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatalf("creating %s: %v", cfg, err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "settings.local.json"), raw, 0o600); err != nil {
		t.Fatalf("writing the hook settings: %v", err)
	}
}

// npAdoptIdle adopts an idle fake whose folder holds the hook settings: in no
// project when loose is true, else inside the rig's project. The HTTP answer
// is the adopt's signal and the fake's exit the process's.
func npAdoptIdle(t *testing.T, a *adRig, loose bool) *nsAdopted {
	t.Helper()
	var f *adFake
	if loose {
		f = nsLoose(a)
	} else {
		f = a.fake("app")
	}
	npWriteHook(t, f.dir)
	f.idle(a.i.Home, adModelSn)
	row := nsRow(t, a, f)
	res := adCreated(t, a.adopt(map[string]any{"outsideId": row["id"], "interrupt": false}))
	f.signalAt(f.expect("signal TERM"), "TERM")
	f.gone()
	g1cKillRuns(t, a.i, res.SessionID)
	return &nsAdopted{a: a, f: f, sid: res.SessionID}
}

// npNoBypass fails t when any claude start line carries a bypass flag.
func npNoBypass(t *testing.T, i *harness.Instance) {
	t.Helper()
	starts := nsStarts(i)
	if len(starts) == 0 {
		t.Fatalf("no claude start line")
	}
	for _, c := range starts {
		if adHasFlag(c.Argv, "--permission-mode", "bypassPermissions") || adHasArg(c.Argv, npBypassFlag) {
			t.Fatalf("a claude start carries a bypass flag although the session has no sandbox: %v", c.Argv)
		}
	}
}

func adHasArg(argv []string, name string) bool {
	for _, a := range argv {
		if a == name {
			return true
		}
	}
	return false
}

// npAskAndDeny sends "!tool Bash" and checks the call waits for the owner: an
// unjoined /ws sees asking, a joining one gets the replayed request, and the
// owner's denial and reason reach the agent.
func npAskAndDeny(t *testing.T, ad *nsAdopted) {
	t.Helper()
	i := ad.a.i
	unjoined := i.WebSocket("/ws", i.Credential("cos"))
	defer unjoined.Close()

	turn := g1aSendHeadless(i, ad.sid)
	// The ask is the signal. A session that runs the call unasked goes back to
	// idle after working instead; that is the failure, read from the turn's
	// reply rather than from a timeout.
	worked := false
	frames := g1aNext(t, unjoined, g1tFrameDeadline, func(f map[string]any) bool {
		if g1aAsking(ad.sid)(f) {
			return true
		}
		if g1aIs("session_state", ad.sid)(f) {
			if f["state"] == "idle" && worked {
				return true
			}
			worked = worked || f["state"] != "idle"
		}
		return false
	})
	if !g1aAsking(ad.sid)(frames[len(frames)-1]) {
		t.Fatalf("the tool call ran without asking the owner; the turn answered %q, want it to wait", g1aMessageText(t, turn))
	}

	late, req := g1aReplay(t, i, ad.sid)
	late.Send(map[string]any{"type": "permission_response", "permissionId": req["permissionId"], "approved": false, "reason": npDenyReason})
	got := g1aMessageText(t, turn)
	if got == npBypassReason {
		t.Fatalf("the hook allowed the call with the bypassPermissions reason: %q", got)
	}
	if want := "hook: deny: " + npDenyReason; got != want {
		t.Fatalf("turn answered %q, want %q", got, want)
	}
}

// AC1-3: an adopted session in no project launches without a bypass flag and
// its tool call waits for the owner, who can deny it with a reason.
func TestAdoptNoProjectAsksForApproval(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	ad := npAdoptIdle(t, a, true)
	npAskAndDeny(t, ad)
	npNoBypass(t, a.i)
}

// AC4: after drop in and hand back the resumed session still asks.
func TestAdoptNoProjectAsksAfterHandBack(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	ad := npAdoptIdle(t, a, true)
	nsSend(t, ad, "acme hello")
	tid := nsDropIn(t, a, ad.sid, ad.f.sid, 2)
	nsHandBack(t, a, ad.sid, tid)

	npAskAndDeny(t, ad)
	npNoBypass(t, a.i)
	starts := nsStarts(a.i)
	if last := starts[len(starts)-1]; !adHasFlag(last.Argv, "--resume", ad.f.sid) {
		t.Fatalf("the newest claude start = %v, want --resume %s", last.Argv, ad.f.sid)
	}
}

// AC5: an adopted session in a project still runs headless with bypass.
func TestAdoptInProjectStaysBypass(t *testing.T) {
	t.Parallel()
	a := adStart(t)
	ad := npAdoptIdle(t, a, false)

	res := g1aSendHeadless(a.i, ad.sid)
	got := g1aMessageText(t, res)
	if got != npBypassReason {
		t.Fatalf("turn answered %q, want %q", got, npBypassReason)
	}
	var start *harness.AgentCall
	for _, c := range nsStarts(a.i) {
		if c.Env["RELAY_SESSION_ID"] == ad.sid {
			c := c
			start = &c
		}
	}
	if start == nil {
		t.Fatalf("no claude start line carries RELAY_SESSION_ID %s", ad.sid)
	}
	if !adHasFlag(start.Argv, "--permission-mode", "bypassPermissions") || !adHasArg(start.Argv, npBypassFlag) {
		t.Fatalf("start argv = %v, want --permission-mode bypassPermissions and %s", start.Argv, npBypassFlag)
	}
}
