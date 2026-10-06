package hostapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessiontypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// maxSendBodyBytes bounds a /send body; relay's own route caps the text well
// below this.
const maxSendBodyBytes = 128 << 10

// handleSend implements POST /send: deliver one user message marked with the
// Chief of Staff origin. The origin is checked here, not trusted: the only
// accepted value is the constant, so this route cannot stamp a person's
// message as someone else's or invent a new origin. An unlisted session
// answers 404 exactly like an unknown one. The message text is never logged.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !s.checkInternalPeer(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	start := time.Now()
	ctx := logging.ContextWithTrace(r.Context(), logging.TraceIDOrNew(r.Header.Get(logging.TraceHeader)))
	var req SendRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxSendBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, ErrInvalidSpec, "malformed body")
		return
	}
	if req.SessionID == "" || strings.TrimSpace(req.Text) == "" || req.Origin != sessiontypes.OriginChiefOfStaff {
		writeErr(w, http.StatusBadRequest, ErrInvalidSpec, "session_id, text and a supported origin are required")
		return
	}

	logSend := func(status string, err error) {
		attrs := []any{"op", "session.send", "status", status, "session_id", req.SessionID,
			"origin", req.Origin, "duration_ms", time.Since(start).Milliseconds()}
		if err != nil {
			attrs = append(attrs, "error", err.Error())
		}
		if status == "ok" {
			slog.InfoContext(ctx, "session send", attrs...)
			return
		}
		slog.WarnContext(ctx, "session send", attrs...)
	}

	if !s.sessions.IsListed(req.SessionID) {
		logSend("error", session.ErrSessionNotFound)
		writeErr(w, http.StatusNotFound, ErrSessionNotFound, "session not found")
		return
	}
	err := s.sessions.SendMessageAs(req.SessionID, req.Text, nil, req.Origin)
	switch {
	case err == nil:
		logSend("ok", nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(SendResponse{
			SessionID: req.SessionID,
			Origin:    req.Origin,
			At:        time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	case errors.Is(err, session.ErrSessionNotFound):
		logSend("error", err)
		writeErr(w, http.StatusNotFound, ErrSessionNotFound, "session not found")
	case errors.Is(err, session.ErrAlreadyProcessing):
		logSend("error", err)
		writeErr(w, http.StatusConflict, ErrAlreadyProcessing, "session is already processing a message")
	case errors.Is(err, session.ErrResumeRequired):
		logSend("error", err)
		writeErr(w, http.StatusConflict, ErrResumeRequired, "session is not running; resume it first")
	default:
		logSend("error", err)
		writeErr(w, http.StatusInternalServerError, ErrSendFailed, "send failed")
	}
}
