package main

import (
	"encoding/json"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/projectfs"
)

// fileAudit writes the file plane's file_op rows (docs/project-files.md). It
// follows the mount plane's shape: a durable intent row before a mutation, a
// completion row after it, and a single denied row for a refusal.
type fileAudit struct {
	rec *audit.AuditRecorder
}

// fileOpRow is the identity every row of one operation shares.
type fileOpRow struct {
	actor audit.AuditActor
	root  string
	tool  string
	args  map[string]any
}

func (r fileOpRow) event(rec *audit.AuditRecorder) audit.AuditEvent {
	// Arguments are written as given, not through the recorder's redaction:
	// the project id and path are what the row exists to carry, and they must
	// be present whether or not tool-argument logging is on.
	raw, err := json.Marshal(r.args)
	if err != nil {
		raw = json.RawMessage("{}")
	}
	return audit.AuditEvent{
		ID: audit.NewAuditID(), TS: time.Now(), Event: audit.AuditEventFileOp,
		Actor: r.actor, McpRoot: r.root, Tool: r.tool,
		Args: raw, ArgsBytes: len(raw),
	}
}

// begin records the intent. With auditing off it records nothing and the
// operation runs. With auditing on, an intent that cannot be written returns
// AUDIT_UNAVAILABLE and the caller must not run the operation. The returned
// end writes the completion row.
func (a fileAudit) begin(row fileOpRow) (end func(error), err error) {
	if !a.rec.Enabled() {
		return func(error) {}, nil
	}
	intent := row.event(a.rec)
	intent.Phase = audit.AuditPhaseIntent
	intent.Outcome = audit.AuditOutcomePending
	if err := a.rec.RecordDurable(intent); err != nil {
		return nil, projectfs.Errf(projectfs.CodeAuditUnavailable, "audit log unavailable; the change was not made")
	}
	start := time.Now()
	return func(opErr error) {
		done := intent
		done.Phase = audit.AuditPhaseCompletion
		done.TS = time.Now()
		done.DurMs = time.Since(start).Milliseconds()
		if opErr != nil {
			done.Outcome = audit.AuditOutcomeError
			done.Error = projectfs.CodeOf(opErr)
		} else {
			done.Outcome = audit.AuditOutcomeOK
		}
		a.rec.Record(done)
	}, nil
}

// denied records one row for a refused mutation: no phase, the code as the
// error, nothing attempted.
func (a fileAudit) denied(row fileOpRow, code string) {
	if !a.rec.Enabled() {
		return
	}
	ev := row.event(a.rec)
	ev.Outcome = audit.AuditOutcomeDenied
	ev.Error = code
	a.rec.Record(ev)
}

// deniedCode reports whether a refusal is one the audit records. Argument
// and availability errors are not: nothing about them probed the boundary.
func deniedCode(code string) bool {
	switch code {
	case projectfs.CodeTraversal, projectfs.CodeSymlink, projectfs.CodeReadOnly:
		return true
	}
	return false
}
