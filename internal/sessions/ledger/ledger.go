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
	// SessionRequest is the exact eve-facing create body relay pushed to
	// relay-sessions (after policy merge) — kept so a resume can rebuild an
	// equivalent LaunchSpec without asking the (possibly gone) original
	// caller again. Never carries a secret or key: C5's session_request is
	// eve's POST /api/sessions body shape, which has no field for either.
	SessionRequest json.RawMessage `json:"session_request,omitempty"`
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
