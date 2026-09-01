package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/control"
)

type auditPathView struct {
	Path string `json:"path"`
}

func auditHTTPStatus(err error) int {
	switch {
	case errors.Is(err, audit.ErrAuditNotFound):
		return http.StatusNotFound
	case errors.Is(err, audit.ErrAuditInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeAuditError(w http.ResponseWriter, err error) {
	writeJSON(w, auditHTTPStatus(err), map[string]string{"error": err.Error()})
}

// parseAuditQueryParams turns URL query values into audit.AuditQueryFields. A
// non-numeric limit or non-boolean deep is a parsing failure the caller must
// fix, so it is reported as a 400 here rather than reaching Query with a
// zero value that would silently answer a different question (or a full
// unbounded scan).
func parseAuditQueryParams(q url.Values) (audit.AuditQueryFields, error) {
	f := audit.AuditQueryFields{
		ProjectID: q.Get("project_id"),
		McpID:     q.Get("mcp_id"),
		Outcome:   q.Get("outcome"),
		Event:     q.Get("event"),
		Kind:      q.Get("kind"),
		Text:      q.Get("text"),
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return audit.AuditQueryFields{}, fmt.Errorf("limit: %q is not an integer", v)
		}
		f.Limit = n
	}
	if v := q.Get("deep"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return audit.AuditQueryFields{}, fmt.Errorf("deep: %q is not a boolean", v)
		}
		f.Deep = b
	}
	return f, nil
}

// ops is the same instance the Tool Calls tab's IPC handlers use, so a query
// run from curl and one run from the tray see identical redaction and
// filtering — Query never reads the log file itself, only audit.AuditRecorder's own
// query path, so there is no second route to the raw bytes for this to drift
// from (see the SECURITY note on audit.AuditOps.Query).
func RegisterAuditRoutes(rr *control.RouteRegistrar, ops *audit.AuditOps) {
	rr.Handle(control.ClassRead, "GET /api/audit", func(w http.ResponseWriter, r *http.Request) {
		fields, err := parseAuditQueryParams(r.URL.Query())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		events, err := ops.Query(fields)
		if err != nil {
			writeAuditError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, events)
	})

	rr.Handle(control.ClassConfigure, "POST /api/audit/export", func(w http.ResponseWriter, r *http.Request) {
		var body audit.AuditQueryFields
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		path, err := ops.Export(body)
		if err != nil {
			writeAuditError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, auditPathView{Path: path})
	})

	// No reveal-in-Finder route: opening a file browser is a desktop side
	// effect a remote caller cannot sensibly trigger. This exposes only the
	// path; ipcRevealAuditLog keeps the Finder action on the IPC door.
	rr.Handle(control.ClassRead, "GET /api/audit/log", func(w http.ResponseWriter, r *http.Request) {
		path, err := ops.LogPath()
		if err != nil {
			writeAuditError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, auditPathView{Path: path})
	})
}
