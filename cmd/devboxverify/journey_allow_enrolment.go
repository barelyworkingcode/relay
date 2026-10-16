package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

const (
	allowEnrolmentID    = "tray-allow-enrolment"
	allowEnrolmentItem  = "Allow Enrolment…"
	cancelEnrolmentItem = "Cancel Enrolment"
	enrolmentLinePrefix = "Enrolment open: Home|Work browser · "
	enrolmentLineSuffix = " left"
	allowEnrolmentWait  = 10 * time.Second
)

// retiredTrayItems are the two items Allow Enrolment replaced; neither may
// remain in the menu.
var retiredTrayItems = []string{"Show Login Code...", "Allow Home|Work Passkey Enrolment…"}

// runAllowEnrolment is the simple path an operator takes: the tray shows one
// Allow Enrolment… item, its sheet offers Home|Work browser, the prompt reads
// the full reason, the tray counts the window down, and Cancel Enrolment
// closes it.
func runAllowEnrolment(ctx context.Context, e env) result {
	const id = allowEnrolmentID
	fail := func(d string) result { return result{id, stateFail, d} }
	s := &allowEnrolmentSteps{liveSettingsSteps: liveSettingsSteps{e: e}}
	if !s.Trusted() {
		return blocked(id, "no Accessibility trust for this process; run devboxverify from a desktop Terminal")
	}
	token, res, ok := runCredential(e, id)
	if !ok {
		return res
	}
	if open, err := eveWindowOpen(ctx, e, token); err != nil {
		return blocked(id, err.Error())
	} else if open {
		return blocked(id, "the Home|Work enrolment window was already open")
	}

	// The window must never outlive the journey, whatever else happened, and a
	// Settings window the journey opened is closed again.
	defer func() {
		bg := context.WithoutCancel(ctx)
		if open, err := eveWindowOpen(bg, e, token); err != nil || open {
			consumeEveWindow(bg, e, token)
		}
		_ = s.Restore(bg)
	}()

	// One Allow Enrolment… item, neither retired item; then press it.
	if err := s.pressTrayItem(ctx, func(labels []string) error {
		n := 0
		for _, l := range labels {
			if l == allowEnrolmentItem {
				n++
			}
			for _, old := range retiredTrayItems {
				if l == old {
					return fmt.Errorf("the tray still has %q", old)
				}
			}
		}
		if n != 1 {
			return fmt.Errorf("the tray has %d %q items, want 1", n, allowEnrolmentItem)
		}
		return nil
	}, func(label string) bool { return label == allowEnrolmentItem }); err != nil {
		return stepResult(id, err)
	}

	// The sheet's three targets.
	if err := s.waitSheet(ctx, "Home|Work browser", "Relay sign-in", "Access profile"); err != nil {
		return stepResult(id, err)
	}
	if err := s.pressSheet(ctx, "AXRadioButton", "Home|Work browser"); err != nil {
		return stepResult(id, err)
	}

	// Open, and answer the prompt with the full reason on it.
	since := time.Now().Add(-time.Second)
	wait, err := startDialog(ctx, e, dialogAnswer, eveOpenReason, dialogTimeout)
	if err != nil {
		return blocked(id, "presence helper not ready: "+notStarted(wait, err).Detail)
	}
	if err := s.pressSheet(ctx, "AXButton", "Open"); err != nil {
		wait()
		return stepResult(id, err)
	}
	if res, refused := positiveRefusal(id, "", wait()); refused {
		return res
	}

	// Waits: the eve.enrolment.open event. The tray menu has no hook, so the
	// countdown line is polled within its own timeout after the event.
	if err := pollCheck(ctx, allowEnrolmentWait, func() (bool, error) { return eveOpenEvent(ctx, e, since) }); err != nil {
		return fail("no eve.enrolment.open event with status ok after the prompt was answered: " + err.Error())
	}
	if open, err := eveWindowOpen(ctx, e, token); err != nil {
		return blocked(id, err.Error())
	} else if !open {
		return fail("the prompt was answered and the event is ok, but the Home|Work window reads closed")
	}

	line := ""
	if err := s.pressTrayItem(ctx, func(labels []string) error {
		for _, l := range labels {
			if strings.HasPrefix(l, enrolmentLinePrefix) && strings.HasSuffix(l, enrolmentLineSuffix) {
				line = l
				return nil
			}
		}
		return fmt.Errorf("the tray shows no %q line", enrolmentLinePrefix+"m:ss"+enrolmentLineSuffix)
	}, func(label string) bool { return label == cancelEnrolmentItem }); err != nil {
		return stepResult(id, err)
	}

	// Cancel Enrolment was pressed: the window reads closed and consume is
	// refused, which is what eve's own login screen sees.
	if err := pollCheck(ctx, allowEnrolmentWait, func() (bool, error) {
		open, err := eveWindowOpen(ctx, e, token)
		return !open, err
	}); err != nil {
		return fail("the Home|Work window still reads open after Cancel Enrolment: " + err.Error())
	}
	if status := consumeEveWindow(ctx, e, token); status != http.StatusConflict {
		return fail(fmt.Sprintf("consume answered %d after Cancel Enrolment, want 409", status))
	}
	return result{id, statePass, fmt.Sprintf("one %s item and neither retired item; the sheet offered three targets; the prompt read the full reason; the tray showed %q; Cancel Enrolment closed the window and consume answered 409", allowEnrolmentItem, line)}
}

// stepResult maps a step that could not be driven: an Accessibility driver
// error is BLOCKED, anything else is what the journey found.
func stepResult(id string, err error) result {
	if errors.Is(err, errAXDriver) {
		return blocked(id, err.Error())
	}
	return result{id, stateFail, err.Error()}
}

