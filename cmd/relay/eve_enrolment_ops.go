package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// eveEnrolmentTTL is docs/eve-passkey-enrolment.md's five-minute window:
// long enough for the operator to walk to a second device and tap "Add this
// browser", short enough that an open window does not linger forgotten.
const eveEnrolmentTTL = 5 * time.Minute

// errEveEnrolmentClosed is Consume's one refusal — no window is open, it
// expired, or another browser already took it. The three are not
// distinguished: eve's own login screen shows the same "not open" message
// either way (docs/eve-passkey-enrolment.md), and the console learns which
// one happened from the notification and `relay audit`, not from this
// error.
var errEveEnrolmentClosed = errors.New("eve passkey enrolment is not open")

// eveEnrolmentOpen reports whether w is a live window: a record exists and
// its expiry both parses and has not passed. An Expires relay cannot parse
// reads as expired — the same rule consumeBootstrapCode's own comment gives
// for LoginBootstrap, applied here so Status(), Consume and the tray's
// countdown line can never disagree about whether the window is still open.
func eveEnrolmentOpen(w *config.EveEnrolmentWindow, now time.Time) bool {
	if w == nil {
		return false
	}
	at, err := time.Parse(time.RFC3339, w.Expires)
	if err != nil {
		return false
	}
	return now.Before(at)
}

// mintEveEnrolment writes a fresh window, replacing any existing one — at
// most one at a time, matching login_ops.go's mintBootstrapCode. Does not
// save; use within store.With.
func mintEveEnrolment(s *config.Settings) string {
	expires := time.Now().UTC().Add(eveEnrolmentTTL).Format(time.RFC3339)
	s.EveEnrolment = &config.EveEnrolmentWindow{Expires: expires}
	return expires
}

// eveEnrolmentClaim is what a consuming browser supplies — the source IP and
// the enrolling browser's User-Agent — matching
// docs/eve-passkey-enrolment.md's POST .../consume body exactly.
type eveEnrolmentClaim struct {
	IP    string `json:"ip"`
	Label string `json:"label"`
}

// eveEnrolmentStatusView is GET /api/eve/passkey-enrolment's body, and
// Open's own return shape: `{"open": true, "expires": "…"}` or
// `{"open": false}`.
type eveEnrolmentStatusView struct {
	Open    bool   `json:"open"`
	Expires string `json:"expires,omitempty"`
}

// eveEnrolmentConsumedView is POST .../consume's 200 body.
type eveEnrolmentConsumedView struct {
	Expires string `json:"expires"`
}

// EveEnrolmentOps is the second-browser counterpart to LoginOps, the same
// Store/Audit/Gate/OnChange shape (ADR-016). Only Open is gated: Status is a
// read, and Consume narrows a window the operator already opened at the
// console rather than widening anything, so neither asks a human to confirm
// twice.
type EveEnrolmentOps struct {
	Store config.SettingsStore
	// Audit records Open's issuance and Consume's own record. Nil-safe like
	// every audit.AuditRecorder method, matching LoginOps.Audit.
	Audit *audit.AuditRecorder
	// Gate is the presence check Open demands before it touches the store.
	// A nil Gate refuses — see requireGate.
	Gate     *presence.Gate
	OnChange func()
	// Notify raises the tray's console banners for Open and Consume
	// (docs/eve-passkey-enrolment.md: "Relay notifies the console..."). Nil
	// is safe and raises nothing.
	Notify func(title, body string)
}

func (o *EveEnrolmentOps) auditor() IssuanceAuditor {
	if o == nil {
		return nil
	}
	return issuanceAuditorOrNil(o.Audit)
}

var errEveEnrolmentOpsUnavailable = errors.New("eve enrolment management is unavailable in this relay process")

func (o *EveEnrolmentOps) notify() {
	if o.OnChange != nil {
		o.OnChange()
	}
}

func (o *EveEnrolmentOps) notifyConsole(title, body string) {
	if o != nil && o.Notify != nil {
		o.Notify(title, body)
	}
}

