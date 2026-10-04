package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func settingsBase() settingsRun {
	return settingsRun{
		Up:         svcView{State: "running", PIDs: []int{101}},
		Before:     cardView{Button: "Stop", Status: "pid 101 · up 3s", PID: 101},
		Stopped:    svcView{State: "-"},
		Down:       true,
		AfterStop:  cardView{Button: "Start", Status: "stopped"},
		AfterStart: cardView{Button: "Stop", Status: "pid 202 · up 1s", PID: 202},
		Running:    svcView{State: "running", PIDs: []int{202}},
		RunningOK:  true,
	}
}

func TestClassifySettingsWindow(t *testing.T) {
	driver := fmt.Errorf("press Stop: %w", errAXDriver)
	checkMuts(t, settingsBase, classifySettingsWindow, []mutCase[settingsRun]{
		{"stopped and started from the window", func(*settingsRun) {}, statePass},
		{"restore failed after a pass", func(r *settingsRun) { r.RestoreErr = errors.New("api stop: refused") }, statePass},
		{"no accessibility trust", func(r *settingsRun) { r.Untrusted = true }, stateBlocked},
		{"no accessibility trust beats other fields", func(r *settingsRun) { r.Untrusted = true; r.OpenErr = errors.New("x") }, stateBlocked},
		{"fixture not set up", func(r *settingsRun) { r.Setup = blocked(settingsWindowID, "no run credential") }, stateBlocked},
		{"driver error opening", func(r *settingsRun) { r.OpenErr = fmt.Errorf("tray: %w", errAXDriver) }, stateBlocked},
		{"driver error on baseline card", func(r *settingsRun) { r.BeforeErr = driver }, stateBlocked},
		{"driver error on press", func(r *settingsRun) { r.StopErr = driver }, stateBlocked},
		{"driver error after stop", func(r *settingsRun) { r.AfterStopErr = driver }, stateBlocked},
		{"driver error on start", func(r *settingsRun) { r.StartErr = driver }, stateBlocked},
		{"driver error after start", func(r *settingsRun) { r.AfterStartErr = driver }, stateBlocked},
		{"service list unreadable", func(r *settingsRun) { r.Stopped = svcView{Err: errors.New("pgrep failed")} }, stateBlocked},
		{"service list unreadable at the end", func(r *settingsRun) { r.Running = svcView{Err: errors.New("pgrep failed")}; r.RunningOK = false }, stateBlocked},
		{"tray item missing", func(r *settingsRun) { r.OpenErr = errors.New(`tray: no item with help "Relay"`) }, stateFail},
		{"no settings window", func(r *settingsRun) { r.OpenErr = errors.New(`no "Relay Settings" window`) }, stateFail},
		{"baseline card unreadable", func(r *settingsRun) { r.BeforeErr = errors.New("no card") }, stateFail},
		{"baseline shows Start", func(r *settingsRun) { r.Before.Button = "Start" }, stateFail},
		{"baseline pid is not the fixture's", func(r *settingsRun) { r.Before.PID = 999 }, stateFail},
		{"press Stop failed", func(r *settingsRun) { r.StopErr = errors.New("no Stop button") }, stateFail},
		{"service still running after Stop", func(r *settingsRun) { r.Down = false; r.Stopped = svcView{State: "running", PIDs: []int{101}} }, stateFail},
		{"window not stopped after Stop", func(r *settingsRun) { r.AfterStop.Status = "pid 101 · up 9s" }, stateFail},
		{"window button not Start after Stop", func(r *settingsRun) { r.AfterStop.Button = "Stop" }, stateFail},
		{"press Start failed", func(r *settingsRun) { r.StartErr = errors.New("no Start button") }, stateFail},
		{"card unreadable after Start", func(r *settingsRun) { r.AfterStartErr = errors.New("no card") }, stateFail},
		{"no pid after Start", func(r *settingsRun) { r.AfterStart = cardView{Button: "Start", Status: "stopped"} }, stateFail},
		{"old pid after Start", func(r *settingsRun) { r.AfterStart.PID = 101 }, stateFail},
		{"service list lacks the new pid", func(r *settingsRun) { r.RunningOK = false; r.Running = svcView{State: "-"} }, stateFail},
	}, map[string]string{
		"stopped and started from the window":       "pid 101 then 202",
		"restore failed after a pass":               "restore: api stop: refused",
		"no accessibility trust":                    "no Accessibility trust for this process",
		"fixture not set up":                        "no run credential",
		"driver error on press":                     "accessibility driver",
		"service list unreadable":                   "pgrep failed",
		"tray item missing":                         "tray:",
		"baseline shows Start":                      "before the Stop",
		"press Stop failed":                         "service card:",
		"service still running after Stop":          "STATE running",
		"window not stopped after Stop":             "after the Stop, want stopped and Start",
		"no pid after Start":                        "no new pid 10 s after its Start",
		"service list lacks the new pid":            "does not show pid 202 running",
		"card unreadable after Start":               "service card:",
		"driver error opening":                      "accessibility driver",
		"service list unreadable at the end":        "pgrep failed",
		"baseline pid is not the fixture's":         "before the Stop",
		"window button not Start after Stop":        "after the Stop, want stopped and Start",
		"old pid after Start":                       "no new pid",
		"no settings window":                        "Relay Settings",
		"press Start failed":                        "service card:",
		"baseline card unreadable":                  "service card:",
		"no accessibility trust beats other fields": "no Accessibility trust",
		"driver error on baseline card":             "accessibility driver",
		"driver error after stop":                   "accessibility driver",
		"driver error on start":                     "accessibility driver",
		"driver error after start":                  "accessibility driver",
	})
}

