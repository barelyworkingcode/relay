// Package ledger is relay's on-disk record of live and dormant sessions
// (plan-broker-and-sessions.md §2 C5). It is a flat JSON file, not a
// database: one atomic read-modify-write per mutation, guarded by an
// in-process mutex so relay's own goroutines cannot tear a concurrent save.
//
// A ledger record is deliberately thin. It never holds a launch secret or a
// model key — both are minted fresh on every launch or resume
// (cmd/relay/session_launch.go) and are never persisted anywhere. Losing the
// ledger loses the list of dormant sessions to offer for resume, never a
// credential.
package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

// State is a ledger record's liveness. Only claude/pi/chat sessions are ever
// recorded — a terminal (pty) is never persisted (C5) — so every record here
// started as "live" and later ages to "dormant" once its provider process
// exits.
type State string

const (
	StateLive    State = "live"
	StateDormant State = "dormant"
	// StateDiscovered marks a record read from a transcript relay did not
	// launch: display-only, no project, never resumable through the session
	// resume door.
	StateDiscovered State = "discovered"
)

// Record is one session the ledger knows about. Field names match C5's
// on-disk shape exactly.
type Record struct {
	SessionID  string    `json:"session_id"`
	Kind       string    `json:"kind"`
	ProjectID  string    `json:"project_id"`
	Directory  string    `json:"directory"`
	TemplateID string    `json:"template_id,omitempty"`
	Created    time.Time `json:"created"`
	State      State     `json:"state"`
	// Origin is who started the session when it was not the person; a resume
	// carries it back to the host.
	Origin string `json:"origin,omitempty"`
	// Entry names the project agent entry the session started from; a resume
	// reads that entry again for its current launcher.
	Entry string `json:"entry,omitempty"`
	// SessionRequest is the exact eve-facing create body relay pushed to
	// relay-sessions (after policy merge) — kept so a resume can rebuild an
	// equivalent LaunchSpec without asking the (possibly gone) original
	// caller again. Never carries a secret or key: C5's session_request is
	// eve's POST /api/sessions body shape, which has no field for either.
	SessionRequest json.RawMessage `json:"session_request,omitempty"`
	// Detached marks a claude session whose agent runs on an SSH host,
	// outside relay's process tree; nil for every other session.
	Detached *DetachedRef `json:"detached,omitempty"`
	// Root is the process the shim's Hello bound at launch: the one relay
	// binds again after a restart. Nil for a record written before relay kept
	// it, and for a session with no shim.
	Root *RootRef `json:"root,omitempty"`
	// CodexThread is the codex daemon thread an attached session drives; a
	// dormant session keeps it to attach again.
	CodexThread string `json:"codex_thread,omitempty"`
	// Unsandboxed marks a Claude session relay runs with no project and no
	// sandbox; its ProjectID is empty. A resume carries the mark back to the host.
	Unsandboxed bool `json:"unsandboxed,omitempty"`
	// ConversationID is the client's own conversation id.
	ConversationID string `json:"conversation_id,omitempty"`
	// Agent is "claude" on a terminal or discovered record.
	Agent string `json:"agent,omitempty"`
	// HostID is a discovered record's SSH host; empty on the console.
	HostID string `json:"host_id,omitempty"`
	// AdoptPending marks a dormant record whose host adopt has not settled:
	// the outside process it names may still run. A resume checks it first.
	AdoptPending *AdoptPending `json:"adopt_pending,omitempty"`
}

// AdoptPending pins the outside process a host adopt was taking over.
type AdoptPending struct {
	ConversationID string `json:"conversation_id"`
	PID            int    `json:"pid"`
	// Lifetime is the row's lifetime string, compared byte for byte.
	Lifetime string `json:"lifetime"`
}

// RootRef names a process by its kernel identity. It is not a credential:
// holding it grants nothing, and relay checks it against the kernel before any
// use.
type RootRef struct {
	PID        int32 `json:"pid"`
	PIDVersion int32 `json:"pidversion"`
	StartSec   int64 `json:"start_sec"`
	StartUsec  int32 `json:"start_usec"`
}

// DetachedRef names the host a detached session's agent runs on. The event
// cursor is not kept here: only relay-sessions reads the agent's output, so it
// saves the cursor with the conversation's state.
type DetachedRef struct {
	HostID string `json:"host_id"`
}

const fileName = "sessions-ledger.json"
const fileMode = 0o600

// Ledger is relay's session record, one per ConfigDir. Safe for concurrent
// use.
type Ledger struct {
	mu      sync.Mutex
	path    string
	records map[string]Record
}

// Open loads the ledger at <configDir>/sessions-ledger.json, creating an
// empty one in memory if the file does not exist yet — the caller decides
// separately whether to run ImportFromRelayLLM before the first save turns
// "does not exist" into "exists and is empty".
func Open(configDir string) (*Ledger, error) {
	l := &Ledger{path: filepath.Join(configDir, fileName), records: map[string]Record{}}
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: read %s: %w", l.path, err)
	}
	var recs []Record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("ledger: parse %s: %w", l.path, err)
	}
	for _, r := range recs {
		l.records[r.SessionID] = r
	}
	return l, nil
}

