package main

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/relayfs"
	"github.com/hugelgupf/p9/linux"
)

// mountCounters is the running total mount_detach reports — kept separate
// from mountAudit itself so the lock protecting it is obviously scoped to
// just these three numbers, not the whole struct.
type mountCounters struct {
	mu                sync.Mutex
	bytesRead         int64
	bytesWritten      int64
	ops               int64
	throttleRefusals  int64     // every refusal, coalesced or not
	lastThrottleRowAt time.Time // zero until the first throttled row is written
}

// mountAudit implements relayfs.Hooks, translating every hook call into
// internal/audit rows and internal/enrolment budget checks. One value per
// live mount session (one per 9P connection).
type mountAudit struct {
	rec      *audit.AuditRecorder
	actor    audit.AuditActor
	mountID  string
	root     string // the mount's host path, for McpRoot — never sent to the client, audit-only
	rc       bridge.RemoteCaller
	budgets  *enrolment.Budgets
	budget   func() config.EnrolmentBudget // re-read fresh on every call — an operator's `relay enrol update` mid-session must take effect immediately, not at next attach
	counters *mountCounters

	// revalidate re-resolves the mount's grant against CURRENT settings and
	// reports its access mode, or an error if the grant no longer resolves
	// at all (project deleted, mount removed, enrolment no longer grants
	// the project). Set by mount_session.go's serveMount, closing over the
	// live *RemoteServer/fingerprint/projectID/mountID — this package
	// doesn't know about RemoteServer at all, keeping the dependency
	// direction one-way.
	revalidate func() (access string, err error)
}

var _ relayfs.Hooks = (*mountAudit)(nil)

func (m *mountAudit) mcpID() string { return "mount:" + m.mountID }

// marshalArgs turns a Hooks-supplied args map into the same redacted,
// capped, sized form every other audit row's Args already goes through —
// reusing AuditRecorder.RedactCallArgs is what keeps this uniform with the
// tool plane, per the design doc's own instruction.
func (m *mountAudit) marshalArgs(args map[string]any) (json.RawMessage, int, bool) {
	raw, err := json.Marshal(args)
	if err != nil {
		raw = json.RawMessage("{}")
	}
	return m.rec.RedactCallArgs(raw)
}

// AdmitOp implements relayfs.Hooks — called once per 9P request, mutating
// or not.
func (m *mountAudit) AdmitOp() error {
	if err := m.budgets.AdmitMountOp(m.rc, m.budget()); err != nil {
		m.recordThrottled("op", nil)
		return linux.EDQUOT
	}
	m.counters.mu.Lock()
	m.counters.ops++
	m.counters.mu.Unlock()
	return nil
}

func (m *mountAudit) ChargeRead(n int) error {
	if err := m.budgets.AdmitMountRead(m.rc, m.budget(), n); err != nil {
		m.recordThrottled("read", nil)
		return linux.EDQUOT
	}
	m.counters.mu.Lock()
	m.counters.bytesRead += int64(n)
	m.counters.mu.Unlock()
	return nil
}

func (m *mountAudit) ChargeWrite(n int) error {
	if err := m.budgets.AdmitMountWrite(m.rc, m.budget(), n); err != nil {
		m.recordThrottled("write", nil)
		return linux.EDQUOT
	}
	m.counters.mu.Lock()
	m.counters.bytesWritten += int64(n)
	m.counters.mu.Unlock()
	return nil
}

