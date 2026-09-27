package main

import (
	"context"
	"flag"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestSelectJourneysKeepsTableOrder(t *testing.T) {
	all := []journey{{ID: "a1", Phase: phaseAPI}, {ID: "s1", Phase: phaseScreen}, {ID: "a2", Phase: phaseAPI}, {ID: "s2", Phase: phaseScreen}}
	ids := func(js []journey) (out []string) {
		for _, j := range js {
			out = append(out, j.ID)
		}
		return out
	}
	for p, want := range map[phase][]string{"": {"a1", "s1", "a2", "s2"}, phaseAPI: {"a1", "a2"}, phaseScreen: {"s1", "s2"}} {
		if got := ids(selectJourneys(all, p)); !slices.Equal(got, want) {
			t.Errorf("selectJourneys(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestJourneyTableInvariants(t *testing.T) {
	seen := map[string]bool{}
	screenStarted := false
	for _, j := range journeys {
		switch {
		case seen[j.ID]:
			t.Errorf("journey id %q appears twice", j.ID)
		case len(j.Areas) == 0:
			t.Errorf("%s names no areas", j.ID)
		case j.Phase != phaseAPI && j.Phase != phaseScreen:
			t.Errorf("%s has phase %q", j.ID, j.Phase)
		case j.Timeout <= 0:
			t.Errorf("%s has timeout %v", j.ID, j.Timeout)
		case j.Run == nil:
			t.Errorf("%s has no Run", j.ID)
		case j.Phase == phaseAPI && screenStarted:
			t.Errorf("api journey %s runs after a screen journey; the default run is api first", j.ID)
		}
		seen[j.ID] = true
		screenStarted = screenStarted || j.Phase == phaseScreen
	}
}

var wantAPIJourneys = []string{
	"blank-model-refused", "permission-mode-restart", "oversized-launch-audit-capped", "acme-sandbox-reach",
	"v1-conversion-refusal", "acme-tools-through-bridge", "tool-call-audited",
}

// screenStage is the contract's screen-phase order: mint, negatives, NOTRUN
// positives, setup positives, features, rotate and eve positives, fixture
// teardown, revoke.
var screenStage = map[string]int{
	"gate-credential-mint-pos": 10, "execute-credential-renewal": 11,

	"gate-credential-mint-neg": 20, "gate-credential-revoke-neg": 20, "gate-mcp-register-neg": 20, "gate-service-register-neg": 20,
	"gate-eve-enrolment-open-neg": 20, "gate-eve-passkey-revoke-neg": 20, "gate-enrolment-create-neg": 20, "gate-enrolment-sign-neg": 20,
	"gate-enrolment-update-neg": 20, "gate-enrolment-revoke-neg": 20, "gate-login-bootstrap-mint-neg": 20, "gate-login-passkey-revoke-neg": 20,
	"gate-project-grant-neg": 20, "gate-project-rotate-token-neg": 20, "gate-remote-configure-neg": 20, "gate-mcp-oauth-start-neg": 20,
	"gate-sealed-reset-neg": 20,

	"gate-enrolment-create-pos": 30, "gate-enrolment-sign-pos": 30, "gate-enrolment-update-pos": 30, "gate-enrolment-revoke-pos": 30,
	"gate-login-bootstrap-mint-pos": 30, "gate-login-passkey-revoke-pos": 30, "gate-mcp-oauth-start-pos": 30,
	"gate-remote-configure-pos": 30, "gate-sealed-reset-pos": 30,

	"gate-mcp-register-pos": 40, "gate-project-grant-pos": 41, "gate-service-register-pos": 42,

	"session-chat-lifecycle": 50, "terminal-lifecycle": 50, "model-list-and-completion": 50, "disabled-tool-refused": 50,
	"grant-narrowing-live": 50, "service-start-stop": 50, "service-restart-on-crash": 50, "stale-derived-access-edit": 50,
	"context-number-resave": 50,

	"gate-project-rotate-token-pos": 60, "gate-eve-enrolment-open-pos": 61, "gate-eve-passkey-revoke-pos": 61,
	"verify-fixtures-removed": 62, "gate-credential-revoke-pos": 70,
}

func TestPhasesHoldTheContractJourneysInOrder(t *testing.T) {
	var api []string
	for _, j := range selectJourneys(journeys, phaseAPI) {
		api = append(api, j.ID)
	}
	if !slices.Equal(api, wantAPIJourneys) {
		t.Errorf("api phase = %v, want %v", api, wantAPIJourneys)
	}
	screen := selectJourneys(journeys, phaseScreen)
	if len(screen) != len(screenStage) {
		t.Errorf("screen phase has %d journeys, want %d", len(screen), len(screenStage))
	}
	last := 0
	for _, j := range screen {
		stage, ok := screenStage[j.ID]
		switch {
		case !ok:
			t.Errorf("screen journey %s is not in the contract", j.ID)
		case stage < last:
			t.Errorf("screen journey %s (stage %d) runs after a stage-%d journey", j.ID, stage, last)
		}
		last = max(last, stage)
	}
}

func TestPhaseFlagParsing(t *testing.T) {
	cases := []struct {
		args []string
		want phase
	}{
		{nil, ""},
		{[]string{"--phase", "api"}, phaseAPI},
		{[]string{"--phase", "screen"}, phaseScreen},
		{[]string{"--post", "7"}, ""},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		pr := fs.Int("post", 0, "")
		ph := fs.String("phase", "", "")
		got, err := parseFlags(fs, c.args, pr, ph)
		if err != nil || got != c.want {
			t.Errorf("parseFlags(%v) = %q, %v; want %q", c.args, got, err, c.want)
		}
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	for _, args := range [][]string{{"--phase", "both"}, {"--phase", ""}, {"--post", "7", "--phase", "api"}} {
		os.Args = append([]string{"devboxverify"}, args...)
		if code := run(); code != 2 {
			t.Errorf("devboxverify %s exit %d, want 2", strings.Join(args, " "), code)
		}
	}
}

func TestAfterSweepLoopRule(t *testing.T) {
	swept := dialogResult{Code: 0, Outcome: "swept", Detail: "cancelled 1 dialog(s)"}
	pass := result{"j1", statePass, "ok"}
	if got := afterSweep(pass, dialogResult{Code: 0, Outcome: "none"}); got != pass {
		t.Errorf("nothing swept changed the result to %+v", got)
	}
	got := afterSweep(pass, swept)
	checkState(t, got, stateFail)
	checkDetail(t, got, "left a presence prompt open")
	for _, s := range []state{stateFail, stateBlocked, stateNotRun} {
		in := result{"j1", s, "why"}
		if got := afterSweep(in, swept); got.State != s || got.Detail == in.Detail {
			t.Errorf("swept after %s: got %+v, want the state kept and the sweep noted", s, got)
		}
	}
}

func TestAlwaysNotRunJourneys(t *testing.T) {
	ids := []string{
		"permission-mode-restart", "v1-conversion-refusal", "gate-eve-passkey-revoke-pos",
		"gate-enrolment-create-pos", "gate-enrolment-sign-pos", "gate-enrolment-update-pos", "gate-enrolment-revoke-pos",
		"gate-login-bootstrap-mint-pos", "gate-login-passkey-revoke-pos", "gate-mcp-oauth-start-pos",
		"gate-remote-configure-pos", "gate-sealed-reset-pos",
	}
	for _, id := range ids {
		i := slices.IndexFunc(journeys, func(j journey) bool { return j.ID == id })
		if i < 0 {
			t.Errorf("no journey %s", id)
			continue
		}
		got := journeys[i].Run(context.Background(), env{})
		if got.ID != id || got.State != stateNotRun || got.Detail == "" {
			t.Errorf("%s = %+v, want NOTRUN under its own id with a reason", id, got)
		}
	}
}
