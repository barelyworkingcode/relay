// Package adopt takes over an agent session that a person started in a
// terminal: it finds the process, judges whether it is idle, stops it and hands
// back what the caller needs to resume the same conversation under relay.
//
// Everything here reads evidence about a process (the agent's own registry
// entry, its transcript, the kernel's process table) and signals at most the
// one process that evidence names. The package decides nothing about who may
// ask: relay's door authorizes, audits and launches.
package adopt

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/barelyworkingcode/relay/internal/outsidesessions"
)

// Clock is the time every bound in this package is judged against.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// The waits of the signal ladder. Each is a bound on the injected clock.
const (
	InterruptWait = 5 * time.Second
	TermWait      = 5 * time.Second
	KillWait      = 5 * time.Second
)

// Reason codes, in the order they are judged; the first that applies is the
// reason. They are the codes of an outside row's adoptReason.
const (
	ReasonEnded          = outsidesessions.AdoptReasonEnded
	ReasonHost           = outsidesessions.AdoptReasonHost
	ReasonNoProject      = outsidesessions.AdoptReasonNoProject
	ReasonNotRegistered  = outsidesessions.AdoptReasonNotRegistered
	ReasonInvalidID      = outsidesessions.AdoptReasonInvalidID
	ReasonStartMismatch  = outsidesessions.AdoptReasonStartMismatch
	ReasonNotInteractive = outsidesessions.AdoptReasonNotInteractive
	ReasonNotCommandLine = outsidesessions.AdoptReasonNotCommandLine
	ReasonAmbiguous      = outsidesessions.AdoptReasonAmbiguous
)

// Vetoes of the idle check.
const (
	VetoRegistryBusy = "registry_busy"
	VetoStatusStale  = "status_stale"
	VetoOpenToolCall = "open_tool_call"
	VetoChildProcess = "child_process"
)

var (
	ErrChanged    = errors.New("adopt: the process or its registry entry changed")
	ErrStopFailed = errors.New("adopt: the process outlived SIGKILL's wait")
)

// Candidate is one live process the agent's registry names.
type Candidate struct {
	Agent          string // "claude"
	ConversationID string // the registry sessionId
	PID            int    // process evidence; never on the wire
	// ProcStart is the kernel's start time of PID. Its second equals the
	// registry's procStart for an adoptable candidate.
	ProcStart time.Time
	Cwd       string // the registry cwd
	Adoptable bool
	Reason    string // a Reason* code when !Adoptable
}

// ToolCall is a tool_use block of the transcript.
type ToolCall struct {
	ID, Name string
	Input    json.RawMessage
}

// Tail is what the end of a transcript says about the session.
type Tail struct {
	OpenTool    *ToolCall // the newest tool_use with no tool_result; nil when none
	LastPrompt  string    // the newest user record's plain text
	LastRecord  time.Time // the newest user or assistant record's timestamp
	ModelFamily string    // "haiku", "sonnet", "opus" or ""
	Model       string    // the newest assistant record's model id; never "" or "<synthetic>" once seen
}

// Readiness is the answer of Check. Tail is read whether or not the session is
// idle, so a caller that interrupts still has what the recovery message quotes.
type Readiness struct {
	Idle bool
	Veto string // "" when Idle
	Tail Tail
}

// Signal is one signal sent to the process. Name is "INT", "TERM" or "KILL";
// At is on the injected clock.
type Signal struct {
	Name string
	At   time.Time
}

// SessionSpec is what a caller needs to resume the conversation under relay.
type SessionSpec struct {
	Kind           string // "claude"
	ConversationID string
	Directory      string
	Model          string   // the derived family; "" when unknown
	Signals        []Signal // what TakeOver sent
}

// Adopter finds and takes over the sessions of one agent.
type Adopter interface {
	// Discover lists the live processes the agent's registry names, each with
	// the verdict on whether it may be adopted.
	Discover(ctx context.Context) ([]Candidate, error)
	// Check re-reads the registry entry, the transcript tail and the process
	// table, and says whether the session is idle. It fails with ErrChanged when
	// the candidate no longer matches the process or its entry.
	Check(ctx context.Context, c Candidate) (Readiness, error)
	// Interrupt sends SIGINT and waits for the process to exit, up to
	// InterruptWait. It returns what it sent.
	Interrupt(ctx context.Context, c Candidate) ([]Signal, error)
	// TakeOver ends the process if it lives: SIGTERM, then SIGKILL, each after
	// its bounded wait. It fails with ErrStopFailed when the process outlives
	// SIGKILL's wait.
	TakeOver(ctx context.Context, c Candidate) (SessionSpec, error)
	// Alive reports whether the process of c still runs.
	Alive(c Candidate) bool
	// TranscriptPath derives the conversation's transcript file and refuses a
	// symlink on the file or its three parent folders.
	TranscriptPath(c Candidate) (string, error)
}