func TestParseStatusLine(t *testing.T) {
	for _, c := range []struct {
		in  string
		pid int
		ok  bool
	}{
		{"pid 4821 · up 3s", 4821, true},
		{"pid 7", 7, true},
		{"pid 12 · up 2 min", 12, true},
		{"stopped", 0, false},
		{"running", 0, false},
		{"", 0, false},
		{"pid ", 0, false},
		{"pid abc · up 1s", 0, false},
		{"pid 12 · up ", 0, false},
		{"Apid 12", 0, false},
		{"pid 12 trailing", 0, false},
	} {
		pid, ok := parseStatusLine(c.in)
		if pid != c.pid || ok != c.ok {
			t.Errorf("parseStatusLine(%q) = %d, %v; want %d, %v", c.in, pid, ok, c.pid, c.ok)
		}
	}
}

type pageCard struct {
	name, button, status string
	noSwitch             bool
}

func txt(s string) *axNode { return &axNode{Role: "AXStaticText", Label: s} }
func btn(s string) *axNode { return &axNode{Role: "AXButton", Label: s} }

// cardNodes lays one card out the way WebKit flattens it: siblings in
// document order, no container.
func cardNodes(c pageCard) []*axNode {
	out := []*axNode{txt(c.name), txt("acme command"), txt(c.status), btn("Edit"), btn(c.button)}
	if !c.noSwitch {
		out = append(out, &axNode{Role: "AXCheckBox", Label: "Show in menu"})
	}
	return out
}

func buildPage(cards ...pageCard) []*axNode {
	page := []*axNode{{Role: "AXHeading", Label: "Services"}}
	for _, c := range cards {
		page = append(page, cardNodes(c)...)
	}
	return page
}

func buttonOf(page []*axNode, name string) *axNode {
	for i, n := range page {
		if n.Role == "AXStaticText" && n.Label == name {
			for _, m := range page[i+1:] {
				if m.Role == "AXButton" && (m.Label == "Start" || m.Label == "Stop") {
					return m
				}
			}
		}
	}
	return nil
}

