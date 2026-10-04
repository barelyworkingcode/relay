package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"time"
)

const (
	settingsWindowID    = "settings-window-services"
	settingsWindowTitle = "Relay Settings"
)

// axNode is one element of an Accessibility snapshot. Label is the first
// non-empty of AXTitle, AXDescription and a string AXValue. press is nil in
// a synthetic tree.
type axNode struct {
	Role, Subrole, Label, Help string
	Children                   []*axNode
	press                      func() error
}

// flatten returns the tree in document order, which is the order the page
// reads in.
func flatten(root *axNode) []*axNode {
	if root == nil {
		return nil
	}
	out := []*axNode{root}
	for _, c := range root.Children {
		out = append(out, flatten(c)...)
	}
	return out
}

type cardView struct {
	Button string
	Status string
	PID    int
}

var statusLinePID = regexp.MustCompile(`^pid (\d+)( · up .+)?$`)

func parseStatusLine(s string) (pid int, ok bool) {
	m := statusLinePID.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	if _, err := fmt.Sscanf(m[1], "%d", &pid); err != nil {
		return 0, false
	}
	return pid, true
}

func isStatusLine(s string) bool {
	_, pid := parseStatusLine(s)
	return pid || s == "stopped" || s == "running"
}

// findServiceCard reads one service's card out of a flattened page. WebKit
// exposes a card as siblings with no container, so the card is the span from
// the service's name to its "Show in menu" switch. It refuses unless the span
// holds one Edit, one Start or Stop and one status line, so it can never hand
// back another service's button.
func findServiceCard(page []*axNode, displayName string) (cardView, *axNode, error) {
	first := -1
	for i, n := range page {
		if n.Role != "AXStaticText" || n.Label != displayName {
			continue
		}
		if first >= 0 {
			return cardView{}, nil, fmt.Errorf("%q appears more than once on the page", displayName)
		}
		first = i
	}
	if first < 0 {
		return cardView{}, nil, fmt.Errorf("no %q on the page", displayName)
	}
	last := -1
	for i := first + 1; i < len(page); i++ {
		if page[i].Role == "AXCheckBox" && page[i].Label == "Show in menu" {
			last = i
			break
		}
	}
	if last < 0 {
		return cardView{}, nil, fmt.Errorf("%q has no Show in menu switch after it", displayName)
	}
	var edits, toggles, statuses int
	var view cardView
	var toggle *axNode
	for _, n := range page[first+1 : last+1] {
		switch {
		case n.Role == "AXButton" && n.Label == "Edit":
			edits++
		case n.Role == "AXButton" && (n.Label == "Start" || n.Label == "Stop"):
			toggles++
			toggle, view.Button = n, n.Label
		case n.Role == "AXStaticText" && isStatusLine(n.Label):
			statuses++
			view.Status = n.Label
			view.PID, _ = parseStatusLine(n.Label)
		}
	}
	if edits != 1 || toggles != 1 || statuses != 1 {
		return cardView{}, nil, fmt.Errorf("%q card holds %d Edit, %d Start or Stop and %d status lines, want one of each", displayName, edits, toggles, statuses)
	}
	return view, toggle, nil
}

type settingsSteps interface {
	Trusted() bool
	StartFixture(ctx context.Context) (up svcView, res result, ok bool)
	OpenServices(ctx context.Context) error
	Card(ctx context.Context, within time.Duration, done func(cardView) bool) (cardView, error)
	Press(ctx context.Context, label string) error
	Service(ctx context.Context, within time.Duration, done func(svcView) bool) (svcView, bool)
	Restore(ctx context.Context) error
}

type settingsRun struct {
	Untrusted     bool
	Setup         result
	Up            svcView
	OpenErr       error
	Before        cardView
	BeforeErr     error
	StopErr       error
	Stopped       svcView
	Down          bool
	AfterStop     cardView
	AfterStopErr  error
	StartErr      error
	AfterStart    cardView
	AfterStartErr error
	Running       svcView
	RunningOK     bool
	RestoreErr    error
}

const (
	settingsCardWait  = 5 * time.Second
	settingsStartWait = 10 * time.Second
)