// pollUntil retries done every 200 ms until it reports true, errors, or the
// time is up.
func pollCheck(ctx context.Context, within time.Duration, done func() (bool, error)) error {
	var lastErr error
	for deadline := time.Now().Add(within); ; time.Sleep(200 * time.Millisecond) {
		ok, err := done()
		if ok {
			return nil
		}
		lastErr = err
		if time.Now().After(deadline) || ctx.Err() != nil {
			if lastErr == nil {
				lastErr = fmt.Errorf("not within %s", within)
			}
			return lastErr
		}
	}
}

// eveOpenEvent reports whether relay logged an ok eve.enrolment.open since
// the given time.
func eveOpenEvent(ctx context.Context, e env, since time.Time) (bool, error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, "logs", "--event", "eve.enrolment.open", "--json").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("relay logs: %w", err)
	}
	lines, err := jsonLines(string(out))
	if err != nil {
		return false, err
	}
	for _, m := range lines {
		ts, _ := m["ts"].(string)
		at, perr := time.Parse(time.RFC3339Nano, ts)
		if m["status"] == "ok" && perr == nil && !at.Before(since) {
			return true, nil
		}
	}
	return false, nil
}

type allowEnrolmentSteps struct {
	liveSettingsSteps
}

// pressTrayItem opens the tray menu, lets check read every item's label, and
// presses the first item whose label pick accepts. A check that errs closes the
// menu again.
func (s *allowEnrolmentSteps) pressTrayItem(ctx context.Context, check func(labels []string) error, pick func(label string) bool) error {
	var bar *axNode
	var release func()
	err := retryInvalidElement(ctx, func() error {
		extras, rel, err := axExtras(s.e.RelayPID)
		if err != nil {
			return err
		}
		bar = findNodeIn(extras, func(n *axNode) bool { return n.Role == "AXMenuBarItem" && n.Help == "Relay" })
		release = rel
		return nil
	})
	if err != nil {
		return err
	}
	if bar == nil {
		release()
		return errors.New(`tray: no item with help "Relay"`)
	}
	// A status item opens its menu and may still report the press as unanswered.
	if err := bar.press(); err != nil && !errors.Is(err, errAXCannotComplete) {
		release()
		return err
	}
	release()

	menu, release, err := pollNode(ctx, allowEnrolmentWait, func() (*axNode, func(), error) {
		root, release, err := axExtras(s.e.RelayPID)
		if err != nil {
			return nil, release, err
		}
		return findNode(root, func(n *axNode) bool {
			return n.Role == "AXMenu" && findNodeIn(n, func(c *axNode) bool { return c.Role == "AXMenuItem" && c.Label == "Quit Relay" }) != nil
		}, release)
	})
	if err != nil {
		return err
	}
	if menu == nil {
		return errors.New("tray: the menu did not open")
	}
	defer release()
	var labels []string
	var target *axNode
	for _, n := range flatten(menu) {
		if n.Role != "AXMenuItem" {
			continue
		}
		labels = append(labels, n.Label)
		if target == nil && pick(n.Label) {
			target = n
		}
	}
	if err := check(labels); err != nil {
		s.toggleTrayMenu(ctx)
		return err
	}
	if target == nil {
		s.toggleTrayMenu(ctx)
		return errors.New("tray: the item to press is not in the menu")
	}
	return target.press()
}

// toggleTrayMenu presses the status item once more so a menu the journey
// opened and will not use does not stay open under the next journey. Best
// effort: the caller already has the error it will report.
func (s *allowEnrolmentSteps) toggleTrayMenu(ctx context.Context) {
	_ = retryInvalidElement(ctx, func() error {
		extras, release, err := axExtras(s.e.RelayPID)
		if err != nil {
			return err
		}
		defer release()
		if bar := findNodeIn(extras, func(n *axNode) bool { return n.Role == "AXMenuBarItem" && n.Help == "Relay" }); bar != nil {
			if err := bar.press(); err != nil && !errors.Is(err, errAXCannotComplete) {
				return err
			}
		}
		return nil
	})
}

// sheetNode finds a control in the Settings window by role and a label that
// contains text, ignoring case: CSS-styled text reads differently to the
// accessibility tree than in the source.
func (s *allowEnrolmentSteps) sheetNode(role, text string) (*axNode, func(), error) {
	win, release, err := s.settingsWindow()
	if err != nil || win == nil {
		return nil, release, err
	}
	return findNode(win, func(n *axNode) bool {
		return n.Role == role && strings.Contains(strings.ToLower(normText(n.Label)), strings.ToLower(text))
	}, release)
}

// waitSheet waits for the sheet to show a radio for every label. A window that
// just opened has not loaded its page yet.
func (s *allowEnrolmentSteps) waitSheet(ctx context.Context, labels ...string) error {
	s.opened = true
	for _, label := range labels {
		n, release, err := pollNode(ctx, allowEnrolmentWait, func() (*axNode, func(), error) { return s.sheetNode("AXRadioButton", label) })
		if err != nil {
			return err
		}
		release()
		if n == nil {
			return fmt.Errorf("the Allow Enrolment sheet has no %q choice within %s", label, allowEnrolmentWait)
		}
	}
	return nil
}

func (s *allowEnrolmentSteps) pressSheet(ctx context.Context, role, label string) error {
	return retryInvalidElement(ctx, func() error {
		n, release, err := s.sheetNode(role, label)
		if err != nil {
			return err
		}
		defer release()
		if n == nil {
			return fmt.Errorf("the sheet has no %s %q", role, label)
		}
		return n.press()
	})
}
