package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// ---------------------------------------------------------------------------
// Tool Calls tab IPC handlers
// ---------------------------------------------------------------------------

// ipcAuditQueryMsg is the Tool Calls tab's filter payload. It mirrors
// AuditQuery so the UI can send its filter state verbatim.
type ipcAuditQueryMsg struct {
	Type string `json:"type"`
	AuditQuery
}

// auditStatus is the header payload for the Tool Calls tab: enough for the UI
// to tell the difference between "no calls yet" and "not logging".
type auditStatus struct {
	Enabled  bool   `json:"enabled"`
	Path     string `json:"path"`
	Dropped  uint64 `json:"dropped"`
	Recorded uint64 `json:"recorded"`
	LogArgs  bool   `json:"log_args"`
	LogLists bool   `json:"log_lists"`
}

func auditStatusOf(rec *AuditRecorder) auditStatus {
	st := auditStatus{
		Enabled:  rec.Enabled(),
		Path:     rec.Path(),
		Dropped:  rec.Dropped(),
		Recorded: rec.Wrote(),
	}
	if rec != nil {
		st.LogArgs = rec.cfg.LogArgs
		st.LogLists = rec.cfg.LogLists
	}
	return st
}

// ipcQueryAudit answers the Tool Calls tab's list request. A deep query touches
// the log file, so it runs off the UI thread; a ring query is a slice copy and
// answers inline.
func ipcQueryAudit(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAuditQueryMsg](raw, MsgQueryAudit)
	if !ok {
		return
	}
	rec := ctx.Audit
	if !rec.Enabled() {
		ctx.UI.EmitEvent("onAuditEvents", []AuditEvent{}, auditStatusOf(rec))
		return
	}
	if msg.Deep {
		ctx.GoFunc(func() {
			events := rec.Query(msg.AuditQuery)
			dispatchEmit(ctx, "onAuditEvents", events, auditStatusOf(rec))
		})
		return
	}
	ctx.UI.EmitEvent("onAuditEvents", rec.Query(msg.AuditQuery), auditStatusOf(rec))
}

// ipcExportAudit writes a filtered snapshot to a timestamped JSONL file next to
// the log and reveals it. Exporting a *filtered* view is the point: handing
// someone the whole log to answer one question over-shares by default.
func ipcExportAudit(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAuditQueryMsg](raw, MsgExportAudit)
	if !ok {
		return
	}
	rec := ctx.Audit
	if !rec.Enabled() {
		ctx.UI.EmitEvent("onAuditError", "auditing is disabled")
		return
	}
	// Export always searches history, not just what the ring happens to hold.
	q := msg.AuditQuery
	q.Deep = true
	q.Limit = intOr(q.Limit, 100000)

	ctx.GoFunc(func() {
		rec.Flush()
		path, err := exportAuditEvents(rec, q)
		if err != nil {
			dispatchEmit(ctx, "onAuditError", err.Error())
			return
		}
		ctx.Platform.DispatchToMain(func() {
			ctx.Platform.OpenURL(fileURL(filepath.Dir(path)))
			ctx.UI.EmitEvent("onAuditExported", path)
		})
	})
}

// fileURL renders a filesystem path as a file:// URL, escaping spaces and other
// characters that would otherwise break the hand-off to the platform opener.
func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// exportAuditEvents writes matching events as JSONL and returns the file path.
func exportAuditEvents(rec *AuditRecorder, q AuditQuery) (string, error) {
	events := rec.Query(q)
	dir := filepath.Dir(rec.Path())
	name := fmt.Sprintf("toolcalls-export-%s.jsonl", time.Now().UTC().Format("20060102-150405"))
	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create export: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	// Oldest-first in the export: a log read top-to-bottom should run forwards
	// in time, even though the UI shows newest-first.
	for i := len(events) - 1; i >= 0; i-- {
		if err := enc.Encode(events[i]); err != nil {
			return "", fmt.Errorf("write export: %w", err)
		}
	}
	return path, nil
}

// ipcRevealAuditLog opens the audit log's directory in Finder.
func ipcRevealAuditLog(ctx *IPCContext, _ json.RawMessage) {
	rec := ctx.Audit
	if !rec.Enabled() {
		ctx.UI.EmitEvent("onAuditError", "auditing is disabled")
		return
	}
	ctx.Platform.OpenURL(fileURL(filepath.Dir(rec.Path())))
}