func runSettingsWindowWith(ctx context.Context, s settingsSteps) (r settingsRun) {
	if !s.Trusted() {
		r.Untrusted = true
		return r
	}
	var ok bool
	// Restore runs once, whatever the steps below did, and not on the
	// caller's cancelled context.
	defer func() { r.RestoreErr = s.Restore(context.WithoutCancel(ctx)) }()
	if r.Up, r.Setup, ok = s.StartFixture(ctx); !ok {
		return r
	}
	if r.OpenErr = s.OpenServices(ctx); r.OpenErr != nil {
		return r
	}
	if r.Before, r.BeforeErr = s.Card(ctx, settingsCardWait, func(c cardView) bool {
		return c.Button == "Stop" && slices.Contains(r.Up.PIDs, c.PID)
	}); r.BeforeErr != nil || !baselineOK(r.Before, r.Up) {
		return r
	}
	if r.StopErr = s.Press(ctx, "Stop"); r.StopErr != nil {
		return r
	}
	// The window shows "stopped" as soon as Stop is pressed, so only the
	// service list shows the stop happened.
	if r.Stopped, r.Down = s.Service(ctx, settingsCardWait, isDown); !r.Down {
		return r
	}
	if r.AfterStop, r.AfterStopErr = s.Card(ctx, settingsCardWait, stoppedCard); r.AfterStopErr != nil || !stoppedCard(r.AfterStop) {
		return r
	}
	if r.StartErr = s.Press(ctx, "Start"); r.StartErr != nil {
		return r
	}
	newPID := func(c cardView) bool { return c.PID != 0 && !slices.Contains(r.Up.PIDs, c.PID) }
	if r.AfterStart, r.AfterStartErr = s.Card(ctx, settingsStartWait, newPID); r.AfterStartErr != nil || !newPID(r.AfterStart) {
		return r
	}
	r.Running, r.RunningOK = s.Service(ctx, settingsCardWait, func(v svcView) bool {
		return isUp(v) && slices.Contains(v.PIDs, r.AfterStart.PID)
	})
	return r
}

func baselineOK(c cardView, up svcView) bool {
	return c.Button == "Stop" && slices.Contains(up.PIDs, c.PID)
}

func stoppedCard(c cardView) bool { return c.Status == "stopped" && c.Button == "Start" }

func classifySettingsWindow(r settingsRun) result {
	const id = settingsWindowID
	fail := func(d string) result { return result{id, stateFail, d} }
	if r.Untrusted {
		return blocked(id, "no Accessibility trust for this process; run devboxverify from a desktop Terminal")
	}
	if r.Setup.State != "" {
		return r.Setup
	}
	for _, err := range []error{r.OpenErr, r.BeforeErr, r.StopErr, r.AfterStopErr, r.StartErr, r.AfterStartErr} {
		if errors.Is(err, errAXDriver) {
			return blocked(id, err.Error())
		}
	}
	for _, v := range []svcView{r.Up, r.Stopped, r.Running} {
		if v.Err != nil {
			return blocked(id, v.Err.Error())
		}
	}
	switch {
	case r.OpenErr != nil:
		return fail(r.OpenErr.Error())
	case r.BeforeErr != nil:
		return fail("service card: " + r.BeforeErr.Error())
	case !baselineOK(r.Before, r.Up):
		return fail(fmt.Sprintf("before the Stop, the window shows %q and %q, want Stop and one of pids %v", r.Before.Button, r.Before.Status, r.Up.PIDs))
	case r.StopErr != nil:
		return fail("service card: " + r.StopErr.Error())
	case !r.Down:
		return fail(fmt.Sprintf("relay service list shows STATE %s, %d processes, 5 s after the window's Stop", r.Stopped.State, len(r.Stopped.PIDs)))
	case r.AfterStopErr != nil:
		return fail("service card: " + r.AfterStopErr.Error())
	case !stoppedCard(r.AfterStop):
		return fail(fmt.Sprintf("after the Stop, want stopped and Start; the window shows %q and %q", r.AfterStop.Status, r.AfterStop.Button))
	case r.StartErr != nil:
		return fail("service card: " + r.StartErr.Error())
	case r.AfterStartErr != nil:
		return fail("service card: " + r.AfterStartErr.Error())
	case r.AfterStart.PID == 0 || slices.Contains(r.Up.PIDs, r.AfterStart.PID):
		return fail(fmt.Sprintf("no new pid 10 s after its Start; the window shows %q and %q", r.AfterStart.Status, r.AfterStart.Button))
	case !r.RunningOK:
		return fail(fmt.Sprintf("relay service list does not show pid %d running 5 s after the window's Start: STATE %s, pids %v", r.AfterStart.PID, r.Running.State, r.Running.PIDs))
	}
	detail := fmt.Sprintf("opened from the tray; stopped and started from the service's row; window and relay service list agree; pid %d then %d", r.Before.PID, r.AfterStart.PID)
	if r.RestoreErr != nil {
		detail += "; restore: " + r.RestoreErr.Error()
	}
	return result{id, statePass, detail}
}

