package world

import (
	"fmt"
	"sync"
)

// HistoryRow is one row of relay's GET /api/session-history, as the world
// spells it. Refuse, when set, makes the resume door answer that code.
type HistoryRow struct {
	ID             string  `json:"id"`
	Record         string  `json:"record"`
	Kind           string  `json:"kind"`
	Agent          *string `json:"agent"`
	ProjectID      *string `json:"projectId"`
	HostID         *string `json:"hostId"`
	Directory      string  `json:"directory"`
	ConversationID *string `json:"conversationId"`
	Title          *string `json:"title"`
	TitleSource    *string `json:"titleSource"`
	LastActiveAt   *string `json:"lastActiveAt"`
	CreatedAt      string  `json:"createdAt"`
	State          string  `json:"state"`
	Transcript     string  `json:"transcript"`
	Resume         string  `json:"resume"`
	ResumeReason   string  `json:"resumeReason,omitempty"`
	Refuse         string  `json:"refuse,omitempty"`
}

var (
	historyRecords     = map[string]bool{"launched": true, "discovered": true}
	historyKinds       = map[string]bool{"claude": true, "pi": true, "codex": true, "chat": true, "terminal": true}
	historyStates      = map[string]bool{"live": true, "dormant": true, "discovered": true}
	historyTranscripts = map[string]bool{"present": true, "absent": true, "unknown": true}
	historyResumes     = map[string]bool{"session": true, "history": true, "none": true}
	historyTitleSrc    = map[string]bool{"client": true, "prompt": true}
	// HistoryReasons maps each code the resume door answers to its status.
	HistoryReasons = map[string]int{
		"invalid_body": 400, "history_not_found": 404, "use_session_resume": 409, "live": 409, "host_unsupported": 409,
		"no_conversation": 409, "held": 409, "transcript_absent": 409, "no_project": 409, "resume_in_progress": 409, "unavailable": 502,
	}
)

// ValidateHistory checks ids and the wire's enums; the first problem is
// reported with its JSON path.
func ValidateHistory(rows []HistoryRow) error {
	seen := map[string]bool{}
	for i, r := range rows {
		path := fmt.Sprintf("sessionHistory[%d]", i)
		switch {
		case r.ID == "" || seen[r.ID]:
			return bad(path+".id", "must be unique and not empty")
		case !historyRecords[r.Record]:
			return bad(path+".record", "must be launched or discovered")
		case !historyKinds[r.Kind]:
			return bad(path+".kind", "must be claude, pi, codex, chat or terminal")
		case !historyStates[r.State]:
			return bad(path+".state", "must be live, dormant or discovered")
		case !historyTranscripts[r.Transcript]:
			return bad(path+".transcript", "must be present, absent or unknown")
		case !historyResumes[r.Resume]:
			return bad(path+".resume", "must be session, history or none")
		case r.TitleSource != nil && !historyTitleSrc[*r.TitleSource]:
			return bad(path+".titleSource", "must be client or prompt")
		case r.ResumeReason != "" && HistoryReasons[r.ResumeReason] == 0 && r.ResumeReason != "kind":
			return bad(path+".resumeReason", "is not a reason the wire carries")
		case r.Refuse != "" && HistoryReasons[r.Refuse] == 0:
			return bad(path+".refuse", "is not a code the resume door answers")
		}
		seen[r.ID] = true
	}
	return nil
}

// HistoryStore is the rows the list door and the resume door share. A resume
// removes its row.
type HistoryStore struct {
	mu       sync.Mutex
	rows     []HistoryRow
	revision uint64
	Down     bool
}

var historyMu sync.Mutex

// History returns the world's shared history store, built on first use.
func (w *World) History() *HistoryStore {
	historyMu.Lock()
	defer historyMu.Unlock()
	if w.history == nil {
		w.history = &HistoryStore{rows: append([]HistoryRow{}, w.SessionHistory...), revision: 1, Down: w.SessionHistoryUnavailable}
	}
	return w.history
}

// Rows returns a copy of the rows (those of one project when projectID is set)
// and the revision.
func (h *HistoryStore) Rows(projectID string) ([]HistoryRow, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []HistoryRow{}
	for _, r := range h.rows {
		if projectID == "" || (r.ProjectID != nil && *r.ProjectID == projectID) {
			out = append(out, r)
		}
	}
	return out, h.revision
}

// Find returns the row named id.
func (h *HistoryStore) Find(id string) (HistoryRow, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.rows {
		if r.ID == id {
			return r, true
		}
	}
	return HistoryRow{}, false
}

// Remove drops the row named id and bumps the revision.
func (h *HistoryStore) Remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, r := range h.rows {
		if r.ID == id {
			h.rows = append(h.rows[:i:i], h.rows[i+1:]...)
			h.revision++
			return
		}
	}
}
