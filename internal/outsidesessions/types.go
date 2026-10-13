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
}
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
	Session    Session
	ClaudeID   string
	PID        int
	Lifetime   string
	Transcript string
	StatusAt   time.Time
}

// ProbeResult is one completed probe. SawSpool is true when the machine has a
// report spool directory, which is the only reason to drain it.
type ProbeResult struct {
	Observations []Observation
	SawSpool     bool
}
type Machine struct {
	Key     string
	Name    string
	HostID  *string
	Probe   func(context.Context) (ProbeResult, error)
	Reports func(context.Context) ([]Report, error)
	Read    func(context.Context, Observation) ([]byte, error)
}
type Options struct {
	Clock    Clock
	Machines func() []Machine
}
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func stamp(t time.Time) string                           { return t.UTC().Format(time.RFC3339Nano) }
