package main

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	notifyMinInterval = 60 * time.Second
	notifyMaxPerHour  = 6
)

// notificationsDeniedWarning is what a person debugging a missing banner has
// to be able to find. macOS refuses notification authorization outright for
// an ad-hoc-signed, non-notarised LSUIElement bundle — no prompt, no user
// action — and the only other evidence is opening System Settings and
// reading "Allow notifications: Off".
const notificationsDeniedWarning = "user notifications are disabled for Relay in System Settings → Notifications, " +
	"so no banner will be raised when a machine is waiting to be registered; " +
	"the tray menu's \"Pending enrolment requests: N\" line still shows them, " +
	"and so does `relay enrol requests`. Notifications are a per-user setting: " +
	"turn \"Allow notifications\" on for Relay in the account that runs the tray."

var notificationsDeniedReported atomic.Bool

// reportNotificationsDenied records a refused notification authorization,
// once. A denial is a persistent state and not an event — macOS answers
// every later request the same way — so a line per attempt would be noise
// that then needs a suppressor of its own.
//
// Called from Objective-C via goOnNotificationsDenied and from nowhere else;
// detail is the NSError description, empty when the refusal carried none.
func reportNotificationsDenied(detail string) {
	if notificationsDeniedReported.Swap(true) {
		return
	}
	if detail != "" {
		slog.Warn(notificationsDeniedWarning, "error", detail)
		return
	}
	slog.Warn(notificationsDeniedWarning)
}

type pendingEnrolmentNotifier struct {
	now      func() time.Time
	notify   func(title, body string)
	lastGen  uint64
	lastAt   time.Time
	inWindow []time.Time
}

func newPendingEnrolmentNotifier(now func() time.Time, notify func(title, body string)) *pendingEnrolmentNotifier {
	if now == nil {
		now = time.Now
	}
	if notify == nil {
		notify = func(title, body string) {}
	}
	return &pendingEnrolmentNotifier{now: now, notify: notify}
}

// tick raises one notification when a new pending enrolment request has
// arrived and the bounding conditions allow it. It is called from a single
// place on a timer; it is a plain function of its arguments and the struct's
// state.
func (n *pendingEnrolmentNotifier) tick(gen uint64, unapproved, live, maxLive int, settingsOpen bool) {
	now := n.now()

	// Prune the rolling window first so a call an hour after a flood is not
	// blocked by stale entries.
	kept := n.inWindow[:0]
	for _, t := range n.inWindow {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	n.inWindow = kept

	// gen is the one counter that moves only when a new request row is
	// inserted. Re-lodges, polls, refusals and throttles leave it alone, so
	// nothing they do can produce a banner.
	if gen <= n.lastGen {
		return
	}
	// lastGen advances on EVERY new generation, whether or not the rate limit
	// lets this one through. A flood that trips the limit is swallowed, never
	// queued to fire later — doing otherwise turns the cap into a delay.
	n.lastGen = gen

	if unapproved <= 0 {
		return
	}
	if maxLive > 0 && live >= maxLive {
		return
	}
	if settingsOpen {
		return
	}
	if !n.lastAt.IsZero() && now.Sub(n.lastAt) < notifyMinInterval {
		return
	}
	if len(n.inWindow) >= notifyMaxPerHour {
		return
	}

	n.inWindow = append(n.inWindow, now)
	n.lastAt = now
	verb := "machines are waiting"
	if unapproved == 1 {
		verb = "machine is waiting"
	}
	n.notify("Relay", fmt.Sprintf(
		"%d %s to be registered.\nOpen Settings → Remote Clients to compare its code and approve. Nothing is granted until you do.",
		unapproved, verb))
}