// BeginMutation implements relayfs.Hooks. The errno returned is the errno a
// 9P client sees, so it must already be the right one for the reason this
// refuses — relayfs maps an error only by unwrapping a linux.Errno, and a
// bare Go error would surface as EIO for every case:
//
//   - the grant no longer resolves at all (project deleted, mount removed,
//     enrolment no longer grants the project) -> EACCES
//   - the grant still resolves but has narrowed to read since attach -> EROFS
//   - the audit log could not record the intent row (RecordDurable failed —
//     a full disk, a broken log) -> EIO, the one genuine "a mutation that
//     cannot be recorded is refused" case
func (m *mountAudit) BeginMutation(op string, args map[string]any) (func(error), error) {
	access, err := m.revalidate()
	if err != nil {
		m.recordDenied(op, args, "grant-gone")
		return func(error) {}, linux.EACCES
	}
	if access != config.AccessWrite {
		m.recordDenied(op, args, "narrowed-to-read")
		return func(error) {}, linux.EROFS
	}

	redacted, size, truncated := m.marshalArgs(args)
	id := audit.NewAuditID()
	intent := audit.AuditEvent{
		ID: id, TS: time.Now(), Event: audit.AuditEventMountOp, Phase: audit.AuditPhaseIntent,
		Actor: m.actor, McpID: m.mcpID(), McpRoot: m.root, Tool: op,
		Args: redacted, ArgsBytes: size, ArgsTruncated: truncated,
		Outcome: audit.AuditOutcomePending,
	}
	if err := m.rec.RecordDurable(intent); err != nil {
		return func(error) {}, linux.EIO
	}

	start := time.Now()
	end := func(opErr error) {
		completion := intent
		completion.Phase = audit.AuditPhaseCompletion
		completion.DurMs = time.Since(start).Milliseconds()
		if opErr != nil {
			completion.Outcome = audit.AuditOutcomeError
			completion.Error = opErr.Error()
		} else {
			completion.Outcome = audit.AuditOutcomeOK
		}
		m.rec.Record(completion)
	}
	return end, nil
}

// Refused implements relayfs.Hooks — a single denied row, no intent/
// completion pairing (nothing was attempted).
func (m *mountAudit) Refused(op string, args map[string]any, rule string) {
	m.recordDenied(op, args, rule)
}

func (m *mountAudit) recordDenied(op string, args map[string]any, rule string) {
	redacted, size, truncated := m.marshalArgs(args)
	m.rec.Record(audit.AuditEvent{
		ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventMountOp,
		Actor: m.actor, McpID: m.mcpID(), McpRoot: m.root, Tool: op,
		Args: redacted, ArgsBytes: size, ArgsTruncated: truncated,
		Outcome: audit.AuditOutcomeDenied, Error: rule,
	})
}

// recordThrottled coalesces: the first refusal in a session writes a row
// immediately; further refusals within 60 seconds of the last written row
// write nothing (design doc: "at most one per minute while refusals
// continue"). Every refusal still increments the counter mount_detach
// reports, coalesced or not.
func (m *mountAudit) recordThrottled(op string, args map[string]any) {
	m.counters.mu.Lock()
	m.counters.throttleRefusals++
	fire := m.counters.lastThrottleRowAt.IsZero() || time.Since(m.counters.lastThrottleRowAt) >= time.Minute
	if fire {
		m.counters.lastThrottleRowAt = time.Now()
	}
	m.counters.mu.Unlock()
	if !fire {
		return
	}
	redacted, size, truncated := m.marshalArgs(args)
	m.rec.Record(audit.AuditEvent{
		ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventMountOp,
		Actor: m.actor, McpID: m.mcpID(), McpRoot: m.root, Tool: op,
		Args: redacted, ArgsBytes: size, ArgsTruncated: truncated,
		Outcome: audit.AuditOutcomeThrottled,
	})
}

// attach writes the mount_attach row durably — before any 9P byte is
// served. A non-nil return means the session must not proceed at all
// (design doc: "a session that cannot be recorded is closed").
func (m *mountAudit) attach(outcome string, attachErr error) error {
	ev := audit.AuditEvent{
		ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventMountAttach,
		Actor: m.actor, McpID: m.mcpID(), McpRoot: m.root, Outcome: outcome,
	}
	if attachErr != nil {
		ev.Error = attachErr.Error()
	}
	return m.rec.RecordDurable(ev)
}

// detach writes the mount_detach row, best-effort (design doc: "no —
// best effort, Record").
func (m *mountAudit) detach(reason string, durMs int64) {
	m.counters.mu.Lock()
	br, bw, ops := m.counters.bytesRead, m.counters.bytesWritten, m.counters.ops
	m.counters.mu.Unlock()
	m.rec.Record(audit.AuditEvent{
		ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventMountDetach,
		Actor: m.actor, McpID: m.mcpID(), McpRoot: m.root,
		DurMs: durMs, BytesRead: br, BytesWritten: bw, Ops: ops, Error: reason,
	})
}