func TestFindServiceCard(t *testing.T) {
	const fx = "devboxverify crash 0a1b2c3d"
	a := pageCard{"Acme alpha", "Stop", "pid 11 · up 1m", false}
	b := pageCard{"Acme beta", "Stop", "pid 22 · up 1m", false}
	cases := []struct {
		name    string
		page    []*axNode
		want    cardView
		wantBtn *axNode // nil: must refuse
	}{}
	mid := buildPage(a, pageCard{fx, "Stop", "pid 33 · up 2s", false}, b)
	cases = append(cases, struct {
		name    string
		page    []*axNode
		want    cardView
		wantBtn *axNode
	}{"fixture in the middle", mid, cardView{"Stop", "pid 33 · up 2s", 33}, buttonOf(mid, fx)})

	stopped := buildPage(a, pageCard{fx, "Start", "stopped", false}, b)
	cases = append(cases, struct {
		name    string
		page    []*axNode
		want    cardView
		wantBtn *axNode
	}{"running neighbours while the fixture is stopped", stopped, cardView{"Start", "stopped", 0}, buttonOf(stopped, fx)})

	last := buildPage(a, pageCard{fx, "Stop", "pid 5", false})
	cases = append(cases, struct {
		name    string
		page    []*axNode
		want    cardView
		wantBtn *axNode
	}{"fixture is the last card", last, cardView{"Stop", "pid 5", 5}, buttonOf(last, fx)})

	refuse := map[string][]*axNode{
		"name appears twice": buildPage(a, pageCard{fx, "Stop", "pid 33", false}, pageCard{fx, "Start", "stopped", false}),
		"name absent":        buildPage(a, b),
		"fixture has no switch, span crosses the next card": buildPage(a, pageCard{fx, "Start", "stopped", true}, b),
		"fixture is last and has no switch":                 buildPage(a, pageCard{fx, "Start", "stopped", true}),
	}
	// The toggle sits before the name: the span holds no Start or Stop.
	before := []*axNode{btn("Stop"), txt(fx), txt("pid 33"), btn("Edit"), {Role: "AXCheckBox", Label: "Show in menu"}}
	refuse["button before the name"] = before
	// The status line is missing.
	refuse["no status line"] = []*axNode{txt(fx), btn("Edit"), btn("Stop"), {Role: "AXCheckBox", Label: "Show in menu"}}
	// Name text under a different role does not count.
	refuse["name is not static text"] = []*axNode{{Role: "AXHeading", Label: fx}, txt("pid 33"), btn("Edit"), btn("Stop"), {Role: "AXCheckBox", Label: "Show in menu"}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, node, err := findServiceCard(c.page, fx)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("view = %+v, want %+v", got, c.want)
			}
			if node != c.wantBtn {
				t.Errorf("returned the wrong button node (label %q)", node.Label)
			}
		})
	}
	for name, page := range refuse {
		t.Run("refuses: "+name, func(t *testing.T) {
			_, node, err := findServiceCard(page, fx)
			if err == nil || node != nil {
				t.Fatalf("want an error and no button, got node %v, err %v", node, err)
			}
		})
	}
}

// fakeSteps scripts each seam call and records what was called.
type fakeSteps struct {
	untrusted  bool
	fixtureErr bool
	openErr    error
	cards      []cardView
	cardErrAt  int // 1-based call that fails; 0 never
	pressErr   map[string]error
	serviceOK  []bool
	cardCalls  int
	svcCalls   int

	startFixture, restore int
	restoreCtxErr         error
	pressed               []string
}