// Open mints a fresh window, replacing any existing one, and records the
// issuance — MintBootstrap's own shape, including withholding the result
// when it cannot be recorded (the window is already persisted at that
// point, exactly as an unshown bootstrap code still expires on its own).
func (o *EveEnrolmentOps) Open(ctx context.Context, via string) (eveEnrolmentStatusView, error) {
	if o == nil {
		return eveEnrolmentStatusView{}, errEveEnrolmentOpsUnavailable
	}
	if err := requireIssuanceAuditor(o.auditor()); err != nil {
		return eveEnrolmentStatusView{}, err
	}
	// The reason string is inlined rather than named, matching
	// login_ops.go's own "mint a login bootstrap code": it is quoted
	// verbatim in docs/eve-passkey-enrolment.md and docs/presence-gate.md,
	// and doc_reason_strings_test.go's AST scan resolves a requireGate
	// reason argument to a constant string literal or a local
	// assignment -- not to a package-level const identifier -- so a named
	// constant here would silently drop out of that guard's coverage.
	grant, err := requireGate(o.Gate, ctx, "eve.enrolment.open",
		presence.NewDigestBuilder("eve.enrolment.open").Build(),
		"open a five-minute window for one new browser to register an Eve passkey")
	if err != nil {
		return eveEnrolmentStatusView{}, err
	}
	var expires string
	if err := o.Store.With(func(s *config.Settings) {
		expires = mintEveEnrolment(s)
	}); err != nil {
		return eveEnrolmentStatusView{}, fmt.Errorf("save settings: %w", err)
	}
	if err := recordEveEnrolmentIssued(o.auditor(), expires, via, grant.ID()); err != nil {
		return eveEnrolmentStatusView{}, fmt.Errorf("an eve passkey enrolment window was opened but could not be recorded in the audit log, so it was not shown: %w", err)
	}
	o.notify()
	o.notifyConsole("Relay", "Eve passkey enrolment open for 5 minutes")
	return eveEnrolmentStatusView{Open: true, Expires: expires}, nil
}

// Status reports whether a window is currently open. Not gated: it discloses
// nothing beyond "is the console currently letting a browser in", the same
// fact eve's own login screen shows a stranger who watches it poll.
func (o *EveEnrolmentOps) Status() eveEnrolmentStatusView {
	if o == nil || o.Store == nil {
		return eveEnrolmentStatusView{}
	}
	w := o.Store.Get().EveEnrolment
	if !eveEnrolmentOpen(w, time.Now()) {
		return eveEnrolmentStatusView{}
	}
	return eveEnrolmentStatusView{Open: true, Expires: w.Expires}
}

// Consume spends an open window atomically: reads and clears
// s.EveEnrolment inside one store.With, matching consumeBootstrapCode's own
// reasoning — resolving and deleting in two separate calls would race a
// second browser's Consume between them.
//
// Deliberately ungated and not behind requireIssuanceAuditor: the operator
// already answered a presence prompt to open the window, and consuming it
// narrows what a caller reaches rather than widening it, the same footing
// ADR-018 step 3 gives every other retirement. The audit record below is
// best-effort, like recordHostProbe's and recordPasskeyRevoked's own
// post-mutation records — a failing log must not un-spend the slot the
// winning browser just took.
func (o *EveEnrolmentOps) Consume(ctx context.Context, claim eveEnrolmentClaim) (eveEnrolmentConsumedView, error) {
	if o == nil {
		return eveEnrolmentConsumedView{}, errEveEnrolmentOpsUnavailable
	}
	var expires string
	var open bool
	if err := o.Store.With(func(s *config.Settings) {
		open = eveEnrolmentOpen(s.EveEnrolment, time.Now())
		if open {
			expires = s.EveEnrolment.Expires
			s.EveEnrolment = nil
		}
	}); err != nil {
		return eveEnrolmentConsumedView{}, fmt.Errorf("save settings: %w", err)
	}
	if !open {
		return eveEnrolmentConsumedView{}, errEveEnrolmentClosed
	}
	if err := recordEveEnrolmentConsumed(o.auditor(), claim, auditViaHTTP); err != nil {
		slog.Error("eve passkey enrolment consumed but not recorded in the audit log", "ip", claim.IP, "error", err)
	}
	o.notify()
	o.notifyConsole("Relay", fmt.Sprintf("Eve: a new browser registered a passkey (from %s)", claim.IP))
	return eveEnrolmentConsumedView{Expires: expires}, nil
}

// eveEnrolmentRemaining formats the time left in an open window as the
// tray's disabled menu line shows it ("4m 12s left"), or reports open=false
// once the window has closed — eveEnrolmentOpen's own rule, so the menu line
// and Status() can never disagree.
func eveEnrolmentRemaining(w *config.EveEnrolmentWindow, now time.Time) (remaining string, open bool) {
	if !eveEnrolmentOpen(w, now) {
		return "", false
	}
	at, _ := time.Parse(time.RFC3339, w.Expires)
	d := at.Sub(now).Round(time.Second)
	return fmt.Sprintf("%dm %02ds left", int(d.Minutes()), int(d.Seconds())%60), true
}
