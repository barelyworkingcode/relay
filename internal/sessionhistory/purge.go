package sessionhistory

import (
	"context"
	"log/slog"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sessions/ledger"
)

// PurgeDeps is what the purge pass asks of relay. A nil field fails closed:
// nothing is purged.
type PurgeDeps struct {
	// Grace is how long a record stays after its transcript goes missing; 0 is off.
	Grace func() time.Duration
	// AuditIntent writes the pending session_purge row durably and returns its
	// id; grace is the period the pass decided with. An error purges nothing.
	AuditIntent func(r ledger.Record, grace time.Duration) (id string, err error)
	// AuditCompletion writes the row that closes an intent: outcome ok, error
	// or denied, with a code for the last two.
	AuditCompletion func(id, outcome, code string)
	// Auditing reports whether the audit log can take a durable write.
	Auditing func() bool
}

// purge sets, clears and acts on the missing mark of one machine's terminal and
// discovered records after a scan that answered. It reads absence only through
// Listing.Absent, so a capped, stale, out-of-scope or refused listing leaves a
// record untouched. Deliberate: any unreadable guard input (held set, managed
// list) stops the whole pass, because a record could then be one a live
// process still holds.
func (x *Index) purge(ctx context.Context, m Machine, listing Listing) {
	p := x.o.Purge
	if p.Grace == nil || p.AuditIntent == nil || p.AuditCompletion == nil || p.Auditing == nil || x.o.Ledger == nil || x.o.Managed == nil {
		return
	}
	grace := p.Grace()
	if grace <= 0 || !p.Auditing() || !listing.Answered {
		return
	}
	v := x.view()
	var candidates []ledger.Record
	for _, r := range x.o.Ledger.All() {
		if purgeCandidate(r) && x.recordMachine(v, r) == m.Key {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return
	}
	// An answered listing with no transcript at all while records exist reads
	// as a store that went away, not as every conversation deleted.
	if len(listing.Present) == 0 || v.Held == nil {
		return
	}
	held, ok := v.Held(m.Key)
	if !ok {
		return
	}
	managed, err := x.o.Managed(ctx)
	if err != nil {
		slog.Warn("session history: managed read failed, nothing purged", "machine", m.Key, "error", err)
		return
	}
	keeps := func(r ledger.Record) bool {
		return r.State == ledger.StateLive || r.AdoptPending != nil || held[r.ConversationID] || managed[r.ConversationID]
	}
	guard := func(r ledger.Record) bool { return purgeCandidate(r) && !keeps(r) && r.MissingSince != nil }

	now := x.o.Clock.Now()
	x.purgeMu.Lock()
	retry := make(map[string]time.Time, len(x.purgeRetry))
	for id, until := range x.purgeRetry {
		if now.Before(until) {
			retry[id] = until
		}
	}
	x.purgeRetry = retry
	x.purgeMu.Unlock()
	var back, gone []string
	var due []ledger.Record
	for _, r := range candidates {
		if _, wait := retry[r.SessionID]; wait {
			continue
		}
		switch {
		case keeps(r):
		case r.ConversationID != "" && listing.Has("claude", r.ConversationID):
			if r.MissingSince != nil {
				back = append(back, r.SessionID)
			}
		case !listing.Absent("claude", r.ConversationID, r.Directory):
			// Deliberate: an unknown stretch breaks "stayed absent", so the mark
			// goes and the grace period starts again.
			if r.MissingSince != nil {
				back = append(back, r.SessionID)
			}
		case r.MissingSince == nil:
			gone = append(gone, r.SessionID)
		case now.Sub(*r.MissingSince) >= grace:
			due = append(due, r)
		}
	}
	if len(back) > 0 {
		if _, err := x.o.Ledger.SetMissing(back, nil); err != nil {
			slog.Warn("session history: missing mark not cleared", "machine", m.Key, "error", err)
		}
	}
	if len(gone) > 0 {
		if _, err := x.o.Ledger.SetMissing(gone, &now); err != nil {
			slog.Warn("session history: missing mark not saved", "machine", m.Key, "error", err)
		}
	}
	for _, r := range due {
		ev := logging.BeginEvent(ctx, "session.history.purge").Set("session_id", r.SessionID)
		id, err := p.AuditIntent(r, grace)
		if err != nil {
			slog.Warn("session history: purge intent not written, record kept", "session", r.SessionID, "error", err)
			ev.End(logging.OutcomeError, "audit_failed", err)
			continue
		}
		removed, err := x.o.Ledger.RemoveIfPurgeable(r.SessionID, guard)
		switch {
		case err != nil:
			p.AuditCompletion(id, "error", "ledger_save_failed")
			// Deliberate: a save that failed once is not retried every scan; the
			// record waits one more grace period on the injected clock.
			x.purgeMu.Lock()
			x.purgeRetry[r.SessionID] = now.Add(grace)
			x.purgeMu.Unlock()
			ev.End(logging.OutcomeError, "ledger_save_failed", err)
		case !removed:
			p.AuditCompletion(id, "denied", "guard_refused")
			ev.End(logging.OutcomeDenied, "guard_refused", nil)
		default:
			p.AuditCompletion(id, "ok", "")
			ev.End(logging.OutcomeOK, "", nil)
		}
	}
}

// purgeCandidate is a launched Claude terminal record or a discovered Claude
// record: the only records a purge may remove. A launched agent record stays.
func purgeCandidate(r ledger.Record) bool {
	return r.Agent == "claude" && (r.Kind == "pty" || r.State == ledger.StateDiscovered)
}
