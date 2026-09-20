package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/presence"
)

var errEvePasskeyOpsUnavailable = errors.New("eve passkey management is unavailable in this relay process")

// errEvePasskeyUnknown, errEvePasskeyAlreadyPending and errEvePasskeyLast are
// Revoke's three pre-gate refusals (docs/eve-passkey-enrolment.md decision
// 13): unlike login.passkey.revoke's uniform "not found" (there is no
// self-service ceremony an oracle could help here), each is told apart
// because an operator staring at the tab needs to know which is true.
var (
	errEvePasskeyUnknown        = errors.New("no eve passkey found")
	errEvePasskeyAlreadyPending = errors.New("that eve passkey's revocation is already pending")
	errEvePasskeyLast           = errors.New("the last Eve passkey cannot be revoked from relay; enrol another browser first, or delete eve's auth.json on the console to start over")
)

// evePasskeyReportEntry is one credential eve reports about itself, matching
// PUT /api/eve/passkeys' per-entry body shape exactly. No public key, no
// counter -- eve keeps those, and this mirror has no legitimate use for
// them (decision 8).
type evePasskeyReportEntry struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Created  string `json:"created"`
	LastUsed string `json:"last_used"`
}

// evePasskeysReportRequest is PUT /api/eve/passkeys' whole body.
type evePasskeysReportRequest struct {
	Passkeys []evePasskeyReportEntry `json:"passkeys"`
}

// evePasskeyRevocationsView is both GET .../revocations' body and PUT
// .../passkeys' 200 response: in each case just the pending id set.
type evePasskeyRevocationsView struct {
	Revocations []string `json:"revocations"`
}

// evePasskeyView is what the Passkeys tab and `relay eve list` are told
// about one mirrored credential -- Short rides along for the same reason
// passkeyView carries it, and RevocationPending is derived rather than
// stored, so it can never disagree with EvePasskeyRevocations.
type evePasskeyView struct {
	ID                string `json:"id"`
	Short             string `json:"short"`
	Label             string `json:"label"`
	Created           string `json:"created"`
	LastUsed          string `json:"last_used"`
	Reported          string `json:"reported"`
	RevocationPending bool   `json:"revocation_pending"`
}

// evePasskeyPendingSet indexes s.EvePasskeyRevocations by id.
func evePasskeyPendingSet(s *config.Settings) map[string]bool {
	pending := make(map[string]bool, len(s.EvePasskeyRevocations))
	for _, r := range s.EvePasskeyRevocations {
		pending[r.ID] = true
	}
	return pending
}

// evePasskeyViews and evePasskeyRevocationIDs are free functions over
// *Settings so the first paint (renderSettingsDocument) and every IPC/HTTP
// door share one definition, the same discipline passkeyViews follows for
// relay's own Passkeys tab.
func evePasskeyViews(s *config.Settings) []evePasskeyView {
	pending := evePasskeyPendingSet(s)
	out := make([]evePasskeyView, 0, len(s.EvePasskeys))
	for _, p := range s.EvePasskeys {
		out = append(out, evePasskeyView{
			ID:                p.ID,
			Short:             abbreviatePasskeyID(p.ID),
			Label:             p.Label,
			Created:           p.Created,
			LastUsed:          p.LastUsed,
			Reported:          p.Reported,
			RevocationPending: pending[p.ID],
		})
	}
	return out
}

func evePasskeyRevocationIDs(s *config.Settings) []string {
	out := make([]string, 0, len(s.EvePasskeyRevocations))
	for _, r := range s.EvePasskeyRevocations {
		out = append(out, r.ID)
	}
	return out
}

// evePasskeyRevocable is Revoke's whole refusal logic, called once before
// the gate (so an operator is never asked to authenticate for a revoke that
// was always going to fail) and once more inside the store.With that
// performs it (so a concurrent revoke of every other credential cannot slip
// this one past the last-credential rule between the two checks).
func evePasskeyRevocable(s *config.Settings, id string) error {
	pending := evePasskeyPendingSet(s)
	var found bool
	nonPending := 0
	for _, p := range s.EvePasskeys {
		if p.ID == id {
			found = true
		}
		if !pending[p.ID] {
			nonPending++
		}
	}
	if !found {
		return fmt.Errorf("%w with id %q", errEvePasskeyUnknown, id)
	}
	if pending[id] {
		return errEvePasskeyAlreadyPending
	}
	// id is confirmed non-pending above, so nonPending <= 1 here means id
	// is the ONLY non-pending credential left -- revoking it would empty
	// the mirror (decision 13).
	if nonPending <= 1 {
		return errEvePasskeyLast
	}
	return nil
}

// EvePasskeyOps is relay's core for the mirror and its pending revocations
// (docs/eve-passkey-enrolment.md decisions 8-13) -- the same
// Store/Audit/Gate shape as EveEnrolmentOps and LoginOps. Only
// Revoke is gated; Report, List and Revocations are reads or narrowings eve
// itself drives, and Unrevoke narrows nothing a caller widened.
type EvePasskeyOps struct {
	Store config.SettingsStore
	Queue *config.CommandQueue
	Audit *audit.AuditRecorder
	Gate  *presence.Gate
	// Notify raises the tray's console banners for Revoke (and, from the
	// report path, for a revocation eve confirms it applied). Nil is safe
	// and raises nothing.
	Notify func(title, body string)
}

func (o *EvePasskeyOps) runQueued(ctx context.Context, fn func() error) error {
	if o.Queue == nil {
		return fn()
	}
	return o.Queue.DoCommitted(ctx, func(context.Context) error { return fn() })
}

