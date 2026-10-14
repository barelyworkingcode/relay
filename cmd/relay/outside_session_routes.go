package main

import (
	"errors"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/outsidesessions"
	"github.com/gorilla/websocket"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

var (
	outsideLimitPattern  = regexp.MustCompile(`^[1-9][0-9]{0,2}$`)
	outsideCursorPattern = regexp.MustCompile(`^[1-9][0-9]{0,14}$`)
)

// handleOutsideTranscript answers one read-only page of an outside session's
// transcript. The checks run in a fixed order and the first failure answers.
func handleOutsideTranscript(i *outsidesessions.Inventory) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 || len(r.TransferEncoding) > 0 || r.Header.Get("Transfer-Encoding") != "" {
			writeChiefOfStaffError(w, http.StatusBadRequest, "invalid_request", "this request takes no body")
			return
		}
		query := r.URL.Query()
		for key, values := range query {
			if (key != "before" && key != "limit") || len(values) != 1 {
				writeChiefOfStaffError(w, http.StatusBadRequest, "invalid_request", "only one before and one limit are accepted")
				return
			}
		}
		limit := outsidesessions.TranscriptPageDefault
		if values, ok := query["limit"]; ok {
			n, err := strconv.Atoi(values[0])
			if !outsideLimitPattern.MatchString(values[0]) || err != nil || n > outsidesessions.TranscriptPageMax {
				writeChiefOfStaffError(w, http.StatusBadRequest, "invalid_limit", "limit must be 1 to 200")
				return
			}
			limit = n
		}
		var before int64
		if values, ok := query["before"]; ok {
			n, err := strconv.ParseInt(values[0], 10, 64)
			if !outsideCursorPattern.MatchString(values[0]) || err != nil {
				writeChiefOfStaffError(w, http.StatusBadRequest, "invalid_cursor", "before is not a cursor")
				return
			}
			before = n
		}
		page, err := i.Transcript(r.Context(), r.PathValue("id"), before, limit)
		switch {
		case errors.Is(err, outsidesessions.ErrNotFound):
			writeChiefOfStaffError(w, http.StatusNotFound, "outside_session_not_found", "outside session not found")
		case errors.Is(err, outsidesessions.ErrBadCursor):
			writeChiefOfStaffError(w, http.StatusBadRequest, "invalid_cursor", "before is past the end of the transcript")
		case err != nil:
			writeChiefOfStaffError(w, http.StatusBadGateway, "outside_status_unavailable", "outside transcript could not be read")
		default:
			writeJSON(w, http.StatusOK, page)
		}
	}
}

func RegisterOutsideSessionRoutes(rr *control.RouteRegistrar, i *outsidesessions.Inventory) {
	rr.Handle(control.ClassChiefOfStaff, "GET /api/chief-of-staff/outside-sessions", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, i.Snapshot()) })
	rr.Handle(control.ClassChiefOfStaff, "GET /api/chief-of-staff/outside-sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		detail, err := i.Detail(r.Context(), r.PathValue("id"))
		if err != nil {
			status, code := http.StatusBadGateway, "outside_status_unavailable"
			if errors.Is(err, outsidesessions.ErrNotFound) {
				status, code = http.StatusNotFound, "outside_session_not_found"
			}
			writeChiefOfStaffError(w, status, code, "outside session status could not be read")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	})
	rr.Handle(control.ClassExecute, "GET /api/outside-sessions/{id}/transcript", handleOutsideTranscript(i))
	rr.Handle(control.ClassChiefOfStaff, "GET /ws/outside-sessions", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		snapshots, unsubscribe := i.Subscribe()
		defer unsubscribe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				kind, _, err := conn.NextReader()
				if err != nil {
					return
				}
				if kind == websocket.TextMessage || kind == websocket.BinaryMessage {
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "outside sessions are read-only"), time.Now().Add(time.Second))
					return
				}
			}
		}()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-done:
				return
			case s := <-snapshots:
				conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
				if conn.WriteJSON(s) != nil {
					return
				}
			}
		}
	})
}
