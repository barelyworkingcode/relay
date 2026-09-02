package main

import (
	"encoding/json"
	"net/url"
	"path/filepath"

	"github.com/barelyworkingcode/relay/internal/audit"
)

type ipcAuditQueryMsg struct {
	Type string `json:"type"`
	audit.AuditQuery
}

// Enough for the UI to tell the difference between "no calls yet" and "not
// logging".
type auditStatus struct {
	Enabled  bool   `json:"enabled"`
	Path     string `json:"path"`
	Dropped  uint64 `json:"dropped"`
	Recorded uint64 `json:"recorded"`
	LogArgs  bool   `json:"log_args"`
	LogLists bool   `json:"log_lists"`
}

func auditStatusOf(rec *audit.AuditRecorder) auditStatus {
	st := auditStatus{
		Enabled:  rec.Enabled(),
		Path:     rec.Path(),
		Dropped:  rec.Dropped(),
		Recorded: rec.Wrote(),
	}
	if rec != nil {
		st.LogArgs = rec.LogArgs()
		st.LogLists = rec.LogLists()
	}
	return st
}

// A deep query touches the log file, so it runs off the UI thread; a ring
// query is a slice copy and answers inline.
func ipcQueryAudit(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAuditQueryMsg](raw, MsgQueryAudit)
	if !ok {
		return
	}
	fields := audit.AuditFieldsFromQuery(msg.AuditQuery)
	if fields.Deep {
		ctx.GoFunc(func() {
			events, err := ctx.AuditOps.Query(fields)
			if err != nil {
				dispatchEmit(ctx, "onAuditError", err.Error())
				return
			}
			dispatchEmit(ctx, "onAuditEvents", events, auditStatusOf(ctx.Audit))
		})
		return
	}
	events, err := ctx.AuditOps.Query(fields)
	if err != nil {
		ctx.UI.EmitEvent("onAuditError", err.Error())
		return
	}
	ctx.UI.EmitEvent("onAuditEvents", events, auditStatusOf(ctx.Audit))
}

// Exporting a *filtered* view is the point: handing someone the whole log to
// answer one question over-shares by default.
func ipcExportAudit(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAuditQueryMsg](raw, MsgExportAudit)
	if !ok {
		return
	}
	fields := audit.AuditFieldsFromQuery(msg.AuditQuery)

	ctx.GoFunc(func() {
		path, err := ctx.AuditOps.Export(fields)
		if err != nil {
			dispatchEmit(ctx, "onAuditError", err.Error())
			return
		}
		// Revealing the export in Finder is a desktop side effect with no HTTP
		// equivalent, so it stays here rather than in audit.AuditOps.Export.
		ctx.Platform.DispatchToMain(func() {
			ctx.Platform.OpenURL(fileURL(filepath.Dir(path)))
			ctx.UI.EmitEvent("onAuditExported", path)
		})
	})
}

func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// Opens Finder on the log's containing directory rather than firing a TCC-free
// "reveal this exact file" API relay doesn't have access to. Cannot become an
// HTTP capability — a remote caller triggering a Finder window on someone's
// desktop is not a sensible API — so this stays IPC-only; audit.AuditOps.LogPath is
// the part of this capability HTTP gets, via GET /api/audit/log.
func ipcRevealAuditLog(ctx *IPCContext, _ json.RawMessage) {
	path, err := ctx.AuditOps.LogPath()
	if err != nil {
		ctx.UI.EmitEvent("onAuditError", err.Error())
		return
	}
	ctx.Platform.OpenURL(fileURL(filepath.Dir(path)))
}
