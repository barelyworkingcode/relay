package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var (
	// errAuditNotFound exists for symmetry with the other ADR-014 slices and
	// the shared status mapper's shape; nothing in this read-only slice
	// currently returns it, because none of its three routes name a resource
	// by id.
	errAuditNotFound = errors.New("audit resource not found")
	errAuditInvalid  = errors.New("invalid audit query")
)

// Carries the reason text verbatim, the same trick serviceValidationError and
// enrolmentValidationError use: wrapping with %w would prefix the sentinel's
// own text.
type auditValidationError struct{ reason string }

func (e *auditValidationError) Error() string        { return e.reason }
func (e *auditValidationError) Is(target error) bool { return target == errAuditInvalid }

// recorder is nil-safe so a caller can derive an IssuanceAuditor from a slice
// that may itself be absent, without a second nil check at every call site.
func (o *AuditOps) recorder() *AuditRecorder {
	if o == nil {
		return nil
	}
	return o.Audit
}

func invalidAudit(reason string) error {
	return &auditValidationError{reason: reason}
}

var auditValidOutcomes = map[string]bool{
	AuditOutcomeOK:             true,
	AuditOutcomeError:          true,
	AuditOutcomeToolError:      true,
	AuditOutcomeDenied:         true,
	AuditOutcomeUnauthorized:   true,
	AuditOutcomeThrottled:      true,
	AuditOutcomePending:        true,
	auditOutcomeScopeViolation: true, // a field, not a stored outcome (ADR-011 decision 7); matches() special-cases it
}

var auditValidKinds = map[string]bool{
	AuditActorProject:  true,
	AuditActorService:  true,
	AuditActorRemote:   true,
	AuditActorUnknown:  true,
	AuditActorRelay:    true,
	AuditActorControl:  true,
	AuditActorOperator: true,
}

// auditQueryFields is the transport-agnostic filter shape behind both doors:
// the IPC query message embeds AuditQuery directly and ipcQueryAudit
// translates it here, while GET /api/audit builds one from URL query params.
// JSON tags mirror AuditQuery's because the settings UI's JS already builds
// its query_audit payload against that spelling.
type auditQueryFields struct {
	ProjectID string `json:"project_id,omitempty"`
	McpID     string `json:"mcp_id,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	Event     string `json:"event,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Text      string `json:"text,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	// Deep searches the on-disk log rather than the in-memory ring, for
	// history older than the ring holds (bounded by auditTailBudget).
	Deep bool `json:"deep,omitempty"`
}

func auditFieldsFromQuery(q AuditQuery) auditQueryFields {
	return auditQueryFields{
		ProjectID: q.ProjectID,
		McpID:     q.McpID,
		Outcome:   q.Outcome,
		Event:     q.Event,
		Kind:      q.Kind,
		Text:      q.Text,
		Limit:     q.Limit,
		Deep:      q.Deep,
	}
}

func (f auditQueryFields) toQuery() AuditQuery {
	return AuditQuery{
		ProjectID: f.ProjectID,
		McpID:     f.McpID,
		Outcome:   f.Outcome,
		Event:     f.Event,
		Kind:      f.Kind,
		Text:      f.Text,
		Limit:     f.Limit,
		Deep:      f.Deep,
	}
}

func (f auditQueryFields) validate() error {
	if f.Limit < 0 {
		return invalidAudit(fmt.Sprintf("limit must be >= 0, got %d", f.Limit))
	}
	if f.Outcome != "" && !auditValidOutcomes[f.Outcome] {
		return invalidAudit(fmt.Sprintf("unknown outcome %q", f.Outcome))
	}
	if f.Kind != "" && !auditValidKinds[f.Kind] {
		return invalidAudit(fmt.Sprintf("unknown actor kind %q", f.Kind))
	}
	return nil
}

// The one core behind both the HTTP door (audit_routes.go) and the WebView
// IPC door (ipc_audit.go). Read-only: nothing here mutates settings or the
// process registry, so unlike ServiceOps/EnrolmentOps there is no OnChange —
// a query has no state change for another view to learn about.
type AuditOps struct {
	// Audit is nil when auditing is disabled by config; every AuditRecorder
	// method is nil-safe, so this holds without a guard field of its own.
	Audit *AuditRecorder
}

// Query degrades to an empty result when auditing is disabled rather than
// erroring — matching the IPC handler this replaces, which is careful to
// distinguish "no calls yet" from "not logging" (see auditStatus) rather
// than reporting the latter as a request failure.
func (o *AuditOps) Query(f auditQueryFields) ([]AuditEvent, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	// A nil *AuditOps reads the same as a nil *AuditRecorder: "auditing off",
	// never a panic. Query's own zero-value filter always validates, so
	// there is no field-empty check upstream of this to lean on instead —
	// see TestIPCDispatch_HandlerSurvivesMalformedPayload, which calls every
	// handler with an ops-less IPCContext.
	if o == nil || !o.Audit.Enabled() {
		return []AuditEvent{}, nil
	}
	return o.Audit.Query(f.toQuery()), nil
}

// Export writes a *filtered* view to disk and returns the path written:
// handing someone the whole log to answer one question over-shares by
// default, same as the IPC handler this replaces.
//
// The destination directory is always the audit log's own directory and the
// filename is generated here from the current time — auditQueryFields carries
// no path field, so a caller has no channel through which to influence where
// the file lands. This is deliberate: the audit log is the most sensitive
// read surface in the product, and an export path built from caller input
// would be a traversal write primitive reachable over HTTP.
func (o *AuditOps) Export(f auditQueryFields) (string, error) {
	if err := f.validate(); err != nil {
		return "", err
	}
	if o == nil || !o.Audit.Enabled() {
		return "", invalidAudit("auditing is disabled")
	}

	q := f.toQuery()
	q.Deep = true
	q.Limit = intOr(q.Limit, 100000)

	o.Audit.Flush()
	events := o.Audit.Query(q)

	dir := filepath.Dir(o.Audit.Path())
	name := fmt.Sprintf("toolcalls-export-%s.jsonl", time.Now().UTC().Format("20060102-150405"))
	path := filepath.Join(dir, name)

	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create export: %w", err)
	}
	defer out.Close()

	enc := json.NewEncoder(out)
	// Oldest-first in the export: a log read top-to-bottom should run forwards
	// in time, even though Query answers newest-first.
	for i := len(events) - 1; i >= 0; i-- {
		if err := enc.Encode(events[i]); err != nil {
			return "", fmt.Errorf("write export: %w", err)
		}
	}
	return path, nil
}

// LogPath answers the log file's location. Revealing it in Finder is a
// desktop side effect that stays on the IPC path (ipcRevealAuditLog); this is
// the part of that capability that generalises to a remote caller.
func (o *AuditOps) LogPath() (string, error) {
	if o == nil || !o.Audit.Enabled() {
		return "", invalidAudit("auditing is disabled")
	}
	return o.Audit.Path(), nil
}