func (f *fakeSteps) Trusted() bool { return !f.untrusted }
func (f *fakeSteps) StartFixture(context.Context) (svcView, result, bool) {
	f.startFixture++
	if f.fixtureErr {
		return svcView{}, blocked(settingsWindowID, "fixture: crash service not running 5 s after the API start"), false
	}
	return svcView{State: "running", PIDs: []int{101}}, result{}, true
}
func (f *fakeSteps) OpenServices(context.Context) error { return f.openErr }
func (f *fakeSteps) Card(_ context.Context, _ time.Duration, _ func(cardView) bool) (cardView, error) {
	f.cardCalls++
	if f.cardErrAt == f.cardCalls {
		return cardView{}, errors.New("no card")
	}
	return f.cards[f.cardCalls-1], nil
}
func (f *fakeSteps) Press(_ context.Context, label string) error {
	f.pressed = append(f.pressed, label)
	return f.pressErr[label]
}
func (f *fakeSteps) Service(context.Context, time.Duration, func(svcView) bool) (svcView, bool) {
	f.svcCalls++
	ok := f.serviceOK[f.svcCalls-1]
	if ok && f.svcCalls == 1 {
		return svcView{State: "-"}, true
	}
	if ok {
		return svcView{State: "running", PIDs: []int{202}}, true
	}
	return svcView{State: "running", PIDs: []int{101}}, false
}
func (f *fakeSteps) Restore(ctx context.Context) error {
	f.restore++
	f.restoreCtxErr = ctx.Err()
	return nil
}

func happySteps() *fakeSteps {
	return &fakeSteps{
		cards: []cardView{
			{"Stop", "pid 101 · up 3s", 101},
			{"Start", "stopped", 0},
			{"Stop", "pid 202 · up 1s", 202},
		},
		serviceOK: []bool{true, true},
	}
}

func TestRunSettingsWindowRestoresOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		mut  func(*fakeSteps)
		want state
	}{
		{"every step passes", func(*fakeSteps) {}, statePass},
		{"fixture does not start", func(f *fakeSteps) { f.fixtureErr = true }, stateBlocked},
		{"window does not open", func(f *fakeSteps) { f.openErr = errors.New(`no "Relay Settings" window`) }, stateFail},
		{"baseline card unreadable", func(f *fakeSteps) { f.cardErrAt = 1 }, stateFail},
		{"baseline shows the wrong state", func(f *fakeSteps) { f.cards[0] = cardView{"Start", "stopped", 0} }, stateFail},
		{"press Stop fails", func(f *fakeSteps) { f.pressErr = map[string]error{"Stop": errors.New("no button")} }, stateFail},
		{"service list never shows stopped", func(f *fakeSteps) { f.serviceOK = []bool{false, true} }, stateFail},
		{"card after Stop unreadable", func(f *fakeSteps) { f.cardErrAt = 2 }, stateFail},
		{"press Start fails", func(f *fakeSteps) { f.pressErr = map[string]error{"Start": errors.New("no button")} }, stateFail},
		{"card after Start unreadable", func(f *fakeSteps) { f.cardErrAt = 3 }, stateFail},
		{"service list never shows the new pid", func(f *fakeSteps) { f.serviceOK = []bool{true, false} }, stateFail},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := happySteps()
			c.mut(f)
			// A cancelled caller context must not stop the restore.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			got := classifySettingsWindow(runSettingsWindowWith(ctx, f))
			checkState(t, got, c.want)
			if f.startFixture != 1 {
				t.Errorf("StartFixture called %d times, want 1", f.startFixture)
			}
			if f.restore != 1 {
				t.Errorf("Restore called %d times, want exactly 1", f.restore)
			}
			if c.want == statePass && fmt.Sprint(f.pressed) != "[Stop Start]" {
				t.Errorf("presses = %v, want [Stop Start]", f.pressed)
			}
			if f.restoreCtxErr != nil {
				t.Errorf("Restore ran on a cancelled context: %v", f.restoreCtxErr)
			}
		})
	}
}

func TestRunSettingsWindowUntrustedTouchesNothing(t *testing.T) {
	f := happySteps()
	f.untrusted = true
	got := classifySettingsWindow(runSettingsWindowWith(context.Background(), f))
	checkState(t, got, stateBlocked)
	if f.startFixture != 0 || f.restore != 0 || len(f.pressed) != 0 {
		t.Errorf("untrusted run touched the system: StartFixture %d, Restore %d, presses %v", f.startFixture, f.restore, f.pressed)
	}
}