// Exists reports whether a ledger file is already on disk at configDir —
// the signal the caller uses to decide whether this is the feature's first
// start and an import from relayLLM's session store should run.
func Exists(configDir string) bool {
	_, err := os.Stat(filepath.Join(configDir, fileName))
	return err == nil
}

// Put inserts or replaces the record named r.SessionID and saves.
func (l *Ledger) Put(r Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records[r.SessionID] = r
	return l.saveLocked()
}

// Get returns the record named id, if any.
func (l *Ledger) Get(id string) (Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.records[id]
	return r, ok
}

// SetState updates only the State field of the record named id and saves.
// A no-op, reporting false, for an id the ledger does not hold.
func (l *Ledger) SetState(id string, state State) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.records[id]
	if !ok {
		return false, nil
	}
	r.State = state
	l.records[id] = r
	return true, l.saveLocked()
}

// MarkLiveDormant sets every StateLive record to StateDormant with a single
// save, except the records keep reports true for, and reports how many
// changed. A nil keep keeps none. It does not save when none changed.
func (l *Ledger) MarkLiveDormant(keep func(Record) bool) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := 0
	for id, r := range l.records {
		if r.State != StateLive || (keep != nil && keep(r)) {
			continue
		}
		r.State = StateDormant
		l.records[id] = r
		changed++
	}
	if changed == 0 {
		return 0, nil
	}
	return changed, l.saveLocked()
}

// DiscoveredID is "d-" plus the first 16 bytes, hex, of
// sha256(machineKey, NUL, agent, NUL, conversationID): one machine's
// conversation always maps to one record.
func DiscoveredID(machineKey, agent, conversationID string) string {
	h := sha256.Sum256([]byte(machineKey + "\x00" + agent + "\x00" + conversationID))
	return "d-" + hex.EncodeToString(h[:16])
}

// PutDiscovered adds the records in one save. It skips an id the ledger already
// holds, so it never replaces a record, discovered or not, and returns how many
// it added. It does not save when it added none.
func (l *Ledger) PutDiscovered(recs []Record) (int, error) {
	added, _, err := l.ReplaceDiscovered(recs, nil)
	return added, err
}

// ReplaceDiscovered adds the records PutDiscovered would and removes the named
// records that are still discovered, all in one save. It returns both counts.
func (l *Ledger) ReplaceDiscovered(recs []Record, evict []string) (added, evicted int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := map[string]Record{}
	for _, id := range evict {
		if r, ok := l.records[id]; ok && r.State == StateDiscovered {
			removed[id] = r
			delete(l.records, id)
		}
	}
	var addedIDs []string
	for _, r := range recs {
		if _, held := l.records[r.SessionID]; held {
			continue
		}
		l.records[r.SessionID] = r
		addedIDs = append(addedIDs, r.SessionID)
	}
	if len(addedIDs) == 0 && len(removed) == 0 {
		return 0, 0, nil
	}
	if err := l.saveLocked(); err != nil {
		for _, id := range addedIDs {
			delete(l.records, id)
		}
		for id, r := range removed {
			l.records[id] = r
		}
		return 0, 0, err
	}
	return len(addedIDs), len(removed), nil
}

// RemoveDiscovered removes, in one save, only the named records that are still
// discovered, and returns how many it removed. A record that has since become
// something else stays.
func (l *Ledger) RemoveDiscovered(ids []string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := map[string]Record{}
	for _, id := range ids {
		if r, ok := l.records[id]; ok && r.State == StateDiscovered {
			removed[id] = r
			delete(l.records, id)
		}
	}
	if len(removed) == 0 {
		return 0, nil
	}
	if err := l.saveLocked(); err != nil {
		for id, r := range removed {
			l.records[id] = r
		}
		return 0, err
	}
	return len(removed), nil
}

// Remove deletes the record named id and saves. A no-op for an id the
// ledger does not hold.
func (l *Ledger) Remove(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.records[id]; !ok {
		return nil
	}
	delete(l.records, id)
	return l.saveLocked()
}

// All returns every record, sorted by session id so a caller's output (and a
// golden test) is deterministic.
func (l *Ledger) All() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, 0, len(l.records))
	for _, r := range l.records {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// saveLocked writes every record to disk atomically at 0600. Called with
// l.mu held.
func (l *Ledger) saveLocked() error {
	recs := make([]Record, 0, len(l.records))
	for _, r := range l.records {
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].SessionID < recs[j].SessionID })
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fmt.Errorf("ledger: marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return fmt.Errorf("ledger: mkdir: %w", err)
	}
	if err := config.AtomicWriteFile(l.path, data, fileMode); err != nil {
		return fmt.Errorf("ledger: write %s: %w", l.path, err)
	}
	return nil
}
