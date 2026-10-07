package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	cosGrantName = "Verify Grant 0a1b2c3d"
	cosAcmeName  = "Acme"
	cosAcmeID    = "p-acme"
)

var cosFix = cosFixture{GrantName: cosGrantName, AcmeName: cosAcmeName, AcmeID: cosAcmeID}

// fakeCos holds the setting in memory the way the route does, and scripts
// the window.
type fakeCos struct {
	untrusted  bool
	setup      result
	cfg        cosConfig
	getErr     error
	getErrAt   int // 1-based Get that fails; 0 never
	gets       int
	putErr     error
	deleteErr  error
	openErr    error
	modelErr   error
	pageErr    error
	page       cosPage
	chooseErr  error
	closeErr   error
	probeErr   error
	grantPick  bool // the pop-up accepts the Verify Grant project
	noApply    bool // the choice never reaches relay
	applyOnErr bool // the choice reaches relay although the press reports an error
	stuckAfter bool // a restore write is accepted but never shows

	calls   []string
	puts    []cosConfig
	ctxErrs []error
}

func (f *fakeCos) Trusted() bool { return !f.untrusted }
func (f *fakeCos) Setup(context.Context) (cosFixture, result, bool) {
	if f.setup.State != "" {
		return cosFixture{}, f.setup, false
	}
	return cosFix, result{}, true
}
func (f *fakeCos) Get(context.Context) (cosConfig, error) {
	f.gets++
	if f.getErr != nil && (f.getErrAt == 0 || f.getErrAt == f.gets) {
		return cosConfig{}, f.getErr
	}
	return f.cfg, nil
}
func (f *fakeCos) Put(ctx context.Context, c cosConfig) error {
	f.calls = append(f.calls, "put")
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.putErr != nil {
		return f.putErr
	}
	f.puts = append(f.puts, c)
	if !f.stuckAfter {
		f.cfg = c
	}
	return nil
}
func (f *fakeCos) Delete(ctx context.Context) error {
	f.calls = append(f.calls, "delete")
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if !f.stuckAfter {
		f.cfg = cosConfig{}
	}
	return nil
}
func (f *fakeCos) Poll(_ context.Context, _ time.Duration, done func(cosConfig) bool) (cosConfig, bool) {
	return f.cfg, done(f.cfg)
}
func (f *fakeCos) OpenProjects(context.Context) error { return f.openErr }
func (f *fakeCos) ChooseModel(context.Context, string) error {
	f.calls = append(f.calls, "model")
	// The panel saves a model change while a project is set.
	if f.modelErr == nil && f.cfg.Configured {
		f.cfg.Model = "haiku"
	}
	return f.modelErr
}
func (f *fakeCos) Page(_ context.Context, _ time.Duration, done func(cosPage) bool) (cosPage, error) {
	f.calls = append(f.calls, "page")
	return f.page, f.pageErr
}
func (f *fakeCos) ProbeProject(_ context.Context, title string) (bool, error) {
	f.calls = append(f.calls, "probe "+title)
	return f.grantPick, f.probeErr
}
func (f *fakeCos) ChooseProject(context.Context, string) error {
	f.calls = append(f.calls, "project")
	if f.chooseErr != nil {
		if f.applyOnErr {
			f.cfg = cosConfig{true, cosAcmeID, "haiku", 100}
		}
		return f.chooseErr
	}
	if !f.noApply {
		f.cfg = cosConfig{true, cosAcmeID, "haiku", 100}
	}
	return nil
}
func (f *fakeCos) Close(ctx context.Context) error {
	f.calls = append(f.calls, "close")
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	return f.closeErr
}

func goodCosPage() cosPage {
	return cosPage{
		Texts:        []string{"Chief of Staff", cosGrantName + ": It doesn't allow the claude-code template."},
		ProjectFound: true,
	}
}

func newFakeCos() *fakeCos { return &fakeCos{page: goodCosPage()} }

