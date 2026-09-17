package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/barelyworkingcode/relay/internal/sessions/terminal"
)

// HandleListTerminals writes every session mgr currently holds (live or
// stopped-but-not-yet-closed) as JSON. A plain function, not a method on
// some *Handlers type: the terminal id (for the other two handlers) and the
// manager are both passed in explicitly so a caller — a real mux route or a
// test — never needs to stand up a router to exercise this.
func HandleListTerminals(mgr *terminal.Manager, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"terminals": mgr.ListSummary()})
}

// HandleDeleteTerminal closes and removes one terminal session. 404 if id
// names no session mgr knows about; 204 on success.
func HandleDeleteTerminal(mgr *terminal.Manager, id string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, ok := mgr.Get(id); !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	mgr.Close(id)
	w.WriteHeader(http.StatusNoContent)
}

// HandleTerminalLog streams a session's stitched head+tail replay log.
// Works for a session that has already exited and left mgr's live table
// too, as long as its log files are still on disk — this handler reads
// straight off mgr.LogDir() rather than through mgr.Get, which is the whole
// point of a persisted replay log surviving session eviction.
func HandleTerminalLog(mgr *terminal.Manager, id string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	dir := mgr.LogDir()
	if dir == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	head, tail, err := terminal.OpenTerminalLogReaders(dir, id)
	if err != nil {
		if errors.Is(err, terminal.ErrTerminalLogNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer func() {
		if head != nil {
			_ = head.Close()
		}
		if tail != nil {
			_ = tail.Close()
		}
	}()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if head != nil {
		_, _ = io.Copy(w, head)
	}
	if tail != nil {
		_, _ = io.Copy(w, tail)
	}
}
