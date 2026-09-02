package main

import (
	"io"
	"log/slog"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
)

// openAuditWriter adapts log_rotate.go's rotating writer to
// audit.OpenWriter's shape. Log rotation is shared by relay's own log, the
// audit log and every managed service's log, so internal/audit does not own
// it — this is the one place that wires the audit engine to it.
func openAuditWriter(path string, maxBytes int64, generations int) (io.WriteCloser, error) {
	return openRotatingLogGenerations(path, maxBytes, generations)
}

// auditLogPath resolves the on-disk path of the tool-call audit log. A
// thin wrapper over audit.LogPath: relay's log directory (serviceLogDir) is
// main's to resolve, not internal/audit's.
func auditLogPath() (string, error) {
	dir, err := serviceLogDir()
	if err != nil {
		return "", err
	}
	return audit.LogPath(dir), nil
}

// startAuditRecorder wires the audit engine to the two things it does not
// own: where relay's rotated logs live (serviceLogDir) and how a log file
// rotates (log_rotate.go). Auditing is observability, not an authorization
// control, so a failure here is logged and auditing stays off rather than
// taking the tray down with it — the Tool Calls tab surfaces the disabled
// state.
func startAuditRecorder(s *config.Settings) *audit.AuditRecorder {
	dir, err := serviceLogDir()
	if err != nil {
		slog.Error("audit log disabled: cannot resolve log dir", "error", err)
		return nil
	}
	return audit.StartAuditRecorder(s.Audit, dir, openAuditWriter)
}
