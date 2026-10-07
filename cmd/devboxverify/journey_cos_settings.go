package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	cosSettingsID    = "cos-settings"
	cosConfigPath    = "/api/chief-of-staff/config"
	cosModelLabel    = "Chief of Staff model"
	cosProjectLabel  = "Chief of Staff project"
	cosPickModel     = "Haiku"
	cosPickModelName = "haiku"
	cosTemplateLine  = "It doesn't allow the claude-code template."

	cosUIWait  = 5 * time.Second
	cosPollGap = 200 * time.Millisecond
)

// errCosEnv marks a failure of the environment (socket, credential), which
// reads BLOCKED.
var errCosEnv = errors.New("environment")

// cosConfig is relay's Chief of Staff setting as GET serves it. The zero
// value is "not configured".
type cosConfig struct {
	Configured      bool
	ProjectID       string
	Model           string
	DailyModelCalls int
}

func parseCosConfig(body []byte) (cosConfig, error) {
	var v struct {
		Configured      bool   `json:"configured"`
		ProjectID       string `json:"projectId"`
		Model           string `json:"model"`
		DailyModelCalls int    `json:"dailyModelCalls"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return cosConfig{}, fmt.Errorf("unreadable config: %w", err)
	}
	if !v.Configured {
		return cosConfig{}, nil
	}
	return cosConfig{true, v.ProjectID, v.Model, v.DailyModelCalls}, nil
}

type cosFixture struct {
	GrantName, AcmeName, AcmeID string
}

// cosPage is what the Projects page shows: its static text and whether the
// project pop-up is there.
type cosPage struct {
	Texts        []string
	ProjectFound bool
}

func (p cosPage) hasText(s string) bool { return slices.Contains(p.Texts, s) }

func (p cosPage) hasTextPrefix(prefix string) bool {
	for _, t := range p.Texts {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

type cosSteps interface {
	Trusted() bool
	Setup(ctx context.Context) (cosFixture, result, bool)
	Get(ctx context.Context) (cosConfig, error)
	Put(ctx context.Context, c cosConfig) error
	Delete(ctx context.Context) error
	// Poll reads the setting until done holds or within passes.
	Poll(ctx context.Context, within time.Duration, done func(cosConfig) bool) (cosConfig, bool)
	OpenProjects(ctx context.Context) error
	// ChooseModel and ChooseProject select an option by typing its text into
	// the focused pop-up and return once the pop-up shows it.
	ChooseModel(ctx context.Context, title string) error
	ChooseProject(ctx context.Context, title string) error
	// ProbeProject types title into the project pop-up and reports whether
	// the pop-up ever showed it within the bounded window.
	ProbeProject(ctx context.Context, title string) (selected bool, err error)
	Page(ctx context.Context, within time.Duration, done func(cosPage) bool) (cosPage, error)
	// Close closes the Settings window if the journey opened it.
	Close(ctx context.Context) error
}

type cosSettingsRun struct {
	Untrusted  bool
	Setup      result
	Fix        cosFixture
	Base       cosConfig
	BaseErr    error
	OpenErr    error
	ModelErr   error
	PageErr    error
	ProbeErr   error
	GrantPick  bool // the pop-up accepted the Verify Grant project
	Page       cosPage
	ChooseErr  error
	Applied    cosConfig
	AppliedOK  bool
	RestoreErr error
	CloseErr   error
}

func (f cosFixture) grantLine() string  { return f.GrantName + ": " + cosTemplateLine }
func (f cosFixture) acmeReason() string { return f.AcmeName + ": " }

func runCosSettingsWith(ctx context.Context, s cosSteps) (r cosSettingsRun) {
	if !s.Trusted() {
		r.Untrusted = true
		return r
	}
	var ok bool
	if r.Fix, r.Setup, ok = s.Setup(ctx); !ok {
		return r
	}
	if r.Base, r.BaseErr = s.Get(ctx); r.BaseErr != nil {
		return r
	}
	// Restore runs once, on every path past the first read, and not on the
	// caller's cancelled context.
	defer func() {
		bg := context.WithoutCancel(ctx)
		r.RestoreErr = restoreCosConfig(bg, s, r.Base)
		r.CloseErr = s.Close(bg)
	}()
	if r.OpenErr = s.OpenProjects(ctx); r.OpenErr != nil {
		return r
	}
	if r.ModelErr = s.ChooseModel(ctx, cosPickModel); r.ModelErr != nil {
		return r
	}
	r.Page, r.PageErr = s.Page(ctx, cosUIWait, func(p cosPage) bool {
		return p.ProjectFound && p.hasText(r.Fix.grantLine())
	})
	if r.PageErr != nil || r.Page.hasTextPrefix(r.Fix.acmeReason()) || !r.pageOK() {
		return r
	}
	// Waits: none possible: an absent option raises nothing; bounded
	// observation of the pop-up for 5 s.
	if r.GrantPick, r.ProbeErr = s.ProbeProject(ctx, r.Fix.GrantName); r.ProbeErr != nil || r.GrantPick {
		return r
	}
	if r.ChooseErr = s.ChooseProject(ctx, r.Fix.AcmeName); r.ChooseErr != nil {
		return r
	}
	// Waits: none possible: the WebView exposes no event to an out-of-process
	// Accessibility reader, and relay raises nothing a harness can subscribe
	// to for this write. Poll GET every 200 ms for up to 5 s.
	r.Applied, r.AppliedOK = s.Poll(ctx, cosUIWait, func(c cosConfig) bool {
		return c.Configured && c.ProjectID == r.Fix.AcmeID && c.Model == cosPickModelName
	})
	return r
}

func (r cosSettingsRun) pageOK() bool {
	return r.Page.ProjectFound && r.Page.hasText(r.Fix.grantLine())
}

// restoreCosConfig puts the setting back exactly as base read: the same
// block, or no block. It leaves a setting that already matches alone.
func restoreCosConfig(ctx context.Context, s cosSteps, base cosConfig) error {
	cur, err := s.Get(ctx)
	if err == nil && cur == base {
		return nil
	}
	if base.Configured {
		err = s.Put(ctx, base)
	} else {
		err = s.Delete(ctx)
	}
	if err != nil {
		return err
	}
	// Waits: none possible; same as the poll after the choice.
	if got, ok := s.Poll(ctx, cosUIWait, func(c cosConfig) bool { return c == base }); !ok {
		return fmt.Errorf("read back after restore shows %+v, want %+v", got, base)
	}
	return nil
}

func classifyCosSettings(r cosSettingsRun) result {
	const id = cosSettingsID
	fail := func(d string) result { return result{id, stateFail, d} }
	if r.Untrusted {
		return blocked(id, "no Accessibility trust for this process; run devboxverify from a desktop Terminal")
	}
	if r.Setup.State != "" {
		return r.Setup
	}
	if errors.Is(r.BaseErr, errCosEnv) {
		return blocked(id, r.BaseErr.Error())
	}
	if r.RestoreErr != nil {
		return fail("restore: " + r.RestoreErr.Error())
	}
	for _, err := range []error{r.OpenErr, r.ModelErr, r.PageErr, r.ProbeErr, r.ChooseErr} {
		if errors.Is(err, errAXDriver) {
			return blocked(id, err.Error())
		}
	}
	switch {
	case r.BaseErr != nil:
		return fail("GET " + cosConfigPath + ": " + r.BaseErr.Error())
	case r.OpenErr != nil:
		return fail(r.OpenErr.Error())
	case r.ModelErr != nil:
		return fail("model pop-up: " + r.ModelErr.Error())
	case r.PageErr != nil:
		return fail("Projects page: " + r.PageErr.Error())
	case r.Page.hasTextPrefix(r.Fix.acmeReason()):
		return blocked(id, "fixture: the panel lists "+r.Fix.AcmeName+" as unable to run the Chief of Staff")
	case !r.Page.ProjectFound:
		return fail(`no pop-up labelled "` + cosProjectLabel + `" on the page`)
	case !r.Page.hasText(r.Fix.grantLine()):
		return fail(fmt.Sprintf("no line %q on the page", r.Fix.grantLine()))
	case r.ProbeErr != nil:
		return fail("project pop-up: " + r.ProbeErr.Error())
	case r.GrantPick:
		return fail(r.Fix.GrantName + " can be selected in the project pop-up, but it cannot run the Chief of Staff")
	case r.ChooseErr != nil:
		return fail(fmt.Sprintf("choosing %s in the project pop-up: %s", r.Fix.AcmeName, r.ChooseErr.Error()))
	case !r.AppliedOK:
		return fail(fmt.Sprintf("5 s after the choice GET shows %+v, want %s with model %s", r.Applied, r.Fix.AcmeID, cosPickModelName))
	}
	detail := "Haiku chosen in the model pop-up; " + r.Fix.GrantName + " listed with its reason and not selectable; " +
		r.Fix.AcmeName + " chosen; GET serves it with model haiku; setting restored"
	if r.CloseErr != nil {
		detail += "; close: " + r.CloseErr.Error()
	}
	return result{id, statePass, detail}
}

func runCosSettings(ctx context.Context, e env) result {
	return classifyCosSettings(runCosSettingsWith(ctx, &liveCosSteps{liveSettingsSteps: liveSettingsSteps{e: e}}))
}

// liveCosSteps drives the real window through Accessibility and the setting
// through the frontend API with the run credential, which has the configure
// class.
type liveCosSteps struct {
	liveSettingsSteps
	token string
}

func (s *liveCosSteps) Setup(ctx context.Context) (cosFixture, result, bool) {
	const id = cosSettingsID
	token, res, ok := runCredential(s.e, id)
	if !ok {
		return cosFixture{}, res, false
	}
	if s.e.Run.GrantProjectID == "" || s.e.Run.GrantProjectName == "" {
		return cosFixture{}, blocked(id, "no Verify Grant project: "+grantPosID+" did not pass"), false
	}
	acme, acmeID, err := grantedAcme(ctx, s.e)
	if err != nil {
		return cosFixture{}, blocked(id, "fixture: "+err.Error()), false
	}
	s.token = token
	return cosFixture{GrantName: s.e.Run.GrantProjectName, AcmeName: acme.Name, AcmeID: acmeID}, result{}, true
}

func (s *liveCosSteps) call(ctx context.Context, method, act string, body []byte) (frontendResponse, error) {
	resp := frontendDo(ctx, s.e, s.token, method, cosConfigPath, body)
	if res, refused := frontendRefusal(cosSettingsID, act, resp); refused {
		if res.State == stateBlocked {
			return resp, fmt.Errorf("%w: %s", errCosEnv, res.Detail)
		}
		return resp, errors.New(res.Detail)
	}
	return resp, nil
}

func (s *liveCosSteps) Get(ctx context.Context) (cosConfig, error) {
	resp, err := s.call(ctx, http.MethodGet, "GET", nil)
	if err != nil {
		return cosConfig{}, err
	}
	return parseCosConfig(resp.Body)
}

func (s *liveCosSteps) Put(ctx context.Context, c cosConfig) error {
	_, err := s.call(ctx, http.MethodPut, "PUT", jsonBody(map[string]any{
		"projectId": c.ProjectID, "model": c.Model, "dailyModelCalls": c.DailyModelCalls,
	}))
	return err
}

func (s *liveCosSteps) Delete(ctx context.Context) error {
	_, err := s.call(ctx, http.MethodDelete, "DELETE", nil)
	return err
}

func (s *liveCosSteps) Poll(ctx context.Context, within time.Duration, done func(cosConfig) bool) (cosConfig, bool) {
	var last cosConfig
	for deadline := time.Now().Add(within); ; {
		if c, err := s.Get(ctx); err == nil {
			if last = c; done(c) {
				return c, true
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return last, false
		}
		time.Sleep(cosPollGap)
	}
}

func (s *liveCosSteps) OpenProjects(ctx context.Context) error {
	win, err := s.findWindow(ctx, 2*time.Second)
	if err != nil {
		return err
	}
	if win == nil {
		if err := s.openFromTray(ctx); err != nil {
			return err
		}
		s.opened = true
	}
	return s.pressTab(ctx, "Projects")
}

// pressTab waits for the tab, because a window that just opened has not
// loaded its page yet.
func (s *liveCosSteps) pressTab(ctx context.Context, label string) error {
	tab, release, err := pollNode(ctx, cosUIWait, func() (*axNode, func(), error) {
		win, release, err := s.settingsWindow()
		if err != nil || win == nil {
			return nil, release, err
		}
		return findNode(win, func(n *axNode) bool { return n.Role == "AXRadioButton" && n.Label == label }, release)
	})
	if err != nil {
		return err
	}
	defer release()
	if tab == nil {
		return fmt.Errorf("no %s tab", label)
	}
	return tab.press()
}

// findPopUp finds a pop-up by its AXTitle, which WebKit sets to the
// select's aria-label. The pop-up's AXValue is the selected option's text.
func findPopUp(page []*axNode, label string) *axNode {
	for _, n := range page {
		if n.Role == "AXPopUpButton" && n.Label == label {
			return n
		}
	}
	return nil
}

// typeInto focuses a pop-up and types title into it, without Return and
// without opening its menu: WebKit's type-ahead selects the option that
// starts with the text. Opening the menu would block Accessibility reads of
// the page.
func (s *liveCosSteps) typeInto(ctx context.Context, label, title string) error {
	pop, release, err := pollNode(ctx, cosUIWait, func() (*axNode, func(), error) {
		win, release, err := s.settingsWindow()
		if err != nil || win == nil {
			return nil, release, err
		}
		return findNode(win, func(n *axNode) bool { return n.Role == "AXPopUpButton" && n.Label == label }, release)
	})
	if err != nil {
		return err
	}
	if pop == nil {
		return fmt.Errorf("no pop-up labelled %q", label)
	}
	defer release()
	if err := axFocus(pop); err != nil {
		return err
	}
	return axTypeText(s.e.RelayPID, title)
}

// popUpValue reads the pop-up's selected text from a fresh snapshot; the page
// re-renders after a change.
func (s *liveCosSteps) popUpValue(label string) (string, error) {
	win, release, err := s.settingsWindow()
	defer release()
	if err != nil {
		return "", err
	}
	if win == nil {
		return "", fmt.Errorf("no %q window", settingsWindowTitle)
	}
	pop := findPopUp(flatten(win), label)
	if pop == nil {
		return "", fmt.Errorf("no pop-up labelled %q", label)
	}
	return pop.Value, nil
}

// watch polls the pop-up every 200 ms for up to within and reports whether
// its value ever equalled title. It stops at the first match.
func (s *liveCosSteps) watch(ctx context.Context, label, title string, within time.Duration) (bool, error) {
	var lastErr error
	for deadline := time.Now().Add(within); ; {
		switch v, err := s.popUpValue(label); {
		case err != nil:
			lastErr = err
		case v == title:
			return true, nil
		default:
			lastErr = nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false, lastErr
		}
		time.Sleep(cosPollGap)
	}
}

func (s *liveCosSteps) choose(ctx context.Context, label, title string) error {
	if err := s.typeInto(ctx, label, title); err != nil {
		return err
	}
	// Waits: none possible: the WebView exposes no event to an out-of-process
	// Accessibility reader. Poll every 200 ms for up to 5 s.
	ok, err := s.watch(ctx, label, title, cosUIWait)
	if !ok {
		if err != nil {
			return err
		}
		return fmt.Errorf("pop-up %q does not show %q 5 s after typing it", label, title)
	}
	return nil
}

func (s *liveCosSteps) ChooseModel(ctx context.Context, title string) error {
	return s.choose(ctx, cosModelLabel, title)
}

func (s *liveCosSteps) ChooseProject(ctx context.Context, title string) error {
	return s.choose(ctx, cosProjectLabel, title)
}

func (s *liveCosSteps) ProbeProject(ctx context.Context, title string) (bool, error) {
	if err := s.typeInto(ctx, cosProjectLabel, title); err != nil {
		return false, err
	}
	return s.watch(ctx, cosProjectLabel, title, cosUIWait)
}

func (s *liveCosSteps) readPage() (cosPage, error) {
	win, release, err := s.settingsWindow()
	defer release()
	if err != nil {
		return cosPage{}, err
	}
	if win == nil {
		return cosPage{}, fmt.Errorf("no %q window", settingsWindowTitle)
	}
	nodes := flatten(win)
	var p cosPage
	for _, n := range nodes {
		if n.Role == "AXStaticText" {
			p.Texts = append(p.Texts, n.Label)
		}
	}
	p.ProjectFound = findPopUp(nodes, cosProjectLabel) != nil
	return p, nil
}

func (s *liveCosSteps) Page(ctx context.Context, within time.Duration, done func(cosPage) bool) (cosPage, error) {
	var last cosPage
	var lastErr error
	for deadline := time.Now().Add(within); ; {
		p, err := s.readPage()
		if err != nil {
			lastErr = err
		} else if last, lastErr = p, nil; done(p) {
			return p, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return last, lastErr
		}
		time.Sleep(cosPollGap)
	}
}

// Close closes the window when the journey opened it. The embedded Restore
// does only that while token is empty, which it is here: this type keeps its
// credential in its own field.
func (s *liveCosSteps) Close(ctx context.Context) error {
	return s.liveSettingsSteps.Restore(ctx)
}