func (o *EvePasskeyOps) auditor() IssuanceAuditor {
	if o == nil {
		return nil
	}
	return issuanceAuditorOrNil(o.Audit)
}

func (o *EvePasskeyOps) notifyConsole(title, body string) {
	if o != nil && o.Notify != nil {
		o.Notify(title, body)
	}
}

// Report replaces the mirror wholesale and stamps Reported on every entry,
// then drops every pending revocation whose id no longer appears in list or
// whose id is the list's only entry (decisions 12 and 13: a report is both
// "here is my list" and "I did what you asked", and relay never lets its own
// pending set outlive what would empty eve's last passkey).
func (o *EvePasskeyOps) Report(list []evePasskeyReportEntry) error {
	if o == nil {
		return errEvePasskeyOpsUnavailable
	}
	reported := time.Now().UTC().Format(time.RFC3339)
	present := make(map[string]bool, len(list))
	for _, e := range list {
		present[e.ID] = true
	}
	lastStanding := len(list) == 1
	if err := o.runQueued(context.Background(), func() error {
		if err := o.Store.With(func(s *config.Settings) {
			out := make([]config.EvePasskey, 0, len(list))
			for _, e := range list {
				out = append(out, config.EvePasskey{
					ID:       e.ID,
					Label:    e.Label,
					Created:  e.Created,
					LastUsed: e.LastUsed,
					Reported: reported,
				})
			}
			s.EvePasskeys = out
			s.EvePasskeyRevocations = slices.DeleteFunc(s.EvePasskeyRevocations, func(r config.EvePasskeyRevocation) bool {
				return !present[r.ID] || (lastStanding && r.ID == list[0].ID)
			})
		}); err != nil {
			return fmt.Errorf("save settings: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// List is the mirror, each entry told whether its revocation is pending.
func (o *EvePasskeyOps) List() []evePasskeyView {
	if o == nil || o.Store == nil {
		return []evePasskeyView{}
	}
	return evePasskeyViews(config.DisplaySettings(o.Store))
}

// Revocations is the pending id set eve polls and, decisively, checks on
// every login attempt (decision 10).
func (o *EvePasskeyOps) Revocations() []string {
	if o == nil || o.Store == nil {
		return []string{}
	}
	return evePasskeyRevocationIDs(config.FreshSettings(o.Store))
}

// Revoke records a pending revocation. It never touches eve -- it cannot
// (decision 9) -- so its whole effect is appending to
// EvePasskeyRevocations; eve applies it on its own next poll or login
// check.
func (o *EvePasskeyOps) Revoke(ctx context.Context, id, via string) (config.EvePasskeyRevocation, error) {
	if o == nil {
		return config.EvePasskeyRevocation{}, errEvePasskeyOpsUnavailable
	}
	id = strings.TrimSpace(id)
	if err := requireIssuanceAuditor(o.auditor()); err != nil {
		return config.EvePasskeyRevocation{}, err
	}
	if err := evePasskeyRevocable(config.FreshSettings(o.Store), id); err != nil {
		return config.EvePasskeyRevocation{}, err
	}
	grant, err := requireGate(o.Gate, ctx, "eve.passkey.revoke",
		singleStringDigest("eve.passkey.revoke", "id", id),
		fmt.Sprintf("revoke the Eve passkey %s", abbreviatePasskeyID(id)))
	if err != nil {
		return config.EvePasskeyRevocation{}, err
	}
	var rec config.EvePasskeyRevocation
	if err := o.runQueued(ctx, func() error {
		if err := config.WithDeclinable(o.Store, func(s *config.Settings) error {
			if err := evePasskeyRevocable(s, id); err != nil {
				return err
			}
			rec = config.EvePasskeyRevocation{ID: id, Requested: time.Now().UTC().Format(time.RFC3339)}
			s.EvePasskeyRevocations = append(s.EvePasskeyRevocations, rec)
			return nil
		}); err != nil {
			if errors.Is(err, errEvePasskeyUnknown) || errors.Is(err, errEvePasskeyAlreadyPending) || errors.Is(err, errEvePasskeyLast) {
				return err
			}
			return fmt.Errorf("save settings: %w", err)
		}
		// Reported and not refused, the same balance recordPasskeyRevoked
		// strikes: the revocation is already pending, and a failing log must
		// not be the reason it goes unrecorded rather than merely un-narrated.
		if err := recordEvePasskeyRevoked(o.auditor(), id, via, grant.ID()); err != nil {
			slog.Error("eve passkey revocation recorded but not written to the audit log", "id", abbreviatePasskeyID(id), "error", err)
		}
		return nil
	}); err != nil {
		if errors.Is(err, errEvePasskeyUnknown) || errors.Is(err, errEvePasskeyAlreadyPending) || errors.Is(err, errEvePasskeyLast) {
			return config.EvePasskeyRevocation{}, err
		}
		return config.EvePasskeyRevocation{}, err
	}
	o.notifyConsole("Relay", "Eve passkey revoked: it stops working on its next use")
	return rec, nil
}

// Unrevoke withdraws a pending revocation eve has not yet applied.
// Deliberately ungated: it narrows nothing a caller widened, the same
// footing ADR-018 step 3 gives unregistering an MCP.
func (o *EvePasskeyOps) Unrevoke(id string) error {
	if o == nil {
		return errEvePasskeyOpsUnavailable
	}
	id = strings.TrimSpace(id)
	if err := o.runQueued(context.Background(), func() error {
		if err := o.Store.With(func(s *config.Settings) {
			s.EvePasskeyRevocations = slices.DeleteFunc(s.EvePasskeyRevocations, func(r config.EvePasskeyRevocation) bool {
				return r.ID == id
			})
		}); err != nil {
			return fmt.Errorf("save settings: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}
