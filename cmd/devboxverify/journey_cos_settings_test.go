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
		ProjectItems: []string{"Not set", "Acme"},
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
			f.page.ProjectItems = []string{"Not set"}
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
		{"Verify Grant is in the menu", func(f *fakeCos) { f.page.ProjectItems = []string{"Not set", cosGrantName, "Acme"} }, stateFail, "cannot run", cosConfig{}},
		{"Acme is not in the menu and shows no reason", func(f *fakeCos) { f.page.ProjectItems = []string{"Not set"} }, stateFail, "not in the project pop-up", cosConfig{}},
		{"project pop-up missing", func(f *fakeCos) { f.page.ProjectFound, f.page.ProjectItems = false, nil }, stateFail, "pop-up", cosConfig{}},
		{"page unreadable", func(f *fakeCos) { f.pageErr = errors.New("no window") }, stateFail, "Projects page", cosConfig{}},
		{"Acme cannot be chosen", func(f *fakeCos) { f.chooseErr = errors.New("no item") }, stateFail, "project pop-up", cosConfig{}},
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

func popUp(label string, titles ...string) *axNode {
	p := &axNode{Role: "AXPopUpButton", Label: label}
	menu := &axNode{Role: "AXMenu"}
	for _, t := range titles {
		menu.Children = append(menu.Children, &axNode{Role: "AXMenuItem", Label: t, press: func() error { return nil }})
	}
	p.Children = []*axNode{menu}
	return p
}

func TestFindPopUp(t *testing.T) {
	byLabel := popUp(cosProjectLabel, "Not set", "Acme")
	titled := popUp("Sonnet", "Haiku", "Sonnet", "Opus")
	other := popUp("Default project", "None", "Acme")
	cases := []struct {
		name   string
		page   []*axNode
		label  string
		marker string
		want   *axNode
	}{
		{"by label", flatten(&axNode{Children: []*axNode{other, byLabel}}), cosProjectLabel, cosNotSet, byLabel},
		{"label wins over an earlier marker match", []*axNode{popUp("x", "Not set"), byLabel}, cosProjectLabel, cosNotSet, byLabel},
		{"by marker item when the title shows the choice", []*axNode{other, titled}, cosModelLabel, "Opus", titled},
		{"none", []*axNode{other}, cosModelLabel, "Opus", nil},
		{"a button that is not a pop-up is skipped", []*axNode{{Role: "AXButton", Label: cosProjectLabel}}, cosProjectLabel, cosNotSet, nil},
	}
	for _, c := range cases {
		if got := findPopUp(c.page, c.label, c.marker); got != c.want {
			t.Errorf("%s: wrong pop-up (%v)", c.name, got)
		}
	}
}

func TestAXPopUpChoose(t *testing.T) {
	var pressed []string
	pop := &axNode{Role: "AXPopUpButton", Children: []*axNode{{Role: "AXMenu"}}}
	for _, title := range []string{"Not set", "Acme", "Acme Two"} {
		pop.Children[0].Children = append(pop.Children[0].Children, &axNode{Role: "AXMenuItem", Label: title, press: func() error {
			pressed = append(pressed, title)
			return nil
		}})
	}
	if err := axPopUpChoose(pop, "Acme"); err != nil || fmt.Sprint(pressed) != "[Acme]" {
		t.Errorf("choose Acme: err %v, pressed %v", err, pressed)
	}
	if err := axPopUpChoose(pop, "Missing"); err == nil {
		t.Error("choosing a missing title succeeded")
	}
	pop.Children[0].Children = append(pop.Children[0].Children, &axNode{Role: "AXMenuItem", Label: "Acme", press: func() error { return nil }})
	if err := axPopUpChoose(pop, "Acme"); err == nil {
		t.Error("choosing a title held twice succeeded")
	}
	if got := axPopUpTitles(pop); fmt.Sprint(got) != "[Not set Acme Acme Two Acme]" {
		t.Errorf("titles = %v", got)
	}
}
