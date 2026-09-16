package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/sessions/session"
	sessionstypes "github.com/barelyworkingcode/relay/internal/sessions/types"
)

// HandleListSessions writes every non-headless session mgr knows about
// (live, idle, or persisted-only) as JSON. A plain function, not a method
// on some *Handlers type — mirrors http_terminal.go's HandleListTerminals.
func HandleListSessions(mgr *session.Manager, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"sessions": mgr.List()})
}

// HandleDeleteSession kills the provider (if any), removes provider-specific
// data, and deletes the persisted session file. 204 unconditionally — like
// relayLLM's DeleteSession, deleting an id nothing knows about is not an
// error (the caller's goal, "this id no longer exists", is already true).
func HandleDeleteSession(mgr *session.Manager, id string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	mgr.DeleteSession(id)
	w.WriteHeader(http.StatusNoContent)
}

// sendMessageRequest is the synchronous-send body: relayLLM's own HTTP
// surface for callers that aren't a WS client (relayScheduler,
// relayTelegram) and want one complete reply rather than a stream.
type sendMessageRequest struct {
	Text  string                         `json:"text"`
	Files []sessionstypes.FileAttachment `json:"files,omitempty"`
}

type sendMessageResponse struct {
	Text  string                     `json:"text"`
	Stats sessionstypes.SessionStats `json:"stats"`
}

// HandleSessionMessageSync sends id a message and blocks for the complete
// response (session.Manager.SendMessageSync's own bound, 5 minutes by
// default). SH-6 still applies: a project-bound session with a dead
// provider answers 409 resume_required rather than silently respawning.
func HandleSessionMessageSync(mgr *session.Manager, id string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	text, stats, err := mgr.SendMessageSync(id, req.Text, req.Files)
	if err != nil {
		writeSessionError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sendMessageResponse{Text: text, Stats: stats})
}

func writeSessionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, session.ErrSessionNotFound):
		w.WriteHeader(http.StatusNotFound)
	case errors.Is(err, session.ErrResumeRequired):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "resume_required"})
	case errors.Is(err, session.ErrAlreadyProcessing):
		w.WriteHeader(http.StatusConflict)
	default:
		w.WriteHeader(http.StatusInternalServerError)
	}
}