func runSettingsWindow(ctx context.Context, e env) result {
	return classifySettingsWindow(runSettingsWindowWith(ctx, &liveSettingsSteps{e: e}))
}

// liveSettingsSteps drives the real window through Accessibility and the
// fixture through the frontend API.
type liveSettingsSteps struct {
	e      env
	token  string
	svcID  string
	opened bool
}

func (s *liveSettingsSteps) displayName() string { return crashNamePrefix + s.e.Nonce }

func (s *liveSettingsSteps) Trusted() bool { return axTrusted() }

func (s *liveSettingsSteps) StartFixture(ctx context.Context) (svcView, result, bool) {
	const id = settingsWindowID
	token, svcID, res, ok := serviceFixture(s.e, id)
	if !ok {
		return svcView{}, res, false
	}
	s.token, s.svcID = token, svcID
	if res, bad := callRefusal(id, "start", serviceAction(ctx, s.e, token, svcID, "start")); bad {
		return svcView{}, blocked(id, "fixture: "+res.Detail), false
	}
	up, ok := pollService(ctx, s.e, svcID, crashPIDPattern(svcID), 5*time.Second, isUp, nil)
	if up.Err != nil {
		return up, blocked(id, up.Err.Error()), false
	}
	if !ok {
		return up, blocked(id, "fixture: crash service not running 5 s after the API start"), false
	}
	return up, result{}, true
}

func (s *liveSettingsSteps) settingsWindow() (*axNode, func(), error) {
	wins, release, err := axWindows(s.e.RelayPID)
	if err != nil {
		return nil, release, err
	}
	for _, w := range wins {
		if w.Role == "AXWindow" && w.Label == settingsWindowTitle {
			return w, release, nil
		}
	}
	release()
	return nil, func() {}, nil
}

func (s *liveSettingsSteps) OpenServices(ctx context.Context) error {
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
	return s.pressServicesTab(ctx)
}

