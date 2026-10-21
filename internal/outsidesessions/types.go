// Package outsidesessions observes Claude processes. Its identities never grant actions.
package outsidesessions

import (
	"context"
	"errors"
	"time"
)

const ProbeInterval = 15 * time.Second
const ReportInterval = 2 * time.Second

var ErrNotFound = errors.New("outside session not found")
var ErrUnavailable = errors.New("outside status unavailable")

// ErrUnsupported marks a machine that cannot be observed at all, such as a
// Windows launcher host. It is a stable status, not a failed probe.
var ErrUnsupported = errors.New("outside observation unsupported")

type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}
type Session struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	ProjectID   *string `json:"projectId"`
	HostID      *string `json:"hostId"`
	Machine     string  `json:"machine"`
	Directory   string  `json:"directory"`
	Branch      string  `json:"branch"`
	State       string  `json:"state"`
	Since       string  `json:"since"`
	UpdatedAt   string  `json:"updatedAt"`
	Source      string  `json:"source"`
	Outside     bool    `json:"outside"`
	ReadOnly    bool    `json:"readOnly"`
	TmuxSession *string `json:"tmuxSession"`
	Viewable    bool    `json:"viewable"`
	ViewReason  string  `json:"viewReason"`
	// EndedAt is set on an ended row only: the transcript file's modification time.
	EndedAt string `json:"endedAt,omitempty"`
	// ClaudeSessionID is the Claude conversation id: the registry sessionId or
	// the transcript's id for a live row, the transcript file's stem for an
	// ended one.
	ClaudeSessionID string `json:"claudeSessionId,omitempty"`
	Agent           string `json:"agent,omitempty"`
	ConversationID  string `json:"conversationId,omitempty"`
	// Adoptable is true when the owner may adopt the session now or with an
	// interrupt. Idleness is checked at the adopt door, not here.
	Adoptable bool `json:"adoptable"`
	// AdoptReason is the AdoptReason* code that makes the row not adoptable;
	// absent when Adoptable.
	AdoptReason string `json:"adoptReason,omitempty"`
}

// The codes of Session.AdoptReason, in the order they are judged: the first
// that applies is the reason.
const (
	AdoptReasonEnded          = "ended"
	AdoptReasonHost           = "host_unsupported"
	AdoptReasonNoProject      = "no_project"
	AdoptReasonNotRegistered  = "not_registered"
	AdoptReasonInvalidID      = "invalid_id"
	AdoptReasonStartMismatch  = "start_mismatch"
	AdoptReasonNotInteractive = "not_interactive"
	AdoptReasonNotCommandLine = "not_command_line"
	AdoptReasonAmbiguous      = "ambiguous"
)

type MachineStatus struct {
	HostID      *string `json:"hostId"`
	Machine     string  `json:"machine"`
	Status      string  `json:"status"`
	LastProbeAt string  `json:"lastProbeAt"`
	Error       string  `json:"error,omitempty"`
}
type Snapshot struct {
	Type            string          `json:"type"`
	Revision        uint64          `json:"revision"`
	ProbeIntervalMs int             `json:"probeIntervalMs"`
	Sessions        []Session       `json:"sessions"`
	Machines        []MachineStatus `json:"machines"`
}
type Turn struct {
	Text     string `json:"text"`
	At       string `json:"at"`
	Complete bool   `json:"complete"`
}
type Detail struct {
	Session     Session `json:"session"`
	LastTurn    Turn    `json:"lastTurn"`
	LastExcerpt string  `json:"lastExcerpt"`
}

// Observation is private process evidence, never serialized to the browser.
type Observation struct {
	Session               Session
	ClaudeID              string
	Agent, ConversationID string
	PID                   int
	Lifetime              string
	Transcript            string
	StatusAt              time.Time
}

// ProbeResult is one completed probe. SawSpool is true when the machine has a
// report spool directory, which is the only reason to drain it.
type ProbeResult struct {
	Observations []Observation
	SawSpool     bool
	// Held holds the Claude session ids a live process or relay-sessions
	// holds: every observed id before project filtering, managed ids, and
	// registry session ids whose pid is a live process.
	Held map[string]bool
}
type Machine struct {
	Key     string
	Name    string
	HostID  *string
	Probe   func(context.Context) (ProbeResult, error)
	Reports func(context.Context) ([]Report, error)
	// Lifetimes answers the current start time of each pid that is alive, in
	// the probe's Lifetime format. A pid that is gone or cannot be read is
	// absent. nil means the machine has no cheap check; the drain probes.
	Lifetimes func(ctx context.Context, pids []int) (map[int]string, error)
	Read      func(context.Context, Observation) ([]byte, error)
	// ReadRange returns the file bytes [start, end) of the observation's
	// transcript, at most max of them. end < 0 means the file's size. A missing
	// file answers an empty window.
	ReadRange func(ctx context.Context, o Observation, end, max int64) (window []byte, start int64, err error)
	// Ended lists transcripts modified at or after cutoff, at most
	// EndedRowsMax, skipping held stems. A file that cannot be read is skipped,
	// never an error.
	Ended func(ctx context.Context, held map[string]bool, cutoff time.Time) ([]Observation, error)
}
type Options struct {
	Clock    Clock
	Machines func() []Machine
	// EndedWindow is how long an ended session stays listed; nil or 0 lists none.
	EndedWindow func() time.Duration
}
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func stamp(t time.Time) string                           { return t.UTC().Format(time.RFC3339Nano) }