func TestRunCosSettings(t *testing.T) {
	driver := fmt.Errorf("press: %w", errAXDriver)
	earlier := cosConfig{true, "p-old", "opus", 40}
	cases := []struct {
		name   string
		mut    func(*fakeCos)
		want   state
		detail string
		end    cosConfig // the setting after the run
	}{
		{"passes and leaves no setting when none was set", func(*fakeCos) {}, statePass, "setting restored", cosConfig{}},
		{"passes and puts an earlier setting back", func(f *fakeCos) { f.cfg = earlier }, statePass, "", earlier},
		{"no Accessibility trust", func(f *fakeCos) { f.untrusted = true }, stateBlocked, "Accessibility", cosConfig{}},
		{"no run credential", func(f *fakeCos) { f.setup = blocked(cosSettingsID, "no run credential") }, stateBlocked, "no run credential", cosConfig{}},
		{"no Verify Grant project", func(f *fakeCos) { f.setup = blocked(cosSettingsID, "no Verify Grant project: "+grantPosID) }, stateBlocked, grantPosID, cosConfig{}},
		{"socket unreachable", func(f *fakeCos) { f.getErr = fmt.Errorf("%w: frontend socket unreachable", errCosEnv) }, stateBlocked, "unreachable", cosConfig{}},
		{"GET refused", func(f *fakeCos) { f.getErr = errors.New("GET: status 404") }, stateFail, "GET", cosConfig{}},
		{"driver error opening", func(f *fakeCos) { f.openErr = driver }, stateBlocked, "accessibility driver", cosConfig{}},
		{"driver error choosing the model", func(f *fakeCos) { f.modelErr = driver }, stateBlocked, "accessibility driver", cosConfig{}},
		{"driver error reading the page", func(f *fakeCos) { f.pageErr = driver }, stateBlocked, "accessibility driver", cosConfig{}},
		{"Acme shows a reason", func(f *fakeCos) {
			f.page.Texts = append(f.page.Texts, "Acme: It has a permission policy.")
		}, stateBlocked, "Acme", cosConfig{}},
		{"window does not open", func(f *fakeCos) { f.openErr = errors.New("no Projects tab") }, stateFail, "Projects", cosConfig{}},
		{"model pop-up missing", func(f *fakeCos) { f.modelErr = errors.New("no pop-up") }, stateFail, "model pop-up", cosConfig{}},
		{"reason line missing", func(f *fakeCos) { f.page.Texts = f.page.Texts[:1] }, stateFail, "claude-code template", cosConfig{}},
		{"reason line for another project", func(f *fakeCos) {
			f.page.Texts = []string{"Verify Grant ffffffff: It doesn't allow the claude-code template."}
		}, stateFail, "claude-code template", cosConfig{}},
		{"reason text differs", func(f *fakeCos) {
			f.page.Texts = []string{cosGrantName + ": It has a permission policy."}
		}, stateFail, "claude-code template", cosConfig{}},
		{"Verify Grant can be selected", func(f *fakeCos) { f.grantPick = true }, stateFail, "can be selected", cosConfig{}},
		{"driver error probing Verify Grant", func(f *fakeCos) { f.probeErr = driver }, stateBlocked, "accessibility driver", cosConfig{}},
		{"probe fails", func(f *fakeCos) { f.probeErr = errors.New("no pop-up") }, stateFail, "project pop-up", cosConfig{}},
		{"project pop-up missing", func(f *fakeCos) { f.page.ProjectFound = false }, stateFail, "pop-up", cosConfig{}},
		{"page unreadable", func(f *fakeCos) { f.pageErr = errors.New("no window") }, stateFail, "Projects page", cosConfig{}},
		{"Acme cannot be chosen", func(f *fakeCos) { f.chooseErr = errors.New("no item") }, stateFail, "choosing Acme", cosConfig{}},
		{"the choice never reaches relay", func(f *fakeCos) { f.noApply = true }, stateFail, "5 s after the choice", cosConfig{}},
		{"restore write refused", func(f *fakeCos) { f.deleteErr = errors.New("DELETE: status 500") }, stateFail, "restore", cosConfig{true, cosAcmeID, "haiku", 100}},
		{"restore does not read back", func(f *fakeCos) { f.stuckAfter = true }, stateFail, "read back", cosConfig{true, cosAcmeID, "haiku", 100}},
		{"close failed after a pass", func(f *fakeCos) { f.closeErr = errors.New("no close button") }, statePass, "close: no close button", cosConfig{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeCos()
			c.mut(f)
			got := classifyCosSettings(runCosSettingsWith(context.Background(), f))
			checkState(t, got, c.want)
			if c.detail != "" {
				checkDetail(t, got, c.detail)
			}
			if f.cfg != c.end {
				t.Errorf("setting after the run = %+v, want %+v", f.cfg, c.end)
			}
		})
	}
}

