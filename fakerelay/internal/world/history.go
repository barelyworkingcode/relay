package world

import "fmt"

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
		case r.Refuse != "" && HistoryReasons[r.Refuse] == 0:
			return bad(path+".refuse", "is not a code the resume door answers")
		}
		seen[r.ID] = true
	}
	return nil
}