// findWindow reports whether the Settings window exists, retrying AX errors
// for up to within.
func (s *liveSettingsSteps) findWindow(ctx context.Context, within time.Duration) (*axNode, error) {
	var lastErr error
	for deadline := time.Now().Add(within); ; {
		win, release, err := s.settingsWindow()
		release()
		if err == nil {
			return win, nil
		}
		lastErr = err
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, lastErr
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *liveSettingsSteps) openFromTray(ctx context.Context) error {
	extras, release, err := axExtras(s.e.RelayPID)
	if err != nil {
		return err
	}
	var item *axNode
	for _, n := range flatten(extras) {
		if n.Role == "AXMenuBarItem" && n.Help == "Relay" {
			item = n
		}
	}
	if item == nil {
		release()
		return errors.New(`tray: no item with help "Relay"`)
	}
	// A status item opens its menu and may still report the press as
	// unanswered.
	if err := item.press(); err != nil && !errors.Is(err, errAXCannotComplete) {
		release()
		return err
	}
	release()
	menu, release, err := pollNode(ctx, 2*time.Second, func() (*axNode, func(), error) {
		root, release, err := axExtras(s.e.RelayPID)
		if err != nil {
			return nil, release, err
		}
		return findNode(root, func(n *axNode) bool { return n.Role == "AXMenuItem" && n.Label == "Settings..." }, release)
	})
	if err != nil {
		return err
	}
	if menu == nil {
		return errors.New(`tray: no "Settings..." item`)
	}
	defer release()
	if err := menu.press(); err != nil {
		return err
	}
	win, release2, err := pollNode(ctx, 10*time.Second, func() (*axNode, func(), error) {
		w, release, err := s.settingsWindow()
		return w, release, err
	})
	if err != nil {
		return err
	}
	release2()
	if win == nil {
		return fmt.Errorf("no %q window within 10 s", settingsWindowTitle)
	}
	return nil
}

// pressServicesTab waits for the tab, because a window that just opened has
// not loaded its page yet.
func (s *liveSettingsSteps) pressServicesTab(ctx context.Context) error {
	tab, release, err := pollNode(ctx, 5*time.Second, func() (*axNode, func(), error) {
		win, release, err := s.settingsWindow()
		if err != nil || win == nil {
			return nil, release, err
		}
		return findNode(win, func(n *axNode) bool { return n.Role == "AXRadioButton" && n.Label == "Services" }, release)
	})
	if err != nil {
		return err
	}
	defer release()
	if tab == nil {
		return errors.New("no Services tab")
	}
	return tab.press()
}

func findNodeIn(root *axNode, match func(*axNode) bool) *axNode {
	for _, n := range flatten(root) {
		if match(n) {
			return n
		}
	}
	return nil
}

// findNode returns the matching node and its release, or releases at once
// when nothing matches.
func findNode(root *axNode, match func(*axNode) bool, release func()) (*axNode, func(), error) {
	if n := findNodeIn(root, match); n != nil {
		return n, release, nil
	}
	release()
	return nil, func() {}, nil
}

// pollNode retries find every 200 ms until it returns a node or the time is
// up. A live page is re-rendered under the walk, so an AX error is retried
// too; the last one is returned when no node turned up. A miss returns a nil
// node and no error.
func pollNode(ctx context.Context, within time.Duration, find func() (*axNode, func(), error)) (*axNode, func(), error) {
	var lastErr error
	for deadline := time.Now().Add(within); ; {
		n, release, err := find()
		if n != nil {
			return n, release, nil
		}
		release()
		if err != nil {
			lastErr = err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, func() {}, lastErr
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// card reads the fixture's card from a fresh snapshot, so the controls it
// returns are live.
func (s *liveSettingsSteps) card() (cardView, *axNode, func(), error) {
	win, release, err := s.settingsWindow()
	if err != nil {
		return cardView{}, nil, release, err
	}
	if win == nil {
		return cardView{}, nil, release, fmt.Errorf("no %q window", settingsWindowTitle)
	}
	view, btn, err := findServiceCard(flatten(win), s.displayName())
	if err != nil {
		release()
		return cardView{}, nil, func() {}, err
	}
	return view, btn, release, nil
}

func (s *liveSettingsSteps) Card(ctx context.Context, within time.Duration, done func(cardView) bool) (cardView, error) {
	var last cardView
	var lastErr error
	for deadline := time.Now().Add(within); ; {
		view, _, release, err := s.card()
		release()
		switch {
		case err != nil:
			lastErr = err
		default:
			last, lastErr = view, nil
			if done(view) {
				return view, nil
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return last, lastErr
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *liveSettingsSteps) Press(ctx context.Context, label string) error {
	view, btn, release, err := s.card()
	defer release()
	if err != nil {
		return err
	}
	if view.Button != label {
		return fmt.Errorf("the card's button reads %q, want %q", view.Button, label)
	}
	return btn.press()
}

func (s *liveSettingsSteps) Service(ctx context.Context, within time.Duration, done func(svcView) bool) (svcView, bool) {
	return pollService(ctx, s.e, s.svcID, crashPIDPattern(s.svcID), within, done, nil)
}

func (s *liveSettingsSteps) Restore(ctx context.Context) error {
	var errs []error
	if s.token != "" {
		if r := serviceAction(ctx, s.e, s.token, s.svcID, "stop"); r.Status != http.StatusOK {
			errs = append(errs, fmt.Errorf("stop answered %d", r.Status))
		}
	}
	if s.opened {
		win, release, err := s.settingsWindow()
		defer release()
		switch {
		case err != nil:
			errs = append(errs, err)
		case win != nil:
			closeBtn := findNodeIn(win, func(n *axNode) bool { return n.Subrole == "AXCloseButton" })
			if closeBtn == nil {
				errs = append(errs, errors.New("no close button on the window"))
			} else if err := closeBtn.press(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