func TestRunCosSettingsRestoresTheEarlierState(t *testing.T) {
	earlier := cosConfig{true, "p-old", "opus", 40}
	for _, c := range []struct {
		name  string
		base  cosConfig
		mut   func(*fakeCos)
		write string // the one write the restore makes
	}{
		{"earlier block is PUT back after a pass", earlier, func(*fakeCos) {}, "put"},
		{"unset is DELETEd after a pass", cosConfig{}, func(*fakeCos) {}, "delete"},
		{"earlier block is PUT back after the page failed", earlier, func(f *fakeCos) { f.pageErr = errors.New("no window") }, "put"},
		{"unset is DELETEd after the choice errored", cosConfig{}, func(f *fakeCos) { f.chooseErr, f.applyOnErr = errors.New("no item"), true }, "delete"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeCos()
			f.cfg = c.base
			c.mut(f)
			// A cancelled caller context must not stop the restore.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			runCosSettingsWith(ctx, f)
			var writes []string
			for _, op := range f.calls {
				if op == "put" || op == "delete" {
					writes = append(writes, op)
				}
			}
			if got := strings.Join(writes, ","); got != c.write {
				t.Errorf("restore writes = %q, want %q", got, c.write)
			}
			if f.cfg != c.base {
				t.Errorf("setting after the run = %+v, want %+v", f.cfg, c.base)
			}
			if c.write == "put" && (len(f.puts) != 1 || f.puts[0] != c.base) {
				t.Errorf("PUT sent %+v, want exactly %+v", f.puts, c.base)
			}
			for i, err := range f.ctxErrs {
				if err != nil {
					t.Errorf("restore call %d ran on a cancelled context: %v", i, err)
				}
			}
			if f.calls[len(f.calls)-1] != "close" {
				t.Errorf("calls = %v, want close last", f.calls)
			}
		})
	}
}

func TestRunCosSettingsLeavesAMatchingSettingAlone(t *testing.T) {
	f := newFakeCos()
	f.page.Texts = f.page.Texts[:1] // fails before any change
	runCosSettingsWith(context.Background(), f)
	for _, op := range f.calls {
		if op == "put" || op == "delete" {
			t.Errorf("wrote %s although the setting never changed; calls %v", op, f.calls)
		}
	}
}

func TestRunCosSettingsStopsBeforeAnyWriteWhenItCannotStart(t *testing.T) {
	for name, mut := range map[string]func(*fakeCos){
		"untrusted":   func(f *fakeCos) { f.untrusted = true },
		"no fixture":  func(f *fakeCos) { f.setup = blocked(cosSettingsID, "x") },
		"GET failing": func(f *fakeCos) { f.getErr = errors.New("GET: status 500") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeCos()
			mut(f)
			runCosSettingsWith(context.Background(), f)
			if len(f.calls) != 0 {
				t.Errorf("touched the window or the setting: %v", f.calls)
			}
		})
	}
}

func TestParseCosConfig(t *testing.T) {
	for _, c := range []struct {
		in   string
		want cosConfig
		ok   bool
	}{
		{`{"configured":false}`, cosConfig{}, true},
		{`{"configured":true,"projectId":"p1","model":"haiku","dailyModelCalls":40}`, cosConfig{true, "p1", "haiku", 40}, true},
		{`{"configured":false,"projectId":"p1"}`, cosConfig{}, true},
		{`not json`, cosConfig{}, false},
	} {
		got, err := parseCosConfig([]byte(c.in))
		if got != c.want || (err == nil) != c.ok {
			t.Errorf("parseCosConfig(%s) = %+v, %v", c.in, got, err)
		}
	}
}

func TestFindPopUp(t *testing.T) {
	project := &axNode{Role: "AXPopUpButton", Label: cosProjectLabel, Value: "Not set"}
	model := &axNode{Role: "AXPopUpButton", Label: cosModelLabel, Value: "Sonnet"}
	notPopUp := &axNode{Role: "AXButton", Label: cosModelLabel}
	page := []*axNode{{Role: "AXPopUpButton", Label: "Default project"}, notPopUp, project, model}
	if got := findPopUp(page, cosProjectLabel); got != project {
		t.Errorf("project pop-up: got %v", got)
	}
	if got := findPopUp(page, cosModelLabel); got != model {
		t.Errorf("model pop-up: got %v (a button with the same label must not match)", got)
	}
	if got := findPopUp(page[:2], cosProjectLabel); got != nil {
		t.Errorf("absent pop-up: got %v", got)
	}
}

func TestRunCosSettingsProbesGrantBeforeChoosingAcme(t *testing.T) {
	f := newFakeCos()
	runCosSettingsWith(context.Background(), f)
	if got, want := strings.Join(f.calls[:4], ","), "model,page,probe "+cosGrantName+",project"; got != want {
		t.Errorf("calls = %q, want it to start %q", got, want)
	}
	f = newFakeCos()
	f.grantPick = true
	runCosSettingsWith(context.Background(), f)
	if strings.Contains(strings.Join(f.calls, ","), "project") {
		t.Errorf("chose a project after the Verify Grant probe was accepted: %v", f.calls)
	}
}
